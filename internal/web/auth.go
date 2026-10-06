package web

import (
	"context"
	"crypto/subtle"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"certsforever/internal/emails"
	"certsforever/internal/mail"
	"certsforever/internal/store"
	"certsforever/internal/totp"
)

// Sign-in is by single-use emailed link. The flow:
//
//	GET  /login           enter email
//	POST /login           email a link (same response whether or not the account exists)
//	GET  /login/{token}   confirmation page (mail scanners prefetch links; a GET never signs in)
//	POST /login/{token}   use the link, start a session
//	GET/POST /login/totp  second factor, for anyone enrolled (required for super admins)
//
// Roles:
//   - super admin: everything; must have TOTP enrolled and verified this session
//   - client admin: only clients they're a member of; other clients are 404

// auth is the signed-in user for a request.
type auth struct {
	sess          *store.Session
	sid           string
	user          *store.User
	impersonating bool // super admin acting in a client they're not a member of
}

type authedHandler func(w http.ResponseWriter, r *http.Request, a *auth)
type clientHandler func(w http.ResponseWriter, r *http.Request, a *auth, sc store.Scope)

func (s *Server) cookieName() string {
	if strings.HasPrefix(s.cfg.BaseURL, "https://") {
		return "__Host-cf_session" // __Host- requires Secure, Path=/ and no Domain
	}
	return "cf_session"
}

func (s *Server) setSessionCookie(w http.ResponseWriter, sid string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     s.cookieName(),
		Value:    sid,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   strings.HasPrefix(s.cfg.BaseURL, "https://"),
		SameSite: http.SameSiteLaxMode,
	})
}

// clientIP is the remote address, or the last X-Forwarded-For hop when the
// server sits behind a trusted reverse proxy (which appends the real client).
func (s *Server) clientIP(r *http.Request) string {
	if s.cfg.TrustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			if ip := strings.TrimSpace(parts[len(parts)-1]); net.ParseIP(ip) != nil {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// loadSession returns the request's session, or nil.
func (s *Server) loadSession(r *http.Request) *auth {
	c, err := r.Cookie(s.cookieName())
	if err != nil || c.Value == "" {
		return nil
	}
	sess, err := s.store.GetSession(r.Context(), c.Value)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.log.Error("load session", "err", err)
		}
		return nil
	}
	if time.Since(sess.LastSeenAt) > 5*time.Minute {
		s.store.TouchSession(r.Context(), c.Value)
	}
	return &auth{sess: sess, sid: c.Value, user: sess.User}
}

// sameOrigin rejects cross-site form posts: if the browser says where the
// request came from, it must be us.
func (s *Server) sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		if ref := r.Referer(); ref != "" {
			if u, err := url.Parse(ref); err == nil {
				origin = u.Scheme + "://" + u.Host
			}
		}
	}
	return origin == "" || origin == s.cfg.BaseURL
}

func (s *Server) checkCSRF(r *http.Request, a *auth) bool {
	got := r.FormValue("csrf")
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(a.sess.CSRF)) == 1
}

// needsTOTP reports whether the session must pass the second factor first.
func needsTOTP(a *auth) bool { return a.user.TOTPEnabled && !a.sess.TOTPVerified }

// user wraps handlers that need a signed-in, fully verified user.
func (s *Server) user(h authedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a := s.loadSession(r)
		if a == nil {
			if r.Method == http.MethodGet {
				http.Redirect(w, r, "/login", http.StatusSeeOther)
			} else {
				s.message(w, http.StatusForbidden, "Signed out", "Your session has ended. Sign in again.")
			}
			return
		}
		if needsTOTP(a) {
			http.Redirect(w, r, "/login/totp", http.StatusSeeOther)
			return
		}
		// Super admins must enrol a second factor before doing anything else.
		if a.user.IsSuper && !a.user.TOTPEnabled && !strings.HasPrefix(r.URL.Path, "/account") {
			http.Redirect(w, r, "/account/totp", http.StatusSeeOther)
			return
		}
		if r.Method == http.MethodPost && (!s.sameOrigin(r) || !s.checkCSRF(r, a)) {
			s.message(w, http.StatusForbidden, "Request blocked",
				"That form had expired or came from another site. Go back, reload the page and try again.")
			return
		}
		h(w, r, a)
	}
}

// super wraps platform-only handlers.
func (s *Server) super(h authedHandler) http.HandlerFunc {
	return s.user(func(w http.ResponseWriter, r *http.Request, a *auth) {
		if !a.user.IsSuper {
			s.message(w, http.StatusForbidden, "Not allowed", "This area is for platform administrators.")
			return
		}
		h(w, r, a)
	})
}

