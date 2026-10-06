package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// APIToken lets automation (an LMS, a script) act for one client.
type APIToken struct {
	ID         int64      `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

// APITokenPrefix starts every client API token, so they're recognizable
// (e.g. by secret scanners).
const APITokenPrefix = "cfk_"

// CreateAPIToken makes a token for the scoped client. The token is
// returned once; only its hash is stored.
func (s *Store) CreateAPIToken(ctx context.Context, sc Scope, name string, createdBy *int64) (string, *APIToken, error) {
	clientID, err := sc.id()
	if err != nil {
		return "", nil, err
	}
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 60 {
		return "", nil, fmt.Errorf("%w: token name is required (up to 60 characters)", ErrInvalid)
	}
	token := APITokenPrefix + randomToken(32)
	var id int64
	if err := s.wdb.QueryRowContext(ctx, `INSERT INTO api_tokens (client_id, name, token_hash, display_prefix, created_by, created_at)
		VALUES (?, ?, ?, ?, ?, ?) RETURNING id`,
		clientID, name, hashToken(token), token[:len(APITokenPrefix)+6], createdBy, nowUTC()).Scan(&id); err != nil {
		return "", nil, err
	}
	t, err := s.getAPIToken(ctx, `t.id = ?`, id)
	return token, t, err
}

const apiTokenSelect = `SELECT t.id, t.name, t.display_prefix, t.created_at, t.last_used_at, t.revoked_at FROM api_tokens t`

func scanAPIToken(r rowScanner) (*APIToken, error) {
	var t APIToken
	var created string
	var used, revoked sql.NullString
	err := r.Scan(&t.ID, &t.Name, &t.Prefix, &created, &used, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if p := parseTime(sql.NullString{String: created, Valid: true}); p != nil {
		t.CreatedAt = *p
	}
	t.LastUsedAt, t.RevokedAt = parseTime(used), parseTime(revoked)
	return &t, nil
}

func (s *Store) getAPIToken(ctx context.Context, where string, args ...any) (*APIToken, error) {
	return scanAPIToken(s.wdb.QueryRowContext(ctx, apiTokenSelect+` WHERE `+where, args...))
}

// ListAPITokens returns the scoped client's tokens, newest first.
func (s *Store) ListAPITokens(ctx context.Context, sc Scope) ([]APIToken, error) {
	clientID, err := sc.id()
	if err != nil {
		return nil, err
	}
	rows, err := s.rdb.QueryContext(ctx, apiTokenSelect+` WHERE t.client_id = ? ORDER BY t.id DESC`, clientID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []APIToken{}
	for rows.Next() {
		t, err := scanAPIToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *t)
	}
	return out, rows.Err()
}

// RevokeAPIToken stops a token working.
func (s *Store) RevokeAPIToken(ctx context.Context, sc Scope, id int64) (*APIToken, error) {
	clientID, err := sc.id()
	if err != nil {
		return nil, err
	}
	res, err := s.wdb.ExecContext(ctx, `UPDATE api_tokens SET revoked_at = ? WHERE client_id = ? AND id = ? AND revoked_at IS NULL`,
		nowUTC(), clientID, id)
	if err := affectedOne(res, err); err != nil {
		return nil, err
	}
	return s.getAPIToken(ctx, `t.id = ?`, id)
}

// ScopeForAPIToken authenticates a client API token and returns its client.
func (s *Store) ScopeForAPIToken(ctx context.Context, token string) (Scope, *APIToken, error) {
	if !strings.HasPrefix(token, APITokenPrefix) {
		return Scope{}, nil, ErrNotFound
	}
	var clientID int64
	var lastUsed sql.NullString
	var id int64
	err := s.rdb.QueryRowContext(ctx, `SELECT id, client_id, last_used_at FROM api_tokens
		WHERE token_hash = ? AND revoked_at IS NULL`, hashToken(token)).Scan(&id, &clientID, &lastUsed)
	if errors.Is(err, sql.ErrNoRows) {
		return Scope{}, nil, ErrNotFound
	}
	if err != nil {
		return Scope{}, nil, err
	}
	// Record use, at most once a minute.
	if t := parseTime(lastUsed); t == nil || clock().Sub(*t) > time.Minute {
		s.wdb.ExecContext(ctx, `UPDATE api_tokens SET last_used_at = ? WHERE id = ?`, nowUTC(), id)
	}
	c, err := s.getClient(ctx, `id = ?`, clientID)
	if err != nil {
		return Scope{}, nil, err
	}
	t, err := s.getAPIToken(ctx, `t.id = ?`, id)
	return Scope{client: c}, t, err
}
