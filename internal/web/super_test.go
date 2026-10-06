package web

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"certsforever/internal/store"
)

// fakeDNS answers the domain check from a table.
type fakeDNS struct {
	cname map[string]string
	hosts map[string][]string
}

func (f fakeDNS) LookupCNAME(_ context.Context, host string) (string, error) {
	if c, ok := f.cname[host]; ok {
		return c, nil
	}
	return host + ".", nil
}

func (f fakeDNS) LookupHost(_ context.Context, host string) ([]string, error) {
	if h, ok := f.hosts[host]; ok {
		return h, nil
	}
	return nil, errors.New("no such host")
}

// superBrowser signs in the seeded platform admin with two-step done.
func superBrowser(h *harness) *browser {
	b := h.browser()
	b.signIn("root@platform.example")
	b.enrollTOTP()
	b.get("/super")
	return b
}

func TestSuperRoutesRefuseClientAdmins(t *testing.T) {
	h := newHarness(t)
	h.seedPlatform()
	b := h.browser()
	b.signIn("admin@zcw.example")
	b.get("/admin/zcw")
	for _, p := range []string{"/super", "/super/clients", "/super/clients/zcw", "/super/clients/twa", "/super/users",
		"/super/users/1", "/super/audit", "/super/system"} {
		if resp, page := b.get(p); resp.StatusCode != http.StatusForbidden || strings.Contains(page, "TwinArrows") {
			t.Errorf("GET %s as client admin: %d", p, resp.StatusCode)
		}
	}
	for _, p := range []string{"/super/clients", "/super/clients/zcw", "/super/clients/zcw/status", "/super/clients/twa/act-as",
		"/super/clients/twa/members", "/super/clients/zcw/domains", "/super/users/1/disable", "/super/users/2/grant-super",
		"/super/admins", "/super/act-as/stop"} {
		if resp, _ := b.post(p, url.Values{"email": {"x@y.example"}, "status": {"suspended"}, "host": {"x.example.org"}}); resp.StatusCode != http.StatusForbidden {
			t.Errorf("POST %s as client admin: %d", p, resp.StatusCode)
		}
	}
	// Still a client admin, still not a platform admin, nothing changed.
	u, _ := h.st.GetUserByEmail(context.Background(), "admin@zcw.example")
	if u.IsSuper {
		t.Fatal("client admin became super")
	}
	if c, _ := h.st.GetClient(context.Background(), "zcw"); c.Status != "active" {
		t.Fatal("client admin suspended their own client")
	}
}

