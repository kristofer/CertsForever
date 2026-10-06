package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

func openTest(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "certs.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func ptr(s string) *string { return &s }

// newClient creates a client and returns its scope.
func newClient(t *testing.T, s *Store, slug, prefix string) Scope {
	t.Helper()
	ctx := context.Background()
	if _, err := s.CreateClient(ctx, ClientInput{Slug: slug, Name: ptr(strings.ToUpper(slug) + " Academy"), IDPrefix: ptr(prefix)}); err != nil {
		t.Fatal(err)
	}
	sc, err := s.Scope(ctx, slug)
	if err != nil {
		t.Fatal(err)
	}
	return sc
}

func seed(t *testing.T, s *Store) IssueResult {
	t.Helper()
	_, r := seedScoped(t, s)
	return r
}

// seedScoped creates client "zcw" with course "java" and one certificate.
func seedScoped(t *testing.T, s *Store) (Scope, IssueResult) {
	t.Helper()
	ctx := context.Background()
	sc := newClient(t, s, "zcw", "ZCW")
	if _, err := s.CreateCourse(ctx, sc, Course{Slug: "java", Title: "Java Developer", Skills: []string{"Java", " ", "SQL"}}); err != nil {
		t.Fatal(err)
	}
	res, err := s.Issue(ctx, sc, []IssueRequest{{
		Email: "ada@example.com", FullName: "Ada Lovelace", CourseSlug: "java", Cohort: "J1",
		CompletedOn: time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return sc, res[0]
}

func pragma(t *testing.T, db *sql.DB, name string) string {
	t.Helper()
	var v string
	if err := db.QueryRow("PRAGMA " + name).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestPragmas(t *testing.T) {
	s, _ := openTest(t)
	want := map[string]string{"journal_mode": "wal", "foreign_keys": "1", "synchronous": "1", "busy_timeout": "5000"}
	for name, v := range want {
		if got := pragma(t, s.wdb, name); got != v {
			t.Errorf("writer %s = %s, want %s", name, got, v)
		}
	}
	for _, name := range []string{"foreign_keys", "synchronous", "busy_timeout"} {
		if got := pragma(t, s.rdb, name); got != want[name] {
			t.Errorf("reader %s = %s, want %s", name, got, want[name])
		}
	}
	if pragma(t, s.rdb, "query_only") != "1" {
		t.Error("reader pool should be query_only")
	}
	if _, err := s.rdb.Exec(`INSERT INTO courses (slug, title) VALUES ('x', 'y')`); err == nil {
		t.Error("reader pool accepted a write")
	}
}

func TestForeignKeysEnforced(t *testing.T) {
	s, _ := openTest(t)
	r := seed(t, s)
	if err := s.RecordEvent(context.Background(), "ZCW-NOSUCHCERT", EventView, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("event for a missing certificate: %v", err)
	}
	if _, err := s.wdb.Exec(`INSERT INTO events (client_id, certificate_id, kind) VALUES (1, 'ZCW-NOSUCHCERT', 'view')`); err == nil {
		t.Fatal("raw insert of an event for a missing certificate should violate the foreign key")
	}
	if err := s.RecordEvent(context.Background(), r.CertificateID, EventView, ""); err != nil {
		t.Fatal(err)
	}
}

func TestReopenIsIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "certs.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Migration.Applied) == 0 || s.Migration.Snapshot != "" {
		t.Fatalf("fresh db: applied=%v snapshot=%q (no snapshot expected)", s.Migration.Applied, s.Migration.Snapshot)
	}
	seed(t, s)
	s.Close()

	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if len(s.Migration.Applied) != 0 || s.Migration.Snapshot != "" {
		t.Fatalf("reopen should be a no-op: %+v", s.Migration)
	}
	v, err := s.Health(ctx)
	if err != nil || v != s.Migration.Version || v < 1 {
		t.Fatalf("Health = %d, %v (want %d)", v, err, s.Migration.Version)
	}
}

func testMigrations(extra ...string) fstest.MapFS {
	fsys := fstest.MapFS{}
	all, _ := embeddedMigrations.ReadDir("migrations")
	for _, e := range all {
		b, _ := embeddedMigrations.ReadFile("migrations/" + e.Name())
		fsys[e.Name()] = &fstest.MapFile{Data: b}
	}
	for i := 0; i+1 < len(extra); i += 2 {
		fsys[extra[i]] = &fstest.MapFile{Data: []byte(extra[i+1])}
	}
	return fsys
}

func TestPendingMigrationTakesSnapshot(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "certs.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	seed(t, s)
	s.Close()

	opts := Options{migrations: testMigrations("900_add_note.sql", `ALTER TABLE courses ADD COLUMN note TEXT;`)}
	s, err = OpenWithOptions(ctx, path, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if len(s.Migration.Applied) != 1 || s.Migration.Applied[0] != 900 {
		t.Fatalf("applied = %v", s.Migration.Applied)
	}
	if s.Migration.Snapshot == "" || !strings.Contains(filepath.Base(s.Migration.Snapshot), "certs.pre-900-") {
		t.Fatalf("snapshot = %q", s.Migration.Snapshot)
	}
	// The snapshot has the data but not the new column.
	snap, err := sql.Open("sqlite3", "file:"+s.Migration.Snapshot+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer snap.Close()
	var n int
	snap.QueryRow(`SELECT COUNT(*) FROM certificates`).Scan(&n)
	if n != 1 {
		t.Errorf("snapshot has %d certificates, want 1", n)
	}
	if _, err := snap.Exec(`SELECT note FROM courses`); err == nil {
		t.Error("snapshot should predate migration 900")
	}
}

func TestFailedMigrationRollsBack(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "certs.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	bad := Options{SnapshotDir: "-", migrations: testMigrations("900_bad.sql",
		`CREATE TABLE half_done (id INTEGER); THIS IS NOT SQL;`)}
	if _, err := OpenWithOptions(ctx, path, bad); err == nil {
		t.Fatal("expected bad migration to fail")
	}
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var n int
	s.wdb.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'half_done'`).Scan(&n)
	if n != 0 {
		t.Error("failed migration left a partial table behind")
	}
}

func TestEditedMigrationIsRejected(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "certs.db")
	s, _ := Open(ctx, path)
	s.wdb.Exec(`UPDATE schema_migrations SET checksum = 'deadbeef' WHERE version = 1`)
	s.Close()
	_, err := Open(ctx, path)
	if err == nil || !strings.Contains(err.Error(), "modified after it was applied") {
		t.Fatalf("err = %v", err)
	}
}

func TestDowngradeGuard(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "certs.db")
	s, _ := Open(ctx, path)
	s.wdb.Exec(`INSERT INTO schema_migrations (version, checksum) VALUES (999, 'x')`)
	s.Close()
	_, err := Open(ctx, path)
	if err == nil || !strings.Contains(err.Error(), "refusing to start") {
		t.Fatalf("err = %v", err)
	}
}

func TestLegacyMigrationsTableIsUpgraded(t *testing.T) {
	// Databases from the first scaffold had no checksum column.
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "certs.db")
	s, _ := Open(ctx, path)
	s.wdb.Exec(`CREATE TABLE sm_old AS SELECT version, applied_at FROM schema_migrations`)
	s.wdb.Exec(`DROP TABLE schema_migrations`)
	s.wdb.Exec(`ALTER TABLE sm_old RENAME TO schema_migrations`)
	s.Close()

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var sum sql.NullString
	s.wdb.QueryRow(`SELECT checksum FROM schema_migrations WHERE version = 1`).Scan(&sum)
	if !sum.Valid || len(sum.String) != 64 {
		t.Errorf("checksum not backfilled: %+v", sum)
	}
}

func TestBackupAndRestore(t *testing.T) {
	ctx := context.Background()
	s, path := openTest(t)
	sc, r := seedScoped(t, s)
	backup := filepath.Join(t.TempDir(), "nested", "backup.db")
	if err := s.Backup(ctx, backup); err != nil {
		t.Fatal(err)
	}
	if err := s.Backup(ctx, backup); err == nil {
		t.Error("backup over an existing file should fail")
	}
	if fi, _ := os.Stat(backup); fi.Mode().Perm() != 0o600 {
		t.Errorf("backup mode = %v, want 0600", fi.Mode().Perm())
	}

	// Change the live db after the backup, then restore.
	if err := s.Revoke(ctx, sc, r.CertificateID, "oops"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	kept, err := Restore(ctx, backup, path)
	if err != nil {
		t.Fatal(err)
	}
	if kept == "" {
		t.Error("previous database should be preserved")
	}
	s2, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	c, err := s2.GetCertificate(ctx, r.CertificateID)
	if err != nil || c.Revoked() {
		t.Fatalf("restored cert = %+v, %v (want active)", c, err)
	}

	// Garbage is refused.
	junk := filepath.Join(t.TempDir(), "junk.db")
	os.WriteFile(junk, []byte("not a database"), 0o600)
	if _, err := Restore(ctx, junk, path); err == nil {
		t.Error("restore of a non-database should fail")
	}
}

func TestConcurrentReadsAndWrites(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	sc, r := seedScoped(t, s)
	var wg sync.WaitGroup
	errs := make(chan error, 1000)
	for w := 0; w < 16; w++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				if err := s.RecordEvent(ctx, r.CertificateID, EventView, "example.com"); err != nil {
					errs <- err
				}
			}
		}()
		go func() {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				if _, err := s.GetCertificate(ctx, r.CertificateID); err != nil {
					errs <- err
				}
				if _, err := s.Stats(ctx, sc); err != nil {
					errs <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err) // e.g. "database is locked" would show up here
	}
	st, _ := s.Stats(ctx, sc)
	if len(st) != 1 || st[0].Views != 16*25 {
		t.Fatalf("views = %+v, want %d", st, 16*25)
	}
}

func TestIssueIsIdempotentAndAtomic(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	sc, first := seedScoped(t, s)
	if first.ClaimToken == "" || first.Existing {
		t.Fatalf("first issue: %+v", first)
	}
	again, err := s.Issue(ctx, sc, []IssueRequest{{
		Email: "ADA@example.com", FullName: "Ada L.", CourseSlug: "java", Cohort: "J1",
		CompletedOn: time.Now(),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !again[0].Existing || again[0].CertificateID != first.CertificateID || again[0].ClaimToken != "" {
		t.Fatalf("re-issue (email case differs) should return existing: %+v", again[0])
	}

	// A bad row anywhere rolls back the whole batch.
	_, err = s.Issue(ctx, sc, []IssueRequest{
		{Email: "alan@example.com", FullName: "Alan Turing", CourseSlug: "java", Cohort: "J1", CompletedOn: time.Now()},
		{Email: "x@example.com", FullName: "X", CourseSlug: "nope", Cohort: "J1", CompletedOn: time.Now()},
	})
	if err == nil || !strings.Contains(err.Error(), `unknown course "nope"`) {
		t.Fatalf("err = %v", err)
	}
	list, _ := s.ListCertificates(ctx, sc, "", "")
	if len(list) != 1 {
		t.Fatalf("batch was not atomic: %d certificates", len(list))
	}
	if len(list[0].Skills) != 2 {
		t.Errorf("blank skills should be dropped: %v", list[0].Skills)
	}
}

func TestLifecycleErrors(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	sc, r := seedScoped(t, s)

	if err := s.SetVisibilityByClaimToken(ctx, r.ClaimToken, "everyone"); !errors.Is(err, ErrInvalid) {
		t.Errorf("invalid visibility: %v", err)
	}
	if err := s.SetVisibilityByClaimToken(ctx, "not-a-token", "public"); !errors.Is(err, ErrNotFound) {
		t.Errorf("bad token: %v", err)
	}
	if err := s.SetVisibilityByClaimToken(ctx, r.ClaimToken, "public"); err != nil {
		t.Fatal(err)
	}
	if err := s.Revoke(ctx, sc, r.CertificateID, "x"); err != nil {
		t.Fatal(err)
	}
	if err := s.Revoke(ctx, sc, r.CertificateID, "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("double revoke: %v", err)
	}
	if err := s.SetVisibilityByClaimToken(ctx, r.ClaimToken, "private"); !errors.Is(err, ErrNotFound) {
		t.Errorf("revoked certificates' visibility must be frozen: %v", err)
	}
	c, _ := s.GetCertificateByClaimToken(ctx, r.ClaimToken)
	if c == nil || c.ID != r.CertificateID {
		t.Fatal("claim token lookup failed")
	}
	tok, err := s.NewClaimLink(ctx, sc, r.CertificateID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetCertificateByClaimToken(ctx, r.ClaimToken); !errors.Is(err, ErrNotFound) {
		t.Error("old claim token should stop working")
	}
	if _, err := s.GetCertificateByClaimToken(ctx, tok); err != nil {
		t.Error("new claim token should work")
	}
}
