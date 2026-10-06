package mail

import (
	"context"
	"encoding/base64"
	"io"
	"mime/quotedprintable"
	"net"
	"net/textproto"
	"strings"
	"testing"
)

// fakeSMTP is a minimal SMTP server: no TLS, AUTH PLAIN, one message.
type fakeSMTP struct {
	ln       net.Listener
	auth     chan string
	from, to chan string
	data     chan string
	starttls bool
	authResp string // default "235 ok"
	rcptResp string // default "250 ok"
}

func newFakeSMTP(t *testing.T, offerStartTLS bool) *fakeSMTP {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTP{ln: ln, auth: make(chan string, 1), from: make(chan string, 1), to: make(chan string, 1),
		data: make(chan string, 1), starttls: offerStartTLS}
	t.Cleanup(func() { ln.Close() })
	return f
}

func (f *fakeSMTP) start() *fakeSMTP { go f.serve(); return f }

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func (f *fakeSMTP) serve() {
	conn, err := f.ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	tp := textproto.NewConn(conn)
	tp.PrintfLine("220 fake ESMTP")
	for {
		line, err := tp.ReadLine()
		if err != nil {
			return
		}
		cmd := strings.ToUpper(strings.Fields(line + " x")[0])
		switch cmd {
		case "EHLO", "HELO":
			if f.starttls {
				tp.PrintfLine("250-fake\r\n250-STARTTLS\r\n250 AUTH PLAIN")
			} else {
				tp.PrintfLine("250-fake\r\n250 AUTH PLAIN")
			}
		case "AUTH":
			raw, _ := base64.StdEncoding.DecodeString(strings.Fields(line)[2])
			f.auth <- string(raw)
			tp.PrintfLine("%s", or(f.authResp, "235 ok"))
		case "MAIL":
			f.from <- line
			tp.PrintfLine("250 ok")
		case "RCPT":
			f.to <- line
			tp.PrintfLine("%s", or(f.rcptResp, "250 ok"))
		case "DATA":
			tp.PrintfLine("354 go")
			b, _ := io.ReadAll(tp.DotReader())
			f.data <- string(b)
			tp.PrintfLine("250 queued")
		case "QUIT":
			tp.PrintfLine("221 bye")
			return
		default:
			tp.PrintfLine("502 no")
		}
	}
}

func TestSMTPSend(t *testing.T) {
	f := newFakeSMTP(t, false).start()
	s, err := NewSMTP("smtp://user:p%40ss@"+f.ln.Addr().String(), "CertsForever <certs@example.org>")
	if err != nil {
		t.Fatal(err)
	}
	link := "https://certs.example.org/login/abc_DEF-123=xyz"
	err = s.Send(context.Background(), Message{To: "Ada Lovelace <ada@example.com>", Subject: "Sign in to CertsForever — ✓",
		Text: "Hello,\nUse this link:\n" + link + "\n"})
	if err != nil {
		t.Fatal(err)
	}
	if a := <-f.auth; a != "\x00user\x00p@ss" {
		t.Errorf("auth %q", a)
	}
	if from := <-f.from; !strings.Contains(from, "<certs@example.org>") {
		t.Errorf("MAIL %q", from)
	}
	if to := <-f.to; !strings.Contains(to, "<ada@example.com>") {
		t.Errorf("RCPT %q", to)
	}
	data := <-f.data
	head, body, _ := strings.Cut(data, "\n\n")
	for _, want := range []string{"From: \"CertsForever\" <certs@example.org>", "To: \"Ada Lovelace\" <ada@example.com>",
		"Subject: =?utf-8?q?", "Message-ID: <", "@example.org>", "Content-Transfer-Encoding: quoted-printable"} {
		if !strings.Contains(head, want) {
			t.Errorf("headers missing %q:\n%s", want, head)
		}
	}
	decoded, _ := io.ReadAll(quotedprintable.NewReader(strings.NewReader(body)))
	if !strings.Contains(string(decoded), link) {
		t.Errorf("link not intact after encoding:\n%s", decoded)
	}
}

func TestSMTPRefusesInjectionAndBadConfig(t *testing.T) {
	s, _ := NewSMTP("smtp://127.0.0.1:1", "certs@example.org")
	if err := s.Send(context.Background(), Message{To: "a@example.com", Subject: "hi\r\nBcc: evil@example.com"}); err == nil {
		t.Error("header injection via subject accepted")
	}
	if err := s.Send(context.Background(), Message{To: "a@example.com\r\nBcc: evil@example.com", Subject: "hi"}); err == nil {
		t.Error("header injection via recipient accepted")
	}
	for _, bad := range []string{"http://mail.example.com", "smtp://", "mail.example.com:25"} {
		if _, err := NewSMTP(bad, "certs@example.org"); err == nil {
			t.Errorf("NewSMTP(%q) accepted", bad)
		}
	}
	if _, err := NewSMTP("smtp://mail.example.com", "not an address"); err == nil {
		t.Error("bad From accepted")
	}
	if s, _ := NewSMTP("smtps://mail.example.com", "a@b.c"); s.port != "465" {
		t.Error("smtps default port")
	}
	if s, _ := NewSMTP("smtp://mail.example.com", "a@b.c"); s.port != "587" {
		t.Error("smtp default port")
	}
}

