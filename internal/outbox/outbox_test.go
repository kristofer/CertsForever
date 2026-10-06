package outbox

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"certsforever/internal/emails"
	"certsforever/internal/mail"
	"certsforever/internal/secretbox"
	"certsforever/internal/store"
)

// scriptedSender fails with the queued errors, then succeeds, recording deliveries.
type scriptedSender struct {
	mu   sync.Mutex
	errs []error
	sent []mail.Message
}

func (s *scriptedSender) Send(_ context.Context, m mail.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.errs) > 0 {
		err := s.errs[0]
		s.errs = s.errs[1:]
		return err
	}
	s.sent = append(s.sent, m)
	return nil
}

type fixture struct {
	st     *store.Store
	q      *Queue
	w      *Worker
	sender *scriptedSender
	box    *secretbox.Box
}

func setup(t *testing.T, errs ...error) *fixture {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "certs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	box, _ := secretbox.New(bytes.Repeat([]byte{9}, 32))
	q := NewQueue(st, box)
	snd := &scriptedSender{errs: errs}
	w := &Worker{Queue: q, Sender: snd, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Platform: emails.Platform{Name: "CertsForever", BaseURL: "https://certs.example.test"}}
	return &fixture{st: st, q: q, w: w, sender: snd, box: box}
}

const secretLink = "https://certs.example.test/login/SECRET-TOKEN-123"

func (f *fixture) queueSignIn(t *testing.T, to string) int64 {
	t.Helper()
	m, _ := emails.SignIn(f.w.Platform, to, secretLink, 15)
	if _, err := f.q.Send(context.Background(), store.Scope{}, "sign_in", m, "", ""); err != nil {
		t.Fatal(err)
	}
	list, _ := f.st.ListEmails(context.Background(), "", 1)
	return list[0].ID
}

func TestDeliversAndWipes(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	id := f.queueSignIn(t, "ada@example.com")

	it, _ := f.st.GetEmail(ctx, id)
	if !it.HasBody || bytes.Contains(it.BodyEnc, []byte("SECRET-TOKEN")) {
		t.Fatal("body must be stored encrypted")
	}
	f.w.Drain(ctx)
	if len(f.sender.sent) != 1 || !strings.Contains(f.sender.sent[0].Text, secretLink) || f.sender.sent[0].FromName != "CertsForever" {
		t.Fatalf("delivered: %+v", f.sender.sent)
	}
	if it, _ = f.st.GetEmail(ctx, id); it.Status != "sent" || it.HasBody {
		t.Fatalf("after send: %+v", it)
	}
}

func TestTemporaryFailuresBackOffThenDeliver(t *testing.T) {
	f := setup(t, errors.New("421 try later"), errors.New("dial tcp: timeout"))
	ctx := context.Background()
	id := f.queueSignIn(t, "ada@example.com")

	f.w.Drain(ctx)
	it, _ := f.st.GetEmail(ctx, id)
	if it.Status != "queued" || it.Attempts != 1 || it.LastError != "421 try later" {
		t.Fatalf("after first failure: %+v", it)
	}
	if wait := time.Until(it.NextAt); wait < 50*time.Second || wait > 70*time.Second {
		t.Errorf("first retry in %v, want about a minute", wait)
	}
	f.w.Drain(ctx) // not due yet
	if len(f.sender.sent) != 0 {
		t.Fatal("retried before the backoff elapsed")
	}

	base := time.Now()
	restore := store.SetClockForTest(func() time.Time { return base.Add(2 * time.Minute) })
	defer restore()
	f.w.Drain(ctx) // second failure
	restore2 := store.SetClockForTest(func() time.Time { return base.Add(10 * time.Minute) })
	defer restore2()
	f.w.Drain(ctx)
	if it, _ = f.st.GetEmail(ctx, id); it.Status != "sent" || it.Attempts != 3 || len(f.sender.sent) != 1 {
		t.Fatalf("after recovery: %+v (sent %d)", it, len(f.sender.sent))
	}
}

