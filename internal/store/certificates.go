package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"certsforever/internal/certid"
)

// Certificate is an issued credential, joined with the data needed to render it.
type Certificate struct {
	ID            string     `json:"id"`
	StudentID     int64      `json:"-"`
	Email         string     `json:"email"`
	RecipientName string     `json:"recipient_name"`
	CourseTitle   string     `json:"course_title"`
	CourseSlug    string     `json:"course_slug"`
	CohortName    string     `json:"cohort"`
	Skills        []string   `json:"skills"`
	IssuedOn      time.Time  `json:"issued_on"`
	Status        string     `json:"status"`
	RevokedAt     *time.Time `json:"revoked_at,omitempty"`
	RevokeReason  string     `json:"revoke_reason,omitempty"`
	Visibility    string     `json:"visibility"`
	ClaimedAt     *time.Time `json:"claimed_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
}

// Public reports whether the student has made the certificate public.
func (c *Certificate) Public() bool { return c.Visibility == "public" }

// Revoked reports whether the certificate has been revoked.
func (c *Certificate) Revoked() bool { return c.Status == "revoked" }

const certSelect = `
SELECT c.id, c.student_id, s.email, c.recipient_name, c.course_title, co.slug, h.name,
       c.skills, c.issued_on, c.status, c.revoked_at, COALESCE(c.revoke_reason, ''),
       c.visibility, c.claimed_at, c.created_at
FROM certificates c
JOIN students s ON s.id = c.student_id
JOIN cohorts  h ON h.id = c.cohort_id
JOIN courses co ON co.id = h.course_id`

type rowScanner interface{ Scan(dest ...any) error }

func scanCert(r rowScanner) (*Certificate, error) {
	var c Certificate
	var skills, issued, created string
	var revokedAt, claimedAt sql.NullString
	err := r.Scan(&c.ID, &c.StudentID, &c.Email, &c.RecipientName, &c.CourseTitle, &c.CourseSlug,
		&c.CohortName, &skills, &issued, &c.Status, &revokedAt, &c.RevokeReason,
		&c.Visibility, &claimedAt, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(skills), &c.Skills)
	if t := parseTime(sql.NullString{String: issued, Valid: true}); t != nil {
		c.IssuedOn = *t
	}
	if t := parseTime(sql.NullString{String: created, Valid: true}); t != nil {
		c.CreatedAt = *t
	}
	c.RevokedAt = parseTime(revokedAt)
	c.ClaimedAt = parseTime(claimedAt)
	return &c, nil
}

// GetCertificate fetches a certificate by its public ID.
func (s *Store) GetCertificate(ctx context.Context, id string) (*Certificate, error) {
	return scanCert(s.db.QueryRowContext(ctx, certSelect+` WHERE c.id = ?`, id))
}

// GetCertificateByClaimToken fetches the certificate a claim token belongs to.
func (s *Store) GetCertificateByClaimToken(ctx context.Context, token string) (*Certificate, error) {
	if token == "" {
		return nil, ErrNotFound
	}
	return scanCert(s.db.QueryRowContext(ctx,
		certSelect+` WHERE c.claim_token_hash = ?`, hashToken(token)))
}

// ListCertificates lists certificates, optionally filtered by course slug and cohort name.
func (s *Store) ListCertificates(ctx context.Context, courseSlug, cohort string) ([]Certificate, error) {
	q := certSelect + ` WHERE (? = '' OR co.slug = ?) AND (? = '' OR h.name = ?)
		ORDER BY c.issued_on DESC, c.recipient_name`
	rows, err := s.db.QueryContext(ctx, q, courseSlug, courseSlug, cohort, cohort)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Certificate
	for rows.Next() {
		c, err := scanCert(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// SetVisibility makes a certificate public or private and records the
// first time the student acted on it.
func (s *Store) SetVisibility(ctx context.Context, id, visibility string) error {
	if visibility != "public" && visibility != "private" {
		return fmt.Errorf("invalid visibility %q", visibility)
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE certificates
		SET visibility = ?, claimed_at = COALESCE(claimed_at, ?)
		WHERE id = ?`, visibility, nowUTC(), id)
	return affectedOne(res, err)
}

