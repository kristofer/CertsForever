package web

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"certsforever/internal/emails"
	"certsforever/internal/imgnorm"
	"certsforever/internal/importer"
	"certsforever/internal/store"
)

// The client console: everything a client admin does day to day. Pages
// share a sub-nav (templates/parts.html) and the clientBase data.

type clientBase struct {
	base
	Client        store.Client
	Impersonating bool
	Tab           string
	Notice        string
	Error         string
}

func (s *Server) clientBase(r *http.Request, a *auth, sc store.Scope, tab string) clientBase {
	return clientBase{base: s.consoleBase(a), Client: sc.Client(), Impersonating: a.impersonating, Tab: tab,
		Notice: notice(r)}
}

func clientPath(sc store.Scope, rest string) string { return "/admin/" + sc.Client().Slug + rest }

// userError turns a store validation/conflict error into a sentence for the page.
func userError(err error) (string, bool) {
	if errors.Is(err, store.ErrInvalid) || errors.Is(err, store.ErrConflict) {
		msg := err.Error()
		for _, p := range []string{"invalid: ", "conflict: "} {
			msg = strings.TrimPrefix(msg, p)
		}
		return strings.ToUpper(msg[:1]) + msg[1:], true
	}
	if errors.Is(err, store.ErrSuspended) {
		return "This client is suspended, so it can't issue certificates. Contact the platform administrator.", true
	}
	return "", false
}

func init() {
	for k, v := range map[string]string{
		"issued":         "Certificates issued.",
		"issued-emailed": "Certificates issued. Students are being emailed their links.",
		"cert-revoked":   "Certificate revoked.",
		"name-corrected": "Name corrected. The certificate's link is unchanged.",
		"resent":         "A new link was emailed to the student. The old link no longer works.",
		"reminded":       "Reminders queued.",
		"course-saved":   "Course saved.",
		"course-deleted": "Course deleted.",
		"design-saved":   "Design saved. New certificates for courses using it will look like this; issued certificates keep their look.",
		"design-deleted": "Design deleted.",
		"settings-saved": "Settings saved.",
		"token-revoked":  "Token revoked. Anything using it will stop working.",
	} {
		notices[k] = v
	}
}

// --- overview ------------------------------------------------------------------

type overviewPage struct {
	clientBase
	Overview *store.Overview
	Stats    []store.CourseStats
	Certs    []store.Certificate
	Activity []store.AuditEntry
}

func (s *Server) handleClientConsole(w http.ResponseWriter, r *http.Request, a *auth, sc store.Scope) {
	ctx := r.Context()
	p := overviewPage{clientBase: s.clientBase(r, a, sc, "overview")}
	var err error
	if p.Overview, err = s.store.GetOverview(ctx, sc); err == nil {
		if p.Stats, err = s.store.Stats(ctx, sc); err == nil {
			if p.Certs, _, err = s.store.SearchCertificates(ctx, sc, store.CertFilter{Limit: 8}); err == nil {
				p.Activity, err = s.store.ListAudit(ctx, sc, 10)
			}
		}
	}
	if err != nil {
		s.serverError(w, err)
		return
	}
	s.render(w, http.StatusOK, "c_overview.html", p)
}

// --- certificates ----------------------------------------------------------------

const certPageSize = 50

type certsPage struct {
	clientBase
	Filter  store.CertFilter
	Courses []store.Course
	Certs   []store.Certificate
	Total   int
	Page    int
	Prev    string
	Next    string
	Export  string
}

func certFilter(q url.Values) store.CertFilter {
	return store.CertFilter{Course: q.Get("course"), Cohort: q.Get("cohort"), Query: q.Get("q"),
		Status: q.Get("status"), Claimed: q.Get("claimed")}
}

func filterQuery(f store.CertFilter) url.Values {
	v := url.Values{}
	for k, val := range map[string]string{"course": f.Course, "cohort": f.Cohort, "q": f.Query,
		"status": f.Status, "claimed": f.Claimed} {
		if val != "" {
			v.Set(k, val)
		}
	}
	return v
}

func (s *Server) handleCertList(w http.ResponseWriter, r *http.Request, a *auth, sc store.Scope) {
	q := r.URL.Query()
	p := certsPage{clientBase: s.clientBase(r, a, sc, "certificates"), Filter: certFilter(q)}
	p.Page, _ = strconv.Atoi(q.Get("page"))
	p.Page = max(p.Page, 1)
	f := p.Filter
	f.Limit, f.Offset = certPageSize, (p.Page-1)*certPageSize
	var err error
	if p.Certs, p.Total, err = s.store.SearchCertificates(r.Context(), sc, f); err == nil {
		p.Courses, err = s.store.ListCourses(r.Context(), sc)
	}
	if err != nil {
		s.serverError(w, err)
		return
	}
	link := func(page int) string {
		v := filterQuery(p.Filter)
		v.Set("page", strconv.Itoa(page))
		return "?" + v.Encode()
	}
	if p.Page > 1 {
		p.Prev = link(p.Page - 1)
	}
	if p.Page*certPageSize < p.Total {
		p.Next = link(p.Page + 1)
	}
	p.Export = filterQuery(p.Filter).Encode()
	s.render(w, http.StatusOK, "c_certs.html", p)
}

