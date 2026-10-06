package web

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"certsforever/internal/store"
	"certsforever/internal/totp"
)

// browser is a cookie-keeping client that doesn't follow redirects.
type browser struct {
	h      *harness
	c      *http.Client
	csrf   string
	origin string
}

func (h *harness) browser() *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{h: h, origin: h.srv.URL, c: &http.Client{Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

var csrfRE = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

func (b *browser) send(method, path string, form url.Values) (*http.Response, string) {
	b.h.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, _ := http.NewRequest(method, b.h.srv.URL+path, body)
	req.Header.Set("User-Agent", "Mozilla/5.0 test")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if method == http.MethodPost && b.origin != "" {
		req.Header.Set("Origin", b.origin)
	}
	resp, err := b.c.Do(req)
	if err != nil {
		b.h.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	page := string(raw)
	if m := csrfRE.FindStringSubmatch(page); m != nil {
		b.csrf = m[1]
	}
	return resp, page
}

func (b *browser) get(path string) (*http.Response, string) { return b.send("GET", path, nil) }

// post sends a form with the current CSRF token.
func (b *browser) post(path string, form url.Values) (*http.Response, string) {
	if form == nil {
		form = url.Values{}
	}
	if form.Get("csrf") == "" && b.csrf != "" {
		form.Set("csrf", b.csrf)
	}
	return b.send("POST", path, form)
}

var linkRE = regexp.MustCompile(`/login/([A-Za-z0-9_-]{20,})`)

// waitMail waits for the n-th email (mail is sent in the background).
func (h *harness) waitMail(n int) {
	h.t.Helper()
	for i := 0; i < 200 && h.mail.Count() < n; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if h.mail.Count() < n {
		h.t.Fatalf("expected %d emails, got %d", n, h.mail.Count())
	}
}

// signIn requests a link for email, follows it, and returns the redirect target.
func (b *browser) signIn(email string) string {
	b.h.t.Helper()
	before := b.h.mail.Count()
	b.send("POST", "/login", url.Values{"email": {email}})
	b.h.waitMail(before + 1)
	return b.useLink(b.h.mail.Last().Text)
}

func (b *browser) useLink(emailText string) string {
	b.h.t.Helper()
	m := linkRE.FindStringSubmatch(emailText)
	if m == nil {
		b.h.t.Fatalf("no sign-in link in email:\n%s", emailText)
	}
	if resp, _ := b.get("/login/" + m[1]); resp.StatusCode != 200 {
		b.h.t.Fatalf("link page: %d", resp.StatusCode)
	}
	resp, _ := b.send("POST", "/login/"+m[1], url.Values{})
	if resp.StatusCode != http.StatusSeeOther {
		b.h.t.Fatalf("sign in: %d", resp.StatusCode)
	}
	return resp.Header.Get("Location")
}

var secretRE = regexp.MustCompile(`id="totp-secret">([A-Z2-7 ]+)<`)
var sealedRE = regexp.MustCompile(`name="sealed" value="([^"]+)"`)

// enrollTOTP completes authenticator setup and returns the secret.
func (b *browser) enrollTOTP() string {
	b.h.t.Helper()
	_, page := b.get("/account/totp")
	sm, sl := secretRE.FindStringSubmatch(page), sealedRE.FindStringSubmatch(page)
	if sm == nil || sl == nil {
		b.h.t.Fatalf("setup page missing secret:\n%s", page)
	}
	secret := strings.ReplaceAll(sm[1], " ", "")
	code, _ := totp.Code(secret, totp.Counter(time.Now()))
	resp, body := b.post("/account/totp", url.Values{"sealed": {sl[1]}, "code": {code}})
	if resp.StatusCode != http.StatusSeeOther {
		b.h.t.Fatalf("enroll: %d %s", resp.StatusCode, body)
	}
	return secret
}

// seedPlatform creates two clients, a client admin for zcw, and a super admin.
func (h *harness) seedPlatform() {
	h.t.Helper()
	ctx := context.Background()
	for _, c := range [][3]string{{"zcw", "Zip Code Wilmington", "ZCW"}, {"twa", "TwinArrows", "TWA"}} {
		name, prefix := c[1], c[2]
		if _, err := h.st.CreateClient(ctx, store.ClientInput{Slug: c[0], Name: &name, IDPrefix: &prefix}); err != nil {
			h.t.Fatal(err)
		}
	}
	zcw, _ := h.st.Scope(ctx, "zcw")
	u, _, _ := h.st.EnsureUser(ctx, "admin@zcw.example", "Zed Admin")
	h.st.AddMember(ctx, zcw, u.ID, nil)
	su, _, _ := h.st.EnsureUser(ctx, "root@platform.example", "")
	h.st.SetSuperAdmin(ctx, su.ID, true)
}

func TestSignInWithEmailLink(t *testing.T) {
	h := newHarness(t)
	h.seedPlatform()
	b := h.browser()

	// Signed-out visitors are sent to sign in.
	if resp, _ := b.get("/admin"); resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Fatalf("/admin signed out: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}

	// Unknown addresses get the same answer and no email.
	_, page := b.send("POST", "/login", url.Values{"email": {"nobody@example.com"}})
	if !strings.Contains(page, "Check your email") {
		t.Fatal("unknown email: different response")
	}
	time.Sleep(50 * time.Millisecond)
	if h.mail.Count() != 0 {
		t.Fatal("email sent to an unknown address")
	}

	// Known address: a link arrives. Opening it (GET) doesn't sign in or use it up.
	b.send("POST", "/login", url.Values{"email": {"ADMIN@zcw.example"}})
	h.waitMail(1)
	msg := h.mail.Last()
	if msg.To != "admin@zcw.example" || !strings.Contains(msg.Subject, "Sign in") {
		t.Fatalf("email: %+v", msg)
	}
	token := linkRE.FindStringSubmatch(msg.Text)[1]
	for i := 0; i < 2; i++ {
		resp, page := b.get("/login/" + token)
		if resp.StatusCode != 200 || !strings.Contains(page, "Sign in as admin@zcw.example") {
			t.Fatalf("confirm page: %d", resp.StatusCode)
		}
		// Regression: with "no-referrer", Chrome posts "Origin: null" and
		// the same-origin check blocks every real sign-in.
		if rp := resp.Header.Get("Referrer-Policy"); rp != "same-origin" {
			t.Fatalf("confirm page Referrer-Policy = %q, want same-origin", rp)
		}
	}
	b.origin = "null"
	if resp, _ := b.send("POST", "/login/"+token, url.Values{}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("Origin: null accepted: %d", resp.StatusCode)
	}
	b.origin = h.srv.URL
	if resp, _ := b.get("/admin"); resp.StatusCode != http.StatusSeeOther {
		t.Fatal("GET of the link signed in")
	}

	// A cross-site POST of the link is refused.
	b.origin = "https://evil.example"
	if resp, _ := b.send("POST", "/login/"+token, url.Values{}); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site link POST: %d", resp.StatusCode)
	}
	b.origin = h.srv.URL

	resp, _ := b.send("POST", "/login/"+token, url.Values{})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/admin" {
		t.Fatalf("sign in: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	// Single use.
	if resp, _ := h.browser().send("POST", "/login/"+token, url.Values{}); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("link reused: %d", resp.StatusCode)
	}

	// One client: /admin goes straight to it.
	resp, _ = b.get("/admin")
	if resp.Header.Get("Location") != "/admin/zcw" {
		t.Fatalf("/admin -> %s", resp.Header.Get("Location"))
	}
	if resp, page := b.get("/admin/zcw"); resp.StatusCode != 200 || !strings.Contains(page, "Zip Code Wilmington") {
		t.Fatalf("console: %d", resp.StatusCode)
	}

	// Sign out ends the session.
	if resp, _ := b.post("/logout", nil); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("logout: %d", resp.StatusCode)
	}
	if resp, _ := b.get("/admin/zcw"); resp.StatusCode != http.StatusSeeOther {
		t.Fatal("still signed in after logout")
	}

	// Sign-in is in the audit log.
	all, _ := h.st.ListAllAudit(context.Background(), 10)
	var actions []string
	for _, e := range all {
		actions = append(actions, e.Action)
	}
	if !strings.Contains(strings.Join(actions, ","), "auth.sign_in") || !strings.Contains(strings.Join(actions, ","), "auth.sign_out") {
		t.Errorf("audit: %v", actions)
	}
}

func TestClientAdminIsConfinedToTheirClient(t *testing.T) {
	h := newHarness(t)
	h.seedPlatform()
	b := h.browser()
	b.signIn("admin@zcw.example")
	b.get("/admin/zcw") // picks up a CSRF token

	for _, path := range []string{"/admin/twa", "/admin/nope"} {
		resp, page := b.get(path)
		if resp.StatusCode != http.StatusNotFound || strings.Contains(page, "TwinArrows") {
			t.Errorf("GET %s = %d (other clients must look nonexistent)", path, resp.StatusCode)
		}
	}
	if resp, _ := b.post("/admin/twa/members", url.Values{"email": {"mole@example.com"}}); resp.StatusCode != http.StatusNotFound {
		t.Errorf("invited into another client: %d", resp.StatusCode)
	}
	twa, _ := h.st.Scope(context.Background(), "twa")
	if list, _ := h.st.ListMembers(context.Background(), twa); len(list) != 0 {
		t.Fatalf("TWA gained members: %+v", list)
	}
	for _, path := range []string{"/super"} {
		if resp, _ := b.get(path); resp.StatusCode != http.StatusForbidden {
			t.Errorf("client admin reached %s: %d", path, resp.StatusCode)
		}
	}
	for _, path := range []string{"/super/clients", "/super/admins"} {
		if resp, _ := b.post(path, url.Values{"email": {"me@example.com"}, "slug": {"x1"}, "name": {"x"}, "id_prefix": {"XX"}}); resp.StatusCode != http.StatusForbidden {
			t.Errorf("client admin posted to %s: %d", path, resp.StatusCode)
		}
	}
	if list, _ := h.st.ListSuperAdmins(context.Background()); len(list) != 1 {
		t.Fatal("client admin made a super admin")
	}
}

func TestCSRFIsRequired(t *testing.T) {
	h := newHarness(t)
	h.seedPlatform()
	b := h.browser()
	b.signIn("admin@zcw.example")
	b.get("/admin/zcw")

	form := url.Values{"email": {"new@zcw.example"}, "csrf": {"wrong"}}
	if resp, _ := b.send("POST", "/admin/zcw/members", form); resp.StatusCode != http.StatusForbidden {
		t.Errorf("bad CSRF token accepted: %d", resp.StatusCode)
	}
	form.Del("csrf")
	if resp, _ := b.send("POST", "/admin/zcw/members", form); resp.StatusCode != http.StatusForbidden {
		t.Errorf("missing CSRF token accepted: %d", resp.StatusCode)
	}
	b.origin = "https://evil.example"
	if resp, _ := b.post("/admin/zcw/members", url.Values{"email": {"new@zcw.example"}}); resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-origin POST accepted: %d", resp.StatusCode)
	}
	if u, err := h.st.GetUserByEmail(context.Background(), "new@zcw.example"); err == nil {
		t.Fatalf("user created by a forged request: %+v", u)
	}
}

func TestInviteFlow(t *testing.T) {
	h := newHarness(t)
	h.seedPlatform()
	admin := h.browser()
	admin.signIn("admin@zcw.example")
	admin.get("/admin/zcw")

	before := h.mail.Count()
	resp, _ := admin.post("/admin/zcw/members", url.Values{"email": {"new@zcw.example"}, "name": {"New Person"}})
	if resp.StatusCode != http.StatusSeeOther || !strings.Contains(resp.Header.Get("Location"), "notice=invited") {
		t.Fatalf("invite: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	h.waitMail(before + 1)
	invite := h.mail.Last()
	if invite.To != "new@zcw.example" || !strings.Contains(invite.Text, "Zed Admin added you") {
		t.Fatalf("invite email: %+v", invite)
	}

	// The invitee signs in with it and lands in Zip Code's console.
	newbie := h.browser()
	if loc := newbie.useLink(invite.Text); loc != "/admin" {
		t.Fatalf("invitee redirected to %s", loc)
	}
	if resp, page := newbie.get("/admin/zcw"); resp.StatusCode != 200 || !strings.Contains(page, "new@zcw.example") {
		t.Fatalf("invitee console: %d", resp.StatusCode)
	}

	// The inviter removes them; their access ends.
	zcw, _ := h.st.Scope(context.Background(), "zcw")
	u, _ := h.st.GetUserByEmail(context.Background(), "new@zcw.example")
	resp, _ = admin.post("/admin/zcw/members/"+itoa(u.ID)+"/remove", nil)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("remove: %d", resp.StatusCode)
	}
	if resp, _ := newbie.get("/admin/zcw"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("removed admin still has access: %d", resp.StatusCode)
	}

	log, _ := h.st.ListAudit(context.Background(), zcw, 10)
	if len(log) < 2 || log[0].Action != "member.remove" || log[1].Action != "member.invite" ||
		log[1].Actor != "admin@zcw.example" || log[1].TargetID != "new@zcw.example" {
		t.Fatalf("client audit: %+v", log)
	}
}

func TestSuperAdminNeedsTwoStepVerification(t *testing.T) {
	h := newHarness(t)
	h.seedPlatform()
	b := h.browser()

	// First sign-in: sent to set up an authenticator; nothing else works yet.
	if loc := b.signIn("root@platform.example"); loc != "/account/totp" {
		t.Fatalf("first sign-in -> %s", loc)
	}
	for _, path := range []string{"/super", "/admin/zcw", "/admin"} {
		if resp, _ := b.get(path); resp.Header.Get("Location") != "/account/totp" {
			t.Errorf("%s before enrolment -> %d %s", path, resp.StatusCode, resp.Header.Get("Location"))
		}
	}

	// A wrong code doesn't enrol.
	_, page := b.get("/account/totp")
	sealed := sealedRE.FindStringSubmatch(page)[1]
	if resp, _ := b.post("/account/totp", url.Values{"sealed": {sealed}, "code": {"000000"}}); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("wrong enrolment code: %d", resp.StatusCode)
	}
	// A setup form from another session can't be used.
	other := h.browser()
	other.signIn("root@platform.example")
	other.get("/account/totp")
	if resp, _ := other.post("/account/totp", url.Values{"sealed": {sealed}, "code": {"123456"}}); resp.StatusCode != http.StatusSeeOther ||
		resp.Header.Get("Location") != "/account/totp" {
		t.Fatalf("cross-session sealed secret: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}

	secret := b.enrollTOTP()
	if resp, page := b.get("/super"); resp.StatusCode != 200 || !strings.Contains(page, "TwinArrows") {
		t.Fatalf("/super after enrolment: %d", resp.StatusCode)
	}

	// Next sign-in requires a code; the code used at enrolment can't be replayed.
	b2 := h.browser()
	if loc := b2.signIn("root@platform.example"); loc != "/login/totp" {
		t.Fatalf("second sign-in -> %s", loc)
	}
	if resp, _ := b2.get("/super"); resp.Header.Get("Location") != "/login/totp" {
		t.Fatal("super console reachable before the code")
	}
	b2.get("/login/totp")
	now := totp.Counter(time.Now())
	used, _ := totp.Code(secret, now)
	if resp, _ := b2.post("/login/totp", url.Values{"code": {used}}); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("replayed code accepted: %d", resp.StatusCode)
	}
	next, _ := totp.Code(secret, now+1) // next step, inside the ±1 window
	if resp, _ := b2.post("/login/totp", url.Values{"code": {next}}); resp.StatusCode != http.StatusSeeOther ||
		resp.Header.Get("Location") != "/super" {
		t.Fatalf("valid code: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp, _ := b2.get("/super"); resp.StatusCode != 200 {
		t.Fatalf("/super after code: %d", resp.StatusCode)
	}
}

func TestSuperAdminActingInAClientIsAudited(t *testing.T) {
	h := newHarness(t)
	h.seedPlatform()
	b := h.browser()
	b.signIn("root@platform.example")
	b.enrollTOTP()

	resp, page := b.get("/admin/twa")
	if resp.StatusCode != 200 || !strings.Contains(page, "as a platform administrator") {
		t.Fatalf("super in TWA console: %d", resp.StatusCode)
	}
	b.post("/admin/twa/members", url.Values{"email": {"first@twa.example"}})
	twa, _ := h.st.Scope(context.Background(), "twa")
	log, _ := h.st.ListAudit(context.Background(), twa, 5)
	if len(log) == 0 || log[0].Action != "member.invite" || !log[0].Impersonated || log[0].Actor != "root@platform.example" {
		t.Fatalf("impersonated action not flagged: %+v", log)
	}
}

func TestSuperConsoleManagesClientsAndAdmins(t *testing.T) {
	h := newHarness(t)
	h.seedPlatform()
	b := h.browser()
	b.signIn("root@platform.example")
	b.enrollTOTP()
	b.get("/super")

	before := h.mail.Count()
	resp, _ := b.post("/super/clients", url.Values{"name": {"Third School"}, "slug": {"third"}, "id_prefix": {"thd"},
		"admin_email": {"boss@third.example"}})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("create client: %d", resp.StatusCode)
	}
	h.waitMail(before + 1)
	if m := h.mail.Last(); m.To != "boss@third.example" || !strings.Contains(m.Subject, "Third School") {
		t.Fatalf("first-admin invite: %+v", m)
	}
	third, err := h.st.Scope(context.Background(), "third")
	if err != nil || third.Client().IDPrefix != "THD" {
		t.Fatalf("client: %+v %v", third.Client(), err)
	}
	if list, _ := h.st.ListMembers(context.Background(), third); len(list) != 1 {
		t.Fatalf("first admin not added: %+v", list)
	}

	// Duplicate prefix: shown as an error, nothing created.
	resp, page := b.post("/super/clients", url.Values{"name": {"Dup"}, "slug": {"dup"}, "id_prefix": {"THD"}})
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(page, "already exists") {
		t.Fatalf("duplicate prefix: %d", resp.StatusCode)
	}

	// Suspend.
	if resp, _ := b.post("/super/clients/third/status", url.Values{"status": {"suspended"}}); resp.StatusCode != http.StatusSeeOther {
		t.Fatal("suspend")
	}
	if c, _ := h.st.GetClient(context.Background(), "third"); c.Status != "suspended" {
		t.Fatal("not suspended")
	}

	// The last super admin can't remove themselves.
	root, _ := h.st.GetUserByEmail(context.Background(), "root@platform.example")
	if resp, _ := b.post("/super/admins/"+itoa(root.ID)+"/remove", nil); resp.StatusCode != http.StatusConflict {
		t.Fatalf("removed last super admin: %d", resp.StatusCode)
	}
	// Add a second, then removal works.
	b.post("/super/admins", url.Values{"email": {"second@platform.example"}})
	if list, _ := h.st.ListSuperAdmins(context.Background()); len(list) != 2 {
		t.Fatal("second super admin not added")
	}
	all, _ := h.st.ListAllAudit(context.Background(), 20)
	got := map[string]bool{}
	for _, e := range all {
		got[e.Action] = true
	}
	for _, want := range []string{"client.create", "member.invite", "client.status", "super.grant", "auth.totp_enabled"} {
		if !got[want] {
			t.Errorf("audit missing %s", want)
		}
	}
}

func TestSignInRequestsAreRateLimited(t *testing.T) {
	h := newHarness(t)
	h.seedPlatform()
	b := h.browser()
	for i := 0; i < 8; i++ {
		_, page := b.send("POST", "/login", url.Values{"email": {"admin@zcw.example"}})
		if !strings.Contains(page, "Check your email") {
			t.Fatal("rate-limited response must look the same")
		}
	}
	time.Sleep(100 * time.Millisecond)
	if n := h.mail.Count(); n != 3 {
		t.Fatalf("sent %d emails to one address in a burst, want 3", n)
	}
}

func TestAdminTokenActionsAreAudited(t *testing.T) {
	h := newHarness(t)
	id, _ := h.issueOne("zcw", "ZCW", "Zip Code Wilmington", "https://zipcode.example", "java", "Ada")
	h.do("POST", "/admin/api/clients/zcw/certificates/"+id+"/revoke", `{"reason":"duplicate"}`, "application/json", true)
	zcw, _ := h.st.Scope(context.Background(), "zcw")
	log, _ := h.st.ListAudit(context.Background(), zcw, 10)
	if len(log) == 0 || log[0].Action != "certificate.revoke" || log[0].Actor != "admin-token" ||
		log[0].TargetID != id || log[0].Details["reason"] != "duplicate" {
		t.Fatalf("audit: %+v", log)
	}
	var actions []string
	for _, e := range log {
		actions = append(actions, e.Action)
	}
	for _, want := range []string{"client.create", "course.save", "certificates.issue"} {
		if !strings.Contains(strings.Join(actions, " "), want) {
			t.Errorf("missing %s in %v", want, actions)
		}
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
