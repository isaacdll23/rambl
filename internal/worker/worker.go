// Package worker is the core abstraction: an autonomous Claude Code worker that
// owns an isolated git worktree, runs to completion with push-based turn
// signalling, supports multi-turn follow-ups, and reports its result.
package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"rambl/internal/hook"
	"rambl/internal/session"
	"rambl/internal/transcript"
)

// gitMu serialises mutating git operations against the shared repo (concurrent
// `git worktree add` / branch ops can race on the index lock).
var gitMu sync.Mutex

// Spec describes a unit of work.
type Spec struct {
	ID           string   // task id; used to name the branch and worktree
	Prompt       string   // the task instruction (first turn)
	RepoPath     string   // path to the main git repository
	Base         string   // base ref for the worktree branch (default "HEAD")
	MergeRefs    []string // refs (dependency branches) merged into the worktree before running
	SystemPrompt string   // appended to the worker's system prompt (autonomy + outcome protocol)
	Worktree     string   // explicit absolute worktree path; if empty, defaults to <RepoPath>/.rambl/worktrees/<ID>
	Reopen       bool     // if true, attach to the existing branch+worktree instead of creating them (used to iterate on a task's prior output)
	Branch       string   // explicit branch name; when "", defaults to "rambl/"+ID
}

// branchName returns the worktree branch for a spec: an explicit Branch when
// set, else the default "rambl/"+ID.
func branchName(spec Spec) string {
	if spec.Branch != "" {
		return spec.Branch
	}
	return "rambl/" + spec.ID
}

// Turn is the outcome of one prompt→completion cycle.
type Turn struct {
	Reply         string // final assistant text of the turn
	DurationMs    int    // from the transcript's system summary line
	TimedOut      bool   // true if we fell back to timeout instead of a Stop signal
	ProcessExited bool   // the claude process exited before signalling turn completion
	ExitReason    string // diagnostic: exit error + last session output (set when ProcessExited)
}

// Worker manages one worktree-isolated autonomous session.
type Worker struct {
	Spec        Spec
	Branch      string
	Worktree    string
	SessionID   string
	TurnTimeout time.Duration // per-turn cap (default 5m)

	sess     *session.Session
	tail     *transcript.Tailer
	hookln   *hook.Listener
	settings string // generated settings file path
	cancel   context.CancelFunc
}

// New builds a worker for spec. selfExe must be the absolute path of this
// binary (os.Executable()) so the Stop hook can invoke it.
func New(spec Spec) *Worker {
	if spec.Base == "" {
		spec.Base = "HEAD"
	}
	return &Worker{Spec: spec, TurnTimeout: 5 * time.Minute}
}

// Start creates the worktree, wires the Stop-hook socket + settings, starts the
// transcript tailer, and spawns the autonomous session (ready for Send).
func (w *Worker) Start(ctx context.Context, selfExe string) error {
	w.Branch = branchName(w.Spec)
	if w.Spec.Worktree != "" {
		w.Worktree = w.Spec.Worktree
	} else {
		w.Worktree = filepath.Join(w.Spec.RepoPath, ".rambl", "worktrees", w.Spec.ID)
	}

	if w.Spec.Reopen {
		// Attach to the worker's prior output instead of creating a fresh
		// worktree+branch — used to iterate on a task that already ran.
		if _, err := os.Stat(w.Worktree); err != nil {
			return fmt.Errorf("cannot reopen %q: worktree %s missing: %v", w.Spec.ID, w.Worktree, err)
		}
	} else {
		if err := os.MkdirAll(filepath.Dir(w.Worktree), 0o755); err != nil {
			return err
		}
		out, err := ensureWorktree(w.Spec.RepoPath, w.Worktree, w.Branch, w.Spec.Base)
		if err != nil {
			return fmt.Errorf("git worktree add: %v: %s", err, out)
		}

		// Integrate dependency outputs by merging their branches into this
		// worktree, so a downstream task actually sees upstream code. A conflict is
		// reported so the orchestrator can mark the task blocked.
		for _, ref := range w.Spec.MergeRefs {
			if out, err := gitID(w.Worktree, "merge", "--no-edit", "-m", "rambl: merge "+ref, ref); err != nil {
				_, _ = gitID(w.Worktree, "merge", "--abort")
				w.rollback()
				return fmt.Errorf("merge %s into %s failed (conflict?): %v: %s", ref, w.Spec.ID, err, out)
			}
		}
	}

	var err error
	if w.hookln, err = hook.NewListener(); err != nil {
		return err
	}
	if w.settings, err = writeStopHookSettings(w.hookln.Command(selfExe)); err != nil {
		return err
	}

	w.tail = transcript.NewTailer(w.Worktree) // snapshot BEFORE spawning
	tctx, cancel := context.WithCancel(ctx)
	w.cancel = cancel
	go w.tail.Run(tctx)

	args := []string{"--dangerously-skip-permissions"}
	if w.Spec.SystemPrompt != "" {
		args = append(args, "--append-system-prompt", w.Spec.SystemPrompt)
	}
	cfg := session.Config{
		Dir:          w.Worktree,
		ExtraArgs:    args,
		SettingsPath: w.settings,
		AcceptBypass: true,
	}
	if p := os.Getenv("RAMBL_DEBUG_PTY"); p != "" {
		if f, ferr := os.Create(p); ferr == nil {
			cfg.LogWriter = f // tee raw PTY stream for debugging
		}
	}
	w.sess, err = session.Start(cfg)
	if err != nil {
		cancel()
		_ = w.hookln.Close()
		_ = os.Remove(w.settings)
		w.rollback() // a failed Start leaves no trace
		return err
	}
	return nil
}