// handleCertExport downloads the filtered certificates as CSV or JSON.
func (s *Server) handleCertExport(w http.ResponseWriter, r *http.Request, a *auth, sc store.Scope) {
	certs, _, err := s.store.SearchCertificates(r.Context(), sc, certFilter(r.URL.Query()))
	if err != nil {
		s.serverError(w, err)
		return
	}
	s.audit(r, a, sc, "certificates.export", "", "", map[string]any{"rows": len(certs), "format": r.URL.Query().Get("format")})
	name := fmt.Sprintf("%s-certificates-%s", sc.Client().Slug, time.Now().UTC().Format("2006-01-02"))
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Query().Get("format") == "json" {
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`.json"`)
		writeJSON(w, http.StatusOK, certs)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`.csv"`)
	cw := csv.NewWriter(w)
	cw.Write([]string{"certificate_id", "url", "email", "full_name", "course", "course_title", "cohort",
		"issued_on", "status", "visibility", "claimed_at", "revoked_at", "revoke_reason"})
	for _, c := range certs {
		cw.Write([]string{c.ID, s.certURL(&c), c.Email, csvSafe(c.RecipientName), c.CourseSlug, csvSafe(c.CourseTitle),
			csvSafe(c.CohortName), c.IssuedOn.Format("2006-01-02"), c.Status, c.Visibility, isoTime(c.ClaimedAt),
			isoTime(c.RevokedAt), csvSafe(c.RevokeReason)})
	}
	cw.Flush()
}

// csvSafe stops spreadsheet apps from treating a cell as a formula.
func csvSafe(v string) string {
	if v != "" && strings.ContainsRune("=+-@\t\r", rune(v[0])) {
		return "'" + v
	}
	return v
}

func isoTime(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

type certDetailPage struct {
	clientBase
	Cert     *store.Certificate
	URL      string
	Events   map[string]int
	Emails   []store.OutboxItem
	History  []store.AuditEntry
	MailOn   bool
	NameForm string
}

func (s *Server) handleCertDetail(w http.ResponseWriter, r *http.Request, a *auth, sc store.Scope) {
	s.renderCertDetail(w, r, a, sc, "", "", http.StatusOK)
}

func (s *Server) renderCertDetail(w http.ResponseWriter, r *http.Request, a *auth, sc store.Scope, errMsg, nameForm string, status int) {
	ctx := r.Context()
	c, err := s.store.GetClientCertificate(ctx, sc, r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		s.message(w, http.StatusNotFound, "Not found", "There's no certificate with that ID here.")
		return
	}
	if err != nil {
		s.serverError(w, err)
		return
	}
	p := certDetailPage{clientBase: s.clientBase(r, a, sc, "certificates"), Cert: c, URL: s.certURL(c),
		MailOn: s.mailConfigured(), NameForm: nameForm}
	p.Error = errMsg
	if p.NameForm == "" {
		p.NameForm = c.RecipientName
	}
	var ev map[store.EventKind]int
	if ev, err = s.store.CertificateEvents(ctx, sc, c.ID); err == nil {
		p.Events = map[string]int{}
		for k, n := range ev {
			p.Events[string(k)] = n
		}
		if p.Emails, err = s.store.CertificateEmails(ctx, sc, c.ID); err == nil {
			p.History, err = s.store.ListAuditForTarget(ctx, sc, "certificate", c.ID, 50)
		}
	}
	if err != nil {
		s.serverError(w, err)
		return
	}
	s.render(w, status, "c_cert.html", p)
}

func (s *Server) handleCertRevoke(w http.ResponseWriter, r *http.Request, a *auth, sc store.Scope) {
	id, reason := r.PathValue("id"), strings.TrimSpace(r.FormValue("reason"))
	if reason == "" {
		s.renderCertDetail(w, r, a, sc, "Say why you're revoking it. Verifiers don't see the reason; your team does.", "", http.StatusBadRequest)
		return
	}
	if err := s.store.Revoke(r.Context(), sc, id, reason); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.renderCertDetail(w, r, a, sc, "It's already revoked.", "", http.StatusConflict)
			return
		}
		s.serverError(w, err)
		return
	}
	s.audit(r, a, sc, "certificate.revoke", "certificate", id, map[string]any{"reason": reason})
	redirectNotice(w, r, clientPath(sc, "/certificates/"+id), "cert-revoked")
}

