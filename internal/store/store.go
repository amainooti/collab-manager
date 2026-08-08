// Package store persists projects, research briefs, drafts, and stage
// history in SQLite. It is the only package that touches the database —
// core, api, web, and mcpserver all go through here.
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"outreach/internal/core"
)

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1) // modernc.org/sqlite + WAL is fine, but keep writes serialized
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
PRAGMA journal_mode=WAL;
PRAGMA foreign_keys=ON;

CREATE TABLE IF NOT EXISTS projects (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	name       TEXT NOT NULL,
	links      TEXT NOT NULL DEFAULT '',
	stage      TEXT NOT NULL DEFAULT 'new',
	created_at DATETIME NOT NULL,
	updated_at DATETIME NOT NULL
);

CREATE TABLE IF NOT EXISTS briefs (
	project_id   INTEGER PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
	summary      TEXT NOT NULL,
	team_json    TEXT NOT NULL,
	socials_json TEXT NOT NULL,
	news_json    TEXT NOT NULL,
	tone         TEXT NOT NULL,
	sources_json TEXT NOT NULL,
	model        TEXT NOT NULL DEFAULT '',
	generated_at DATETIME NOT NULL
);

CREATE TABLE IF NOT EXISTS drafts (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	goal       TEXT NOT NULL,
	content    TEXT NOT NULL,
	model      TEXT NOT NULL DEFAULT '',
	created_at DATETIME NOT NULL
);

CREATE TABLE IF NOT EXISTS history (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	stage      TEXT NOT NULL,
	notes      TEXT NOT NULL DEFAULT '',
	created_at DATETIME NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_drafts_project ON drafts(project_id);
CREATE INDEX IF NOT EXISTS idx_history_project ON history(project_id);
`)
	return err
}

// --- Projects ---

func (s *Store) CreateProject(name, links string) (core.Project, error) {
	now := time.Now().UTC()
	res, err := s.db.Exec(
		`INSERT INTO projects (name, links, stage, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
		name, links, core.StageNew, now, now,
	)
	if err != nil {
		return core.Project{}, fmt.Errorf("create project: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return core.Project{}, err
	}
	return s.GetProject(id)
}

func (s *Store) GetProject(id int64) (core.Project, error) {
	var p core.Project
	err := s.db.QueryRow(
		`SELECT id, name, links, stage, created_at, updated_at FROM projects WHERE id = ?`, id,
	).Scan(&p.ID, &p.Name, &p.Links, &p.Stage, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return core.Project{}, fmt.Errorf("get project %d: %w", id, err)
	}
	return p, nil
}

func (s *Store) ListProjects() ([]core.Project, error) {
	rows, err := s.db.Query(`SELECT id, name, links, stage, created_at, updated_at FROM projects ORDER BY updated_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()

	var out []core.Project
	for rows.Next() {
		var p core.Project
		if err := rows.Scan(&p.ID, &p.Name, &p.Links, &p.Stage, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// FindProjects returns projects whose name contains query, case-insensitive.
// Used by callers that reference a project by name rather than ID (the
// Telegram bot's /find and /research) since typing a numeric ID from
// memory is worse UX than searching.
func (s *Store) FindProjects(query string) ([]core.Project, error) {
	rows, err := s.db.Query(
		`SELECT id, name, links, stage, created_at, updated_at FROM projects WHERE name LIKE ? ESCAPE '\' ORDER BY updated_at DESC`,
		"%"+escapeLike(query)+"%",
	)
	if err != nil {
		return nil, fmt.Errorf("find projects: %w", err)
	}
	defer rows.Close()

	var out []core.Project
	for rows.Next() {
		var p core.Project
		if err := rows.Scan(&p.ID, &p.Name, &p.Links, &p.Stage, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// escapeLike escapes SQL LIKE wildcards in user input so a search for
// "50% off" or "a_b" doesn't get treated as a pattern.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

func (s *Store) UpdateStage(projectID int64, stage core.Stage, notes string) error {
	now := time.Now().UTC()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`UPDATE projects SET stage = ?, updated_at = ? WHERE id = ?`, stage, now, projectID); err != nil {
		return fmt.Errorf("update stage: %w", err)
	}
	if _, err := tx.Exec(
		`INSERT INTO history (project_id, stage, notes, created_at) VALUES (?, ?, ?, ?)`,
		projectID, stage, notes, now,
	); err != nil {
		return fmt.Errorf("insert history: %w", err)
	}
	return tx.Commit()
}

// --- Briefs (one cached brief per project, upserted) ---

func (s *Store) SaveBrief(b core.Brief) error {
	team, err := json.Marshal(b.Team)
	if err != nil {
		return err
	}
	socials, err := json.Marshal(b.Socials)
	if err != nil {
		return err
	}
	news, err := json.Marshal(b.RecentNews)
	if err != nil {
		return err
	}
	sources, err := json.Marshal(b.Sources)
	if err != nil {
		return err
	}

	_, err = s.db.Exec(`
INSERT INTO briefs (project_id, summary, team_json, socials_json, news_json, tone, sources_json, model, generated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(project_id) DO UPDATE SET
	summary = excluded.summary,
	team_json = excluded.team_json,
	socials_json = excluded.socials_json,
	news_json = excluded.news_json,
	tone = excluded.tone,
	sources_json = excluded.sources_json,
	model = excluded.model,
	generated_at = excluded.generated_at
`, b.ProjectID, b.Summary, team, socials, news, b.Tone, sources, b.Model, b.GeneratedAt)
	if err != nil {
		return fmt.Errorf("save brief: %w", err)
	}
	return nil
}

func (s *Store) GetBrief(projectID int64) (*core.Brief, error) {
	var b core.Brief
	var team, socials, news, sources string
	err := s.db.QueryRow(
		`SELECT project_id, summary, team_json, socials_json, news_json, tone, sources_json, model, generated_at FROM briefs WHERE project_id = ?`,
		projectID,
	).Scan(&b.ProjectID, &b.Summary, &team, &socials, &news, &b.Tone, &sources, &b.Model, &b.GeneratedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get brief %d: %w", projectID, err)
	}
	if err := json.Unmarshal([]byte(team), &b.Team); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(socials), &b.Socials); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(news), &b.RecentNews); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(sources), &b.Sources); err != nil {
		return nil, err
	}
	return &b, nil
}