// ensureWorktree creates branch from base in a new worktree at worktreePath:
//
//	git worktree add -b <branch> <worktreePath> <base>
//
// If that fails because a stale worktree dir and/or branch from a prior
// interrupted run already exist, it reclaims them (worktree remove --force,
// worktree prune, branch -D) and retries the add exactly once. Serialised on
// gitMu. Returns the final git output and error. base "" is treated as "HEAD".
func ensureWorktree(repoPath, worktreePath, branch, base string) (string, error) {
	if base == "" {
		base = "HEAD"
	}
	gitMu.Lock()
	defer gitMu.Unlock()

	out, err := git(repoPath, "worktree", "add", "-b", branch, worktreePath, base)
	if err == nil {
		return out, nil
	}
	// Only reclaim when the failure is a stale worktree/branch left by a prior
	// interrupted run; any other error surfaces unchanged.
	lc := strings.ToLower(out)
	reclaimable := strings.Contains(lc, "already exists") ||
		strings.Contains(lc, "already used by worktree") ||
		strings.Contains(lc, "already checked out")
	if !reclaimable {
		return out, err
	}
	// Reclaim and retry exactly once (all best-effort).
	_, _ = git(repoPath, "worktree", "remove", "--force", worktreePath)
	_, _ = git(repoPath, "worktree", "prune")
	_, _ = git(repoPath, "branch", "-D", branch)
	return git(repoPath, "worktree", "add", "-b", branch, worktreePath, base)
}

// rollback removes this worker's worktree and branch (best-effort).
func (w *Worker) rollback() {
	gitMu.Lock()
	defer gitMu.Unlock()
	_, _ = git(w.Spec.RepoPath, "worktree", "remove", "--force", w.Worktree)
	_, _ = git(w.Spec.RepoPath, "branch", "-D", w.Branch)
}

// Commit stages everything in the worktree and commits it to the task branch,
// so the branch becomes a usable base for dependent tasks. No-op if nothing changed.
func (w *Worker) Commit(message string) error {
	if _, err := gitID(w.Worktree, "add", "-A"); err != nil {
		return err
	}
	// Nothing staged → success without an empty commit.
	if _, err := gitID(w.Worktree, "diff", "--cached", "--quiet"); err == nil {
		return nil
	}
	out, err := gitID(w.Worktree, "commit", "-m", message)
	if err != nil {
		return fmt.Errorf("commit: %v: %s", err, out)
	}
	return nil
}

// SalvageCommit stages and commits all current worktree changes to the branch
// as a WIP commit, so partial progress survives a non-success exit. It returns
// committed=false with a nil error when there is nothing to commit. summary is a
// short human-readable diffstat of what was salvaged (empty when committed is false).
func (w *Worker) SalvageCommit(message string) (committed bool, summary string, err error) {
	// No worktree → nothing to salvage. Guards against running git in the
	// process's cwd when Worktree was never set.
	if w.Worktree == "" {
		return false, "", nil
	}
	summary, _ = w.Changes() // best-effort; ignore error for the summary
	if _, err := gitID(w.Worktree, "add", "-A"); err != nil {
		return false, "", err
	}
	// Nothing staged → no commit, and no salvage summary to report.
	if _, err := gitID(w.Worktree, "diff", "--cached", "--quiet"); err == nil {
		return false, "", nil
	}
	if out, err := gitID(w.Worktree, "commit", "-m", message); err != nil {
		return false, "", fmt.Errorf("commit: %v: %s", err, out)
	}
	return true, summary, nil
}

// Run executes the spec's first prompt and returns the resulting turn.
func (w *Worker) Run(ctx context.Context) (Turn, error) {
	return w.Send(ctx, w.Spec.Prompt)
}

