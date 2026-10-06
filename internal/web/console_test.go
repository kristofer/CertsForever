package web

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"certsforever/internal/store"
)

// postMultipart sends a multipart form (for uploads) with the CSRF token.
func (b *browser) postMultipart(path string, fields map[string]string, files map[string][]byte) (*http.Response, string) {
	b.h.t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if _, ok := fields["csrf"]; !ok {
		mw.WriteField("csrf", b.csrf)
	}
	for k, v := range fields {
		mw.WriteField(k, v)
	}
	for k, data := range files {
		fw, _ := mw.CreateFormFile(k, k+".png")
		fw.Write(data)
	}
	mw.Close()
	req, _ := http.NewRequest("POST", b.h.srv.URL+path, &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Origin", b.origin)
	req.Header.Set("User-Agent", "Mozilla/5.0 test")
	resp, err := b.c.Do(req)
	if err != nil {
		b.h.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if m := csrfRE.FindStringSubmatch(string(raw)); m != nil {
		b.csrf = m[1]
	}
	return resp, string(raw)
}

func testPNG(t *testing.T, w, h int, c color.Color) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	var b bytes.Buffer
	png.Encode(&b, img)
	return b.Bytes()
}

func mustStatus(t *testing.T, what string, resp *http.Response, page string, want int) {
	t.Helper()
	if resp.StatusCode != want {
		if len(page) > 3000 {
			page = page[:3000]
		}
		t.Fatalf("%s: status %d, want %d\n%s", what, resp.StatusCode, want, page)
	}
}

var certIDRE = regexp.MustCompile(`ZCW-[0-9A-Z]{10}`)

// TestClientConsoleWalkthrough is a client admin's first day: a course with
// its own design, a cohort issued from CSV, then fixing a name, revoking,
// reminding and exporting.
func TestClientConsoleWalkthrough(t *testing.T) {
	h := newHarness(t)
	h.seedPlatform()
	ctx := context.Background()
	b := h.browser()
	b.signIn("admin@zcw.example")

	resp, page := b.get("/admin/zcw")
	mustStatus(t, "overview", resp, page, 200)
	if !strings.Contains(page, "Get started") {
		t.Fatal("empty client should see the getting-started steps")
	}

	// Settings.
	resp, page = b.post("/admin/zcw/settings", url.Values{"name": {"Zip Code Wilmington"}, "site_url": {"https://zipcodewilmington.com"},
		"blurb": {"A coding bootcamp."}, "reply_to": {"hello@zcw.example"}, "send_reminders": {"on"}})
	mustStatus(t, "settings", resp, page, http.StatusSeeOther)
	if c := mustScope(h, "zcw").Client(); c.SiteURL != "https://zipcodewilmington.com" || c.ReplyTo != "hello@zcw.example" || !c.SendReminders {
		t.Fatalf("settings not saved: %+v", c)
	}
	resp, page = b.post("/admin/zcw/settings", url.Values{"name": {"Zip Code Wilmington"}, "site_url": {"javascript:alert(1)"}})
	mustStatus(t, "bad site url", resp, page, http.StatusBadRequest)

	// A design with a logo and a signature.
	b.get("/admin/zcw/designs/new")
	resp, page = b.postMultipart("/admin/zcw/designs", map[string]string{"name": "Bootcamp", "heading": "Certificate of Achievement",
		"body_text": "has completed the program", "accent_color": "#aa3300", "sig1_name": "Kris Younger", "sig1_title": "Lead Instructor"},
		map[string][]byte{"logo": testPNG(t, 1200, 300, color.RGBA{0, 0, 255, 255}), "sig1_file": testPNG(t, 300, 100, color.White)})
	mustStatus(t, "save design", resp, page, http.StatusSeeOther)
	designPath := strings.SplitN(resp.Header.Get("Location"), "?", 2)[0]
	resp, page = b.get(designPath)
	mustStatus(t, "design page", resp, page, 200)
	if !strings.Contains(page, `src="/assets/`) || !strings.Contains(page, "Kris Younger") {
		t.Fatal("design page lacks logo or signatory")
	}
	// A non-image upload is refused with a message, and nothing changes.
	resp, page = b.postMultipart(designPath, map[string]string{"name": "Bootcamp", "heading": "H", "accent_color": "#aa3300"},
		map[string][]byte{"logo": []byte("<svg onload=alert(1)>")})
	mustStatus(t, "svg logo", resp, page, http.StatusBadRequest)
	if !strings.Contains(page, "PNG or JPEG") {
		t.Fatalf("no upload error shown")
	}
	resp, page = b.get(designPath + "/preview")
	mustStatus(t, "preview", resp, page, 200)
	if !strings.Contains(page, "Certificate of Achievement") || !strings.Contains(page, "Preview.") || !strings.Contains(page, "/theme/aa3300.css") ||
		strings.Contains(page, "og:image") {
		t.Fatal("preview page")
	}
	resp, _ = b.get(designPath + "/preview.png")
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/png" {
		t.Fatalf("preview png: %d", resp.StatusCode)
	}
	designID := designPath[strings.LastIndex(designPath, "/")+1:]

	// A course using it.
	resp, page = b.post("/admin/zcw/courses", url.Values{"slug": {"java"}, "title": {"Java Developer"}, "skills": {"Java, SQL"},
		"design_id": {designID}})
	mustStatus(t, "add course", resp, page, http.StatusSeeOther)
	resp, page = b.post("/admin/zcw/courses", url.Values{"slug": {"java"}, "title": {"Dup"}})
	mustStatus(t, "duplicate course", resp, page, http.StatusBadRequest)
	resp, page = b.post("/admin/zcw/courses", url.Values{"slug": {"typo"}, "title": {"Typo"}})
	mustStatus(t, "add typo course", resp, page, http.StatusSeeOther)
	resp, page = b.post("/admin/zcw/courses/typo/delete", nil)
	mustStatus(t, "delete course", resp, page, http.StatusSeeOther)

	// Issue: preview first (nothing written), then confirm.
	csv := "email,full_name,course,cohort,completed_on\n" +
		"ada@example.com,Ada Lovelace,java,Spring 2026,2026-05-30\n" +
		"grace@example.com,Grace Hoper,java,Spring 2026,2026-05-30\n" +
		"ada@example.com,Ada Lovelace,java,Spring 2026,2026-05-30\n" +
		"=cmd@example.com,\"=HYPERLINK(\"\"x\"\")\",java,Spring 2026,2026-05-30\n"
	b.get("/admin/zcw/issue")
	resp, page = b.postMultipart("/admin/zcw/issue/preview", map[string]string{"notify": "on"}, map[string][]byte{"file": []byte(csv)})
	mustStatus(t, "preview", resp, page, 200)
	if !strings.Contains(page, "<strong>3</strong> new") || !strings.Contains(page, "repeated in the file") || !strings.Contains(page, "Issue 3 certificates") {
		t.Fatalf("preview counts wrong")
	}
	if _, n, _ := h.st.SearchCertificates(ctx, mustScope(h, "zcw"), store.CertFilter{}); n != 0 {
		t.Fatal("preview issued certificates")
	}
	before := h.mail.Count()
	resp, page = b.post("/admin/zcw/issue", url.Values{"csv": {csv}, "notify": {"on"}})
	mustStatus(t, "issue", resp, page, http.StatusSeeOther)
	h.waitMail(before + 3)
	certs, _, _ := h.st.SearchCertificates(ctx, mustScope(h, "zcw"), store.CertFilter{Query: "grace"})
	if len(certs) != 1 {
		t.Fatal("grace not issued")
	}
	grace := certs[0]
	if grace.Design.Heading != "Certificate of Achievement" || grace.Design.LogoAssetID == "" || len(grace.Design.Signatories) != 1 {
		t.Fatalf("design not snapshotted: %+v", grace.Design)
	}
	// Re-issuing the same file is a no-op.
	resp, page = b.post("/admin/zcw/issue", url.Values{"csv": {csv}})
	mustStatus(t, "reissue", resp, page, http.StatusBadRequest)
	if !strings.Contains(page, "already has a certificate") {
		t.Fatal("reissue message")
	}

	// The certificate list and search.
	resp, page = b.get("/admin/zcw/certificates?q=hoper")
	mustStatus(t, "search", resp, page, 200)
	if !strings.Contains(page, "Grace Hoper") || strings.Contains(page, "Ada Lovelace") {
		t.Fatal("search results")
	}

	// Fix the typo in Grace's name, in place.
	certPath := "/admin/zcw/certificates/" + grace.ID
	b.get(certPath)
	resp, page = b.post(certPath+"/name", url.Values{"name": {"Grace Hopper"}})
	mustStatus(t, "correct name", resp, page, http.StatusSeeOther)
	resp, page = b.get(certPath)
	mustStatus(t, "cert detail", resp, page, 200)
	if !strings.Contains(page, "Grace Hopper") || !strings.Contains(page, "certificate.name_correct") || !strings.Contains(page, "from: Grace Hoper") {
		t.Fatalf("name correction not shown in history:\n%s", page[strings.Index(page, "<h2>History"):])
	}

	// The public page uses the design (once the student publishes it).
	h.st.SetVisibilityByClaimToken(ctx, claimTokenFromMail(t, h, "grace@example.com"), "public")
	resp, page = h.do("GET", "/c/"+grace.ID, "", "", false)
	mustStatus(t, "public page", resp, page, 200)
	for _, want := range []string{"Certificate of Achievement", "has completed the program", "Grace Hopper", "Kris Younger",
		"Lead Instructor", `href="/theme/aa3300.css"`, `src="/assets/` + grace.Design.LogoAssetID + `.png"`} {
		if !strings.Contains(page, want) {
			t.Errorf("public page lacks %q", want)
		}
	}
	resp, _ = h.do("GET", "/c/"+grace.ID+"/og.png", "", "", false)
	if resp.StatusCode != 200 {
		t.Fatalf("og image %d", resp.StatusCode)
	}
	// Editing the design later doesn't change the issued certificate.
	resp, page = b.postMultipart(designPath, map[string]string{"name": "Bootcamp", "heading": "Changed Heading", "accent_color": "#000000", "sig1_name": "Kris Younger"}, nil)
	mustStatus(t, "edit design", resp, page, http.StatusSeeOther)
	if _, page = h.do("GET", "/c/"+grace.ID, "", "", false); !strings.Contains(page, "Certificate of Achievement") {
		t.Fatal("issued certificate changed with its design")
	}

	// Cohort roster and reminders: Ada hasn't opened hers; Grace has (published).
	resp, page = b.get("/admin/zcw/cohorts")
	mustStatus(t, "cohorts", resp, page, 200)
	resp, page = b.get("/admin/zcw/cohorts/roster?course=java&cohort=Spring+2026")
	mustStatus(t, "roster", resp, page, 200)
	if !strings.Contains(page, "Remind") {
		t.Fatal("no remind button")
	}
	// Everyone was emailed minutes ago, so a reminder now is skipped.
	before = h.mail.Count()
	resp, page = b.post("/admin/zcw/cohorts/remind?course=java&cohort=Spring+2026", nil)
	mustStatus(t, "remind", resp, page, http.StatusSeeOther)

	// Resend a fresh link; then revoke.
	b.get(certPath)
	resp, page = b.post(certPath+"/resend", nil)
	mustStatus(t, "resend", resp, page, http.StatusSeeOther)
	h.waitMail(before + 1)
	resp, page = b.post(certPath+"/revoke", url.Values{"reason": {""}})
	mustStatus(t, "revoke without reason", resp, page, http.StatusBadRequest)
	resp, page = b.post(certPath+"/revoke", url.Values{"reason": {"issued in error"}})
	mustStatus(t, "revoke", resp, page, http.StatusSeeOther)
	if _, page = h.do("GET", "/c/"+grace.ID, "", "", false); !strings.Contains(page, "Revoked.") {
		t.Fatal("public page not revoked")
	}

	// Export neutralizes spreadsheet formulas.
	resp, page = b.get("/admin/zcw/certificates/export?format=csv")
	mustStatus(t, "export", resp, page, 200)
	if !strings.Contains(page, `'=HYPERLINK`) || strings.Contains(page, `,=HYPERLINK`) {
		t.Fatalf("formula not neutralized:\n%s", page)
	}
	if !strings.Contains(resp.Header.Get("Content-Disposition"), "zcw-certificates-") {
		t.Fatal("export filename")
	}
	_, page = b.get("/admin/zcw/certificates/export?format=json&status=revoked")
	var out []store.Certificate
	if json.Unmarshal([]byte(page), &out) != nil || len(out) != 1 || out[0].ID != grace.ID {
		t.Fatalf("json export: %s", page)
	}

	// The design in use can't be deleted.
	resp, page = b.post(designPath+"/delete", nil)
	mustStatus(t, "delete used design", resp, page, http.StatusConflict)

	// Every page renders.
	for _, p := range []string{"", "/certificates", "/issue", "/cohorts", "/courses", "/courses?edit=java", "/designs",
		"/team", "/settings", "/tokens"} {
		if resp, page := b.get("/admin/zcw" + p); resp.StatusCode != 200 {
			mustStatus(t, p, resp, page, 200)
		}
	}
}

