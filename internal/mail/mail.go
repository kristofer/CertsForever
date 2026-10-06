// Package mail sends CertsForever's transactional email (sign-in links and
// invitations for now). Milestone 3 adds a durable outbox with retries in
// front of a Sender; this package is the delivery end.
package mail

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Message is an email with a plain-text body and an optional HTML body.
type Message struct {
	To       string
	Subject  string
	Text     string
	HTML     string // optional; sent as multipart/alternative with Text
	FromName string // display name override (address stays CERTS_MAIL_FROM)
	ReplyTo  string // optional Reply-To address
}

// RejectedError means the receiving server permanently refused the
// recipient (an SMTP 5xx reply to RCPT TO): the address doesn't exist or
// won't accept mail. Retrying won't help; the address should be suppressed.
// Other 5xx replies (e.g. authentication failures) are configuration
// problems and are NOT RejectedErrors, so a broken setup never suppresses
// good addresses.
type RejectedError struct {
	Code int
	Msg  string
}

func (e *RejectedError) Error() string {
	return fmt.Sprintf("recipient rejected: %d %s", e.Code, e.Msg)
}

// IsRejected reports whether err is a permanent recipient rejection.
func IsRejected(err error) bool {
	var r *RejectedError
	return errors.As(err, &r)
}

// Sender delivers messages.
type Sender interface {
	Send(ctx context.Context, m Message) error
}

// ErrNotConfigured means no way to send email is configured.
var ErrNotConfigured = errors.New("email sending is not configured")

// Disabled refuses to send. Used in production when SMTP isn't configured;
// operators can still print sign-in links with `certsforever login-link`.
type Disabled struct{}

func (Disabled) Send(context.Context, Message) error { return ErrNotConfigured }

// Log writes messages to the log instead of sending them. Development only:
// sign-in links in logs are as good as passwords.
type Log struct{ Logger *slog.Logger }

func (l Log) Send(_ context.Context, m Message) error {
	l.Logger.Info("email (development: not sent)", "to", m.To, "subject", m.Subject, "body", m.Text)
	return nil
}

// Recorder keeps messages in memory (tests). Fail, if set, can refuse a
// message to simulate a failing server.
type Recorder struct {
	mu   sync.Mutex
	Sent []Message
	fail func(Message) error
}

// FailWith makes Send return f's error (nil: deliver normally).
func (r *Recorder) FailWith(f func(Message) error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fail = f
}

func (r *Recorder) Send(_ context.Context, m Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail != nil {
		if err := r.fail(m); err != nil {
			return err
		}
	}
	r.Sent = append(r.Sent, m)
	return nil
}

// Last returns the most recent message (zero Message if none).
// All returns a copy of the messages sent so far.
func (r *Recorder) All() []Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Message(nil), r.Sent...)
}

// Last returns the most recent message.
func (r *Recorder) Last() Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.Sent) == 0 {
		return Message{}
	}
	return r.Sent[len(r.Sent)-1]
}

// Count returns how many messages were sent.
func (r *Recorder) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.Sent)
}

// SMTP sends through an SMTP server, configured by URL:
//
//	smtp://user:pass@smtp.example.com:587   STARTTLS (required except on loopback)
//	smtps://user:pass@smtp.example.com:465  implicit TLS
type SMTP struct {
	host, port string
	implicit   bool
	user, pass string
	from       *mail.Address
	requireTLS bool // false only for loopback servers (local relays, tests)
	timeout    time.Duration
	tlsConfig  *tls.Config // nil: system roots, ServerName = host
}

// NewSMTP parses the server URL and From address.
func NewSMTP(rawURL, from string) (*SMTP, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "smtp" && u.Scheme != "smtps") || u.Hostname() == "" {
		return nil, fmt.Errorf("SMTP URL must look like smtp://user:pass@host:587 or smtps://user:pass@host:465")
	}
	addr, err := mail.ParseAddress(from)
	if err != nil {
		return nil, fmt.Errorf("mail From address %q: %w", from, err)
	}
	s := &SMTP{host: u.Hostname(), port: u.Port(), implicit: u.Scheme == "smtps", from: addr,
		requireTLS: !isLoopback(u.Hostname()), timeout: 20 * time.Second}
	if s.port == "" {
		s.port = map[bool]string{true: "465", false: "587"}[s.implicit]
	}
	if u.User != nil {
		s.user = u.User.Username()
		s.pass, _ = u.User.Password()
	}
	return s, nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Send delivers one message.
