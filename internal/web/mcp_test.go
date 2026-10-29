package web

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"certsforever/internal/store"
)

// bearer adds a client API token to every request the MCP client makes.
type bearer struct{ token string }

// RoundTrip sends r with the token's Authorization header.
func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

// mcpFixture is two clients with certificates and an MCP session for the first.
type mcpFixture struct {
	h       *harness
	cs      *mcp.ClientSession
	ids     []string // zcw's certificates: lin@example.com, sam@example.com
	otherID string   // the other client's certificate
}

// newMCPFixture issues certificates to two clients, makes an API token
// named "Claude" for zcw and connects an MCP client with it.
// Fails the test if any step fails.
func newMCPFixture(t *testing.T, o *harnessOpts) *mcpFixture {
	t.Helper()
	h := newHarnessWith(t, o)
	h.createClient("zcw", "Zip Code", "ZCW", "")
	h.createClient("other", "Other School", "OTH", "")
	f := &mcpFixture{h: h}
	for _, c := range []struct{ client, csv string }{
		{"zcw", "lin@example.com,Lin Smth,java,C1,2026-09-30\nsam@example.com,Sam Lee,java,C1,2026-09-30\n"},
		{"other", "pat@example.com,Pat Doe,java,C9,2026-09-30\n"},
	} {
		resp, b := h.do("POST", "/admin/api/clients/"+c.client+"/import",
			"email,full_name,course,cohort,completed_on\n"+c.csv, "text/csv", true)
		if resp.StatusCode != 200 {
			t.Fatalf("import %s: %d %s", c.client, resp.StatusCode, b)
		}
		var links []IssuedLink
		json.Unmarshal([]byte(b), &links)
		for _, l := range links {
			if c.client == "zcw" {
				f.ids = append(f.ids, l.CertificateID)
			} else {
				f.otherID = l.CertificateID
			}
		}
	}
	tok, _, err := h.st.CreateAPIToken(context.Background(), mustScope(h, "zcw"), "Claude", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.cs = mcpConnect(t, h, tok)
	return f
}

// mcpConnect opens an MCP session to /mcp with token. Fails the test on error.
func mcpConnect(t *testing.T, h *harness, token string) *mcp.ClientSession {
	t.Helper()
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: h.srv.URL + "/mcp", HTTPClient: &http.Client{Transport: bearer{token}},
		DisableStandaloneSSE: true, MaxRetries: -1}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

// call runs a tool and decodes its structured result into out (when non-nil).
// Returns the result text and whether the tool reported an error.
// Fails the test on a protocol error.
func (f *mcpFixture) call(t *testing.T, name string, args map[string]any, out any) (string, bool) {
	t.Helper()
	res, err := f.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var text strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text.WriteString(tc.Text)
		}
	}
	if out != nil && !res.IsError {
		b, _ := json.Marshal(res.StructuredContent)
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatalf("%s: decode %s: %v", name, b, err)
		}
	}
	return text.String(), res.IsError
}

