package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Course is a program a certificate can be issued for.
type Course struct {
	ID          int64    `json:"id"`
	Slug        string   `json:"slug"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Skills      []string `json:"skills"`
	Hours       int      `json:"hours,omitempty"`
}

// CreateCourse inserts a course, or updates the title/description/skills of
// an existing course with the same slug. Already-issued certificates are not
// affected (they keep their snapshot).
func (s *Store) CreateCourse(ctx context.Context, c Course) (int64, error) {
	c.Slug = strings.TrimSpace(strings.ToLower(c.Slug))
	if c.Slug == "" || strings.TrimSpace(c.Title) == "" {
		return 0, errors.New("course slug and title are required")
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
		INSERT INTO courses (slug, title, description, skills, hours)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(slug) DO UPDATE SET
			title = excluded.title,
			description = excluded.description,
			skills = excluded.skills,
			hours = excluded.hours
		RETURNING id`, c.Slug, c.Title, c.Description, string(skills), hours).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("create course: %w", err)
	}
	return id, nil
}

// GetCourse looks up a course by slug.
func (s *Store) GetCourse(ctx context.Context, slug string) (*Course, error) {
	var c Course
	var skills string
	var hours sql.NullInt64
	err := s.rdb.QueryRowContext(ctx,
		`SELECT id, slug, title, description, skills, hours FROM courses WHERE slug = ?`,
		strings.ToLower(slug)).Scan(&c.ID, &c.Slug, &c.Title, &c.Description, &skills, &hours)
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
