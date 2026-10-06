// Package store is the SQLite persistence layer for CertsForever.
//
// Connection model: SQLite allows one writer at a time, so the store keeps
// two pools. The write pool has exactly one connection and starts every
// transaction with BEGIN IMMEDIATE, so writers queue inside Go instead of
// racing for the lock and failing with SQLITE_BUSY. The read pool has
// several query-only connections that read concurrently under WAL.
package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

//go:embed migrations/*.sql
var embeddedMigrations embed.FS

// ErrNotFound is returned when a requested row does not exist.
var ErrNotFound = errors.New("not found")

// Options tune Open. The zero value is the production default.
type Options struct {
	// ReadConns is the size of the read pool (default 8).
	ReadConns int
	// SnapshotDir is where pre-migration snapshots are written (default:
	// the database's directory). Set to "-" to disable snapshots.
	SnapshotDir string

	migrations fs.FS // overridden in tests; nil means the embedded set
}

// MigrationReport describes what Open did to the schema.
type MigrationReport struct {
	Applied  []int  // versions applied during this Open, in order
	Snapshot string // path of the pre-migration snapshot, if one was taken
	Version  int    // schema version after migrating
}

// Store wraps the database handles.
type Store struct {
	path      string
	wdb       *sql.DB // single writer
	rdb       *sql.DB // concurrent readers
	Migration MigrationReport
}

// Open opens (creating if needed) the SQLite database at path and applies
// any pending migrations.
func Open(ctx context.Context, path string) (*Store, error) {
	return OpenWithOptions(ctx, path, Options{})
}

// OpenWithOptions is Open with explicit options.
func OpenWithOptions(ctx context.Context, path string, opts Options) (*Store, error) {
	if path == "" {
		return nil, errors.New("database path is empty")
	}
	if opts.ReadConns <= 0 {
		opts.ReadConns = 8
	}
	if opts.migrations == nil {
		sub, err := fs.Sub(embeddedMigrations, "migrations")
		if err != nil {
			return nil, err
		}
		opts.migrations = sub
	}

	wdb, err := sql.Open("sqlite3", dsn(path, true))
	if err != nil {
		return nil, err
	}
	wdb.SetMaxOpenConns(1)
	wdb.SetMaxIdleConns(1)
	wdb.SetConnMaxLifetime(0)
	if err := wdb.PingContext(ctx); err != nil {
		wdb.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}

	s := &Store{path: path, wdb: wdb}
	if err := s.migrate(ctx, opts); err != nil {
		wdb.Close()
		return nil, err
	}

	// Readers are opened after migrating so they never see a half-built schema.
	rdb, err := sql.Open("sqlite3", dsn(path, false))
	if err != nil {
		wdb.Close()
		return nil, err
	}
	rdb.SetMaxOpenConns(opts.ReadConns)
	rdb.SetMaxIdleConns(opts.ReadConns)
	if err := rdb.PingContext(ctx); err != nil {
		wdb.Close()
		rdb.Close()
		return nil, fmt.Errorf("open readers %s: %w", path, err)
	}
	s.rdb = rdb
	return s, nil
}

// dsn builds a mattn/go-sqlite3 connection string.
func dsn(path string, writer bool) string {
	q := url.Values{}
	q.Set("_foreign_keys", "on")
	q.Set("_busy_timeout", "5000")
	q.Set("_synchronous", "NORMAL") // safe with WAL; fsync at checkpoints
	if writer {
		q.Set("_journal_mode", "WAL")
		q.Set("_txlock", "immediate")
	} else {
		q.Set("_query_only", "on")
	}
	return "file:" + escapePath(path) + "?" + q.Encode()
}

// escapePath escapes the characters SQLite's URI parser treats specially.
func escapePath(p string) string {
	return strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23").Replace(p)
}

// Close runs PRAGMA optimize and closes both pools.
func (s *Store) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = s.wdb.ExecContext(ctx, "PRAGMA optimize")
	var errs []error
	if s.rdb != nil {
		errs = append(errs, s.rdb.Close())
	}
	errs = append(errs, s.wdb.Close())
	return errors.Join(errs...)
}

// Path returns the database file path.
func (s *Store) Path() string { return s.path }

// Health checks both pools and returns the current schema version.
func (s *Store) Health(ctx context.Context) (int, error) {
	if err := s.wdb.PingContext(ctx); err != nil {
		return 0, fmt.Errorf("writer: %w", err)
	}
	var v sql.NullInt64
	if err := s.rdb.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&v); err != nil {
		return 0, fmt.Errorf("reader: %w", err)
	}
	return int(v.Int64), nil
}

// Optimize runs PRAGMA optimize; call it periodically on long-running servers.
func (s *Store) Optimize(ctx context.Context) error {
	_, err := s.wdb.ExecContext(ctx, "PRAGMA optimize")
	return err
}

// newClaimToken returns a random URL-safe token and its sha256 hash.
// Only the hash is stored; the token goes in the email to the student.
func newClaimToken() (token string, hash []byte) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failing is unrecoverable
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, hashToken(token)
}

func hashToken(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

const timeLayout = "2006-01-02T15:04:05Z"
const dateLayout = "2006-01-02"

func nowUTC() string { return time.Now().UTC().Format(timeLayout) }

func parseTime(s sql.NullString) *time.Time {
	if !s.Valid || s.String == "" {
		return nil
	}
	for _, layout := range []string{timeLayout, time.RFC3339, dateLayout} {
		if t, err := time.Parse(layout, s.String); err == nil {
			return &t
		}
	}
	return nil
}
