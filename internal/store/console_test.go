package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDesignSnapshotFrozenAtIssue(t *testing.T) {
	ctx := context.Background()
	x := twoClients(t)
	s := x.s

	// Certificates issued before a design exists render the default look.
	c, err := s.GetCertificate(ctx, x.aCert.CertificateID)
	if err != nil {
		t.Fatal(err)
	}
	if c.Design.Heading != DefaultDesign().Heading || c.Design.AccentColor != "#1f8f81" {
		t.Fatalf("default design: %+v", c.Design)
	}

	logo, err := s.SaveAsset(ctx, x.a, "logo", []byte("\x89PNG fake"), 10, 10)
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.SaveDesign(ctx, x.a, Design{Name: "Bootcamp", Heading: "Certificate of Achievement",
		BodyText: "completed", AccentColor: "#AA3300", LogoAssetID: logo,
		Signatories: []Signatory{{Name: "Kris Younger", Title: "Lead Instructor"}, {}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetCourseDesign(ctx, x.a, "java", id); err != nil {
		t.Fatal(err)
	}
	res, err := s.Issue(ctx, x.a, []IssueRequest{{Email: "grace@example.com", FullName: "Grace",
		CourseSlug: "java", Cohort: "C2", CompletedOn: time.Now()}})
	if err != nil {
		t.Fatal(err)
	}
	// Editing the design afterwards doesn't change the issued certificate.
	if _, err := s.SaveDesign(ctx, x.a, Design{ID: id, Name: "Bootcamp", Heading: "Changed", AccentColor: "#000000"}); err != nil {
		t.Fatal(err)
	}
	c, _ = s.GetCertificate(ctx, res[0].CertificateID)
	if c.Design.Heading != "Certificate of Achievement" || c.Design.AccentColor != "#aa3300" ||
		c.Design.LogoAssetID != logo || len(c.Design.Signatories) != 1 || c.Design.DesignID != id {
		t.Fatalf("snapshot: %+v", c.Design)
	}
	// The design in use can't be deleted.
	if err := s.DeleteDesign(ctx, x.a, id); !errors.Is(err, ErrConflict) {
		t.Fatalf("delete in-use design: %v", err)
	}
}

func TestDesignsAndAssetsAreClientScoped(t *testing.T) {
	ctx := context.Background()
	x := twoClients(t)
	s := x.s
	bLogo, _ := s.SaveAsset(ctx, x.b, "logo", []byte("png"), 1, 1)
	// A's design can't use B's logo.
	if _, err := s.SaveDesign(ctx, x.a, Design{Name: "D", Heading: "H", AccentColor: "#123456", LogoAssetID: bLogo}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("cross-client logo: %v", err)
	}
	if _, err := s.SaveDesign(ctx, x.a, Design{Name: "D", Heading: "H", AccentColor: "#123456",
		Signatories: []Signatory{{Name: "X", SignatureAssetID: bLogo}}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("cross-client signature: %v", err)
	}
	bDesign, err := s.SaveDesign(ctx, x.b, Design{Name: "B design", Heading: "H", AccentColor: "#123456"})
	if err != nil {
		t.Fatal(err)
	}
	// A can't see, edit, delete or use B's design.
	if _, err := s.GetDesign(ctx, x.a, bDesign); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get: %v", err)
	}
	if _, err := s.SaveDesign(ctx, x.a, Design{ID: bDesign, Name: "pwn", Heading: "H", AccentColor: "#123456"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update: %v", err)
	}
	if err := s.DeleteDesign(ctx, x.a, bDesign); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete: %v", err)
	}
	if err := s.SetCourseDesign(ctx, x.a, "java", bDesign); !errors.Is(err, ErrInvalid) {
		t.Fatalf("use: %v", err)
	}
	// Even bypassing the store, the trigger refuses a cross-client design.
	if _, err := s.wdb.Exec(`UPDATE courses SET design_id = ? WHERE slug = 'java' AND client_id = ?`, bDesign, x.a.client.ID); err == nil {
		t.Fatal("trigger allowed a cross-client design")
	}
	if list, _ := s.ListDesigns(ctx, x.a); len(list) != 0 {
		t.Fatalf("A sees designs: %+v", list)
	}
	// Validation.
	for _, d := range []Design{
		{Name: "", Heading: "H", AccentColor: "#123456"},
		{Name: "n", Heading: "H", AccentColor: "red"},
		{Name: "n", Heading: "H", AccentColor: "#123456", Signatories: []Signatory{{Name: "a"}, {Name: "b"}, {Name: "c"}}},
	} {
		if _, err := s.SaveDesign(ctx, x.a, d); !errors.Is(err, ErrInvalid) {
			t.Fatalf("validate %+v: %v", d, err)
		}
	}
	if _, err := s.SaveDesign(ctx, x.b, Design{Name: "B design", Heading: "H", AccentColor: "#123456"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate name: %v", err)
	}
}

func TestCorrectName(t *testing.T) {
	ctx := context.Background()
	x := twoClients(t)
	s := x.s
	id := x.aCert.CertificateID
	if _, err := s.CorrectName(ctx, x.b, id, "Pwned"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-client correction: %v", err)
	}
	old, err := s.CorrectName(ctx, x.a, id, "  Ada   Lovelace ")
	if err != nil || old != "Ada at ZCW" {
		t.Fatalf("correct: %q %v", old, err)
	}
	c, _ := s.GetCertificate(ctx, id)
	if c.RecipientName != "Ada Lovelace" || c.NameCorrectedAt == nil {
		t.Fatalf("after: %+v", c)
	}
	if _, err := s.CorrectName(ctx, x.a, id, "Ada Lovelace"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("same name: %v", err)
	}
	_ = s.Revoke(ctx, x.a, id, "test")
	if _, err := s.CorrectName(ctx, x.a, id, "Someone"); !errors.Is(err, ErrConflict) {
		t.Fatalf("revoked: %v", err)
	}
}

func TestPreviewIssue(t *testing.T) {
	ctx := context.Background()
	x := twoClients(t)
	s := x.s
	_ = s.Suppress(ctx, "bounced@example.com", "test", "manual")
	now := time.Now()
	p, err := s.PreviewIssue(ctx, x.a, []IssueRequest{
		{Email: "new@example.com", FullName: "New", CourseSlug: "java", Cohort: "C1", CompletedOn: now},
		{Email: "SAME@example.com", FullName: "Ada at ZCW", CourseSlug: "JAVA", Cohort: "C1", CompletedOn: now},
		{Email: "new@example.com", FullName: "New", CourseSlug: "java", Cohort: "C1", CompletedOn: now},
		{Email: "x@example.com", FullName: "X", CourseSlug: "b-only", Cohort: "C1", CompletedOn: now}, // TWA's course
		{Email: "", FullName: "Y", CourseSlug: "java", Cohort: "C1", CompletedOn: now},
		{Email: "bounced@example.com", FullName: "B", CourseSlug: "java", Cohort: "C1", CompletedOn: now},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"new", "existing", "duplicate", "error", "error", "new"}
	for i, w := range want {
		if p.Rows[i].Action != w {
			t.Fatalf("row %d: %+v, want %s", i+1, p.Rows[i], w)
		}
	}
	if p.New != 2 || p.Existing != 1 || p.Duplicate != 1 || p.Errors != 2 || p.OK() {
		t.Fatalf("counts: %+v", p)
	}
	if !p.Rows[5].Suppressed || p.Rows[0].Suppressed {
		t.Fatal("suppression flag")
	}
	if p.Rows[1].CertificateID != x.aCert.CertificateID {
		t.Fatalf("existing id: %+v", p.Rows[1])
	}
	// Another client's certificate for the same person/course/cohort names
	// doesn't make the row "existing".
	if _, err := s.Issue(ctx, x.b, []IssueRequest{{Email: "onlyb@example.com", FullName: "B", CourseSlug: "java",
		Cohort: "C9", CompletedOn: now}}); err != nil {
		t.Fatal(err)
	}
	p2, _ := s.PreviewIssue(ctx, x.a, []IssueRequest{{Email: "onlyb@example.com", FullName: "B", CourseSlug: "java",
		Cohort: "C9", CompletedOn: now}})
	if p2.Rows[0].Action != "new" || p2.Rows[0].CertificateID != "" {
		t.Fatalf("preview saw another client's certificate: %+v", p2.Rows[0])
	}
	// Nothing was written.
	if certs, total, _ := s.SearchCertificates(ctx, x.a, CertFilter{}); total != 1 || len(certs) != 1 {
		t.Fatalf("preview wrote: %d", total)
	}
}

func TestSearchCohortsOverview(t *testing.T) {
	ctx := context.Background()
	x := twoClients(t)
	s := x.s
	if _, err := s.Issue(ctx, x.a, []IssueRequest{
		{Email: "grace@example.com", FullName: "Grace Hopper", CourseSlug: "java", Cohort: "C2", CompletedOn: time.Now()},
		{Email: "alan@example.com", FullName: "Alan 100%_", CourseSlug: "java", Cohort: "C2", CompletedOn: time.Now()},
	}); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		f    CertFilter
		want int
	}{
		{CertFilter{}, 3},
		{CertFilter{Cohort: "C2"}, 2},
		{CertFilter{Query: "hopper"}, 1},
		{CertFilter{Query: "%"}, 1}, // LIKE wildcards are literal
		{CertFilter{Query: "_"}, 1},
		{CertFilter{Query: x.aCert.CertificateID}, 1},
		{CertFilter{Query: x.bCert.CertificateID}, 0}, // other client's
		{CertFilter{Claimed: "yes"}, 0},
		{CertFilter{Status: "revoked"}, 0},
	}
	for _, c := range cases {
		if _, n, err := s.SearchCertificates(ctx, x.a, c.f); err != nil || n != c.want {
			t.Errorf("%+v: %d %v, want %d", c.f, n, err, c.want)
		}
	}
	page, total, _ := s.SearchCertificates(ctx, x.a, CertFilter{Limit: 2})
	if len(page) != 2 || total != 3 {
		t.Fatalf("page: %d of %d", len(page), total)
	}
	cohorts, err := s.ListCohorts(ctx, x.a)
	if err != nil || len(cohorts) != 2 {
		t.Fatalf("cohorts: %+v %v", cohorts, err)
	}
	for _, c := range cohorts {
		if c.Name == "C2" && (c.Total != 2 || c.Unclaimed != 2) {
			t.Fatalf("C2: %+v", c)
		}
	}
	o, err := s.GetOverview(ctx, x.a)
	if err != nil || o.Certificates != 3 || o.Courses != 1 {
		t.Fatalf("overview: %+v %v", o, err)
	}
	if _, err := s.GetOverview(ctx, Scope{}); !errors.Is(err, ErrNoScope) {
		t.Fatal("zero scope")
	}
}

func TestCourseDelete(t *testing.T) {
	ctx := context.Background()
	x := twoClients(t)
	s := x.s
	if err := s.DeleteCourse(ctx, x.a, "java"); !errors.Is(err, ErrConflict) {
		t.Fatalf("delete used course: %v", err)
	}
	if err := s.DeleteCourse(ctx, x.a, "b-only"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete other client's course: %v", err)
	}
	if err := s.DeleteCourse(ctx, x.b, "b-only"); err != nil {
		t.Fatal(err)
	}
}

func TestAPITokens(t *testing.T) {
	ctx := context.Background()
	x := twoClients(t)
	s := x.s
	tok, meta, err := s.CreateAPIToken(ctx, x.a, "LMS", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) < 40 || meta.Prefix != tok[:10] {
		t.Fatalf("token %q %+v", tok, meta)
	}
	sc, _, err := s.ScopeForAPIToken(ctx, tok)
	if err != nil || sc.Client().Slug != "zcw" {
		t.Fatalf("auth: %v", err)
	}
	if _, _, err := s.ScopeForAPIToken(ctx, tok+"x"); !errors.Is(err, ErrNotFound) {
		t.Fatal("wrong token accepted")
	}
	if _, err := s.RevokeAPIToken(ctx, x.b, meta.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("B revoked A's token")
	}
	if list, _ := s.ListAPITokens(ctx, x.b); len(list) != 0 {
		t.Fatal("B sees A's tokens")
	}
	if _, err := s.RevokeAPIToken(ctx, x.a, meta.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ScopeForAPIToken(ctx, tok); !errors.Is(err, ErrNotFound) {
		t.Fatal("revoked token accepted")
	}
}
