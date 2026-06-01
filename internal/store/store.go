// Package store is the SQLite-backed state for rambl: projects, tasks, their
// dependency edges, and runtime status. It replaces the earlier in-repo
// markdown task files. State lives in a single database (default
// ~/.rambl/state.db), single-host for now; ids are stable UUIDs and rows
// carry timestamps so cross-machine sync stays feasible later.
package store

import (
	"crypto/rand"
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Status is a task's lifecycle state.
type Status string

const (
	Todo       Status = "todo"
	Running    Status = "running"
	NeedsInput Status = "needs_input" // worker reported BLOCKED; awaiting the PM (or you)
	Done       Status = "done"
	Failed     Status = "failed"
	Blocked    Status = "blocked" // a dependency failed / could not be integrated
)

// Task is one unit of work plus its runtime state.
type Task struct {
	ID        string // stable UUID (sync identity)
	ProjectID string
	Slug      string // human id, unique within a project; deps reference this
	Title     string
	Prompt    string
	Status    Status
	Branch    string
	SessionID string
	Question  string // set when Status == NeedsInput (the worker's blocking question)
	Result    string // the worker's latest assistant message (progress / done summary)
	Deps      []string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Store is the database handle.
type Store struct{ db *sql.DB }

// Open opens (creating if needed) the database at path and applies migrations.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// WAL + a busy timeout so a separate read-only process (the monitor) can
	// read concurrently while the environment writes.
	for _, pragma := range []string{
		"PRAGMA foreign_keys = ON;",
		"PRAGMA journal_mode = WAL;",
		"PRAGMA busy_timeout = 5000;",
	} {
		if _, err := db.Exec(pragma); err != nil {
			return nil, err
		}
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

const schema = `
CREATE TABLE IF NOT EXISTS projects (
  id         TEXT PRIMARY KEY,
  path       TEXT UNIQUE NOT NULL,
  name       TEXT NOT NULL,
  created_at TEXT NOT NULL,
  last_opened_at TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS tasks (
  id         TEXT PRIMARY KEY,
  project_id TEXT NOT NULL REFERENCES projects(id),
  slug       TEXT NOT NULL,
  title      TEXT NOT NULL,
  prompt     TEXT NOT NULL,
  status     TEXT NOT NULL,
  branch     TEXT NOT NULL DEFAULT '',
  session_id TEXT NOT NULL DEFAULT '',
  question   TEXT NOT NULL DEFAULT '',
  result     TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  UNIQUE(project_id, slug)
);
CREATE TABLE IF NOT EXISTS task_deps (
  project_id TEXT NOT NULL,
  task_slug  TEXT NOT NULL,
  depends_on TEXT NOT NULL,
  PRIMARY KEY (project_id, task_slug, depends_on)
);
`

func (s *Store) migrate() error {
	if _, err := s.db.Exec(schema); err != nil {
		return err
	}
	if _, err := s.db.Exec(`ALTER TABLE projects ADD COLUMN last_opened_at TEXT NOT NULL DEFAULT ''`); err != nil {
		if !strings.Contains(err.Error(), "duplicate column") {
			return err
		}
	}
	return nil
}

// EnsureProject returns the id of the project rooted at path, creating it if new.
func (s *Store) EnsureProject(path, name string) (string, error) {
	var id string
	err := s.db.QueryRow(`SELECT id FROM projects WHERE path = ?`, path).Scan(&id)
	if err == nil {
		if _, err := s.db.Exec(`UPDATE projects SET last_opened_at=? WHERE id=?`, now(), id); err != nil {
			return "", err
		}
		return id, nil
	}
	if err != sql.ErrNoRows {
		return "", err
	}
	id = newID()
	_, err = s.db.Exec(`INSERT INTO projects (id, path, name, created_at, last_opened_at) VALUES (?, ?, ?, ?, ?)`,
		id, path, name, now(), now())
	return id, err
}

// Project is a tracked repository rambl knows about.
type Project struct {
	ID           string
	Path         string
	Name         string
	CreatedAt    time.Time
	LastOpenedAt time.Time
}

// ListProjects returns every known project, most-recently-opened first
// (falling back to created_at for rows never explicitly opened).
func (s *Store) ListProjects() ([]*Project, error) {
	rows, err := s.db.Query(`SELECT id, path, name, created_at, last_opened_at FROM projects
		ORDER BY CASE WHEN last_opened_at = '' THEN created_at ELSE last_opened_at END DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Project
	for rows.Next() {
		p := &Project{}
		var created, lastOpened string
		if err := rows.Scan(&p.ID, &p.Path, &p.Name, &created, &lastOpened); err != nil {
			return nil, err
		}
		p.CreatedAt, _ = time.Parse(time.RFC3339, created)
		if lastOpened != "" {
			p.LastOpenedAt, _ = time.Parse(time.RFC3339, lastOpened)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ProjectID returns the id of the project at path, or "" if none exists.
func (s *Store) ProjectID(path string) (string, error) {
	var id string
	err := s.db.QueryRow(`SELECT id FROM projects WHERE path = ?`, path).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return id, err
}

// AddTask creates a task. deps are slugs within the same project.
func (s *Store) AddTask(projectID, slug, title, prompt string, deps []string) (*Task, error) {
	t := &Task{
		ID: newID(), ProjectID: projectID, Slug: slug, Title: title,
		Prompt: prompt, Status: Todo, Deps: deps,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO tasks
		(id, project_id, slug, title, prompt, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, projectID, slug, title, prompt, string(Todo), iso(t.CreatedAt), iso(t.UpdatedAt)); err != nil {
		return nil, err
	}
	for _, d := range deps {
		if _, err := tx.Exec(`INSERT INTO task_deps (project_id, task_slug, depends_on) VALUES (?, ?, ?)`,
			projectID, slug, d); err != nil {
			return nil, err
		}
	}
	return t, tx.Commit()
}

// Update persists a task's mutable runtime fields (status/branch/session/question).
func (s *Store) Update(t *Task) error {
	t.UpdatedAt = time.Now()
	_, err := s.db.Exec(`UPDATE tasks SET status=?, branch=?, session_id=?, question=?, result=?, updated_at=?
		WHERE id=?`,
		string(t.Status), t.Branch, t.SessionID, t.Question, t.Result, iso(t.UpdatedAt), t.ID)
	return err
}

// ListTasks returns all tasks in a project (with deps), ordered by slug.
func (s *Store) ListTasks(projectID string) ([]*Task, error) {
	rows, err := s.db.Query(`SELECT id, project_id, slug, title, prompt, status, branch, session_id, question, result, created_at, updated_at
		FROM tasks WHERE project_id=? ORDER BY slug`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, t := range out {
		if t.Deps, err = s.depsOf(projectID, t.Slug); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// GetTask fetches one task by slug within a project (nil if absent).
func (s *Store) GetTask(projectID, slug string) (*Task, error) {
	row := s.db.QueryRow(`SELECT id, project_id, slug, title, prompt, status, branch, session_id, question, result, created_at, updated_at
		FROM tasks WHERE project_id=? AND slug=?`, projectID, slug)
	t, err := scanTask(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if t.Deps, err = s.depsOf(projectID, slug); err != nil {
		return nil, err
	}
	return t, nil
}

func (s *Store) depsOf(projectID, slug string) ([]string, error) {
	rows, err := s.db.Query(`SELECT depends_on FROM task_deps WHERE project_id=? AND task_slug=? ORDER BY depends_on`, projectID, slug)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var deps []string
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, err
		}
		deps = append(deps, d)
	}
	return deps, rows.Err()
}

type scanner interface {
	Scan(dest ...any) error
}

func scanTask(sc scanner) (*Task, error) {
	t := &Task{}
	var status, created, updated string
	if err := sc.Scan(&t.ID, &t.ProjectID, &t.Slug, &t.Title, &t.Prompt, &status,
		&t.Branch, &t.SessionID, &t.Question, &t.Result, &created, &updated); err != nil {
		return nil, err
	}
	t.Status = Status(status)
	t.CreatedAt, _ = time.Parse(time.RFC3339, created)
	t.UpdatedAt, _ = time.Parse(time.RFC3339, updated)
	return t, nil
}

func now() string         { return iso(time.Now()) }
func iso(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// newID returns a random UUIDv4 string (no external dependency).
func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
