package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"

	"certsforever/internal/emails"
	"certsforever/internal/importer"
	"certsforever/internal/outbox"
	"certsforever/internal/store"
)

// Admin JSON API, authenticated by CERTS_ADMIN_TOKEN: a platform-level
// credential for automation and scripts (people use the consoles). Its
// actions are audited as "admin-token". Every route that touches a client's
// data is nested under /admin/api/clients/{client}/ and goes through
// scoped(), which resolves the client into a store.Scope.

type scopedHandler func(w http.ResponseWriter, r *http.Request, sc store.Scope)

// scoped resolves {client} and passes its Scope to h. Unknown clients 404.
func (s *Server) scoped(h scopedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sc, err := s.store.Scope(r.Context(), r.PathValue("client"))
		if err != nil {
			s.storeError(w, err)
			return
		}
		h(w, r, sc)
	}
}

// storeError maps store errors to HTTP statuses.
func (s *Server) storeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeJSONError(w, http.StatusNotFound, "not found")
	case errors.Is(err, store.ErrInvalid):
		writeJSONError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, store.ErrConflict), errors.Is(err, store.ErrSuspended):
		writeJSONError(w, http.StatusConflict, err.Error())
	default:
		s.log.Error("admin api", "err", err)
		writeJSONError(w, http.StatusInternalServerError, "internal error")
	}
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return false
	}
	return true
}

// --- clients (platform level) ------------------------------------------

type clientJSON struct {
	Slug          string  `json:"slug"`
	Name          *string `json:"name"`
	IDPrefix      *string `json:"id_prefix"`
	SiteURL       *string `json:"site_url"`
	Blurb         *string `json:"blurb"`
	LinkedInOrgID *string `json:"linkedin_org_id"`
	ReplyTo       *string `json:"reply_to"`
	SendReminders *bool   `json:"send_reminders"`
}

func (c clientJSON) input() store.ClientInput {
	return store.ClientInput{Slug: c.Slug, Name: c.Name, IDPrefix: c.IDPrefix, SiteURL: c.SiteURL,
		Blurb: c.Blurb, LinkedInOrgID: c.LinkedInOrgID, ReplyTo: c.ReplyTo, SendReminders: c.SendReminders}
}

