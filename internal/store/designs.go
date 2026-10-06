package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Signatory is a name (and optional signature image) printed on a certificate.
type Signatory struct {
	Name             string `json:"name"`
	Title            string `json:"title,omitempty"`
	SignatureAssetID string `json:"signature_asset_id,omitempty"`
}

// Design is how a course's certificates look and read.
type Design struct {
	ID          int64       `json:"id"`
	Name        string      `json:"name"`
	Heading     string      `json:"heading"`
	BodyText    string      `json:"body_text"`
	AccentColor string      `json:"accent_color"`
	LogoAssetID string      `json:"logo_asset_id,omitempty"`
	Signatories []Signatory `json:"signatories"`
	UpdatedAt   time.Time   `json:"updated_at"`
	Courses     int         `json:"courses"` // how many courses use it
}

// DesignSnapshot is the design frozen onto a certificate when it's issued.
type DesignSnapshot struct {
	DesignID    int64       `json:"design_id,omitempty"`
	Heading     string      `json:"heading"`
	BodyText    string      `json:"body_text"`
	AccentColor string      `json:"accent_color"`
	LogoAssetID string      `json:"logo_asset_id,omitempty"`
	Signatories []Signatory `json:"signatories,omitempty"`
}

// DefaultDesign is the look of certificates without a design.
func DefaultDesign() DesignSnapshot {
	return DesignSnapshot{Heading: "Certificate of Completion", BodyText: "has successfully completed", AccentColor: "#1f8f81"}
}

// Snapshot freezes a design.
func (d Design) Snapshot() DesignSnapshot {
	return DesignSnapshot{DesignID: d.ID, Heading: d.Heading, BodyText: d.BodyText, AccentColor: d.AccentColor,
		LogoAssetID: d.LogoAssetID, Signatories: d.Signatories}
}

var hexColor = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

func validateDesign(d *Design) error {
	d.Name, d.Heading, d.BodyText = strings.TrimSpace(d.Name), strings.TrimSpace(d.Heading), strings.TrimSpace(d.BodyText)
	d.AccentColor = strings.ToLower(strings.TrimSpace(d.AccentColor))
	var errs []string
	if d.Name == "" || len(d.Name) > 80 {
		errs = append(errs, "name is required (up to 80 characters)")
	}
	if d.Heading == "" || len(d.Heading) > 60 {
		errs = append(errs, "heading is required (up to 60 characters)")
	}
	if len(d.BodyText) > 100 {
		errs = append(errs, "body text is up to 100 characters")
	}
	if !hexColor.MatchString(d.AccentColor) {
		errs = append(errs, "accent color must look like #1f8f81")
	}
	var sigs []Signatory
	for _, sg := range d.Signatories {
		sg.Name, sg.Title = strings.TrimSpace(sg.Name), strings.TrimSpace(sg.Title)
		if sg.Name == "" && sg.SignatureAssetID == "" {
			continue
		}
		if sg.Name == "" || len(sg.Name) > 60 || len(sg.Title) > 60 {
			errs = append(errs, "each signatory needs a name (up to 60 characters) and an optional title")
		}
		sigs = append(sigs, sg)
	}
	if len(sigs) > 2 {
		errs = append(errs, "at most two signatories")
	}
	d.Signatories = sigs
	if len(errs) > 0 {
		return fmt.Errorf("%w: %s", ErrInvalid, strings.Join(errs, "; "))
	}
	return nil
}

