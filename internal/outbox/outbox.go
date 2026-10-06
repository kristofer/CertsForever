// Package outbox queues email in the database and delivers it in the
// background with retries.
//
//   - Queue seals each message (AES-GCM, master key) before it's stored:
//     bodies hold sign-in and claim links. The seal is bound to the
//     recipient, so a body can't be moved to another row and still open.
//   - Worker claims due messages one at a time (with a lease, so a crash
//     mid-send is retried), sends them, and records the outcome: sent (body
//     wiped), retry with exponential backoff, or a permanent recipient
//     rejection (address suppressed). It also sends publish reminders and
//     prunes old rows.
//
// Delivery is at-least-once.
package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"time"

	"certsforever/internal/emails"
	"certsforever/internal/mail"
	"certsforever/internal/secretbox"
	"certsforever/internal/store"
)

// Queue puts messages in the outbox.
type Queue struct {
	st   *store.Store
	box  *secretbox.Box
	wake chan struct{}
}

// NewQueue returns a Queue.
func NewQueue(st *store.Store, box *secretbox.Box) *Queue {
	return &Queue{st: st, box: box, wake: make(chan struct{}, 1)}
}

func ad(to string) string { return "outbox:" + strings.ToLower(strings.TrimSpace(to)) }

// Prepare seals a message into a row for store.Enqueue or an in-transaction
// hook (IssueAndNotify, SendReminder). Call Wake after the commit.
func (q *Queue) Prepare(template string, m mail.Message, refType, refID string) (*store.NewEmail, error) {
	body, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return &store.NewEmail{To: m.To, Template: template, Subject: m.Subject,
		BodyEnc: q.box.Seal(body, ad(m.To)), RefType: refType, RefID: refID}, nil
}

// Send queues a message and wakes the worker. It returns the row's status:
// "queued", or "suppressed" if the address is on the suppression list.
func (q *Queue) Send(ctx context.Context, sc store.Scope, template string, m mail.Message, refType, refID string) (string, error) {
	e, err := q.Prepare(template, m, refType, refID)
	if err != nil {
		return "", err
	}
	_, status, err := q.st.Enqueue(ctx, sc, *e)
	if err == nil {
		q.Wake()
	}
	return status, err
}

// Wake tells the worker there's new mail (non-blocking).
func (q *Queue) Wake() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *Queue) open(it *store.OutboxItem) (mail.Message, error) {
	var m mail.Message
	raw, err := q.box.Open(it.BodyEnc, ad(it.To))
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return m, err
	}
	if !strings.EqualFold(strings.TrimSpace(m.To), it.To) {
		return m, errors.New("sealed recipient doesn't match row")
	}
	return m, nil
}

// Worker delivers queued mail.
type Worker struct {
	Queue    *Queue
	Sender   mail.Sender
	Log      *slog.Logger
	Platform emails.Platform

	Poll          time.Duration // how often to look for due mail without a wake-up (default 15s)
	Housekeeping  time.Duration // how often to send reminders and prune (default 1h)
	ReminderAfter time.Duration // publish reminder after this long (default 7 days)
	SendTimeout   time.Duration // per message (default 60s)
}

func (w *Worker) defaults() {
	if w.Poll == 0 {
		w.Poll = 15 * time.Second
	}
	if w.Housekeeping == 0 {
		w.Housekeeping = time.Hour
	}
	if w.ReminderAfter == 0 {
		w.ReminderAfter = 7 * 24 * time.Hour
	}
	if w.SendTimeout == 0 {
		w.SendTimeout = 60 * time.Second
	}
}

// Backoff is the wait before attempt n+1 after n failed attempts:
// 1, 2, 4 … minutes, capped at 6 hours, with ±10% jitter.
func Backoff(n int) time.Duration {
	d := time.Minute << min(n-1, 9)
	if d > 6*time.Hour {
		d = 6 * time.Hour
	}
	return time.Duration(float64(d) * (0.9 + 0.2*rand.Float64()))
}

