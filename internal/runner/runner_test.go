package runner

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"rambl/internal/store"
	"rambl/internal/worker"
)

// --- test helpers ----------------------------------------------------------

// runGit runs a git subcommand in dir and fails the test on error.
func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s (in %s): %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return string(out)
}

// wantErr asserts err is non-nil and its message contains sub.
func wantErr(t *testing.T, err error, sub string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error containing %q, got nil", sub)
	}
	if !strings.Contains(err.Error(), sub) {
		t.Fatalf("error %q does not contain %q", err.Error(), sub)
	}
}

// harness is a hermetic runner fixture: a temp git repo with one commit, a
// fresh store with a project, and a Runner whose worktree base is a separate
// temp dir. selfExe is "" — no worker is ever spawned by these tests.
type harness struct {
	t            *testing.T
	st           *store.Store
	repo         string
	worktreeBase string
	projectID    string
	r            *Runner
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	repo := t.TempDir()
	runGit(t, repo, "init")
	runGit(t, repo, "config", "user.email", "rambl@test")
	runGit(t, repo, "config", "user.name", "rambl")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("init\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-m", "init")

	dbPath := filepath.Join(t.TempDir(), "state.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	projectID, err := st.EnsureProject(repo, "test")
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	worktreeBase := t.TempDir()
	r := New(st, repo, "HEAD", "", worktreeBase)
	return &harness{t: t, st: st, repo: repo, worktreeBase: worktreeBase, projectID: projectID, r: r}
}

func (h *harness) addTask(slug string, deps []string) *store.Task {
	h.t.Helper()
	tk, err := h.st.AddTask(h.projectID, slug, slug+" title", "do "+slug, deps)
	if err != nil {
		h.t.Fatalf("AddTask %q: %v", slug, err)
	}
	return tk
}

func (h *harness) get(slug string) *store.Task {
	h.t.Helper()
	tk, err := h.st.GetTask(h.projectID, slug)
	if err != nil {
		h.t.Fatalf("GetTask %q: %v", slug, err)
	}
	return tk
}

// mutate fetches the task, applies fn, and persists it.
func (h *harness) mutate(slug string, fn func(*store.Task)) {
	h.t.Helper()
	tk := h.get(slug)
	if tk == nil {
		h.t.Fatalf("task %q not found", slug)
	}
	fn(tk)
	if err := h.st.Update(tk); err != nil {
		h.t.Fatalf("Update %q: %v", slug, err)
	}
}

// addWorktree creates a real branch rambl/<slug> + worktree dir at HEAD.
func (h *harness) addWorktree(slug string) string {
	h.t.Helper()
	wt := filepath.Join(h.worktreeBase, h.projectID, slug)
	if err := os.MkdirAll(filepath.Dir(wt), 0o755); err != nil {
		h.t.Fatalf("mkdir worktree parent: %v", err)
	}
	runGit(h.t, h.repo, "worktree", "add", "-b", "rambl/"+slug, wt, "HEAD")
	return wt
}

// mkWorktreeDir creates a plain directory where Verify expects a worktree (no
// git involvement — Verify only os.Stat's it and runs the command there).
func (h *harness) mkWorktreeDir(slug string) string {
	h.t.Helper()
	wt := filepath.Join(h.worktreeBase, h.projectID, slug)
	if err := os.MkdirAll(wt, 0o755); err != nil {
		h.t.Fatalf("mkdir worktree dir: %v", err)
	}
	return wt
}

// --- 1. classify -----------------------------------------------------------

func TestClassify(t *testing.T) {
	tests := []struct {
		name      string
		reply     string
		wantStat  store.Status
		wantQ     string // exact expected question ("" means assert empty)
		qNonEmpty bool   // if true, assert question is non-empty instead of exact
	}{
		{
			name:     "trailing done",
			reply:    "all finished, build is green\nRAMBL_DONE",
			wantStat: store.Done,
			wantQ:    "",
		},
		{
			name:     "blocked on last line",
			reply:    "made progress\nRAMBL_BLOCKED: need X",
			wantStat: store.NeedsInput,
			wantQ:    "need X",
		},
		{
			name:      "neither marker yields fallback question",
			reply:     "I did some stuff but never signalled an outcome",
			wantStat:  store.NeedsInput,
			qNonEmpty: true,
		},
		{
			// The bug being fixed: a body that merely QUOTES the BLOCKED token in
			// prose must not be classified as blocked when the final line is DONE.
			name:     "blocked quoted in body, done on last line -> done",
			reply:    "I considered emitting RAMBL_BLOCKED: need a thing but resolved it myself.\nRAMBL_DONE",
			wantStat: store.Done,
			wantQ:    "",
		},
		{
			name:     "blocked on last non-empty line with trailing blanks",
			reply:    "made progress\nRAMBL_BLOCKED: need X\n\n  \n",
			wantStat: store.NeedsInput,
			wantQ:    "need X",
		},
		{
			name:     "done with trailing newline and whitespace",
			reply:    "all green\nRAMBL_DONE\n  \n",
			wantStat: store.Done,
			wantQ:    "",
		},
		{
			name:     "done quoted in body, prose on last line -> fallback",
			reply:    "The marker RAMBL_DONE means the task is complete.\nStill working on it, will report back.",
			wantStat: store.NeedsInput,
			wantQ:    "(worker ended its turn without a DONE or BLOCKED marker)",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotStat, gotQ := classify(tc.reply)
			if gotStat != tc.wantStat {
				t.Fatalf("status = %q, want %q", gotStat, tc.wantStat)
			}
			if tc.qNonEmpty {
				if gotQ == "" {
					t.Fatalf("expected non-empty fallback question, got empty")
				}
				return
			}
			if gotQ != tc.wantQ {
				t.Fatalf("question = %q, want %q", gotQ, tc.wantQ)
			}
		})
	}
}