// claimTokenFromMail finds the latest claim token emailed to addr.
func claimTokenFromMail(t *testing.T, h *harness, addr string) string {
	t.Helper()
	re := regexp.MustCompile(`/claim/([A-Za-z0-9_-]+)`)
	all := h.mail.All()
	for i := len(all) - 1; i >= 0; i-- {
		m := all[i]
		if strings.EqualFold(m.To, addr) {
			if sm := re.FindStringSubmatch(m.Text); sm != nil {
				return sm[1]
			}
		}
	}
	t.Fatalf("no claim link mailed to %s", addr)
	return ""
}

// TestConsoleIsolation: an admin of one client can't reach another's
// console pages, or the other's records through their own console.
func TestConsoleIsolation(t *testing.T) {
	h := newHarness(t)
	h.seedPlatform()
	ctx := context.Background()
	twa := mustScope(h, "twa")
	h.st.CreateCourse(ctx, twa, store.Course{Slug: "java", Title: "TWA Java"})
	res, _ := h.st.Issue(ctx, twa, []store.IssueRequest{{Email: "x@example.com", FullName: "TWA Student",
		CourseSlug: "java", Cohort: "C1", CompletedOn: mustDate("2026-05-01")}})
	twaCert := res[0].CertificateID
	twaDesign, _ := h.st.SaveDesign(ctx, twa, store.Design{Name: "TWA", Heading: "H", AccentColor: "#123456"})
	tok, _, _ := h.st.CreateAPIToken(ctx, twa, "twa", nil)
	did := itoa(twaDesign)

	b := h.browser()
	b.signIn("admin@zcw.example")
	b.get("/admin/zcw")

	gets := []string{"/admin/twa", "/admin/twa/certificates", "/admin/twa/certificates/export", "/admin/twa/issue",
		"/admin/twa/cohorts", "/admin/twa/cohorts/roster?course=java&cohort=C1", "/admin/twa/courses", "/admin/twa/designs",
		"/admin/twa/designs/" + did, "/admin/twa/designs/" + did + "/preview", "/admin/twa/designs/" + did + "/preview.png",
		"/admin/twa/team", "/admin/twa/settings", "/admin/twa/tokens",
		// TWA's records through ZCW's console:
		"/admin/zcw/certificates/" + twaCert, "/admin/zcw/designs/" + did, "/admin/zcw/designs/" + did + "/preview",
		"/admin/zcw/designs/" + did + "/preview.png", "/admin/zcw/cohorts/roster?course=java&cohort=C1"}
	for _, p := range gets {
		if resp, _ := b.get(p); resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusSeeOther {
			t.Errorf("GET %s: %d", p, resp.StatusCode)
		}
	}
	posts := []string{"/admin/twa/settings", "/admin/twa/courses", "/admin/twa/issue", "/admin/twa/issue/preview",
		"/admin/twa/tokens", "/admin/twa/designs", "/admin/twa/cohorts/remind?course=java&cohort=C1",
		"/admin/zcw/certificates/" + twaCert + "/revoke", "/admin/zcw/certificates/" + twaCert + "/name",
		"/admin/zcw/certificates/" + twaCert + "/resend", "/admin/zcw/designs/" + did, "/admin/zcw/designs/" + did + "/delete",
		"/admin/zcw/courses/java/delete", "/admin/zcw/tokens/1/revoke"}
	for _, p := range posts {
		resp, _ := b.post(p, url.Values{"reason": {"x"}, "name": {"Pwned"}, "title": {"x"}, "slug": {"java"}, "heading": {"x"}, "accent_color": {"#000000"}})
		if resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusSeeOther {
			t.Errorf("POST %s: %d", p, resp.StatusCode)
		}
	}
	c, _ := h.st.GetClientCertificate(ctx, twa, twaCert)
	if c.Revoked() || c.RecipientName != "TWA Student" {
		t.Fatalf("TWA certificate changed: %+v", c)
	}
	if d, _ := h.st.GetDesign(ctx, twa, twaDesign); d.Name != "TWA" {
		t.Fatal("TWA design changed")
	}
	if list, _ := h.st.ListAPITokens(ctx, twa); list[0].RevokedAt != nil {
		t.Fatal("TWA token revoked")
	}
	if co, _ := h.st.ListCourses(ctx, twa); len(co) != 1 || co[0].Title != "TWA Java" {
		t.Fatalf("TWA courses changed: %+v", co)
	}

	// A ZCW API token can't see TWA's certificates either.
	zcwTok, _, _ := h.st.CreateAPIToken(ctx, mustScope(h, "zcw"), "zcw", nil)
	if resp, _ := apiDo(h, zcwTok, "GET", "/api/v1/certificates/"+twaCert, ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-client API read: %d", resp.StatusCode)
	}
	if resp, _ := apiDo(h, zcwTok, "POST", "/api/v1/certificates/"+twaCert+"/revoke", `{"reason":"x"}`); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-client API revoke: %d", resp.StatusCode)
	}
	if _, body := apiDo(h, tok, "GET", "/api/v1/certificates", ""); !strings.Contains(body, twaCert) {
		t.Fatal("TWA token can't see its own certificates")
	}
}

