package web

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"certsforever/internal/buildinfo"
	"certsforever/internal/store"
)

// --- /super/system: the email queue and suppression list ------------------

type systemPage struct {
	base
	Notice       string
	Error        string
	Version      string
	Schema       int
	EmailMode    string
	Counts       map[string]int
	Failed       []store.OutboxItem
	Recent       []store.OutboxItem
	Suppressions []store.Suppression
	WebhookOn    bool
}

func (s *Server) handleSystem(w http.ResponseWriter, r *http.Request, a *auth) {
	s.renderSystem(w, r, a, "", http.StatusOK)
}

func (s *Server) renderSystem(w http.ResponseWriter, r *http.Request, a *auth, errMsg string, status int) {
	ctx := r.Context()
	p := systemPage{base: s.consoleBase(a), Notice: notice(r), Error: errMsg, Version: buildinfo.Get().String(),
		EmailMode: s.emailMode, WebhookOn: s.cfg.BounceWebhookToken != ""}
	var err error
	if p.Schema, err = s.store.Health(ctx); err == nil {
		if p.Counts, err = s.store.OutboxCounts(ctx); err == nil {
			if p.Failed, err = s.store.ListEmails(ctx, "failed", 50); err == nil {
				if p.Recent, err = s.store.ListEmails(ctx, "", 25); err == nil {
					p.Suppressions, err = s.store.ListSuppressions(ctx, 100)
				}
			}
		}
	}
	if err != nil {
		s.serverError(w, err)
		return
	}
	s.render(w, status, "system.html", p)
}

func (s *Server) handleRetryEmail(w http.ResponseWriter, r *http.Request, a *auth) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err == nil {
		err = s.store.RetryFailedEmail(r.Context(), id)
	}
	if errors.Is(err, store.ErrConflict) {
		s.renderSystem(w, r, a, strings.TrimPrefix(err.Error(), "conflict: "), http.StatusConflict)
		return
	}
	if err != nil {
		s.renderSystem(w, r, a, "That message can't be retried.", http.StatusBadRequest)
		return
	}
	s.audit(r, a, store.Scope{}, "email.retry", "email", r.PathValue("id"), nil)
	if s.queue != nil {
		s.queue.Wake()
	}
	redirectNotice(w, r, "/super/system", "retried")
}

func (s *Server) handleSuppress(w http.ResponseWriter, r *http.Request, a *auth) {
	email := strings.TrimSpace(r.FormValue("email"))
	if err := s.store.Suppress(r.Context(), email, strings.TrimSpace(r.FormValue("reason")), "manual"); err != nil {
		s.renderSystem(w, r, a, "Enter a valid email address.", http.StatusBadRequest)
		return
	}
	s.audit(r, a, store.Scope{}, "email.suppressed", "email", email, map[string]any{"source": "manual"})
	redirectNotice(w, r, "/super/system", "suppressed")
}

func (s *Server) handleUnsuppress(w http.ResponseWriter, r *http.Request, a *auth) {
	email := strings.TrimSpace(r.FormValue("email"))
	if err := s.store.Unsuppress(r.Context(), email); err != nil {
		s.renderSystem(w, r, a, "That address isn't suppressed.", http.StatusNotFound)
		return
	}
	s.audit(r, a, store.Scope{}, "email.unsuppressed", "email", email, nil)
	redirectNotice(w, r, "/super/system", "unsuppressed")
}

// --- bounce / complaint webhook ----------------------------------------------

// The webhook accepts a generic payload, {"email": "...", "type":
// "bounce"|"complaint", "reason": "..."}, and Postmark's bounce and
// spam-complaint webhooks, {"RecordType": "Bounce", "Type": "HardBounce",
// "Email": "...", "Description": "..."}. Only permanent failures suppress.

// Postmark bounce types that mean "stop sending to this address".
var permanentPostmark = map[string]bool{"HardBounce": true, "BadEmailAddress": true, "SpamComplaint": true,
	"ManuallyDeactivated": true, "Unsubscribe": true}

func (s *Server) webhookAuthorized(r *http.Request) bool {
	want := []byte(s.cfg.BounceWebhookToken)
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if _, pass, ok := r.BasicAuth(); ok {
		got = pass
	} else if t := r.URL.Query().Get("token"); t != "" {
		got = t
	}
	return subtle.ConstantTimeCompare([]byte(got), want) == 1
}

func (s *Server) handleBounceWebhook(w http.ResponseWriter, r *http.Request) {
	if s.cfg.BounceWebhookToken == "" {
		http.NotFound(w, r)
		return
	}
	if !s.webhookAuthorized(r) {
		if !s.limits.adminToken.Allow(s.clientIP(r)) {
			writeJSONError(w, http.StatusTooManyRequests, "too many failed attempts")
			return
		}
		writeJSONError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var raw map[string]any
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&raw); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	str := func(k string) string { v, _ := raw[k].(string); return strings.TrimSpace(v) }

	var email, reason string
	var permanent bool
	switch {
	case str("RecordType") != "": // Postmark
		email = str("Email")
		kind := str("Type")
		if str("RecordType") == "SpamComplaint" {
			kind = "SpamComplaint"
		}
		permanent = permanentPostmark[kind]
		reason = strings.TrimSpace(kind + ": " + str("Description"))
	default:
		email = str("email")
		kind := strings.ToLower(str("type"))
		permanent = kind == "bounce" || kind == "complaint"
		reason = strings.TrimSpace(kind + ": " + str("reason"))
	}
	if email == "" {
		writeJSONError(w, http.StatusBadRequest, "no email address in payload")
		return
	}
	if !permanent {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored", "reason": "not a permanent failure"})
		return
	}
	if err := s.store.Suppress(r.Context(), email, reason, "webhook"); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid email address")
		return
	}
	s.auditAs(r, "bounce-webhook", store.Scope{}, "email.suppressed", "email", email, map[string]any{"reason": reason})
	writeJSON(w, http.StatusOK, map[string]string{"status": "suppressed"})
}
