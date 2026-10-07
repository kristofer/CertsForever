// Package web serves the public certificate pages, the student claim flow
// and the admin JSON API.
package web

import (
	"bytes"
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"certsforever/internal/buildinfo"
	"certsforever/internal/config"
	"certsforever/internal/outbox"
	"certsforever/internal/ratelimit"
	"certsforever/internal/secretbox"
	"certsforever/internal/store"
)

//go:embed templates/*.html static/*
var assets embed.FS

// Server holds dependencies for the HTTP handlers.
type Server struct {
	cfg   config.Config
	store *store.Store
	log   *slog.Logger
	queue *outbox.Queue // nil when email isn't configured
	// emailMode describes delivery for the System page ("SMTP", "log", "off").
	emailMode string
	box       *secretbox.Box
	resolver  Resolver
	limits    limits
	pages     map[string]*template.Template
	mux       *http.ServeMux
}

type limits struct {
	loginIP, loginEmail, linkIP, totp, adminToken, api *ratelimit.Limiter
}

// Deps are the Server's replaceable collaborators.
type Deps struct {
	// Queue is the email outbox. nil means email isn't configured: sign-in
	// links must be printed with the CLI, and nothing is sent to students.
	Queue *outbox.Queue
	// EmailMode describes delivery for the System page.
	EmailMode string
	// Resolver checks custom domains' DNS (default: the system resolver).
	Resolver Resolver
}

// New builds a Server and registers its routes.
func New(cfg config.Config, st *store.Store, log *slog.Logger, deps Deps) (*Server, error) {
	box, err := secretbox.New(cfg.MasterKey)
	if err != nil {
		return nil, fmt.Errorf("master key: %w", err)
	}
	if deps.EmailMode == "" {
		deps.EmailMode = "off: CERTS_SMTP_URL not set"
		if deps.Queue != nil {
			deps.EmailMode = "on"
		}
	}
	if deps.Resolver == nil {
		deps.Resolver = netResolver{net.DefaultResolver}
	}
	s := &Server{cfg: cfg, store: st, log: log, queue: deps.Queue, emailMode: deps.EmailMode, box: box, resolver: deps.Resolver,
		pages: map[string]*template.Template{}, mux: http.NewServeMux(),
		limits: limits{
			loginIP:    ratelimit.New(10, 10, 15*time.Minute), // sign-in emails per IP
			loginEmail: ratelimit.New(3, 3, 15*time.Minute),   // sign-in emails per address
			linkIP:     ratelimit.New(20, 20, 15*time.Minute), // link uses per IP
			totp:       ratelimit.New(5, 5, 5*time.Minute),    // codes per user
			adminToken: ratelimit.New(10, 10, time.Minute),    // failed admin/API-token attempts per IP
			api:        ratelimit.New(120, 120, time.Minute),  // client API requests per token
		}}
	funcs := template.FuncMap{"date": formatDate, "month": formatMonth, "datetime": formatDateTime,
		"asset": assetURL, "pct": pct, "join": strings.Join, "dict": dict, "bytes": humanBytes,
		"sub1": func(n int) int { return n - 1 }}
	for _, p := range []string{"cert.html", "claim.html", "verify.html", "message.html",
		"login.html", "login_confirm.html", "totp.html", "totp_setup.html",
		"account.html", "admin_home.html", "super.html", "system.html", "act_as.html",
		"s_clients.html", "s_client.html", "s_users.html", "s_user.html", "s_audit.html",
		"c_overview.html", "c_certs.html", "c_cert.html", "c_issue.html", "c_cohorts.html", "c_roster.html",
		"c_courses.html", "c_designs.html", "c_design.html", "c_team.html", "c_settings.html", "c_tokens.html"} {
		t, err := template.New("").Funcs(funcs).ParseFS(assets, "templates/layout.html", "templates/parts.html", "templates/"+p)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", p, err)
		}
		s.pages[p] = t
	}
	// The landing page is a standalone document with its own stylesheet.
	lp, err := template.New("").Funcs(funcs).ParseFS(assets, "templates/landing.html")
	if err != nil {
		return nil, fmt.Errorf("parse landing.html: %w", err)
	}
	s.pages["landing.html"] = lp
	s.routes()
	return s, nil
}

