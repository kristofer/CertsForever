package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// withClock runs the rest of the test with the store's clock shifted.
func shiftClock(t *testing.T, d time.Duration) {
	t.Helper()
	prev := clock
	clock = func() time.Time { return prev().Add(d) }
	t.Cleanup(func() { clock = prev })
}

func TestEnsureUser(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	u, created, err := s.EnsureUser(ctx, " Ada@Example.com ", "")
	if err != nil || !created || u.Email != "Ada@Example.com" {
		t.Fatalf("create: %+v %v %v", u, created, err)
	}
	u2, created, err := s.EnsureUser(ctx, "ada@example.com", "Ada Lovelace")
	if err != nil || created || u2.ID != u.ID || u2.Name != "Ada Lovelace" {
		t.Fatalf("existing (case-insensitive) fills name: %+v %v %v", u2, created, err)
	}
	u3, _, _ := s.EnsureUser(ctx, "ada@example.com", "Someone Else")
	if u3.Name != "Ada Lovelace" {
		t.Errorf("name overwritten: %q", u3.Name)
	}
	for _, bad := range []string{"", "ada", "@example.com", "ada@", "ada@localhost", "a b@example.com", "<a@b.c>"} {
		if _, _, err := s.EnsureUser(ctx, bad, ""); !errors.Is(err, ErrInvalid) {
			t.Errorf("EnsureUser(%q) = %v, want ErrInvalid", bad, err)
		}
	}
}

func TestLoginTokensAreSingleUseAndExpire(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	u, _, _ := s.EnsureUser(ctx, "ada@example.com", "")

	tok, err := s.CreateLoginToken(ctx, u.ID, "login", LoginLinkTTL)
	if err != nil {
		t.Fatal(err)
	}
	// Peeking (the GET from a mail scanner) doesn't use it up.
	for i := 0; i < 3; i++ {
		if pu, err := s.PeekLoginToken(ctx, tok); err != nil || pu.ID != u.ID {
			t.Fatalf("peek: %v", err)
		}
	}
	got, purpose, err := s.ConsumeLoginToken(ctx, tok)
	if err != nil || got.ID != u.ID || purpose != "login" {
		t.Fatalf("consume: %+v %q %v", got, purpose, err)
	}
	if u2, _ := s.GetUser(ctx, u.ID); u2.LastLoginAt == nil {
		t.Error("last_login_at not set")
	}
	if _, _, err := s.ConsumeLoginToken(ctx, tok); !errors.Is(err, ErrNotFound) {
		t.Errorf("second use: %v", err)
	}
	if _, err := s.PeekLoginToken(ctx, tok); !errors.Is(err, ErrNotFound) {
		t.Errorf("peek after use: %v", err)
	}

	// Expiry.
	tok, _ = s.CreateLoginToken(ctx, u.ID, "login", LoginLinkTTL)
	shiftClock(t, LoginLinkTTL+time.Second)
	if _, _, err := s.ConsumeLoginToken(ctx, tok); !errors.Is(err, ErrNotFound) {
		t.Errorf("expired token: %v", err)
	}
}

