package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// User is a person who can sign in: a super admin, a client admin, or both.
type User struct {
	ID          int64      `json:"id"`
	Email       string     `json:"email"`
	Name        string     `json:"name"`
	IsSuper     bool       `json:"is_super_admin"`
	TOTPEnabled bool       `json:"totp_enabled"`
	Disabled    bool       `json:"disabled"`
	CreatedAt   time.Time  `json:"created_at"`
	LastLoginAt *time.Time `json:"last_login_at,omitempty"`
}

// DisplayName is the name if set, otherwise the email.
func (u *User) DisplayName() string {
	if u.Name != "" {
		return u.Name
	}
	return u.Email
}

const userSelect = `SELECT u.id, u.email, u.name, u.is_super_admin, u.totp_enabled_at IS NOT NULL,
	u.disabled_at IS NOT NULL, u.created_at, u.last_login_at FROM users u`

func scanUser(r rowScanner) (*User, error) {
	var u User
	var created string
	var lastLogin sql.NullString
	err := r.Scan(&u.ID, &u.Email, &u.Name, &u.IsSuper, &u.TOTPEnabled, &u.Disabled, &created, &lastLogin)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if t := parseTime(sql.NullString{String: created, Valid: true}); t != nil {
		u.CreatedAt = *t
	}
	u.LastLoginAt = parseTime(lastLogin)
	return &u, nil
}

// NormalizeEmail trims and validates an email address. It is deliberately
// loose (the real check is whether the sign-in link arrives).
func NormalizeEmail(email string) (string, error) {
	e := strings.TrimSpace(email)
	at := strings.LastIndexByte(e, '@')
	if len(e) > 254 || at < 1 || at == len(e)-1 || strings.ContainsAny(e, " \t\r\n<>,;\"") ||
		!strings.Contains(e[at:], ".") {
		return "", fmt.Errorf("%w: %q is not an email address", ErrInvalid, email)
	}
	return e, nil
}

// EnsureUser returns the user with this email, creating them if needed.
// An existing user's name is only filled in, never overwritten.
func (s *Store) EnsureUser(ctx context.Context, email, name string) (u *User, created bool, err error) {
	email, err = NormalizeEmail(email)
	if err != nil {
		return nil, false, err
	}
	name = strings.TrimSpace(name)
	tx, err := s.wdb.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	var id int64
	err = tx.QueryRowContext(ctx, `SELECT id FROM users WHERE email = ?`, email).Scan(&id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if err := tx.QueryRowContext(ctx, `INSERT INTO users (email, name) VALUES (?, ?) RETURNING id`,
			email, name).Scan(&id); err != nil {
			return nil, false, err
		}
		created = true
	case err != nil:
		return nil, false, err
	case name != "":
		if _, err := tx.ExecContext(ctx, `UPDATE users SET name = ? WHERE id = ? AND name = ''`, name, id); err != nil {
			return nil, false, err
		}
	}
	u, err = scanUser(tx.QueryRowContext(ctx, userSelect+` WHERE u.id = ?`, id))
	if err != nil {
		return nil, false, err
	}
	return u, created, tx.Commit()
}

// GetUser looks up a user by id.
func (s *Store) GetUser(ctx context.Context, id int64) (*User, error) {
	return scanUser(s.rdb.QueryRowContext(ctx, userSelect+` WHERE u.id = ?`, id))
}

// GetUserByEmail looks up a user by email (case-insensitive).
func (s *Store) GetUserByEmail(ctx context.Context, email string) (*User, error) {
	return scanUser(s.rdb.QueryRowContext(ctx, userSelect+` WHERE u.email = ?`, strings.TrimSpace(email)))
}