// Run delivers mail until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) {
	w.defaults()
	tick := time.NewTicker(w.Poll)
	defer tick.Stop()
	nextChores := time.Now()
	for {
		w.Drain(ctx)
		if time.Now().After(nextChores) {
			w.Chores(ctx)
			nextChores = time.Now().Add(w.Housekeeping)
		}
		select {
		case <-ctx.Done():
			return
		case <-w.Queue.wake:
		case <-tick.C:
		}
	}
}

// Drain sends everything that's due.
func (w *Worker) Drain(ctx context.Context) {
	w.defaults()
	for ctx.Err() == nil {
		sent, err := w.deliverOne(ctx)
		if err != nil {
			w.Log.Error("outbox", "err", err)
			return
		}
		if !sent {
			return
		}
	}
}

// deliverOne handles one due message; false when there was nothing to do.
func (w *Worker) deliverOne(ctx context.Context) (bool, error) {
	st := w.Queue.st
	it, err := st.ClaimNextEmail(ctx)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	log := w.Log.With("email_id", it.ID, "template", it.Template, "attempt", it.Attempts)

	m, err := w.Queue.open(it)
	if err != nil {
		log.Error("outbox: can't open message (was CERTS_MASTER_KEY changed?)", "err", err)
		return true, st.FailEmail(ctx, it.ID, "cannot decrypt message: "+err.Error())
	}
	sendCtx, cancel := context.WithTimeout(ctx, w.SendTimeout)
	err = w.Sender.Send(sendCtx, m)
	cancel()

	switch {
	case err == nil:
		log.Info("email sent")
		return true, st.MarkEmailSent(ctx, it.ID)
	case mail.IsRejected(err):
		log.Warn("email rejected by recipient's server; address suppressed", "err", err)
		if serr := st.Suppress(ctx, it.To, err.Error(), "smtp"); serr != nil {
			return true, serr
		}
		if aerr := st.Audit(ctx, store.Scope{}, store.AuditEntry{Actor: "mailer", Action: "email.suppressed",
			TargetType: "email", TargetID: it.To, Details: map[string]any{"reason": err.Error()}}); aerr != nil {
			log.Error("audit", "err", aerr)
		}
		return true, st.FailEmail(ctx, it.ID, err.Error())
	default:
		delay := Backoff(it.Attempts)
		failed, rerr := st.RetryEmailAfter(ctx, it.ID, delay, err.Error())
		if failed {
			log.Error("email failed permanently after retries", "err", err)
		} else {
			log.Warn("email send failed; will retry", "err", err, "retry_in", delay.Round(time.Second).String())
		}
		return true, rerr
	}
}

// Chores sends due publish reminders and prunes old outbox rows.
func (w *Worker) Chores(ctx context.Context) {
	w.defaults()
	st := w.Queue.st
	due, err := st.DueReminders(ctx, w.ReminderAfter, 200)
	if err != nil {
		w.Log.Error("reminders", "err", err)
	}
	sent := 0
	for _, d := range due {
		sc, err := st.Scope(ctx, d.ClientSlug)
		if err != nil {
			continue
		}
		cl := sc.Client()
		err = st.SendReminder(ctx, sc, d.CertificateID, func(c *store.Certificate, token string) (*store.NewEmail, error) {
			m, err := emails.Reminder(w.Platform, emails.Client{Name: cl.Name, ReplyTo: cl.ReplyTo, SiteURL: cl.SiteURL},
				c.Email, c.RecipientName, c.CourseTitle, c.PublicBase(w.Platform.BaseURL)+"/claim/"+token)
			if err != nil {
				return nil, err
			}
			return w.Queue.Prepare("reminder", m, "certificate", c.ID)
		})
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			w.Log.Error("reminder", "certificate", d.CertificateID, "err", err)
			continue
		}
		if err == nil {
			sent++
		}
	}
	if sent > 0 {
		w.Log.Info("publish reminders queued", "count", sent)
		w.Queue.Wake()
	}
	if err := st.PruneOutbox(ctx); err != nil {
		w.Log.Error("prune outbox", "err", err)
	}
}

// Describe summarizes a sender for logs.
func Describe(s mail.Sender) string {
	switch s.(type) {
	case *mail.SMTP:
		return "smtp"
	case mail.Log:
		return "log (development)"
	default:
		return fmt.Sprintf("%T", s)
	}
}
