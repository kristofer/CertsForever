package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"certsforever/internal/config"
	"certsforever/internal/mail"
	"certsforever/internal/store"
)

const hookToken = "0123456789abcdef0123456789abcdef-hook"

func (h *harness) createClient(slug, name, prefix, replyTo string) {
	h.t.Helper()
	body := `{"slug":"` + slug + `","name":"` + name + `","id_prefix":"` + prefix + `","reply_to":"` + replyTo + `"}`
	if resp, b := h.do("POST", "/admin/api/clients", body, "application/json", true); resp.StatusCode != 201 {
		h.t.Fatalf("create client: %d %s", resp.StatusCode, b)
	}
	h.do("POST", "/admin/api/clients/"+slug+"/courses", `{"slug":"java","title":"Java Developer"}`, "application/json", true)
}

func claimPathIn(text string) string {
	i := strings.Index(text, "/claim/")
	if i < 0 {
		return ""
	}
	return "/claim/" + strings.Fields(text[i+len("/claim/"):])[0]
}

func TestImportWithNotifyEmailsStudents(t *testing.T) {
	h := newHarness(t)
	h.createClient("zcw", "Zip Code Wilmington", "ZCW", "info@zipcode.example")

	csv := "email,full_name,course,cohort,completed_on\nada@example.com,Ada Lovelace,java,J1,2026-09-30\n"
	// Without notify=true, nothing is emailed.
	h.do("POST", "/admin/api/clients/zcw/import", csv, "text/csv", true)
	time.Sleep(100 * time.Millisecond)
	if h.mail.Count() != 0 {
		t.Fatal("emailed without notify=true")
	}

	csv = "email,full_name,course,cohort,completed_on\nalan@example.com,Alan Turing,java,J1,2026-09-30\n"
	resp, body := h.do("POST", "/admin/api/clients/zcw/import?notify=true", csv, "text/csv", true)
	if resp.StatusCode != 200 || !strings.Contains(body, `"emailed": true`) {
		t.Fatalf("import: %d %s", resp.StatusCode, body)
	}
	h.waitMail(1)
	m := h.mail.Last()
	if m.To != "alan@example.com" || m.FromName != "Zip Code Wilmington" || m.ReplyTo != "info@zipcode.example" ||
		!strings.Contains(m.Subject, "Java Developer") || m.HTML == "" {
		t.Fatalf("certificate email: %+v", m)
	}
	path := claimPathIn(m.Text)
	if resp, page := h.do("GET", path, "", "", false); resp.StatusCode != 200 || !strings.Contains(page, "Alan Turing") {
		t.Fatalf("emailed claim link: %d", resp.StatusCode)
	}
	// Delivered rows keep no body.
	list, _ := h.st.ListEmails(context.Background(), "", 5)
	if len(list) != 1 || list[0].Status != "sent" || list[0].HasBody || list[0].ClientSlug != "zcw" {
		t.Fatalf("outbox: %+v", list)
	}

	// Re-sending a claim link by email replaces the old one.
	var links []IssuedLink
	json.Unmarshal([]byte(body), &links)
	resp, body = h.do("POST", "/admin/api/clients/zcw/certificates/"+links[0].CertificateID+"/claim-link?notify=true", "", "", true)
	if resp.StatusCode != 200 {
		t.Fatalf("resend: %d %s", resp.StatusCode, body)
	}
	h.waitMail(2)
	if newPath := claimPathIn(h.mail.Last().Text); newPath == path || newPath == "" {
		t.Fatal("resend didn't carry a new link")
	}
	if resp, _ := h.do("GET", path, "", "", false); resp.StatusCode != 404 {
		t.Fatal("old claim link still works after resend")
	}
}

