package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"time"
)

// Lifetimes of sign-in links and sessions.
const (
	LoginLinkTTL     = 15 * time.Minute
	InviteLinkTTL    = 7 * 24 * time.Hour
	BootstrapLinkTTL = time.Hour // links printed by the CLI
	SessionIdle      = 12 * time.Hour
	SessionMax       = 30 * 24 * time.Hour
)

func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// --- sign-in links ---------------------------------------------------------

// CreateLoginToken issues a single-use sign-in link token for a user.
// purpose is "login" or "invite".
func (s *Store) CreateLoginToken(ctx context.Context, userID int64, purpose string, ttl time.Duration) (string, error) {
	if purpose != "login" && purpose != "invite" {
		return "", fmt.Errorf("%w: purpose", ErrInvalid)
	}
	token := randomToken(32)
	_, err := s.wdb.ExecContext(ctx, `
		INSERT INTO login_tokens (token_hash, user_id, purpose, created_at, expires_at) VALUES (?, ?, ?, ?, ?)`,
		hashToken(token), userID, purpose, nowUTC(), fmtTime(clock().Add(ttl)))
	if err != nil {
		return "", err
	}
	return token, nil
}

// PeekLoginToken returns the user a valid, unused token belongs to without
// using it up. Sign-in links are consumed by a POST from a confirmation
// page, never by the GET, because mail scanners prefetch links.
func (s *Store) PeekLoginToken(ctx context.Context, token string) (*User, error) {
	u, err := scanUser(s.rdb.QueryRowContext(ctx, userSelect+`
		JOIN login_tokens t ON t.user_id = u.id
		WHERE t.token_hash = ? AND t.used_at IS NULL AND t.expires_at > ? AND u.disabled_at IS NULL`,
		hashToken(token), nowUTC()))
	return u, err
}

// ConsumeLoginToken uses up a sign-in token and returns its user. It fails
// with ErrNotFound if the token is unknown, used, expired, or the user is
// disabled.
func (s *Store) ConsumeLoginToken(ctx context.Context, token string) (*User, string, error) {
	tx, err := s.wdb.BeginTx(ctx, nil)
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback()
	var userID int64
	var purpose string
	now := nowUTC()
	err = tx.QueryRowContext(ctx, `
		UPDATE login_tokens SET used_at = ?
		WHERE token_hash = ? AND used_at IS NULL AND expires_at > ?
		RETURNING user_id, purpose`, now, hashToken(token), now).Scan(&userID, &purpose)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", ErrNotFound
	}
	if err != nil {
		return nil, "", err
	}
	u, err := scanUser(tx.QueryRowContext(ctx, userSelect+` WHERE u.id = ? AND u.disabled_at IS NULL`, userID))
	if err != nil {
		return nil, "", err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE users SET last_login_at = ? WHERE id = ?`, now, userID); err != nil {
		return nil, "", err
	}
	return u, purpose, tx.Commit()
}

// --- sessions --------------------------------------------------------------

// Session is a signed-in browser.
type Session struct {
	User         *User
	CSRF         string
	TOTPVerified bool
	CreatedAt    time.Time
	LastSeenAt   time.Time
	ExpiresAt    time.Time
}

// CreateSession starts a session and returns its secret id (for the cookie).
func (s *Store) CreateSession(ctx context.Context, userID int64, totpVerified bool, ip, userAgent string) (string, *Session, error) {
	id := randomToken(32)
	csrf := randomToken(24)
	now := clock()
	if len(userAgent) > 300 {
		userAgent = userAgent[:300]
	}
	_, err := s.wdb.ExecContext(ctx, `
		INSERT INTO sessions (id_hash, user_id, csrf_token, totp_verified, created_at, last_seen_at, expires_at, ip, user_agent)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		hashToken(id), userID, csrf, totpVerified, fmtTime(now), fmtTime(now), fmtTime(now.Add(SessionMax)), ip, userAgent)
	if err != nil {
		return "", nil, err
	}
	sess, err := s.GetSession(ctx, id)
	return id, sess, err
}

