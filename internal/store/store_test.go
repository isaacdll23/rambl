package store

import (
	"path/filepath"
	"testing"
	"time"
)

func TestStoreLifecycle(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")

	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	proj, err := s.EnsureProject("/repo/calc", "calc")
	if err != nil {
		t.Fatalf("ensure project: %v", err)
	}
	// EnsureProject is idempotent.
	if proj2, err := s.EnsureProject("/repo/calc", "calc"); err != nil || proj2 != proj {
		t.Fatalf("ensure project not idempotent: %v %q vs %q", err, proj2, proj)
	}

	if _, err := s.AddTask(proj, "core", "Core lib", "build core", nil); err != nil {
		t.Fatalf("add core: %v", err)
	}
	if _, err := s.AddTask(proj, "cli", "CLI", "build cli", []string{"core"}); err != nil {
		t.Fatalf("add cli: %v", err)
	}

	tasks, err := s.ListTasks(proj)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("want 2 tasks, got %d", len(tasks))
	}
	// Ordered by slug: cli, core.
	if tasks[0].Slug != "cli" || tasks[1].Slug != "core" {
		t.Fatalf("unexpected order: %s, %s", tasks[0].Slug, tasks[1].Slug)
	}
	if len(tasks[0].Deps) != 1 || tasks[0].Deps[0] != "core" {
		t.Fatalf("cli deps = %v, want [core]", tasks[0].Deps)
	}
	if len(tasks[1].Deps) != 0 {
		t.Fatalf("core deps = %v, want []", tasks[1].Deps)
	}

	// Mutate runtime state.
	cli := tasks[0]
	cli.Status = NeedsInput
	cli.Branch = "rambl/cli"
	cli.SessionID = "sess-123"
	cli.Question = "Postgres or SQLite?"
	if err := s.Update(cli); err != nil {
		t.Fatalf("update: %v", err)
	}

	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Reopen — state must persist.
	s2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	got, err := s2.GetTask(proj, "cli")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got == nil {
		t.Fatal("cli task missing after reopen")
	}
	if got.Status != NeedsInput || got.Branch != "rambl/cli" ||
		got.SessionID != "sess-123" || got.Question != "Postgres or SQLite?" {
		t.Fatalf("persisted state wrong: %+v", got)
	}
	if len(got.Deps) != 1 || got.Deps[0] != "core" {
		t.Fatalf("persisted deps = %v", got.Deps)
	}

	// Missing task → nil, no error.
	if missing, err := s2.GetTask(proj, "nope"); err != nil || missing != nil {
		t.Fatalf("expected nil for missing task, got %v %v", missing, err)
	}
}

func TestDeleteTask(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")

	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	proj, err := s.EnsureProject("/repo/calc", "calc")
	if err != nil {
		t.Fatalf("ensure project: %v", err)
	}

	if _, err := s.AddTask(proj, "core", "Core lib", "build core", nil); err != nil {
		t.Fatalf("add core: %v", err)
	}
	if _, err := s.AddTask(proj, "cli", "CLI", "build cli", []string{"core"}); err != nil {
		t.Fatalf("add cli: %v", err)
	}

	// Delete the task that has a dependency edge.
	if err := s.DeleteTask(proj, "cli"); err != nil {
		t.Fatalf("delete cli: %v", err)
	}

	// The task is gone.
	got, err := s.GetTask(proj, "cli")
	if err != nil {
		t.Fatalf("get cli after delete: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil for deleted task, got %+v", got)
	}

	// Its dependency edge is gone too — core no longer has cli pointing at it,
	// and there are no dangling rows. Re-querying core proves the deps table is
	// clean (core itself has no deps; cli's edge to core was removed).
	tasks, err := s.ListTasks(proj)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(tasks) != 1 || tasks[0].Slug != "core" {
		t.Fatalf("want only [core] remaining, got %+v", tasks)
	}
	if len(tasks[0].Deps) != 0 {
		t.Fatalf("core deps = %v, want []", tasks[0].Deps)
	}

	// Deleting a non-existent slug errors.
	if err := s.DeleteTask(proj, "nope"); err == nil {
		t.Fatal("expected error deleting non-existent task, got nil")
	}
}

// Deleting a task that is a *prerequisite* of another removes the depends_on
// edge so no dangling reference remains: the dependent task no longer reports
// the deleted slug among its Deps.
func TestDeleteTaskPrerequisite(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")

	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	proj, err := s.EnsureProject("/repo/calc", "calc")
	if err != nil {
		t.Fatalf("ensure project: %v", err)
	}

	// A is a prerequisite of B.
	if _, err := s.AddTask(proj, "a", "Task A", "build a", nil); err != nil {
		t.Fatalf("add a: %v", err)
	}
	if _, err := s.AddTask(proj, "b", "Task B", "build b", []string{"a"}); err != nil {
		t.Fatalf("add b: %v", err)
	}

	// Delete the prerequisite A.
	if err := s.DeleteTask(proj, "a"); err != nil {
		t.Fatalf("delete a: %v", err)
	}

	// A is gone.
	if got, err := s.GetTask(proj, "a"); err != nil || got != nil {
		t.Fatalf("expected a deleted, got %+v (err %v)", got, err)
	}

	// B survives, but its edge to A is removed — re-fetch proves no dangling dep.
	b, err := s.GetTask(proj, "b")
	if err != nil {
		t.Fatalf("get b: %v", err)
	}
	if b == nil {
		t.Fatal("task b missing after deleting its prerequisite")
	}
	if len(b.Deps) != 0 {
		t.Fatalf("b deps = %v, want [] (edge to deleted a should be gone)", b.Deps)
	}
}