// --- 2. Dispatch validation (must not reach worker spawn) ------------------

func TestDispatchValidation(t *testing.T) {
	h := newHarness(t)

	// Unknown slug -> "no task".
	wantErr(t, h.r.Dispatch(h.projectID, "ghost"), "no task")

	// Non-dispatchable statuses (Done / Running / NeedsInput) -> "not dispatchable".
	for _, st := range []store.Status{store.Done, store.Running, store.NeedsInput} {
		slug := "status-" + string(st)
		h.addTask(slug, nil)
		h.mutate(slug, func(tk *store.Task) { tk.Status = st })
		wantErr(t, h.r.Dispatch(h.projectID, slug), "not dispatchable")
	}

	// Dependency exists but is not Done -> error mentioning the dependency.
	h.addTask("dep-todo", nil) // stays Todo
	h.addTask("needs-dep", []string{"dep-todo"})
	err := h.r.Dispatch(h.projectID, "needs-dep")
	wantErr(t, err, "dep-todo")
	if !strings.Contains(err.Error(), "must be done") {
		t.Fatalf("dep-not-done error %q missing 'must be done'", err.Error())
	}

	// Dependency slug is unknown -> "depends on unknown task".
	h.addTask("needs-ghost", []string{"ghostdep"})
	wantErr(t, h.r.Dispatch(h.projectID, "needs-ghost"), "depends on unknown task")

	// NOTE: the all-deps-done happy path and the "already has a live worker"
	// branch both require spawning a real worker (claude) or a populated
	// r.workers map, which is impossible to set up hermetically without adding
	// production seams — intentionally left untested.
}

// --- 3. Delete -------------------------------------------------------------