func TestEmailOffWithoutSMTP(t *testing.T) {
	h := newHarnessWith(t, &harnessOpts{noEmail: true})
	h.createClient("zcw", "Zip Code Wilmington", "ZCW", "")
	csv := "email,full_name,course,cohort,completed_on\nada@example.com,Ada,java,J1,2026-09-30\n"
	if resp, body := h.do("POST", "/admin/api/clients/zcw/import?notify=true", csv, "text/csv", true); resp.StatusCode != 409 ||
		!strings.Contains(body, "isn't configured") {
		t.Fatalf("notify without email: %d %s", resp.StatusCode, body)
	}
	if certs, _ := h.st.ListCertificates(context.Background(), mustScope(h, "zcw"), "", ""); len(certs) != 0 {
		t.Fatal("certificates issued even though the request was refused")
	}
	_, page := h.do("GET", "/login", "", "", false)
	if !strings.Contains(page, "isn't set up") {
		t.Error("login page should say email sign-in isn't set up")
	}
}

func mustScope(h *harness, slug string) store.Scope {
	sc, err := h.st.Scope(context.Background(), slug)
	if err != nil {
		h.t.Fatal(err)
	}
	return sc
}

func TestBounceWebhook(t *testing.T) {
	// Off unless a token is configured.
	off := newHarness(t)
	if resp, _ := off.do("POST", "/hooks/email/bounce", `{"email":"a@example.com","type":"bounce"}`, "application/json", false); resp.StatusCode != 404 {
		t.Fatalf("webhook without token configured: %d", resp.StatusCode)
	}

	h := newHarnessWith(t, &harnessOpts{cfg: func(c *config.Config) { c.BounceWebhookToken = hookToken }})
	post := func(path, body string, hdr map[string]string) (int, string) {
		req, _ := http.NewRequest("POST", h.srv.URL+path, strings.NewReader(body))
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]string
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out["status"]
	}
	if code, _ := post("/hooks/email/bounce", `{"email":"a@example.com","type":"bounce"}`, map[string]string{"Authorization": "Bearer wrong"}); code != 401 {
		t.Fatalf("wrong token: %d", code)
	}
	// Postmark hard bounce via basic auth (how Postmark webhook URLs carry credentials).
	code, status := post("/hooks/email/bounce", `{"RecordType":"Bounce","Type":"HardBounce","Email":"Ghost@Example.com","Description":"mailbox does not exist"}`,
		map[string]string{"Authorization": "Basic " + basic("postmark", hookToken)})
	if code != 200 || status != "suppressed" {
		t.Fatalf("hard bounce: %d %s", code, status)
	}
	code, status = post("/hooks/email/bounce?token="+hookToken, `{"RecordType":"Bounce","Type":"SoftBounce","Email":"busy@example.com"}`, nil)
	if code != 200 || status != "ignored" {
		t.Fatalf("soft bounce: %d %s", code, status)
	}
	code, status = post("/hooks/email/bounce", `{"email":"angry@example.com","type":"complaint","reason":"spam button"}`,
		map[string]string{"Authorization": "Bearer " + hookToken})
	if code != 200 || status != "suppressed" {
		t.Fatalf("generic complaint: %d %s", code, status)
	}
	ctx := context.Background()
	for addr, want := range map[string]bool{"ghost@example.com": true, "busy@example.com": false, "angry@example.com": true} {
		if got, _ := h.st.IsSuppressed(ctx, addr); got != want {
			t.Errorf("IsSuppressed(%s) = %v, want %v", addr, got, want)
		}
	}
	log, _ := h.st.ListAllAudit(ctx, 5)
	if len(log) < 2 || log[0].Actor != "bounce-webhook" || log[0].Action != "email.suppressed" {
		t.Fatalf("webhook not audited: %+v", log)
	}

	// Suppressed students aren't emailed, and the console flags them.
	h.createClient("zcw", "Zip Code Wilmington", "ZCW", "")
	csv := "email,full_name,course,cohort,completed_on\nghost@example.com,Gus Ghost,java,J1,2026-09-30\n"
	h.do("POST", "/admin/api/clients/zcw/import?notify=true", csv, "text/csv", true)
	time.Sleep(150 * time.Millisecond)
	if h.mail.Count() != 0 {
		t.Fatal("emailed a suppressed address")
	}
	if list, _ := h.st.ListEmails(ctx, "suppressed", 5); len(list) != 1 {
		t.Fatalf("suppressed row not recorded: %+v", list)
	}
	certs, _ := h.st.ListCertificates(ctx, mustScope(h, "zcw"), "", "")
	if len(certs) != 1 || !certs[0].EmailBlocked {
		t.Fatalf("certificate should show the bounce: %+v", certs)
	}
}

