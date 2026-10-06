package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// Backup writes a consistent, compacted copy of the live database to dest
// using VACUUM INTO. It is safe to run while the server is serving traffic.
// dest must not already exist.
func (s *Store) Backup(ctx context.Context, dest string) error {
	if _, err := os.Stat(dest); err == nil {
		return fmt.Errorf("backup destination %s already exists", dest)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		return err
	}
	if _, err := s.wdb.ExecContext(ctx, `VACUUM INTO ?`, dest); err != nil {
		return fmt.Errorf("vacuum into %s: %w", dest, err)
	}
	return os.Chmod(dest, 0o600)
}

// Restore replaces the database at dbPath with the backup at src.
//
// The server must be stopped first. Restore checks the backup's integrity,
// keeps the current database as dbPath + ".before-restore-<time>", removes
// stale WAL/SHM files and atomically moves the backup into place. It
// returns the path of the preserved previous database ("" if there was none).
func Restore(ctx context.Context, src, dbPath string) (string, error) {
	if err := checkIntegrity(ctx, src); err != nil {
		return "", fmt.Errorf("backup %s failed integrity check: %w", src, err)
	}

	var kept string
	if _, err := os.Stat(dbPath); err == nil {
		// Snapshot the current database first (it may have uncheckpointed WAL).
		kept = fmt.Sprintf("%s.before-restore-%s", dbPath, time.Now().UTC().Format("20060102T150405Z"))
		cur, err := sql.Open("sqlite3", dsn(dbPath, true))
		if err != nil {
			return "", err
		}
		_, err = cur.ExecContext(ctx, `VACUUM INTO ?`, kept)
		cur.Close()
		if err != nil {
			return "", fmt.Errorf("preserve current database: %w", err)
		}
	}

	tmp := dbPath + ".restore-tmp"
	if err := copyFile(src, tmp); err != nil {
		return kept, err
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Remove(dbPath + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			os.Remove(tmp)
			return kept, err
		}
	}
	if err := os.Rename(tmp, dbPath); err != nil {
		os.Remove(tmp)
		return kept, err
	}
	return kept, nil
}

func checkIntegrity(ctx context.Context, path string) error {
	if _, err := os.Stat(path); err != nil {
		return err
	}
	db, err := sql.Open("sqlite3", "file:"+escapePath(path)+"?mode=ro")
	if err != nil {
		return err
	}
	defer db.Close()
	var res string
	if err := db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&res); err != nil {
		return err
	}
	if res != "ok" {
		return errors.New(res)
	}
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE name = 'schema_migrations'`).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return errors.New("not a CertsForever database (no schema_migrations table)")
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
