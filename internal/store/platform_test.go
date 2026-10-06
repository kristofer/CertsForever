package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestDomains(t *testing.T) {
	ctx := context.Background()
	x := twoClients(t)
	s := x.s

	for in, want := range map[string]string{
		"certs.zipcodewilmington.com":          "certs.zipcodewilmington.com",
		" HTTPS://Certs.Example.ORG/path?q=1 ": "certs.example.org",
		"certs.example.org.":                   "certs.example.org",
	} {
		if got, err := NormalizeHost(in); err != nil || got != want {
			t.Errorf("NormalizeHost(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "localhost", "exa mple.com", "-bad.example.com", "10.0.0.1", "a.b:8080",
		"under_score.example.com", strings.Repeat("a", 64) + ".com"} {
		if _, err := NormalizeHost(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("NormalizeHost(%q) accepted", bad)
		}
	}

	d, err := s.AddDomain(ctx, x.a, "Certs.ZCW.example")
	if err != nil || d.Host != "certs.zcw.example" || d.Verified() {
		t.Fatalf("add: %+v %v", d, err)
	}
	// Hosts are unique across clients, whatever the case.
	if _, err := s.AddDomain(ctx, x.b, "CERTS.zcw.example"); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate host at another client: %v", err)
	}
	// Unverified domains can't be canonical, and don't route.
	if err := s.SetCanonicalDomain(ctx, x.a, d.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("canonical before verify: %v", err)
	}
	if _, _, err := s.ClientForHost(ctx, "certs.zcw.example"); !errors.Is(err, ErrNotFound) {
		t.Fatal("unverified domain routes")
	}
	// Client B can't touch A's domain.
	if err := s.MarkDomainVerified(ctx, x.b, d.ID, "manual"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("B verified A's domain: %v", err)
	}
	if err := s.DeleteDomain(ctx, x.b, d.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("B deleted A's domain: %v", err)
	}
	if err := s.MarkDomainVerified(ctx, x.a, d.ID, "dns"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCanonicalDomain(ctx, x.a, d.ID); err != nil {
		t.Fatal(err)
	}
	sc, got, err := s.ClientForHost(ctx, "CERTS.zcw.example.")
	if err != nil || sc.Client().Slug != "zcw" || !got.Canonical {
		t.Fatalf("route: %v %+v", err, got)
	}
	// A second verified domain takes over as canonical; the first keeps routing.
	d2, _ := s.AddDomain(ctx, x.a, "badges.zcw.example")
	s.MarkDomainVerified(ctx, x.a, d2.ID, "manual")
	if err := s.SetCanonicalDomain(ctx, x.a, d2.ID); err != nil {
		t.Fatal(err)
	}
	list, _ := s.ListDomains(ctx, x.a)
	if len(list) != 2 || list[0].Host != "badges.zcw.example" || !list[0].Canonical || list[1].Canonical {
		t.Fatalf("list: %+v", list)
	}
	if _, _, err := s.ClientForHost(ctx, "certs.zcw.example"); err != nil {
		t.Fatal("retired domain stopped routing")
	}
	// Verified domains are never deleted (even bypassing the store).
	if err := s.DeleteDomain(ctx, x.a, d.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("delete verified: %v", err)
	}
	if _, err := s.wdb.Exec(`DELETE FROM client_domains WHERE id = ?`, d.ID); err == nil {
		t.Fatal("trigger allowed deleting a verified domain")
	}
	if _, err := s.wdb.Exec(`UPDATE client_domains SET client_id = ? WHERE id = ?`, x.b.client.ID, d.ID); err == nil {
		t.Fatal("trigger allowed moving a domain to another client")
	}
	if _, err := s.wdb.Exec(`INSERT INTO client_domains (client_id, host) VALUES (?, 'UPPER.example.org')`, x.a.client.ID); err == nil {
		t.Fatal("check allowed an uppercase host")
	}
	// Unverified ones can be.
	d3, _ := s.AddDomain(ctx, x.a, "typo.zcw.example")
	if err := s.DeleteDomain(ctx, x.a, d3.ID); err != nil {
		t.Fatal(err)
	}
	// Back to the platform domain.
	if err := s.SetCanonicalDomain(ctx, x.a, 0); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.ListDomains(ctx, x.a); list[0].Canonical || list[1].Canonical {
		t.Fatal("still canonical")
	}
}

func TestActingAndSuspendReason(t *testing.T) {
	ctx := context.Background()
	x := twoClients(t)
	s := x.s
	u, _, _ := s.EnsureUser(ctx, "root@example.org", "")
	sid, sess, err := s.CreateSession(ctx, u.ID, true, "", "")
	if err != nil || sess.ActingClientID != 0 {
		t.Fatalf("new session acting: %+v %v", sess, err)
	}
	if err := s.SetActing(ctx, sid, x.b.client.ID); err != nil {
		t.Fatal(err)
	}
	if sess, _ = s.GetSession(ctx, sid); sess.ActingClientID != x.b.client.ID {
		t.Fatal("acting not stored")
	}
	if err := s.SetActing(ctx, sid, 0); err != nil {
		t.Fatal(err)
	}
	if sess, _ = s.GetSession(ctx, sid); sess.ActingClientID != 0 {
		t.Fatal("acting not cleared")
	}

	if err := s.SetClientStatusReason(ctx, "twa", "suspended", "invoice unpaid"); err != nil {
		t.Fatal(err)
	}
	c, _ := s.GetClient(ctx, "twa")
	if c.Status != "suspended" || c.SuspendReason != "invoice unpaid" || c.SuspendedAt == nil {
		t.Fatalf("suspended: %+v", c)
	}
	s.SetClientStatusReason(ctx, "twa", "active", "ignored")
	if c, _ = s.GetClient(ctx, "twa"); c.SuspendReason != "" || c.SuspendedAt != nil {
		t.Fatalf("reactivated: %+v", c)
	}
	if locked, _ := s.PrefixLocked(ctx, x.a); !locked {
		t.Fatal("prefix with certificates not locked")
	}
}

func TestPlatformQueries(t *testing.T) {
	ctx := context.Background()
	x := twoClients(t)
	s := x.s
	a, _, _ := s.EnsureUser(ctx, "ada@zcw.example", "Ada Admin")
	s.AddMember(ctx, x.a, a.ID, nil)
	root, _, _ := s.EnsureUser(ctx, "root@example.org", "")
	s.SetSuperAdmin(ctx, root.ID, true)
	gone, _, _ := s.EnsureUser(ctx, "gone@example.org", "")
	s.SetUserDisabled(ctx, gone.ID, true)

	sums, err := s.ListClientSummaries(ctx)
	if err != nil || len(sums) != 2 {
		t.Fatalf("summaries: %v %+v", err, sums)
	}
	for _, c := range sums {
		if c.Certificates != 1 || c.Issued30d != 1 || c.LastIssued == nil {
			t.Errorf("%s: %+v", c.Slug, c)
		}
		if c.Slug == "zcw" && c.Admins != 1 {
			t.Errorf("zcw admins %d", c.Admins)
		}
	}
	p, err := s.GetPlatformStats(ctx)
	if err != nil || p.Clients != 2 || p.Certificates != 2 || p.Users != 2 || p.SuperAdmins != 1 || p.SupersNoTOTP != 1 {
		t.Fatalf("stats: %+v %v", p, err)
	}
	for f, want := range map[UserFilter]int{{}: 3, {Role: "super"}: 1, {Role: "disabled"}: 1, {Role: "admin"}: 1,
		{Query: "ADA"}: 1, {Query: "%"}: 0} {
		if got, err := s.ListUsers(ctx, f); err != nil || len(got) != want {
			t.Errorf("ListUsers(%+v) = %d %v, want %d", f, len(got), err, want)
		}
	}
	if last, _ := s.LastEnabledSuper(ctx, root.ID); !last {
		t.Fatal("root is the last super")
	}

	for i := 0; i < 5; i++ {
		s.Audit(ctx, x.a, AuditEntry{Actor: "ada@zcw.example", Action: "certificate.revoke"})
		s.Audit(ctx, x.b, AuditEntry{Actor: "bob@twa.example", Action: "course.save"})
	}
	s.Audit(ctx, Scope{}, AuditEntry{Actor: "root@example.org", Action: "super.grant"})
	for f, want := range map[AuditFilter]int{
		{Client: "zcw"}: 5, {Client: "-"}: 1, {Actor: "bob"}: 5, {Action: "certificate."}: 5, {Action: "%"}: 0,
		{Client: "twa", Action: "course"}: 5,
	} {
		if got, _, err := s.SearchAudit(ctx, f); err != nil || len(got) != want {
			t.Errorf("SearchAudit(%+v) = %d %v, want %d", f, len(got), err, want)
		}
	}
	page, more, _ := s.SearchAudit(ctx, AuditFilter{Limit: 4})
	next, _, _ := s.SearchAudit(ctx, AuditFilter{Limit: 4, Before: page[3].ID})
	if len(page) != 4 || !more || next[0].ID >= page[3].ID {
		t.Fatal("audit paging")
	}

	si, err := s.GetSystemInfo(ctx)
	if err != nil || si.SizeBytes == 0 || len(si.Migrations) < 6 || si.Migrations[len(si.Migrations)-1].Version != 6 {
		t.Fatalf("system: %+v %v", si, err)
	}
}