func (s *Server) handleCertName(w http.ResponseWriter, r *http.Request, a *auth, sc store.Scope) {
	id, name := r.PathValue("id"), r.FormValue("name")
	old, err := s.store.CorrectName(r.Context(), sc, id, name)
	if msg, ok := userError(err); ok {
		s.renderCertDetail(w, r, a, sc, msg, name, http.StatusBadRequest)
		return
	}
	if errors.Is(err, store.ErrNotFound) {
		s.message(w, http.StatusNotFound, "Not found", "There's no certificate with that ID here.")
		return
	}
	if err != nil {
		s.serverError(w, err)
		return
	}
	c, _ := s.store.GetClientCertificate(r.Context(), sc, id)
	newName := name
	if c != nil {
		newName = c.RecipientName
	}
	s.audit(r, a, sc, "certificate.name_correct", "certificate", id, map[string]any{"from": old, "to": newName})
	redirectNotice(w, r, clientPath(sc, "/certificates/"+id), "name-corrected")
}

// handleCertResend replaces the claim link and emails the new one.
func (s *Server) handleCertResend(w http.ResponseWriter, r *http.Request, a *auth, sc store.Scope) {
	id := r.PathValue("id")
	if !s.mailConfigured() {
		s.renderCertDetail(w, r, a, sc, "Email isn't configured on this server.", "", http.StatusConflict)
		return
	}
	_, err := s.store.NewClaimLinkAndNotify(r.Context(), sc, id, func(c *store.Certificate, tok string) (*store.NewEmail, error) {
		if c.Revoked() {
			return nil, fmt.Errorf("%w: revoked certificates can't be sent", store.ErrConflict)
		}
		if c.EmailBlocked {
			return nil, fmt.Errorf("%w: mail to %s bounced or was reported, so nothing can be sent to it", store.ErrConflict, c.Email)
		}
		return CertificateReadyEmail(s.queue, s.platform(), sc.Client(), c.Email, c.RecipientName, c.CourseTitle,
			c.ID, ClaimURL(c.PublicBase(s.cfg.BaseURL), tok))
	})
	if msg, ok := userError(err); ok {
		s.renderCertDetail(w, r, a, sc, msg, "", http.StatusConflict)
		return
	}
	if err != nil {
		s.storeOrServerError(w, err)
		return
	}
	s.queue.Wake()
	s.audit(r, a, sc, "certificate.claim_link", "certificate", id, map[string]any{"emailed": true})
	redirectNotice(w, r, clientPath(sc, "/certificates/"+id), "resent")
}

func (s *Server) storeOrServerError(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		s.message(w, http.StatusNotFound, "Not found", "There's nothing here.")
		return
	}
	s.serverError(w, err)
}

// --- issue -------------------------------------------------------------------------

type issuePage struct {
	clientBase
	Courses []store.Course
	MailOn  bool
	CSV     string
	Preview *store.Preview
	Notify  bool
}

const maxCSV = 5 << 20

func (s *Server) handleIssueForm(w http.ResponseWriter, r *http.Request, a *auth, sc store.Scope) {
	s.renderIssue(w, r, a, sc, issuePage{Notify: true}, http.StatusOK)
}

func (s *Server) renderIssue(w http.ResponseWriter, r *http.Request, a *auth, sc store.Scope, p issuePage, status int) {
	errMsg := p.Error
	p.clientBase = s.clientBase(r, a, sc, "issue")
	p.Error = errMsg
	p.MailOn = s.mailConfigured()
	if !p.MailOn {
		p.Notify = false
	}
	var err error
	if p.Courses, err = s.store.ListCourses(r.Context(), sc); err != nil {
		s.serverError(w, err)
		return
	}
	s.render(w, status, "c_issue.html", p)
}

// readIssueCSV takes the CSV from an uploaded file, or else the text field.
func readIssueCSV(w http.ResponseWriter, r *http.Request) (string, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxCSV+1<<20)
	if err := r.ParseMultipartForm(1 << 20); err != nil && !errors.Is(err, http.ErrNotMultipart) {
		return "", errors.New("that upload is too large (5 MB at most)")
	}
	if f, _, err := r.FormFile("file"); err == nil {
		defer f.Close()
		b, err := io.ReadAll(io.LimitReader(f, maxCSV+1))
		if err != nil || len(b) > maxCSV {
			return "", errors.New("that file is too large (5 MB at most)")
		}
		return string(b), nil
	}
	return r.FormValue("csv"), nil
}