// Revoke marks a certificate revoked. Revoked public certificates stay
// resolvable so anyone verifying them sees that they are no longer valid.
func (s *Store) Revoke(ctx context.Context, id, reason string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE certificates SET status = 'revoked', revoked_at = ?, revoke_reason = ?
		WHERE id = ? AND status = 'active'`, nowUTC(), reason, id)
	return affectedOne(res, err)
}

// NewClaimLink replaces a certificate's claim token (e.g. the student lost
// the email) and returns the new token. The old link stops working.
func (s *Store) NewClaimLink(ctx context.Context, id string) (string, error) {
	token, hash := newClaimToken()
	res, err := s.db.ExecContext(ctx,
		`UPDATE certificates SET claim_token_hash = ? WHERE id = ?`, hash, id)
	if err := affectedOne(res, err); err != nil {
		return "", err
	}
	return token, nil
}

func affectedOne(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// IssueRequest is one row of a cohort completion import.
type IssueRequest struct {
	Email       string    `json:"email"`
	FullName    string    `json:"full_name"`
	CourseSlug  string    `json:"course"`
	Cohort      string    `json:"cohort"`
	CompletedOn time.Time `json:"completed_on"`
}

// IssueResult reports what happened for one IssueRequest. ClaimToken is
// only set for newly issued certificates.
type IssueResult struct {
	Email         string `json:"email"`
	FullName      string `json:"full_name"`
	CertificateID string `json:"certificate_id"`
	ClaimToken    string `json:"-"`
	Existing      bool   `json:"existing"`
}

// Issue creates certificates for a batch of completions in one transaction.
// It is idempotent per (student, cohort): re-importing the same CSV returns
// the existing certificate IDs instead of creating duplicates.
func (s *Store) Issue(ctx context.Context, reqs []IssueRequest) ([]IssueResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	type courseSnap struct {
		id     int64
		title  string
		skills string
	}
	courses := map[string]courseSnap{}
	results := make([]IssueResult, 0, len(reqs))

	for i, r := range reqs {
		email := strings.TrimSpace(r.Email)
		name := strings.TrimSpace(r.FullName)
		slug := strings.ToLower(strings.TrimSpace(r.CourseSlug))
		cohort := strings.TrimSpace(r.Cohort)
		if email == "" || name == "" || slug == "" || cohort == "" || r.CompletedOn.IsZero() {
			return nil, fmt.Errorf("row %d: email, full_name, course, cohort and completed_on are required", i+1)
		}

		cs, ok := courses[slug]
		if !ok {
			err := tx.QueryRowContext(ctx, `SELECT id, title, skills FROM courses WHERE slug = ?`, slug).
				Scan(&cs.id, &cs.title, &cs.skills)
			if errors.Is(err, sql.ErrNoRows) {
				return nil, fmt.Errorf("row %d: unknown course %q (create it first)", i+1, slug)
			}
			if err != nil {
				return nil, err
			}
			courses[slug] = cs
		}

		var cohortID int64
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO cohorts (course_id, name) VALUES (?, ?)
			ON CONFLICT(course_id, name) DO UPDATE SET name = excluded.name
			RETURNING id`, cs.id, cohort).Scan(&cohortID); err != nil {
			return nil, fmt.Errorf("row %d: cohort: %w", i+1, err)
		}

		var studentID int64
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO students (email, full_name) VALUES (?, ?)
			ON CONFLICT(email) DO UPDATE SET email = students.email
			RETURNING id`, email, name).Scan(&studentID); err != nil {
			return nil, fmt.Errorf("row %d: student: %w", i+1, err)
		}

		res := IssueResult{Email: email, FullName: name}
		err := tx.QueryRowContext(ctx,
			`SELECT id FROM certificates WHERE student_id = ? AND cohort_id = ?`,
			studentID, cohortID).Scan(&res.CertificateID)
		switch {
		case err == nil:
			res.Existing = true
		case errors.Is(err, sql.ErrNoRows):
			token, hash := newClaimToken()
			res.CertificateID = certid.New()
			res.ClaimToken = token
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO certificates
					(id, student_id, cohort_id, recipient_name, course_title, skills, issued_on, claim_token_hash)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
				res.CertificateID, studentID, cohortID, name, cs.title, cs.skills,
				r.CompletedOn.Format(dateLayout), hash); err != nil {
				return nil, fmt.Errorf("row %d: insert certificate: %w", i+1, err)
			}
		default:
			return nil, err
		}
		results = append(results, res)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return results, nil
}