// Deleting the same slug twice: the first call succeeds, the second errors
// because the task no longer exists.
func TestDeleteTaskTwice(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")

	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	proj, err := s.EnsureProject("/repo/calc", "calc")
	if err != nil {
		t.Fatalf("ensure project: %v", err)
	}

	if _, err := s.AddTask(proj, "solo", "Solo", "do solo", nil); err != nil {
		t.Fatalf("add solo: %v", err)
	}

	if err := s.DeleteTask(proj, "solo"); err != nil {
		t.Fatalf("first delete: %v", err)
	}
	if err := s.DeleteTask(proj, "solo"); err == nil {
		t.Fatal("expected error on second delete of the same slug, got nil")
	}
}

func TestActivity(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")

	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	proj, err := s.EnsureProject("/repo/calc", "calc")
	if err != nil {
		t.Fatalf("ensure project: %v", err)
	}
	if _, err := s.AddTask(proj, "core", "Core lib", "build core", nil); err != nil {
		t.Fatalf("add core: %v", err)
	}

	acts := []Activity{
		{Kind: "tool", Tool: "Edit", Detail: "internal/store/store.go"},
		{Kind: "tool", Tool: "Bash", Detail: "go test ./..."},
	}
	if err := s.SetActivity(proj, "core", acts); err != nil {
		t.Fatalf("set activity: %v", err)
	}

	// Round-trip via GetTask, preserving order.
	got, err := s.GetTask(proj, "core")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got.Activity) != 2 {
		t.Fatalf("want 2 activities, got %d: %+v", len(got.Activity), got.Activity)
	}
	if got.Activity[0] != acts[0] || got.Activity[1] != acts[1] {
		t.Fatalf("activity round-trip mismatch: %+v", got.Activity)
	}

	// Round-trip via ListTasks too.
	tasks, err := s.ListTasks(proj)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(tasks) != 1 || len(tasks[0].Activity) != 2 ||
		tasks[0].Activity[0] != acts[0] || tasks[0].Activity[1] != acts[1] {
		t.Fatalf("list activity mismatch: %+v", tasks)
	}

	// SetActivity must NOT change UpdatedAt (the heartbeat uses it ~every 1.5s).
	before := got.UpdatedAt
	time.Sleep(5 * time.Millisecond)
	if err := s.SetActivity(proj, "core", []Activity{{Kind: "tool", Tool: "Read", Detail: "main.go"}}); err != nil {
		t.Fatalf("set activity again: %v", err)
	}
	after, err := s.GetTask(proj, "core")
	if err != nil {
		t.Fatalf("get after: %v", err)
	}
	if !after.UpdatedAt.Equal(before) {
		t.Fatalf("SetActivity changed UpdatedAt: before %v, after %v", before, after.UpdatedAt)
	}

	// SetActivity(nil) clears the feed.
	if err := s.SetActivity(proj, "core", nil); err != nil {
		t.Fatalf("clear activity: %v", err)
	}
	cleared, err := s.GetTask(proj, "core")
	if err != nil {
		t.Fatalf("get cleared: %v", err)
	}
	if len(cleared.Activity) != 0 {
		t.Fatalf("expected empty activity after clear, got %+v", cleared.Activity)
	}
}

func TestStartingStatusRoundTrip(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")

	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	proj, err := s.EnsureProject("/repo/calc", "calc")
	if err != nil {
		t.Fatalf("ensure project: %v", err)
	}
	task, err := s.AddTask(proj, "core", "Core lib", "build core", nil)
	if err != nil {
		t.Fatalf("add core: %v", err)
	}

	task.Status = Starting
	if err := s.Update(task); err != nil {
		t.Fatalf("update: %v", err)
	}

	got, err := s.GetTask(proj, "core")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != Starting || string(got.Status) != "starting" {
		t.Fatalf("status round-trip = %q, want \"starting\"", got.Status)
	}
}

func TestListProjects(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")

	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	if _, err := s.EnsureProject("/repo/alpha", "alpha"); err != nil {
		t.Fatalf("ensure alpha: %v", err)
	}
	// Timestamps are second-precision (RFC3339); sleep past a second boundary
	// so beta's last_opened_at is strictly greater than alpha's.
	time.Sleep(1100 * time.Millisecond)
	if _, err := s.EnsureProject("/repo/beta", "beta"); err != nil {
		t.Fatalf("ensure beta: %v", err)
	}

	projects, err := s.ListProjects()
	if err != nil {
		t.Fatalf("list projects: %v", err)
	}
	if len(projects) != 2 {
		t.Fatalf("want 2 projects, got %d", len(projects))
	}
	// Most-recently-ensured (beta) first.
	if projects[0].Path != "/repo/beta" || projects[1].Path != "/repo/alpha" {
		t.Fatalf("unexpected order: %s, %s", projects[0].Path, projects[1].Path)
	}
	if projects[0].LastOpenedAt.IsZero() {
		t.Fatalf("expected non-zero LastOpenedAt, got zero")
	}
}