// --- Drafts ---

func (s *Store) SaveDraft(projectID int64, goal, content, model string) (core.Draft, error) {
	now := time.Now().UTC()
	res, err := s.db.Exec(
		`INSERT INTO drafts (project_id, goal, content, model, created_at) VALUES (?, ?, ?, ?, ?)`,
		projectID, goal, content, model, now,
	)
	if err != nil {
		return core.Draft{}, fmt.Errorf("save draft: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return core.Draft{}, err
	}
	return core.Draft{ID: id, ProjectID: projectID, Goal: goal, Content: content, Model: model, CreatedAt: now}, nil
}

func (s *Store) ListDrafts(projectID int64) ([]core.Draft, error) {
	rows, err := s.db.Query(
		`SELECT id, project_id, goal, content, model, created_at FROM drafts WHERE project_id = ? ORDER BY created_at DESC`,
		projectID,
	)
	if err != nil {
		return nil, fmt.Errorf("list drafts: %w", err)
	}
	defer rows.Close()

	var out []core.Draft
	for rows.Next() {
		var d core.Draft
		if err := rows.Scan(&d.ID, &d.ProjectID, &d.Goal, &d.Content, &d.Model, &d.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// --- History ---

func (s *Store) ListHistory(projectID int64) ([]core.HistoryEntry, error) {
	rows, err := s.db.Query(
		`SELECT id, project_id, stage, notes, created_at FROM history WHERE project_id = ? ORDER BY created_at DESC`,
		projectID,
	)
	if err != nil {
		return nil, fmt.Errorf("list history: %w", err)
	}
	defer rows.Close()

	var out []core.HistoryEntry
	for rows.Next() {
		var h core.HistoryEntry
		if err := rows.Scan(&h.ID, &h.ProjectID, &h.Stage, &h.Notes, &h.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
