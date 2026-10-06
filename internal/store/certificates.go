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

// Issuer is the client that issued a certificate, as shown on public pages.
type Issuer struct {
	Slug          string `json:"slug"`
	Name          string `json:"name"`
	SiteURL       string `json:"site_url"`
	Blurb         string `json:"-"`
	LinkedInOrgID string `json:"-"`
	Status        string `json:"-"`
}

// Certificate is an issued credential, joined with the data needed to render it.
type Certificate struct {
	ID            string     `json:"id"`
	ClientID      int64      `json:"-"`
	Issuer        Issuer     `json:"issuer"`
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
	// EmailBlocked: the student's address is on the suppression list
	// (bounced or complained), so no email reaches them.
	EmailBlocked bool `json:"email_blocked,omitempty"`
}

// Public reports whether the student has made the certificate public.
func (c *Certificate) Public() bool { return c.Visibility == "public" }

// Revoked reports whether the certificate has been revoked.
func (c *Certificate) Revoked() bool { return c.Status == "revoked" }

const certSelect = `
SELECT c.id, c.client_id, cl.slug, cl.name, cl.site_url, cl.blurb, cl.linkedin_org_id, cl.status,
       c.student_id, s.email, c.recipient_name, c.course_title, co.slug, h.name,
       c.skills, c.issued_on, c.status, c.revoked_at, COALESCE(c.revoke_reason, ''),
       c.visibility, c.claimed_at, c.created_at,
       EXISTS (SELECT 1 FROM email_suppressions es WHERE es.email = s.email)
FROM certificates c
JOIN clients  cl ON cl.id = c.client_id
JOIN students s  ON s.id  = c.student_id
JOIN cohorts  h  ON h.id  = c.cohort_id
JOIN courses  co ON co.id = c.course_id`

type rowScanner interface{ Scan(dest ...any) error }

