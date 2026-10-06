package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Platform-wide queries for the super console. None of these take a Scope:
// they're only reachable by platform administrators.

// --- acting as a client ----------------------------------------------------------

// SetActing records which client a session is acting as (0 = none).
func (s *Store) SetActing(ctx context.Context, sessionID string, clientID int64) error {
	v := sql.NullInt64{Int64: clientID, Valid: clientID != 0}
	res, err := s.wdb.ExecContext(ctx, `UPDATE sessions SET acting_client_id = ? WHERE id_hash = ?`, v, hashToken(sessionID))
	return affectedOne(res, err)
}

// --- clients ---------------------------------------------------------------------

// SetClientStatusReason suspends (with a reason) or reactivates a client.
func (s *Store) SetClientStatusReason(ctx context.Context, slug, status, reason string) error {
	if status != "active" && status != "suspended" {
		return fmt.Errorf("%w: status must be active or suspended", ErrInvalid)
	}
	if status == "active" {
		reason = ""
	}
	reason = strings.TrimSpace(reason)
	if len(reason) > 300 {
		return fmt.Errorf("%w: reason is up to 300 characters", ErrInvalid)
	}
	res, err := s.wdb.ExecContext(ctx, `
		UPDATE clients
		SET status = ?, suspend_reason = ?,
		    suspended_at = CASE WHEN ? = 'suspended' THEN COALESCE(suspended_at, ?) ELSE NULL END
		WHERE slug = ?`, status, reason, status, nowUTC(), strings.ToLower(slug))
	return affectedOne(res, err)
}

// PrefixLocked reports whether a client's ID prefix is frozen (it has
// issued certificates, so the prefix is in public URLs).
func (s *Store) PrefixLocked(ctx context.Context, sc Scope) (bool, error) {
	clientID, err := sc.id()
	if err != nil {
		return false, err
	}
	var n int
	err = s.rdb.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM certificates WHERE client_id = ?)`, clientID).Scan(&n)
	return n == 1, err
}

// ClientSummary is one row of the platform's client list.
type ClientSummary struct {
	Client
	Certificates int        `json:"certificates"`
	Issued30d    int        `json:"issued_30d"`
	Public       int        `json:"public"`
	Admins       int        `json:"admins"`
	Courses      int        `json:"courses"`
	LastIssued   *time.Time `json:"last_issued,omitempty"`
	Domain       string     `json:"domain,omitempty"` // canonical custom domain
}

// ListClientSummaries returns every client with its headline numbers.
func (s *Store) ListClientSummaries(ctx context.Context) ([]ClientSummary, error) {
	since := fmtTime(clock().Add(-30 * 24 * time.Hour))
	rows, err := s.rdb.QueryContext(ctx, `
		SELECT cl.slug,
		       (SELECT COUNT(*) FROM certificates c WHERE c.client_id = cl.id),
		       (SELECT COUNT(*) FROM certificates c WHERE c.client_id = cl.id AND c.created_at >= ?),
		       (SELECT COUNT(*) FROM certificates c WHERE c.client_id = cl.id AND c.visibility = 'public'),
		       (SELECT COUNT(*) FROM memberships m JOIN users u ON u.id = m.user_id
		         WHERE m.client_id = cl.id AND u.disabled_at IS NULL),
		       (SELECT COUNT(*) FROM courses co WHERE co.client_id = cl.id),
		       (SELECT MAX(created_at) FROM certificates c WHERE c.client_id = cl.id),
		       COALESCE((SELECT host FROM client_domains d WHERE d.client_id = cl.id AND d.is_canonical = 1), '')
		FROM clients cl ORDER BY cl.name`, since)
	if err != nil {
		return nil, err
	}
	type extra struct {
		slug                                   string
		certs, issued, public, admins, courses int
		last                                   sql.NullString
		domain                                 string
	}
	var ex []extra
	for rows.Next() {
		var e extra
		if err := rows.Scan(&e.slug, &e.certs, &e.issued, &e.public, &e.admins, &e.courses, &e.last, &e.domain); err != nil {
			rows.Close()
			return nil, err
		}
		ex = append(ex, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]ClientSummary, 0, len(ex))
	for _, e := range ex {
		c, err := s.GetClient(ctx, e.slug)
		if err != nil {
			return nil, err
		}
		out = append(out, ClientSummary{Client: *c, Certificates: e.certs, Issued30d: e.issued, Public: e.public,
			Admins: e.admins, Courses: e.courses, LastIssued: parseTime(e.last), Domain: e.domain})
	}
	return out, nil
}

// PlatformStats is the super console's headline numbers.
type PlatformStats struct {
	Clients       int `json:"clients"`
	Suspended     int `json:"suspended"`
	Certificates  int `json:"certificates"`
	Issued30d     int `json:"issued_30d"`
	Users         int `json:"users"`
	SuperAdmins   int `json:"super_admins"`
	SupersNoTOTP  int `json:"super_admins_without_2fa"`
	EmailsFailed  int `json:"emails_failed"`
	EmailsWaiting int `json:"emails_waiting"`
}

// GetPlatformStats returns platform-wide counts.
func (s *Store) GetPlatformStats(ctx context.Context) (*PlatformStats, error) {
	since := fmtTime(clock().Add(-30 * 24 * time.Hour))
	var p PlatformStats
	err := s.rdb.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM clients),
		(SELECT COUNT(*) FROM clients WHERE status = 'suspended'),
		(SELECT COUNT(*) FROM certificates),
		(SELECT COUNT(*) FROM certificates WHERE created_at >= ?),
		(SELECT COUNT(*) FROM users WHERE disabled_at IS NULL),
		(SELECT COUNT(*) FROM users WHERE disabled_at IS NULL AND is_super_admin = 1),
		(SELECT COUNT(*) FROM users WHERE disabled_at IS NULL AND is_super_admin = 1 AND totp_enabled_at IS NULL),
		(SELECT COUNT(*) FROM email_outbox WHERE status = 'failed'),
		(SELECT COUNT(*) FROM email_outbox WHERE status IN ('queued', 'sending'))`, since).
		Scan(&p.Clients, &p.Suspended, &p.Certificates, &p.Issued30d, &p.Users, &p.SuperAdmins, &p.SupersNoTOTP,
			&p.EmailsFailed, &p.EmailsWaiting)
	return &p, err
}

