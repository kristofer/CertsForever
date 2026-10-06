package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"certsforever/internal/config"
	"certsforever/internal/emails"
	"certsforever/internal/mail"
	"certsforever/internal/outbox"
	"certsforever/internal/secretbox"
	"certsforever/internal/store"
)

const adminToken = "test-admin-token"

type harness struct {
	t    *testing.T
	srv  *httptest.Server
	st   *store.Store
	mail *mail.Recorder
}

func newHarness(t *testing.T) *harness { return newHarnessWith(t, nil) }

// harnessOpts adjusts a test server.
type harnessOpts struct {
	noEmail  bool // run with email off (no queue), like production without SMTP
	cfg      func(*config.Config)
	resolver Resolver // DNS for the domain check
}

func newHarnessWith(t *testing.T, o *harnessOpts) *harness {
	t.Helper()
	if o == nil {
		o = &harnessOpts{}
	}
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := config.FromEnv()
	cfg.AdminToken = adminToken
	cfg.MasterKey = bytes.Repeat([]byte{7}, 32)
	if o.cfg != nil {
		o.cfg(&cfg)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	rec := &mail.Recorder{}
	box, _ := secretbox.New(cfg.MasterKey)
	var q *outbox.Queue
	if !o.noEmail {
		q = outbox.NewQueue(st, box)
	}
	s, err := New(cfg, st, log, Deps{Queue: q, Resolver: o.resolver})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	s.cfg.BaseURL = ts.URL
	if q != nil {
		worker := &outbox.Worker{Queue: q, Sender: rec, Log: log, Poll: 20 * time.Millisecond,
			Platform: emails.Platform{Name: cfg.PlatformName, BaseURL: ts.URL}}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); worker.Run(ctx) }()
		t.Cleanup(func() { cancel(); <-done }) // stop before the store closes
	}
	return &harness{t: t, srv: ts, st: st, mail: rec}
}

