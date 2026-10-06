package store

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

// twoClients sets up ZCW and TWA, each with a course called "java" (slugs
// are per client) and one certificate for the same student email.
type tenants struct {
	s            *Store
	a, b         Scope
	aCert, bCert IssueResult
}

func twoClients(t *testing.T) tenants {
	t.Helper()
	ctx := context.Background()
	s, _ := openTest(t)
	a := newClient(t, s, "zcw", "ZCW")
	b := newClient(t, s, "twa", "TWA")
	for _, x := range []struct {
		sc    Scope
		title string
	}{{a, "ZCW Java"}, {b, "TWA Java"}} {
		if _, err := s.CreateCourse(ctx, x.sc, Course{Slug: "java", Title: x.title}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.CreateCourse(ctx, b, Course{Slug: "b-only", Title: "Only at TWA"}); err != nil {
		t.Fatal(err)
	}
	issue := func(sc Scope, name string) IssueResult {
		res, err := s.Issue(ctx, sc, []IssueRequest{{Email: "same@example.com", FullName: name,
			CourseSlug: "java", Cohort: "C1", CompletedOn: time.Now()}})
		if err != nil {
			t.Fatal(err)
		}
		return res[0]
	}
	return tenants{s: s, a: a, b: b, aCert: issue(a, "Ada at ZCW"), bCert: issue(b, "Ada at TWA")}
}

func TestTenantIsolationThroughStoreAPI(t *testing.T) {
	ctx := context.Background()
	x := twoClients(t)
	s := x.s

	if !strings.HasPrefix(x.aCert.CertificateID, "ZCW-") || !strings.HasPrefix(x.bCert.CertificateID, "TWA-") {
		t.Fatalf("prefixes: %s %s", x.aCert.CertificateID, x.bCert.CertificateID)
	}

	// Same course slug resolves to each client's own course.
	ca, _ := s.GetCourse(ctx, x.a, "java")
	cb, _ := s.GetCourse(ctx, x.b, "java")
	if ca == nil || cb == nil || ca.Title != "ZCW Java" || cb.Title != "TWA Java" || ca.ID == cb.ID {
		t.Fatalf("courses: %+v %+v", ca, cb)
	}
	if _, err := s.GetCourse(ctx, x.a, "b-only"); !errors.Is(err, ErrNotFound) {
		t.Errorf("A can read B's course: %v", err)
	}
	if list, _ := s.ListCourses(ctx, x.a); len(list) != 1 {
		t.Errorf("A lists %d courses, want 1", len(list))
	}

	// A cannot see, revoke or re-key B's certificate.
	if _, err := s.GetClientCertificate(ctx, x.a, x.bCert.CertificateID); !errors.Is(err, ErrNotFound) {
		t.Errorf("A read B's certificate: %v", err)
	}
	if err := s.Revoke(ctx, x.a, x.bCert.CertificateID, "attack"); !errors.Is(err, ErrNotFound) {
		t.Errorf("A revoked B's certificate: %v", err)
	}
	if _, err := s.NewClaimLink(ctx, x.a, x.bCert.CertificateID); !errors.Is(err, ErrNotFound) {
		t.Errorf("A re-keyed B's certificate: %v", err)
	}
	bc, err := s.GetCertificateByClaimToken(ctx, x.bCert.ClaimToken)
	if err != nil || bc.Revoked() || bc.Issuer.Slug != "twa" {
		t.Fatalf("B's certificate was affected: %+v %v", bc, err)
	}

	// Listing and stats only show the scoped client's data.
	la, _ := s.ListCertificates(ctx, x.a, "", "")
	if len(la) != 1 || la[0].ID != x.aCert.CertificateID {
		t.Errorf("A's list: %+v", la)
	}
	s.RecordEvent(ctx, x.bCert.CertificateID, EventView, "")
	sa, _ := s.Stats(ctx, x.a)
	if len(sa) != 1 || sa[0].Certificates != 1 || sa[0].Views != 0 {
		t.Errorf("A's stats include B's data: %+v", sa)
	}

	// A can't issue against B's course, even by naming it in a CSV.
	if _, err := s.Issue(ctx, x.a, []IssueRequest{{Email: "x@example.com", FullName: "X",
		CourseSlug: "b-only", Cohort: "C1", CompletedOn: time.Now()}}); !errors.Is(err, ErrInvalid) {
		t.Errorf("A issued for B's course: %v", err)
	}

	// The same email is a separate student per client.
	if bc.RecipientName != "Ada at TWA" {
		t.Errorf("B's recipient overwritten: %q", bc.RecipientName)
	}
}

func TestZeroScopeIsRejected(t *testing.T) {
	ctx := context.Background()
	x := twoClients(t)
	s, z := x.s, Scope{}
	checks := map[string]error{}
	_, checks["CreateCourse"] = s.CreateCourse(ctx, z, Course{Slug: "x", Title: "x"})
	_, checks["GetCourse"] = s.GetCourse(ctx, z, "java")
	_, checks["ListCourses"] = s.ListCourses(ctx, z)
	_, checks["GetClientCertificate"] = s.GetClientCertificate(ctx, z, x.aCert.CertificateID)
	_, checks["ListCertificates"] = s.ListCertificates(ctx, z, "", "")
	checks["Revoke"] = s.Revoke(ctx, z, x.aCert.CertificateID, "")
	_, checks["NewClaimLink"] = s.NewClaimLink(ctx, z, x.aCert.CertificateID)
	_, checks["Issue"] = s.Issue(ctx, z, nil)
	_, checks["Stats"] = s.Stats(ctx, z)
	for name, err := range checks {
		if !errors.Is(err, ErrNoScope) {
			t.Errorf("%s with zero Scope: %v (want ErrNoScope)", name, err)
		}
	}
}

// The schema itself must refuse cross-tenant rows, even from raw SQL that
// bypasses the store API.
func TestSchemaRejectsCrossTenantRows(t *testing.T) {
	x := twoClients(t)
	db := x.s.wdb
	ids := func(q string, args ...any) (a, b int64) {
		t.Helper()
		if err := db.QueryRow(q, args...).Scan(&a, &b); err != nil {
			t.Fatal(err)
		}
		return
	}
	aClient, bClient := ids(`SELECT (SELECT id FROM clients WHERE slug='zcw'), (SELECT id FROM clients WHERE slug='twa')`)
	aStudent, bCourse := ids(`SELECT (SELECT id FROM students WHERE client_id=?), (SELECT id FROM courses WHERE client_id=? AND slug='java')`, aClient, bClient)
	_, aCourse := ids(`SELECT id, course_id FROM cohorts WHERE client_id=?`, aClient)

	// Control first: a correct row is accepted, so failures below aren't noise.
	// Its fresh cohort also keeps the certificate attempts clear of the
	// (student, cohort) uniqueness rule.
	res, err := db.Exec(`INSERT INTO cohorts (client_id, course_id, name) VALUES (?, ?, 'fresh')`, aClient, aCourse)
	if err != nil {
		t.Fatalf("legitimate cohort rejected: %v", err)
	}
	aCohort, _ := res.LastInsertId()
	certInsert := func(id string, course int64) string {
		return `INSERT INTO certificates (id, client_id, student_id, course_id, cohort_id, recipient_name, course_title, issued_on)
			VALUES ('` + id + `', ` + itoa(aClient) + `, ` + itoa(aStudent) + `, ` + itoa(course) + `, ` + itoa(aCohort) + `, 'x', 'x', '2026-01-01')`
	}
	if _, err := db.Exec(certInsert("ZCW-CCCCCCCCCC", aCourse)); err != nil {
		t.Fatalf("legitimate certificate rejected: %v", err)
	}
	db.Exec(`DELETE FROM certificates WHERE id = 'ZCW-CCCCCCCCCC'`)

	attempts := []struct{ name, sql, wantErr string }{
		{"cert with another client's course", certInsert("ZCW-AAAAAAAAAA", bCourse), "FOREIGN KEY"},
		{"cohort under another client's course",
			`INSERT INTO cohorts (client_id, course_id, name) VALUES (` + itoa(aClient) + `, ` + itoa(bCourse) + `, 'sneaky')`, "FOREIGN KEY"},
		{"event for another client's cert",
			`INSERT INTO events (client_id, certificate_id, kind) VALUES (` + itoa(aClient) + `, '` + x.bCert.CertificateID + `', 'view')`, "FOREIGN KEY"},
		{"cert with the wrong prefix", certInsert("TWA-BBBBBBBBBB", aCourse), "id_prefix"},
		{"moving a cert to another client",
			`UPDATE certificates SET client_id = ` + itoa(bClient) + ` WHERE id = '` + x.aCert.CertificateID + `'`, "immutable"},
		{"deleting a client with certificates", `DELETE FROM clients WHERE id = ` + itoa(aClient), "FOREIGN KEY"},
	}
	for _, a := range attempts {
		_, err := db.Exec(a.sql)
		if err == nil {
			t.Errorf("schema allowed: %s", a.name)
		} else if !strings.Contains(err.Error(), a.wantErr) {
			t.Errorf("%s: failed for the wrong reason: %v (want %q)", a.name, err, a.wantErr)
		}
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestClientRules(t *testing.T) {
	ctx := context.Background()
	s, _ := openTest(t)
	bad := []ClientInput{
		{Slug: "X", Name: ptr("x"), IDPrefix: ptr("XX")},        // slug too short
		{Slug: "good-slug", Name: ptr(""), IDPrefix: ptr("GS")}, // no name
		{Slug: "api", Name: ptr("API"), IDPrefix: ptr("API")},   // reserved
		{Slug: "good-slug", Name: ptr("G"), IDPrefix: ptr("G1")},
		{Slug: "good-slug", Name: ptr("G"), IDPrefix: ptr("GS"), SiteURL: ptr("example.org")},
		{Slug: "good-slug", Name: ptr("G"), IDPrefix: ptr("GS"), LinkedInOrgID: ptr("acme")},
	}
	for _, in := range bad {
		if _, err := s.CreateClient(ctx, in); !errors.Is(err, ErrInvalid) {
			t.Errorf("CreateClient(%+v) = %v, want ErrInvalid", in, err)
		}
	}
	c, err := s.CreateClient(ctx, ClientInput{Slug: "Zip-Code", Name: ptr("Zip Code"), IDPrefix: ptr("zcw"),
		SiteURL: ptr("https://zipcodewilmington.com"), LinkedInOrgID: ptr("123")})
	if err != nil || c.Slug != "zip-code" || c.IDPrefix != "ZCW" || !c.Active() {
		t.Fatalf("create normalized: %+v %v", c, err)
	}
	if _, err := s.CreateClient(ctx, ClientInput{Slug: "zip-code", Name: ptr("dup"), IDPrefix: ptr("DUP")}); !errors.Is(err, ErrConflict) {
		t.Errorf("duplicate slug: %v", err)
	}
	if _, err := s.CreateClient(ctx, ClientInput{Slug: "other", Name: ptr("dup"), IDPrefix: ptr("ZCW")}); !errors.Is(err, ErrConflict) {
		t.Errorf("duplicate prefix: %v", err)
	}

	// Prefix can change until the first certificate, then it's frozen.
	if c, err = s.UpdateClient(ctx, ClientInput{Slug: "zip-code", IDPrefix: ptr("ZC")}); err != nil || c.IDPrefix != "ZC" {
		t.Fatalf("prefix change before issuing: %+v %v", c, err)
	}
	sc, _ := s.Scope(ctx, "zip-code")
	s.CreateCourse(ctx, sc, Course{Slug: "java", Title: "Java"})
	if _, err := s.Issue(ctx, sc, []IssueRequest{{Email: "a@b.c", FullName: "A", CourseSlug: "java", Cohort: "1", CompletedOn: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateClient(ctx, ClientInput{Slug: "zip-code", IDPrefix: ptr("ZCW")}); !errors.Is(err, ErrConflict) {
		t.Errorf("prefix change after issuing: %v", err)
	}
	if c, err = s.UpdateClient(ctx, ClientInput{Slug: "zip-code", Name: ptr("Zip Code Wilmington")}); err != nil || c.Name != "Zip Code Wilmington" || c.IDPrefix != "ZC" {
		t.Errorf("partial update: %+v %v", c, err)
	}
	if _, err := s.UpdateClient(ctx, ClientInput{Slug: "nope", Name: ptr("x")}); !errors.Is(err, ErrNotFound) {
		t.Errorf("update missing client: %v", err)
	}

	// Suspension blocks issuing; certificates stay readable.
	if err := s.SetClientStatus(ctx, "zip-code", "suspended"); err != nil {
		t.Fatal(err)
	}
	sc, _ = s.Scope(ctx, "zip-code")
	if _, err := s.Issue(ctx, sc, []IssueRequest{{Email: "b@b.c", FullName: "B", CourseSlug: "java", Cohort: "1", CompletedOn: time.Now()}}); !errors.Is(err, ErrSuspended) {
		t.Errorf("suspended client issued: %v", err)
	}
	if list, _ := s.ListCertificates(ctx, sc, "", ""); len(list) != 1 || list[0].Issuer.Status != "suspended" {
		t.Errorf("suspended client's certificates: %+v", list)
	}
	if err := s.SetClientStatus(ctx, "zip-code", "active"); err != nil {
		t.Fatal(err)
	}
	if c, _ := s.GetClient(ctx, "zip-code"); c.SuspendedAt != nil {
		t.Error("reactivation should clear suspended_at")
	}

	if sc, err := s.ScopeForCertificate(ctx, "ZC-0000000000"); err != nil || sc.Client().Slug != "zip-code" {
		t.Errorf("ScopeForCertificate: %+v %v", sc.Client(), err)
	}
	if list, _ := s.ListClients(ctx); len(list) != 1 {
		t.Errorf("ListClients: %d", len(list))
	}
}

// A database created before clients existed (only migration 001, data in
// it) must come through 002 with everything assigned to the "zcw" client.
func TestLegacyDataMigratesIntoZCWClient(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "certs.db")
	only001 := fstest.MapFS{}
	b, _ := embeddedMigrations.ReadFile("migrations/001_init.sql")
	only001["001_init.sql"] = &fstest.MapFile{Data: b}

	s, err := OpenWithOptions(ctx, path, Options{migrations: only001})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO courses (id, slug, title, skills) VALUES (1, 'java', 'Java Developer', '["Java"]')`,
		`INSERT INTO cohorts (id, course_id, name) VALUES (1, 1, 'J13')`,
		`INSERT INTO students (id, email, full_name) VALUES (1, 'grace@example.com', 'Grace Hopper')`,
		`INSERT INTO certificates (id, student_id, cohort_id, recipient_name, course_title, skills, issued_on, visibility, claim_token_hash)
		 VALUES ('ZCW-7K3M9QF2XA', 1, 1, 'Grace Hopper', 'Java Developer', '["Java"]', '2026-10-01', 'public', x'01')`,
		`INSERT INTO events (certificate_id, kind) VALUES ('ZCW-7K3M9QF2XA', 'view')`,
	} {
		if _, err := s.wdb.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	s.Close()

	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if len(s.Migration.Applied) == 0 || s.Migration.Applied[0] != 2 || s.Migration.Snapshot == "" {
		t.Fatalf("migration report: %+v", s.Migration)
	}
	c, err := s.GetCertificate(ctx, "ZCW-7K3M9QF2XA")
	if err != nil {
		t.Fatal(err)
	}
	if c.Issuer.Slug != "zcw" || c.Issuer.Name != "Zip Code Wilmington" || !c.Public() || c.CourseSlug != "java" {
		t.Fatalf("migrated certificate: %+v", c)
	}
	sc, _ := s.Scope(ctx, "zcw")
	if st, _ := s.Stats(ctx, sc); len(st) != 1 || st[0].Views != 1 {
		t.Errorf("events not carried over: %+v", st)
	}
	// And the migrated client keeps working: new certificates get its prefix.
	res, err := s.Issue(ctx, sc, []IssueRequest{{Email: "alan@example.com", FullName: "Alan", CourseSlug: "java", Cohort: "J14", CompletedOn: time.Now()}})
	if err != nil || !strings.HasPrefix(res[0].CertificateID, "ZCW-") {
		t.Fatalf("issue after migration: %+v %v", res, err)
	}
}

func TestFreshDatabaseHasNoClients(t *testing.T) {
	s, _ := openTest(t)
	if list, err := s.ListClients(context.Background()); err != nil || len(list) != 0 {
		t.Fatalf("fresh db clients: %v %v", list, err)
	}
}