func TestDelete(t *testing.T) {
	h := newHarness(t)

	// No such task.
	wantErr(t, h.r.Delete(h.projectID, "ghost"), "no task")

	// Running task is refused.
	h.addTask("running1", nil)
	h.mutate("running1", func(tk *store.Task) { tk.Status = store.Running })
	wantErr(t, h.r.Delete(h.projectID, "running1"), "running")

	// Todo task with no branch/worktree: succeeds, then gone.
	h.addTask("todo1", nil)
	if err := h.r.Delete(h.projectID, "todo1"); err != nil {
		t.Fatalf("Delete todo1: %v", err)
	}
	if tk := h.get("todo1"); tk != nil {
		t.Fatalf("expected todo1 deleted, got %+v", tk)
	}

	// Task with a real branch + worktree: dir removed, branch gone, task gone.
	h.addTask("wt1", nil)
	wt := h.addWorktree("wt1")
	h.mutate("wt1", func(tk *store.Task) { tk.Branch = "rambl/wt1" })
	if err := h.r.Delete(h.projectID, "wt1"); err != nil {
		t.Fatalf("Delete wt1: %v", err)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("worktree %s should be gone, stat err = %v", wt, err)
	}
	if out := strings.TrimSpace(runGit(t, h.repo, "branch", "--list", "rambl/wt1")); out != "" {
		t.Fatalf("branch rambl/wt1 should be gone, got %q", out)
	}
	if tk := h.get("wt1"); tk != nil {
		t.Fatalf("expected wt1 deleted, got %+v", tk)
	}
}

// --- 4. Diff ---------------------------------------------------------------

func TestDiff(t *testing.T) {
	h := newHarness(t)

	// Empty branch -> error.
	h.addTask("nobranch", nil)
	_, err := h.r.Diff(h.projectID, "nobranch")
	wantErr(t, err, "no branch")

	// Branch identical to base -> "(no changes ...)" sentinel.
	h.addTask("same", nil)
	h.addWorktree("same")
	h.mutate("same", func(tk *store.Task) { tk.Branch = "rambl/same" })
	out, err := h.r.Diff(h.projectID, "same")
	if err != nil {
		t.Fatalf("Diff same: %v", err)
	}
	if !strings.Contains(out, "no changes") {
		t.Fatalf("expected no-changes sentinel, got %q", out)
	}

	// Branch that adds a file -> diff contains filename + hunk marker.
	h.addTask("added", nil)
	wt := h.addWorktree("added")
	if err := os.WriteFile(filepath.Join(wt, "added.txt"), []byte("hello world\n"), 0o644); err != nil {
		t.Fatalf("write added.txt: %v", err)
	}
	runGit(t, wt, "add", "-A")
	runGit(t, wt, "commit", "-m", "add file")
	h.mutate("added", func(tk *store.Task) { tk.Branch = "rambl/added" })
	out, err = h.r.Diff(h.projectID, "added")
	if err != nil {
		t.Fatalf("Diff added: %v", err)
	}
	if !strings.Contains(out, "added.txt") {
		t.Fatalf("diff missing filename: %q", out)
	}
	if !strings.Contains(out, "@@") {
		t.Fatalf("diff missing hunk marker '@@': %q", out)
	}

	// Truncation: a diff exceeding 60000 bytes is capped + noted.
	h.addTask("big", nil)
	wtBig := h.addWorktree("big")
	var b strings.Builder
	line := strings.Repeat("x", 80) + "\n"
	for i := 0; i < 2000; i++ { // ~162KB of added content
		b.WriteString(line)
	}
	if err := os.WriteFile(filepath.Join(wtBig, "big.txt"), []byte(b.String()), 0o644); err != nil {
		t.Fatalf("write big.txt: %v", err)
	}
	runGit(t, wtBig, "add", "-A")
	runGit(t, wtBig, "commit", "-m", "big file")
	h.mutate("big", func(tk *store.Task) { tk.Branch = "rambl/big" })
	out, err = h.r.Diff(h.projectID, "big")
	if err != nil {
		t.Fatalf("Diff big: %v", err)
	}
	if !strings.Contains(out, "diff truncated at 60000") {
		t.Fatalf("expected truncation note, got tail: %q", out[max(0, len(out)-200):])
	}
	if len(out) > 61000 {
		t.Fatalf("truncated diff too large: %d bytes", len(out))
	}
}

// --- 5. Verify -------------------------------------------------------------