// handleIssuePreview shows what an issue would do, row by row.
func (s *Server) handleIssuePreview(w http.ResponseWriter, r *http.Request, a *auth, sc store.Scope) {
	text, err := readIssueCSV(w, r)
	p := issuePage{CSV: text, Notify: r.FormValue("notify") == "on"}
	if err != nil {
		p.Error = err.Error()
		s.renderIssue(w, r, a, sc, p, http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(text) == "" {
		p.Error = "Choose a CSV file or paste the rows."
		s.renderIssue(w, r, a, sc, p, http.StatusBadRequest)
		return
	}
	reqs, err := importer.Parse(strings.NewReader(text))
	if err != nil {
		p.Error = "The CSV has problems: " + err.Error()
		s.renderIssue(w, r, a, sc, p, http.StatusBadRequest)
		return
	}
	if p.Preview, err = s.store.PreviewIssue(r.Context(), sc, reqs); err != nil {
		s.serverError(w, err)
		return
	}
	s.renderIssue(w, r, a, sc, p, http.StatusOK)
}

// handleIssue issues the confirmed CSV. It re-checks everything: the
// preview may be stale, and the hidden field is just form input.
func (s *Server) handleIssue(w http.ResponseWriter, r *http.Request, a *auth, sc store.Scope) {
	r.Body = http.MaxBytesReader(w, r.Body, maxCSV+1<<20)
	text := r.FormValue("csv")
	p := issuePage{CSV: text, Notify: r.FormValue("notify") == "on"}
	reqs, err := importer.Parse(strings.NewReader(text))
	if err != nil {
		p.Error = "The CSV has problems: " + err.Error()
		s.renderIssue(w, r, a, sc, p, http.StatusBadRequest)
		return
	}
	if p.Preview, err = s.store.PreviewIssue(r.Context(), sc, reqs); err != nil {
		s.serverError(w, err)
		return
	}
	if !p.Preview.OK() {
		p.Error = "Nothing was issued: fix the rows marked below and try again."
		if p.Preview.Errors == 0 {
			p.Error = "Nothing to issue: every row already has a certificate."
		}
		s.renderIssue(w, r, a, sc, p, http.StatusBadRequest)
		return
	}
	notify := p.Notify && s.mailConfigured()
	var hook store.NotifyFunc
	if notify {
		hook = func(res store.IssueResult) (*store.NewEmail, error) {
			return CertificateReadyEmail(s.queue, s.platform(), sc.Client(), res.Email, res.FullName, res.CourseTitle,
				res.CertificateID, ClaimURL(s.clientBaseURL(sc), res.ClaimToken))
		}
	}
	results, err := s.store.IssueAndNotify(r.Context(), sc, reqs, hook)
	if msg, ok := userError(err); ok {
		p.Error = msg
		s.renderIssue(w, r, a, sc, p, http.StatusBadRequest)
		return
	}
	if err != nil {
		s.serverError(w, err)
		return
	}
	issued := 0
	for _, res := range results {
		if !res.Existing {
			issued++
		}
	}
	if notify {
		s.queue.Wake()
	}
	s.audit(r, a, sc, "certificates.issue", "", "", map[string]any{"rows": len(results), "issued": issued, "emailed": notify})
	code := "issued"
	if notify {
		code = "issued-emailed"
	}
	http.Redirect(w, r, clientPath(sc, "/certificates?notice="+code+"&claimed=no"), http.StatusSeeOther)
}

// --- cohorts --------------------------------------------------------------------------

type cohortsPage struct {
	clientBase
	Cohorts []store.CohortSummary
}

func (s *Server) handleCohorts(w http.ResponseWriter, r *http.Request, a *auth, sc store.Scope) {
	p := cohortsPage{clientBase: s.clientBase(r, a, sc, "cohorts")}
	var err error
	if p.Cohorts, err = s.store.ListCohorts(r.Context(), sc); err != nil {
		s.serverError(w, err)
		return
	}
	s.render(w, http.StatusOK, "c_cohorts.html", p)
}

type rosterPage struct {
	clientBase
	Course     string
	Cohort     string
	Certs      []store.Certificate
	EmailState map[string]string
	Unclaimed  int
	MailOn     bool
}

func (s *Server) handleRoster(w http.ResponseWriter, r *http.Request, a *auth, sc store.Scope) {
	s.renderRoster(w, r, a, sc, "", http.StatusOK)
}

func (s *Server) renderRoster(w http.ResponseWriter, r *http.Request, a *auth, sc store.Scope, errMsg string, status int) {
	q := r.URL.Query()
	p := rosterPage{clientBase: s.clientBase(r, a, sc, "cohorts"), Course: q.Get("course"), Cohort: q.Get("cohort"),
		MailOn: s.mailConfigured()}
	p.Error = errMsg
	if p.Course == "" || p.Cohort == "" {
		http.Redirect(w, r, clientPath(sc, "/cohorts"), http.StatusSeeOther)
		return
	}
	var err error
	p.Certs, _, err = s.store.SearchCertificates(r.Context(), sc, store.CertFilter{Course: p.Course, Cohort: p.Cohort})
	if err == nil {
		ids := make([]string, len(p.Certs))
		for i, c := range p.Certs {
			ids[i] = c.ID
			if !c.Revoked() && c.ClaimedAt == nil && !c.EmailBlocked {
				p.Unclaimed++
			}
		}
		p.EmailState, err = s.store.LatestEmailStatus(r.Context(), sc, ids)
	}
	if err != nil {
		s.serverError(w, err)
		return
	}
	if len(p.Certs) == 0 {
		s.message(w, http.StatusNotFound, "Not found", "There's no cohort by that name here.")
		return
	}
	s.render(w, status, "c_roster.html", p)
}

// handleRemind emails everyone in a cohort who hasn't opened their link,
// skipping anyone emailed in the last day.
func (s *Server) handleRemind(w http.ResponseWriter, r *http.Request, a *auth, sc store.Scope) {
	q := r.URL.Query()
	if !s.mailConfigured() {
		s.renderRoster(w, r, a, sc, "Email isn't configured on this server.", http.StatusConflict)
		return
	}
	certs, _, err := s.store.SearchCertificates(r.Context(), sc, store.CertFilter{Course: q.Get("course"),
		Cohort: q.Get("cohort"), Status: "active", Claimed: "no"})
	if err != nil {
		s.serverError(w, err)
		return
	}
	cl := sc.Client()
	sent, skipped := 0, 0
	for _, c := range certs {
		if c.EmailBlocked {
			skipped++
			continue
		}
		err := s.store.RemindNow(r.Context(), sc, c.ID, func(c *store.Certificate, token string) (*store.NewEmail, error) {
			m, err := emails.Reminder(s.platform(), emails.Client{Name: cl.Name, ReplyTo: cl.ReplyTo, SiteURL: cl.SiteURL},
				c.Email, c.RecipientName, c.CourseTitle, ClaimURL(c.PublicBase(s.cfg.BaseURL), token))
			if err != nil {
				return nil, err
			}
			return s.queue.Prepare("reminder", m, "certificate", c.ID)
		})
		switch {
		case err == nil:
			sent++
		case errors.Is(err, store.ErrConflict), errors.Is(err, store.ErrNotFound):
			skipped++
		default:
			s.serverError(w, err)
			return
		}
	}
	if sent > 0 {
		s.queue.Wake()
	}
	s.audit(r, a, sc, "cohort.remind", "cohort", q.Get("course")+"/"+q.Get("cohort"), map[string]any{"sent": sent, "skipped": skipped})
	v := url.Values{"course": {q.Get("course")}, "cohort": {q.Get("cohort")}, "notice": {"reminded"}}
	http.Redirect(w, r, clientPath(sc, "/cohorts/roster?"+v.Encode()), http.StatusSeeOther)
}

// --- courses --------------------------------------------------------------------------

type coursesPage struct {
	clientBase
	Courses []store.CourseRow
	Designs []store.Design
	Edit    store.CourseRow // the form
	Editing bool
}

func (s *Server) handleCourses(w http.ResponseWriter, r *http.Request, a *auth, sc store.Scope) {
	p := coursesPage{}
	if slug := r.URL.Query().Get("edit"); slug != "" {
		p.Editing = true
		p.Edit.Slug = slug
	}
	s.renderCourses(w, r, a, sc, p, http.StatusOK)
}

func (s *Server) renderCourses(w http.ResponseWriter, r *http.Request, a *auth, sc store.Scope, p coursesPage, status int) {
	errMsg := p.Error
	p.clientBase = s.clientBase(r, a, sc, "courses")
	p.Error = errMsg
	var err error
	if p.Courses, err = s.store.ListCourseRows(r.Context(), sc); err == nil {
		p.Designs, err = s.store.ListDesigns(r.Context(), sc)
	}
	if err != nil {
		s.serverError(w, err)
		return
	}
	if p.Editing && p.Edit.Title == "" && errMsg == "" {
		found := false
		for _, c := range p.Courses {
			if c.Slug == p.Edit.Slug {
				p.Edit, found = c, true
			}
		}
		if !found {
			s.message(w, http.StatusNotFound, "Not found", "There's no course by that name here.")
			return
		}
	}
	s.render(w, status, "c_courses.html", p)
}

func splitSkills(v string) []string {
	return strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == '\n' })
}