// Send issues a prompt and waits for the turn to complete, preferring the Stop
// hook (push) and falling back to the turn timeout. Safe to call repeatedly for
// multi-turn follow-ups.
func (w *Worker) Send(ctx context.Context, prompt string) (Turn, error) {
	// Drain any stale Stop event so we wait for THIS turn's completion.
	select {
	case <-w.hookln.C:
	default:
	}

	if err := w.sess.Send(prompt); err != nil {
		return Turn{}, err
	}

	var timedOut bool
	var processExited bool
	select {
	case p := <-w.hookln.C:
		if p.SessionID != "" {
			w.SessionID = p.SessionID
		}
	case <-w.sess.Exited():
		// The process died mid-turn (crash/exit) — fail fast instead of
		// blocking until the timeout.
		processExited = true
	case <-time.After(w.TurnTimeout):
		timedOut = true
	case <-ctx.Done():
		return Turn{}, ctx.Err()
	}

	time.Sleep(400 * time.Millisecond) // let the transcript flush after Stop
	sid, reply, dur := w.tail.Latest()
	if sid != "" {
		w.SessionID = sid
	}
	t := Turn{Reply: reply, DurationMs: dur, TimedOut: timedOut}
	if processExited {
		t.ProcessExited = true
		t.ExitReason = fmt.Sprintf("session exited (%v): %s", w.sess.ExitErr(), w.sess.Tail())
	}
	return t, nil
}

// Activity returns the worker's live tool-activity feed (nil if not started).
func (w *Worker) Activity() []transcript.Activity {
	if w.tail == nil {
		return nil
	}
	return w.tail.Recent()
}

// SessionTail returns the last chunk of the session's PTY output, for diagnostics.
func (w *Worker) SessionTail() string {
	if w.sess == nil {
		return ""
	}
	return w.sess.Tail()
}

// Changes returns a short summary of what the worker did to its worktree
// (porcelain status plus a diffstat including untracked files).
func (w *Worker) Changes() (string, error) {
	if _, err := git(w.Worktree, "add", "-A", "-N"); err != nil { // intent-to-add untracked
		return "", err
	}
	status, err := git(w.Worktree, "status", "--short")
	if err != nil {
		return "", err
	}
	stat, err := git(w.Worktree, "diff", "--stat")
	if err != nil {
		return "", err
	}
	return strings.TrimRight(status+"\n"+stat, "\n"), nil
}

// Close ends the session and the tailer. The worktree/branch are left intact
// for review; call Cleanup to remove them.
func (w *Worker) Close() error {
	if w.cancel != nil {
		w.cancel()
	}
	var err error
	if w.sess != nil {
		err = w.sess.Close()
	}
	if w.hookln != nil {
		_ = w.hookln.Close()
	}
	if w.settings != "" {
		_ = os.Remove(w.settings)
	}
	return err
}

// Cleanup removes the worktree and its branch. Irreversible — only call once
// the work has been merged or discarded.
func (w *Worker) Cleanup() error {
	gitMu.Lock()
	defer gitMu.Unlock()
	if _, err := git(w.Spec.RepoPath, "worktree", "remove", "--force", w.Worktree); err != nil {
		return err
	}
	_, err := git(w.Spec.RepoPath, "branch", "-D", w.Branch)
	return err
}

// CleanupWorktree removes a worktree directory and deletes its branch
// (best-effort, force). Safe to call when no live worker exists. Uses the
// shared gitMu lock to avoid racing concurrent git operations on the repo.
func CleanupWorktree(repoPath, worktreePath, branch string) error {
	gitMu.Lock()
	defer gitMu.Unlock()
	if worktreePath != "" {
		_, _ = git(repoPath, "worktree", "remove", "--force", worktreePath)
	}
	if branch != "" {
		_, _ = git(repoPath, "branch", "-D", branch)
	}
	return nil
}

// AddFeatureWorktree creates featureBranch from base and checks it out in a new
// worktree at worktreePath: `git worktree add -b <featureBranch> <worktreePath> <base>`.
// Uses the gitID identity helper. Errors if the branch or worktree already exists.
func AddFeatureWorktree(repoPath, worktreePath, featureBranch, base string) error {
	if base == "" {
		base = "HEAD"
	}
	if err := os.MkdirAll(filepath.Dir(worktreePath), 0o755); err != nil {
		return err
	}
	gitMu.Lock()
	defer gitMu.Unlock()
	out, err := gitID(repoPath, "worktree", "add", "-b", featureBranch, worktreePath, base)
	if err != nil {
		return fmt.Errorf("git worktree add: %v: %s", err, out)
	}
	return nil
}

