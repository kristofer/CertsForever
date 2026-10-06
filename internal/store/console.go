package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// --- certificate search ------------------------------------------------------

// CertFilter narrows a certificate search. Empty fields match everything.
type CertFilter struct {
	Course  string // course slug
	Cohort  string // cohort name
	Query   string // matches recipient name, email or certificate ID
	Status  string // active | revoked
	Claimed string // yes | no
	Limit   int    // 0 = no limit
	Offset  int
}

// SearchCertificates returns one page of the scoped client's certificates
// and the total number matching.
func (s *Store) SearchCertificates(ctx context.Context, sc Scope, f CertFilter) ([]Certificate, int, error) {
	clientID, err := sc.id()
	if err != nil {
		return nil, 0, err
	}
	where := ` WHERE c.client_id = ?`
	args := []any{clientID}
	if f.Course != "" {
		where += ` AND co.slug = ?`
		args = append(args, strings.ToLower(f.Course))
	}
	if f.Cohort != "" {
		where += ` AND h.name = ?`
		args = append(args, f.Cohort)
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		like := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q) + "%"
		where += ` AND (c.recipient_name LIKE ? ESCAPE '\' OR s.email LIKE ? ESCAPE '\' OR c.id LIKE ? ESCAPE '\')`
		args = append(args, like, like, like)
	}
	switch f.Status {
	case "active", "revoked":
		where += ` AND c.status = ?`
		args = append(args, f.Status)
	}
	switch f.Claimed {
	case "yes":
		where += ` AND c.claimed_at IS NOT NULL`
	case "no":
		where += ` AND c.claimed_at IS NULL`
	}
	var total int
	if err := s.rdb.QueryRowContext(ctx, `SELECT COUNT(*) FROM certificates c
		JOIN students s ON s.id = c.student_id JOIN cohorts h ON h.id = c.cohort_id
		JOIN courses co ON co.id = c.course_id`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	q := certSelect + where + ` ORDER BY c.issued_on DESC, c.created_at DESC, c.recipient_name`
	if f.Limit > 0 {
		q += ` LIMIT ? OFFSET ?`
		args = append(args, f.Limit, f.Offset)
	}
	rows, err := s.rdb.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []Certificate{}
	for rows.Next() {
		c, err := scanCert(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *c)
	}
	return out, total, rows.Err()
}

// --- overview ------------------------------------------------------------------

// Overview is the headline numbers for a client's console.
type Overview struct {
	Courses      int `json:"courses"`
	Certificates int `json:"certificates"`
	Revoked      int `json:"revoked"`
	Claimed      int `json:"claimed"`
	Public       int `json:"public"`
	Views30d     int `json:"views_30d"`
	LearnMore30d int `json:"learn_more_30d"`
	EmailsFailed int `json:"emails_failed"`
}