func (s *Server) handleCourseSave(w http.ResponseWriter, r *http.Request, a *auth, sc store.Scope) {
	hours, _ := strconv.Atoi(r.FormValue("hours"))
	designID, _ := strconv.ParseInt(r.FormValue("design_id"), 10, 64)
	c := store.Course{Slug: r.FormValue("slug"), Title: strings.TrimSpace(r.FormValue("title")),
		Description: strings.TrimSpace(r.FormValue("description")), Skills: splitSkills(r.FormValue("skills")), Hours: hours}
	editing := r.FormValue("editing") == "1"
	fail := func(msg string) {
		s.renderCourses(w, r, a, sc, coursesPage{Edit: store.CourseRow{Course: c, DesignID: designID}, Editing: editing,
			clientBase: clientBase{Error: msg}}, http.StatusBadRequest)
	}
	if editing {
		if _, err := s.store.GetCourse(r.Context(), sc, c.Slug); err != nil {
			s.storeOrServerError(w, err)
			return
		}
	} else if _, err := s.store.GetCourse(r.Context(), sc, c.Slug); err == nil {
		fail("A course with that short name already exists. Edit it instead.")
		return
	}
	if _, err := s.store.CreateCourse(r.Context(), sc, c); err != nil {
		if msg, ok := userError(err); ok {
			fail(msg)
			return
		}
		s.serverError(w, err)
		return
	}
	if err := s.store.SetCourseDesign(r.Context(), sc, c.Slug, designID); err != nil {
		if msg, ok := userError(err); ok {
			fail(msg)
			return
		}
		s.serverError(w, err)
		return
	}
	s.audit(r, a, sc, "course.save", "course", strings.ToLower(c.Slug), map[string]any{"title": c.Title, "design_id": designID})
	redirectNotice(w, r, clientPath(sc, "/courses"), "course-saved")
}