func TestVerify(t *testing.T) {
	h := newHarness(t)

	// worktreeBase "" -> "no worktree base configured" (task must still exist).
	noBase := New(h.st, h.repo, "HEAD", "", "")
	h.addTask("vbase", nil)
	_, err := noBase.Verify(h.projectID, "vbase", "true")
	wantErr(t, err, "no worktree base configured")

	// Worktree dir missing on disk -> "no worktree for".
	h.addTask("vmissing", nil)
	_, err = h.r.Verify(h.projectID, "vmissing", "true")
	wantErr(t, err, "no worktree for")

	// Explicit "true" against an existing worktree dir -> PASSED.
	h.addTask("vpass", nil)
	h.mkWorktreeDir("vpass")
	out, err := h.r.Verify(h.projectID, "vpass", "true")
	if err != nil {
		t.Fatalf("Verify vpass: %v", err)
	}
	if !strings.HasPrefix(out, "VERIFY PASSED") {
		t.Fatalf("expected VERIFY PASSED, got %q", out)
	}

	// Explicit "exit 1" -> FAILED.
	h.addTask("vfail", nil)
	h.mkWorktreeDir("vfail")
	out, err = h.r.Verify(h.projectID, "vfail", "exit 1")
	if err != nil {
		t.Fatalf("Verify vfail: %v", err)
	}
	if !strings.HasPrefix(out, "VERIFY FAILED") {
		t.Fatalf("expected VERIFY FAILED, got %q", out)
	}

	// Empty command with no go.mod -> "could not auto-detect".
	h.addTask("vauto", nil)
	h.mkWorktreeDir("vauto")
	_, err = h.r.Verify(h.projectID, "vauto", "")
	wantErr(t, err, "could not auto-detect")

	// Empty command WITH a minimal Go module -> auto-detects, PASSED.
	h.addTask("vgomod", nil)
	dir := h.mkWorktreeDir("vgomod")
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module verifytest\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "lib.go"), []byte("package verifytest\n\nfunc Add(a, b int) int { return a + b }\n"), 0o644); err != nil {
		t.Fatalf("write lib.go: %v", err)
	}
	out, err = h.r.Verify(h.projectID, "vgomod", "")
	if err != nil {
		t.Fatalf("Verify vgomod: %v", err)
	}
	if !strings.HasPrefix(out, "VERIFY PASSED") {
		t.Fatalf("expected auto-detected VERIFY PASSED, got %q", out)
	}

	// Output capping: a command emitting >30000 bytes is capped + noted.
	h.addTask("vbig", nil)
	h.mkWorktreeDir("vbig")
	out, err = h.r.Verify(h.projectID, "vbig", "head -c 40000 /dev/zero | tr '\\0' a")
	if err != nil {
		t.Fatalf("Verify vbig: %v", err)
	}
	if !strings.Contains(out, "output truncated at 30000") {
		t.Fatalf("expected output truncation note, got %d bytes", len(out))
	}
}

// --- 6. Revise -------------------------------------------------------------

func TestReviseValidation(t *testing.T) {
	h := newHarness(t)

	// No task -> error.
	wantErr(t, h.r.Revise(h.projectID, "ghost", "please fix"), "no task")

	// Task with empty branch -> error.
	h.addTask("rv-nobranch", nil)
	wantErr(t, h.r.Revise(h.projectID, "rv-nobranch", "please fix"), "no branch")

	// Running task -> error (needs a non-empty branch to reach the running check).
	h.addTask("rv-running", nil)
	h.mutate("rv-running", func(tk *store.Task) {
		tk.Branch = "rambl/rv-running"
		tk.Status = store.Running
	})
	wantErr(t, h.r.Revise(h.projectID, "rv-running", "please fix"), "is running")

	// NOTE: the reopen success path spawns a real worker (claude) and is not
	// tested here.
}

// --- 7. OpenPR helpers -----------------------------------------------------

