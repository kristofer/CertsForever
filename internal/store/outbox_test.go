package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func mail(to string) NewEmail {
	return NewEmail{To: to, Template: "test", Subject: "Hello", BodyEnc: []byte("sealed")}
}

func TestOutboxDeliveryLifecycle(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)

	id, status, err := s.Enqueue(ctx, Scope{}, mail("Ada@Example.com"))
	if err != nil || status != "queued" {
		t.Fatalf("enqueue: %v %s", err, status)
	}
	it, err := s.ClaimNextEmail(ctx)
	if err != nil || it.ID != id || it.Status != "sending" || it.Attempts != 1 || string(it.BodyEnc) != "sealed" {
		t.Fatalf("claim: %+v %v", it, err)
	}
	if _, err := s.ClaimNextEmail(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatal("a leased message was claimed twice")
	}
	if err := s.MarkEmailSent(ctx, id); err != nil {
		t.Fatal(err)
	}
	it, _ = s.GetEmail(ctx, id)
	if it.Status != "sent" || it.HasBody || it.SentAt == nil {
		t.Fatalf("sent row must have its body wiped: %+v", it)
	}
}

func TestOutboxRetriesThenFails(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	id, _, _ := s.Enqueue(ctx, Scope{}, mail("ada@example.com"))
	base := clock
	t.Cleanup(func() { clock = base })
	elapsed := time.Duration(0)
	for attempt := 1; ; attempt++ {
		e := elapsed
		clock = func() time.Time { return base().Add(e) }
		it, err := s.ClaimNextEmail(ctx)
		if err != nil {
			t.Fatalf("attempt %d not due: %v", attempt, err)
		}
		if it.Attempts != attempt {
			t.Fatalf("attempts = %d, want %d", it.Attempts, attempt)
		}
		failed, err := s.RetryEmailAfter(ctx, id, time.Minute, "421 try later")
		if err != nil {
			t.Fatal(err)
		}
		if failed {
			if attempt != OutboxMaxAttempts {
				t.Fatalf("failed after %d attempts, want %d", attempt, OutboxMaxAttempts)
			}
			break
		}
		// Not due again until the delay passes.
		if _, err := s.ClaimNextEmail(ctx); !errors.Is(err, ErrNotFound) {
			t.Fatal("retried before its delay")
		}
		elapsed += time.Minute + time.Second
	}
	it, _ := s.GetEmail(ctx, id)
	if it.Status != "failed" || !it.HasBody || it.LastError != "421 try later" {
		t.Fatalf("failed row: %+v", it)
	}

	// Operators can retry within the keep window; then the body is wiped.
	if err := s.RetryFailedEmail(ctx, id); err != nil {
		t.Fatal(err)
	}
	if it, _ := s.GetEmail(ctx, id); it.Status != "queued" || it.Attempts != 0 {
		t.Fatalf("retried row: %+v", it)
	}
	s.ClaimNextEmail(ctx)
	s.FailEmail(ctx, id, "gave up")
	clock = func() time.Time { return base().Add(elapsed + OutboxKeepFailed + time.Hour) }
	if err := s.PruneOutbox(ctx); err != nil {
		t.Fatal(err)
	}
	if it, _ := s.GetEmail(ctx, id); it.HasBody {
		t.Fatal("failed body kept past the window")
	}
	if err := s.RetryFailedEmail(ctx, id); !errors.Is(err, ErrConflict) {
		t.Fatalf("retry after wipe: %v", err)
	}
	clock = func() time.Time { return base().Add(elapsed + OutboxKeepFinished + time.Hour) }
	s.PruneOutbox(ctx)
	if _, err := s.GetEmail(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Fatal("finished row not deleted after the retention window")
	}
}

func TestOutboxLeaseExpiryRecoversCrashedSends(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	id, _, _ := s.Enqueue(ctx, Scope{}, mail("ada@example.com"))
	s.ClaimNextEmail(ctx) // "worker crashes" here
	shiftClock(t, OutboxLease+time.Second)
	it, err := s.ClaimNextEmail(ctx)
	if err != nil || it.ID != id || it.Attempts != 2 {
		t.Fatalf("stale lease not reclaimed: %+v %v", it, err)
	}
}

