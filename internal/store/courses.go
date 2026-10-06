package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Course is a program a client issues certificates for.
type Course struct {
	ID          int64    `json:"id"`
	Slug        string   `json:"slug"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Skills      []string `json:"skills"`
	Hours       int      `json:"hours,omitempty"`
}

// CreateCourse inserts a course for the scoped client, or updates the
// title/description/skills of its existing course with the same slug.
// Already-issued certificates are not affected (they keep their snapshot).
func (s *Store) CreateCourse(ctx context.Context, sc Scope, c Course) (int64, error) {
	clientID, err := sc.id()
	if err != nil {
		return 0, err
	}
	c.Slug = strings.TrimSpace(strings.ToLower(c.Slug))
	if !slugRE.MatchString(c.Slug) || strings.TrimSpace(c.Title) == "" {
		return 0, fmt.Errorf("%w: course needs a slug (2–40 lowercase letters, digits, dashes) and a title", ErrInvalid)
	}
	skills, err := json.Marshal(cleanSkills(c.Skills))
	if err != nil {
		return 0, err
	}
	var hours sql.NullInt64
	if c.Hours > 0 {
		hours = sql.NullInt64{Int64: int64(c.Hours), Valid: true}
	}
	var id int64
	err = s.wdb.QueryRowContext(ctx, `
		INSERT INTO courses (client_id, slug, title, description, skills, hours)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(client_id, slug) DO UPDATE SET
			title = excluded.title,
			description = excluded.description,
			skills = excluded.skills,
			hours = excluded.hours
		RETURNING id`, clientID, c.Slug, c.Title, c.Description, string(skills), hours).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("create course: %w", err)
	}
	return id, nil
}

const courseSelect = `SELECT id, slug, title, description, skills, hours FROM courses`

// GetCourse looks up one of the scoped client's courses by slug.
func (s *Store) GetCourse(ctx context.Context, sc Scope, slug string) (*Course, error) {
	clientID, err := sc.id()
	if err != nil {
		return nil, err
	}
	return scanCourse(s.rdb.QueryRowContext(ctx, courseSelect+` WHERE client_id = ? AND slug = ?`,
		clientID, strings.ToLower(slug)))
}

// ListCourses returns the scoped client's courses.
func (s *Store) ListCourses(ctx context.Context, sc Scope) ([]Course, error) {
	clientID, err := sc.id()
	if err != nil {
		return nil, err
	}
	rows, err := s.rdb.QueryContext(ctx, courseSelect+` WHERE client_id = ? ORDER BY slug`, clientID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Course{}
	for rows.Next() {
		c, err := scanCourse(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

func scanCourse(r rowScanner) (*Course, error) {
	var c Course
	var skills string
	var hours sql.NullInt64
	err := r.Scan(&c.ID, &c.Slug, &c.Title, &c.Description, &skills, &hours)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	_ = json.Unmarshal([]byte(skills), &c.Skills)
	c.Hours = int(hours.Int64)
	return &c, nil
}

func cleanSkills(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