func (s *Server) handleCourseDelete(w http.ResponseWriter, r *http.Request, a *auth, sc store.Scope) {
	slug := r.PathValue("course")
	err := s.store.DeleteCourse(r.Context(), sc, slug)
	if msg, ok := userError(err); ok {
		s.renderCourses(w, r, a, sc, coursesPage{clientBase: clientBase{Error: msg}}, http.StatusConflict)
		return
	}
	if err != nil {
		s.storeOrServerError(w, err)
		return
	}
	s.audit(r, a, sc, "course.delete", "course", slug, nil)
	redirectNotice(w, r, clientPath(sc, "/courses"), "course-deleted")
}

// --- designs ---------------------------------------------------------------------------

type designsPage struct {
	clientBase
	Designs []store.Design
}

func (s *Server) handleDesigns(w http.ResponseWriter, r *http.Request, a *auth, sc store.Scope) {
	p := designsPage{clientBase: s.clientBase(r, a, sc, "designs")}
	var err error
	if p.Designs, err = s.store.ListDesigns(r.Context(), sc); err != nil {
		s.serverError(w, err)
		return
	}
	s.render(w, http.StatusOK, "c_designs.html", p)
}

type designPage struct {
	clientBase
	Design store.Design
	Sigs   [2]store.Signatory // form slots
}

func (s *Server) handleDesignForm(w http.ResponseWriter, r *http.Request, a *auth, sc store.Scope) {
	d := store.Design{Heading: store.DefaultDesign().Heading, BodyText: store.DefaultDesign().BodyText,
		AccentColor: store.DefaultDesign().AccentColor}
	if idv := r.PathValue("design"); idv != "" {
		got, err := s.loadDesign(r, sc, idv)
		if err != nil {
			s.storeOrServerError(w, err)
			return
		}
		d = *got
	}
	s.renderDesign(w, r, a, sc, d, "", http.StatusOK)
}

func (s *Server) loadDesign(r *http.Request, sc store.Scope, idv string) (*store.Design, error) {
	id, err := strconv.ParseInt(idv, 10, 64)
	if err != nil {
		return nil, store.ErrNotFound
	}
	return s.store.GetDesign(r.Context(), sc, id)
}

func (s *Server) renderDesign(w http.ResponseWriter, r *http.Request, a *auth, sc store.Scope, d store.Design, errMsg string, status int) {
	p := designPage{clientBase: s.clientBase(r, a, sc, "designs"), Design: d}
	p.Error = errMsg
	copy(p.Sigs[:], d.Signatories)
	s.render(w, status, "c_design.html", p)
}

// upload normalizes and stores an uploaded image field; "" if none was sent.
func (s *Server) upload(r *http.Request, sc store.Scope, field, kind string) (string, error) {
	f, hdr, err := r.FormFile(field)
	if errors.Is(err, http.ErrMissingFile) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer f.Close()
	if hdr.Size == 0 {
		return "", nil
	}
	data, err := io.ReadAll(io.LimitReader(f, imgnorm.MaxUpload+1))
	if err != nil {
		return "", err
	}
	png, wd, ht, err := imgnorm.Normalize(data, kind)
	if err != nil {
		return "", fmt.Errorf("%w: %s: %v", store.ErrInvalid, kind, err)
	}
	return s.store.SaveAsset(r.Context(), sc, kind, png, wd, ht)
}