func (s *Server) routes() {
	m := s.mux
	m.Handle("GET /static/", http.FileServerFS(assets))
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	m.HandleFunc("GET /readyz", s.handleReady)
	m.HandleFunc("GET /internal/tls-ask", s.handleTLSAsk)

	// Public: what employers and LinkedIn see.
	m.HandleFunc("GET /{$}", s.handleHome)
	m.HandleFunc("GET /verify", s.handleVerify)
	m.HandleFunc("GET /c/{id}", s.handleCert)
	m.HandleFunc("GET /c/{id}/og.png", s.handleOGImage)
	m.HandleFunc("GET /c/{id}/credential.json", s.handleCredentialJSON)
	m.HandleFunc("GET /c/{id}/share/linkedin", s.handleShareLinkedIn)
	m.HandleFunc("GET /c/{id}/learn", s.handleLearnMore)
	m.HandleFunc("GET /assets/{file}", s.handleAsset)
	m.HandleFunc("GET /theme/{file}", s.handleTheme)

	// Student: reached from the emailed claim link.
	m.HandleFunc("GET /claim/{token}", s.handleClaim)
	m.HandleFunc("POST /claim/{token}", s.handleClaimUpdate)
	m.HandleFunc("GET /claim/{token}/linkedin/add", s.handleAddToLinkedIn)

	// Admin JSON API (Authorization: Bearer $CERTS_ADMIN_TOKEN). Everything
	// about one client's data lives under /admin/api/clients/{client}/.
	m.Handle("GET /admin/api/clients", s.admin(s.handleListClients))
	m.Handle("POST /admin/api/clients", s.admin(s.handleCreateClient))
	c := "/admin/api/clients/{client}"
	m.Handle("GET "+c, s.admin(s.scoped(s.handleGetClient)))
	m.Handle("PATCH "+c, s.admin(s.scoped(s.handleUpdateClient)))
	m.Handle("POST "+c+"/status", s.admin(s.scoped(s.handleClientStatus)))
	m.Handle("GET "+c+"/courses", s.admin(s.scoped(s.handleListCourses)))
	m.Handle("POST "+c+"/courses", s.admin(s.scoped(s.handleCreateCourse)))
	m.Handle("POST "+c+"/import", s.admin(s.scoped(s.handleImport)))
	m.Handle("GET "+c+"/certificates", s.admin(s.scoped(s.handleListCerts)))
	m.Handle("GET "+c+"/certificates/{id}", s.admin(s.scoped(s.handleGetCert)))
	m.Handle("POST "+c+"/certificates/{id}/revoke", s.admin(s.scoped(s.handleRevoke)))
	m.Handle("POST "+c+"/certificates/{id}/claim-link", s.admin(s.scoped(s.handleNewClaimLink)))
	m.Handle("GET "+c+"/stats", s.admin(s.scoped(s.handleStats)))

	// Sign-in.
	m.HandleFunc("GET /login", s.handleLoginForm)
	m.HandleFunc("POST /login", s.handleLoginRequest)
	m.HandleFunc("GET /login/totp", s.handleTOTPForm)
	m.HandleFunc("POST /login/totp", s.handleTOTPVerify)
	m.HandleFunc("GET /login/{token}", s.handleLoginLink)
	m.HandleFunc("POST /login/{token}", s.handleLoginConsume)
	m.HandleFunc("POST /logout", s.handleLogout)

	// Signed-in consoles.
	m.HandleFunc("GET /account", s.user(s.handleAccount))
	m.HandleFunc("GET /account/totp", s.user(s.handleTOTPSetup))
	m.HandleFunc("POST /account/totp", s.user(s.handleTOTPEnroll))
	m.HandleFunc("POST /account/sign-out-elsewhere", s.user(s.handleSignOutElsewhere))

	m.HandleFunc("GET /admin", s.user(s.handleAdminHome))
	cc := "/admin/{client}"
	m.HandleFunc("GET "+cc, s.client(s.handleClientConsole))
	m.HandleFunc("GET "+cc+"/certificates", s.client(s.handleCertList))
	m.HandleFunc("GET "+cc+"/certificates/export", s.client(s.handleCertExport))
	m.HandleFunc("GET "+cc+"/certificates/{id}", s.client(s.handleCertDetail))
	m.HandleFunc("POST "+cc+"/certificates/{id}/revoke", s.client(s.handleCertRevoke))
	m.HandleFunc("POST "+cc+"/certificates/{id}/name", s.client(s.handleCertName))
	m.HandleFunc("POST "+cc+"/certificates/{id}/resend", s.client(s.handleCertResend))
	m.HandleFunc("GET "+cc+"/issue", s.client(s.handleIssueForm))
	m.HandleFunc("POST "+cc+"/issue/preview", s.client(s.handleIssuePreview))
	m.HandleFunc("POST "+cc+"/issue", s.client(s.handleIssue))
	m.HandleFunc("GET "+cc+"/cohorts", s.client(s.handleCohorts))
	m.HandleFunc("GET "+cc+"/cohorts/roster", s.client(s.handleRoster))
	m.HandleFunc("POST "+cc+"/cohorts/remind", s.client(s.handleRemind))
	m.HandleFunc("GET "+cc+"/courses", s.client(s.handleCourses))
	m.HandleFunc("POST "+cc+"/courses", s.client(s.handleCourseSave))
	m.HandleFunc("POST "+cc+"/courses/{course}/delete", s.client(s.handleCourseDelete))
	m.HandleFunc("GET "+cc+"/designs", s.client(s.handleDesigns))
	m.HandleFunc("GET "+cc+"/designs/new", s.client(s.handleDesignForm))
	m.HandleFunc("POST "+cc+"/designs", s.client(s.handleDesignSave))
	m.HandleFunc("GET "+cc+"/designs/{design}", s.client(s.handleDesignForm))
	m.HandleFunc("POST "+cc+"/designs/{design}", s.client(s.handleDesignSave))
	m.HandleFunc("POST "+cc+"/designs/{design}/delete", s.client(s.handleDesignDelete))
	m.HandleFunc("GET "+cc+"/designs/{design}/preview", s.client(s.handleDesignPreview))
	m.HandleFunc("GET "+cc+"/designs/{design}/preview.png", s.client(s.handleDesignPreviewPNG))
	m.HandleFunc("GET "+cc+"/team", s.client(s.handleTeam))
	m.HandleFunc("POST "+cc+"/members", s.client(s.handleInviteMember))
	m.HandleFunc("POST "+cc+"/members/{user}/remove", s.client(s.handleRemoveMember))
	m.HandleFunc("GET "+cc+"/settings", s.client(s.handleSettings))
	m.HandleFunc("POST "+cc+"/settings", s.client(s.handleSettingsSave))
	m.HandleFunc("GET "+cc+"/tokens", s.client(s.handleTokens))
	m.HandleFunc("POST "+cc+"/tokens", s.client(s.handleTokenCreate))
	m.HandleFunc("POST "+cc+"/tokens/{token}/revoke", s.client(s.handleTokenRevoke))

	// Client API (Authorization: Bearer cfk_…): one client's token, so no
	// {client} in the path. Same handlers as the platform admin API.
	m.Handle("GET /api/v1/courses", s.apiToken(s.handleListCourses))
	m.Handle("POST /api/v1/courses", s.apiToken(s.handleCreateCourse))
	m.Handle("POST /api/v1/import", s.apiToken(s.handleImport))
	m.Handle("GET /api/v1/certificates", s.apiToken(s.handleListCerts))
	m.Handle("GET /api/v1/certificates/{id}", s.apiToken(s.handleGetCert))
	m.Handle("POST /api/v1/certificates/{id}/revoke", s.apiToken(s.handleRevoke))
	m.Handle("POST /api/v1/certificates/{id}/claim-link", s.apiToken(s.handleNewClaimLink))
	m.Handle("GET /api/v1/stats", s.apiToken(s.handleStats))

	m.HandleFunc("GET /super", s.super(s.handleSuper))
	m.HandleFunc("GET /super/clients", s.super(s.handleSuperClients))
	m.HandleFunc("POST /super/clients", s.super(s.handleSuperCreateClient))
	sc := "/super/clients/{client}"
	m.HandleFunc("GET "+sc, s.super(s.handleSuperClient))
	m.HandleFunc("POST "+sc, s.super(s.handleSuperClientSave))
	m.HandleFunc("POST "+sc+"/status", s.super(s.handleSuperClientStatus))
	m.HandleFunc("POST "+sc+"/act-as", s.super(s.handleActAs))
	m.HandleFunc("POST "+sc+"/members", s.super(s.handleSuperInviteMember))
	m.HandleFunc("POST "+sc+"/members/{user}/remove", s.super(s.handleSuperRemoveMember))
	m.HandleFunc("POST "+sc+"/domains", s.super(s.handleDomainAdd))
	m.HandleFunc("POST "+sc+"/domains/{domain}/check", s.super(s.handleDomainCheck))
	m.HandleFunc("POST "+sc+"/domains/{domain}/verify", s.super(s.handleDomainVerifyManual))
	m.HandleFunc("POST "+sc+"/domains/{domain}/canonical", s.super(s.handleDomainCanonical))
	m.HandleFunc("POST "+sc+"/domains/{domain}/delete", s.super(s.handleDomainDelete))
	m.HandleFunc("POST /super/act-as/stop", s.super(s.handleStopActing))
	m.HandleFunc("GET /super/users", s.super(s.handleSuperUsers))
	m.HandleFunc("GET /super/users/{user}", s.super(s.handleSuperUser))
	m.HandleFunc("POST /super/users/{user}/{action}", s.super(s.handleUserAction))
	m.HandleFunc("POST /super/admins", s.super(s.handleSuperGrant))
	m.HandleFunc("POST /super/admins/{user}/remove", s.super(s.handleSuperRevoke))
	m.HandleFunc("GET /super/audit", s.super(s.handleSuperAudit))
	m.HandleFunc("GET /super/system", s.super(s.handleSystem))
	m.HandleFunc("POST /super/system/emails/{id}/retry", s.super(s.handleRetryEmail))
	m.HandleFunc("POST /super/system/suppressions", s.super(s.handleSuppress))
	m.HandleFunc("POST /super/system/suppressions/remove", s.super(s.handleUnsuppress))

	// Email provider webhook (CERTS_BOUNCE_WEBHOOK_TOKEN).
	m.HandleFunc("POST /hooks/email/bounce", s.handleBounceWebhook)
}