// GetSession returns a live session: not expired, not idle too long, and
// belonging to an enabled user.
func (s *Store) GetSession(ctx context.Context, id string) (*Session, error) {
	if id == "" {
		return nil, ErrNotFound
	}
	var sess Session
	var created, seen, expires string
	var totp int
	row := s.rdb.QueryRowContext(ctx, `
		SELECT u.id, u.email, u.name, u.is_super_admin, u.totp_enabled_at IS NOT NULL, u.disabled_at IS NOT NULL,
		       u.created_at, u.last_login_at,
		       s.csrf_token, s.totp_verified, s.created_at, s.last_seen_at, s.expires_at
		FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.id_hash = ? AND s.expires_at > ? AND s.last_seen_at > ? AND u.disabled_at IS NULL`,
		hashToken(id), nowUTC(), fmtTime(clock().Add(-SessionIdle)))
	var u User
	var uCreated string
	var lastLogin sql.NullString
	err := row.Scan(&u.ID, &u.Email, &u.Name, &u.IsSuper, &u.TOTPEnabled, &u.Disabled, &uCreated, &lastLogin,
		&sess.CSRF, &totp, &created, &seen, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	for dst, src := range map[*time.Time]string{&u.CreatedAt: uCreated, &sess.CreatedAt: created,
		&sess.LastSeenAt: seen, &sess.ExpiresAt: expires} {
		if t := parseTime(sql.NullString{String: src, Valid: true}); t != nil {
			*dst = *t
		}
	}
	u.LastLoginAt = parseTime(lastLogin)
	sess.User = &u
	sess.TOTPVerified = totp == 1
	return &sess, nil
}

// TouchSession records activity, keeping the session from going idle.
func (s *Store) TouchSession(ctx context.Context, id string) error {
	_, err := s.wdb.ExecContext(ctx, `UPDATE sessions SET last_seen_at = ? WHERE id_hash = ?`, nowUTC(), hashToken(id))
	return err
}

// MarkSessionTOTPVerified records that the session passed the second factor.
func (s *Store) MarkSessionTOTPVerified(ctx context.Context, id string) error {
	res, err := s.wdb.ExecContext(ctx, `UPDATE sessions SET totp_verified = 1 WHERE id_hash = ?`, hashToken(id))
	return affectedOne(res, err)
}

// DeleteSession signs a session out.
func (s *Store) DeleteSession(ctx context.Context, id string) error {
	_, err := s.wdb.ExecContext(ctx, `DELETE FROM sessions WHERE id_hash = ?`, hashToken(id))
	return err
}

// DeleteUserSessions signs a user out everywhere except the given session.
func (s *Store) DeleteUserSessions(ctx context.Context, userID int64, exceptID string) (int64, error) {
	res, err := s.wdb.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ? AND id_hash <> ?`,
		userID, hashToken(exceptID))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// PruneAuth deletes expired sessions and sign-in links.
func (s *Store) PruneAuth(ctx context.Context) error {
	now := nowUTC()
	if _, err := s.wdb.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ? OR last_seen_at <= ?`,
		now, fmtTime(clock().Add(-SessionIdle))); err != nil {
		return err
	}
	_, err := s.wdb.ExecContext(ctx, `DELETE FROM login_tokens WHERE expires_at <= ?`,
		fmtTime(clock().Add(-24*time.Hour)))
	return err
}

// --- TOTP --------------------------------------------------------------------

// TOTPSecret returns a user's encrypted TOTP secret (nil if not enrolled).
func (s *Store) TOTPSecret(ctx context.Context, userID int64) ([]byte, error) {
	var enc []byte
	err := s.rdb.QueryRowContext(ctx, `SELECT totp_secret_enc FROM users WHERE id = ? AND totp_enabled_at IS NOT NULL`,
		userID).Scan(&enc)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return enc, err
}

// EnableTOTP stores a confirmed (encrypted) TOTP secret. counter is the
// time step of the code used to confirm it, so that code can't be reused.
func (s *Store) EnableTOTP(ctx context.Context, userID int64, secretEnc []byte, counter int64) error {
	res, err := s.wdb.ExecContext(ctx, `UPDATE users
		SET totp_secret_enc = ?, totp_enabled_at = ?, totp_last_counter = ? WHERE id = ?`,
		secretEnc, nowUTC(), counter, userID)
	return affectedOne(res, err)
}

// UseTOTPCounter records that a code for time step counter was accepted.
// It fails with ErrConflict if that step (or a later one) was already used,
// which stops a code from being replayed.
func (s *Store) UseTOTPCounter(ctx context.Context, userID int64, counter int64) error {
	res, err := s.wdb.ExecContext(ctx, `UPDATE users SET totp_last_counter = ?
		WHERE id = ? AND totp_last_counter < ?`, counter, userID, counter)
	if err := affectedOne(res, err); errors.Is(err, ErrNotFound) {
		return fmt.Errorf("%w: code already used", ErrConflict)
	} else if err != nil {
		return err
	}
	return nil
}

// ResetTOTP removes a user's second factor (e.g. lost phone) and signs
// them out everywhere.
func (s *Store) ResetTOTP(ctx context.Context, userID int64) error {
	res, err := s.wdb.ExecContext(ctx, `UPDATE users
		SET totp_secret_enc = NULL, totp_enabled_at = NULL, totp_last_counter = 0 WHERE id = ?`, userID)
	if err := affectedOne(res, err); err != nil {
		return err
	}
	_, err = s.wdb.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID)
	return err
}