// client wraps handlers for one client's console. Members get in. A super
// admin who isn't a member gets in only after choosing to act as the client
// (recorded on the session and in the audit log); until then GETs show the
// "act as" page and POSTs are refused. While acting, a banner shows and
// every action is audited as impersonated. Everyone else gets 404, which
// doesn't reveal that the client exists.
func (s *Server) client(h clientHandler) http.HandlerFunc {
	return s.user(func(w http.ResponseWriter, r *http.Request, a *auth) {
		sc, err := s.store.Scope(r.Context(), r.PathValue("client"))
		if errors.Is(err, store.ErrNotFound) {
			s.message(w, http.StatusNotFound, "Not found", "There's nothing here.")
			return
		}
		if err != nil {
			s.serverError(w, err)
			return
		}
		member, err := s.store.IsMember(r.Context(), sc, a.user.ID)
		if err != nil {
			s.serverError(w, err)
			return
		}
		if !member && !a.user.IsSuper {
			s.message(w, http.StatusNotFound, "Not found", "There's nothing here.")
			return
		}
		if !member && a.sess.ActingClientID != sc.Client().ID {
			if r.Method != http.MethodGet {
				s.message(w, http.StatusForbidden, "Not acting as this client",
					"Open the client's console and choose to act as it first.")
				return
			}
			s.renderActAs(w, r, a, sc)
			return
		}
		a.impersonating = !member
		h(w, r, a, sc)
	})
}

func (s *Server) serverError(w http.ResponseWriter, err error) {
	s.log.Error("request failed", "err", err)
	s.message(w, http.StatusInternalServerError, "Something went wrong", "Please try again in a moment.")
}

// auditAs records an action by a non-person actor (e.g. "bounce-webhook").
func (s *Server) auditAs(r *http.Request, actor string, sc store.Scope, action, targetType, targetID string, details map[string]any) {
	e := store.AuditEntry{Actor: actor, Action: action, TargetType: targetType, TargetID: targetID,
		Details: details, IP: s.clientIP(r)}
	if err := s.store.Audit(r.Context(), sc, e); err != nil {
		s.log.Error("audit write failed", "action", action, "err", err)
	}
}

// audit records an action by a signed-in user (a may be nil for the admin
// token, CLI and anonymous events).
func (s *Server) audit(r *http.Request, a *auth, sc store.Scope, action, targetType, targetID string, details map[string]any) {
	e := store.AuditEntry{Action: action, TargetType: targetType, TargetID: targetID, Details: details, IP: s.clientIP(r)}
	switch {
	case a != nil:
		id := a.user.ID
		e.ActorUserID, e.Actor, e.Impersonated = &id, a.user.Email, a.impersonating
	case r.Context().Value(actorKey{}) != nil:
		e.Actor = r.Context().Value(actorKey{}).(string)
	case r.Header.Get("Authorization") != "":
		e.Actor = "admin-token"
	default:
		e.Actor = "anonymous"
	}
	if err := s.store.Audit(r.Context(), sc, e); err != nil {
		s.log.Error("audit write failed", "action", action, "err", err)
	}
}

func (s *Server) mailConfigured() bool { return s.queue != nil }

func (s *Server) platform() emails.Platform {
	return emails.Platform{Name: s.cfg.PlatformName, BaseURL: s.cfg.BaseURL}
}

// --- pages ------------------------------------------------------------

type loginPage struct {
	base
	Sent        bool
	NoMail      bool
	Email       string
	SignedOut   bool
	Error       string
	ConfirmUser *store.User
	Token       string
}

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	if a := s.loadSession(r); a != nil && !needsTOTP(a) {
		http.Redirect(w, r, s.homeFor(a.user), http.StatusSeeOther)
		return
	}
	p := loginPage{base: s.platformBase(), SignedOut: r.URL.Query().Has("signed_out"), NoMail: !s.mailConfigured()}
	p.NoIndex = true
	s.render(w, http.StatusOK, "login.html", p)
}

func (s *Server) handleLoginRequest(w http.ResponseWriter, r *http.Request) {
	p := loginPage{base: s.platformBase(), NoMail: !s.mailConfigured()}
	p.NoIndex = true
	if !s.sameOrigin(r) {
		s.message(w, http.StatusForbidden, "Request blocked", "That form came from another site.")
		return
	}
	email, err := store.NormalizeEmail(r.FormValue("email"))
	if err != nil {
		p.Error = "Enter the email address you were invited with."
		s.render(w, http.StatusBadRequest, "login.html", p)
		return
	}
	p.Email, p.Sent = email, true
	if !s.limits.loginIP.Allow(s.clientIP(r)) || !s.limits.loginEmail.Allow(strings.ToLower(email)) {
		// Same response as success: don't help anyone probe or flood.
		s.render(w, http.StatusOK, "login.html", p)
		return
	}
	// The lookup, link and queueing happen after the response, so how long
	// it takes doesn't reveal whether the address has an account.
	if s.mailConfigured() {
		go s.queueSignIn(email)
	}
	s.render(w, http.StatusOK, "login.html", p)
}