// auditFor returns zcw's audit entries with the given action.
func (f *mcpFixture) auditFor(t *testing.T, action string) []store.AuditEntry {
	t.Helper()
	all, err := f.h.st.ListAudit(context.Background(), mustScope(f.h, "zcw"), 50)
	if err != nil {
		t.Fatal(err)
	}
	var out []store.AuditEntry
	for _, e := range all {
		if e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

func TestMCPRequiresAClientToken(t *testing.T) {
	h := newHarness(t)
	init := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"x","version":"0"}}}`
	for _, tok := range []string{"", adminToken, "cfk_not-a-real-token"} {
		if resp, _ := apiDo(h, tok, "POST", "/mcp", init); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("token %q: %d", tok, resp.StatusCode)
		}
	}
}

func TestMCPListsTools(t *testing.T) {
	f := newMCPFixture(t, nil)
	res, err := f.cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	readOnly := map[string]bool{}
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
		readOnly[tool.Name] = tool.Annotations != nil && tool.Annotations.ReadOnlyHint
	}
	sort.Strings(names)
	want := "cohort_roster correct_name get_certificate list_cohorts list_courses overview preview_issue remind_cohort resend_claim_link search_certificates"
	if got := strings.Join(names, " "); got != want {
		t.Fatalf("tools:\n got %s\nwant %s", got, want)
	}
	for _, n := range []string{"overview", "list_courses", "list_cohorts", "search_certificates", "cohort_roster", "get_certificate", "preview_issue"} {
		if !readOnly[n] {
			t.Errorf("%s should be read-only", n)
		}
	}
	for _, n := range []string{"correct_name", "resend_claim_link", "remind_cohort"} {
		if readOnly[n] {
			t.Errorf("%s writes, so it can't be read-only", n)
		}
	}
}

func TestMCPReadTools(t *testing.T) {
	f := newMCPFixture(t, nil)

	var ov struct {
		Client   struct{ Slug, Name string }
		Overview store.Overview
		Courses  []store.CourseStats
	}
	f.call(t, "overview", nil, &ov)
	if ov.Client.Slug != "zcw" || ov.Overview.Certificates != 2 || len(ov.Courses) != 1 {
		t.Fatalf("overview: %+v", ov)
	}

	var courses struct{ Courses []store.Course }
	f.call(t, "list_courses", nil, &courses)
	if len(courses.Courses) != 1 || courses.Courses[0].Slug != "java" {
		t.Fatalf("courses: %+v", courses)
	}

	var cohorts struct{ Cohorts []store.CohortSummary }
	f.call(t, "list_cohorts", nil, &cohorts)
	if len(cohorts.Cohorts) != 1 || cohorts.Cohorts[0].Name != "C1" || cohorts.Cohorts[0].Unclaimed != 2 {
		t.Fatalf("cohorts (the other client's C9 must not appear): %+v", cohorts)
	}

	var found struct {
		Certificates []store.Certificate
		Total        int
	}
	f.call(t, "search_certificates", map[string]any{"query": "sam"}, &found)
	if found.Total != 1 || found.Certificates[0].Email != "sam@example.com" {
		t.Fatalf("search sam: %+v", found)
	}
	f.call(t, "search_certificates", map[string]any{"limit": 1}, &found)
	if found.Total != 2 || len(found.Certificates) != 1 {
		t.Fatalf("limit 1: total %d, page %d", found.Total, len(found.Certificates))
	}

	var roster struct {
		Certificates []store.Certificate
		EmailStatus  map[string]string `json:"email_status"`
	}
	f.call(t, "cohort_roster", map[string]any{"course": "java", "cohort": "C1"}, &roster)
	if len(roster.Certificates) != 2 {
		t.Fatalf("roster: %+v", roster)
	}

	var detail struct {
		Certificate store.Certificate
		URL         string
		History     []store.AuditEntry
	}
	text, isErr := f.call(t, "get_certificate", map[string]any{"id": f.ids[0]}, &detail)
	if isErr || detail.Certificate.ID != f.ids[0] || !strings.Contains(detail.URL, "/c/"+f.ids[0]) {
		t.Fatalf("get_certificate: %s %+v", text, detail)
	}
	if strings.Contains(text, "claim_token") || strings.Contains(text, "/claim/") {
		t.Fatalf("certificate detail leaks a claim link: %s", text)
	}
}

func TestMCPIsConfinedToTheTokensClient(t *testing.T) {
	f := newMCPFixture(t, nil)
	if text, isErr := f.call(t, "get_certificate", map[string]any{"id": f.otherID}, nil); !isErr || !strings.Contains(text, "no certificate") {
		t.Fatalf("other client's certificate: error=%v %s", isErr, text)
	}
	var found struct{ Total int }
	f.call(t, "search_certificates", map[string]any{"query": "pat@example.com"}, &found)
	if found.Total != 0 {
		t.Fatal("search reached the other client's certificate")
	}
	if text, isErr := f.call(t, "correct_name", map[string]any{"id": f.otherID, "name": "Hacked"}, nil); !isErr {
		t.Fatalf("renamed the other client's certificate: %s", text)
	}
	if text, isErr := f.call(t, "resend_claim_link", map[string]any{"id": f.otherID}, nil); !isErr {
		t.Fatalf("resent the other client's certificate: %s", text)
	}
}

func TestMCPPreviewIssueChangesNothing(t *testing.T) {
	f := newMCPFixture(t, nil)
	csv := "email,full_name,course,cohort,completed_on\nlin@example.com,Lin Smth,java,C1,2026-09-30\nnew@example.com,New Person,java,C2,2026-10-30\nbad@example.com,Bad,nosuch,C2,2026-10-30\n"
	var p store.Preview
	if text, isErr := f.call(t, "preview_issue", map[string]any{"csv": csv}, &p); isErr {
		t.Fatal(text)
	}
	if p.New != 1 || p.Existing != 1 || p.Errors != 1 {
		t.Fatalf("preview: %+v", p)
	}
	if _, n, _ := f.h.st.SearchCertificates(context.Background(), mustScope(f.h, "zcw"), store.CertFilter{}); n != 2 {
		t.Fatalf("preview issued certificates: now %d", n)
	}
	if text, isErr := f.call(t, "preview_issue", map[string]any{"csv": "not,a,valid\ncsv"}, nil); !isErr {
		t.Fatalf("bad CSV accepted: %s", text)
	}
}

func TestMCPCorrectName(t *testing.T) {
	f := newMCPFixture(t, nil)
	var out struct{ From, To string }
	if text, isErr := f.call(t, "correct_name", map[string]any{"id": f.ids[0], "name": "Lin Smith"}, &out); isErr {
		t.Fatal(text)
	}
	if out.From != "Lin Smth" || out.To != "Lin Smith" {
		t.Fatalf("correct_name: %+v", out)
	}
	c, _ := f.h.st.GetCertificate(context.Background(), f.ids[0])
	if c.RecipientName != "Lin Smith" {
		t.Fatalf("name not saved: %q", c.RecipientName)
	}
	entries := f.auditFor(t, "certificate.name_correct")
	if len(entries) != 1 || entries[0].Actor != "api-token:Claude" || entries[0].Details["via"] != "mcp" {
		t.Fatalf("not audited as the token via MCP: %+v", entries)
	}
	if text, isErr := f.call(t, "correct_name", map[string]any{"id": f.ids[0], "name": "Lin Smith"}, nil); !isErr || !strings.Contains(text, "already") {
		t.Fatalf("same name accepted: %s", text)
	}
}

func TestMCPResendClaimLink(t *testing.T) {
	f := newMCPFixture(t, nil)
	if text, isErr := f.call(t, "resend_claim_link", map[string]any{"id": f.ids[0]}, nil); isErr {
		t.Fatal(text)
	}
	f.h.waitMail(1)
	tok := claimTokenFromMail(t, f.h, "lin@example.com")
	if resp, _ := f.h.do("GET", "/claim/"+tok, "", "", false); resp.StatusCode != 200 {
		t.Fatalf("emailed link: %d", resp.StatusCode)
	}
	if len(f.auditFor(t, "certificate.claim_link")) != 1 {
		t.Fatal("resend not audited")
	}

	// Revoked certificates can't be resent.
	if err := f.h.st.Revoke(context.Background(), mustScope(f.h, "zcw"), f.ids[1], "test"); err != nil {
		t.Fatal(err)
	}
	if text, isErr := f.call(t, "resend_claim_link", map[string]any{"id": f.ids[1]}, nil); !isErr || !strings.Contains(strings.ToLower(text), "revoked") {
		t.Fatalf("revoked certificate resent: %s", text)
	}
}

func TestMCPRemindCohort(t *testing.T) {
	f := newMCPFixture(t, nil)
	// Lin was emailed just now, so only Sam gets a reminder.
	f.call(t, "resend_claim_link", map[string]any{"id": f.ids[0]}, nil)
	var out struct{ Sent, Skipped int }
	if text, isErr := f.call(t, "remind_cohort", map[string]any{"course": "java", "cohort": "C1"}, &out); isErr {
		t.Fatal(text)
	}
	if out.Sent != 1 || out.Skipped != 1 {
		t.Fatalf("remind: %+v", out)
	}
	f.h.waitMail(2)
	entries := f.auditFor(t, "cohort.remind")
	if len(entries) != 1 || entries[0].TargetID != "java/C1" {
		t.Fatalf("remind audit: %+v", entries)
	}
}

func TestMCPWritesNeedEmail(t *testing.T) {
	f := newMCPFixture(t, &harnessOpts{noEmail: true})
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{"resend_claim_link", map[string]any{"id": f.ids[0]}},
		{"remind_cohort", map[string]any{"course": "java", "cohort": "C1"}},
	} {
		if text, isErr := f.call(t, c.tool, c.args, nil); !isErr || !strings.Contains(text, "email isn't configured") {
			t.Fatalf("%s without email: %s", c.tool, text)
		}
	}
}

func TestMCPRevokedTokenStopsWorking(t *testing.T) {
	f := newMCPFixture(t, nil)
	sc := mustScope(f.h, "zcw")
	list, _ := f.h.st.ListAPITokens(context.Background(), sc)
	if _, err := f.h.st.RevokeAPIToken(context.Background(), sc, list[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "overview"}); err == nil {
		t.Fatal("revoked token still works")
	}
}
