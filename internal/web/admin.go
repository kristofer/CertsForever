package web

import (
	"encoding/json"
	"errors"
	"net/http"

	"certsforever/internal/importer"
	"certsforever/internal/store"
)

func (s *Server) handleCreateCourse(w http.ResponseWriter, r *http.Request) {
	var c store.Course
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&c); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	id, err := s.store.CreateCourse(r.Context(), c)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	c.ID = id
	writeJSON(w, http.StatusOK, c)
}

// IssuedLink is returned for each imported row so the caller can email it.
type IssuedLink struct {
	store.IssueResult
	CertificateURL string `json:"certificate_url"`
	ClaimURL       string `json:"claim_url,omitempty"`
}

// ClaimURL builds the student's private claim link.
func ClaimURL(baseURL, token string) string { return baseURL + "/claim/" + token }

// handleImport accepts a cohort CSV (see internal/importer) as the request body.
func (s *Server) handleImport(w http.ResponseWriter, r *http.Request) {
	reqs, err := importer.Parse(http.MaxBytesReader(w, r.Body, 5<<20))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	results, err := s.store.Issue(r.Context(), reqs)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	out := make([]IssuedLink, len(results))
	for i, res := range results {
		out[i] = IssuedLink{IssueResult: res, CertificateURL: s.certURL(res.CertificateID)}
		if res.ClaimToken != "" {
			out[i].ClaimURL = ClaimURL(s.cfg.BaseURL, res.ClaimToken)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleListCerts(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	certs, err := s.store.ListCertificates(r.Context(), q.Get("course"), q.Get("cohort"))
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if certs == nil {
		certs = []store.Certificate{}
	}
	writeJSON(w, http.StatusOK, certs)
}

func (s *Server) handleRevoke(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&body)
	err := s.store.Revoke(r.Context(), r.PathValue("id"), body.Reason)
	if errors.Is(err, store.ErrNotFound) {
		writeJSONError(w, http.StatusNotFound, "no active certificate with that id")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked"})
}

func (s *Server) handleNewClaimLink(w http.ResponseWriter, r *http.Request) {
	token, err := s.store.NewClaimLink(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeJSONError(w, http.StatusNotFound, "no certificate with that id")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"claim_url": ClaimURL(s.cfg.BaseURL, token)})
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	st, err := s.store.Stats(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if st == nil {
		st = []store.CourseStats{}
	}
	writeJSON(w, http.StatusOK, st)
}