func (s *Server) queueSignIn(email string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	u, err := s.store.GetUserByEmail(ctx, email)
	if err != nil || u.Disabled {
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			s.log.Error("login lookup", "err", err)
		}
		return
	}
	token, err := s.store.CreateLoginToken(ctx, u.ID, "login", store.LoginLinkTTL)
	if err == nil {
		var m mail.Message
		if m, err = emails.SignIn(s.platform(), u.Email, s.cfg.BaseURL+"/login/"+token,
			int(store.LoginLinkTTL.Minutes())); err == nil {
			_, err = s.queue.Send(ctx, store.Scope{}, "sign_in", m, "user", u.Email)
		}
	}
	if err != nil {
		s.log.Error("queue sign-in email", "err", err)
	}
}

func (s *Server) handleLoginLink(w http.ResponseWriter, r *http.Request) {
	u, err := s.store.PeekLoginToken(r.Context(), r.PathValue("token"))
	if err != nil {
		s.message(w, http.StatusNotFound, "Link expired",
			"This sign-in link has expired or was already used. Request a new one from the sign-in page.")
		return
	}
	p := loginPage{base: s.platformBase(), ConfirmUser: u, Token: r.PathValue("token")}
	p.NoIndex = true
	w.Header().Set("Cache-Control", "no-store")
	// same-origin (not no-referrer): the token never leaves for another site,
	// and browsers still send a real Origin on the form post. With
	// no-referrer, Chrome sends "Origin: null" and sign-in would be blocked.
	w.Header().Set("Referrer-Policy", "same-origin")
	s.render(w, http.StatusOK, "login_confirm.html", p)
}

func (s *Server) handleLoginConsume(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) || !s.limits.linkIP.Allow(s.clientIP(r)) {
		s.message(w, http.StatusForbidden, "Request blocked", "Please wait a moment and try again.")
		return
	}
	u, purpose, err := s.store.ConsumeLoginToken(r.Context(), r.PathValue("token"))
	if err != nil {
		s.message(w, http.StatusNotFound, "Link expired",
			"This sign-in link has expired or was already used. Request a new one from the sign-in page.")
		return
	}
	// A fresh session id on every sign-in (no session fixation).
	if old := s.loadSession(r); old != nil {
		s.store.DeleteSession(r.Context(), old.sid)
	}
	sid, sess, err := s.store.CreateSession(r.Context(), u.ID, false, s.clientIP(r), r.UserAgent())
	if err != nil {
		s.serverError(w, err)
		return
	}
	s.setSessionCookie(w, sid, int(store.SessionMax.Seconds()))
	a := &auth{sess: sess, sid: sid, user: u}
	s.audit(r, a, store.Scope{}, "auth.sign_in", "user", u.Email, map[string]any{"link": purpose})
	switch {
	case u.TOTPEnabled:
		http.Redirect(w, r, "/login/totp", http.StatusSeeOther)
	case u.IsSuper:
		http.Redirect(w, r, "/account/totp", http.StatusSeeOther)
	default:
		http.Redirect(w, r, s.homeFor(u), http.StatusSeeOther)
	}
}

func (s *Server) homeFor(u *store.User) string {
	if u.IsSuper {
		return "/super"
	}
	return "/admin"
}

type totpPage struct {
	base
	Error  string
	Secret string // setup only
	Sealed string // setup only: the pending secret, encrypted for the round trip
	URI    string
}

func (s *Server) handleTOTPForm(w http.ResponseWriter, r *http.Request) {
	a := s.loadSession(r)
	if a == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	if !needsTOTP(a) {
		http.Redirect(w, r, s.homeFor(a.user), http.StatusSeeOther)
		return
	}
	p := totpPage{base: s.consoleBase(a)}
	s.render(w, http.StatusOK, "totp.html", p)
}