// Handler returns the root handler with middleware applied.
func (s *Server) Handler() http.Handler {
	return s.logRequests(securityHeaders(s.hostRouter(s.mux)))
}

// handleReady reports whether the server can serve traffic: both database
// pools answer and the schema is migrated. Used by Docker HEALTHCHECK,
// load balancers and uptime monitors.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	info := buildinfo.Get()
	v, err := s.store.Health(ctx)
	w.Header().Set("Cache-Control", "no-store")
	if err != nil {
		s.log.Error("readiness check failed", "err", err)
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "unavailable", "version": info.Version})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok", "version": info.Version, "commit": info.Commit, "schema_version": v,
	})
}

// --- middleware ---------------------------------------------------------

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("X-Frame-Options", "DENY")
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
		if (path == "/readyz" || path == "/healthz") && rec.status == http.StatusOK {
			return // health probes every few seconds would drown the log
		}
		// Never log secrets that live in paths: claim and sign-in tokens.
		if strings.HasPrefix(path, "/claim/") {
			path = "/claim/…"
		} else if strings.HasPrefix(path, "/login/") && path != "/login/totp" {
			path = "/login/…"
		}
		s.log.Info("http", "method", r.Method, "path", path, "status", rec.status,
			"dur_ms", time.Since(start).Milliseconds())
	})
}