func TestDisabledUsersCantSignIn(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	u, _, _ := s.EnsureUser(ctx, "ada@example.com", "")
	tok, _ := s.CreateLoginToken(ctx, u.ID, "invite", InviteLinkTTL)
	sid, _, _ := s.CreateSession(ctx, u.ID, false, "", "")
	if err := s.SetUserDisabled(ctx, u.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ConsumeLoginToken(ctx, tok); !errors.Is(err, ErrNotFound) {
		t.Errorf("disabled user consumed a link: %v", err)
	}
	if _, err := s.GetSession(ctx, sid); !errors.Is(err, ErrNotFound) {
		t.Errorf("disabled user's session survived: %v", err)
	}
	s.SetUserDisabled(ctx, u.ID, false)
	if _, _, err := s.ConsumeLoginToken(ctx, tok); !errors.Is(err, ErrNotFound) {
		t.Errorf("links voided at disable must stay void: %v", err)
	}
}

func TestSessionsExpire(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	u, _, _ := s.EnsureUser(ctx, "ada@example.com", "")
	sid, sess, err := s.CreateSession(ctx, u.ID, false, "1.2.3.4", strings.Repeat("x", 1000))
	if err != nil || sess.User.ID != u.ID || sess.CSRF == "" || sess.TOTPVerified {
		t.Fatalf("create: %+v %v", sess, err)
	}
	if err := s.MarkSessionTOTPVerified(ctx, sid); err != nil {
		t.Fatal(err)
	}
	if sess, _ = s.GetSession(ctx, sid); !sess.TOTPVerified {
		t.Error("totp flag not saved")
	}
	if _, err := s.GetSession(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown session: %v", err)
	}

	// Idle timeout.
	shiftClock(t, SessionIdle+time.Minute)
	if _, err := s.GetSession(ctx, sid); !errors.Is(err, ErrNotFound) {
		t.Errorf("idle session still valid: %v", err)
	}
}

func TestSessionAbsoluteLimitAndSignOutEverywhere(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	u, _, _ := s.EnsureUser(ctx, "ada@example.com", "")
	keep, _, _ := s.CreateSession(ctx, u.ID, false, "", "")
	other, _, _ := s.CreateSession(ctx, u.ID, false, "", "")
	if n, err := s.DeleteUserSessions(ctx, u.ID, keep); err != nil || n != 1 {
		t.Fatalf("sign out elsewhere: %d %v", n, err)
	}
	if _, err := s.GetSession(ctx, other); !errors.Is(err, ErrNotFound) {
		t.Error("other session survived")
	}

	// Activity keeps it alive past the idle limit, but not past the absolute limit.
	base := clock
	t.Cleanup(func() { clock = base })
	for elapsed := time.Duration(0); elapsed < SessionMax; elapsed += 10 * time.Hour {
		e := elapsed
		clock = func() time.Time { return base().Add(e) }
		if _, err := s.GetSession(ctx, keep); err != nil {
			t.Fatalf("active session dropped at %v: %v", e, err)
		}
		s.TouchSession(ctx, keep)
	}
	clock = func() time.Time { return base().Add(SessionMax + time.Minute) }
	if _, err := s.GetSession(ctx, keep); !errors.Is(err, ErrNotFound) {
		t.Errorf("session outlived the absolute limit: %v", err)
	}
	if err := s.PruneAuth(ctx); err != nil {
		t.Fatal(err)
	}
	var n int
	s.wdb.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&n)
	if n != 0 {
		t.Errorf("prune left %d sessions", n)
	}
}

func TestLastSuperAdminCantBeRemoved(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	a, _, _ := s.EnsureUser(ctx, "a@example.com", "")
	b, _, _ := s.EnsureUser(ctx, "b@example.com", "")
	s.SetSuperAdmin(ctx, a.ID, true)
	if err := s.SetSuperAdmin(ctx, a.ID, false); !errors.Is(err, ErrConflict) {
		t.Fatalf("removed the last super admin: %v", err)
	}
	s.SetSuperAdmin(ctx, b.ID, true)
	if err := s.SetSuperAdmin(ctx, a.ID, false); err != nil {
		t.Fatalf("remove with another super present: %v", err)
	}
	if list, _ := s.ListSuperAdmins(ctx); len(list) != 1 || list[0].ID != b.ID {
		t.Errorf("supers: %+v", list)
	}
}

