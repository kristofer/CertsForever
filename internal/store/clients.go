package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/mattn/go-sqlite3"

	"certsforever/internal/certid"
)

var (
	// ErrInvalid wraps validation failures; the message says what's wrong.
	ErrInvalid = errors.New("invalid")
	// ErrConflict means a uniqueness rule or a frozen field was violated.
	ErrConflict = errors.New("conflict")
	// ErrNoScope means a client-scoped method was called without a resolved
	// client. It indicates a programming error, never a user error.
	ErrNoScope = errors.New("store: missing client scope")
	// ErrSuspended means the client is suspended and cannot issue.
	ErrSuspended = errors.New("client is suspended")
)

// Client is a tenant: an organization that issues certificates.
type Client struct {
	ID            int64      `json:"-"`
	Slug          string     `json:"slug"`
	Name          string     `json:"name"`
	IDPrefix      string     `json:"id_prefix"`
	Status        string     `json:"status"`
	SiteURL       string     `json:"site_url"`
	Blurb         string     `json:"blurb"`
	LinkedInOrgID string     `json:"linkedin_org_id"`
	ReplyTo       string     `json:"reply_to"`       // Reply-To on student emails
	SendReminders bool       `json:"send_reminders"` // one nudge to publish after 7 days
	CreatedAt     time.Time  `json:"created_at"`
	SuspendedAt   *time.Time `json:"suspended_at,omitempty"`
}

// Active reports whether the client may issue certificates.
func (c *Client) Active() bool { return c.Status == "active" }

// Scope is proof that a client was resolved. Every method that reads or
// writes one client's data takes a Scope, so forgetting the tenant check is
// a compile error, and the zero Scope is rejected at runtime.
type Scope struct {
	client *Client
}

// Client returns the scoped client.
func (sc Scope) Client() Client {
	if sc.client == nil {
		return Client{}
	}
	return *sc.client
}

func (sc Scope) id() (int64, error) {
	if sc.client == nil || sc.client.ID == 0 {
		return 0, ErrNoScope
	}
	return sc.client.ID, nil
}

// Scope resolves a client by slug.
func (s *Store) Scope(ctx context.Context, slug string) (Scope, error) {
	c, err := s.GetClient(ctx, slug)
	if err != nil {
		return Scope{}, err
	}
	return Scope{client: c}, nil
}

// ScopeForCertificate resolves the client that issued a certificate ID,
// using its prefix.
func (s *Store) ScopeForCertificate(ctx context.Context, certID string) (Scope, error) {
	c, err := s.getClient(ctx, `id_prefix = ?`, certid.Prefix(certID))
	if err != nil {
		return Scope{}, err
	}
	return Scope{client: c}, nil
}

var slugRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,39}$`)

// reservedSlugs would collide with console routes (/admin/{client}).
var reservedSlugs = map[string]bool{"api": true, "new": true, "admin": true, "super": true, "login": true,
	"logout": true, "account": true, "static": true}

// ClientInput is used to create a client or (with nil fields left alone)
// update one.
type ClientInput struct {
	Slug          string
	Name          *string
	IDPrefix      *string
	SiteURL       *string
	Blurb         *string
	LinkedInOrgID *string
	ReplyTo       *string
	SendReminders *bool
}

func str(p *string) string {
	if p == nil {
		return ""
	}
	return strings.TrimSpace(*p)
}

func validateClient(in ClientInput, creating bool) error {
	var errs []string
	if creating && !slugRE.MatchString(in.Slug) {
		errs = append(errs, "slug must be 2–40 lowercase letters, digits or dashes, starting with a letter or digit")
	} else if creating && reservedSlugs[in.Slug] {
		errs = append(errs, fmt.Sprintf("slug %q is reserved", in.Slug))
	}
	if (creating || in.Name != nil) && str(in.Name) == "" {
		errs = append(errs, "name is required")
	}
	if creating || in.IDPrefix != nil {
		if p := str(in.IDPrefix); !certid.ValidPrefix(p) {
			errs = append(errs, fmt.Sprintf("id_prefix %q must be 2–4 uppercase letters", p))
		}
	}
	if u := str(in.SiteURL); u != "" {
		if pu, err := url.Parse(u); err != nil || (pu.Scheme != "http" && pu.Scheme != "https") || pu.Host == "" {
			errs = append(errs, fmt.Sprintf("site_url %q must be an absolute http(s) URL", u))
		}
	}
	if r := str(in.ReplyTo); r != "" {
		if _, err := NormalizeEmail(r); err != nil {
			errs = append(errs, fmt.Sprintf("reply_to %q must be an email address", r))
		}
	}
	if l := str(in.LinkedInOrgID); strings.Trim(l, "0123456789") != "" {
		errs = append(errs, fmt.Sprintf("linkedin_org_id %q must be numeric", l))
	}
	if len(errs) > 0 {
		return fmt.Errorf("%w: %s", ErrInvalid, strings.Join(errs, "; "))
	}
	return nil
}

// CreateClient adds a client. Slug and prefix must be unused.
func (s *Store) CreateClient(ctx context.Context, in ClientInput) (*Client, error) {
	in.Slug = strings.ToLower(strings.TrimSpace(in.Slug))
	if in.IDPrefix != nil {
		p := strings.ToUpper(str(in.IDPrefix))
		in.IDPrefix = &p
	}
	if err := validateClient(in, true); err != nil {
		return nil, err
	}
	_, err := s.wdb.ExecContext(ctx, `
		INSERT INTO clients (slug, name, id_prefix, site_url, blurb, linkedin_org_id, reply_to, send_reminders)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		in.Slug, str(in.Name), str(in.IDPrefix), str(in.SiteURL), str(in.Blurb), str(in.LinkedInOrgID),
		str(in.ReplyTo), in.SendReminders == nil || *in.SendReminders)
	if err != nil {
		return nil, mapConstraint(err, "a client with that slug or id_prefix already exists")
	}
	return s.GetClient(ctx, in.Slug)
}