func TestSuppressionBlocksAndCancels(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	queued, _, _ := s.Enqueue(ctx, Scope{}, mail("bea@example.com"))
	if err := s.Suppress(ctx, "BEA@example.com", "550 no such user", "smtp"); err != nil {
		t.Fatal(err)
	}
	if it, _ := s.GetEmail(ctx, queued); it.Status != "suppressed" || it.HasBody {
		t.Fatalf("queued mail to a suppressed address: %+v", it)
	}
	_, status, _ := s.Enqueue(ctx, Scope{}, mail("Bea@Example.com"))
	if status != "suppressed" {
		t.Fatalf("new mail to suppressed address (other case) = %s", status)
	}
	if _, err := s.ClaimNextEmail(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatal("suppressed mail was claimed")
	}

	// A failed message to an address suppressed later (any case) can't be retried.
	id, _, _ := s.Enqueue(ctx, Scope{}, mail("Cy@Example.com"))
	s.ClaimNextEmail(ctx)
	s.FailEmail(ctx, id, "timeout")
	s.Suppress(ctx, "cy@example.com", "complaint", "webhook")
	if err := s.RetryFailedEmail(ctx, id); !errors.Is(err, ErrConflict) {
		t.Fatalf("retry to suppressed address: %v", err)
	}
	if it, _ := s.GetEmail(ctx, id); it.Retryable() || !it.Suppressed {
		t.Fatalf("failed mail to a suppressed address must not look retryable: %+v", it)
	}

	if ok, _ := s.IsSuppressed(ctx, "bea@EXAMPLE.com"); !ok {
		t.Error("IsSuppressed is case-sensitive")
	}
	if list, _ := s.ListSuppressions(ctx, 10); len(list) != 2 {
		t.Errorf("suppressions: %+v", list)
	}
	if err := s.Unsuppress(ctx, "bea@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, status, _ := s.Enqueue(ctx, Scope{}, mail("bea@example.com")); status != "queued" {
		t.Error("unsuppressed address still blocked")
	}
	counts, _ := s.OutboxCounts(ctx)
	if counts["suppressed"] != 2 || counts["queued"] != 1 || counts["failed"] != 1 {
		t.Errorf("counts: %v", counts)
	}
}

func TestIssueAndNotifyIsAtomic(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	sc := newClient(t, s, "zcw", "ZCW")
	s.CreateCourse(ctx, sc, Course{Slug: "java", Title: "Java"})
	reqs := []IssueRequest{
		{Email: "a@example.com", FullName: "A", CourseSlug: "java", Cohort: "1", CompletedOn: time.Now()},
		{Email: "b@example.com", FullName: "B", CourseSlug: "java", Cohort: "1", CompletedOn: time.Now()},
	}
	var tokens []string
	notify := func(r IssueResult) (*NewEmail, error) {
		tokens = append(tokens, r.ClaimToken)
		e := mail(r.Email)
		e.Template, e.RefType, e.RefID = "certificate_ready", "certificate", r.CertificateID
		return &e, nil
	}
	res, err := s.IssueAndNotify(ctx, sc, reqs, notify)
	if err != nil || len(res) != 2 || len(tokens) != 2 || tokens[0] == "" {
		t.Fatalf("issue: %+v %v", res, err)
	}
	list, _ := s.ListEmails(ctx, "queued", 10)
	if len(list) != 2 || list[0].ClientSlug != "zcw" || list[0].RefID == "" {
		t.Fatalf("queued: %+v", list)
	}
	// Re-issuing existing certificates sends nothing new.
	s.IssueAndNotify(ctx, sc, reqs, notify)
	if list, _ := s.ListEmails(ctx, "", 10); len(list) != 2 {
		t.Fatalf("re-import queued duplicates: %d", len(list))
	}
	// If building an email fails, nothing is issued or queued.
	_, err = s.IssueAndNotify(ctx, sc, []IssueRequest{{Email: "c@example.com", FullName: "C", CourseSlug: "java",
		Cohort: "1", CompletedOn: time.Now()}}, func(IssueResult) (*NewEmail, error) { return nil, errors.New("template broke") })
	if err == nil || !strings.Contains(err.Error(), "template broke") {
		t.Fatalf("notify error: %v", err)
	}
	if certs, _ := s.ListCertificates(ctx, sc, "", ""); len(certs) != 2 {
		t.Fatalf("certificate committed without its email: %d", len(certs))
	}
}

