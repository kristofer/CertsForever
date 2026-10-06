package web

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"certsforever/internal/store"
)

// onHost sends a request to the test server as if it arrived for host.
func (h *harness) onHost(host, method, path string, header http.Header) (*http.Response, string) {
	h.t.Helper()
	req, _ := http.NewRequest(method, h.srv.URL+path, strings.NewReader(""))
	req.Host = host
	req.Header.Set("User-Agent", "Mozilla/5.0 test")
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := client.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

type domainFixture struct {
	platform           string // the platform host (the test server's)
	zcwCert, twaCert   string
	privateCert        string
	zcwClaim, twaClaim string
}

// domainSetup: ZCW uses certs.zcw.example (verified, canonical), used to
// use old.zcw.example (verified, retired), and has pending.zcw.example
// (unverified). TWA has no custom domain.
func domainSetup(t *testing.T, h *harness) domainFixture {
	t.Helper()
	ctx := context.Background()
	h.seedPlatform()
	zcw, twa := mustScope(h, "zcw"), mustScope(h, "twa")
	f := domainFixture{platform: hostOf(h.srv.URL)}
	issue := func(sc store.Scope, email string, public bool) (string, string) {
		h.st.CreateCourse(ctx, sc, store.Course{Slug: "java", Title: "Java"})
		res, err := h.st.Issue(ctx, sc, []store.IssueRequest{{Email: email, FullName: "Student", CourseSlug: "java",
			Cohort: "C1", CompletedOn: mustDate("2026-05-01")}})
		if err != nil {
			t.Fatal(err)
		}
		if public {
			h.st.SetVisibilityByClaimToken(ctx, res[0].ClaimToken, "public")
		}
		return res[0].CertificateID, res[0].ClaimToken
	}
	f.zcwCert, f.zcwClaim = issue(zcw, "a@example.com", true)
	f.twaCert, f.twaClaim = issue(twa, "b@example.com", true)
	f.privateCert, _ = issue(zcw, "c@example.com", false)
	for _, host := range []string{"old.zcw.example", "certs.zcw.example"} {
		d, err := h.st.AddDomain(ctx, zcw, host)
		if err != nil {
			t.Fatal(err)
		}
		h.st.MarkDomainVerified(ctx, zcw, d.ID, "manual")
		h.st.SetCanonicalDomain(ctx, zcw, d.ID) // the last one wins
	}
	h.st.AddDomain(ctx, zcw, "pending.zcw.example")
	return f
}

func TestCustomDomainRouting(t *testing.T) {
	h := newHarness(t)
	f := domainSetup(t, h)
	zcwBase := "http://certs.zcw.example" // the test platform is http, so links are too

	cases := []struct {
		name, host, method, path string
		status                   int
		location                 string
	}{
		// Certificates are served on their client's canonical host...
		{"zcw on its domain", "certs.zcw.example", "GET", "/c/" + f.zcwCert, 200, ""},
		{"zcw share image on its domain", "certs.zcw.example", "GET", "/c/" + f.zcwCert + "/og.png", 200, ""},
		{"twa on the platform", f.platform, "GET", "/c/" + f.twaCert, 200, ""},
		// ...and every other known host redirects there, path and all.
		{"zcw on the platform", f.platform, "GET", "/c/" + f.zcwCert, 301, zcwBase + "/c/" + f.zcwCert},
		{"zcw share image on the platform", f.platform, "GET", "/c/" + f.zcwCert + "/og.png", 301, zcwBase + "/c/" + f.zcwCert + "/og.png"},
		{"zcw on its retired domain", "old.zcw.example", "GET", "/c/" + f.zcwCert + "?utm=x", 301, zcwBase + "/c/" + f.zcwCert + "?utm=x"},
		{"twa on zcw's domain", "certs.zcw.example", "GET", "/c/" + f.twaCert, 301, h.srv.URL + "/c/" + f.twaCert},
		{"lowercase id on zcw's domain", "certs.zcw.example", "GET", "/c/" + strings.ToLower(f.zcwCert), 301, "/c/" + f.zcwCert},
		// Private and unknown certificates are 404 everywhere: a redirect would reveal they exist.
		{"private on the platform", f.platform, "GET", "/c/" + f.privateCert, 404, ""},
		{"private on zcw's domain", "certs.zcw.example", "GET", "/c/" + f.privateCert, 404, ""},
		{"unknown on zcw's domain", "certs.zcw.example", "GET", "/c/ZCW-0000000000", 404, ""},
		// Claim links: platform links from older emails keep working; another client's domain redirects.
		{"zcw claim on the platform", f.platform, "GET", "/claim/" + f.zcwClaim, 200, ""},
		{"zcw claim on its domain", "certs.zcw.example", "GET", "/claim/" + f.zcwClaim, 200, ""},
		{"twa claim on zcw's domain", "certs.zcw.example", "GET", "/claim/" + f.twaClaim, 301, h.srv.URL + "/claim/" + f.twaClaim},
		{"twa claim form on zcw's domain", "certs.zcw.example", "POST", "/claim/" + f.twaClaim, 308, h.srv.URL + "/claim/" + f.twaClaim},
		// The console, sign-in and APIs exist only on the platform host.
		{"console on a client domain", "certs.zcw.example", "GET", "/admin/zcw?x=1", 302, h.srv.URL + "/admin/zcw?x=1"},
		{"sign-in on a client domain", "certs.zcw.example", "GET", "/login", 302, h.srv.URL + "/login"},
		{"super on a client domain", "certs.zcw.example", "GET", "/super", 302, h.srv.URL + "/super"},
		{"sign-in form post on a client domain", "certs.zcw.example", "POST", "/login", 404, ""},
		{"api post on a client domain", "certs.zcw.example", "POST", "/api/v1/import", 404, ""},
		{"webhook on a client domain", "certs.zcw.example", "POST", "/hooks/email/bounce", 404, ""},
		{"verify page on a client domain", "certs.zcw.example", "GET", "/", 200, ""},
		{"assets on a client domain", "certs.zcw.example", "GET", "/theme/1f8f81.css", 200, ""},
		// Unverified domains and internal names behave like the platform host
		// (in production Caddy won't serve TLS for them) and never redirect.
		{"zcw on localhost", "localhost", "GET", "/c/" + f.zcwCert, 200, ""},
		{"zcw on an unverified domain", "pending.zcw.example", "GET", "/c/" + f.zcwCert, 200, ""},
	}
	for _, c := range cases {
		resp, _ := h.onHost(c.host, c.method, c.path, nil)
		if resp.StatusCode != c.status || resp.Header.Get("Location") != c.location {
			t.Errorf("%s: %s %s on %s = %d %q, want %d %q", c.name, c.method, c.path, c.host,
				resp.StatusCode, resp.Header.Get("Location"), c.status, c.location)
		}
	}
}

func TestCustomDomainPages(t *testing.T) {
	h := newHarness(t)
	f := domainSetup(t, h)
	zcwBase := "http://certs.zcw.example"

	_, page := h.onHost("certs.zcw.example", "GET", "/c/"+f.zcwCert, nil)
	for _, want := range []string{`<link rel="canonical" href="` + zcwBase + `/c/` + f.zcwCert + `"`,
		`content="` + zcwBase + `/c/` + f.zcwCert + `/og.png"`} {
		if !strings.Contains(page, want) {
			t.Errorf("certificate page lacks %s", want)
		}
	}
	// LinkedIn gets the canonical URL too.
	resp, _ := h.onHost("certs.zcw.example", "GET", "/c/"+f.zcwCert+"/share/linkedin", nil)
	if loc := resp.Header.Get("Location"); !strings.Contains(loc, url.QueryEscape(zcwBase+"/c/"+f.zcwCert)) {
		t.Errorf("share link: %s", loc)
	}
	_, claim := h.onHost("certs.zcw.example", "GET", "/claim/"+f.zcwClaim, nil)
	if !strings.Contains(claim, zcwBase+"/c/"+f.zcwCert) {
		t.Error("claim page doesn't show the canonical public link")
	}
	resp, _ = h.onHost("certs.zcw.example", "GET", "/claim/"+f.zcwClaim+"/linkedin/add", nil)
	if loc := resp.Header.Get("Location"); !strings.Contains(loc, url.QueryEscape(zcwBase+"/c/"+f.zcwCert)) {
		t.Errorf("add to profile: %s", loc)
	}

	// The verify page and "not found" are branded for the domain's client.
	for _, path := range []string{"/", "/verify", "/c/ZCW-0000000000"} {
		_, page := h.onHost("certs.zcw.example", "GET", path, nil)
		if !strings.Contains(page, "<title>") || !strings.Contains(page, "Zip Code Wilmington") || strings.Contains(page, "TwinArrows") {
			t.Errorf("%s on zcw's domain isn't branded for ZCW", path)
		}
	}
	// Verifying another client's ID on zcw's domain lands on that client's host.
	resp, _ = h.onHost("certs.zcw.example", "GET", "/verify?id="+f.twaCert, nil)
	if resp.Header.Get("Location") != "/c/"+f.twaCert {
		t.Fatalf("verify: %s", resp.Header.Get("Location"))
	}
	resp, _ = h.onHost("certs.zcw.example", "GET", "/c/"+f.twaCert, nil)
	if resp.StatusCode != 301 || !strings.HasPrefix(resp.Header.Get("Location"), h.srv.URL) {
		t.Fatalf("verify follow-on: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}

	// X-Forwarded-Host is ignored: a client can set it to anything.
	resp, _ = h.onHost(f.platform, "GET", "/login", http.Header{"X-Forwarded-Host": {"certs.zcw.example"}})
	if resp.StatusCode != 200 {
		t.Fatalf("X-Forwarded-Host moved the platform's sign-in page: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	resp, _ = h.onHost(f.platform, "GET", "/c/"+f.zcwCert, http.Header{"X-Forwarded-Host": {"evil.example"}})
	if resp.StatusCode != 301 || resp.Header.Get("Location") != zcwBase+"/c/"+f.zcwCert {
		t.Fatalf("X-Forwarded-Host changed routing: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	// A forged Host never ends up in a redirect.
	resp, _ = h.onHost("evil.example", "GET", "/c/"+f.zcwCert, http.Header{"X-Forwarded-Host": {"certs.zcw.example"}})
	if resp.StatusCode != 200 || resp.Header.Get("Location") != "" {
		t.Fatalf("unknown host: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
}

func TestCustomDomainLinksInEmailsAndAPI(t *testing.T) {
	h := newHarness(t)
	f := domainSetup(t, h)
	_ = f
	csv := "email,full_name,course,cohort,completed_on\nnew@example.com,New Student,java,C2,2026-05-01\n"
	resp, body := h.do("POST", "/admin/api/clients/zcw/import?notify=true", csv, "text/csv", true)
	if resp.StatusCode != 200 || !strings.Contains(body, `"certificate_url": "http://certs.zcw.example/c/ZCW-`) ||
		!strings.Contains(body, `"claim_url": "http://certs.zcw.example/claim/`) {
		t.Fatalf("import links: %d %s", resp.StatusCode, body)
	}
	h.waitMail(1)
	if m := h.mail.Last(); !strings.Contains(m.Text, "http://certs.zcw.example/claim/") {
		t.Fatalf("emailed claim link isn't on the client's domain:\n%s", m.Text)
	}
	// TWA has no domain: platform links.
	resp, body = h.do("POST", "/admin/api/clients/twa/import", strings.Replace(csv, "C2", "C3", 1), "text/csv", true)
	if resp.StatusCode != 200 || !strings.Contains(body, `"claim_url": "`+h.srv.URL+`/claim/`) {
		t.Fatalf("twa links: %s", body)
	}
	// Sign-in links stay on the platform host.
	b := h.browser()
	b.send("POST", "/login", url.Values{"email": {"admin@zcw.example"}})
	h.waitMail(2)
	if m := h.mail.Last(); !strings.Contains(m.Text, h.srv.URL+"/login/") {
		t.Fatalf("sign-in link:\n%s", m.Text)
	}
}

func TestTLSAsk(t *testing.T) {
	h := newHarness(t)
	f := domainSetup(t, h)
	cases := map[string]int{
		f.platform:            200,
		"certs.zcw.example":   200,
		"CERTS.zcw.example.":  200,
		"old.zcw.example":     200, // retired but verified: its links must keep working over TLS
		"pending.zcw.example": 404,
		"evil.example":        404,
		"":                    404,
	}
	for d, want := range cases {
		if resp, _ := h.onHost("certsforever:8080", "GET", "/internal/tls-ask?domain="+url.QueryEscape(d), nil); resp.StatusCode != want {
			t.Errorf("ask %q = %d, want %d", d, resp.StatusCode, want)
		}
	}
	// Relayed through the proxy from outside: refused, so it can't be used to
	// probe which domains are registered.
	resp, _ := h.onHost(f.platform, "GET", "/internal/tls-ask?domain=certs.zcw.example", http.Header{"X-Forwarded-For": {"203.0.113.5"}})
	if resp.StatusCode != 404 {
		t.Fatalf("relayed ask: %d", resp.StatusCode)
	}
	// On a client's domain it isn't served at all.
	if resp, _ := h.onHost("certs.zcw.example", "GET", "/internal/tls-ask?domain=certs.zcw.example", nil); resp.StatusCode != 302 {
		t.Fatalf("ask on client domain: %d", resp.StatusCode)
	}
}
