package web

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"certsforever/internal/emails"
	"certsforever/internal/store"
)

// Console pages for signed-in admins. Milestone 2 covers who can get in and
// who can manage whom; the issuing, design and settings screens come in
// Milestones 4 and 5.

func encodeSealed(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
func decodeSealed(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(s)
}

// consoleBase brands console pages with the platform and the signed-in user.
func (s *Server) consoleBase(a *auth) base {
	b := s.platformBase()
	b.NoIndex = true
	b.User = a.user
	b.CSRF = a.sess.CSRF
	return b
}

// notices are fixed messages chosen by a code in the URL, so nothing the
// URL says is ever echoed into the page.
var notices = map[string]string{
	"invited":            "Invitation sent.",
	"invited-nomail":     "Added. Email isn't configured on this server, so no invitation was sent: ask the platform operator to run `certsforever login-link EMAIL`.",
	"invited-suppressed": "Added, but that address previously bounced or complained, so no invitation was sent. A platform administrator can clear it on the System page.",
	"retried":            "Message queued again.",
	"suppressed":         "Address suppressed.",
	"unsuppressed":       "Address can receive email again.",
	"removed":            "Access removed.",
	"created":            "Client created.",
	"status":             "Client status updated.",
	"granted":            "Super admin added.",
	"revoked":            "Super admin removed.",
	"signed-out":         "Signed out of your other sessions.",
}

func notice(r *http.Request) string { return notices[r.URL.Query().Get("notice")] }

func redirectNotice(w http.ResponseWriter, r *http.Request, path, code string) {
	http.Redirect(w, r, path+"?notice="+code, http.StatusSeeOther)
}

// --- account ------------------------------------------------------------

type accountPage struct {
	base
	Notice  string
	Clients []store.Client
}

func (s *Server) handleAccount(w http.ResponseWriter, r *http.Request, a *auth) {
	clients, err := s.store.ClientsForUser(r.Context(), a.user.ID)
	if err != nil {
		s.serverError(w, err)
		return
	}
	s.render(w, http.StatusOK, "account.html", accountPage{base: s.consoleBase(a), Notice: notice(r), Clients: clients})
}

func (s *Server) handleSignOutElsewhere(w http.ResponseWriter, r *http.Request, a *auth) {
	n, err := s.store.DeleteUserSessions(r.Context(), a.user.ID, a.sid)
	if err != nil {
		s.serverError(w, err)
		return
	}
	s.audit(r, a, store.Scope{}, "auth.sign_out_elsewhere", "user", a.user.Email, map[string]any{"sessions": n})
	redirectNotice(w, r, "/account", "signed-out")
}

// --- client admin ---------------------------------------------------------

type adminHomePage struct {
	base
	Clients []store.Client
}

func (s *Server) handleAdminHome(w http.ResponseWriter, r *http.Request, a *auth) {
	var clients []store.Client
	var err error
	if a.user.IsSuper {
		clients, err = s.store.ListClients(r.Context())
	} else {
		clients, err = s.store.ClientsForUser(r.Context(), a.user.ID)
	}
	if err != nil {
		s.serverError(w, err)
		return
	}
	if len(clients) == 1 && !a.user.IsSuper {
		http.Redirect(w, r, "/admin/"+clients[0].Slug, http.StatusSeeOther)
		return
	}
	s.render(w, http.StatusOK, "admin_home.html", adminHomePage{base: s.consoleBase(a), Clients: clients})
}

type clientPage struct {
	base
	Client        store.Client
	Impersonating bool
	Notice        string
	Error         string
	Courses       []store.Course
	Certificates  []store.Certificate
	Members       []store.Member
	Audit         []store.AuditEntry
}

func (s *Server) handleClientConsole(w http.ResponseWriter, r *http.Request, a *auth, sc store.Scope) {
	s.renderClient(w, r, a, sc, "", http.StatusOK)
}

func (s *Server) renderClient(w http.ResponseWriter, r *http.Request, a *auth, sc store.Scope, errMsg string, status int) {
	ctx := r.Context()
	p := clientPage{base: s.consoleBase(a), Client: sc.Client(), Impersonating: a.impersonating,
		Notice: notice(r), Error: errMsg}
	var err error
	if p.Courses, err = s.store.ListCourses(ctx, sc); err == nil {
		if p.Certificates, err = s.store.ListCertificates(ctx, sc, "", ""); err == nil {
			if p.Members, err = s.store.ListMembers(ctx, sc); err == nil {
				p.Audit, err = s.store.ListAudit(ctx, sc, 30)
			}
		}
	}
	if err != nil {
		s.serverError(w, err)
		return
	}
	if len(p.Certificates) > 20 {
		p.Certificates = p.Certificates[:20]
	}
	s.render(w, status, "client.html", p)
}

// handleInviteMember adds a client admin and emails them a sign-in link.
func (s *Server) handleInviteMember(w http.ResponseWriter, r *http.Request, a *auth, sc store.Scope) {
	u, _, err := s.store.EnsureUser(r.Context(), r.FormValue("email"), r.FormValue("name"))
	if errors.Is(err, store.ErrInvalid) {
		s.renderClient(w, r, a, sc, "Enter a valid email address.", http.StatusBadRequest)
		return
	}
	if err != nil {
		s.serverError(w, err)
		return
	}
	actor := a.user.ID
	if err := s.store.AddMember(r.Context(), sc, u.ID, &actor); err != nil {
		s.serverError(w, err)
		return
	}
	s.audit(r, a, sc, "member.invite", "user", u.Email, nil)
	c := sc.Client()
	code, err := s.sendInvite(r, u, fmt.Sprintf("You've been invited to manage %s certificates", c.Name),
		fmt.Sprintf("%s added you as an administrator for %s on %s.", a.user.DisplayName(), c.Name, s.cfg.PlatformName))
	if err != nil {
		s.serverError(w, err)
		return
	}
	redirectNotice(w, r, "/admin/"+c.Slug, code)
}

// sendInvite emails an invitation link (valid for a week). The link is never
// shown to the inviter: anyone holding it can sign in as the invitee.
func (s *Server) sendInvite(r *http.Request, u *store.User, subject, intro string) (notice string, err error) {
	if !s.mailConfigured() {
		return "invited-nomail", nil
	}
	token, err := s.store.CreateLoginToken(r.Context(), u.ID, "invite", store.InviteLinkTTL)
	if err != nil {
		return "", err
	}
	m, err := emails.Invite(s.platform(), u.Email, subject, intro, s.cfg.BaseURL+"/login/"+token)
	if err != nil {
		return "", err
	}
	status, err := s.queue.Send(r.Context(), store.Scope{}, "invite", m, "user", u.Email)
	if err != nil {
		return "", err
	}
	if status == "suppressed" {
		return "invited-suppressed", nil
	}
	return "invited", nil
}

func (s *Server) handleRemoveMember(w http.ResponseWriter, r *http.Request, a *auth, sc store.Scope) {
	id, err := strconv.ParseInt(r.PathValue("user"), 10, 64)
	if err != nil {
		s.message(w, http.StatusNotFound, "Not found", "There's nothing here.")
		return
	}
	u, err := s.store.GetUser(r.Context(), id)
	if err == nil {
		err = s.store.RemoveMember(r.Context(), sc, id)
	}
	if errors.Is(err, store.ErrNotFound) {
		s.message(w, http.StatusNotFound, "Not found", "That person isn't an administrator here.")
		return
	}
	if err != nil {
		s.serverError(w, err)
		return
	}
	s.audit(r, a, sc, "member.remove", "user", u.Email, nil)
	redirectNotice(w, r, "/admin/"+sc.Client().Slug, "removed")
}

// --- super admin ------------------------------------------------------------

type superPage struct {
	base
	Notice  string
	Error   string
	Clients []store.Client
	Supers  []store.User
	Audit   []store.AuditEntry
}

func (s *Server) handleSuper(w http.ResponseWriter, r *http.Request, a *auth) {
	s.renderSuper(w, r, a, "", http.StatusOK)
}

func (s *Server) renderSuper(w http.ResponseWriter, r *http.Request, a *auth, errMsg string, status int) {
	p := superPage{base: s.consoleBase(a), Notice: notice(r), Error: errMsg}
	var err error
	if p.Clients, err = s.store.ListClients(r.Context()); err == nil {
		if p.Supers, err = s.store.ListSuperAdmins(r.Context()); err == nil {
			p.Audit, err = s.store.ListAllAudit(r.Context(), 50)
		}
	}
	if err != nil {
		s.serverError(w, err)
		return
	}
	s.render(w, status, "super.html", p)
}

func optional(v string) *string {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	return &v
}

func (s *Server) handleSuperCreateClient(w http.ResponseWriter, r *http.Request, a *auth) {
	name, prefix := r.FormValue("name"), r.FormValue("id_prefix")
	c, err := s.store.CreateClient(r.Context(), store.ClientInput{
		Slug: r.FormValue("slug"), Name: &name, IDPrefix: &prefix,
		SiteURL: optional(r.FormValue("site_url")), LinkedInOrgID: optional(r.FormValue("linkedin_org_id")),
	})
	if errors.Is(err, store.ErrInvalid) || errors.Is(err, store.ErrConflict) {
		s.renderSuper(w, r, a, strings.TrimPrefix(strings.TrimPrefix(err.Error(), "invalid: "), "conflict: "), http.StatusBadRequest)
		return
	}
	if err != nil {
		s.serverError(w, err)
		return
	}
	sc, err := s.store.Scope(r.Context(), c.Slug)
	if err != nil {
		s.serverError(w, err)
		return
	}
	s.audit(r, a, sc, "client.create", "client", c.Slug, map[string]any{"name": c.Name, "id_prefix": c.IDPrefix})

	code := "created"
	if email := strings.TrimSpace(r.FormValue("admin_email")); email != "" {
		u, _, err := s.store.EnsureUser(r.Context(), email, "")
		if err != nil {
			s.renderSuper(w, r, a, "Client created, but the first admin's email isn't valid. Invite them from the client's page.", http.StatusBadRequest)
			return
		}
		actor := a.user.ID
		if err := s.store.AddMember(r.Context(), sc, u.ID, &actor); err != nil {
			s.serverError(w, err)
			return
		}
		s.audit(r, a, sc, "member.invite", "user", u.Email, nil)
		if code, err = s.sendInvite(r, u, fmt.Sprintf("Your %s certificates are ready to set up", c.Name),
			fmt.Sprintf("%s set up %s on %s and made you its administrator.", a.user.DisplayName(), c.Name, s.cfg.PlatformName)); err != nil {
			s.serverError(w, err)
			return
		}
	}
	redirectNotice(w, r, "/super", code)
}

func (s *Server) handleSuperClientStatus(w http.ResponseWriter, r *http.Request, a *auth) {
	slug, status := r.PathValue("client"), r.FormValue("status")
	if err := s.store.SetClientStatus(r.Context(), slug, status); err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInvalid) {
			s.renderSuper(w, r, a, "Couldn't change that client's status.", http.StatusBadRequest)
			return
		}
		s.serverError(w, err)
		return
	}
	sc, _ := s.store.Scope(r.Context(), slug)
	s.audit(r, a, sc, "client.status", "client", slug, map[string]any{"status": status})
	redirectNotice(w, r, "/super", "status")
}