func TestSuperClientPage(t *testing.T) {
	h := newHarness(t)
	h.seedPlatform()
	ctx := context.Background()
	b := superBrowser(h)

	resp, page := b.get("/super")
	mustStatus(t, "overview", resp, page, 200)
	if !strings.Contains(page, "TwinArrows") || !strings.Contains(page, "Zip Code Wilmington") {
		t.Fatal("overview lacks clients")
	}

	// Create a client; land on its page.
	resp, page = b.post("/super/clients", url.Values{"name": {"Third School"}, "slug": {"third"}, "id_prefix": {"thd"}})
	mustStatus(t, "create", resp, page, http.StatusSeeOther)
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/super/clients/third") {
		t.Fatalf("after create: %s", loc)
	}
	resp, page = b.post("/super/clients", url.Values{"name": {"Bad"}, "slug": {"Bad Slug"}, "id_prefix": {"BD"}})
	mustStatus(t, "bad create", resp, page, http.StatusBadRequest)
	if !strings.Contains(page, `value="Bad"`) {
		t.Fatal("form not refilled after an error")
	}

	// Edit: the prefix can change until the first certificate.
	b.get("/super/clients/third")
	resp, page = b.post("/super/clients/third", url.Values{"name": {"Third School of Code"}, "id_prefix": {"tsc"},
		"site_url": {"https://third.example"}, "reply_to": {"hi@third.example"}})
	mustStatus(t, "edit", resp, page, http.StatusSeeOther)
	if c, _ := h.st.GetClient(ctx, "third"); c.Name != "Third School of Code" || c.IDPrefix != "TSC" || c.SendReminders {
		t.Fatalf("edit not saved: %+v", c)
	}
	zcw := mustScope(h, "zcw")
	h.st.CreateCourse(ctx, zcw, store.Course{Slug: "java", Title: "Java"})
	h.st.Issue(ctx, zcw, []store.IssueRequest{{Email: "s@example.com", FullName: "S", CourseSlug: "java", Cohort: "C1",
		CompletedOn: mustDate("2026-05-01")}})
	_, page = b.get("/super/clients/zcw")
	if !strings.Contains(page, "fixed: it's in the links") {
		t.Fatal("locked prefix not shown")
	}
	resp, page = b.post("/super/clients/zcw", url.Values{"name": {"Zip Code Wilmington"}, "id_prefix": {"ZIP"}})
	mustStatus(t, "change locked prefix", resp, page, http.StatusBadRequest)
	if c, _ := h.st.GetClient(ctx, "zcw"); c.IDPrefix != "ZCW" {
		t.Fatal("locked prefix changed")
	}

	// Suspend with a reason; the client can't issue; reactivate.
	resp, page = b.post("/super/clients/zcw/status", url.Values{"status": {"suspended"}, "reason": {"contract ended"}})
	mustStatus(t, "suspend", resp, page, http.StatusSeeOther)
	if _, page = b.get("/super/clients/zcw"); !strings.Contains(page, "contract ended") {
		t.Fatal("reason not shown")
	}
	if _, err := h.st.Issue(ctx, mustScope(h, "zcw"), []store.IssueRequest{{Email: "t@example.com", FullName: "T",
		CourseSlug: "java", Cohort: "C1", CompletedOn: mustDate("2026-05-01")}}); !errors.Is(err, store.ErrSuspended) {
		t.Fatalf("suspended client issued: %v", err)
	}
	b.post("/super/clients/zcw/status", url.Values{"status": {"active"}})
	if c, _ := h.st.GetClient(ctx, "zcw"); c.Status != "active" || c.SuspendReason != "" {
		t.Fatalf("reactivate: %+v", c)
	}

	// Admins: invite and remove from the client's page.
	before := h.mail.Count()
	resp, page = b.post("/super/clients/twa/members", url.Values{"email": {"coach@twa.example"}})
	mustStatus(t, "invite", resp, page, http.StatusSeeOther)
	h.waitMail(before + 1)
	u, _ := h.st.GetUserByEmail(ctx, "coach@twa.example")
	if ok, _ := h.st.IsMember(ctx, mustScope(h, "twa"), u.ID); !ok {
		t.Fatal("not a member")
	}
	resp, page = b.post("/super/clients/twa/members/"+itoa(u.ID)+"/remove", nil)
	mustStatus(t, "remove", resp, page, http.StatusSeeOther)
	if ok, _ := h.st.IsMember(ctx, mustScope(h, "twa"), u.ID); ok {
		t.Fatal("still a member")
	}
	if log, _ := h.st.ListAudit(ctx, mustScope(h, "twa"), 1); log[0].Action != "member.remove" || log[0].Impersonated {
		t.Fatalf("remove audit: %+v", log[0])
	}

	if resp, _ := b.get("/super/clients/nope"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown client: %d", resp.StatusCode)
	}
}

