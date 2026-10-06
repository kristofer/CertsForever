package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

// AuditEntry is one recorded admin action.
type AuditEntry struct {
	ID           int64          `json:"id"`
	At           time.Time      `json:"at"`
	ActorUserID  *int64         `json:"actor_user_id,omitempty"`
	Actor        string         `json:"actor"` // email, "cli", "admin-token"
	ClientSlug   string         `json:"client,omitempty"`
	Impersonated bool           `json:"impersonated,omitempty"`
	Action       string         `json:"action"`
	TargetType   string         `json:"target_type,omitempty"`
	TargetID     string         `json:"target_id,omitempty"`
	Details      map[string]any `json:"details,omitempty"`
	IP           string         `json:"ip,omitempty"`
}

// Audit appends an entry. Pass the zero Scope for platform-level actions
// (creating clients, granting super admin, signing in).
func (s *Store) Audit(ctx context.Context, sc Scope, e AuditEntry) error {
	var clientID any
	if id, err := sc.id(); err == nil {
		clientID = id
	}
	details := "{}"
	if len(e.Details) > 0 {
		b, err := json.Marshal(e.Details)
		if err != nil {
			return err
		}
		details = string(b)
	}
	_, err := s.wdb.ExecContext(ctx, `
		INSERT INTO audit_log (at, actor_user_id, actor_label, client_id, impersonated, action, target_type, target_id, details, ip)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		nowUTC(), e.ActorUserID, e.Actor, clientID, e.Impersonated, e.Action, e.TargetType, e.TargetID, details, e.IP)
	return err
}

const auditSelect = `SELECT a.id, a.at, a.actor_user_id, a.actor_label, COALESCE(c.slug, ''), a.impersonated,
	a.action, a.target_type, a.target_id, a.details, a.ip
	FROM audit_log a LEFT JOIN clients c ON c.id = a.client_id`

// ListAudit returns the scoped client's most recent audit entries.
func (s *Store) ListAudit(ctx context.Context, sc Scope, limit int) ([]AuditEntry, error) {
	clientID, err := sc.id()
	if err != nil {
		return nil, err
	}
	return s.listAudit(ctx, auditSelect+` WHERE a.client_id = ? ORDER BY a.id DESC LIMIT ?`, clientID, limit)
}

// ListAllAudit returns the most recent audit entries across the platform.
func (s *Store) ListAllAudit(ctx context.Context, limit int) ([]AuditEntry, error) {
	return s.listAudit(ctx, auditSelect+` ORDER BY a.id DESC LIMIT ?`, limit)
}

func (s *Store) listAudit(ctx context.Context, q string, args ...any) ([]AuditEntry, error) {
	rows, err := s.rdb.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuditEntry{}
	for rows.Next() {
		var e AuditEntry
		var at, details string
		var actor sql.NullInt64
		if err := rows.Scan(&e.ID, &at, &actor, &e.Actor, &e.ClientSlug, &e.Impersonated, &e.Action,
			&e.TargetType, &e.TargetID, &details, &e.IP); err != nil {
			return nil, err
		}
		if t := parseTime(sql.NullString{String: at, Valid: true}); t != nil {
			e.At = *t
		}
		if actor.Valid {
			e.ActorUserID = &actor.Int64
		}
		_ = json.Unmarshal([]byte(details), &e.Details)
		out = append(out, e)
	}
	return out, rows.Err()
}