func (s *Server) handleDesignSave(w http.ResponseWriter, r *http.Request, a *auth, sc store.Scope) {
	r.Body = http.MaxBytesReader(w, r.Body, 3*imgnorm.MaxUpload+1<<20)
	if err := r.ParseMultipartForm(1 << 20); err != nil && !errors.Is(err, http.ErrNotMultipart) {
		s.message(w, http.StatusRequestEntityTooLarge, "Too large", "Images can be up to 2 MB each.")
		return
	}
	var d store.Design
	if idv := r.PathValue("design"); idv != "" {
		got, err := s.loadDesign(r, sc, idv)
		if err != nil {
			s.storeOrServerError(w, err)
			return
		}
		d = *got
	}
	d.Name, d.Heading, d.BodyText = r.FormValue("name"), r.FormValue("heading"), r.FormValue("body_text")
	d.AccentColor = r.FormValue("accent_color")
	var sigs [2]store.Signatory
	copy(sigs[:], d.Signatories)
	fail := func(err error) {
		if msg, ok := userError(err); ok {
			d.Signatories = sigs[:]
			s.renderDesign(w, r, a, sc, d, msg, http.StatusBadRequest)
			return
		}
		s.serverError(w, err)
	}
	if r.FormValue("remove_logo") == "on" {
		d.LogoAssetID = ""
	}
	if id, err := s.upload(r, sc, "logo", "logo"); err != nil {
		fail(err)
		return
	} else if id != "" {
		d.LogoAssetID = id
	}
	for i := range sigs {
		n := strconv.Itoa(i + 1)
		sigs[i].Name, sigs[i].Title = r.FormValue("sig"+n+"_name"), r.FormValue("sig"+n+"_title")
		if r.FormValue("sig"+n+"_remove") == "on" {
			sigs[i].SignatureAssetID = ""
		}
		id, err := s.upload(r, sc, "sig"+n+"_file", "signature")
		if err != nil {
			fail(err)
			return
		}
		if id != "" {
			sigs[i].SignatureAssetID = id
		}
	}
	d.Signatories = sigs[:]
	id, err := s.store.SaveDesign(r.Context(), sc, d)
	if err != nil {
		fail(err)
		return
	}
	s.audit(r, a, sc, "design.save", "design", strconv.FormatInt(id, 10), map[string]any{"name": strings.TrimSpace(d.Name)})
	redirectNotice(w, r, clientPath(sc, "/designs/"+strconv.FormatInt(id, 10)), "design-saved")
}

func (s *Server) handleDesignDelete(w http.ResponseWriter, r *http.Request, a *auth, sc store.Scope) {
	d, err := s.loadDesign(r, sc, r.PathValue("design"))
	if err == nil {
		err = s.store.DeleteDesign(r.Context(), sc, d.ID)
	}
	if msg, ok := userError(err); ok {
		s.renderDesign(w, r, a, sc, *d, msg+". Pick another design for those courses first.", http.StatusConflict)
		return
	}
	if err != nil {
		s.storeOrServerError(w, err)
		return
	}
	s.audit(r, a, sc, "design.delete", "design", strconv.FormatInt(d.ID, 10), map[string]any{"name": d.Name})
	redirectNotice(w, r, clientPath(sc, "/designs"), "design-deleted")
}

// sampleCert is the made-up certificate a design preview shows.
func (s *Server) sampleCert(r *http.Request, sc store.Scope, d *store.Design) *store.Certificate {
	cl := sc.Client()
	c := &store.Certificate{ID: cl.IDPrefix + "-SAMPLE0000", RecipientName: "Ada Lovelace",
		CourseTitle: "Your Course Title", CohortName: "Sample cohort", Skills: []string{"Skill one", "Skill two"},
		IssuedOn: time.Now(), Status: "active", Visibility: "public", Design: d.Snapshot(),
		Issuer: store.Issuer{Slug: cl.Slug, Name: cl.Name, SiteURL: cl.SiteURL, Blurb: cl.Blurb}}
	if courses, err := s.store.ListCourseRows(r.Context(), sc); err == nil {
		for _, co := range courses {
			if co.DesignID == d.ID {
				c.CourseTitle, c.Skills = co.Title, co.Skills
				break
			}
		}
	}
	return c
}

func (s *Server) handleDesignPreview(w http.ResponseWriter, r *http.Request, a *auth, sc store.Scope) {
	d, err := s.loadDesign(r, sc, r.PathValue("design"))
	if err != nil {
		s.storeOrServerError(w, err)
		return
	}
	c := s.sampleCert(r, sc, d)
	p := certPage{base: s.issuerBase(c), Cert: c, Design: c.Design, ThemeURL: themeURL(c.Design.AccentColor), Preview: true}
	if c.Issuer.SiteURL != "" {
		p.LearnURL = "#"
	}
	p.NoIndex = true
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, http.StatusOK, "cert.html", p)
}