func TestSuperDomains(t *testing.T) {
	dns := fakeDNS{
		cname: map[string]string{"certs.zcw.example": "127.0.0.1."}, // the test server's host
		hosts: map[string][]string{"127.0.0.1": {"127.0.0.1"}, "elsewhere.zcw.example": {"203.0.113.9"},
			"sameip.zcw.example": {"127.0.0.1"}},
	}
	h := newHarnessWith(t, &harnessOpts{resolver: dns})
	h.seedPlatform()
	ctx := context.Background()
	b := superBrowser(h)
	zcw := mustScope(h, "zcw")
	b.get("/super/clients/zcw")

	add := func(host string, want int) {
		t.Helper()
		resp, page := b.post("/super/clients/zcw/domains", url.Values{"host": {host}})
		mustStatus(t, "add "+host, resp, page, want)
	}
	add("https://Certs.ZCW.example/", http.StatusSeeOther)
	add("elsewhere.zcw.example", http.StatusSeeOther)
	add("sameip.zcw.example", http.StatusSeeOther)
	add("not a domain", http.StatusBadRequest)
	add("certs.zcw.example", http.StatusBadRequest) // duplicate
	ds, _ := h.st.ListDomains(ctx, zcw)
	id := map[string]string{}
	for _, d := range ds {
		id[d.Host] = itoa(d.ID)
	}
	path := func(host, action string) string { return "/super/clients/zcw/domains/" + id[host] + "/" + action }

	// A CNAME to the platform host verifies; so does the same address.
	resp, page := b.post(path("certs.zcw.example", "check"), nil)
	mustStatus(t, "check cname", resp, page, http.StatusSeeOther)
	resp, page = b.post(path("sameip.zcw.example", "check"), nil)
	mustStatus(t, "check same ip", resp, page, http.StatusSeeOther)
	// Pointing elsewhere doesn't, and says why.
	resp, page = b.post(path("elsewhere.zcw.example", "check"), nil)
	mustStatus(t, "check elsewhere", resp, page, 200)
	if !strings.Contains(page, "points at 203.0.113.9") {
		t.Fatal("no explanation for a failed check")
	}
	if d, _ := h.st.GetDomain(ctx, zcw, mustID(id["elsewhere.zcw.example"])); d.Verified() {
		t.Fatal("wrong DNS verified")
	}
	// Manual verification needs the confirmation box.
	resp, page = b.post(path("elsewhere.zcw.example", "verify"), nil)
	mustStatus(t, "manual without confirm", resp, page, http.StatusBadRequest)
	resp, page = b.post(path("elsewhere.zcw.example", "verify"), url.Values{"confirm": {"on"}})
	mustStatus(t, "manual", resp, page, http.StatusSeeOther)

	resp, page = b.post(path("certs.zcw.example", "canonical"), nil)
	mustStatus(t, "canonical", resp, page, http.StatusSeeOther)
	if sc, d, err := h.st.ClientForHost(ctx, "certs.zcw.example"); err != nil || sc.Client().Slug != "zcw" || !d.Canonical {
		t.Fatalf("canonical: %v", err)
	}
	resp, page = b.post(path("certs.zcw.example", "delete"), nil)
	mustStatus(t, "delete verified", resp, page, http.StatusConflict)
	resp, page = b.post("/super/clients/zcw/domains/platform/canonical", nil)
	mustStatus(t, "back to platform", resp, page, http.StatusSeeOther)

	// Another client's domain id through this client's URL is not found.
	twa := mustScope(h, "twa")
	td, _ := h.st.AddDomain(ctx, twa, "certs.twa.example")
	for _, a := range []string{"check", "verify", "canonical", "delete"} {
		if resp, _ := b.post("/super/clients/zcw/domains/"+itoa(td.ID)+"/"+a, url.Values{"confirm": {"on"}}); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s TWA's domain via ZCW: %d", a, resp.StatusCode)
		}
	}
	if d, _ := h.st.GetDomain(ctx, twa, td.ID); d.Verified() {
		t.Fatal("TWA domain changed")
	}
	if log, _, _ := h.st.SearchAudit(ctx, store.AuditFilter{Client: "zcw", Action: "domain."}); len(log) < 6 {
		t.Fatalf("domain changes not audited: %d", len(log))
	}
}