// GetOverview returns the scoped client's headline numbers.
func (s *Store) GetOverview(ctx context.Context, sc Scope) (*Overview, error) {
	clientID, err := sc.id()
	if err != nil {
		return nil, err
	}
	since := clock().Add(-30 * 24 * time.Hour).UTC().Format(time.RFC3339)
	var o Overview
	err = s.rdb.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM courses WHERE client_id = ?1),
		(SELECT COUNT(*) FROM certificates WHERE client_id = ?1),
		(SELECT COUNT(*) FROM certificates WHERE client_id = ?1 AND status = 'revoked'),
		(SELECT COUNT(*) FROM certificates WHERE client_id = ?1 AND claimed_at IS NOT NULL),
		(SELECT COUNT(*) FROM certificates WHERE client_id = ?1 AND visibility = 'public'),
		(SELECT COUNT(*) FROM events WHERE client_id = ?1 AND kind = 'view' AND at >= ?2),
		(SELECT COUNT(*) FROM events WHERE client_id = ?1 AND kind = 'learn_more' AND at >= ?2),
		(SELECT COUNT(*) FROM email_outbox WHERE client_id = ?1 AND status = 'failed')`, clientID, since).
		Scan(&o.Courses, &o.Certificates, &o.Revoked, &o.Claimed, &o.Public, &o.Views30d, &o.LearnMore30d, &o.EmailsFailed)
	return &o, err
}

// --- cohorts -------------------------------------------------------------------

// CohortSummary is one cohort with its certificate counts.
type CohortSummary struct {
	Course      string    `json:"course"`
	CourseTitle string    `json:"course_title"`
	Name        string    `json:"name"`
	Total       int       `json:"total"`
	Claimed     int       `json:"claimed"`
	Public      int       `json:"public"`
	Revoked     int       `json:"revoked"`
	Unclaimed   int       `json:"unclaimed"` // active and not claimed
	IssuedOn    time.Time `json:"issued_on"` // latest
}

// ListCohorts returns the scoped client's cohorts, newest first.
func (s *Store) ListCohorts(ctx context.Context, sc Scope) ([]CohortSummary, error) {
	clientID, err := sc.id()
	if err != nil {
		return nil, err
	}
	rows, err := s.rdb.QueryContext(ctx, `
		SELECT co.slug, co.title, h.name, COUNT(c.id),
		       COUNT(c.claimed_at),
		       COUNT(CASE WHEN c.visibility = 'public' THEN 1 END),
		       COUNT(CASE WHEN c.status = 'revoked' THEN 1 END),
		       COUNT(CASE WHEN c.status = 'active' AND c.claimed_at IS NULL THEN 1 END),
		       COALESCE(MAX(c.issued_on), '')
		FROM cohorts h
		JOIN courses co ON co.id = h.course_id
		LEFT JOIN certificates c ON c.cohort_id = h.id
		WHERE h.client_id = ?
		GROUP BY h.id
		ORDER BY MAX(c.issued_on) DESC, co.slug, h.name`, clientID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CohortSummary{}
	for rows.Next() {
		var cs CohortSummary
		var issued string
		if err := rows.Scan(&cs.Course, &cs.CourseTitle, &cs.Name, &cs.Total, &cs.Claimed, &cs.Public,
			&cs.Revoked, &cs.Unclaimed, &issued); err != nil {
			return nil, err
		}
		if t := parseTime(sql.NullString{String: issued, Valid: issued != ""}); t != nil {
			cs.IssuedOn = *t
		}
		out = append(out, cs)
	}
	return out, rows.Err()
}

// LatestEmailStatus maps each of the given certificates to the status of
// the most recent email about it (queued, sending, sent, failed, suppressed).
func (s *Store) LatestEmailStatus(ctx context.Context, sc Scope, certIDs []string) (map[string]string, error) {
	clientID, err := sc.id()
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for start := 0; start < len(certIDs); start += 500 {
		chunk := certIDs[start:min(start+500, len(certIDs))]
		args := []any{clientID}
		for _, id := range chunk {
			args = append(args, id)
		}
		rows, err := s.rdb.QueryContext(ctx, `
			SELECT ref_id, status FROM email_outbox o
			WHERE client_id = ? AND ref_type = 'certificate' AND ref_id IN (?`+strings.Repeat(",?", len(chunk)-1)+`)
			  AND id = (SELECT MAX(id) FROM email_outbox o2 WHERE o2.client_id = o.client_id
			            AND o2.ref_type = 'certificate' AND o2.ref_id = o.ref_id)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id, st string
			if err := rows.Scan(&id, &st); err != nil {
				rows.Close()
				return nil, err
			}
			out[id] = st
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// CertificateEmails returns the emails sent about one certificate.
func (s *Store) CertificateEmails(ctx context.Context, sc Scope, certID string) ([]OutboxItem, error) {
	clientID, err := sc.id()
	if err != nil {
		return nil, err
	}
	rows, err := s.rdb.QueryContext(ctx, outboxSelect+`
		WHERE o.client_id = ? AND o.ref_type = 'certificate' AND o.ref_id = ? ORDER BY o.id DESC`, clientID, certID)
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
		out = append(out, *it)
	}
	return out, rows.Err()
}

// CertificateEvents counts each kind of event for one certificate.
func (s *Store) CertificateEvents(ctx context.Context, sc Scope, certID string) (map[EventKind]int, error) {
	clientID, err := sc.id()
	if err != nil {
		return nil, err
	}
	rows, err := s.rdb.QueryContext(ctx, `SELECT kind, COUNT(*) FROM events
		WHERE client_id = ? AND certificate_id = ? GROUP BY kind`, clientID, certID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[EventKind]int{}
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			return nil, err
		}
		out[EventKind(k)] = n
	}
	return out, rows.Err()
}

// --- name corrections ------------------------------------------------------------