// A non-loopback server without STARTTLS must be refused: no clear-text
// credentials or sign-in links. The fake server is on loopback, so the test
// switches the requirement on as it would be for a public host.
func TestSMTPRequiresStartTLSOffLoopback(t *testing.T) {
	if isLoopback("mail.example.com") || !isLoopback("127.0.0.1") || !isLoopback("localhost") || !isLoopback("::1") {
		t.Fatal("isLoopback")
	}
	if s, _ := NewSMTP("smtp://mail.example.com", "a@b.c"); !s.requireTLS {
		t.Fatal("public host must require TLS")
	}
	f := newFakeSMTP(t, false).start()
	s, _ := NewSMTP("smtp://user:pass@"+f.ln.Addr().String(), "certs@example.org")
	s.requireTLS = true
	err := s.Send(context.Background(), Message{To: "a@example.com", Subject: "hi", Text: "secret link"})
	if err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("sent without TLS: %v", err)
	}
	select {
	case a := <-f.auth:
		t.Fatalf("credentials sent in clear text: %q", a)
	default:
	}
}

func TestRecorderAndDisabled(t *testing.T) {
	var r Recorder
	r.Send(context.Background(), Message{To: "a@b.c", Subject: "one"})
	r.Send(context.Background(), Message{To: "a@b.c", Subject: "two"})
	if r.Count() != 2 || r.Last().Subject != "two" {
		t.Errorf("recorder: %+v", r.Sent)
	}
	if err := (Disabled{}).Send(context.Background(), Message{}); err != ErrNotConfigured {
		t.Errorf("disabled: %v", err)
	}
}

func TestSMTPMultipartWithBranding(t *testing.T) {
	f := newFakeSMTP(t, false).start()
	s, _ := NewSMTP("smtp://"+f.ln.Addr().String(), "CertsForever <certs@example.org>")
	err := s.Send(context.Background(), Message{To: "ada@example.com", Subject: "Your certificate",
		Text: "plain https://x.example/claim/abc", HTML: "<p>Hi <b>Ada</b> <a href=\"https://x.example/claim/abc\">claim</a></p>",
		FromName: "Zip Code Wilmington", ReplyTo: "info@zipcode.example"})
	if err != nil {
		t.Fatal(err)
	}
	<-f.from
	<-f.to
	data := <-f.data
	for _, want := range []string{"From: \"Zip Code Wilmington\" <certs@example.org>", "Reply-To: <info@zipcode.example>",
		"Content-Type: multipart/alternative; boundary=", "Content-Type: text/plain; charset=utf-8",
		"Content-Type: text/html; charset=utf-8"} {
		if !strings.Contains(data, want) {
			t.Errorf("missing %q in:\n%s", want, data)
		}
	}
	if err := s.Send(context.Background(), Message{To: "a@example.com", Subject: "x", ReplyTo: "bad\r\nBcc: x@evil.example"}); err == nil {
		t.Error("header injection via Reply-To accepted")
	}
	if err := s.Send(context.Background(), Message{To: "a@example.com", Subject: "x", FromName: "x\nBcc: y"}); err == nil {
		t.Error("header injection via From name accepted")
	}
}

func TestRecipientRejectionIsClassified(t *testing.T) {
	f := newFakeSMTP(t, false)
	f.rcptResp = "550 5.1.1 no such user"
	f.start()
	s, _ := NewSMTP("smtp://"+f.ln.Addr().String(), "certs@example.org")
	err := s.Send(context.Background(), Message{To: "ghost@example.com", Subject: "x", Text: "y"})
	if !IsRejected(err) || !strings.Contains(err.Error(), "550") {
		t.Fatalf("550 at RCPT: %v (IsRejected=%v)", err, IsRejected(err))
	}

	// A 5xx elsewhere (bad credentials) is our problem, not the recipient's.
	f2 := newFakeSMTP(t, false)
	f2.authResp = "535 5.7.8 authentication failed"
	f2.start()
	s2, _ := NewSMTP("smtp://user:wrong@"+f2.ln.Addr().String(), "certs@example.org")
	err = s2.Send(context.Background(), Message{To: "ada@example.com", Subject: "x", Text: "y"})
	if err == nil || IsRejected(err) {
		t.Fatalf("auth failure classified as recipient rejection: %v", err)
	}

	// A 4xx at RCPT is temporary.
	f3 := newFakeSMTP(t, false)
	f3.rcptResp = "450 mailbox busy"
	f3.start()
	s3, _ := NewSMTP("smtp://"+f3.ln.Addr().String(), "certs@example.org")
	if err := s3.Send(context.Background(), Message{To: "ada@example.com", Subject: "x", Text: "y"}); err == nil || IsRejected(err) {
		t.Fatalf("450 classified as permanent: %v", err)
	}
}