func mustID(s string) int64 {
	var n int64
	for _, c := range s {
		n = n*10 + int64(c-'0')
	}
	return n
}

func TestSuperPeople(t *testing.T) {
	h := newHarness(t)
	h.seedPlatform()
	ctx := context.Background()
	root := superBrowser(h)

	// The client admin signs in on their own.
	admin := h.browser()
	admin.signIn("admin@zcw.example")
	if resp, _ := admin.get("/admin/zcw"); resp.StatusCode != 200 {
		t.Fatal("admin can't sign in")
	}
	au, _ := h.st.GetUserByEmail(ctx, "admin@zcw.example")
	ru, _ := h.st.GetUserByEmail(ctx, "root@platform.example")

	for q, want := range map[string]string{"": "admin@zcw.example", "?role=super": "root@platform.example", "?q=zed": "admin@zcw.example"} {
		resp, page := root.get("/super/users" + q)
		mustStatus(t, "users"+q, resp, page, 200)
		if !strings.Contains(page, want) {
			t.Errorf("users%s lacks %s", q, want)
		}
	}
	resp, page := root.get("/super/users/" + itoa(au.ID))
	mustStatus(t, "user page", resp, page, 200)
	if !strings.Contains(page, "Zip Code Wilmington") || !strings.Contains(page, "1 active session") {
		t.Fatal("user page lacks clients or sessions")
	}

	// Not on yourself.
	for _, a := range []string{"disable", "reset-2fa", "revoke-super", "sign-out"} {
		resp, _ := root.post("/super/users/"+itoa(ru.ID)+"/"+a, nil)
		if resp.StatusCode != http.StatusConflict {
			t.Errorf("%s on self: %d", a, resp.StatusCode)
		}
	}
	if u, _ := h.st.GetUser(ctx, ru.ID); !u.IsSuper || u.Disabled || !u.TOTPEnabled {
		t.Fatal("self-action changed root")
	}

	// Disable signs them out at once.
	resp, page = root.post("/super/users/"+itoa(au.ID)+"/disable", nil)
	mustStatus(t, "disable", resp, page, http.StatusSeeOther)
	if resp, _ := admin.get("/admin/zcw"); resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Fatalf("disabled user still signed in: %d", resp.StatusCode)
	}
	root.post("/super/users/"+itoa(au.ID)+"/enable", nil)
	if u, _ := h.st.GetUser(ctx, au.ID); u.Disabled {
		t.Fatal("not re-enabled")
	}

	// Make and unmake a platform admin.
	resp, page = root.post("/super/users/"+itoa(au.ID)+"/grant-super", nil)
	mustStatus(t, "grant", resp, page, http.StatusSeeOther)
	if u, _ := h.st.GetUser(ctx, au.ID); !u.IsSuper {
		t.Fatal("not granted")
	}
	root.post("/super/users/"+itoa(au.ID)+"/revoke-super", nil)
	if u, _ := h.st.GetUser(ctx, au.ID); u.IsSuper {
		t.Fatal("not revoked")
	}
	if resp, _ := root.post("/super/users/"+itoa(au.ID)+"/explode", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown action: %d", resp.StatusCode)
	}

	// Each change is in the platform audit log, with who did it.
	log, _, _ := h.st.SearchAudit(ctx, store.AuditFilter{Client: "-", Action: "user."})
	if len(log) != 2 || log[0].Actor != "root@platform.example" {
		t.Fatalf("user actions audit: %+v", log)
	}
	resp, page = root.get("/super/audit?client=-&action=super.")
	mustStatus(t, "audit page", resp, page, 200)
	if !strings.Contains(page, "super.grant") || strings.Contains(page, "user.disable") {
		t.Fatal("audit filter")
	}

	resp, page = root.get("/super/system")
	mustStatus(t, "system", resp, page, 200)
	if !strings.Contains(page, "Migrations") || !strings.Contains(page, "test.db") {
		t.Fatal("system page lacks database info")
	}
}