func (s *Server) handleListClients(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListClients(r.Context())
	if err != nil {
		s.storeError(w, err)
		return
	}
	if list == nil {
		list = []store.Client{}
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) handleCreateClient(w http.ResponseWriter, r *http.Request) {
	var in clientJSON
	if !decodeJSON(w, r, &in) {
		return
	}
	c, err := s.store.CreateClient(r.Context(), in.input())
	if err != nil {
		s.storeError(w, err)
		return
	}
	sc, _ := s.store.Scope(r.Context(), c.Slug)
	s.audit(r, nil, sc, "client.create", "client", c.Slug, map[string]any{"name": c.Name, "id_prefix": c.IDPrefix})
	writeJSON(w, http.StatusCreated, c)
}

func (s *Server) handleGetClient(w http.ResponseWriter, r *http.Request, sc store.Scope) {
	writeJSON(w, http.StatusOK, sc.Client())
}

func (s *Server) handleUpdateClient(w http.ResponseWriter, r *http.Request, sc store.Scope) {
	var in clientJSON
	if !decodeJSON(w, r, &in) {
		return
	}
	in.Slug = sc.Client().Slug // the URL decides which client; slugs are immutable
	c, err := s.store.UpdateClient(r.Context(), in.input())
	if err != nil {
		s.storeError(w, err)
		return
	}
	s.audit(r, nil, sc, "client.update", "client", c.Slug, changedFields(in))
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) handleClientStatus(w http.ResponseWriter, r *http.Request, sc store.Scope) {
	var body struct {
		Status string `json:"status"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if err := s.store.SetClientStatus(r.Context(), sc.Client().Slug, body.Status); err != nil {
		s.storeError(w, err)
		return
	}
	s.audit(r, nil, sc, "client.status", "client", sc.Client().Slug, map[string]any{"status": body.Status})
	writeJSON(w, http.StatusOK, map[string]string{"status": body.Status})
}

// --- client-scoped ------------------------------------------------------

func (s *Server) handleCreateCourse(w http.ResponseWriter, r *http.Request, sc store.Scope) {
	var c store.Course
	if !decodeJSON(w, r, &c) {
		return
	}
	id, err := s.store.CreateCourse(r.Context(), sc, c)
	if err != nil {
		s.storeError(w, err)
		return
	}
	s.audit(r, nil, sc, "course.save", "course", c.Slug, map[string]any{"title": c.Title})
	c.ID = id
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) handleListCourses(w http.ResponseWriter, r *http.Request, sc store.Scope) {
	list, err := s.store.ListCourses(r.Context(), sc)
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

// IssuedLink is returned for each imported row so the caller can email it
// (or knows the server did: Emailed).
type IssuedLink struct {
	store.IssueResult
	CertificateURL string `json:"certificate_url"`
	ClaimURL       string `json:"claim_url,omitempty"`
	Emailed        bool   `json:"emailed,omitempty"`
}

func wantNotify(r *http.Request) bool {
	v := r.URL.Query().Get("notify")
	return v == "true" || v == "1"
}

// CertificateReadyEmail prepares the "your certificate is ready" email for
// queueing inside the issuing transaction.
func CertificateReadyEmail(q *outbox.Queue, p emails.Platform, c store.Client, to, name, course, certID, claimURL string) (*store.NewEmail, error) {
	m, err := emails.CertificateReady(p, emails.Client{Name: c.Name, ReplyTo: c.ReplyTo, SiteURL: c.SiteURL},
		to, name, course, claimURL)
	if err != nil {
		return nil, err
	}
	return q.Prepare("certificate_ready", m, "certificate", certID)
}

// ClaimURL builds the student's private claim link.
func ClaimURL(baseURL, token string) string { return baseURL + "/claim/" + token }

// handleImport accepts a cohort CSV (see internal/importer) as the request body.
func (s *Server) handleImport(w http.ResponseWriter, r *http.Request, sc store.Scope) {
	reqs, err := importer.Parse(http.MaxBytesReader(w, r.Body, 5<<20))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	notify := wantNotify(r)
	if notify && !s.mailConfigured() {
		writeJSONError(w, http.StatusConflict, "email isn't configured on this server (CERTS_SMTP_URL); import without notify=true and send the claim links yourself")
		return
	}
	var hook store.NotifyFunc
	if notify {
		hook = func(res store.IssueResult) (*store.NewEmail, error) {
			return CertificateReadyEmail(s.queue, s.platform(), sc.Client(), res.Email, res.FullName, res.CourseTitle,
				res.CertificateID, ClaimURL(s.cfg.BaseURL, res.ClaimToken))
		}
	}
	results, err := s.store.IssueAndNotify(r.Context(), sc, reqs, hook)
	if err != nil {
		s.storeError(w, err)
		return
	}
	if notify {
		s.queue.Wake()
	}
	issued := 0
	for _, res := range results {
		if !res.Existing {
			issued++
		}
	}
	s.audit(r, nil, sc, "certificates.issue", "", "", map[string]any{"rows": len(results), "issued": issued, "emailed": notify})
	out := make([]IssuedLink, len(results))
	for i, res := range results {
		out[i] = IssuedLink{IssueResult: res, CertificateURL: s.certURL(res.CertificateID)}
		if res.ClaimToken != "" {
			out[i].ClaimURL = ClaimURL(s.cfg.BaseURL, res.ClaimToken)
			out[i].Emailed = notify
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleListCerts(w http.ResponseWriter, r *http.Request, sc store.Scope) {
	q := r.URL.Query()
	certs, err := s.store.ListCertificates(r.Context(), sc, q.Get("course"), q.Get("cohort"))
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, certs)
}

func (s *Server) handleGetCert(w http.ResponseWriter, r *http.Request, sc store.Scope) {
	c, err := s.store.GetClientCertificate(r.Context(), sc, r.PathValue("id"))
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) handleRevoke(w http.ResponseWriter, r *http.Request, sc store.Scope) {
	var body struct {
		Reason string `json:"reason"`
	}
	if r.ContentLength != 0 && !decodeJSON(w, r, &body) {
		return
	}
	if err := s.store.Revoke(r.Context(), sc, r.PathValue("id"), body.Reason); err != nil {
		s.storeError(w, err)
		return
	}
	s.audit(r, nil, sc, "certificate.revoke", "certificate", r.PathValue("id"), map[string]any{"reason": body.Reason})
	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

func (s *Server) handleNewClaimLink(w http.ResponseWriter, r *http.Request, sc store.Scope) {
	notify := wantNotify(r)
	if notify && !s.mailConfigured() {
		writeJSONError(w, http.StatusConflict, "email isn't configured on this server")
		return
	}
	var token string
	var err error
	if notify {
		token, err = s.store.NewClaimLinkAndNotify(r.Context(), sc, r.PathValue("id"),
			func(c *store.Certificate, tok string) (*store.NewEmail, error) {
				return CertificateReadyEmail(s.queue, s.platform(), sc.Client(), c.Email, c.RecipientName, c.CourseTitle,
					c.ID, ClaimURL(s.cfg.BaseURL, tok))
			})
		if err == nil {
			s.queue.Wake()
		}
	} else {
		token, err = s.store.NewClaimLink(r.Context(), sc, r.PathValue("id"))
	}
	if err != nil {
		s.storeError(w, err)
		return
	}
	s.audit(r, nil, sc, "certificate.claim_link", "certificate", r.PathValue("id"), map[string]any{"emailed": notify})
	writeJSON(w, http.StatusOK, map[string]string{"claim_url": ClaimURL(s.cfg.BaseURL, token)})
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request, sc store.Scope) {
	st, err := s.store.Stats(r.Context(), sc)
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// changedFields lists which client fields a PATCH set (not their values;
// they're on the client record).
func changedFields(c clientJSON) map[string]any {
	var f []string
	for name, set := range map[string]bool{"name": c.Name != nil, "id_prefix": c.IDPrefix != nil,
		"site_url": c.SiteURL != nil, "blurb": c.Blurb != nil, "linkedin_org_id": c.LinkedInOrgID != nil,
		"reply_to": c.ReplyTo != nil, "send_reminders": c.SendReminders != nil} {
		if set {
			f = append(f, name)
		}
	}
	sort.Strings(f)
	return map[string]any{"fields": f}
}