// SquashMerge squashes taskBranch into the branch checked out in featureWorktree,
// producing exactly one commit with the given message. It runs, in featureWorktree:
//
//	git merge --squash <taskBranch>
//
// then commits via the gitID identity helper. Behavior:
//   - On conflict: abort cleanly (git merge --abort / reset --hard so the worktree
//     is left clean with no merge in progress) and return (true, err) naming the
//     conflicted files.
//   - When the squash stages no changes (task added nothing): return (false, nil)
//     WITHOUT creating an empty commit.
//   - Otherwise commit and return (false, nil).
func SquashMerge(featureWorktree, taskBranch, message string) (conflict bool, err error) {
	gitMu.Lock()
	defer gitMu.Unlock()

	if out, mergeErr := gitID(featureWorktree, "merge", "--squash", taskBranch); mergeErr != nil {
		// Capture conflicted paths before cleaning the worktree.
		files, _ := ConflictedFiles(featureWorktree)
		// `git merge --squash` records no MERGE_HEAD, so `git merge --abort` is a
		// best-effort no-op; `git reset --hard` reliably clears the staged squash
		// and any conflict markers, leaving the worktree clean.
		_, _ = gitID(featureWorktree, "merge", "--abort")
		_, _ = gitID(featureWorktree, "reset", "--hard", "HEAD")
		if len(files) > 0 {
			return true, fmt.Errorf("squash merge of %s conflicted in: %s", taskBranch, strings.Join(files, ", "))
		}
		return false, fmt.Errorf("git merge --squash %s: %v: %s", taskBranch, mergeErr, out)
	}

	// A squash that staged nothing (the task added no changes vs the feature tip)
	// must not produce an empty commit.
	if _, err := gitID(featureWorktree, "diff", "--cached", "--quiet"); err == nil {
		return false, nil
	}

	if out, err := gitID(featureWorktree, "commit", "-m", message); err != nil {
		return false, fmt.Errorf("commit: %v: %s", err, out)
	}
	return false, nil
}

// ConflictedFiles returns unmerged paths in worktree (git diff --name-only --diff-filter=U).
func ConflictedFiles(worktree string) ([]string, error) {
	out, err := git(worktree, "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return nil, err
	}
	var files []string
	for _, line := range strings.Split(out, "\n") {
		if p := strings.TrimSpace(line); p != "" {
			files = append(files, p)
		}
	}
	return files, nil
}

// BranchExists reports whether branch exists in repoPath.
func BranchExists(repoPath, branch string) bool {
	_, err := git(repoPath, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

// DiffBranch returns the diffstat and full patch of branch relative to base
// (three-dot, i.e. since their merge-base), computed in repoPath. Read-only.
func DiffBranch(repoPath, base, branch string) (stat string, patch string, err error) {
	stat, err = git(repoPath, "diff", "--stat", base+"..."+branch)
	if err != nil {
		return "", "", err
	}
	patch, err = git(repoPath, "diff", base+"..."+branch)
	if err != nil {
		return "", "", err
	}
	return strings.TrimRight(stat, " \t\r\n"), strings.TrimRight(patch, " \t\r\n"), nil
}

// PushBranch pushes branch to remote, setting upstream. Returns combined output.
func PushBranch(repoPath, remote, branch string) (string, error) {
	return git(repoPath, "push", "-u", remote, branch)
}

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// gitID runs git, falling back to a deterministic synthetic identity ONLY when
// the repo at dir has no git user configured. When the repo already has an
// identity (the human's, inherited by the worktree), commits are authored as
// that user — so squash-merges don't credit a synthetic "rambl" co-author.
func gitID(dir string, args ...string) (string, error) {
	var prefix []string
	if !hasGitIdentity(dir) {
		prefix = []string{"-c", "user.name=rambl", "-c", "user.email=rambl@localhost"}
	}
	return git(dir, append(prefix, args...)...)
}

// hasGitIdentity reports whether dir has both user.name and user.email set.
func hasGitIdentity(dir string) bool {
	name, errN := git(dir, "config", "user.name")
	email, errE := git(dir, "config", "user.email")
	return errN == nil && errE == nil &&
		strings.TrimSpace(name) != "" && strings.TrimSpace(email) != ""
}

// writeStopHookSettings writes a temp settings file registering a Stop hook
// that runs the given command, and returns its path.
func writeStopHookSettings(command string) (string, error) {
	settings := map[string]any{
		"hooks": map[string]any{
			"Stop": []any{
				map[string]any{
					"hooks": []any{
						map[string]any{"type": "command", "command": command},
					},
				},
			},
		},
	}
	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp("", "rambl-settings-*.json")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return "", err
	}
	return f.Name(), nil
}
