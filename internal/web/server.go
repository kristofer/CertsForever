// Package web serves the public certificate pages, the student claim flow
// and the admin JSON API.
package web

import (
	"bytes"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"certsforever/internal/config"
	"certsforever/internal/store"
)

//go:embed templates/*.html static/*
var assets embed.FS

// Server holds dependencies for the HTTP handlers.
type Server struct {
	cfg   config.Config
	store *store.Store
	log   *slog.Logger
	pages map[string]*template.Template
	mux   *http.ServeMux
}

// New builds a Server and registers its routes.
func New(cfg config.Config, st *store.Store, log *slog.Logger) (*Server, error) {
	s := &Server{cfg: cfg, store: st, log: log, pages: map[string]*template.Template{}, mux: http.NewServeMux()}
	funcs := template.FuncMap{"date": formatDate, "month": formatMonth}
	for _, p := range []string{"cert.html", "claim.html", "verify.html", "message.html"} {
		t, err := template.New("").Funcs(funcs).ParseFS(assets, "templates/layout.html", "templates/"+p)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", p, err)
		}
		s.pages[p] = t
	}
	s.routes()
	return s, nil
}

func (s *Server) routes() {
	m := s.mux
	m.Handle("GET /static/", http.FileServerFS(assets))
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })

	// Public: what employers and LinkedIn see.
	m.HandleFunc("GET /{$}", s.handleVerify)
	m.HandleFunc("GET /verify", s.handleVerify)
	m.HandleFunc("GET /c/{id}", s.handleCert)
	m.HandleFunc("GET /c/{id}/og.png", s.handleOGImage)
	m.HandleFunc("GET /c/{id}/credential.json", s.handleCredentialJSON)
	m.HandleFunc("GET /c/{id}/share/linkedin", s.handleShareLinkedIn)
	m.HandleFunc("GET /c/{id}/learn", s.handleLearnMore)

	// Student: reached from the emailed claim link.
	m.HandleFunc("GET /claim/{token}", s.handleClaim)
	m.HandleFunc("POST /claim/{token}", s.handleClaimUpdate)
	m.HandleFunc("GET /claim/{token}/linkedin/add", s.handleAddToLinkedIn)

	// Admin JSON API (Authorization: Bearer $CERTS_ADMIN_TOKEN).
	m.Handle("POST /admin/api/courses", s.admin(s.handleCreateCourse))
	m.Handle("POST /admin/api/import", s.admin(s.handleImport))
	m.Handle("GET /admin/api/certificates", s.admin(s.handleListCerts))
	m.Handle("POST /admin/api/certificates/{id}/revoke", s.admin(s.handleRevoke))
	m.Handle("POST /admin/api/certificates/{id}/claim-link", s.admin(s.handleNewClaimLink))
	m.Handle("GET /admin/api/stats", s.admin(s.handleStats))
}

// Handler returns the root handler with middleware applied.
func (s *Server) Handler() http.Handler {
	return s.logRequests(securityHeaders(s.mux))
}

// --- middleware ---------------------------------------------------------

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("Content-Security-Policy",
			"default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; "+
				"frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) { r.status = code; r.ResponseWriter.WriteHeader(code) }

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(rec, r)
		path := r.URL.Path
		if strings.HasPrefix(path, "/claim/") {
			path = "/claim/…" // never log claim tokens
		}
		s.log.Info("http", "method", r.Method, "path", path, "status", rec.status,
			"dur_ms", time.Since(start).Milliseconds())
	})
}

func (s *Server) admin(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.AdminToken == "" {
			writeJSONError(w, http.StatusServiceUnavailable, "admin API disabled: set CERTS_ADMIN_TOKEN")
			return
		}
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.AdminToken)) != 1 {
			writeJSONError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		h(w, r)
	})
}

// --- rendering helpers --------------------------------------------------

// base is embedded in every page's data.
type base struct {
	Org      string
	OrgBlurb string
	SiteURL  string
	NoIndex  bool
}

func (s *Server) base() base {
	return base{Org: s.cfg.OrgName, OrgBlurb: s.cfg.OrgBlurb, SiteURL: s.cfg.SiteURL}
}

func (s *Server) render(w http.ResponseWriter, status int, page string, data any) {
	var buf bytes.Buffer
	if err := s.pages[page].ExecuteTemplate(&buf, "layout", data); err != nil {
		s.log.Error("render", "page", page, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}

type messagePage struct {
	base
	Title, Body string
}

func (s *Server) message(w http.ResponseWriter, status int, title, body string) {
	p := messagePage{base: s.base(), Title: title, Body: body}
	p.NoIndex = true
	s.render(w, status, "message.html", p)
}

func (s *Server) notFound(w http.ResponseWriter) {
	s.message(w, http.StatusNotFound, "Certificate not found",
		"We couldn't find a public certificate with that ID. Check the ID and try again, "+
			"or ask the certificate holder for their verification link.")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func formatDate(v any) string {
	switch t := v.(type) {
	case time.Time:
		return t.Format("January 2, 2006")
	case *time.Time:
		if t != nil {
			return t.Format("January 2, 2006")
		}
	}
	return ""
}

func formatMonth(t time.Time) string { return t.Format("January 2006") }

var botUA = regexp.MustCompile(`(?i)bot|crawl|spider|slurp|preview|facebookexternalhit|embedly|whatsapp|curl|wget|python|go-http`)

func isBot(r *http.Request) bool { return r.UserAgent() == "" || botUA.MatchString(r.UserAgent()) }

func referrerHost(r *http.Request) string {
	if u, err := url.Parse(r.Referer()); err == nil {
		return u.Hostname()
	}
	return ""
}

func (s *Server) track(r *http.Request, certID string, kind store.EventKind) {
	if err := s.store.RecordEvent(r.Context(), certID, kind, referrerHost(r)); err != nil {
		s.log.Warn("record event", "kind", kind, "err", err)
	}
}