func TestRejectedRecipientIsSuppressed(t *testing.T) {
	f := setup(t, &mail.RejectedError{Code: 550, Msg: "5.1.1 no such user"})
	ctx := context.Background()
	id := f.queueSignIn(t, "ghost@example.com")
	f.w.Drain(ctx)

	it, _ := f.st.GetEmail(ctx, id)
	if it.Status != "failed" || it.Attempts != 1 || !strings.Contains(it.LastError, "550") {
		t.Fatalf("rejected message: %+v", it)
	}
	if ok, _ := f.st.IsSuppressed(ctx, "ghost@example.com"); !ok {
		t.Fatal("address not suppressed")
	}
	if m, _ := emails.SignIn(f.w.Platform, "Ghost@Example.com", secretLink, 15); true {
		if status, _ := f.q.Send(ctx, store.Scope{}, "sign_in", m, "", ""); status != "suppressed" {
			t.Fatalf("later mail to a rejected address: %s", status)
		}
	}
	log, _ := f.st.ListAllAudit(ctx, 5)
	if len(log) == 0 || log[0].Action != "email.suppressed" || log[0].Actor != "mailer" {
		t.Fatalf("suppression not audited: %+v", log)
	}
}

func TestUndecryptableMessageFails(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	id := f.queueSignIn(t, "ada@example.com")
	// The server restarted with a different master key.
	other, _ := secretbox.New(bytes.Repeat([]byte{1}, 32))
	f.w.Queue = &Queue{st: f.st, box: other, wake: make(chan struct{}, 1)}
	f.w.Drain(ctx)
	it, _ := f.st.GetEmail(ctx, id)
	if it.Status != "failed" || !strings.Contains(it.LastError, "decrypt") || len(f.sender.sent) != 0 {
		t.Fatalf("undecryptable: %+v", it)
	}
}

func TestRemindersAreQueuedOnceWithAWorkingLink(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	name, prefix, reply := "Zip Code Wilmington", "ZCW", "info@zipcode.example"
	f.st.CreateClient(ctx, store.ClientInput{Slug: "zcw", Name: &name, IDPrefix: &prefix, ReplyTo: &reply})
	sc, _ := f.st.Scope(ctx, "zcw")
	f.st.CreateCourse(ctx, sc, store.Course{Slug: "java", Title: "Java Developer"})
	f.st.Issue(ctx, sc, []store.IssueRequest{{Email: "ada@example.com", FullName: "Ada Lovelace", CourseSlug: "java",
		Cohort: "J1", CompletedOn: time.Now()}})

	f.w.Chores(ctx)
	f.w.Drain(ctx)
	if len(f.sender.sent) != 0 {
		t.Fatal("reminder sent too early")
	}
	restore := store.SetClockForTest(func() time.Time { return time.Now().Add(8 * 24 * time.Hour) })
	defer restore()
	f.w.Chores(ctx)
	f.w.Chores(ctx) // twice: still one reminder
	f.w.Drain(ctx)
	if len(f.sender.sent) != 1 {
		t.Fatalf("reminders sent: %d", len(f.sender.sent))
	}
	m := f.sender.sent[0]
	if m.FromName != name || m.ReplyTo != reply || !strings.Contains(m.Subject, "Java Developer") {
		t.Fatalf("reminder: %+v", m)
	}
	i := strings.Index(m.Text, "/claim/")
	token := strings.Fields(m.Text[i+len("/claim/"):])[0]
	if c, err := f.st.GetCertificateByClaimToken(ctx, token); err != nil || c.RecipientName != "Ada Lovelace" {
		t.Fatalf("reminder link doesn't work: %v", err)
	}
}

func TestBackoff(t *testing.T) {
	for n, want := range map[int]time.Duration{1: time.Minute, 2: 2 * time.Minute, 4: 8 * time.Minute, 7: 64 * time.Minute, 20: 6 * time.Hour} {
		got := Backoff(n)
		if got < time.Duration(float64(want)*0.89) || got > time.Duration(float64(want)*1.11) {
			t.Errorf("Backoff(%d) = %v, want about %v", n, got, want)
		}
	}
}

func TestRunStopsAndWakes(t *testing.T) {
	f := setup(t)
	f.w.Poll = time.Hour // only a wake-up can trigger delivery
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.w.Run(ctx); close(done) }()
	time.Sleep(20 * time.Millisecond)
	f.queueSignIn(t, "ada@example.com") // Send wakes the worker
	deadline := time.Now().Add(2 * time.Second)
	for {
		f.sender.mu.Lock()
		n := len(f.sender.sent)
		f.sender.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("wake-up didn't deliver")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run didn't stop")
	}
}