// --- users -----------------------------------------------------------------------

// UserFilter narrows the user list.
type UserFilter struct {
	Query string // email or name contains
	Role  string // "super", "disabled", "admin" (member of some client), "" = all
}

// UserRow is a user with how many clients they administer.
type UserRow struct {
	User
	Clients int `json:"clients"`
}

// ListUsers returns users matching f, newest sign-in first.
func (s *Store) ListUsers(ctx context.Context, f UserFilter) ([]UserRow, error) {
	where := ` WHERE 1 = 1`
	var args []any
	if q := strings.TrimSpace(f.Query); q != "" {
		like := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q) + "%"
		where += ` AND (u.email LIKE ? ESCAPE '\' OR u.name LIKE ? ESCAPE '\')`
		args = append(args, like, like)
	}
	switch f.Role {
	case "super":
		where += ` AND u.is_super_admin = 1`
	case "disabled":
		where += ` AND u.disabled_at IS NOT NULL`
	case "admin":
		where += ` AND EXISTS (SELECT 1 FROM memberships m WHERE m.user_id = u.id)`
	}
	q := `SELECT u.id, u.email, u.name, u.is_super_admin, u.totp_enabled_at IS NOT NULL,
		u.disabled_at IS NOT NULL, u.created_at, u.last_login_at,
		(SELECT COUNT(*) FROM memberships m WHERE m.user_id = u.id)
		FROM users u` + where + ` ORDER BY u.last_login_at IS NULL, u.last_login_at DESC, u.email LIMIT 500`
	rows, err := s.rdb.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []UserRow{}
	for rows.Next() {
		var r UserRow
		var created string
		var last sql.NullString
		if err := rows.Scan(&r.ID, &r.Email, &r.Name, &r.IsSuper, &r.TOTPEnabled, &r.Disabled, &created, &last, &r.Clients); err != nil {
			return nil, err
		}
		if t := parseTime(sql.NullString{String: created, Valid: true}); t != nil {
			r.CreatedAt = *t
		}
		r.LastLoginAt = parseTime(last)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ActiveSessions counts a user's live sessions.
func (s *Store) ActiveSessions(ctx context.Context, userID int64) (int, error) {
	var n int
	err := s.rdb.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions WHERE user_id = ? AND expires_at > ? AND last_seen_at > ?`,
		userID, nowUTC(), fmtTime(clock().Add(-SessionIdle))).Scan(&n)
	return n, err
}

// LastEnabledSuper reports whether userID is the only enabled super admin.
func (s *Store) LastEnabledSuper(ctx context.Context, userID int64) (bool, error) {
	var n int
	err := s.rdb.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE is_super_admin = 1 AND disabled_at IS NULL AND id != ?`,
		userID).Scan(&n)
	return n == 0, err
}

