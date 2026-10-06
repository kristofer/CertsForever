package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// The email outbox: every message is a row; a worker claims due rows,
// delivers them, and records the outcome. Delivery is at-least-once (a crash
// after the SMTP server accepted a message but before it was marked sent
// means it's sent again).

// Outbox tuning.
const (
	OutboxMaxAttempts  = 8
	OutboxLease        = 2 * time.Minute     // a claimed row is retried if not finished by then
	OutboxKeepFailed   = 7 * 24 * time.Hour  // failed bodies kept this long (for retry), then wiped
	OutboxKeepFinished = 90 * 24 * time.Hour // finished rows (metadata only) kept this long
)

// NewEmail is a message to queue. BodyEnc is already sealed by the caller.
type NewEmail struct {
	ClientID *int64 // set from the Scope by Enqueue; nil for platform mail
	To       string
	Template string
	Subject  string
	BodyEnc  []byte
	RefType  string
	RefID    string
}

// OutboxItem is a queued message.
type OutboxItem struct {
	ID         int64      `json:"id"`
	ClientSlug string     `json:"client,omitempty"`
	To         string     `json:"to"`
	Template   string     `json:"template"`
	Subject    string     `json:"subject"`
	BodyEnc    []byte     `json:"-"`
	HasBody    bool       `json:"has_body"`
	Suppressed bool       `json:"address_suppressed"` // the address is on the suppression list now
	Status     string     `json:"status"`
	Attempts   int        `json:"attempts"`
	NextAt     time.Time  `json:"next_attempt_at"`
	LastError  string     `json:"last_error,omitempty"`
	RefType    string     `json:"ref_type,omitempty"`
	RefID      string     `json:"ref_id,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	SentAt     *time.Time `json:"sent_at,omitempty"`
}

// Enqueue queues a message. Mail to a suppressed address is recorded with
// status "suppressed" and never sent. Pass the zero Scope for platform mail.
func (s *Store) Enqueue(ctx context.Context, sc Scope, e NewEmail) (int64, string, error) {
	if id, err := sc.id(); err == nil {
		e.ClientID = &id
	}
	tx, err := s.wdb.BeginTx(ctx, nil)
	if err != nil {
		return 0, "", err
	}
	defer tx.Rollback()
	id, err := enqueue(ctx, tx, e)
	if err != nil {
		return 0, "", err
	}
	var status string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM email_outbox WHERE id = ?`, id).Scan(&status); err != nil {
		return 0, "", err
	}
	return id, status, tx.Commit()
}

func enqueue(ctx context.Context, tx *sql.Tx, e NewEmail) (int64, error) {
	to, err := NormalizeEmail(e.To)
	if err != nil {
		return 0, err
	}
	if e.Template == "" || len(e.BodyEnc) == 0 {
		return 0, fmt.Errorf("%w: email needs a template and a body", ErrInvalid)
	}
	now := nowUTC()
	var id int64
	err = tx.QueryRowContext(ctx, `
		INSERT INTO email_outbox (client_id, to_addr, template, subject, body_enc, status, next_attempt_at,
		                          ref_type, ref_id, created_at, finished_at, last_error)
		SELECT ?, ?, ?, ?,
		       CASE WHEN sup.email IS NULL THEN ? END,
		       CASE WHEN sup.email IS NULL THEN 'queued' ELSE 'suppressed' END,
		       ?, ?, ?, ?,
		       CASE WHEN sup.email IS NULL THEN NULL ELSE ? END,
		       COALESCE('address suppressed: ' || sup.reason, '')
		FROM (SELECT 1) LEFT JOIN email_suppressions sup ON sup.email = ?
		RETURNING id`,
		e.ClientID, to, e.Template, e.Subject, e.BodyEnc, now, e.RefType, e.RefID, now, now, to).Scan(&id)
	return id, err
}

const outboxSelect = `SELECT o.id, COALESCE(c.slug, ''), o.to_addr, o.template, o.subject, o.body_enc,
	o.status, o.attempts, o.next_attempt_at, o.last_error, o.ref_type, o.ref_id, o.created_at, o.sent_at,
	EXISTS (SELECT 1 FROM email_suppressions sp WHERE sp.email = o.to_addr COLLATE NOCASE)
	FROM email_outbox o LEFT JOIN clients c ON c.id = o.client_id`

