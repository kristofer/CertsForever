package store

import "context"

// EventKind is a tracked interaction with a certificate.
type EventKind string

const (
	EventView          EventKind = "view"           // human opened the public page
	EventOGImage       EventKind = "og_image"       // a social network fetched the preview image
	EventLinkedInAdd   EventKind = "linkedin_add"   // student clicked "Add to profile"
	EventLinkedInShare EventKind = "linkedin_share" // someone clicked "Share on LinkedIn"
	EventLearnMore     EventKind = "learn_more"     // visitor clicked through to the program site
)

// RecordEvent stores one event. The client is taken from the certificate.
// Failures are the caller's to log; analytics should never break a page view.
func (s *Store) RecordEvent(ctx context.Context, certID string, kind EventKind, referrerHost string) error {
	res, err := s.wdb.ExecContext(ctx, `
		INSERT INTO events (client_id, certificate_id, kind, referrer_host)
		SELECT client_id, id, ?, NULLIF(?, '') FROM certificates WHERE id = ?`,
		string(kind), referrerHost, certID)
	return affectedOne(res, err)
}

// CourseStats summarizes certificates and traffic per course.
type CourseStats struct {
	Course        string `json:"course"`
	Certificates  int    `json:"certificates"`
	Public        int    `json:"public"`
	Views         int    `json:"views"`
	PreviewFetch  int    `json:"preview_fetches"`
	LinkedInAdds  int    `json:"linkedin_adds"`
	Shares        int    `json:"linkedin_shares"`
	LearnMoreClks int    `json:"learn_more_clicks"`
}

// Stats returns per-course totals for the scoped client.
func (s *Store) Stats(ctx context.Context, sc Scope) ([]CourseStats, error) {
	clientID, err := sc.id()
	if err != nil {
		return nil, err
	}
	rows, err := s.rdb.QueryContext(ctx, `
		WITH ev AS (
			SELECT certificate_id,
			       SUM(kind = 'view')           AS views,
			       SUM(kind = 'og_image')       AS og,
			       SUM(kind = 'linkedin_add')   AS adds,
			       SUM(kind = 'linkedin_share') AS shares,
			       SUM(kind = 'learn_more')     AS learn
			FROM events WHERE client_id = ?1 GROUP BY certificate_id
		)
		SELECT co.slug,
		       COUNT(c.id),
		       COUNT(CASE WHEN c.visibility = 'public' THEN 1 END),
		       COALESCE(SUM(ev.views), 0), COALESCE(SUM(ev.og), 0),
		       COALESCE(SUM(ev.adds), 0), COALESCE(SUM(ev.shares), 0),
		       COALESCE(SUM(ev.learn), 0)
		FROM courses co
		JOIN certificates c ON c.course_id = co.id
		LEFT JOIN ev ON ev.certificate_id = c.id
		WHERE co.client_id = ?1
		GROUP BY co.slug ORDER BY co.slug`, clientID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CourseStats{}
	for rows.Next() {
		var cs CourseStats
		if err := rows.Scan(&cs.Course, &cs.Certificates, &cs.Public, &cs.Views, &cs.PreviewFetch,
			&cs.LinkedInAdds, &cs.Shares, &cs.LearnMoreClks); err != nil {
			return nil, err
		}
		out = append(out, cs)
	}
	return out, rows.Err()
}
