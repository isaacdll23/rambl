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
	FeatureID string // "" == standalone task (today's behavior); else the owning feature's id
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

// FeatureStatus is a feature's lifecycle state.
type FeatureStatus string

const (
	FeaturePlanning    FeatureStatus = "planning"    // tasks being added, nothing dispatched
	FeatureRunning     FeatureStatus = "running"     // tasks executing / merging
	FeatureIntegrating FeatureStatus = "integrating" // integration gate working the branch
	FeatureDone        FeatureStatus = "done"        // merged + PR opened
	FeatureFailed      FeatureStatus = "failed"
)

// Feature groups multiple tasks that will later map to a single feature branch + PR.
type Feature struct {
	ID        string
	ProjectID string
	Slug      string
	Title     string
	Branch    string // "rambl/feat/<slug>", set once started; "" before
	Status    FeatureStatus
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
CREATE TABLE IF NOT EXISTS events (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  project_id TEXT NOT NULL,
  kind       TEXT NOT NULL,
  slug       TEXT NOT NULL DEFAULT '',
  summary    TEXT NOT NULL,
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_events_project ON events(project_id, id);
CREATE TABLE IF NOT EXISTS features (
  id         TEXT PRIMARY KEY,
  project_id TEXT NOT NULL REFERENCES projects(id),
  slug       TEXT NOT NULL,
  title      TEXT NOT NULL,
  branch     TEXT NOT NULL DEFAULT '',
  status     TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  UNIQUE(project_id, slug)
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
	if _, err := s.db.Exec(`ALTER TABLE tasks ADD COLUMN feature_id TEXT NOT NULL DEFAULT ''`); err != nil {
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

// AddTask creates a standalone task. deps are slugs within the same project.
func (s *Store) AddTask(projectID, slug, title, prompt string, deps []string) (*Task, error) {
	return s.AddTaskToFeature(projectID, "", slug, title, prompt, deps)
}

// AddTaskToFeature creates a task associated with featureID ("" for standalone).
// deps are slugs within the same project.
func (s *Store) AddTaskToFeature(projectID, featureID, slug, title, prompt string, deps []string) (*Task, error) {
	t := &Task{
		ID: newID(), ProjectID: projectID, FeatureID: featureID, Slug: slug, Title: title,
		Prompt: prompt, Status: Todo, Deps: deps,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO tasks
		(id, project_id, feature_id, slug, title, prompt, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, projectID, featureID, slug, title, prompt, string(Todo), iso(t.CreatedAt), iso(t.UpdatedAt)); err != nil {
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
	rows, err := s.db.Query(`SELECT id, project_id, feature_id, slug, title, prompt, status, branch, session_id, question, result, created_at, updated_at
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
	row := s.db.QueryRow(`SELECT id, project_id, feature_id, slug, title, prompt, status, branch, session_id, question, result, created_at, updated_at
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

// DeleteTask removes a task and its dependency edges. It also removes any
// dependency edges in which this task is the prerequisite, so no dangling
// references remain. Returns an error if no such task exists.
func (s *Store) DeleteTask(projectID, slug string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM tasks WHERE project_id=? AND slug=?`, projectID, slug).Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		return fmt.Errorf("no task %q", slug)
	}
	if _, err := tx.Exec(`DELETE FROM task_deps WHERE project_id=? AND (task_slug=? OR depends_on=?)`, projectID, slug, slug); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM tasks WHERE project_id=? AND slug=?`, projectID, slug); err != nil {
		return err
	}
	return tx.Commit()
}

// AddFeature creates a feature in status "planning"; errors on duplicate (project_id, slug).
func (s *Store) AddFeature(projectID, slug, title string) (*Feature, error) {
	f := &Feature{
		ID: newID(), ProjectID: projectID, Slug: slug, Title: title,
		Status: FeaturePlanning, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if _, err := s.db.Exec(`INSERT INTO features
		(id, project_id, slug, title, branch, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		f.ID, projectID, slug, title, f.Branch, string(f.Status), iso(f.CreatedAt), iso(f.UpdatedAt)); err != nil {
		return nil, err
	}
	return f, nil
}

// GetFeature returns the feature or (nil, nil) when not found.
func (s *Store) GetFeature(projectID, slug string) (*Feature, error) {
	row := s.db.QueryRow(`SELECT id, project_id, slug, title, branch, status, created_at, updated_at
		FROM features WHERE project_id=? AND slug=?`, projectID, slug)
	f, err := scanFeature(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return f, nil
}

// ListFeatures returns all features for the project, ordered by slug.
func (s *Store) ListFeatures(projectID string) ([]*Feature, error) {
	rows, err := s.db.Query(`SELECT id, project_id, slug, title, branch, status, created_at, updated_at
		FROM features WHERE project_id=? ORDER BY slug`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Feature
	for rows.Next() {
		f, err := scanFeature(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// UpdateFeature persists title, branch, status and stamps updated_at.
func (s *Store) UpdateFeature(f *Feature) error {
	f.UpdatedAt = time.Now()
	_, err := s.db.Exec(`UPDATE features SET title=?, branch=?, status=?, updated_at=?
		WHERE id=?`,
		f.Title, f.Branch, string(f.Status), iso(f.UpdatedAt), f.ID)
	return err
}

// DeleteFeature removes the feature; errors if any task still references it.
func (s *Store) DeleteFeature(projectID, slug string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id string
	err = tx.QueryRow(`SELECT id FROM features WHERE project_id=? AND slug=?`, projectID, slug).Scan(&id)
	if err == sql.ErrNoRows {
		return fmt.Errorf("no feature %q", slug)
	}
	if err != nil {
		return err
	}
	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM tasks WHERE project_id=? AND feature_id=?`, projectID, id).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return fmt.Errorf("feature %q still has %d task(s)", slug, count)
	}
	if _, err := tx.Exec(`DELETE FROM features WHERE id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// TasksByFeature returns tasks with feature_id = featureID, ordered by slug, deps populated.
func (s *Store) TasksByFeature(projectID, featureID string) ([]*Task, error) {
	rows, err := s.db.Query(`SELECT id, project_id, feature_id, slug, title, prompt, status, branch, session_id, question, result, created_at, updated_at
		FROM tasks WHERE project_id=? AND feature_id=? ORDER BY slug`, projectID, featureID)
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

// Event is one PM-activity entry (a record of a PM tool action).
type Event struct {
	ID        int64
	ProjectID string
	Kind      string // short verb: "create" | "dispatch" | "send" | "delete" | "verify" | "revise" | "open_pr"
	Slug      string // task slug involved; may be ""
	Summary   string // human one-liner, e.g. "dispatched api-routes"
	CreatedAt time.Time
}

// AppendEvent inserts one PM-activity event, stamping CreatedAt with the current time.
func (s *Store) AppendEvent(projectID, kind, slug, summary string) error {
	_, err := s.db.Exec(`INSERT INTO events (project_id, kind, slug, summary, created_at)
		VALUES (?, ?, ?, ?, ?)`,
		projectID, kind, slug, summary, now())
	return err
}

// RecentEvents returns up to limit most-recent events for the project, NEWEST FIRST
// (ORDER BY id DESC LIMIT ?). Returns an empty slice (not nil error) when there are none.
func (s *Store) RecentEvents(projectID string, limit int) ([]*Event, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.Query(`SELECT id, project_id, kind, slug, summary, created_at
		FROM events WHERE project_id=? ORDER BY id DESC LIMIT ?`, projectID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Event{}
	for rows.Next() {
		e := &Event{}
		var created string
		if err := rows.Scan(&e.ID, &e.ProjectID, &e.Kind, &e.Slug, &e.Summary, &created); err != nil {
			return nil, err
		}
		e.CreatedAt, _ = time.Parse(time.RFC3339, created)
		out = append(out, e)
	}
	return out, rows.Err()
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
	if err := sc.Scan(&t.ID, &t.ProjectID, &t.FeatureID, &t.Slug, &t.Title, &t.Prompt, &status,
		&t.Branch, &t.SessionID, &t.Question, &t.Result, &created, &updated); err != nil {
		return nil, err
	}
	t.Status = Status(status)
	t.CreatedAt, _ = time.Parse(time.RFC3339, created)
	t.UpdatedAt, _ = time.Parse(time.RFC3339, updated)
	return t, nil
}

func scanFeature(sc scanner) (*Feature, error) {
	f := &Feature{}
	var status, created, updated string
	if err := sc.Scan(&f.ID, &f.ProjectID, &f.Slug, &f.Title, &f.Branch, &status, &created, &updated); err != nil {
		return nil, err
	}
	f.Status = FeatureStatus(status)
	f.CreatedAt, _ = time.Parse(time.RFC3339, created)
	f.UpdatedAt, _ = time.Parse(time.RFC3339, updated)
	return f, nil
}

func now() string            { return iso(time.Now()) }
func iso(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// newID returns a random UUIDv4 string (no external dependency).
func newID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