// TestConsoleCSRF: the new forms are protected like the old ones.
func TestConsoleCSRF(t *testing.T) {
	h := newHarness(t)
	h.seedPlatform()
	b := h.browser()
	b.signIn("admin@zcw.example")
	for _, p := range []string{"/admin/zcw/settings", "/admin/zcw/courses", "/admin/zcw/tokens", "/admin/zcw/issue"} {
		if resp, _ := b.send("POST", p, url.Values{"name": {"x"}, "slug": {"x"}, "title": {"x"}}); resp.StatusCode != http.StatusForbidden {
			t.Errorf("POST %s without CSRF token: %d", p, resp.StatusCode)
		}
	}
	if resp, _ := b.postMultipart("/admin/zcw/designs", map[string]string{"csrf": "wrong", "name": "x"}, nil); resp.StatusCode != http.StatusForbidden {
		t.Errorf("design upload without CSRF token: %d", resp.StatusCode)
	}
}

func mustDate(s string) time.Time {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return t
}

func apiDo(h *harness, token, method, path, body string) (*http.Response, string) {
	h.t.Helper()
	req, _ := http.NewRequest(method, h.srv.URL+path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if strings.HasPrefix(body, "{") {
		req.Header.Set("Content-Type", "application/json")
	} else if body != "" {
		req.Header.Set("Content-Type", "text/csv")
	}
	resp, err := client.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestClientAPITokens(t *testing.T) {
	h := newHarness(t)
	h.seedPlatform()
	ctx := context.Background()
	b := h.browser()
	b.signIn("admin@zcw.example")
	b.get("/admin/zcw/tokens")
	resp, page := b.post("/admin/zcw/tokens", url.Values{"name": {"Canvas sync"}})
	mustStatus(t, "create token", resp, page, 200)
	m := regexp.MustCompile(`<code>(cfk_[A-Za-z0-9_-]+)</code>`).FindStringSubmatch(page)
	if m == nil {
		t.Fatal("token not shown")
	}
	tok := m[1]
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatal("token page cacheable")
	}
	// Shown once.
	if _, page = b.get("/admin/zcw/tokens"); strings.Contains(page, tok) {
		t.Fatal("token shown again")
	}

	if resp, _ := apiDo(h, "", "GET", "/api/v1/courses", ""); resp.StatusCode != 401 {
		t.Fatalf("no token: %d", resp.StatusCode)
	}
	if resp, _ := apiDo(h, adminToken, "GET", "/api/v1/courses", ""); resp.StatusCode != 401 {
		t.Fatalf("platform token on client API: %d", resp.StatusCode)
	}
	if resp, body := apiDo(h, tok, "POST", "/api/v1/courses", `{"slug":"java","title":"Java"}`); resp.StatusCode != 201 && resp.StatusCode != 200 {
		t.Fatalf("create course: %d %s", resp.StatusCode, body)
	}
	resp, body := apiDo(h, tok, "POST", "/api/v1/import?notify=true",
		"email,full_name,course,cohort,completed_on\nlin@example.com,Lin,java,C1,2026-05-01\n")
	if resp.StatusCode != 200 || !strings.Contains(body, `"emailed": true`) {
		t.Fatalf("import: %d %s", resp.StatusCode, body)
	}
	id := certIDRE.FindString(body)
	if resp, _ := apiDo(h, tok, "GET", "/api/v1/certificates?q=lin", ""); resp.StatusCode != 200 {
		t.Fatal("list")
	}
	if resp, _ := apiDo(h, tok, "POST", "/api/v1/certificates/"+id+"/revoke", `{"reason":"test"}`); resp.StatusCode != 200 {
		t.Fatal("revoke")
	}
	audit, _ := h.st.ListAudit(ctx, mustScope(h, "zcw"), 10)
	found := false
	for _, e := range audit {
		if e.Action == "certificate.revoke" && e.Actor == "api-token:Canvas sync" {
			found = true
		}
	}
	if !found {
		t.Fatalf("API action not audited as the token: %+v", audit)
	}

	// Revoked tokens stop working.
	list, _ := h.st.ListAPITokens(ctx, mustScope(h, "zcw"))
	resp, page = b.post("/admin/zcw/tokens/"+itoa(list[0].ID)+"/revoke", nil)
	mustStatus(t, "revoke token", resp, page, http.StatusSeeOther)
	if resp, _ := apiDo(h, tok, "GET", "/api/v1/courses", ""); resp.StatusCode != 401 {
		t.Fatalf("revoked token: %d", resp.StatusCode)
	}
}

func TestAssetsAndTheme(t *testing.T) {
	h := newHarness(t)
	h.seedPlatform()
	id, err := h.st.SaveAsset(context.Background(), mustScope(h, "zcw"), "logo", testPNG(t, 4, 4, color.Black), 4, 4)
	if err != nil {
		t.Fatal(err)
	}
	resp, body := h.do("GET", "/assets/"+id+".png", "", "", false)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/png" ||
		!strings.Contains(resp.Header.Get("Cache-Control"), "immutable") || !strings.HasPrefix(body, "\x89PNG") {
		t.Fatalf("asset: %d %v", resp.StatusCode, resp.Header)
	}
	req, _ := http.NewRequest("GET", h.srv.URL+"/assets/"+id+".png", nil)
	req.Header.Set("If-None-Match", resp.Header.Get("ETag"))
	if r2, _ := client.Do(req); r2.StatusCode != http.StatusNotModified {
		t.Fatalf("etag: %d", r2.StatusCode)
	}
	for _, p := range []string{"/assets/nope.png", "/assets/" + strings.Repeat("0", 24) + ".png", "/assets/..%2f..%2fetc.png"} {
		if resp, _ := h.do("GET", p, "", "", false); resp.StatusCode != 404 {
			t.Errorf("%s: %d", p, resp.StatusCode)
		}
	}
	resp, body = h.do("GET", "/theme/aa3300.css", "", "", false)
	if resp.StatusCode != 200 || !strings.Contains(body, "--cert-accent: #aa3300") {
		t.Fatalf("theme: %d %s", resp.StatusCode, body)
	}
	for _, p := range []string{"/theme/red.css", "/theme/aa3300;}body{x.css", "/theme/AA3300.css"} {
		if resp, _ := h.do("GET", p, "", "", false); resp.StatusCode != 404 {
			t.Errorf("%s: %d", p, resp.StatusCode)
		}
	}
}