// --- audit search ------------------------------------------------------------------

// AuditFilter narrows the platform audit log.
type AuditFilter struct {
	Client string // client slug; "-" = platform-level entries only
	Actor  string // contains
	Action string // prefix, e.g. "certificate." or "client.create"
	Before int64  // entries with id < Before (paging); 0 = newest
	Limit  int
}

// SearchAudit returns matching entries, newest first, and whether more exist.
func (s *Store) SearchAudit(ctx context.Context, f AuditFilter) ([]AuditEntry, bool, error) {
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 100
	}
	where := ` WHERE 1 = 1`
	var args []any
	switch f.Client {
	case "":
	case "-":
		where += ` AND a.client_id IS NULL`
	default:
		where += ` AND c.slug = ?`
		args = append(args, strings.ToLower(f.Client))
	}
	esc := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	if v := strings.TrimSpace(f.Actor); v != "" {
		where += ` AND a.actor_label LIKE ? ESCAPE '\'`
		args = append(args, "%"+esc.Replace(v)+"%")
	}
	if v := strings.TrimSpace(f.Action); v != "" {
		where += ` AND a.action LIKE ? ESCAPE '\'`
		args = append(args, esc.Replace(v)+"%")
	}
	if f.Before > 0 {
		where += ` AND a.id < ?`
		args = append(args, f.Before)
	}
	args = append(args, f.Limit+1)
	list, err := s.listAudit(ctx, auditSelect+where+` ORDER BY a.id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, false, err
	}
	more := len(list) > f.Limit
	if more {
		list = list[:f.Limit]
	}
	return list, more, nil
}

// --- domains -------------------------------------------------------------------------

// Domain is a client's custom domain.
type Domain struct {
	ID         int64      `json:"id"`
	Host       string     `json:"host"`
	Canonical  bool       `json:"canonical"`
	VerifiedAt *time.Time `json:"verified_at,omitempty"`
	VerifiedBy string     `json:"verified_by,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

// Verified reports whether the domain has been verified.
func (d *Domain) Verified() bool { return d.VerifiedAt != nil }

var hostLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// NormalizeHost validates a hostname typed by a person: it accepts a pasted
// URL ("https://certs.example.org/") and returns "certs.example.org".
func NormalizeHost(in string) (string, error) {
	h := strings.ToLower(strings.TrimSpace(in))
	h = strings.TrimPrefix(strings.TrimPrefix(h, "https://"), "http://")
	h = strings.TrimSuffix(strings.SplitN(h, "/", 2)[0], ".")
	labels := strings.Split(h, ".")
	ok := len(h) >= 4 && len(h) <= 253 && len(labels) >= 2 && !strings.Contains(h, ":")
	for _, l := range labels {
		ok = ok && hostLabel.MatchString(l)
	}
	if ok && strings.Trim(labels[len(labels)-1], "0123456789") == "" {
		ok = false // an IP address
	}
	if !ok {
		return "", fmt.Errorf("%w: %q isn't a domain name like certs.example.org", ErrInvalid, in)
	}
	return h, nil
}

const domainSelect = `SELECT id, host, is_canonical, verified_at, verified_by, created_at FROM client_domains`

func scanDomain(r rowScanner) (*Domain, error) {
	var d Domain
	var verified sql.NullString
	var created string
	err := r.Scan(&d.ID, &d.Host, &d.Canonical, &verified, &d.VerifiedBy, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	d.VerifiedAt = parseTime(verified)
	if t := parseTime(sql.NullString{String: created, Valid: true}); t != nil {
		d.CreatedAt = *t
	}
	return &d, nil
}

// ListDomains returns the scoped client's domains.
func (s *Store) ListDomains(ctx context.Context, sc Scope) ([]Domain, error) {
	clientID, err := sc.id()
	if err != nil {
		return nil, err
	}
	rows, err := s.rdb.QueryContext(ctx, domainSelect+` WHERE client_id = ? ORDER BY is_canonical DESC, host`, clientID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Domain{}
	for rows.Next() {
		d, err := scanDomain(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *d)
	}
	return out, rows.Err()
}

// GetDomain returns one of the scoped client's domains.
func (s *Store) GetDomain(ctx context.Context, sc Scope, id int64) (*Domain, error) {
	clientID, err := sc.id()
	if err != nil {
		return nil, err
	}
	return scanDomain(s.rdb.QueryRowContext(ctx, domainSelect+` WHERE client_id = ? AND id = ?`, clientID, id))
}

// ClientForHost returns the client a custom domain belongs to (for routing
// and the TLS "ask" check in Milestone 6). Only verified domains count.
func (s *Store) ClientForHost(ctx context.Context, host string) (Scope, *Domain, error) {
	var clientID int64
	row := s.rdb.QueryRowContext(ctx, `SELECT client_id, id, host, is_canonical, verified_at, verified_by, created_at
		FROM client_domains WHERE host = ? AND verified_at IS NOT NULL`, strings.ToLower(strings.TrimSuffix(host, ".")))
	var d Domain
	var verified sql.NullString
	var created string
	err := row.Scan(&clientID, &d.ID, &d.Host, &d.Canonical, &verified, &d.VerifiedBy, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Scope{}, nil, ErrNotFound
	}
	if err != nil {
		return Scope{}, nil, err
	}
	d.VerifiedAt = parseTime(verified)
	c, err := s.getClient(ctx, `id = ?`, clientID)
	if err != nil {
		return Scope{}, nil, err
	}
	return Scope{client: c}, &d, nil
}

// AddDomain records an unverified custom domain for the scoped client.
func (s *Store) AddDomain(ctx context.Context, sc Scope, host string) (*Domain, error) {
	clientID, err := sc.id()
	if err != nil {
		return nil, err
	}
	h, err := NormalizeHost(host)
	if err != nil {
		return nil, err
	}
	var id int64
	err = s.wdb.QueryRowContext(ctx, `INSERT INTO client_domains (client_id, host) VALUES (?, ?) RETURNING id`, clientID, h).Scan(&id)
	if err != nil {
		return nil, mapConstraint(err, "that domain is already registered")
	}
	return s.GetDomain(ctx, sc, id)
}

// MarkDomainVerified records that a domain points at this server ("dns")
// or that an administrator checked it ("manual").
func (s *Store) MarkDomainVerified(ctx context.Context, sc Scope, id int64, by string) error {
	clientID, err := sc.id()
	if err != nil {
		return err
	}
	if by != "dns" && by != "manual" {
		return fmt.Errorf("%w: verified by", ErrInvalid)
	}
	res, err := s.wdb.ExecContext(ctx, `UPDATE client_domains SET verified_at = COALESCE(verified_at, ?), verified_by = ?
		WHERE client_id = ? AND id = ?`, nowUTC(), by, clientID, id)
	return affectedOne(res, err)
}

// SetCanonicalDomain makes a verified domain the one certificate links use
// (id 0: use the platform domain). Other domains keep working.
func (s *Store) SetCanonicalDomain(ctx context.Context, sc Scope, id int64) error {
	clientID, err := sc.id()
	if err != nil {
		return err
	}
	tx, err := s.wdb.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE client_domains SET is_canonical = 0 WHERE client_id = ? AND is_canonical = 1`, clientID); err != nil {
		return err
	}
	if id != 0 {
		res, err := tx.ExecContext(ctx, `UPDATE client_domains SET is_canonical = 1 WHERE client_id = ? AND id = ?`, clientID, id)
		if err := affectedOne(res, err); err != nil {
			if strings.Contains(err.Error(), "must be verified") {
				return fmt.Errorf("%w: verify the domain first", ErrConflict)
			}
			return err
		}
	}
	return tx.Commit()
}

// DeleteDomain removes a domain that was never verified (e.g. a typo).
// Verified domains can only stop being canonical, since links may use them.
func (s *Store) DeleteDomain(ctx context.Context, sc Scope, id int64) error {
	clientID, err := sc.id()
	if err != nil {
		return err
	}
	res, err := s.wdb.ExecContext(ctx, `DELETE FROM client_domains WHERE client_id = ? AND id = ?`, clientID, id)
	if err != nil && strings.Contains(err.Error(), "retired, not deleted") {
		return fmt.Errorf("%w: a verified domain can't be removed, because certificate links may use it", ErrConflict)
	}
	return affectedOne(res, err)
}

// --- system ------------------------------------------------------------------------------

// MigrationInfo is one applied schema migration.
type MigrationInfo struct {
	Version   int       `json:"version"`
	AppliedAt time.Time `json:"applied_at"`
}

// SystemInfo describes the database for the System page.
type SystemInfo struct {
	Path       string          `json:"path"`
	SizeBytes  int64           `json:"size_bytes"`
	WALBytes   int64           `json:"wal_bytes"`
	FreeBytes  int64           `json:"free_bytes"` // reclaimable by VACUUM
	Migrations []MigrationInfo `json:"migrations"`
	Snapshots  []SnapshotFile  `json:"snapshots"` // pre-migration snapshots next to the database
}

// SnapshotFile is a pre-migration snapshot on disk.
type SnapshotFile struct {
	Name    string    `json:"name"`
	Bytes   int64     `json:"bytes"`
	ModTime time.Time `json:"mod_time"`
}

// preMigrationSnapshots lists the snapshots Open wrote next to the database
// (they're never deleted automatically; the runbook says when to).
func preMigrationSnapshots(dbPath string) []SnapshotFile {
	base := strings.TrimSuffix(filepath.Base(dbPath), filepath.Ext(dbPath))
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(dbPath), base+".pre-*.db"))
	out := []SnapshotFile{}
	for _, m := range matches {
		if fi, err := os.Stat(m); err == nil {
			out = append(out, SnapshotFile{Name: filepath.Base(m), Bytes: fi.Size(), ModTime: fi.ModTime()})
		}
	}
	return out
}

// GetSystemInfo reports database size and migration history.
func (s *Store) GetSystemInfo(ctx context.Context) (*SystemInfo, error) {
	si := &SystemInfo{Path: s.path}
	var pages, pageSize, free int64
	if err := s.rdb.QueryRowContext(ctx, `SELECT page_count, page_size, freelist_count
		FROM pragma_page_count(), pragma_page_size(), pragma_freelist_count()`).Scan(&pages, &pageSize, &free); err != nil {
		return nil, err
	}
	si.SizeBytes, si.FreeBytes = pages*pageSize, free*pageSize
	if fi, err := os.Stat(s.path + "-wal"); err == nil {
		si.WALBytes = fi.Size()
	}
	rows, err := s.rdb.QueryContext(ctx, `SELECT version, applied_at FROM schema_migrations ORDER BY version`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var m MigrationInfo
		var at string
		if err := rows.Scan(&m.Version, &at); err != nil {
			return nil, err
		}
		if t := parseTime(sql.NullString{String: at, Valid: true}); t != nil {
			m.AppliedAt = *t
		}
		si.Migrations = append(si.Migrations, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	si.Snapshots = preMigrationSnapshots(s.path)
	return si, nil
}
