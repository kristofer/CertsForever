package web

import (
	"errors"
	"net/http"
	"net/url"
	"strings"

	"certsforever/internal/certid"
	"certsforever/internal/linkedin"
	"certsforever/internal/ogimage"
	"certsforever/internal/store"
)

// loadPublic resolves {id} to a public certificate. Private and unknown IDs
// both 404 so the page never reveals that a private certificate exists.
// Non-canonical IDs redirect to the canonical URL.
func (s *Server) loadPublic(w http.ResponseWriter, r *http.Request) (*store.Certificate, bool) {
	raw := r.PathValue("id")
	id, ok := certid.Normalize(raw)
	if !ok {
		s.notFound(w)
		return nil, false
	}
	if id != raw {
		target := strings.Replace(r.URL.Path, "/c/"+raw, "/c/"+id, 1)
		http.Redirect(w, r, target, http.StatusMovedPermanently)
		return nil, false
	}
	c, err := s.store.GetCertificate(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && !c.Public()) {
		s.notFound(w)
		return nil, false
	}
	if err != nil {
		s.log.Error("get certificate", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return nil, false
	}
	return c, true
}

func (s *Server) certURL(id string) string { return s.cfg.BaseURL + "/c/" + id }

type certPage struct {
	base
	Cert         *store.Certificate
	CanonicalURL string
	OGImageURL   string
	ShareURL     string
	LearnURL     string
}

func (s *Server) handleCert(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadPublic(w, r)
	if !ok {
		return
	}
	if !isBot(r) {
		s.track(r, c.ID, store.EventView)
	}
	p := certPage{
		base:         s.issuerBase(c),
		Cert:         c,
		CanonicalURL: s.certURL(c.ID),
		OGImageURL:   s.certURL(c.ID) + "/og.png",
		ShareURL:     "/c/" + c.ID + "/share/linkedin",
	}
	if c.Issuer.SiteURL != "" {
		p.LearnURL = "/c/" + c.ID + "/learn"
	}
	p.NoIndex = c.Revoked()
	s.render(w, http.StatusOK, "cert.html", p)
}

func (s *Server) handleOGImage(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadPublic(w, r)
	if !ok {
		return
	}
	png, err := ogimage.Render(ogimage.Card{
		Org:     c.Issuer.Name,
		Heading: "Certificate of Completion",
		Name:    c.RecipientName,
		Course:  c.CourseTitle,
		Footer:  "Issued " + formatMonth(c.IssuedOn) + "  ·  Verify at " + displayHost(s.certURL(c.ID)),
		Revoked: c.Revoked(),
	})
	if err != nil {
		s.log.Error("render og image", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.track(r, c.ID, store.EventOGImage)
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Write(png)
}

// handleCredentialJSON is a machine-readable record of the certificate.
// Phase 2 replaces this with a signed Open Badges 3.0 credential.
func (s *Server) handleCredentialJSON(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadPublic(w, r)
	if !ok {
		return
	}
	out := map[string]any{
		"id":         c.ID,
		"url":        s.certURL(c.ID),
		"recipient":  c.RecipientName,
		"course":     c.CourseTitle,
		"skills":     c.Skills,
		"issued_on":  c.IssuedOn.Format("2006-01-02"),
		"status":     c.Status,
		"issuer":     map[string]string{"name": c.Issuer.Name, "url": c.Issuer.SiteURL},
		"revoked_at": c.RevokedAt,
	}
	w.Header().Set("Access-Control-Allow-Origin", "*")
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleShareLinkedIn(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadPublic(w, r)
	if !ok {
		return
	}
	s.track(r, c.ID, store.EventLinkedInShare)
	http.Redirect(w, r, linkedin.ShareURL(s.certURL(c.ID)), http.StatusFound)
}

// handleLearnMore is the marketing hand-off: count it, tag it, send them on.
func (s *Server) handleLearnMore(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadPublic(w, r)
	if !ok {
		return
	}
	u, err := url.Parse(c.Issuer.SiteURL)
	if c.Issuer.SiteURL == "" || err != nil {
		s.notFound(w)
		return
	}
	s.track(r, c.ID, store.EventLearnMore)
	q := u.Query()
	q.Set("utm_source", "certificate")
	q.Set("utm_medium", "referral")
	q.Set("utm_campaign", c.CourseSlug)
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

type verifyPage struct {
	base
	Query string
	Error string
}

func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("id"))
	p := verifyPage{base: s.platformBase(), Query: q}
	if q != "" {
		if id, ok := certid.Normalize(q); ok {
			http.Redirect(w, r, "/c/"+id, http.StatusSeeOther)
			return
		}
		p.Error = "That doesn't look like a certificate ID. IDs look like ZCW-7K3M9QF2XA: letters, a dash, then 10 characters."
	}
	s.render(w, http.StatusOK, "verify.html", p)
}

// --- student claim flow -------------------------------------------------

type claimPage struct {
	base
	Cert        *store.Certificate
	Token       string
	PublicURL   string
	AddURL      string
	ShareURL    string
	JustUpdated bool
}

func (s *Server) loadClaim(w http.ResponseWriter, r *http.Request) (*store.Certificate, bool) {
	c, err := s.store.GetCertificateByClaimToken(r.Context(), r.PathValue("token"))
	if errors.Is(err, store.ErrNotFound) {
		s.message(w, http.StatusNotFound, "Link not valid",
			"This certificate link is invalid or has been replaced. Contact your instructor for a new one.")
		return nil, false
	}
	if err != nil {
		s.log.Error("claim lookup", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return nil, false
	}
	return c, true
}

func (s *Server) handleClaim(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadClaim(w, r)
	if !ok {
		return
	}
	token := r.PathValue("token")
	p := claimPage{
		base:        s.issuerBase(c),
		Cert:        c,
		Token:       token,
		PublicURL:   s.certURL(c.ID),
		AddURL:      "/claim/" + token + "/linkedin/add",
		ShareURL:    "/c/" + c.ID + "/share/linkedin",
		JustUpdated: r.URL.Query().Has("updated"),
	}
	p.NoIndex = true
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, http.StatusOK, "claim.html", p)
}

func (s *Server) handleClaimUpdate(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadClaim(w, r)
	if !ok {
		return
	}
	if c.Revoked() {
		http.Error(w, "certificate revoked", http.StatusConflict)
		return
	}
	vis := r.FormValue("visibility")
	if err := s.store.SetVisibilityByClaimToken(r.Context(), r.PathValue("token"), vis); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/claim/"+r.PathValue("token")+"?updated=1", http.StatusSeeOther)
}

func (s *Server) handleAddToLinkedIn(w http.ResponseWriter, r *http.Request) {
	c, ok := s.loadClaim(w, r)
	if !ok {
		return
	}
	if !c.Public() || c.Revoked() {
		http.Redirect(w, r, "/claim/"+r.PathValue("token"), http.StatusSeeOther)
		return
	}
	s.track(r, c.ID, store.EventLinkedInAdd)
	http.Redirect(w, r, linkedin.AddToProfileURL(linkedin.Certification{
		Name:    c.CourseTitle,
		OrgID:   c.Issuer.LinkedInOrgID,
		OrgName: c.Issuer.Name,
		Issued:  c.IssuedOn,
		CertURL: s.certURL(c.ID),
		CertID:  c.ID,
	}), http.StatusFound)
}

func displayHost(u string) string {
	u = strings.TrimPrefix(u, "https://")
	return strings.TrimPrefix(u, "http://")
}
