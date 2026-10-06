package web

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"certsforever/internal/store"
)

// The super console: platform administrators manage clients, people and
// the platform itself. Every route is wrapped in s.super, so client admins
// get 403 (and never see client data from here).

type superBase struct {
	base
	Tab    string
	Notice string
	Error  string
}

func (s *Server) superBase(r *http.Request, a *auth, tab string) superBase {
	return superBase{base: s.consoleBase(a), Tab: tab, Notice: notice(r)}
}

func init() {
	for k, v := range map[string]string{
		"client-saved":      "Client saved.",
		"acting":            "You're now acting as this client. Your actions are recorded in its audit log.",
		"stopped-acting":    "You've stopped acting as that client.",
		"domain-added":      "Domain added. Point it at this server, then check it.",
		"domain-verified":   "Domain verified.",
		"domain-canonical":  "Certificate links now use this domain. Links on other domains redirect here.",
		"domain-platform":   "Certificate links use the platform domain again. Links on the client's domains redirect there.",
		"domain-deleted":    "Domain removed.",
		"user-disabled":     "User disabled and signed out everywhere.",
		"user-enabled":      "User can sign in again.",
		"user-2fa-reset":    "Two-step sign-in reset. They'll set it up again next time they sign in.",
		"user-signed-out":   "User signed out everywhere.",
		"invited-member":    "Administrator added and invited.",
		"member-removed":    "Administrator removed.",
		"status-active":     "Client reactivated.",
		"status-suspended":  "Client suspended. Its certificates stay online; it can't issue new ones.",
		"superadmin-gone":   "Platform administrator removed.",
		"superadmin-gained": "Platform administrator added.",
	} {
		notices[k] = v
	}
}

// --- overview -------------------------------------------------------------------

type superPage struct {
	superBase
	Stats   *store.PlatformStats
	Clients []store.ClientSummary
	Audit   []store.AuditEntry
}

func (s *Server) handleSuper(w http.ResponseWriter, r *http.Request, a *auth) {
	p := superPage{superBase: s.superBase(r, a, "overview")}
	var err error
	if p.Stats, err = s.store.GetPlatformStats(r.Context()); err == nil {
		if p.Clients, err = s.store.ListClientSummaries(r.Context()); err == nil {
			p.Audit, _, err = s.store.SearchAudit(r.Context(), store.AuditFilter{Limit: 15})
		}
	}
	if err != nil {
		s.serverError(w, err)
		return
	}
	s.render(w, http.StatusOK, "super.html", p)
}

// --- clients --------------------------------------------------------------------

type superClientsPage struct {
	superBase
	Clients []store.ClientSummary
	Form    map[string]string
}

func (s *Server) handleSuperClients(w http.ResponseWriter, r *http.Request, a *auth) {
	s.renderSuperClients(w, r, a, nil, "", http.StatusOK)
}

func (s *Server) renderSuperClients(w http.ResponseWriter, r *http.Request, a *auth, form map[string]string, errMsg string, status int) {
	p := superClientsPage{superBase: s.superBase(r, a, "clients"), Form: form}
	p.Error = errMsg
	var err error
	if p.Clients, err = s.store.ListClientSummaries(r.Context()); err != nil {
		s.serverError(w, err)
		return
	}
	s.render(w, status, "s_clients.html", p)
}

func optional(v string) *string {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	return &v
}

func trimErr(err error) string {
	msg, _ := userError(err)
	if msg == "" {
		msg = err.Error()
	}
	return msg
}