// admin guards the JSON API with CERTS_ADMIN_TOKEN, a platform-level
// credential for automation. Its actions are audited as "admin-token".
func (s *Server) admin(h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.AdminToken == "" {
			writeJSONError(w, http.StatusServiceUnavailable, "admin API disabled: set CERTS_ADMIN_TOKEN")
			return
		}
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.cfg.AdminToken)) != 1 {
			if !s.limits.adminToken.Allow(s.clientIP(r)) {
				writeJSONError(w, http.StatusTooManyRequests, "too many failed attempts")
				return
			}
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
	User     *store.User // set on console pages: shows the signed-in nav
	CSRF     string
	// Platform: the page belongs to the platform, not a client, so it shows
	// the platform's emblem and favicon. Certificate pages show the school.
	Platform bool
}

// platformBase is for pages that belong to no single client (verify, errors).
func (s *Server) platformBase() base {
	return base{Org: s.cfg.PlatformName, SiteURL: "/", Platform: true}
}

// issuerBase brands a page with the client that issued the certificate.
func (s *Server) issuerBase(c *store.Certificate) base {
	b := base{Org: c.Issuer.Name, OrgBlurb: c.Issuer.Blurb, SiteURL: c.Issuer.SiteURL}
	if b.SiteURL == "" {
		b.SiteURL = "/"
	}
	return b
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
	p := messagePage{base: s.platformBase(), Title: title, Body: body}
	p.NoIndex = true
	s.render(w, status, "message.html", p)
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	p := messagePage{base: s.siteBase(r), Title: "Certificate not found",
		Body: "We couldn't find a public certificate with that ID. Check the ID and try again, " +
			"or ask the certificate holder for their verification link."}
	p.NoIndex = true
	s.render(w, http.StatusNotFound, "message.html", p)
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

// dict builds a map for passing several values to a sub-template.
func dict(kv ...any) (map[string]any, error) {
	if len(kv)%2 != 0 {
		return nil, fmt.Errorf("dict: odd number of arguments")
	}
	m := make(map[string]any, len(kv)/2)
	for i := 0; i < len(kv); i += 2 {
		k, ok := kv[i].(string)
		if !ok {
			return nil, fmt.Errorf("dict: key %v is not a string", kv[i])
		}
		m[k] = kv[i+1]
	}
	return m, nil
}

// humanBytes formats a size like "1.4 MB".
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// pct is n as a whole percentage of total ("–" when total is 0).
func pct(n, total int) string {
	if total == 0 {
		return "–"
	}
	return fmt.Sprintf("%d%%", (n*100+total/2)/total)
}

func formatMonth(t time.Time) string { return t.Format("January 2006") }

func formatDateTime(t time.Time) string { return t.UTC().Format("2006-01-02 15:04 UTC") }

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