func scanCert(r rowScanner) (*Certificate, error) {
	var c Certificate
	var skills, issued, created string
	var revokedAt, claimedAt sql.NullString
	err := r.Scan(&c.ID, &c.ClientID, &c.Issuer.Slug, &c.Issuer.Name, &c.Issuer.SiteURL, &c.Issuer.Blurb,
		&c.Issuer.LinkedInOrgID, &c.Issuer.Status,
		&c.StudentID, &c.Email, &c.RecipientName, &c.CourseTitle, &c.CourseSlug,
		&c.CohortName, &skills, &issued, &c.Status, &revokedAt, &c.RevokeReason,
		&c.Visibility, &claimedAt, &created, &c.EmailBlocked)
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

// --- public lookups (no scope: certificate IDs and claim tokens are global) ---

// GetCertificate fetches a certificate by its public ID, for public pages.
// Callers must still check Public() before showing it.
func (s *Store) GetCertificate(ctx context.Context, id string) (*Certificate, error) {
	return scanCert(s.rdb.QueryRowContext(ctx, certSelect+` WHERE c.id = ?`, id))
}

// GetCertificateByClaimToken fetches the certificate a claim token belongs to.
func (s *Store) GetCertificateByClaimToken(ctx context.Context, token string) (*Certificate, error) {
	if token == "" {
		return nil, ErrNotFound
	}
	return scanCert(s.rdb.QueryRowContext(ctx,
		certSelect+` WHERE c.claim_token_hash = ?`, hashToken(token)))
}

// SetVisibilityByClaimToken makes the certificate a claim token belongs to
// public or private. The token is the authorization; revoked certificates
// can't be changed.
func (s *Store) SetVisibilityByClaimToken(ctx context.Context, token, visibility string) error {
	if visibility != "public" && visibility != "private" {
		return fmt.Errorf("%w: visibility must be public or private", ErrInvalid)
	}
	if token == "" {
		return ErrNotFound
	}
	res, err := s.wdb.ExecContext(ctx, `
		UPDATE certificates
		SET visibility = ?, claimed_at = COALESCE(claimed_at, ?)
		WHERE claim_token_hash = ? AND status = 'active'`, visibility, nowUTC(), hashToken(token))
	return affectedOne(res, err)
}

// --- client-scoped administration ------------------------------------------

// GetClientCertificate fetches one of the scoped client's certificates.
func (s *Store) GetClientCertificate(ctx context.Context, sc Scope, id string) (*Certificate, error) {
	clientID, err := sc.id()
	if err != nil {
		return nil, err
	}
	return scanCert(s.rdb.QueryRowContext(ctx, certSelect+` WHERE c.client_id = ? AND c.id = ?`, clientID, id))
}

// ListCertificates lists the scoped client's certificates, optionally
// filtered by course slug and cohort name.
func (s *Store) ListCertificates(ctx context.Context, sc Scope, courseSlug, cohort string) ([]Certificate, error) {
	clientID, err := sc.id()
	if err != nil {
		return nil, err
	}
	q := certSelect + ` WHERE c.client_id = ? AND (? = '' OR co.slug = ?) AND (? = '' OR h.name = ?)
		ORDER BY c.issued_on DESC, c.recipient_name`
	rows, err := s.rdb.QueryContext(ctx, q, clientID, courseSlug, courseSlug, cohort, cohort)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Certificate{}
	for rows.Next() {
		c, err := scanCert(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// Revoke marks one of the scoped client's certificates revoked. Revoked
// public certificates stay resolvable so verifiers see they're invalid.
func (s *Store) Revoke(ctx context.Context, sc Scope, id, reason string) error {
	clientID, err := sc.id()
	if err != nil {
		return err
	}
	res, err := s.wdb.ExecContext(ctx, `
		UPDATE certificates SET status = 'revoked', revoked_at = ?, revoke_reason = ?
		WHERE client_id = ? AND id = ? AND status = 'active'`, nowUTC(), reason, clientID, id)
	return affectedOne(res, err)
}

// NewClaimLink replaces a certificate's claim token (e.g. the student lost
// the email) and returns the new token. The old link stops working.
func (s *Store) NewClaimLink(ctx context.Context, sc Scope, id string) (string, error) {
	clientID, err := sc.id()
	if err != nil {
		return "", err
	}
	token, hash := newClaimToken()
	res, err := s.wdb.ExecContext(ctx,
		`UPDATE certificates SET claim_token_hash = ? WHERE client_id = ? AND id = ?`, hash, clientID, id)
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
	CourseTitle   string `json:"course_title"`
	ClaimToken    string `json:"-"`
	Existing      bool   `json:"existing"`
}

// Issue creates certificates for the scoped client in one transaction.
// It is idempotent per (student, cohort): re-importing the same CSV returns
// the existing certificate IDs instead of creating duplicates. Courses are
// looked up within the client only, so a CSV can't name another client's
// course.
func (s *Store) Issue(ctx context.Context, sc Scope, reqs []IssueRequest) ([]IssueResult, error) {
	return s.IssueAndNotify(ctx, sc, reqs, nil)
}

// NotifyFunc builds the email for a newly issued certificate (it gets the
// claim token). It runs inside the issuing transaction, so a certificate and
// its email are committed together or not at all.
type NotifyFunc func(IssueResult) (*NewEmail, error)

// IssueAndNotify is Issue, also queueing an email for each new certificate
// when notify is non-nil.
func (s *Store) IssueAndNotify(ctx context.Context, sc Scope, reqs []IssueRequest, notify NotifyFunc) ([]IssueResult, error) {
	clientID, err := sc.id()
	if err != nil {
		return nil, err
	}
	tx, err := s.wdb.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var status, prefix string
	if err := tx.QueryRowContext(ctx, `SELECT status, id_prefix FROM clients WHERE id = ?`, clientID).
		Scan(&status, &prefix); err != nil {
		return nil, err
	}
	if status != "active" {
		return nil, ErrSuspended
	}

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
			return nil, fmt.Errorf("%w: row %d: email, full_name, course, cohort and completed_on are required", ErrInvalid, i+1)
		}

		cs, ok := courses[slug]
		if !ok {
			err := tx.QueryRowContext(ctx,
				`SELECT id, title, skills FROM courses WHERE client_id = ? AND slug = ?`, clientID, slug).
				Scan(&cs.id, &cs.title, &cs.skills)
			if errors.Is(err, sql.ErrNoRows) {
				return nil, fmt.Errorf("%w: row %d: unknown course %q (create it first)", ErrInvalid, i+1, slug)
			}
			if err != nil {
				return nil, err
			}
			courses[slug] = cs
		}

		var cohortID int64
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO cohorts (client_id, course_id, name) VALUES (?, ?, ?)
			ON CONFLICT(course_id, name) DO UPDATE SET name = excluded.name
			RETURNING id`, clientID, cs.id, cohort).Scan(&cohortID); err != nil {
			return nil, fmt.Errorf("row %d: cohort: %w", i+1, err)
		}

		var studentID int64
		if err := tx.QueryRowContext(ctx, `
			INSERT INTO students (client_id, email, full_name) VALUES (?, ?, ?)
			ON CONFLICT(client_id, email) DO UPDATE SET email = students.email
			RETURNING id`, clientID, email, name).Scan(&studentID); err != nil {
			return nil, fmt.Errorf("row %d: student: %w", i+1, err)
		}

		res := IssueResult{Email: email, FullName: name, CourseTitle: cs.title}
		err := tx.QueryRowContext(ctx,
			`SELECT id FROM certificates WHERE student_id = ? AND cohort_id = ?`,
			studentID, cohortID).Scan(&res.CertificateID)
		switch {
		case err == nil:
			res.Existing = true
		case errors.Is(err, sql.ErrNoRows):
			token, hash := newClaimToken()
			res.CertificateID = certid.New(prefix)
			res.ClaimToken = token
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO certificates
					(id, client_id, student_id, course_id, cohort_id, recipient_name, course_title,
					 skills, issued_on, claim_token_hash)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				res.CertificateID, clientID, studentID, cs.id, cohortID, name, cs.title, cs.skills,
				r.CompletedOn.Format(dateLayout), hash); err != nil {
				return nil, fmt.Errorf("row %d: insert certificate: %w", i+1, err)
			}
			if notify != nil {
				e, err := notify(res)
				if err != nil {
					return nil, fmt.Errorf("row %d: email: %w", i+1, err)
				}
				if e != nil {
					e.ClientID = &clientID
					if _, err := enqueue(ctx, tx, *e); err != nil {
						return nil, fmt.Errorf("row %d: queue email: %w", i+1, err)
					}
				}
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
