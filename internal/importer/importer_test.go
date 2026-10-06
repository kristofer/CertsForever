package importer

import (
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	in := "\uFEFFEmail, Full_Name ,course,cohort,completed_on,notes\n" +
		"ada@example.com,Ada Lovelace,java,J1,2026-09-30,extra column ignored\n" +
		"\n" +
		"alan@example.com,Alan Turing,java,J1,2026-09-30,\n"
	reqs, err := Parse(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(reqs) != 2 || reqs[0].FullName != "Ada Lovelace" || reqs[1].CompletedOn.Day() != 30 {
		t.Fatalf("got %+v", reqs)
	}
}

func TestParseReportsAllBadRows(t *testing.T) {
	in := "email,full_name,course,cohort,completed_on\n" +
		"ada@example.com,Ada,java,J1,09/30/2026\n" +
		"not-an-email,Bob,java,J1,2026-09-30\n" +
		"ok@example.com,Ok,java,J1,2026-09-30\n"
	_, err := Parse(strings.NewReader(in))
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"line 2", "YYYY-MM-DD", "line 3"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
}

func TestParseMissingColumn(t *testing.T) {
	_, err := Parse(strings.NewReader("email,full_name,course\nx@y.z,X,java\n"))
	if err == nil || !strings.Contains(err.Error(), `missing column "cohort"`) {
		t.Fatalf("err = %v", err)
	}
}

func TestParseEmpty(t *testing.T) {
	if _, err := Parse(strings.NewReader("email,full_name,course,cohort,completed_on\n")); err == nil {
		t.Fatal("header-only file should fail")
	}
}
