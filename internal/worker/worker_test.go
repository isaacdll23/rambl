package worker

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// newRepo creates a temp git repo with a configured identity and an initial
// commit on the current branch, returning its path.
func newRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		if out, err := gitID(repo, args...); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init")
	if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	run("add", "-A")
	run("commit", "-m", "initial")
	return repo
}

func TestCleanupWorktree(t *testing.T) {
	repo := newRepo(t)
	wtPath := filepath.Join(t.TempDir(), "wt")

	if out, err := gitID(repo, "worktree", "add", "-b", "rambl/x", wtPath, "HEAD"); err != nil {
		t.Fatalf("worktree add: %v: %s", err, out)
	}
	if _, err := os.Stat(wtPath); err != nil {
		t.Fatalf("worktree should exist after add: %v", err)
	}

	if err := CleanupWorktree(repo, wtPath, "rambl/x"); err != nil {
		t.Fatalf("CleanupWorktree: %v", err)
	}

	if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
		t.Errorf("worktree dir should be gone, stat err = %v", err)
	}
	out, err := gitID(repo, "branch", "--list", "rambl/x")
	if err != nil {
		t.Fatalf("branch --list: %v: %s", err, out)
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("branch rambl/x should be gone, got %q", out)
	}
}

func TestCleanupWorktreeBestEffort(t *testing.T) {
	repo := newRepo(t)

	// Empty args must be a no-op and never panic.
	if err := CleanupWorktree(repo, "", ""); err != nil {
		t.Errorf("CleanupWorktree empty: %v", err)
	}
	// Nonexistent path/branch is swallowed (best-effort) and returns nil.
	if err := CleanupWorktree(repo, "/nonexistent/path", "no-such-branch"); err != nil {
		t.Errorf("CleanupWorktree nonexistent: %v", err)
	}
}

func TestPushBranch(t *testing.T) {
	source := newRepo(t)
	run := func(args ...string) {
		t.Helper()
		if out, err := gitID(source, args...); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}

	// A branch with its own commit to push.
	run("checkout", "-b", "rambl/y")
	if err := os.WriteFile(filepath.Join(source, "y.txt"), []byte("y\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	run("add", "-A")
	run("commit", "-m", "y change")

	// A local bare repo standing in for the remote — fully offline.
	bareDir := filepath.Join(t.TempDir(), "bare.git")
	if out, err := gitID("", "init", "--bare", bareDir); err != nil {
		t.Fatalf("init --bare: %v: %s", err, out)
	}
	run("remote", "add", "origin", bareDir)

	if out, err := PushBranch(source, "origin", "rambl/y"); err != nil {
		t.Fatalf("PushBranch: %v: %s", err, out)
	}

	// The ref must have landed in the bare repo.
	out, err := gitID(source, "ls-remote", "origin", "rambl/y")
	if err != nil {
		t.Fatalf("ls-remote: %v: %s", err, out)
	}
	if strings.TrimSpace(out) == "" {
		t.Errorf("expected ref rambl/y in remote, got empty ls-remote")
	}
	if out, err := gitID("", "--git-dir="+bareDir, "rev-parse", "refs/heads/rambl/y"); err != nil {
		t.Errorf("bare repo missing refs/heads/rambl/y: %v: %s", err, out)
	}
}

func TestDiffBranchEqualBase(t *testing.T) {
	repo := newRepo(t)
	// A branch identical to base produces an empty diffstat.
	if out, err := gitID(repo, "branch", "same", "HEAD"); err != nil {
		t.Fatalf("branch: %v: %s", err, out)
	}
	stat, patch, err := DiffBranch(repo, "HEAD", "same")
	if err != nil {
		t.Fatalf("DiffBranch: %v", err)
	}
	if stat != "" {
		t.Errorf("expected empty stat for equal branch, got %q", stat)
	}
	if patch != "" {
		t.Errorf("expected empty patch for equal branch, got %q", patch)
	}
}

func TestReopenMissingWorktree(t *testing.T) {
	repo := newRepo(t)
	missing := filepath.Join(t.TempDir(), "does-not-exist")

	w := New(Spec{ID: "z", RepoPath: repo, Worktree: missing, Reopen: true})
	// The Reopen stat-check precedes any session spawn, so Start returns here
	// without touching Claude/network.
	err := w.Start(context.Background(), "dummy-self-exe")
	if err == nil {
		t.Fatalf("expected error for missing worktree on reopen")
	}
	msg := err.Error()
	if !strings.Contains(msg, "worktree") || !strings.Contains(msg, "missing") {
		t.Errorf("error should mention missing worktree, got %q", msg)
	}
}

func TestDiffBranch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()

	run := func(args ...string) {
		t.Helper()
		if out, err := gitID(repo, args...); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	write := func(content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte(content), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	run("init", "-b", "base")
	write("one\n")
	run("add", "-A")
	run("commit", "-m", "initial")

	run("checkout", "-b", "feature")
	write("one\ntwo\n")
	run("add", "-A")
	run("commit", "-m", "change")

	stat, patch, err := DiffBranch(repo, "base", "feature")
	if err != nil {
		t.Fatalf("DiffBranch: %v", err)
	}
	if stat == "" {
		t.Errorf("expected non-empty stat")
	}
	if patch == "" {
		t.Errorf("expected non-empty patch")
	}
	if !strings.Contains(stat, "file.txt") {
		t.Errorf("stat does not mention file.txt: %q", stat)
	}
	if !strings.Contains(patch, "file.txt") {
		t.Errorf("patch does not mention file.txt: %q", patch)
	}
}