func scanOutbox(r rowScanner) (*OutboxItem, error) {
	var it OutboxItem
	var next, created string
	var sent sql.NullString
	err := r.Scan(&it.ID, &it.ClientSlug, &it.To, &it.Template, &it.Subject, &it.BodyEnc, &it.Status,
		&it.Attempts, &next, &it.LastError, &it.RefType, &it.RefID, &created, &sent, &it.Suppressed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	it.HasBody = len(it.BodyEnc) > 0
	if t := parseTime(sql.NullString{String: next, Valid: true}); t != nil {
		it.NextAt = *t
	}
	if t := parseTime(sql.NullString{String: created, Valid: true}); t != nil {
		it.CreatedAt = *t
	}
	it.SentAt = parseTime(sent)
	return &it, nil
}

// ClaimNextEmail takes the oldest due message for sending (or one whose
// previous sender's lease expired, e.g. after a crash) and leases it.
func (s *Store) ClaimNextEmail(ctx context.Context) (*OutboxItem, error) {
	now := nowUTC()
	var id int64
	err := s.wdb.QueryRowContext(ctx, `
		UPDATE email_outbox
		SET status = 'sending', attempts = attempts + 1, locked_until = ?
		WHERE id = (
			SELECT id FROM email_outbox
			WHERE (status = 'queued' AND next_attempt_at <= ?)
			   OR (status = 'sending' AND locked_until <= ?)
			ORDER BY next_attempt_at, id LIMIT 1)
		RETURNING id`, fmtTime(clock().Add(OutboxLease)), now, now).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return scanOutbox(s.wdb.QueryRowContext(ctx, outboxSelect+` WHERE o.id = ?`, id))
}

// MarkEmailSent records delivery and wipes the body (it contained links).
func (s *Store) MarkEmailSent(ctx context.Context, id int64) error {
	now := nowUTC()
	res, err := s.wdb.ExecContext(ctx, `UPDATE email_outbox
		SET status = 'sent', sent_at = ?, finished_at = ?, body_enc = NULL, locked_until = NULL, last_error = ''
		WHERE id = ?`, now, now, id)
	return affectedOne(res, err)
}

// RetryEmailAfter schedules another attempt, or fails the message for good
// once it has used OutboxMaxAttempts.
func (s *Store) RetryEmailAfter(ctx context.Context, id int64, delay time.Duration, lastErr string) (failed bool, err error) {
	var attempts int
	if err := s.wdb.QueryRowContext(ctx, `SELECT attempts FROM email_outbox WHERE id = ?`, id).Scan(&attempts); err != nil {
		return false, err
	}
	if attempts >= OutboxMaxAttempts {
		return true, s.FailEmail(ctx, id, lastErr)
	}
	res, err := s.wdb.ExecContext(ctx, `UPDATE email_outbox
		SET status = 'queued', next_attempt_at = ?, locked_until = NULL, last_error = ? WHERE id = ?`,
		fmtTime(clock().Add(delay)), truncate(lastErr, 500), id)
	return false, affectedOne(res, err)
}

// FailEmail gives up on a message. Its body is kept for OutboxKeepFailed so
// an operator can retry, then wiped by PruneOutbox.
func (s *Store) FailEmail(ctx context.Context, id int64, lastErr string) error {
	res, err := s.wdb.ExecContext(ctx, `UPDATE email_outbox
		SET status = 'failed', finished_at = ?, locked_until = NULL, last_error = ? WHERE id = ?`,
		nowUTC(), truncate(lastErr, 500), id)
	return affectedOne(res, err)
}

// Retryable reports whether RetryFailedEmail would accept the message.
func (it *OutboxItem) Retryable() bool { return it.Status == "failed" && it.HasBody && !it.Suppressed }

// RetryFailedEmail puts a failed message back in the queue, if its body
// hasn't been wiped yet and its address isn't suppressed.
func (s *Store) RetryFailedEmail(ctx context.Context, id int64) error {
	res, err := s.wdb.ExecContext(ctx, `UPDATE email_outbox
		SET status = 'queued', attempts = 0, next_attempt_at = ?, finished_at = NULL, last_error = ''
		WHERE id = ? AND status = 'failed' AND body_enc IS NOT NULL
		  AND NOT EXISTS (SELECT 1 FROM email_suppressions sp WHERE sp.email = email_outbox.to_addr COLLATE NOCASE)`,
		nowUTC(), id)
	if err := affectedOne(res, err); errors.Is(err, ErrNotFound) {
		return fmt.Errorf("%w: only failed messages that still have a body (within %d days) and an unsuppressed address can be retried",
			ErrConflict, int(OutboxKeepFailed.Hours()/24))
	} else if err != nil {
		return err
	}
	return nil
}

// GetEmail returns one outbox row.
func (s *Store) GetEmail(ctx context.Context, id int64) (*OutboxItem, error) {
	return scanOutbox(s.rdb.QueryRowContext(ctx, outboxSelect+` WHERE o.id = ?`, id))
}

// ListEmails returns recent outbox rows, newest first, optionally by status.
func (s *Store) ListEmails(ctx context.Context, status string, limit int) ([]OutboxItem, error) {
	rows, err := s.rdb.QueryContext(ctx, outboxSelect+` WHERE (? = '' OR o.status = ?) ORDER BY o.id DESC LIMIT ?`,
		status, status, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []OutboxItem{}
	for rows.Next() {
		it, err := scanOutbox(rows)
		if err != nil {
			return nil, err
		}
		it.BodyEnc = nil
		out = append(out, *it)
	}
	return out, rows.Err()
}

// OutboxCounts returns how many messages are in each status.
func (s *Store) OutboxCounts(ctx context.Context) (map[string]int, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT status, COUNT(*) FROM email_outbox GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{"queued": 0, "sending": 0, "sent": 0, "failed": 0, "suppressed": 0}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		out[st] = n
	}
	return out, rows.Err()
}

// PruneOutbox wipes failed bodies after OutboxKeepFailed and deletes
// finished rows after OutboxKeepFinished.
func (s *Store) PruneOutbox(ctx context.Context) error {
	if _, err := s.wdb.ExecContext(ctx, `UPDATE email_outbox SET body_enc = NULL
		WHERE status IN ('failed', 'suppressed') AND body_enc IS NOT NULL AND finished_at <= ?`,
		fmtTime(clock().Add(-OutboxKeepFailed))); err != nil {
		return err
	}
	_, err := s.wdb.ExecContext(ctx, `DELETE FROM email_outbox
		WHERE status IN ('sent', 'failed', 'suppressed') AND finished_at <= ?`,
		fmtTime(clock().Add(-OutboxKeepFinished)))
	return err
}

// --- suppressions -------------------------------------------------------------

// Suppression is an address we don't send to.
type Suppression struct {
	Email     string    `json:"email"`
	Reason    string    `json:"reason"`
	Source    string    `json:"source"`
	CreatedAt time.Time `json:"created_at"`
}

// Suppress stops all mail to an address and cancels anything queued for it.
func (s *Store) Suppress(ctx context.Context, email, reason, source string) error {
	email, err := NormalizeEmail(email)
	if err != nil {
		return err
	}
	tx, err := s.wdb.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO email_suppressions (email, reason, source, created_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(email) DO UPDATE SET reason = excluded.reason, source = excluded.source`,
		email, truncate(reason, 300), source, nowUTC()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE email_outbox
		SET status = 'suppressed', body_enc = NULL, finished_at = ?, locked_until = NULL,
		    last_error = 'address suppressed: ' || ?
		WHERE to_addr = ? COLLATE NOCASE AND status = 'queued'`, nowUTC(), truncate(reason, 300), email); err != nil {
		return err
	}
	return tx.Commit()
}

// Unsuppress allows mail to an address again.
func (s *Store) Unsuppress(ctx context.Context, email string) error {
	res, err := s.wdb.ExecContext(ctx, `DELETE FROM email_suppressions WHERE email = ?`, strings.TrimSpace(email))
	return affectedOne(res, err)
}

// IsSuppressed reports whether an address is suppressed.
func (s *Store) IsSuppressed(ctx context.Context, email string) (bool, error) {
	var n int
	err := s.rdb.QueryRowContext(ctx, `SELECT COUNT(*) FROM email_suppressions WHERE email = ?`,
		strings.TrimSpace(email)).Scan(&n)
	return n > 0, err
}

// ListSuppressions returns suppressed addresses, newest first.
func (s *Store) ListSuppressions(ctx context.Context, limit int) ([]Suppression, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT email, reason, source, created_at FROM email_suppressions
		ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Suppression{}
	for rows.Next() {
		var sp Suppression
		var created string
		if err := rows.Scan(&sp.Email, &sp.Reason, &sp.Source, &created); err != nil {
			return nil, err
		}
		if t := parseTime(sql.NullString{String: created, Valid: true}); t != nil {
			sp.CreatedAt = *t
		}
		out = append(out, sp)
	}
	return out, rows.Err()
}

// --- publish reminders --------------------------------------------------------

// ReminderDue is a certificate whose student hasn't published it.
type ReminderDue struct {
	CertificateID string
	ClientSlug    string
}

// DueReminders lists active, private, never-claimed certificates issued at
// least `after` ago, for clients with reminders on, that haven't had a
// reminder and whose student's address isn't suppressed.
func (s *Store) DueReminders(ctx context.Context, after time.Duration, limit int) ([]ReminderDue, error) {
	rows, err := s.rdb.QueryContext(ctx, `
		SELECT c.id, cl.slug FROM certificates c
		JOIN clients cl ON cl.id = c.client_id
		JOIN students s ON s.id = c.student_id
		WHERE c.status = 'active' AND c.visibility = 'private' AND c.claimed_at IS NULL
		  AND c.reminder_sent_at IS NULL AND c.created_at <= ?
		  AND cl.send_reminders = 1 AND cl.status = 'active'
		  AND s.email NOT IN (SELECT email FROM email_suppressions)
		ORDER BY c.created_at LIMIT ?`, fmtTime(clock().Add(-after)), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReminderDue
	for rows.Next() {
		var r ReminderDue
		if err := rows.Scan(&r.CertificateID, &r.ClientSlug); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SendReminder atomically marks the certificate reminded, gives it a fresh
// claim token (only hashes are stored, so the original link can't be
// re-sent) and queues the email built by build. It does nothing (returns
// ErrNotFound) if the certificate was claimed or reminded meanwhile.
func (s *Store) SendReminder(ctx context.Context, sc Scope, certID string, build func(c *Certificate, token string) (*NewEmail, error)) error {
	clientID, err := sc.id()
	if err != nil {
		return err
	}
	tx, err := s.wdb.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	token, hash := newClaimToken()
	res, err := tx.ExecContext(ctx, `UPDATE certificates SET reminder_sent_at = ?, claim_token_hash = ?
		WHERE id = ? AND client_id = ? AND reminder_sent_at IS NULL AND claimed_at IS NULL
		  AND status = 'active' AND visibility = 'private'`, nowUTC(), hash, certID, clientID)
	if err := affectedOne(res, err); err != nil {
		return err
	}
	c, err := scanCert(tx.QueryRowContext(ctx, certSelect+` WHERE c.id = ?`, certID))
	if err != nil {
		return err
	}
	e, err := build(c, token)
	if err != nil {
		return err
	}
	e.ClientID = &clientID
	if _, err := enqueue(ctx, tx, *e); err != nil {
		return err
	}
	return tx.Commit()
}

// NewClaimLinkAndNotify is NewClaimLink that also queues an email with the
// new link, in the same transaction.
func (s *Store) NewClaimLinkAndNotify(ctx context.Context, sc Scope, certID string, build func(c *Certificate, token string) (*NewEmail, error)) (string, error) {
	clientID, err := sc.id()
	if err != nil {
		return "", err
	}
	tx, err := s.wdb.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	token, hash := newClaimToken()
	res, err := tx.ExecContext(ctx, `UPDATE certificates SET claim_token_hash = ? WHERE client_id = ? AND id = ?`,
		hash, clientID, certID)
	if err := affectedOne(res, err); err != nil {
		return "", err
	}
	c, err := scanCert(tx.QueryRowContext(ctx, certSelect+` WHERE c.id = ?`, certID))
	if err != nil {
		return "", err
	}
	e, err := build(c, token)
	if err != nil {
		return "", err
	}
	e.ClientID = &clientID
	if _, err := enqueue(ctx, tx, *e); err != nil {
		return "", err
	}
	return token, tx.Commit()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
