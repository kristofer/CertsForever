package web

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"

	"certsforever/internal/store"
)

// Custom domains. A request's Host is one of:
//
//   - the platform host (from CERTS_BASE_URL): everything is served;
//   - a client's verified custom domain: only the public pages (certificates,
//     claim links, the verify page and their assets) are served, branded for
//     that client. Sign-in, consoles and APIs live only on the platform host,
//     so session cookies never exist on a client's domain;
//   - anything else (localhost, the container name, an IP): treated like the
//     platform host. Caddy's "ask" check means no TLS certificate is issued
//     for such hosts, so in production they're only reachable internally.
//
// Certificate URLs are canonical on the client's chosen domain (or the
// platform's). Asked for on any other known host, they 301 there, so every
// link ever shared keeps working through domain changes.

// site is the client whose custom domain a request arrived on.
type site struct {
	sc     store.Scope
	domain *store.Domain
}

type siteKey struct{}

func siteFrom(r *http.Request) *site {
	st, _ := r.Context().Value(siteKey{}).(*site)
	return st
}

// requestHost is the request's host, lowercase, without port or trailing dot.
// It's the Host header: Caddy passes it through unchanged, and nothing reads
// X-Forwarded-Host, which a client could set.
func requestHost(r *http.Request) string {
	h := r.Host
	if hp, _, err := net.SplitHostPort(h); err == nil {
		h = hp
	}
	return strings.TrimSuffix(strings.ToLower(strings.Trim(h, "[]")), ".")
}

func hostOf(base string) string {
	u, err := url.Parse(base)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// customDomainPath reports whether path is served on a client's domain.
func customDomainPath(path string) bool {
	switch path {
	case "/", "/verify", "/healthz", "/readyz":
		return true
	}
	for _, p := range []string{"/c/", "/claim/", "/static/", "/assets/", "/theme/"} {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// hostRouter applies the rules above.
func (s *Server) hostRouter(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := requestHost(r)
		if host == "" || host == s.platformHost() {
			next.ServeHTTP(w, r)
			return
		}
		sc, d, err := s.store.ClientForHost(r.Context(), host)
		if errors.Is(err, store.ErrNotFound) {
			next.ServeHTTP(w, r)
			return
		}
		if err != nil {
			s.log.Error("host lookup", "host", host, "err", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if !customDomainPath(r.URL.Path) {
			// The console, sign-in and APIs are only on the platform host.
			if r.Method == http.MethodGet || r.Method == http.MethodHead {
				http.Redirect(w, r, s.cfg.BaseURL+r.URL.RequestURI(), http.StatusFound)
				return
			}
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), siteKey{}, &site{sc: sc, domain: d})))
	})
}

// knownHost reports whether the request came in on the platform host or a
// client's domain (as opposed to localhost or an internal name).
func (s *Server) knownHost(r *http.Request) bool {
	return siteFrom(r) != nil || requestHost(r) == s.platformHost()
}

// toCanonical redirects to the same path on base's host when the request
// came in on a different known host. It reports whether it redirected.
func (s *Server) toCanonical(w http.ResponseWriter, r *http.Request, base string) bool {
	if !s.knownHost(r) || requestHost(r) == hostOf(base) {
		return false
	}
	code := http.StatusMovedPermanently
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		code = http.StatusPermanentRedirect // keep the method and body
	}
	http.Redirect(w, r, base+r.URL.RequestURI(), code)
	return true
}

// siteBase brands pages that belong to no certificate (verify, not found):
// as the client on its domain, else as the platform.
func (s *Server) siteBase(r *http.Request) base {
	if st := siteFrom(r); st != nil {
		c := st.sc.Client()
		b := base{Org: c.Name, OrgBlurb: c.Blurb, SiteURL: c.SiteURL}
		if b.SiteURL == "" {
			b.SiteURL = "/"
		}
		return b
	}
	return s.platformBase()
}

// handleTLSAsk is Caddy's on-demand TLS "ask" endpoint: 200 means "you may
// get a certificate for this domain". Only the platform host and verified
// client domains qualify. It answers only direct requests (Caddy calling
// the container), not requests relayed through the proxy from outside.
func (s *Server) handleTLSAsk(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("X-Forwarded-Host") != "" {
		http.NotFound(w, r)
		return
	}
	d := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(r.URL.Query().Get("domain"))), ".")
	if d != "" && d == s.platformHost() {
		w.WriteHeader(http.StatusOK)
		return
	}
	if _, _, err := s.store.ClientForHost(r.Context(), d); err == nil {
		w.WriteHeader(http.StatusOK)
		return
	} else if !errors.Is(err, store.ErrNotFound) {
		s.log.Error("tls ask", "domain", d, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.NotFound(w, r)
}