func TestTOTPCounterBlocksReplay(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	u, _, _ := s.EnsureUser(ctx, "a@example.com", "")
	if _, err := s.TOTPSecret(ctx, u.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("secret before enrolment")
	}
	if err := s.EnableTOTP(ctx, u.ID, []byte("enc"), 100); err != nil {
		t.Fatal(err)
	}
	if enc, _ := s.TOTPSecret(ctx, u.ID); string(enc) != "enc" {
		t.Error("secret not stored")
	}
	if err := s.UseTOTPCounter(ctx, u.ID, 100); !errors.Is(err, ErrConflict) {
		t.Errorf("enrolment code replayed: %v", err)
	}
	if err := s.UseTOTPCounter(ctx, u.ID, 101); err != nil {
		t.Fatal(err)
	}
	if err := s.UseTOTPCounter(ctx, u.ID, 101); !errors.Is(err, ErrConflict) {
		t.Errorf("replay: %v", err)
	}
	if err := s.ResetTOTP(ctx, u.ID); err != nil {
		t.Fatal(err)
	}
	if u, _ = s.GetUser(ctx, u.ID); u.TOTPEnabled {
		t.Error("reset didn't clear TOTP")
	}
}

func TestMembershipsAreScoped(t *testing.T) {
	ctx := context.Background()
	x := twoClients(t)
	s := x.s
	u, _, _ := s.EnsureUser(ctx, "admin@zcw.example", "")
	if err := s.AddMember(ctx, x.a, u.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, x.a, u.ID, nil); err != nil {
		t.Errorf("re-adding should be a no-op: %v", err)
	}
	if ok, _ := s.IsMember(ctx, x.a, u.ID); !ok {
		t.Error("not a member of A")
	}
	if ok, _ := s.IsMember(ctx, x.b, u.ID); ok {
		t.Error("member of B")
	}
	if list, _ := s.ListMembers(ctx, x.b); len(list) != 0 {
		t.Errorf("B lists A's admin: %+v", list)
	}
	if err := s.RemoveMember(ctx, x.b, u.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("B removed A's admin: %v", err)
	}
	if cs, _ := s.ClientsForUser(ctx, u.ID); len(cs) != 1 || cs[0].Slug != "zcw" {
		t.Errorf("clients for user: %+v", cs)
	}
	if err := s.RemoveMember(ctx, x.a, u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListMembers(ctx, Scope{}); !errors.Is(err, ErrNoScope) {
		t.Errorf("zero scope: %v", err)
	}
}

func TestAuditLogIsAppendOnlyAndScoped(t *testing.T) {
	ctx := context.Background()
	x := twoClients(t)
	s := x.s
	u, _, _ := s.EnsureUser(ctx, "ops@example.com", "")
	uid := u.ID
	if err := s.Audit(ctx, x.a, AuditEntry{ActorUserID: &uid, Actor: u.Email, Action: "certificate.revoke",
		TargetType: "certificate", TargetID: x.aCert.CertificateID, Details: map[string]any{"reason": "test"}}); err != nil {
		t.Fatal(err)
	}
	s.Audit(ctx, x.b, AuditEntry{Actor: "cli", Action: "course.create", Impersonated: true})
	s.Audit(ctx, Scope{}, AuditEntry{Actor: "cli", Action: "client.create"})

	la, _ := s.ListAudit(ctx, x.a, 50)
	if len(la) != 1 || la[0].Action != "certificate.revoke" || la[0].ClientSlug != "zcw" ||
		la[0].Details["reason"] != "test" || *la[0].ActorUserID != uid {
		t.Errorf("A's audit: %+v", la)
	}
	all, _ := s.ListAllAudit(ctx, 50)
	if len(all) != 3 || all[0].Action != "client.create" || !all[1].Impersonated {
		t.Errorf("all audit (newest first): %+v", all)
	}
	if _, err := s.wdb.Exec(`UPDATE audit_log SET action = 'nothing'`); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Errorf("audit update allowed: %v", err)
	}
	if _, err := s.wdb.Exec(`DELETE FROM audit_log`); err == nil || !strings.Contains(err.Error(), "append-only") {
		t.Errorf("audit delete allowed: %v", err)
	}
}