func (s *Server) handleDesignPreviewPNG(w http.ResponseWriter, r *http.Request, a *auth, sc store.Scope) {
	d, err := s.loadDesign(r, sc, r.PathValue("design"))
	if err != nil {
		s.storeOrServerError(w, err)
		return
	}
	png, err := s.renderOG(r.Context(), s.sampleCert(r, sc, d))
	if err != nil {
		s.serverError(w, err)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(png)
}

// --- team ------------------------------------------------------------------------------

type teamPage struct {
	clientBase
	Members []store.Member
	Audit   []store.AuditEntry
}

func (s *Server) handleTeam(w http.ResponseWriter, r *http.Request, a *auth, sc store.Scope) {
	s.renderTeam(w, r, a, sc, "", http.StatusOK)
}

func (s *Server) renderTeam(w http.ResponseWriter, r *http.Request, a *auth, sc store.Scope, errMsg string, status int) {
	p := teamPage{clientBase: s.clientBase(r, a, sc, "team")}
	p.Error = errMsg
	var err error
	if p.Members, err = s.store.ListMembers(r.Context(), sc); err == nil {
		p.Audit, err = s.store.ListAudit(r.Context(), sc, 100)
	}
	if err != nil {
		s.serverError(w, err)
		return
	}
	s.render(w, status, "c_team.html", p)
}

// --- settings ---------------------------------------------------------------------------

type settingsPage struct {
	clientBase
	Form store.Client
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request, a *auth, sc store.Scope) {
	p := settingsPage{clientBase: s.clientBase(r, a, sc, "settings"), Form: sc.Client()}
	s.render(w, http.StatusOK, "c_settings.html", p)
}

func (s *Server) handleSettingsSave(w http.ResponseWriter, r *http.Request, a *auth, sc store.Scope) {
	f := func(k string) *string { v := strings.TrimSpace(r.FormValue(k)); return &v }
	reminders := r.FormValue("send_reminders") == "on"
	in := store.ClientInput{Slug: sc.Client().Slug, Name: f("name"), SiteURL: f("site_url"), Blurb: f("blurb"),
		LinkedInOrgID: f("linkedin_org_id"), ReplyTo: f("reply_to"), SendReminders: &reminders}
	if _, err := s.store.UpdateClient(r.Context(), in); err != nil {
		if msg, ok := userError(err); ok {
			p := settingsPage{clientBase: s.clientBase(r, a, sc, "settings"), Form: sc.Client()}
			p.Error = msg
			p.Form.Name, p.Form.SiteURL, p.Form.Blurb = *in.Name, *in.SiteURL, *in.Blurb
			p.Form.LinkedInOrgID, p.Form.ReplyTo, p.Form.SendReminders = *in.LinkedInOrgID, *in.ReplyTo, reminders
			s.render(w, http.StatusBadRequest, "c_settings.html", p)
			return
		}
		s.serverError(w, err)
		return
	}
	s.audit(r, a, sc, "client.update", "client", sc.Client().Slug, map[string]any{"fields": []string{"name", "site_url",
		"blurb", "linkedin_org_id", "reply_to", "send_reminders"}})
	redirectNotice(w, r, clientPath(sc, "/settings"), "settings-saved")
}

// --- API tokens ------------------------------------------------------------------------

type tokensPage struct {
	clientBase
	Tokens   []store.APIToken
	NewToken string
	BaseURL  string
}

func (s *Server) handleTokens(w http.ResponseWriter, r *http.Request, a *auth, sc store.Scope) {
	s.renderTokens(w, r, a, sc, "", "", http.StatusOK)
}

func (s *Server) renderTokens(w http.ResponseWriter, r *http.Request, a *auth, sc store.Scope, newToken, errMsg string, status int) {
	p := tokensPage{clientBase: s.clientBase(r, a, sc, "tokens"), NewToken: newToken, BaseURL: s.cfg.BaseURL}
	p.Error = errMsg
	var err error
	if p.Tokens, err = s.store.ListAPITokens(r.Context(), sc); err != nil {
		s.serverError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, status, "c_tokens.html", p)
}

// handleTokenCreate shows the new token once, in the response to the POST.
func (s *Server) handleTokenCreate(w http.ResponseWriter, r *http.Request, a *auth, sc store.Scope) {
	if a.impersonating {
		// A token acts as the client with no trace of who made it, so
		// platform admins can revoke tokens but not create them.
		s.renderTokens(w, r, a, sc, "", "Platform administrators can revoke tokens but not create them. Ask a client administrator.", http.StatusForbidden)
		return
	}
	actor := a.user.ID
	tok, meta, err := s.store.CreateAPIToken(r.Context(), sc, r.FormValue("name"), &actor)
	if msg, ok := userError(err); ok {
		s.renderTokens(w, r, a, sc, "", msg, http.StatusBadRequest)
		return
	}
	if err != nil {
		s.serverError(w, err)
		return
	}
	s.audit(r, a, sc, "api_token.create", "api_token", meta.Prefix, map[string]any{"name": meta.Name})
	s.renderTokens(w, r, a, sc, tok, "", http.StatusOK)
}

func (s *Server) handleTokenRevoke(w http.ResponseWriter, r *http.Request, a *auth, sc store.Scope) {
	id, _ := strconv.ParseInt(r.PathValue("token"), 10, 64)
	meta, err := s.store.RevokeAPIToken(r.Context(), sc, id)
	if err != nil {
		s.storeOrServerError(w, err)
		return
	}
	s.audit(r, a, sc, "api_token.revoke", "api_token", meta.Prefix, map[string]any{"name": meta.Name})
	redirectNotice(w, r, clientPath(sc, "/tokens"), "token-revoked")
}