// UpdateClient changes the non-nil fields of a client. The prefix can only
// change while the client has no certificates.
func (s *Store) UpdateClient(ctx context.Context, in ClientInput) (*Client, error) {
	if in.IDPrefix != nil {
		p := strings.ToUpper(str(in.IDPrefix))
		in.IDPrefix = &p
	}
	if err := validateClient(in, false); err != nil {
		return nil, err
	}
	sets, args := []string{}, []any{}
	add := func(col string, v *string) {
		if v != nil {
			sets = append(sets, col+" = ?")
			args = append(args, str(v))
		}
	}
	add("name", in.Name)
	add("id_prefix", in.IDPrefix)
	add("site_url", in.SiteURL)
	add("blurb", in.Blurb)
	add("linkedin_org_id", in.LinkedInOrgID)
	add("reply_to", in.ReplyTo)
	if in.SendReminders != nil {
		sets = append(sets, "send_reminders = ?")
		args = append(args, *in.SendReminders)
	}
	if len(sets) > 0 {
		args = append(args, in.Slug)
		res, err := s.wdb.ExecContext(ctx,
			`UPDATE clients SET `+strings.Join(sets, ", ")+` WHERE slug = ?`, args...)
		if err := affectedOne(res, err); err != nil {
			return nil, mapConstraint(err, "id_prefix is already used by another client")
		}
	}
	return s.GetClient(ctx, in.Slug)
}

// SetClientStatus suspends or reactivates a client. Suspended clients
// cannot issue, but their public certificates keep resolving.
func (s *Store) SetClientStatus(ctx context.Context, slug, status string) error {
	if status != "active" && status != "suspended" {
		return fmt.Errorf("%w: status must be active or suspended", ErrInvalid)
	}
	res, err := s.wdb.ExecContext(ctx, `
		UPDATE clients
		SET status = ?, suspended_at = CASE WHEN ? = 'suspended' THEN ? ELSE NULL END
		WHERE slug = ?`, status, status, nowUTC(), slug)
	return affectedOne(res, err)
}

// GetClient looks up a client by slug.
func (s *Store) GetClient(ctx context.Context, slug string) (*Client, error) {
	return s.getClient(ctx, `slug = ?`, strings.ToLower(strings.TrimSpace(slug)))
}

// ListClients returns all clients, by slug.
func (s *Store) ListClients(ctx context.Context) ([]Client, error) {
	rows, err := s.rdb.QueryContext(ctx, clientSelect+` ORDER BY slug`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Client
	for rows.Next() {
		c, err := scanClient(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

const clientSelect = `SELECT id, slug, name, id_prefix, status, site_url, blurb, linkedin_org_id,
	reply_to, send_reminders, created_at, suspended_at FROM clients`

func (s *Store) getClient(ctx context.Context, where string, arg any) (*Client, error) {
	return scanClient(s.rdb.QueryRowContext(ctx, clientSelect+` WHERE `+where, arg))
}

func scanClient(r rowScanner) (*Client, error) {
	var c Client
	var created string
	var suspended sql.NullString
	err := r.Scan(&c.ID, &c.Slug, &c.Name, &c.IDPrefix, &c.Status, &c.SiteURL, &c.Blurb,
		&c.LinkedInOrgID, &c.ReplyTo, &c.SendReminders, &created, &suspended)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if t := parseTime(sql.NullString{String: created, Valid: true}); t != nil {
		c.CreatedAt = *t
	}
	c.SuspendedAt = parseTime(suspended)
	return &c, nil
}

// mapConstraint turns SQLite constraint/trigger errors into ErrConflict.
func mapConstraint(err error, msg string) error {
	var se sqlite3.Error
	if errors.As(err, &se) && se.Code == sqlite3.ErrConstraint {
		if strings.Contains(se.Error(), "cannot change") {
			return fmt.Errorf("%w: %s", ErrConflict, se.Error())
		}
		return fmt.Errorf("%w: %s", ErrConflict, msg)
	}
	return err
}
