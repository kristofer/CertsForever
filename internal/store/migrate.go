package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Migrations are forward-only SQL files named NNN_description.sql. Each is
// applied once, in its own transaction, and its SHA-256 is recorded. On every
// start the store checks that:
//
//   - no applied migration has been edited since it ran (checksum match);
//   - the database isn't from a newer build (downgrade guard);
//
// and, before applying anything to a database that already has data, it
// writes a consistent snapshot with VACUUM INTO so a bad migration can be
// rolled back by restoring the snapshot.

type migration struct {
	version  int
	name     string
	body     string
	checksum string
}

func loadMigrations(fsys fs.FS) ([]migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, err
	}
	var out []migration
	seen := map[int]string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue
		}
		v, err := strconv.Atoi(strings.SplitN(name, "_", 2)[0])
		if err != nil || v <= 0 {
			return nil, fmt.Errorf("migration %s: name must start with a positive number", name)
		}
		if other, dup := seen[v]; dup {
			return nil, fmt.Errorf("migrations %s and %s share version %d", other, name, v)
		}
		seen[v] = name
		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(body)
		out = append(out, migration{version: v, name: name, body: string(body), checksum: hex.EncodeToString(sum[:])})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

func (s *Store) migrate(ctx context.Context, opts Options) error {
	migs, err := loadMigrations(opts.migrations)
	if err != nil {
		return err
	}
	if _, err := s.wdb.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
		checksum   TEXT)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	// Databases created by the first scaffold have no checksum column.
	var hasChecksum int
	if err := s.wdb.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info('schema_migrations') WHERE name = 'checksum'`).Scan(&hasChecksum); err != nil {
		return err
	}
	if hasChecksum == 0 {
		if _, err := s.wdb.ExecContext(ctx, `ALTER TABLE schema_migrations ADD COLUMN checksum TEXT`); err != nil {
			return fmt.Errorf("upgrade schema_migrations: %w", err)
		}
	}

	applied := map[int]sql.NullString{}
	rows, err := s.wdb.QueryContext(ctx, `SELECT version, checksum FROM schema_migrations`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var v int
		var sum sql.NullString
		if err := rows.Scan(&v, &sum); err != nil {
			rows.Close()
			return err
		}
		applied[v] = sum
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	known := map[int]migration{}
	latest := 0
	for _, m := range migs {
		known[m.version] = m
		latest = m.version
	}
	for v, sum := range applied {
		m, ok := known[v]
		if !ok {
			if v > latest {
				return fmt.Errorf("database schema is at version %d but this build only knows up to %d; "+
					"refusing to start (deploy a newer build or restore a snapshot)", v, latest)
			}
			return fmt.Errorf("database has migration %d applied, which this build does not contain", v)
		}
		switch {
		case !sum.Valid || sum.String == "":
			// Applied before checksums existed: record it now.
			if _, err := s.wdb.ExecContext(ctx,
				`UPDATE schema_migrations SET checksum = ? WHERE version = ?`, m.checksum, v); err != nil {
				return err
			}
		case sum.String != m.checksum:
			return fmt.Errorf("migration %s was modified after it was applied (checksum mismatch); "+
				"add a new migration instead of editing an applied one", m.name)
		}
	}

	var pending []migration
	for _, m := range migs {
		if _, ok := applied[m.version]; !ok {
			pending = append(pending, m)
		}
	}
	s.Migration.Version = latest
	if len(pending) == 0 {
		return nil
	}

	if len(applied) > 0 && opts.SnapshotDir != "-" {
		dir := opts.SnapshotDir
		if dir == "" {
			dir = filepath.Dir(s.path)
		}
		base := strings.TrimSuffix(filepath.Base(s.path), filepath.Ext(s.path))
		snap := filepath.Join(dir, fmt.Sprintf("%s.pre-%03d-%s.db",
			base, pending[0].version, time.Now().UTC().Format("20060102T150405Z")))
		if err := s.Backup(ctx, snap); err != nil {
			return fmt.Errorf("pre-migration snapshot: %w", err)
		}
		s.Migration.Snapshot = snap
	}

	for _, m := range pending {
		tx, err := s.wdb.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, m.body); err != nil {
			tx.Rollback()
			return fmt.Errorf("apply %s: %w", m.name, err)
		}
		// Table rebuilds can leave dangling references that SQLite only
		// reports on demand; refuse to commit them.
		if err := foreignKeyCheck(ctx, tx); err != nil {
			tx.Rollback()
			return fmt.Errorf("apply %s: %w", m.name, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, checksum) VALUES (?, ?)`, m.version, m.checksum); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit %s: %w", m.name, err)
		}
		s.Migration.Applied = append(s.Migration.Applied, m.version)
	}
	return nil
}

func foreignKeyCheck(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var problems []string
	for rows.Next() {
		var table, parent string
		var rowid sql.NullInt64
		var fkid int
		if err := rows.Scan(&table, &rowid, &parent, &fkid); err != nil {
			return err
		}
		problems = append(problems, fmt.Sprintf("%s row %d -> %s", table, rowid.Int64, parent))
		if len(problems) == 5 {
			break
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("foreign key violations: %s", strings.Join(problems, "; "))
	}
	return rows.Err()
}