func TestDefaultBase(t *testing.T) {
	h := newHarness(t)

	// No origin/HEAD symbolic ref -> falls back to "main".
	if got := h.r.defaultBase(); got != "main" {
		t.Fatalf("defaultBase with no origin = %q, want \"main\"", got)
	}

	// With origin/HEAD pointing at origin/trunk -> "trunk".
	runGit(t, h.repo, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/trunk")
	if got := h.r.defaultBase(); got != "trunk" {
		t.Fatalf("defaultBase with origin/HEAD->trunk = %q, want \"trunk\"", got)
	}
}

func TestOpenPRValidation(t *testing.T) {
	h := newHarness(t)

	// No task -> error.
	_, err := h.r.OpenPR(h.projectID, "ghost", "", "")
	wantErr(t, err, "no task")

	// Empty branch -> error (returns before any push/gh call).
	h.addTask("pr-nobranch", nil)
	_, err = h.r.OpenPR(h.projectID, "pr-nobranch", "", "")
	wantErr(t, err, "no branch")
}

// TestOpenPRGhStep exercises the gh-failure branch ONLY when gh is genuinely
// absent, using a local bare "origin" so the push succeeds and the call fails
// at the gh step (never creating a real PR). Skipped when gh is installed.
func TestOpenPRGhStep(t *testing.T) {
	if _, err := exec.LookPath("gh"); err == nil {
		t.Skip("gh is installed; skipping to avoid creating a real PR")
	}
	h := newHarness(t)

	// Bare repo acting as origin so PushBranch succeeds.
	bare := t.TempDir()
	runGit(t, bare, "init", "--bare")
	runGit(t, h.repo, "remote", "add", "origin", bare)

	h.addTask("prgh", nil)
	wt := h.addWorktree("prgh")
	if err := os.WriteFile(filepath.Join(wt, "x.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatalf("write x.txt: %v", err)
	}
	runGit(t, wt, "add", "-A")
	runGit(t, wt, "commit", "-m", "change")
	h.mutate("prgh", func(tk *store.Task) { tk.Branch = "rambl/prgh" })

	_, err := h.r.OpenPR(h.projectID, "prgh", "title", "body")
	wantErr(t, err, "gh pr create failed")
}

// --- feature lifecycle ------------------------------------------------------

func TestStartFeatureAndTaskBase(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	h := newHarness(t)

	f, err := h.st.AddFeature(h.projectID, "auth", "Auth feature")
	if err != nil {
		t.Fatalf("AddFeature: %v", err)
	}

	got, err := h.r.StartFeature(h.projectID, "auth")
	if err != nil {
		t.Fatalf("StartFeature: %v", err)
	}
	if got.Branch != "rambl/feat/auth" {
		t.Errorf("feature branch = %q, want rambl/feat/auth", got.Branch)
	}
	if got.Status != store.FeatureRunning {
		t.Errorf("feature status = %q, want %q", got.Status, store.FeatureRunning)
	}

	// Persisted to the store.
	reloaded, err := h.st.GetFeature(h.projectID, "auth")
	if err != nil {
		t.Fatalf("GetFeature: %v", err)
	}
	if reloaded.Branch != "rambl/feat/auth" || reloaded.Status != store.FeatureRunning {
		t.Errorf("persisted feature = {%q,%q}, want {rambl/feat/auth,%q}", reloaded.Branch, reloaded.Status, store.FeatureRunning)
	}

	// Real branch and worktree at the contract path.
	if !worker.BranchExists(h.repo, "rambl/feat/auth") {
		t.Errorf("branch rambl/feat/auth should exist")
	}
	wt := filepath.Join(h.worktreeBase, h.projectID, "@feat-auth")
	if _, err := os.Stat(wt); err != nil {
		t.Errorf("feature worktree should exist at %s: %v", wt, err)
	}

	// Idempotent: a second call is a no-op returning the current feature.
	again, err := h.r.StartFeature(h.projectID, "auth")
	if err != nil {
		t.Fatalf("StartFeature (idempotent): %v", err)
	}
	if again.Branch != "rambl/feat/auth" {
		t.Errorf("idempotent StartFeature branch = %q", again.Branch)
	}

	// taskBase: standalone task → r.base; feature task → feature branch.
	standalone := h.addTask("solo", nil)
	base, err := h.r.taskBase(h.projectID, standalone)
	if err != nil {
		t.Fatalf("taskBase standalone: %v", err)
	}
	if base != h.r.base {
		t.Errorf("taskBase standalone = %q, want %q", base, h.r.base)
	}

	featTask, err := h.st.AddTaskToFeature(h.projectID, f.ID, "login", "Login", "do login", nil)
	if err != nil {
		t.Fatalf("AddTaskToFeature: %v", err)
	}
	base, err = h.r.taskBase(h.projectID, featTask)
	if err != nil {
		t.Fatalf("taskBase feature: %v", err)
	}
	if base != "rambl/feat/auth" {
		t.Errorf("taskBase feature = %q, want rambl/feat/auth", base)
	}

	// CleanupFeature removes the worktree and branch.
	if err := h.r.CleanupFeature(h.projectID, "auth"); err != nil {
		t.Fatalf("CleanupFeature: %v", err)
	}
	if worker.BranchExists(h.repo, "rambl/feat/auth") {
		t.Errorf("branch rambl/feat/auth should be gone after CleanupFeature")
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Errorf("feature worktree should be gone, stat err = %v", err)
	}
}

func TestStartFeatureUnknown(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	h := newHarness(t)
	_, err := h.r.StartFeature(h.projectID, "nope")
	wantErr(t, err, "no feature")
}

// --- squash merge + topo order ---------------------------------------------

// commitTaskBranch creates rambl/<slug> off base in the repo and commits a file
// change, via a throwaway worktree so the main checkout is untouched.
func (h *harness) commitTaskBranch(slug, file, content string) {
	h.t.Helper()
	wt := filepath.Join(h.t.TempDir(), "tb-"+slug)
	runGit(h.t, h.repo, "worktree", "add", "-b", "rambl/"+slug, wt, "HEAD")
	if err := os.WriteFile(filepath.Join(wt, file), []byte(content), 0o644); err != nil {
		h.t.Fatalf("write %s: %v", file, err)
	}
	runGit(h.t, wt, "add", "-A")
	runGit(h.t, wt, "commit", "-m", "change "+file)
	runGit(h.t, h.repo, "worktree", "remove", "--force", wt)
}

func TestMergeTaskIntoFeatureOrdering(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	h := newHarness(t)

	f, err := h.st.AddFeature(h.projectID, "auth", "Auth feature")
	if err != nil {
		t.Fatalf("AddFeature: %v", err)
	}
	if _, err := h.r.StartFeature(h.projectID, "auth"); err != nil {
		t.Fatalf("StartFeature: %v", err)
	}

	if _, err := h.st.AddTaskToFeature(h.projectID, f.ID, "a", "Task A", "do a", nil); err != nil {
		t.Fatalf("AddTaskToFeature a: %v", err)
	}
	if _, err := h.st.AddTaskToFeature(h.projectID, f.ID, "b", "Task B", "do b", nil); err != nil {
		t.Fatalf("AddTaskToFeature b: %v", err)
	}
	// Disjoint file changes on each task branch.
	h.commitTaskBranch("a", "a.txt", "a\n")
	h.commitTaskBranch("b", "b.txt", "b\n")

	if err := h.r.MergeTaskIntoFeature(h.projectID, "auth", "a"); err != nil {
		t.Fatalf("MergeTaskIntoFeature a: %v", err)
	}
	if err := h.r.MergeTaskIntoFeature(h.projectID, "auth", "b"); err != nil {
		t.Fatalf("MergeTaskIntoFeature b: %v", err)
	}

	wt := filepath.Join(h.worktreeBase, h.projectID, "@feat-auth")
	// Both files present on the feature branch.
	for _, fn := range []string{"a.txt", "b.txt"} {
		if _, err := os.Stat(filepath.Join(wt, fn)); err != nil {
			t.Errorf("%s should exist on feature worktree: %v", fn, err)
		}
	}
	// Exactly two commits on top of base, with the expected subjects.
	out := runGit(t, wt, "log", "--format=%s", "HEAD~2..HEAD")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 commits on top of base, got %d: %q", len(lines), out)
	}
	// log is newest-first: b then a.
	if lines[0] != "feat(b): Task B" || lines[1] != "feat(a): Task A" {
		t.Errorf("commit subjects = %v, want [feat(b): Task B, feat(a): Task A]", lines)
	}
}

func TestMergeTaskIntoFeatureConflict(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	h := newHarness(t)

	f, err := h.st.AddFeature(h.projectID, "auth", "Auth feature")
	if err != nil {
		t.Fatalf("AddFeature: %v", err)
	}
	if _, err := h.r.StartFeature(h.projectID, "auth"); err != nil {
		t.Fatalf("StartFeature: %v", err)
	}
	if _, err := h.st.AddTaskToFeature(h.projectID, f.ID, "a", "Task A", "do a", nil); err != nil {
		t.Fatalf("AddTaskToFeature a: %v", err)
	}
	if _, err := h.st.AddTaskToFeature(h.projectID, f.ID, "b", "Task B", "do b", nil); err != nil {
		t.Fatalf("AddTaskToFeature b: %v", err)
	}
	// Both edit the same line of README.md (from newHarness's repo).
	h.commitTaskBranch("a", "README.md", "from-a\n")
	h.commitTaskBranch("b", "README.md", "from-b\n")

	if err := h.r.MergeTaskIntoFeature(h.projectID, "auth", "a"); err != nil {
		t.Fatalf("MergeTaskIntoFeature a: %v", err)
	}
	err = h.r.MergeTaskIntoFeature(h.projectID, "auth", "b")
	var mce *MergeConflictError
	if !errors.As(err, &mce) {
		t.Fatalf("expected *MergeConflictError, got %v", err)
	}
	if mce.Feature != "auth" || mce.Task != "b" {
		t.Errorf("conflict err fields = {%q,%q}, want {auth,b}", mce.Feature, mce.Task)
	}
	found := false
	for _, fn := range mce.Files {
		if fn == "README.md" {
			found = true
		}
	}
	if !found {
		t.Errorf("conflict files = %v, want to include README.md", mce.Files)
	}
	// Feature worktree left clean, no merge in progress.
	wt := filepath.Join(h.worktreeBase, h.projectID, "@feat-auth")
	st := runGit(t, wt, "status", "--porcelain")
	if strings.TrimSpace(st) != "" {
		t.Errorf("feature worktree should be clean after conflict, got %q", st)
	}
}

func slugs(tasks []*store.Task) []string {
	out := make([]string, len(tasks))
	for i, t := range tasks {
		out[i] = t.Slug
	}
	return out
}

func TestTopoOrder(t *testing.T) {
	tk := func(slug string, deps ...string) *store.Task {
		return &store.Task{Slug: slug, Deps: deps}
	}
	eq := func(got, want []string) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}

	// Linear chain a -> b -> c (a depends on b depends on c).
	chain := []*store.Task{tk("a", "b"), tk("b", "c"), tk("c")}
	got, err := TopoOrder(chain)
	if err != nil {
		t.Fatalf("chain: %v", err)
	}
	if want := []string{"c", "b", "a"}; !eq(slugs(got), want) {
		t.Errorf("chain order = %v, want %v", slugs(got), want)
	}

	// Diamond: d depends on b and c; b and c depend on a. Ties break by slug.
	diamond := []*store.Task{tk("d", "b", "c"), tk("b", "a"), tk("c", "a"), tk("a")}
	got, err = TopoOrder(diamond)
	if err != nil {
		t.Fatalf("diamond: %v", err)
	}
	if want := []string{"a", "b", "c", "d"}; !eq(slugs(got), want) {
		t.Errorf("diamond order = %v, want %v", slugs(got), want)
	}

	// All independent → ascending slug order.
	indep := []*store.Task{tk("c"), tk("a"), tk("b")}
	got, err = TopoOrder(indep)
	if err != nil {
		t.Fatalf("indep: %v", err)
	}
	if want := []string{"a", "b", "c"}; !eq(slugs(got), want) {
		t.Errorf("indep order = %v, want %v", slugs(got), want)
	}

	// Deps referencing slugs outside the set are ignored.
	external := []*store.Task{tk("a", "external"), tk("b", "a")}
	got, err = TopoOrder(external)
	if err != nil {
		t.Fatalf("external: %v", err)
	}
	if want := []string{"a", "b"}; !eq(slugs(got), want) {
		t.Errorf("external order = %v, want %v", slugs(got), want)
	}

	// Cycle a <-> b → error.
	cycle := []*store.Task{tk("a", "b"), tk("b", "a")}
	if _, err := TopoOrder(cycle); err == nil {
		t.Errorf("expected error for cycle, got nil")
	}
}