func (s *Server) handleSuperGrant(w http.ResponseWriter, r *http.Request, a *auth) {
	u, _, err := s.store.EnsureUser(r.Context(), r.FormValue("email"), r.FormValue("name"))
	if errors.Is(err, store.ErrInvalid) {
		s.renderSuper(w, r, a, "Enter a valid email address.", http.StatusBadRequest)
		return
	}
	if err == nil {
		err = s.store.SetSuperAdmin(r.Context(), u.ID, true)
	}
	if err != nil {
		s.serverError(w, err)
		return
	}
	s.audit(r, a, store.Scope{}, "super.grant", "user", u.Email, nil)
	code, err := s.sendInvite(r, u, "You're now a "+s.cfg.PlatformName+" platform administrator",
		a.user.DisplayName()+" made you a platform administrator. You'll set up an authenticator app when you first sign in.")
	if err != nil {
		s.serverError(w, err)
		return
	}
	if code == "invited" {
		code = "granted"
	}
	redirectNotice(w, r, "/super", code)
}

func (s *Server) handleSuperRevoke(w http.ResponseWriter, r *http.Request, a *auth) {
	id, err := strconv.ParseInt(r.PathValue("user"), 10, 64)
	if err != nil {
		s.message(w, http.StatusNotFound, "Not found", "There's nothing here.")
		return
	}
	u, err := s.store.GetUser(r.Context(), id)
	if err == nil {
		err = s.store.SetSuperAdmin(r.Context(), id, false)
	}
	switch {
	case errors.Is(err, store.ErrConflict):
		s.renderSuper(w, r, a, "You can't remove the last platform administrator.", http.StatusConflict)
		return
	case errors.Is(err, store.ErrNotFound):
		s.message(w, http.StatusNotFound, "Not found", "There's nothing here.")
		return
	case err != nil:
		s.serverError(w, err)
		return
	}
	s.audit(r, a, store.Scope{}, "super.revoke", "user", u.Email, nil)
	redirectNotice(w, r, "/super", "revoked")
}