// client does not follow redirects so tests can assert on them.
var client = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func (h *harness) do(method, path, body, contentType string, admin bool) (*http.Response, string) {
	h.t.Helper()
	req, _ := http.NewRequest(method, h.srv.URL+path, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 test")
	if admin {
		req.Header.Set("Authorization", "Bearer "+adminToken)
	}
	resp, err := client.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestCertificateLifecycle(t *testing.T) {
	h := newHarness(t)

	// Admin auth is enforced.
	if resp, _ := h.do("GET", "/admin/api/clients", "", "", false); resp.StatusCode != 401 {
		t.Fatalf("stats without token: %d", resp.StatusCode)
	}

	// Create a client and a course, then import a cohort.
	resp, body := h.do("POST", "/admin/api/clients",
		`{"slug":"zcw","name":"Zip Code Wilmington","id_prefix":"ZCW","site_url":"https://example.org/apply","blurb":"A coding school."}`,
		"application/json", true)
	if resp.StatusCode != 201 {
		t.Fatalf("create client: %d %s", resp.StatusCode, body)
	}
	resp, body = h.do("POST", "/admin/api/clients/zcw/courses",
		`{"slug":"java","title":"Java Full-Stack Developer","skills":["Java","Spring","SQL"]}`, "application/json", true)
	if resp.StatusCode != 200 {
		t.Fatalf("create course: %d %s", resp.StatusCode, body)
	}
	csv := "email,full_name,course,cohort,completed_on\nada@example.com,Ada Lovelace,java,Java 13,2026-09-30\n"
	resp, body = h.do("POST", "/admin/api/clients/zcw/import", csv, "text/csv", true)
	if resp.StatusCode != 200 {
		t.Fatalf("import: %d %s", resp.StatusCode, body)
	}
	var links []IssuedLink
	if err := json.Unmarshal([]byte(body), &links); err != nil || len(links) != 1 {
		t.Fatalf("import body: %s (%v)", body, err)
	}
	id := links[0].CertificateID
	claimPath := strings.TrimPrefix(links[0].ClaimURL, h.srv.URL)
	if !strings.HasPrefix(id, "ZCW-") || !strings.HasPrefix(claimPath, "/claim/") {
		t.Fatalf("unexpected ids: %+v", links[0])
	}

	// Re-import is idempotent.
	_, body = h.do("POST", "/admin/api/clients/zcw/import", csv, "text/csv", true)
	if !strings.Contains(body, `"existing": true`) || !strings.Contains(body, id) {
		t.Fatalf("re-import should return existing cert: %s", body)
	}

	// Private by default: public page 404s, as does the image.
	if resp, _ := h.do("GET", "/c/"+id, "", "", false); resp.StatusCode != 404 {
		t.Fatalf("private cert page: %d", resp.StatusCode)
	}
	if resp, _ := h.do("GET", "/c/"+id+"/og.png", "", "", false); resp.StatusCode != 404 {
		t.Fatalf("private og image: %d", resp.StatusCode)
	}

	// Student opens the claim page and publishes.
	resp, body = h.do("GET", claimPath, "", "", false)
	if resp.StatusCode != 200 || !strings.Contains(body, "Publish my certificate") {
		t.Fatalf("claim page: %d", resp.StatusCode)
	}
	resp, _ = h.do("POST", claimPath, url.Values{"visibility": {"public"}}.Encode(),
		"application/x-www-form-urlencoded", false)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("publish: %d", resp.StatusCode)
	}

	// Public page now renders with Open Graph tags.
	resp, body = h.do("GET", "/c/"+id, "", "", false)
	if resp.StatusCode != 200 {
		t.Fatalf("public cert page: %d", resp.StatusCode)
	}
	for _, want := range []string{"Ada Lovelace", "Java Full-Stack Developer", `property="og:image"`,
		"/c/" + id + "/og.png", "Verified certificate issued by Zip Code Wilmington", "Spring", "A coding school."} {
		if !strings.Contains(body, want) {
			t.Errorf("cert page missing %q", want)
		}
	}

	// Lowercase / sloppy IDs redirect to the canonical URL.
	resp, _ = h.do("GET", "/c/"+strings.ToLower(id), "", "", false)
	if resp.StatusCode != http.StatusMovedPermanently || resp.Header.Get("Location") != "/c/"+id {
		t.Errorf("non-canonical id: %d -> %s", resp.StatusCode, resp.Header.Get("Location"))
	}

	// Image, JSON, LinkedIn redirects, learn-more UTM.
	resp, _ = h.do("GET", "/c/"+id+"/og.png", "", "", false)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/png" {
		t.Errorf("og image: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	resp, body = h.do("GET", "/c/"+id+"/credential.json", "", "", false)
	if resp.StatusCode != 200 || !strings.Contains(body, `"status": "active"`) {
		t.Errorf("credential json: %d %s", resp.StatusCode, body)
	}
	resp, _ = h.do("GET", claimPath+"/linkedin/add", "", "", false)
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "https://www.linkedin.com/profile/add?") ||
		!strings.Contains(loc, "certId="+id) {
		t.Errorf("add to profile redirect: %s", loc)
	}
	resp, _ = h.do("GET", "/c/"+id+"/learn", "", "", false)
	if loc := resp.Header.Get("Location"); !strings.Contains(loc, "utm_campaign=java") ||
		!strings.HasPrefix(loc, "https://example.org/apply?") {
		t.Errorf("learn redirect: %s", loc)
	}

	// Stats count the human view, image fetch, add and learn-more.
	_, body = h.do("GET", "/admin/api/clients/zcw/stats", "", "", true)
	var stats []store.CourseStats
	json.Unmarshal([]byte(body), &stats)
	if len(stats) != 1 || stats[0].Public != 1 || stats[0].Views < 1 || stats[0].LinkedInAdds != 1 ||
		stats[0].LearnMoreClks != 1 || stats[0].PreviewFetch != 1 {
		t.Errorf("stats: %s", body)
	}

	// Revoke: page stays up but says so.
	resp, body = h.do("POST", "/admin/api/clients/zcw/certificates/"+id+"/revoke", `{"reason":"test"}`, "application/json", true)
	if resp.StatusCode != 200 {
		t.Fatalf("revoke: %d %s", resp.StatusCode, body)
	}
	_, body = h.do("GET", "/c/"+id, "", "", false)
	if !strings.Contains(body, "Revoked.") || strings.Contains(body, "Verified certificate") {
		t.Error("revoked page should show revoked banner only")
	}

	// New claim link invalidates the old one.
	_, body = h.do("POST", "/admin/api/clients/zcw/certificates/"+id+"/claim-link", "", "", true)
	if !strings.Contains(body, "claim_url") {
		t.Fatalf("claim-link: %s", body)
	}
	if resp, _ := h.do("GET", claimPath, "", "", false); resp.StatusCode != 404 {
		t.Errorf("old claim link should 404, got %d", resp.StatusCode)
	}
}

func TestReadyz(t *testing.T) {
	h := newHarness(t)
	resp, body := h.do("GET", "/readyz", "", "", false)
	var ready struct {
		Status        string `json:"status"`
		SchemaVersion int    `json:"schema_version"`
	}
	json.Unmarshal([]byte(body), &ready)
	if resp.StatusCode != 200 || ready.Status != "ok" || ready.SchemaVersion < 2 {
		t.Fatalf("readyz: %d %s", resp.StatusCode, body)
	}
	h.st.Close() // database gone -> not ready
	if resp, _ := h.do("GET", "/readyz", "", "", false); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("readyz with closed db: %d", resp.StatusCode)
	}
}