func (s *Server) handleTOTPVerify(w http.ResponseWriter, r *http.Request) {
	a := s.loadSession(r)
	if a == nil || !needsTOTP(a) {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	p := totpPage{base: s.consoleBase(a)}
	if !s.sameOrigin(r) || !s.checkCSRF(r, a) {
		s.message(w, http.StatusForbidden, "Request blocked", "Reload the page and try again.")
		return
	}
	if !s.limits.totp.Allow(strconv.FormatInt(a.user.ID, 10)) {
		p.Error = "Too many attempts. Wait a few minutes and try again."
		s.render(w, http.StatusTooManyRequests, "totp.html", p)
		return
	}
	if !s.verifyTOTP(r, a.user.ID, r.FormValue("code")) {
		s.audit(r, a, store.Scope{}, "auth.totp_failed", "user", a.user.Email, nil)
		p.Error = "That code didn't work. Check your authenticator app and try the current code."
		s.render(w, http.StatusUnauthorized, "totp.html", p)
		return
	}
	if err := s.store.MarkSessionTOTPVerified(r.Context(), a.sid); err != nil {
		s.serverError(w, err)
		return
	}
	http.Redirect(w, r, s.homeFor(a.user), http.StatusSeeOther)
}

func totpAD(userID int64) string { return "totp:user:" + strconv.FormatInt(userID, 10) }
func enrollAD(userID int64, sid string) string {
	return "totp-enroll:user:" + strconv.FormatInt(userID, 10) + ":" + sid
}

// verifyTOTP checks a code against the user's secret and burns its time step.
func (s *Server) verifyTOTP(r *http.Request, userID int64, code string) bool {
	sealed, err := s.store.TOTPSecret(r.Context(), userID)
	if err != nil {
		return false
	}
	secret, err := s.box.Open(sealed, totpAD(userID))
	if err != nil {
		s.log.Error("TOTP secret can't be decrypted (was CERTS_MASTER_KEY changed?)", "user", userID)
		return false
	}
	counter, ok := totp.Verify(string(secret), code, time.Now(), 1)
	if !ok {
		return false
	}
	return s.store.UseTOTPCounter(r.Context(), userID, counter) == nil
}

func (s *Server) handleTOTPSetup(w http.ResponseWriter, r *http.Request, a *auth) {
	if a.user.TOTPEnabled {
		http.Redirect(w, r, "/account", http.StatusSeeOther)
		return
	}
	secret := totp.NewSecret()
	s.renderTOTPSetup(w, a, secret, "", http.StatusOK)
}

func (s *Server) renderTOTPSetup(w http.ResponseWriter, a *auth, secret, errMsg string, status int) {
	p := totpPage{
		base:   s.consoleBase(a),
		Error:  errMsg,
		Secret: totp.Group(secret),
		Sealed: encodeSealed(s.box.Seal([]byte(secret), enrollAD(a.user.ID, a.sid))),
		URI:    totp.URI(s.cfg.PlatformName, a.user.Email, secret),
	}
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, status, "totp_setup.html", p)
}

func (s *Server) handleTOTPEnroll(w http.ResponseWriter, r *http.Request, a *auth) {
	if a.user.TOTPEnabled {
		http.Redirect(w, r, "/account", http.StatusSeeOther)
		return
	}
	raw, err := decodeSealed(r.FormValue("sealed"))
	var secret []byte
	if err == nil {
		secret, err = s.box.Open(raw, enrollAD(a.user.ID, a.sid))
	}
	if err != nil {
		http.Redirect(w, r, "/account/totp", http.StatusSeeOther)
		return
	}
	if !s.limits.totp.Allow(strconv.FormatInt(a.user.ID, 10)) {
		s.renderTOTPSetup(w, a, string(secret), "Too many attempts. Wait a few minutes.", http.StatusTooManyRequests)
		return
	}
	counter, ok := totp.Verify(string(secret), r.FormValue("code"), time.Now(), 1)
	if !ok {
		s.renderTOTPSetup(w, a, string(secret), "That code didn't match. Make sure the key was entered correctly and try the current code.", http.StatusBadRequest)
		return
	}
	if err := s.store.EnableTOTP(r.Context(), a.user.ID, s.box.Seal(secret, totpAD(a.user.ID)), counter); err != nil {
		s.serverError(w, err)
		return
	}
	if err := s.store.MarkSessionTOTPVerified(r.Context(), a.sid); err != nil {
		s.serverError(w, err)
		return
	}
	s.audit(r, a, store.Scope{}, "auth.totp_enabled", "user", a.user.Email, nil)
	http.Redirect(w, r, s.homeFor(a.user), http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if a := s.loadSession(r); a != nil {
		if !s.sameOrigin(r) || !s.checkCSRF(r, a) {
			s.message(w, http.StatusForbidden, "Request blocked", "Reload the page and try again.")
			return
		}
		s.store.DeleteSession(r.Context(), a.sid)
		s.audit(r, a, store.Scope{}, "auth.sign_out", "user", a.user.Email, nil)
	}
	s.setSessionCookie(w, "", -1)
	http.Redirect(w, r, "/login?signed_out=1", http.StatusSeeOther)
}