// SetSuperAdmin grants or revokes super admin. The last active super admin
// can't be revoked, so the platform always has someone who can run it.
func (s *Store) SetSuperAdmin(ctx context.Context, userID int64, on bool) error {
	tx, err := s.wdb.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if !on {
		var others int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users
			WHERE is_super_admin = 1 AND disabled_at IS NULL AND id <> ?`, userID).Scan(&others); err != nil {
			return err
		}
		if others == 0 {
			return fmt.Errorf("%w: can't remove the last super admin", ErrConflict)
		}
	}
	res, err := tx.ExecContext(ctx, `UPDATE users SET is_super_admin = ? WHERE id = ?`, on, userID)
	if err := affectedOne(res, err); err != nil {
		return err
	}
	return tx.Commit()
}

// ListSuperAdmins returns all super admins.
func (s *Store) ListSuperAdmins(ctx context.Context) ([]User, error) {
	return s.listUsers(ctx, userSelect+` WHERE u.is_super_admin = 1 ORDER BY u.email`)
}

// SetUserDisabled disables (or re-enables) a user. Disabling ends their
// sessions and voids their unused sign-in links.
func (s *Store) SetUserDisabled(ctx context.Context, userID int64, disabled bool) error {
	tx, err := s.wdb.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var at any
	if disabled {
		at = nowUTC()
	}
	res, err := tx.ExecContext(ctx, `UPDATE users SET disabled_at = ? WHERE id = ?`, at, userID)
	if err := affectedOne(res, err); err != nil {
		return err
	}
	if disabled {
		if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE login_tokens SET used_at = ? WHERE user_id = ? AND used_at IS NULL`,
			nowUTC(), userID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) listUsers(ctx context.Context, q string, args ...any) ([]User, error) {
	rows, err := s.rdb.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

// --- memberships (client-scoped) -------------------------------------------

// Member is a client admin.
type Member struct {
	User
	Role    string    `json:"role"`
	AddedAt time.Time `json:"added_at"`
}

// AddMember makes a user an admin of the scoped client. Adding an existing
// member is a no-op.
func (s *Store) AddMember(ctx context.Context, sc Scope, userID int64, addedBy *int64) error {
	clientID, err := sc.id()
	if err != nil {
		return err
	}
	_, err = s.wdb.ExecContext(ctx, `
		INSERT INTO memberships (user_id, client_id, role, created_by) VALUES (?, ?, 'admin', ?)
		ON CONFLICT(user_id, client_id) DO NOTHING`, userID, clientID, addedBy)
	return err
}

// RemoveMember removes a user's admin access to the scoped client and ends
// none of their other access.
func (s *Store) RemoveMember(ctx context.Context, sc Scope, userID int64) error {
	clientID, err := sc.id()
	if err != nil {
		return err
	}
	res, err := s.wdb.ExecContext(ctx, `DELETE FROM memberships WHERE client_id = ? AND user_id = ?`, clientID, userID)
	return affectedOne(res, err)
}

// ListMembers returns the scoped client's admins.
func (s *Store) ListMembers(ctx context.Context, sc Scope) ([]Member, error) {
	clientID, err := sc.id()
	if err != nil {
		return nil, err
	}
	rows, err := s.rdb.QueryContext(ctx, `SELECT u.id, u.email, u.name, u.is_super_admin,
		u.totp_enabled_at IS NOT NULL, u.disabled_at IS NOT NULL, u.created_at, u.last_login_at,
		m.role, m.created_at
		FROM memberships m JOIN users u ON u.id = m.user_id
		WHERE m.client_id = ? ORDER BY u.email`, clientID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Member{}
	for rows.Next() {
		var m Member
		var created, added string
		var lastLogin sql.NullString
		if err := rows.Scan(&m.ID, &m.Email, &m.Name, &m.IsSuper, &m.TOTPEnabled, &m.Disabled, &created,
			&lastLogin, &m.Role, &added); err != nil {
			return nil, err
		}
		if t := parseTime(sql.NullString{String: created, Valid: true}); t != nil {
			m.CreatedAt = *t
		}
		if t := parseTime(sql.NullString{String: added, Valid: true}); t != nil {
			m.AddedAt = *t
		}
		m.LastLoginAt = parseTime(lastLogin)
		out = append(out, m)
	}
	return out, rows.Err()
}

// IsMember reports whether the user administers the scoped client.
func (s *Store) IsMember(ctx context.Context, sc Scope, userID int64) (bool, error) {
	clientID, err := sc.id()
	if err != nil {
		return false, err
	}
	var n int
	err = s.rdb.QueryRowContext(ctx, `SELECT COUNT(*) FROM memberships WHERE client_id = ? AND user_id = ?`,
		clientID, userID).Scan(&n)
	return n > 0, err
}

// ClientsForUser returns the clients a user administers.
func (s *Store) ClientsForUser(ctx context.Context, userID int64) ([]Client, error) {
	rows, err := s.rdb.QueryContext(ctx, clientSelect+`
		WHERE id IN (SELECT client_id FROM memberships WHERE user_id = ?) ORDER BY slug`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Client{}
	for rows.Next() {
		c, err := scanClient(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}