func TestVerifyRedirectsAndUnknownIDs(t *testing.T) {
	h := newHarness(t)
	resp, _ := h.do("GET", "/verify?id=zcw-7k3m9qf2xa", "", "", false)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/c/ZCW-7K3M9QF2XA" {
		t.Errorf("verify redirect: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp, body := h.do("GET", "/verify?id=nope", "", "", false)
	if resp.StatusCode != 200 || !strings.Contains(body, "doesn&#39;t look like") {
		t.Errorf("verify bad id: %d", resp.StatusCode)
	}
	if resp, _ := h.do("GET", "/c/ZCW-7K3M9QF2XA", "", "", false); resp.StatusCode != 404 {
		t.Errorf("unknown id: %d", resp.StatusCode)
	}
	if resp, _ := h.do("GET", "/claim/not-a-token", "", "", false); resp.StatusCode != 404 {
		t.Errorf("bad claim token: %d", resp.StatusCode)
	}
}

// issueOne creates client+course and issues one certificate through the
// admin API, returning its ID and claim path.
func (h *harness) issueOne(client, prefix, name, site, course, student string) (id, claimPath string) {
	h.t.Helper()
	body := `{"slug":"` + client + `","name":"` + name + `","id_prefix":"` + prefix + `","site_url":"` + site + `"}`
	if resp, b := h.do("POST", "/admin/api/clients", body, "application/json", true); resp.StatusCode != 201 {
		h.t.Fatalf("create client %s: %d %s", client, resp.StatusCode, b)
	}
	if resp, b := h.do("POST", "/admin/api/clients/"+client+"/courses", `{"slug":"`+course+`","title":"`+course+` course"}`,
		"application/json", true); resp.StatusCode != 200 {
		h.t.Fatalf("create course: %d %s", resp.StatusCode, b)
	}
	csv := "email,full_name,course,cohort,completed_on\nsame@example.com," + student + "," + course + ",C1,2026-09-30\n"
	resp, b := h.do("POST", "/admin/api/clients/"+client+"/import", csv, "text/csv", true)
	if resp.StatusCode != 200 {
		h.t.Fatalf("import: %d %s", resp.StatusCode, b)
	}
	var links []IssuedLink
	json.Unmarshal([]byte(b), &links)
	claimPath = strings.TrimPrefix(links[0].ClaimURL, h.srv.URL)
	h.do("POST", claimPath, "visibility=public", "application/x-www-form-urlencoded", false)
	return links[0].CertificateID, claimPath
}

func TestAdminAPIIsolatesClients(t *testing.T) {
	h := newHarness(t)
	aID, _ := h.issueOne("zcw", "ZCW", "Zip Code Wilmington", "https://zipcode.example/apply", "java", "Ada")
	bID, bClaim := h.issueOne("twa", "TWA", "TwinArrows", "https://twinarrows.example/", "data", "Bea")
	if !strings.HasPrefix(aID, "ZCW-") || !strings.HasPrefix(bID, "TWA-") {
		t.Fatalf("ids: %s %s", aID, bID)
	}

	// Every per-certificate route, addressed through client A, must 404 for B's certificate.
	for _, r := range []struct{ method, path string }{
		{"GET", "/admin/api/clients/zcw/certificates/" + bID},
		{"POST", "/admin/api/clients/zcw/certificates/" + bID + "/revoke"},
		{"POST", "/admin/api/clients/zcw/certificates/" + bID + "/claim-link"},
	} {
		if resp, body := h.do(r.method, r.path, "", "", true); resp.StatusCode != 404 {
			t.Errorf("%s %s = %d %s, want 404", r.method, r.path, resp.StatusCode, body)
		}
	}
	// B's certificate and claim link are untouched.
	if resp, body := h.do("GET", bClaim, "", "", false); resp.StatusCode != 200 || !strings.Contains(body, "data course") {
		t.Fatalf("B's claim page broken: %d", resp.StatusCode)
	}

	// A can't issue against B's course slug.
	csv := "email,full_name,course,cohort,completed_on\nx@example.com,X,data,C1,2026-09-30\n"
	if resp, body := h.do("POST", "/admin/api/clients/zcw/import", csv, "text/csv", true); resp.StatusCode != 400 ||
		!strings.Contains(body, `unknown course`) {
		t.Errorf("A imported into B's course: %d %s", resp.StatusCode, body)
	}

	// Lists and stats are per client.
	_, body := h.do("GET", "/admin/api/clients/zcw/certificates", "", "", true)
	if strings.Contains(body, bID) || !strings.Contains(body, aID) {
		t.Errorf("A's list leaks B: %s", body)
	}
	_, body = h.do("GET", "/admin/api/clients/zcw/courses", "", "", true)
	if strings.Contains(body, `"data"`) {
		t.Errorf("A's courses leak B: %s", body)
	}
	if resp, _ := h.do("GET", "/admin/api/clients/nope/stats", "", "", true); resp.StatusCode != 404 {
		t.Errorf("unknown client: %d", resp.StatusCode)
	}

	// Public pages carry the issuing client's branding, not another client's.
	_, body = h.do("GET", "/c/"+bID, "", "", false)
	if !strings.Contains(body, "Verified certificate issued by TwinArrows") || strings.Contains(body, "Zip Code") {
		t.Errorf("B's public page has the wrong branding")
	}
	resp, _ := h.do("GET", "/c/"+bID+"/learn", "", "", false)
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "https://twinarrows.example/?") || !strings.Contains(loc, "utm_campaign=data") {
		t.Errorf("B's learn-more goes to %s", loc)
	}
	_, body = h.do("GET", "/c/"+aID+"/credential.json", "", "", false)
	if !strings.Contains(body, `"name": "Zip Code Wilmington"`) {
		t.Errorf("A's credential issuer: %s", body)
	}
}