func basic(user, pass string) string {
	req, _ := http.NewRequest("GET", "/", nil)
	req.SetBasicAuth(user, pass)
	return strings.TrimPrefix(req.Header.Get("Authorization"), "Basic ")
}

func TestSystemPage(t *testing.T) {
	h := newHarness(t)
	h.seedPlatform()
	ctx := context.Background()

	// A client admin can't see it.
	admin := h.browser()
	admin.signIn("admin@zcw.example")
	if resp, _ := admin.get("/super/system"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("client admin reached /super/system: %d", resp.StatusCode)
	}

	root := h.browser()
	root.signIn("root@platform.example")
	root.enrollTOTP()
	q2id := failOnce(t, h, "retry-me@example.com")
	if it, _ := h.st.GetEmail(ctx, q2id); it.Status != "failed" || !it.HasBody || !strings.Contains(it.LastError, "451") {
		t.Fatalf("failed message: %+v", it)
	}

	resp, page := root.get("/super/system")
	if resp.StatusCode != 200 || !strings.Contains(page, "retry-me@example.com") || !strings.Contains(page, "Retry") {
		t.Fatalf("system page: %d", resp.StatusCode)
	}
	before := h.mail.Count()
	if resp, _ := root.post("/super/system/emails/"+itoa(q2id)+"/retry", nil); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("retry: %d", resp.StatusCode)
	}
	h.waitMail(before + 1)
	if h.mail.Last().To != "retry-me@example.com" {
		t.Fatalf("retried message went to %s", h.mail.Last().To)
	}

	// Suppress and allow again from the page.
	root.get("/super/system")
	root.post("/super/system/suppressions", url.Values{"email": {"nope@example.com"}, "reason": {"asked us to stop"}})
	if ok, _ := h.st.IsSuppressed(ctx, "nope@example.com"); !ok {
		t.Fatal("manual suppression")
	}
	root.post("/super/system/suppressions/remove", url.Values{"email": {"nope@example.com"}})
	if ok, _ := h.st.IsSuppressed(ctx, "nope@example.com"); ok {
		t.Fatal("unsuppress")
	}
	all, _ := h.st.ListAllAudit(ctx, 10)
	got := map[string]bool{}
	for _, e := range all {
		got[e.Action] = true
	}
	for _, want := range []string{"email.retry", "email.suppressed", "email.unsuppressed"} {
		if !got[want] {
			t.Errorf("audit missing %s", want)
		}
	}
}

// failOnce makes delivery to addr fail once for real (the worker schedules
// a retry about a minute out), then marks it failed while its body is kept,
// as if the server had kept refusing. Returns the message id.
func failOnce(t *testing.T, h *harness, addr string) int64 {
	t.Helper()
	ctx := context.Background()
	h.mail.FailWith(func(m mail.Message) error {
		if m.To == addr {
			return errors.New("451 temporary local problem")
		}
		return nil
	})
	defer h.mail.FailWith(nil)
	h.st.EnsureUser(ctx, addr, "")
	h.browser().send("POST", "/login", url.Values{"email": {addr}})
	for i := 0; i < 200; i++ {
		list, _ := h.st.ListEmails(ctx, "", 20)
		for _, it := range list {
			if it.To == addr && it.Status == "queued" && it.Attempts == 1 {
				if err := h.st.FailEmail(ctx, it.ID, it.LastError); err != nil {
					t.Fatal(err)
				}
				return it.ID
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("delivery attempt never happened")
	return 0
}