func TestRemindersOncePerUnclaimedCertificate(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	sc, r := seedScoped(t, s)
	other, _ := s.Issue(ctx, sc, []IssueRequest{{Email: "pub@example.com", FullName: "Pub", CourseSlug: "java",
		Cohort: "J1", CompletedOn: time.Now()}})
	s.SetVisibilityByClaimToken(ctx, other[0].ClaimToken, "public")

	if due, _ := s.DueReminders(ctx, 7*24*time.Hour, 10); len(due) != 0 {
		t.Fatalf("reminders due immediately: %+v", due)
	}
	shiftClock(t, 7*24*time.Hour+time.Minute)
	due, _ := s.DueReminders(ctx, 7*24*time.Hour, 10)
	if len(due) != 1 || due[0].CertificateID != r.CertificateID {
		t.Fatalf("due: %+v (published certificates must be skipped)", due)
	}

	var newToken string
	build := func(c *Certificate, token string) (*NewEmail, error) {
		newToken = token
		e := mail(c.Email)
		return &e, nil
	}
	if err := s.SendReminder(ctx, sc, r.CertificateID, build); err != nil {
		t.Fatal(err)
	}
	if err := s.SendReminder(ctx, sc, r.CertificateID, build); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second reminder: %v", err)
	}
	if due, _ := s.DueReminders(ctx, 7*24*time.Hour, 10); len(due) != 0 {
		t.Fatalf("still due after reminding: %+v", due)
	}
	if c, err := s.GetCertificateByClaimToken(ctx, newToken); err != nil || c.ID != r.CertificateID {
		t.Fatal("reminder link doesn't work")
	}
	if _, err := s.GetCertificateByClaimToken(ctx, r.ClaimToken); !errors.Is(err, ErrNotFound) {
		t.Fatal("old link still works after the reminder replaced it")
	}

	// Clients can turn reminders off; suppressed addresses are skipped.
	sc2 := newClient(t, s, "twa", "TWA")
	off := false
	s.UpdateClient(ctx, ClientInput{Slug: "twa", SendReminders: &off})
	s.CreateCourse(ctx, sc2, Course{Slug: "data", Title: "Data"})
	s.Issue(ctx, sc2, []IssueRequest{{Email: "x@example.com", FullName: "X", CourseSlug: "data", Cohort: "1", CompletedOn: time.Now()}})
	s.Issue(ctx, sc, []IssueRequest{{Email: "bounced@example.com", FullName: "B", CourseSlug: "java", Cohort: "J1", CompletedOn: time.Now()}})
	s.Suppress(ctx, "bounced@example.com", "550", "smtp")
	shiftClock(t, 8*24*time.Hour)
	if due, _ := s.DueReminders(ctx, 7*24*time.Hour, 10); len(due) != 0 {
		t.Fatalf("reminders for opted-out client or suppressed address: %+v", due)
	}
	certs, _ := s.ListCertificates(ctx, sc, "", "")
	for _, c := range certs {
		if c.Email == "bounced@example.com" && !c.EmailBlocked {
			t.Error("EmailBlocked not set for a suppressed student")
		}
	}
}

func TestClientEmailSettings(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	c, err := s.CreateClient(ctx, ClientInput{Slug: "zcw", Name: ptr("Zip"), IDPrefix: ptr("ZCW"), ReplyTo: ptr("info@zipcode.example")})
	if err != nil || c.ReplyTo != "info@zipcode.example" || !c.SendReminders {
		t.Fatalf("create: %+v %v", c, err)
	}
	if _, err := s.UpdateClient(ctx, ClientInput{Slug: "zcw", ReplyTo: ptr("not-an-email")}); !errors.Is(err, ErrInvalid) {
		t.Errorf("bad reply-to: %v", err)
	}
	off := false
	if c, _ = s.UpdateClient(ctx, ClientInput{Slug: "zcw", SendReminders: &off}); c.SendReminders || c.ReplyTo == "" {
		t.Errorf("update reminders only: %+v", c)
	}
}