func TestAdminClientManagement(t *testing.T) {
	h := newHarness(t)
	for _, c := range []struct {
		body string
		want int
	}{
		{`{"slug":"zcw","name":"Zip Code","id_prefix":"ZCW"}`, 201},
		{`{"slug":"zcw","name":"Again","id_prefix":"ZZZ"}`, 409},            // slug taken
		{`{"slug":"other","name":"Other","id_prefix":"ZCW"}`, 409},          // prefix taken
		{`{"slug":"bad slug","name":"x","id_prefix":"BS"}`, 400},            // invalid slug
		{`{"slug":"ok","name":"x","id_prefix":"OK","color":"red"}`, 400},    // unknown field
		{`{"slug":"ok2","name":"x","id_prefix":"OKK","site_url":"x"}`, 400}, // bad URL
	} {
		if resp, body := h.do("POST", "/admin/api/clients", c.body, "application/json", true); resp.StatusCode != c.want {
			t.Errorf("POST %s = %d %s, want %d", c.body, resp.StatusCode, body, c.want)
		}
	}
	resp, body := h.do("PATCH", "/admin/api/clients/zcw", `{"linkedin_org_id":"12345","slug":"hijack"}`, "application/json", true)
	if resp.StatusCode != 200 || !strings.Contains(body, `"linkedin_org_id": "12345"`) || !strings.Contains(body, `"slug": "zcw"`) {
		t.Errorf("PATCH: %d %s", resp.StatusCode, body)
	}
	if resp, _ := h.do("POST", "/admin/api/clients/zcw/status", `{"status":"suspended"}`, "application/json", true); resp.StatusCode != 200 {
		t.Fatalf("suspend: %d", resp.StatusCode)
	}
	h.do("POST", "/admin/api/clients/zcw/courses", `{"slug":"java","title":"Java"}`, "application/json", true)
	csv := "email,full_name,course,cohort,completed_on\nx@example.com,X,java,C1,2026-09-30\n"
	if resp, body := h.do("POST", "/admin/api/clients/zcw/import", csv, "text/csv", true); resp.StatusCode != 409 {
		t.Errorf("suspended client imported: %d %s", resp.StatusCode, body)
	}
	_, body = h.do("GET", "/admin/api/clients", "", "", true)
	if !strings.Contains(body, `"status": "suspended"`) {
		t.Errorf("list: %s", body)
	}
}
