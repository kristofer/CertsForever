// Package importer parses cohort completion CSVs.
//
// Expected header (column order doesn't matter, extra columns are ignored):
//
//	email,full_name,course,cohort,completed_on
//
// completed_on is YYYY-MM-DD; course is a course slug.
package importer

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"certsforever/internal/store"
)

var required = []string{"email", "full_name", "course", "cohort", "completed_on"}

// Parse reads a CSV and returns issue requests. It reports every bad row,
// not just the first, so a whole file can be fixed in one pass.
func Parse(r io.Reader) ([]store.IssueRequest, error) {
	cr := csv.NewReader(r)
	cr.TrimLeadingSpace = true
	header, err := cr.Read()
	if err != nil {
		return nil, fmt.Errorf("read header: %w", err)
	}
	col := map[string]int{}
	for i, h := range header {
		col[strings.ToLower(strings.TrimSpace(strings.TrimPrefix(h, "\uFEFF")))] = i
	}
	for _, name := range required {
		if _, ok := col[name]; !ok {
			return nil, fmt.Errorf("missing column %q (need %s)", name, strings.Join(required, ","))
		}
	}

	var reqs []store.IssueRequest
	var errs []error
	line := 1
	for {
		rec, err := cr.Read()
		line++
		if err == io.EOF {
			break
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("line %d: %w", line, err))
			continue
		}
		get := func(name string) string {
			if i := col[name]; i < len(rec) {
				return strings.TrimSpace(rec[i])
			}
			return ""
		}
		if strings.Join(rec, "") == "" {
			continue // blank line
		}
		d, err := time.Parse("2006-01-02", get("completed_on"))
		if err != nil {
			errs = append(errs, fmt.Errorf("line %d: completed_on %q is not YYYY-MM-DD", line, get("completed_on")))
			continue
		}
		req := store.IssueRequest{
			Email:       get("email"),
			FullName:    get("full_name"),
			CourseSlug:  get("course"),
			Cohort:      get("cohort"),
			CompletedOn: d,
		}
		if req.Email == "" || !strings.Contains(req.Email, "@") || req.FullName == "" ||
			req.CourseSlug == "" || req.Cohort == "" {
			errs = append(errs, fmt.Errorf("line %d: email, full_name, course and cohort are required", line))
			continue
		}
		reqs = append(reqs, req)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	if len(reqs) == 0 {
		return nil, errors.New("no rows")
	}
	return reqs, nil
}
