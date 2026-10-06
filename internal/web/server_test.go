package web

import (
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

	"certsforever/internal/config"
	"certsforever/internal/store"
)

const adminToken = "test-admin-token"

type harness struct {
	t   *testing.T
	srv *httptest.Server
	st  *store.Store
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := config.FromEnv()
	cfg.AdminToken = adminToken
	cfg.SiteURL = "https://example.org/apply"
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := New(cfg, st, log)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	s.cfg.BaseURL = ts.URL
	return &harness{t: t, srv: ts, st: st}
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
	if resp, _ := h.do("GET", "/admin/api/stats", "", "", false); resp.StatusCode != 401 {
		t.Fatalf("stats without token: %d", resp.StatusCode)
	}

	// Create a course and import a cohort.
	resp, body := h.do("POST", "/admin/api/courses",
		`{"slug":"java","title":"Java Full-Stack Developer","skills":["Java","Spring","SQL"]}`, "application/json", true)
	if resp.StatusCode != 200 {
		t.Fatalf("create course: %d %s", resp.StatusCode, body)
	}
	csv := "email,full_name,course,cohort,completed_on\nada@example.com,Ada Lovelace,java,Java 13,2026-09-30\n"
	resp, body = h.do("POST", "/admin/api/import", csv, "text/csv", true)
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
	_, body = h.do("POST", "/admin/api/import", csv, "text/csv", true)
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
		"/c/" + id + "/og.png", "Verified certificate", "Spring"} {
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
	_, body = h.do("GET", "/admin/api/stats", "", "", true)
	var stats []store.CourseStats
	json.Unmarshal([]byte(body), &stats)
	if len(stats) != 1 || stats[0].Public != 1 || stats[0].Views < 1 || stats[0].LinkedInAdds != 1 ||
		stats[0].LearnMoreClks != 1 || stats[0].PreviewFetch != 1 {
		t.Errorf("stats: %s", body)
	}

	// Revoke: page stays up but says so.
	resp, body = h.do("POST", "/admin/api/certificates/"+id+"/revoke", `{"reason":"test"}`, "application/json", true)
	if resp.StatusCode != 200 {
		t.Fatalf("revoke: %d %s", resp.StatusCode, body)
	}
	_, body = h.do("GET", "/c/"+id, "", "", false)
	if !strings.Contains(body, "Revoked.") || strings.Contains(body, "Verified certificate") {
		t.Error("revoked page should show revoked banner only")
	}

	// New claim link invalidates the old one.
	_, body = h.do("POST", "/admin/api/certificates/"+id+"/claim-link", "", "", true)
	if !strings.Contains(body, "claim_url") {
		t.Fatalf("claim-link: %s", body)
	}
	if resp, _ := h.do("GET", claimPath, "", "", false); resp.StatusCode != 404 {
		t.Errorf("old claim link should 404, got %d", resp.StatusCode)
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
