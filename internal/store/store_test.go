package store

import (
	"path/filepath"
	"testing"
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