func (s *Server) handleSuperCreateClient(w http.ResponseWriter, r *http.Request, a *auth) {
	form := map[string]string{}
	for _, k := range []string{"name", "slug", "id_prefix", "site_url", "linkedin_org_id", "admin_email"} {
		form[k] = strings.TrimSpace(r.FormValue(k))
	}
	name, prefix := form["name"], form["id_prefix"]
	c, err := s.store.CreateClient(r.Context(), store.ClientInput{
		Slug: form["slug"], Name: &name, IDPrefix: &prefix,
		SiteURL: optional(form["site_url"]), LinkedInOrgID: optional(form["linkedin_org_id"]),
	})
	if errors.Is(err, store.ErrInvalid) || errors.Is(err, store.ErrConflict) {
		s.renderSuperClients(w, r, a, form, trimErr(err), http.StatusBadRequest)
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
	if email := form["admin_email"]; email != "" {
		code, err = s.inviteAdmin(r, a, sc, email, "", fmt.Sprintf("Your %s certificates are ready to set up", c.Name),
			fmt.Sprintf("%s set up %s on %s and made you its administrator.", a.user.DisplayName(), c.Name, s.cfg.PlatformName))
		if errors.Is(err, store.ErrInvalid) {
			redirectNotice(w, r, "/super/clients/"+c.Slug, "created")
			return
		}
		if err != nil {
			s.serverError(w, err)
			return
		}
	}
	redirectNotice(w, r, "/super/clients/"+c.Slug, code)
}

type superClientPage struct {
	superBase
	Client       store.Client
	PrefixLocked bool
	Overview     *store.Overview
	Members      []store.Member
	Domains      []store.Domain
	PlatformHost string
	Scheme       string // for test links to custom domains
	Audit        []store.AuditEntry
	Acting       bool
	DNSResult    string // the latest domain check, explained
}

func (s *Server) superScope(w http.ResponseWriter, r *http.Request) (store.Scope, bool) {
	sc, err := s.store.Scope(r.Context(), r.PathValue("client"))
	if errors.Is(err, store.ErrNotFound) {
		s.message(w, http.StatusNotFound, "Not found", "There's no client by that name.")
		return sc, false
	}
	if err != nil {
		s.serverError(w, err)
		return sc, false
	}
	return sc, true
}

func (s *Server) handleSuperClient(w http.ResponseWriter, r *http.Request, a *auth) {
	sc, ok := s.superScope(w, r)
	if !ok {
		return
	}
	s.renderSuperClient(w, r, a, sc, "", "", http.StatusOK)
}

func (s *Server) renderSuperClient(w http.ResponseWriter, r *http.Request, a *auth, sc store.Scope, errMsg, dns string, status int) {
	ctx := r.Context()
	p := superClientPage{superBase: s.superBase(r, a, "clients"), Client: sc.Client(), PlatformHost: s.platformHost(),
		Scheme: strings.SplitN(s.cfg.BaseURL, "://", 2)[0],
		Acting: a.sess.ActingClientID == sc.Client().ID, DNSResult: dns}
	p.Error = errMsg
	var err error
	if p.PrefixLocked, err = s.store.PrefixLocked(ctx, sc); err == nil {
		if p.Overview, err = s.store.GetOverview(ctx, sc); err == nil {
			if p.Members, err = s.store.ListMembers(ctx, sc); err == nil {
				if p.Domains, err = s.store.ListDomains(ctx, sc); err == nil {
					p.Audit, err = s.store.ListAudit(ctx, sc, 20)
				}
			}
		}
	}
	if err != nil {
		s.serverError(w, err)
		return
	}
	s.render(w, status, "s_client.html", p)
}

func (s *Server) handleSuperClientSave(w http.ResponseWriter, r *http.Request, a *auth) {
	sc, ok := s.superScope(w, r)
	if !ok {
		return
	}
	f := func(k string) *string { v := strings.TrimSpace(r.FormValue(k)); return &v }
	reminders := r.FormValue("send_reminders") == "on"
	in := store.ClientInput{Slug: sc.Client().Slug, Name: f("name"), SiteURL: f("site_url"), Blurb: f("blurb"),
		LinkedInOrgID: f("linkedin_org_id"), ReplyTo: f("reply_to"), SendReminders: &reminders}
	changed := []string{"name", "site_url", "blurb", "linkedin_org_id", "reply_to", "send_reminders"}
	if p := strings.ToUpper(strings.TrimSpace(r.FormValue("id_prefix"))); p != "" && p != sc.Client().IDPrefix {
		in.IDPrefix = &p
		changed = append(changed, "id_prefix")
	}
	if _, err := s.store.UpdateClient(r.Context(), in); err != nil {
		if msg, ok := userError(err); ok {
			s.renderSuperClient(w, r, a, sc, msg, "", http.StatusBadRequest)
			return
		}
		s.serverError(w, err)
		return
	}
	s.audit(r, a, sc, "client.update", "client", sc.Client().Slug, map[string]any{"fields": changed})
	redirectNotice(w, r, "/super/clients/"+sc.Client().Slug, "client-saved")
}

func (s *Server) handleSuperClientStatus(w http.ResponseWriter, r *http.Request, a *auth) {
	sc, ok := s.superScope(w, r)
	if !ok {
		return
	}
	status, reason := r.FormValue("status"), strings.TrimSpace(r.FormValue("reason"))
	if err := s.store.SetClientStatusReason(r.Context(), sc.Client().Slug, status, reason); err != nil {
		if msg, ok := userError(err); ok {
			s.renderSuperClient(w, r, a, sc, msg, "", http.StatusBadRequest)
			return
		}
		s.serverError(w, err)
		return
	}
	details := map[string]any{"status": status}
	if status == "suspended" && reason != "" {
		details["reason"] = reason
	}
	s.audit(r, a, sc, "client.status", "client", sc.Client().Slug, details)
	redirectNotice(w, r, "/super/clients/"+sc.Client().Slug, "status-"+status)
}

func (s *Server) handleSuperInviteMember(w http.ResponseWriter, r *http.Request, a *auth) {
	sc, ok := s.superScope(w, r)
	if !ok {
		return
	}
	c := sc.Client()
	code, err := s.inviteAdmin(r, a, sc, r.FormValue("email"), r.FormValue("name"),
		fmt.Sprintf("You've been invited to manage %s certificates", c.Name),
		fmt.Sprintf("%s added you as an administrator for %s on %s.", a.user.DisplayName(), c.Name, s.cfg.PlatformName))
	if errors.Is(err, store.ErrInvalid) {
		s.renderSuperClient(w, r, a, sc, "Enter a valid email address.", "", http.StatusBadRequest)
		return
	}
	if err != nil {
		s.serverError(w, err)
		return
	}
	if code == "invited" {
		code = "invited-member"
	}
	redirectNotice(w, r, "/super/clients/"+c.Slug, code)
}

func (s *Server) handleSuperRemoveMember(w http.ResponseWriter, r *http.Request, a *auth) {
	sc, ok := s.superScope(w, r)
	if !ok {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("user"), 10, 64)
	u, err := s.store.GetUser(r.Context(), id)
	if err == nil {
		err = s.store.RemoveMember(r.Context(), sc, id)
	}
	if err != nil {
		s.storeOrServerError(w, err)
		return
	}
	s.audit(r, a, sc, "member.remove", "user", u.Email, nil)
	redirectNotice(w, r, "/super/clients/"+sc.Client().Slug, "member-removed")
}

// --- acting as a client -------------------------------------------------------------

type actAsPage struct {
	base
	Client store.Client
	Return string
	Other  string // the client this session is acting as now, if any
}

// renderActAs is shown when a super admin opens a console of a client they
// aren't a member of: acting as it is a deliberate, recorded step.
func (s *Server) renderActAs(w http.ResponseWriter, r *http.Request, a *auth, sc store.Scope) {
	p := actAsPage{base: s.consoleBase(a), Client: sc.Client(), Return: r.URL.RequestURI()}
	if id := a.sess.ActingClientID; id != 0 {
		for _, c := range s.mustClients(r) {
			if c.ID == id {
				p.Other = c.Name
			}
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, http.StatusOK, "act_as.html", p)
}

func (s *Server) mustClients(r *http.Request) []store.Client {
	list, _ := s.store.ListClients(r.Context())
	return list
}

// safeReturn accepts only a path inside the client's console.
func safeReturn(v, slug string) string {
	prefix := "/admin/" + slug
	u, err := url.Parse(v)
	if err != nil || u.IsAbs() || u.Host != "" || strings.HasPrefix(v, "//") ||
		(u.Path != prefix && !strings.HasPrefix(u.Path, prefix+"/")) {
		return prefix
	}
	return u.RequestURI()
}

func (s *Server) handleActAs(w http.ResponseWriter, r *http.Request, a *auth) {
	sc, ok := s.superScope(w, r)
	if !ok {
		return
	}
	if err := s.store.SetActing(r.Context(), a.sid, sc.Client().ID); err != nil {
		s.serverError(w, err)
		return
	}
	// Recorded as an impersonated action, in the client's own log.
	a.impersonating = true
	s.audit(r, a, sc, "client.act_as", "client", sc.Client().Slug, nil)
	target := safeReturn(r.FormValue("return"), sc.Client().Slug)
	sep := "?"
	if strings.Contains(target, "?") {
		sep = "&"
	}
	http.Redirect(w, r, target+sep+"notice=acting", http.StatusSeeOther)
}

func (s *Server) handleStopActing(w http.ResponseWriter, r *http.Request, a *auth) {
	if id := a.sess.ActingClientID; id != 0 {
		var slug string
		for _, c := range s.mustClients(r) {
			if c.ID == id {
				slug = c.Slug
			}
		}
		if err := s.store.SetActing(r.Context(), a.sid, 0); err != nil {
			s.serverError(w, err)
			return
		}
		if sc, err := s.store.Scope(r.Context(), slug); err == nil {
			a.impersonating = true
			s.audit(r, a, sc, "client.act_as_end", "client", slug, nil)
			redirectNotice(w, r, "/super/clients/"+slug, "stopped-acting")
			return
		}
	}
	redirectNotice(w, r, "/super", "stopped-acting")
}

// --- domains ------------------------------------------------------------------------

// Resolver is the DNS lookups the domain check needs (net.DefaultResolver
// in production, a fake in tests).
type Resolver interface {
	LookupCNAME(ctx context.Context, host string) (string, error)
	LookupHost(ctx context.Context, host string) ([]string, error)
}

// platformHost is the host certificate links use by default.
func (s *Server) platformHost() string {
	u, err := url.Parse(s.cfg.BaseURL)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// checkDNS reports whether host points at this server: a CNAME to the
// platform host, or the same addresses. The explanation is for the page.
func (s *Server) checkDNS(ctx context.Context, host string) (bool, string) {
	ph := s.platformHost()
	if cname, err := s.resolver.LookupCNAME(ctx, host); err == nil {
		if strings.TrimSuffix(strings.ToLower(cname), ".") == ph {
			return true, fmt.Sprintf("%s is a CNAME for %s.", host, ph)
		}
	}
	got, err := s.resolver.LookupHost(ctx, host)
	if err != nil || len(got) == 0 {
		return false, fmt.Sprintf("%s doesn't resolve yet. Add a CNAME record pointing it at %s (DNS changes can take a while).", host, ph)
	}
	want, err := s.resolver.LookupHost(ctx, ph)
	if err != nil {
		return false, fmt.Sprintf("Couldn't look up this server's own address (%s): %v", ph, err)
	}
	for _, ip := range got {
		if slices.Contains(want, ip) {
			return true, fmt.Sprintf("%s resolves to %s, the same address as %s.", host, ip, ph)
		}
	}
	return false, fmt.Sprintf("%s points at %s, not this server (%s). Change it to a CNAME for %s.",
		host, strings.Join(got, ", "), strings.Join(want, ", "), ph)
}

func (s *Server) domainParam(w http.ResponseWriter, r *http.Request, sc store.Scope) (*store.Domain, bool) {
	id, _ := strconv.ParseInt(r.PathValue("domain"), 10, 64)
	d, err := s.store.GetDomain(r.Context(), sc, id)
	if err != nil {
		s.storeOrServerError(w, err)
		return nil, false
	}
	return d, true
}

func (s *Server) handleDomainAdd(w http.ResponseWriter, r *http.Request, a *auth) {
	sc, ok := s.superScope(w, r)
	if !ok {
		return
	}
	host, err := store.NormalizeHost(r.FormValue("host"))
	if err == nil && (host == s.platformHost() || strings.HasSuffix(s.platformHost(), "."+host)) {
		err = fmt.Errorf("%w: that's the platform's own domain", store.ErrInvalid)
	}
	var d *store.Domain
	if err == nil {
		d, err = s.store.AddDomain(r.Context(), sc, host)
	}
	if msg, ok := userError(err); ok {
		s.renderSuperClient(w, r, a, sc, msg, "", http.StatusBadRequest)
		return
	}
	if err != nil {
		s.serverError(w, err)
		return
	}
	s.audit(r, a, sc, "domain.add", "domain", d.Host, nil)
	redirectNotice(w, r, "/super/clients/"+sc.Client().Slug, "domain-added")
}

func (s *Server) handleDomainCheck(w http.ResponseWriter, r *http.Request, a *auth) {
	sc, ok := s.superScope(w, r)
	if !ok {
		return
	}
	d, ok := s.domainParam(w, r, sc)
	if !ok {
		return
	}
	pass, why := s.checkDNS(r.Context(), d.Host)
	if !pass {
		s.renderSuperClient(w, r, a, sc, "", why, http.StatusOK)
		return
	}
	if err := s.store.MarkDomainVerified(r.Context(), sc, d.ID, "dns"); err != nil {
		s.serverError(w, err)
		return
	}
	s.audit(r, a, sc, "domain.verify", "domain", d.Host, map[string]any{"by": "dns", "check": why})
	redirectNotice(w, r, "/super/clients/"+sc.Client().Slug, "domain-verified")
}

// handleDomainVerifyManual is for DNS setups the check can't see (e.g. a
// proxy in front); the administrator vouches for it, and that's recorded.
func (s *Server) handleDomainVerifyManual(w http.ResponseWriter, r *http.Request, a *auth) {
	sc, ok := s.superScope(w, r)
	if !ok {
		return
	}
	d, ok := s.domainParam(w, r, sc)
	if !ok {
		return
	}
	if r.FormValue("confirm") != "on" {
		s.renderSuperClient(w, r, a, sc, "Tick the box to confirm you've checked the DNS yourself.", "", http.StatusBadRequest)
		return
	}
	if err := s.store.MarkDomainVerified(r.Context(), sc, d.ID, "manual"); err != nil {
		s.serverError(w, err)
		return
	}
	s.audit(r, a, sc, "domain.verify", "domain", d.Host, map[string]any{"by": "manual"})
	redirectNotice(w, r, "/super/clients/"+sc.Client().Slug, "domain-verified")
}

func (s *Server) handleDomainCanonical(w http.ResponseWriter, r *http.Request, a *auth) {
	sc, ok := s.superScope(w, r)
	if !ok {
		return
	}
	var id int64
	host := s.platformHost()
	if r.PathValue("domain") != "platform" {
		d, ok := s.domainParam(w, r, sc)
		if !ok {
			return
		}
		id, host = d.ID, d.Host
	}
	err := s.store.SetCanonicalDomain(r.Context(), sc, id)
	if msg, ok := userError(err); ok {
		s.renderSuperClient(w, r, a, sc, msg, "", http.StatusConflict)
		return
	}
	if err != nil {
		s.serverError(w, err)
		return
	}
	s.audit(r, a, sc, "domain.canonical", "domain", host, nil)
	code := "domain-canonical"
	if id == 0 {
		code = "domain-platform"
	}
	redirectNotice(w, r, "/super/clients/"+sc.Client().Slug, code)
}

func (s *Server) handleDomainDelete(w http.ResponseWriter, r *http.Request, a *auth) {
	sc, ok := s.superScope(w, r)
	if !ok {
		return
	}
	d, ok := s.domainParam(w, r, sc)
	if !ok {
		return
	}
	err := s.store.DeleteDomain(r.Context(), sc, d.ID)
	if msg, ok := userError(err); ok {
		s.renderSuperClient(w, r, a, sc, msg, "", http.StatusConflict)
		return
	}
	if err != nil {
		s.serverError(w, err)
		return
	}
	s.audit(r, a, sc, "domain.delete", "domain", d.Host, nil)
	redirectNotice(w, r, "/super/clients/"+sc.Client().Slug, "domain-deleted")
}

// --- users ----------------------------------------------------------------------------

type superUsersPage struct {
	superBase
	Filter store.UserFilter
	Users  []store.UserRow
}

func (s *Server) handleSuperUsers(w http.ResponseWriter, r *http.Request, a *auth) {
	s.renderSuperUsers(w, r, a, "", http.StatusOK)
}

func (s *Server) renderSuperUsers(w http.ResponseWriter, r *http.Request, a *auth, errMsg string, status int) {
	q := r.URL.Query()
	p := superUsersPage{superBase: s.superBase(r, a, "users"), Filter: store.UserFilter{Query: q.Get("q"), Role: q.Get("role")}}
	p.Error = errMsg
	var err error
	if p.Users, err = s.store.ListUsers(r.Context(), p.Filter); err != nil {
		s.serverError(w, err)
		return
	}
	s.render(w, status, "s_users.html", p)
}

type superUserPage struct {
	superBase
	U        *store.User
	Clients  []store.Client
	Sessions int
	Self     bool
	Audit    []store.AuditEntry
}

func (s *Server) userParam(w http.ResponseWriter, r *http.Request) (*store.User, bool) {
	id, _ := strconv.ParseInt(r.PathValue("user"), 10, 64)
	u, err := s.store.GetUser(r.Context(), id)
	if err != nil {
		s.storeOrServerError(w, err)
		return nil, false
	}
	return u, true
}

func (s *Server) handleSuperUser(w http.ResponseWriter, r *http.Request, a *auth) {
	u, ok := s.userParam(w, r)
	if !ok {
		return
	}
	s.renderSuperUser(w, r, a, u, "", http.StatusOK)
}

func (s *Server) renderSuperUser(w http.ResponseWriter, r *http.Request, a *auth, u *store.User, errMsg string, status int) {
	p := superUserPage{superBase: s.superBase(r, a, "users"), U: u, Self: u.ID == a.user.ID}
	p.Error = errMsg
	var err error
	if p.Clients, err = s.store.ClientsForUser(r.Context(), u.ID); err == nil {
		if p.Sessions, err = s.store.ActiveSessions(r.Context(), u.ID); err == nil {
			p.Audit, _, err = s.store.SearchAudit(r.Context(), store.AuditFilter{Actor: u.Email, Limit: 25})
		}
	}
	if err != nil {
		s.serverError(w, err)
		return
	}
	s.render(w, status, "s_user.html", p)
}

// handleUserAction changes another user's account. A platform admin can't
// do these to themselves here (their own account page covers what's safe),
// and the last platform admin can't be disabled or demoted.
func (s *Server) handleUserAction(w http.ResponseWriter, r *http.Request, a *auth) {
	u, ok := s.userParam(w, r)
	if !ok {
		return
	}
	action := r.PathValue("action")
	fail := func(msg string, status int) { s.renderSuperUser(w, r, a, u, msg, status) }
	if u.ID == a.user.ID {
		fail("You can't do that to your own account here. Ask another platform administrator.", http.StatusConflict)
		return
	}
	ctx := r.Context()
	var err error
	var code, audit string
	switch action {
	case "disable":
		if last, lerr := s.store.LastEnabledSuper(ctx, u.ID); lerr == nil && last && u.IsSuper {
			fail("That's the last platform administrator; add another first.", http.StatusConflict)
			return
		}
		err, code, audit = s.store.SetUserDisabled(ctx, u.ID, true), "user-disabled", "user.disable"
	case "enable":
		err, code, audit = s.store.SetUserDisabled(ctx, u.ID, false), "user-enabled", "user.enable"
	case "reset-2fa":
		if err = s.store.ResetTOTP(ctx, u.ID); err == nil {
			_, err = s.store.DeleteUserSessions(ctx, u.ID, "")
		}
		code, audit = "user-2fa-reset", "user.reset_2fa"
	case "sign-out":
		_, err = s.store.DeleteUserSessions(ctx, u.ID, "")
		code, audit = "user-signed-out", "user.sign_out"
	case "grant-super":
		err, code, audit = s.store.SetSuperAdmin(ctx, u.ID, true), "superadmin-gained", "super.grant"
	case "revoke-super":
		err, code, audit = s.store.SetSuperAdmin(ctx, u.ID, false), "superadmin-gone", "super.revoke"
	default:
		s.message(w, http.StatusNotFound, "Not found", "There's nothing here.")
		return
	}
	if errors.Is(err, store.ErrConflict) {
		fail("That's the last platform administrator; add another first.", http.StatusConflict)
		return
	}
	if err != nil {
		s.serverError(w, err)
		return
	}
	s.audit(r, a, store.Scope{}, audit, "user", u.Email, nil)
	redirectNotice(w, r, "/super/users/"+strconv.FormatInt(u.ID, 10), code)
}

// handleSuperGrant adds a platform administrator by email (inviting them).
func (s *Server) handleSuperGrant(w http.ResponseWriter, r *http.Request, a *auth) {
	u, _, err := s.store.EnsureUser(r.Context(), r.FormValue("email"), r.FormValue("name"))
	if errors.Is(err, store.ErrInvalid) {
		s.renderSuperUsers(w, r, a, "Enter a valid email address.", http.StatusBadRequest)
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
	redirectNotice(w, r, "/super/users", code)
}

// handleSuperRevoke is the older "remove platform admin" form action.
func (s *Server) handleSuperRevoke(w http.ResponseWriter, r *http.Request, a *auth) {
	u, ok := s.userParam(w, r)
	if !ok {
		return
	}
	err := s.store.SetSuperAdmin(r.Context(), u.ID, false)
	if errors.Is(err, store.ErrConflict) {
		s.renderSuperUsers(w, r, a, "You can't remove the last platform administrator.", http.StatusConflict)
		return
	}
	if err != nil {
		s.serverError(w, err)
		return
	}
	s.audit(r, a, store.Scope{}, "super.revoke", "user", u.Email, nil)
	redirectNotice(w, r, "/super/users", "revoked")
}

// --- audit ------------------------------------------------------------------------------

type superAuditPage struct {
	superBase
	Filter  store.AuditFilter
	Clients []store.Client
	Entries []store.AuditEntry
	Older   string
}

func (s *Server) handleSuperAudit(w http.ResponseWriter, r *http.Request, a *auth) {
	q := r.URL.Query()
	before, _ := strconv.ParseInt(q.Get("before"), 10, 64)
	p := superAuditPage{superBase: s.superBase(r, a, "audit"),
		Filter: store.AuditFilter{Client: q.Get("client"), Actor: q.Get("actor"), Action: q.Get("action"), Before: before, Limit: 100}}
	var more bool
	var err error
	if p.Entries, more, err = s.store.SearchAudit(r.Context(), p.Filter); err == nil {
		p.Clients, err = s.store.ListClients(r.Context())
	}
	if err != nil {
		s.serverError(w, err)
		return
	}
	if more && len(p.Entries) > 0 {
		v := url.Values{}
		for k, val := range map[string]string{"client": p.Filter.Client, "actor": p.Filter.Actor, "action": p.Filter.Action} {
			if val != "" {
				v.Set(k, val)
			}
		}
		v.Set("before", strconv.FormatInt(p.Entries[len(p.Entries)-1].ID, 10))
		p.Older = "?" + v.Encode()
	}
	s.render(w, http.StatusOK, "s_audit.html", p)
}

// netResolver adapts net.Resolver to Resolver.
type netResolver struct{ r *net.Resolver }

func (n netResolver) LookupCNAME(ctx context.Context, host string) (string, error) {
	return n.r.LookupCNAME(ctx, host)
}
func (n netResolver) LookupHost(ctx context.Context, host string) ([]string, error) {
	return n.r.LookupHost(ctx, host)
}