// SaveDesign creates (ID 0) or updates one of the scoped client's designs.
// Logos and signatures must be the client's own assets.
func (s *Store) SaveDesign(ctx context.Context, sc Scope, d Design) (int64, error) {
	clientID, err := sc.id()
	if err != nil {
		return 0, err
	}
	if err := validateDesign(&d); err != nil {
		return 0, err
	}
	tx, err := s.wdb.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	for _, a := range append([]string{d.LogoAssetID}, sigAssets(d.Signatories)...) {
		if a == "" {
			continue
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM assets WHERE client_id = ? AND id = ?`, clientID, a).Scan(&n); err != nil {
			return 0, err
		}
		if n == 0 {
			return 0, fmt.Errorf("%w: unknown image", ErrInvalid)
		}
	}
	sigs, _ := json.Marshal(d.Signatories)
	logo := sql.NullString{String: d.LogoAssetID, Valid: d.LogoAssetID != ""}
	if d.ID == 0 {
		err = tx.QueryRowContext(ctx, `INSERT INTO certificate_designs
			(client_id, name, heading, body_text, accent_color, logo_asset_id, signatories, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`,
			clientID, d.Name, d.Heading, d.BodyText, d.AccentColor, logo, string(sigs), nowUTC(), nowUTC()).Scan(&d.ID)
	} else {
		var res sql.Result
		res, err = tx.ExecContext(ctx, `UPDATE certificate_designs
			SET name = ?, heading = ?, body_text = ?, accent_color = ?, logo_asset_id = ?, signatories = ?, updated_at = ?
			WHERE client_id = ? AND id = ?`,
			d.Name, d.Heading, d.BodyText, d.AccentColor, logo, string(sigs), nowUTC(), clientID, d.ID)
		err = affectedOne(res, err)
	}
	if err != nil {
		return 0, mapConstraint(err, "a design with that name already exists")
	}
	return d.ID, tx.Commit()
}

func sigAssets(sigs []Signatory) []string {
	var out []string
	for _, s := range sigs {
		out = append(out, s.SignatureAssetID)
	}
	return out
}

const designSelect = `SELECT d.id, d.name, d.heading, d.body_text, d.accent_color, COALESCE(d.logo_asset_id, ''),
	d.signatories, d.updated_at, (SELECT COUNT(*) FROM courses c WHERE c.design_id = d.id)
	FROM certificate_designs d`

func scanDesign(r rowScanner) (*Design, error) {
	var d Design
	var sigs, updated string
	err := r.Scan(&d.ID, &d.Name, &d.Heading, &d.BodyText, &d.AccentColor, &d.LogoAssetID, &sigs, &updated, &d.Courses)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(sigs), &d.Signatories)
	if t := parseTime(sql.NullString{String: updated, Valid: true}); t != nil {
		d.UpdatedAt = *t
	}
	return &d, nil
}

// GetDesign returns one of the scoped client's designs.
func (s *Store) GetDesign(ctx context.Context, sc Scope, id int64) (*Design, error) {
	clientID, err := sc.id()
	if err != nil {
		return nil, err
	}
	return scanDesign(s.rdb.QueryRowContext(ctx, designSelect+` WHERE d.client_id = ? AND d.id = ?`, clientID, id))
}

// ListDesigns returns the scoped client's designs.
func (s *Store) ListDesigns(ctx context.Context, sc Scope) ([]Design, error) {
	clientID, err := sc.id()
	if err != nil {
		return nil, err
	}
	rows, err := s.rdb.QueryContext(ctx, designSelect+` WHERE d.client_id = ? ORDER BY d.name`, clientID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Design{}
	for rows.Next() {
		d, err := scanDesign(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *d)
	}
	return out, rows.Err()
}

// DeleteDesign removes a design no course uses. Certificates keep their
// snapshots.
func (s *Store) DeleteDesign(ctx context.Context, sc Scope, id int64) error {
	clientID, err := sc.id()
	if err != nil {
		return err
	}
	res, err := s.wdb.ExecContext(ctx, `DELETE FROM certificate_designs WHERE client_id = ? AND id = ?
		AND NOT EXISTS (SELECT 1 FROM courses WHERE design_id = ?)`, clientID, id, id)
	if err := affectedOne(res, err); errors.Is(err, ErrNotFound) {
		if _, gerr := s.GetDesign(ctx, sc, id); gerr == nil {
			return fmt.Errorf("%w: courses still use this design", ErrConflict)
		}
		return ErrNotFound
	} else if err != nil {
		return err
	}
	return nil
}

// snapshotFor returns the JSON snapshot of a course's design (or the default).
func snapshotFor(ctx context.Context, tx *sql.Tx, courseID int64) (string, error) {
	snap := DefaultDesign()
	d, err := scanDesign(tx.QueryRowContext(ctx, designSelect+`
		WHERE d.id = (SELECT design_id FROM courses WHERE id = ?)`, courseID))
	switch {
	case err == nil:
		snap = d.Snapshot()
	case !errors.Is(err, ErrNotFound):
		return "", err
	}
	b, err := json.Marshal(snap)
	return string(b), err
}

func parseSnapshot(s sql.NullString) DesignSnapshot {
	snap := DefaultDesign()
	if s.Valid && s.String != "" {
		_ = json.Unmarshal([]byte(s.String), &snap)
	}
	return snap
}

// --- assets ------------------------------------------------------------------

// Asset is an uploaded image (already normalized to PNG by the caller).
type Asset struct {
	ID          string
	Kind        string
	ContentType string
	Bytes       []byte
	Width       int
	Height      int
	SHA256      string
}

// SaveAsset stores an image for the scoped client and returns its id.
func (s *Store) SaveAsset(ctx context.Context, sc Scope, kind string, png []byte, width, height int) (string, error) {
	clientID, err := sc.id()
	if err != nil {
		return "", err
	}
	if kind != "logo" && kind != "signature" {
		return "", fmt.Errorf("%w: asset kind", ErrInvalid)
	}
	sum := sha256.Sum256(png)
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	id := hex.EncodeToString(b)
	_, err = s.wdb.ExecContext(ctx, `INSERT INTO assets (id, client_id, kind, content_type, bytes, sha256, width, height)
		VALUES (?, ?, ?, 'image/png', ?, ?, ?, ?)`, id, clientID, kind, png, hex.EncodeToString(sum[:]), width, height)
	return id, err
}

// GetAsset returns an asset by id (public: assets appear on certificate pages).
func (s *Store) GetAsset(ctx context.Context, id string) (*Asset, error) {
	var a Asset
	err := s.rdb.QueryRowContext(ctx, `SELECT id, kind, content_type, bytes, width, height, sha256 FROM assets WHERE id = ?`, id).
		Scan(&a.ID, &a.Kind, &a.ContentType, &a.Bytes, &a.Width, &a.Height, &a.SHA256)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &a, err
}
