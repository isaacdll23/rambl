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

func TestBranchName(t *testing.T) {
	if got := branchName(Spec{ID: "login"}); got != "rambl/login" {
		t.Errorf("branchName with empty Branch = %q, want rambl/login", got)
	}
	if got := branchName(Spec{ID: "login", Branch: "rambl/feat/auth"}); got != "rambl/feat/auth" {
		t.Errorf("branchName with explicit Branch = %q, want rambl/feat/auth", got)
	}
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

func TestAddFeatureWorktreeAndBranchExists(t *testing.T) {
	repo := newRepo(t)
	wtPath := filepath.Join(t.TempDir(), "@feat-x")
	branch := "rambl/feat/x"

	if BranchExists(repo, branch) {
		t.Fatalf("branch %q should not exist before AddFeatureWorktree", branch)
	}
	if BranchExists(repo, "no-such-branch") {
		t.Errorf("BranchExists should be false for a bogus name")
	}

	if err := AddFeatureWorktree(repo, wtPath, branch, "HEAD"); err != nil {
		t.Fatalf("AddFeatureWorktree: %v", err)
	}

	// The worktree dir exists and is populated from base (file.txt from newRepo).
	if _, err := os.Stat(wtPath); err != nil {
		t.Fatalf("worktree should exist after add: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wtPath, "file.txt")); err != nil {
		t.Errorf("worktree should be populated from base (file.txt missing): %v", err)
	}
	if !BranchExists(repo, branch) {
		t.Errorf("BranchExists should be true after AddFeatureWorktree")
	}

	// Adding the same branch again must error (branch already exists).
	if err := AddFeatureWorktree(repo, filepath.Join(t.TempDir(), "dup"), branch, "HEAD"); err == nil {
		t.Errorf("AddFeatureWorktree should error when branch already exists")
	}

	if err := CleanupWorktree(repo, wtPath, branch); err != nil {
		t.Fatalf("CleanupWorktree: %v", err)
	}
	if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
		t.Errorf("worktree dir should be gone, stat err = %v", err)
	}
	if BranchExists(repo, branch) {
		t.Errorf("branch %q should be gone after cleanup", branch)
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

// TestCommitUsesConfiguredIdentity verifies that when the repo has a configured
// git identity, worker commits are authored as that user — NOT the synthetic
// rambl@localhost fallback.
func TestCommitUsesConfiguredIdentity(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		if out, err := git(repo, args...); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init")
	// A specific local identity that must be honoured by the commit.
	run("config", "user.email", "alice@example.com")
	run("config", "user.name", "Alice")

	if !hasGitIdentity(repo) {
		t.Fatalf("hasGitIdentity should be true with local identity set")
	}

	w := &Worker{Worktree: repo}
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Commit("alice change"); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	out, err := git(repo, "log", "-1", "--format=%ae")
	if err != nil {
		t.Fatalf("git log: %v: %s", err, out)
	}
	if got := strings.TrimSpace(out); got != "alice@example.com" {
		t.Errorf("commit author email = %q, want alice@example.com", got)
	}
}

// TestHasGitIdentityNoConfig checks the no-identity fallback path: with global
// and system config isolated and no local identity, hasGitIdentity is false and
// gitID then injects the synthetic rambl identity so commits still succeed.
func TestHasGitIdentityNoConfig(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()

	// Isolate config sources so only the (empty) local config is consulted.
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("HOME", repo)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(repo, ".config"))
	// Unset any ambient identity env vars (an empty value would override the
	// -c fallback and break the commit). t.Setenv registers cleanup; clear after.
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(k, "")
		if err := os.Unsetenv(k); err != nil {
			t.Fatalf("unsetenv %s: %v", k, err)
		}
	}

	if out, err := git(repo, "init"); err != nil {
		t.Fatalf("init: %v: %s", err, out)
	}

	if hasGitIdentity(repo) {
		t.Skip("environment has an ambient git identity; cannot exercise no-config fallback here")
	}

	// gitID must inject the rambl fallback so a commit still succeeds.
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if out, err := gitID(repo, "add", "-A"); err != nil {
		t.Fatalf("add: %v: %s", err, out)
	}
	if out, err := gitID(repo, "commit", "-m", "fallback"); err != nil {
		t.Fatalf("commit with fallback identity should succeed: %v: %s", err, out)
	}
	out, err := git(repo, "log", "-1", "--format=%ae")
	if err != nil {
		t.Fatalf("git log: %v: %s", err, out)
	}
	if got := strings.TrimSpace(out); got != "rambl@localhost" {
		t.Errorf("fallback commit author email = %q, want rambl@localhost", got)
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

// commitBranch creates branch off HEAD in repo, writes content to file, and
// commits — using a throwaway worktree so the main worktree is untouched.
func commitBranch(t *testing.T, repo, branch, file, content string) {
	t.Helper()
	wt := filepath.Join(t.TempDir(), "wt-"+strings.ReplaceAll(branch, "/", "-"))
	if out, err := gitID(repo, "worktree", "add", "-b", branch, wt, "HEAD"); err != nil {
		t.Fatalf("worktree add %s: %v: %s", branch, err, out)
	}
	if err := os.WriteFile(filepath.Join(wt, file), []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", file, err)
	}
	if out, err := gitID(wt, "add", "-A"); err != nil {
		t.Fatalf("add: %v: %s", err, out)
	}
	if out, err := gitID(wt, "commit", "-m", "change "+file); err != nil {
		t.Fatalf("commit: %v: %s", err, out)
	}
	if out, err := gitID(repo, "worktree", "remove", "--force", wt); err != nil {
		t.Fatalf("worktree remove: %v: %s", err, out)
	}
}

func TestSquashMergeHappyPath(t *testing.T) {
	repo := newRepo(t)
	feat := filepath.Join(t.TempDir(), "@feat-x")
	if err := AddFeatureWorktree(repo, feat, "rambl/feat/x", "HEAD"); err != nil {
		t.Fatalf("AddFeatureWorktree: %v", err)
	}
	commitBranch(t, repo, "rambl/a", "a.txt", "a\n")

	conflict, err := SquashMerge(feat, "rambl/a", "feat(a): add a")
	if err != nil || conflict {
		t.Fatalf("SquashMerge a = (%v, %v), want (false, nil)", conflict, err)
	}
	if _, err := os.Stat(filepath.Join(feat, "a.txt")); err != nil {
		t.Errorf("a.txt should exist on feature worktree: %v", err)
	}
	out, err := gitID(feat, "log", "-1", "--format=%s")
	if err != nil {
		t.Fatalf("log: %v: %s", err, out)
	}
	if strings.TrimSpace(out) != "feat(a): add a" {
		t.Errorf("commit subject = %q, want %q", strings.TrimSpace(out), "feat(a): add a")
	}
}

func TestSquashMergeConflictLeavesClean(t *testing.T) {
	repo := newRepo(t)
	feat := filepath.Join(t.TempDir(), "@feat-x")
	if err := AddFeatureWorktree(repo, feat, "rambl/feat/x", "HEAD"); err != nil {
		t.Fatalf("AddFeatureWorktree: %v", err)
	}
	// Both branches edit the same line of file.txt (from newRepo).
	commitBranch(t, repo, "rambl/a", "file.txt", "from-a\n")
	commitBranch(t, repo, "rambl/b", "file.txt", "from-b\n")

	if conflict, err := SquashMerge(feat, "rambl/a", "feat(a): edit"); err != nil || conflict {
		t.Fatalf("SquashMerge a = (%v, %v), want (false, nil)", conflict, err)
	}
	conflict, err := SquashMerge(feat, "rambl/b", "feat(b): edit")
	if !conflict {
		t.Fatalf("SquashMerge b should conflict, got (%v, %v)", conflict, err)
	}
	if err == nil || !strings.Contains(err.Error(), "file.txt") {
		t.Errorf("conflict error should name file.txt, got %v", err)
	}
	// Worktree must be left clean with no merge in progress.
	st, gerr := gitID(feat, "status", "--porcelain")
	if gerr != nil {
		t.Fatalf("status: %v: %s", gerr, st)
	}
	if strings.TrimSpace(st) != "" {
		t.Errorf("feature worktree should be clean, got %q", st)
	}
	if files, _ := ConflictedFiles(feat); len(files) != 0 {
		t.Errorf("no unmerged paths expected after abort, got %v", files)
	}
}

func TestSquashMergeEmptyNoCommit(t *testing.T) {
	repo := newRepo(t)
	feat := filepath.Join(t.TempDir(), "@feat-x")
	if err := AddFeatureWorktree(repo, feat, "rambl/feat/x", "HEAD"); err != nil {
		t.Fatalf("AddFeatureWorktree: %v", err)
	}
	before, err := gitID(feat, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse: %v: %s", err, before)
	}
	// A branch identical to base (no changes).
	if out, err := gitID(repo, "branch", "rambl/empty", "HEAD"); err != nil {
		t.Fatalf("branch: %v: %s", err, out)
	}

	conflict, err := SquashMerge(feat, "rambl/empty", "feat(empty): nothing")
	if err != nil || conflict {
		t.Fatalf("SquashMerge empty = (%v, %v), want (false, nil)", conflict, err)
	}
	after, err := gitID(feat, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("rev-parse: %v: %s", err, after)
	}
	if strings.TrimSpace(before) != strings.TrimSpace(after) {
		t.Errorf("HEAD moved on empty squash: %q -> %q", before, after)
	}
}