// CorrectName fixes the recipient name on one of the scoped client's
// certificates, in place (its URL and LinkedIn entry stay valid), and
// returns the old name. The caller audits the change.
func (s *Store) CorrectName(ctx context.Context, sc Scope, certID, newName string) (string, error) {
	clientID, err := sc.id()
	if err != nil {
		return "", err
	}
	newName = strings.Join(strings.Fields(newName), " ")
	if newName == "" || len(newName) > 120 {
		return "", fmt.Errorf("%w: name is required (up to 120 characters)", ErrInvalid)
	}
	tx, err := s.wdb.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var old, status string
	err = tx.QueryRowContext(ctx, `SELECT recipient_name, status FROM certificates WHERE client_id = ? AND id = ?`,
		clientID, certID).Scan(&old, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	if status != "active" {
		return "", fmt.Errorf("%w: revoked certificates can't be changed", ErrConflict)
	}
	if old == newName {
		return old, fmt.Errorf("%w: that's already the name", ErrInvalid)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE certificates SET recipient_name = ?, name_corrected_at = ?
		WHERE client_id = ? AND id = ?`, newName, nowUTC(), clientID, certID); err != nil {
		return "", err
	}
	return old, tx.Commit()
}

// --- issue preview -----------------------------------------------------------------

// PreviewRow is what issuing one CSV row would do.
type PreviewRow struct {
	Row           int    `json:"row"`
	Email         string `json:"email"`
	FullName      string `json:"full_name"`
	Course        string `json:"course"`
	CourseTitle   string `json:"course_title,omitempty"`
	Cohort        string `json:"cohort"`
	CompletedOn   string `json:"completed_on"`
	Action        string `json:"action"` // new | existing | duplicate | error
	Message       string `json:"message,omitempty"`
	CertificateID string `json:"certificate_id,omitempty"` // for existing
	Suppressed    bool   `json:"email_suppressed,omitempty"`
}

// Preview is a dry run of an issue.
type Preview struct {
	Rows      []PreviewRow `json:"rows"`
	New       int          `json:"new"`
	Existing  int          `json:"existing"`
	Duplicate int          `json:"duplicate"`
	Errors    int          `json:"errors"`
}

// OK reports whether the issue can go ahead.
func (p *Preview) OK() bool { return p.Errors == 0 && p.New > 0 }

// PreviewIssue reports, row by row, what IssueAndNotify would do with reqs,
// without changing anything. Unlike IssueAndNotify it doesn't stop at the
// first bad row, so every problem can be shown at once.
func (s *Store) PreviewIssue(ctx context.Context, sc Scope, reqs []IssueRequest) (*Preview, error) {
	clientID, err := sc.id()
	if err != nil {
		return nil, err
	}
	courses := map[string]string{} // slug → title ("" = unknown)
	seen := map[string]int{}       // email|course|cohort → first row
	p := &Preview{Rows: []PreviewRow{}}
	for i, r := range reqs {
		pr := PreviewRow{Row: i + 1, Email: strings.TrimSpace(r.Email), FullName: strings.TrimSpace(r.FullName),
			Course: strings.ToLower(strings.TrimSpace(r.CourseSlug)), Cohort: strings.TrimSpace(r.Cohort)}
		if !r.CompletedOn.IsZero() {
			pr.CompletedOn = r.CompletedOn.Format(dateLayout)
		}
		fail := func(msg string) {
			pr.Action, pr.Message = "error", msg
			p.Errors++
			p.Rows = append(p.Rows, pr)
		}
		if pr.Email == "" || pr.FullName == "" || pr.Course == "" || pr.Cohort == "" || r.CompletedOn.IsZero() {
			fail("email, full_name, course, cohort and completed_on are required")
			continue
		}
		if !strings.Contains(pr.Email, "@") {
			fail("email doesn't look like an address")
			continue
		}
		title, ok := courses[pr.Course]
		if !ok {
			err := s.rdb.QueryRowContext(ctx, `SELECT title FROM courses WHERE client_id = ? AND slug = ?`,
				clientID, pr.Course).Scan(&title)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
			courses[pr.Course] = title
		}
		if title == "" {
			fail(fmt.Sprintf("unknown course %q (create it first)", pr.Course))
			continue
		}
		pr.CourseTitle = title
		key := strings.ToLower(pr.Email) + "|" + pr.Course + "|" + pr.Cohort
		if first, dup := seen[key]; dup {
			pr.Action, pr.Message = "duplicate", fmt.Sprintf("same person and cohort as row %d", first)
			p.Duplicate++
			p.Rows = append(p.Rows, pr)
			continue
		}
		seen[key] = pr.Row
		var existingName string
		err := s.rdb.QueryRowContext(ctx, `
			SELECT c.id, c.recipient_name FROM certificates c
			JOIN students st ON st.id = c.student_id
			JOIN cohorts h ON h.id = c.cohort_id
			JOIN courses co ON co.id = c.course_id
			WHERE c.client_id = ? AND st.email = ? AND co.slug = ? AND h.name = ?`,
			clientID, pr.Email, pr.Course, pr.Cohort).Scan(&pr.CertificateID, &existingName)
		switch {
		case err == nil:
			pr.Action = "existing"
			if existingName != pr.FullName {
				pr.Message = fmt.Sprintf("already issued as %q; use a name correction to change it", existingName)
			}
			p.Existing++
		case errors.Is(err, sql.ErrNoRows):
			pr.Action = "new"
			p.New++
		default:
			return nil, err
		}
		if pr.Action == "new" {
			if pr.Suppressed, err = s.IsSuppressed(ctx, pr.Email); err != nil {
				return nil, err
			}
		}
		p.Rows = append(p.Rows, pr)
	}
	return p, nil
}

// --- courses -------------------------------------------------------------------------

// CourseRow is a course with what the console shows about it.
type CourseRow struct {
	Course
	DesignID     int64  `json:"design_id,omitempty"`
	DesignName   string `json:"design,omitempty"`
	Certificates int    `json:"certificates"`
}

// ListCourseRows returns the scoped client's courses with their design and
// certificate counts.
func (s *Store) ListCourseRows(ctx context.Context, sc Scope) ([]CourseRow, error) {
	clientID, err := sc.id()
	if err != nil {
		return nil, err
	}
	rows, err := s.rdb.QueryContext(ctx, `
		SELECT co.id, co.slug, co.title, co.description, co.skills, co.hours,
		       COALESCE(co.design_id, 0), COALESCE(d.name, ''),
		       (SELECT COUNT(*) FROM certificates c WHERE c.course_id = co.id)
		FROM courses co LEFT JOIN certificate_designs d ON d.id = co.design_id
		WHERE co.client_id = ? ORDER BY co.slug`, clientID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CourseRow{}
	for rows.Next() {
		var cr CourseRow
		var skills string
		var hours sql.NullInt64
		if err := rows.Scan(&cr.ID, &cr.Slug, &cr.Title, &cr.Description, &skills, &hours,
			&cr.DesignID, &cr.DesignName, &cr.Certificates); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(skills), &cr.Skills)
		cr.Hours = int(hours.Int64)
		out = append(out, cr)
	}
	return out, rows.Err()
}

// SetCourseDesign picks the design for a course's future certificates
// (0 = the default look).
func (s *Store) SetCourseDesign(ctx context.Context, sc Scope, slug string, designID int64) error {
	clientID, err := sc.id()
	if err != nil {
		return err
	}
	d := sql.NullInt64{Int64: designID, Valid: designID != 0}
	if d.Valid {
		if _, err := s.GetDesign(ctx, sc, designID); err != nil {
			return fmt.Errorf("%w: unknown design", ErrInvalid)
		}
	}
	res, err := s.wdb.ExecContext(ctx, `UPDATE courses SET design_id = ? WHERE client_id = ? AND slug = ?`,
		d, clientID, strings.ToLower(slug))
	return affectedOne(res, mapConstraint(err, "design belongs to another client"))
}

// DeleteCourse removes a course that has no certificates (e.g. a typo).
func (s *Store) DeleteCourse(ctx context.Context, sc Scope, slug string) error {
	clientID, err := sc.id()
	if err != nil {
		return err
	}
	tx, err := s.wdb.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id int64
	var n int
	err = tx.QueryRowContext(ctx, `SELECT id, (SELECT COUNT(*) FROM certificates WHERE course_id = courses.id)
		FROM courses WHERE client_id = ? AND slug = ?`, clientID, strings.ToLower(slug)).Scan(&id, &n)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("%w: this course has certificates; it can't be deleted", ErrConflict)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM cohorts WHERE client_id = ? AND course_id = ?`, clientID, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM courses WHERE client_id = ? AND id = ?`, clientID, id); err != nil {
		return err
	}
	return tx.Commit()
}

// RemindNow is a manual "please publish your certificate" nudge from the
// console: like SendReminder, but regardless of whether a reminder was
// sent before. It records the reminder so the automatic one won't follow.
// Certificates that are claimed, revoked or emailed in the last day are
// skipped (ErrNotFound / ErrConflict).
func (s *Store) RemindNow(ctx context.Context, sc Scope, certID string, build func(c *Certificate, token string) (*NewEmail, error)) error {
	clientID, err := sc.id()
	if err != nil {
		return err
	}
	tx, err := s.wdb.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var recent int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM email_outbox WHERE client_id = ? AND ref_type = 'certificate'
		AND ref_id = ? AND created_at > ?`, clientID, certID, fmtTime(clock().Add(-24*time.Hour))).Scan(&recent); err != nil {
		return err
	}
	if recent > 0 {
		return fmt.Errorf("%w: emailed in the last day", ErrConflict)
	}
	token, hash := newClaimToken()
	res, err := tx.ExecContext(ctx, `UPDATE certificates SET reminder_sent_at = ?, claim_token_hash = ?
		WHERE id = ? AND client_id = ? AND claimed_at IS NULL AND status = 'active'`, nowUTC(), hash, certID, clientID)
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