func (s *SMTP) Send(ctx context.Context, m Message) error {
	to, err := mail.ParseAddress(m.To)
	if err != nil {
		return fmt.Errorf("recipient %q: %w", m.To, err)
	}
	if strings.ContainsAny(m.Subject+m.FromName+m.ReplyTo, "\r\n") {
		return errors.New("header field contains a line break")
	}
	body, err := s.build(to, m)
	if err != nil {
		return err
	}

	deadline := time.Now().Add(s.timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	dialer := &net.Dialer{Deadline: deadline}
	addr := net.JoinHostPort(s.host, s.port)
	tlsCfg := s.tlsConfig
	if tlsCfg == nil {
		tlsCfg = &tls.Config{ServerName: s.host, MinVersion: tls.VersionTLS12}
	}
	var conn net.Conn
	if s.implicit {
		conn, err = (&tls.Dialer{NetDialer: dialer, Config: tlsCfg}).DialContext(ctx, "tcp", addr)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("connect %s: %w", addr, err)
	}
	conn.SetDeadline(deadline)
	c, err := smtp.NewClient(conn, s.host)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()

	if !s.implicit {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(tlsCfg); err != nil {
				return fmt.Errorf("starttls: %w", err)
			}
		} else if s.requireTLS {
			return errors.New("SMTP server doesn't offer STARTTLS; refusing to send credentials and sign-in links in clear text")
		}
	}
	if s.user != "" {
		if err := c.Auth(smtp.PlainAuth("", s.user, s.pass, s.host)); err != nil {
			return fmt.Errorf("smtp auth: %w", err)
		}
	}
	if err := c.Mail(s.from.Address); err != nil {
		return err
	}
	if err := c.Rcpt(to.Address); err != nil {
		var te *textproto.Error
		if errors.As(err, &te) && te.Code >= 500 && te.Code < 600 {
			return &RejectedError{Code: te.Code, Msg: te.Msg}
		}
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(body); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

func (s *SMTP) build(to *mail.Address, m Message) ([]byte, error) {
	id := make([]byte, 12)
	rand.Read(id)
	domain := s.from.Address[strings.LastIndexByte(s.from.Address, '@')+1:]
	from := *s.from
	if m.FromName != "" {
		from.Name = m.FromName
	}
	var b bytes.Buffer
	h := func(k, v string) { fmt.Fprintf(&b, "%s: %s\r\n", k, v) }
	h("From", from.String())
	h("To", to.String())
	if m.ReplyTo != "" {
		rt, err := mail.ParseAddress(m.ReplyTo)
		if err != nil {
			return nil, fmt.Errorf("reply-to %q: %w", m.ReplyTo, err)
		}
		h("Reply-To", rt.String())
	}
	h("Subject", mime.QEncoding.Encode("utf-8", m.Subject))
	h("Date", time.Now().Format(time.RFC1123Z))
	h("Message-ID", "<"+hex.EncodeToString(id)+"@"+domain+">")
	h("MIME-Version", "1.0")
	h("Auto-Submitted", "auto-generated")
	if m.HTML == "" {
		h("Content-Type", "text/plain; charset=utf-8")
		h("Content-Transfer-Encoding", "quoted-printable")
		b.WriteString("\r\n")
		if err := writeQP(&b, m.Text); err != nil {
			return nil, err
		}
		return b.Bytes(), nil
	}
	mw := multipart.NewWriter(&b)
	h("Content-Type", "multipart/alternative; boundary="+mw.Boundary())
	b.WriteString("\r\n")
	for _, part := range []struct{ ctype, body string }{{"text/plain", m.Text}, {"text/html", m.HTML}} {
		pw, err := mw.CreatePart(textproto.MIMEHeader{
			"Content-Type":              {part.ctype + "; charset=utf-8"},
			"Content-Transfer-Encoding": {"quoted-printable"},
		})
		if err != nil {
			return nil, err
		}
		if err := writeQP(pw, part.body); err != nil {
			return nil, err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func writeQP(w io.Writer, text string) error {
	qp := quotedprintable.NewWriter(w)
	if _, err := qp.Write([]byte(strings.ReplaceAll(text, "\n", "\r\n"))); err != nil {
		return err
	}
	return qp.Close()
}
