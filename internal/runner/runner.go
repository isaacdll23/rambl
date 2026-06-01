// Package runner manages live worker sessions on behalf of the PM. It
// dispatches workers (worktree-isolated, autonomous), keeps blocked ones alive
// so the PM can answer them, classifies each turn's outcome via the DONE/BLOCKED
// protocol, and writes status back to the store. The MCP tool layer calls into
// this; it does not spawn workers itself.
package runner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"rambl/internal/store"
	"rambl/internal/worker"
)

// WorkerSystemPrompt is appended to every worker's system prompt. It sets the
// autonomy contract (don't ask the human; assume and proceed) and the
// machine-detectable outcome protocol the runner classifies on.
const WorkerSystemPrompt = `You are an autonomous coding worker. You run with no human watching, inside an isolated git worktree. Your task brief is your COMPLETE specification.

Work autonomously to completion. Do NOT ask the human clarifying questions — make reasonable, well-documented assumptions and proceed.

Signal your outcome with EXACTLY one marker as the final line of your final message:
- When the task is fully complete: a line containing only
    RAMBL_DONE
- Only if you are genuinely blocked and cannot proceed without a decision you cannot reasonably make yourself: a line
    RAMBL_BLOCKED: <one sentence stating exactly what you need decided>

Do not emit either marker until you are actually done or actually blocked. Emit only one.

Before declaring completion, verify your work: make sure it compiles and that the relevant build and tests pass; only emit RAMBL_DONE once it is green.`

// Runner owns the set of live workers for a project.
type Runner struct {
	store        *store.Store
	repoPath     string
	base         string
	selfExe      string
	worktreeBase string
	turnTimeout  time.Duration

	mu      sync.Mutex
	workers map[string]*worker.Worker // keyed by task slug
}

// New constructs a Runner. selfExe is this binary's path (for workers' Stop hook).
func New(st *store.Store, repoPath, base, selfExe, worktreeBase string) *Runner {
	if base == "" {
		base = "HEAD"
	}
	return &Runner{
		store: st, repoPath: repoPath, base: base, selfExe: selfExe,
		worktreeBase: worktreeBase,
		turnTimeout:  5 * time.Minute,
		workers:      map[string]*worker.Worker{},
	}
}

// Dispatch validates the task is runnable, marks it running, and starts the
// worker in the background. Returns quickly; progress is observed via the store.
func (r *Runner) Dispatch(projectID, slug string) error {
	t, err := r.store.GetTask(projectID, slug)
	if err != nil {
		return err
	}
	if t == nil {
		return fmt.Errorf("no task %q", slug)
	}
	if t.Status != store.Todo && t.Status != store.Failed && t.Status != store.Blocked {
		return fmt.Errorf("task %q is %s, not dispatchable", slug, t.Status)
	}
	var mergeRefs []string
	for _, dep := range t.Deps {
		d, err := r.store.GetTask(projectID, dep)
		if err != nil {
			return err
		}
		if d == nil {
			return fmt.Errorf("task %q depends on unknown task %q", slug, dep)
		}
		if d.Status != store.Done {
			return fmt.Errorf("dependency %q is %s (must be done before %q can run)", dep, d.Status, slug)
		}
		mergeRefs = append(mergeRefs, "rambl/"+dep)
	}

	r.mu.Lock()
	if _, exists := r.workers[slug]; exists {
		r.mu.Unlock()
		return fmt.Errorf("task %q already has a live worker", slug)
	}
	r.mu.Unlock()

	t.Status = store.Running
	t.Branch = "rambl/" + slug
	t.Question = ""
	if err := r.store.Update(t); err != nil {
		return err
	}
	go r.start(projectID, slug, t.Prompt, mergeRefs)
	return nil
}

func (r *Runner) start(projectID, slug, prompt string, mergeRefs []string) {
	t, err := r.store.GetTask(projectID, slug)
	if err != nil || t == nil {
		r.fail(projectID, slug, "start: task lookup failed")
		return
	}
	base, err := r.taskBase(projectID, t)
	if err != nil {
		r.fail(projectID, slug, "start: "+err.Error())
		return
	}
	spec := worker.Spec{
		ID: slug, Prompt: prompt, RepoPath: r.repoPath, Base: base,
		MergeRefs: mergeRefs, SystemPrompt: WorkerSystemPrompt,
	}
	if r.worktreeBase != "" {
		spec.Worktree = filepath.Join(r.worktreeBase, projectID, slug)
	}
	w := worker.New(spec)
	w.TurnTimeout = r.turnTimeout

	if err := w.Start(context.Background(), r.selfExe); err != nil {
		r.fail(projectID, slug, "start: "+err.Error())
		return
	}
	r.mu.Lock()
	r.workers[slug] = w
	r.mu.Unlock()

	turn, err := w.Run(context.Background())
	if err != nil {
		r.fail(projectID, slug, "run: "+err.Error())
		return
	}
	r.apply(projectID, slug, w, turn)
}

// featureWorktree returns the integration worktree path for a feature. The
// "@feat-" prefix cannot collide with a kebab-case task slug.
func (r *Runner) featureWorktree(projectID, featureSlug string) string {
	return filepath.Join(r.worktreeBase, projectID, "@feat-"+featureSlug)
}

// featureBranch returns the integration branch name for a feature.
func featureBranch(featureSlug string) string { return "rambl/feat/" + featureSlug }

// taskBase returns the ref a task's worktree should branch from: the feature's
// branch when t.FeatureID != "" (looked up via the store), else r.base.
func (r *Runner) taskBase(projectID string, t *store.Task) (string, error) {
	if t.FeatureID == "" {
		return r.base, nil
	}
	feats, err := r.store.ListFeatures(projectID)
	if err != nil {
		return "", err
	}
	for _, f := range feats {
		if f.ID == t.FeatureID {
			return featureBranch(f.Slug), nil
		}
	}
	return "", fmt.Errorf("task %q references unknown feature id %q", t.Slug, t.FeatureID)
}

// StartFeature creates the feature's integration branch + worktree (if not
// already started), sets Feature.Branch = "rambl/feat/<slug>" and Status =
// FeatureRunning, and persists it. Idempotent: a no-op (returning the current
// feature) if already started.
func (r *Runner) StartFeature(projectID, featureSlug string) (*store.Feature, error) {
	f, err := r.store.GetFeature(projectID, featureSlug)
	if err != nil {
		return nil, err
	}
	if f == nil {
		return nil, fmt.Errorf("no feature %q", featureSlug)
	}
	branch := featureBranch(featureSlug)
	if f.Branch == branch && worker.BranchExists(r.repoPath, branch) {
		return f, nil // already started
	}
	if r.worktreeBase == "" {
		return nil, fmt.Errorf("no worktree base configured")
	}
	wt := r.featureWorktree(projectID, featureSlug)
	if err := worker.AddFeatureWorktree(r.repoPath, wt, branch, r.base); err != nil {
		return nil, err
	}
	f.Branch = branch
	f.Status = store.FeatureRunning
	if err := r.store.UpdateFeature(f); err != nil {
		return nil, err
	}
	return f, nil
}

// CleanupFeature best-effort removes the feature's integration worktree and branch.
func (r *Runner) CleanupFeature(projectID, featureSlug string) error {
	var wt string
	if r.worktreeBase != "" {
		wt = r.featureWorktree(projectID, featureSlug)
	}
	return worker.CleanupWorktree(r.repoPath, wt, featureBranch(featureSlug))
}

// Send pushes a follow-up (e.g. the PM answering a blocked worker) into the
// live session and re-classifies the resulting turn. The worker must be alive
// (status needs_input).
func (r *Runner) Send(projectID, slug, message string) error {
	r.mu.Lock()
	w := r.workers[slug]
	r.mu.Unlock()
	if w == nil {
		return fmt.Errorf("no live worker for %q (it may have finished or never started)", slug)
	}
	t, err := r.store.GetTask(projectID, slug)
	if err != nil {
		return err
	}
	t.Status = store.Running
	t.Question = ""
	if err := r.store.Update(t); err != nil {
		return err
	}
	go func() {
		turn, err := w.Send(context.Background(), message)
		if err != nil {
			r.fail(projectID, slug, "send: "+err.Error())
			return
		}
		r.apply(projectID, slug, w, turn)
	}()
	return nil
}

// Delete retires any live worker for the task, removes its worktree and
// branch, and deletes the task from the store. Refuses a running task.
func (r *Runner) Delete(projectID, slug string) error {
	t, err := r.store.GetTask(projectID, slug)
	if err != nil {
		return err
	}
	if t == nil {
		return fmt.Errorf("no task %q", slug)
	}
	if t.Status == store.Running {
		return fmt.Errorf("task %q is running; cannot delete a running task", slug)
	}
	r.retire(slug)
	var worktreePath string
	if r.worktreeBase != "" {
		worktreePath = filepath.Join(r.worktreeBase, projectID, slug)
	}
	_ = worker.CleanupWorktree(r.repoPath, worktreePath, t.Branch)
	return r.store.DeleteTask(projectID, slug)
}

// Diff returns a human-readable diff (stat + capped patch) of the task's
// branch relative to the runner's base ref, for the PM to review.
func (r *Runner) Diff(projectID, slug string) (string, error) {
	t, err := r.store.GetTask(projectID, slug)
	if err != nil {
		return "", err
	}
	if t == nil {
		return "", fmt.Errorf("no task %q", slug)
	}
	if t.Branch == "" {
		return "", fmt.Errorf("task %q has no branch yet (nothing dispatched)", slug)
	}
	stat, patch, err := worker.DiffBranch(r.repoPath, r.base, t.Branch)
	if err != nil {
		return "", err
	}
	if stat == "" && patch == "" {
		return fmt.Sprintf("(no changes on %s relative to %s)", t.Branch, r.base), nil
	}
	if len(patch) > 60000 {
		origLen := len(patch)
		patch = patch[:60000] + fmt.Sprintf("\n... [diff truncated at 60000 of %d bytes]", origLen)
	}
	return stat + "\n\n" + patch, nil
}

// Verify runs a build/test command inside the task's worktree and returns a
// PASS/FAIL-prefixed combined output, so the PM can validate a worker's work.
func (r *Runner) Verify(projectID, slug, command string) (string, error) {
	t, err := r.store.GetTask(projectID, slug)
	if err != nil {
		return "", err
	}
	if t == nil {
		return "", fmt.Errorf("no task %q", slug)
	}
	if r.worktreeBase == "" {
		return "", fmt.Errorf("no worktree base configured")
	}
	worktreePath := filepath.Join(r.worktreeBase, projectID, slug)
	if _, err := os.Stat(worktreePath); err != nil {
		return "", fmt.Errorf("no worktree for %q (dispatch it first)", slug)
	}
	if command == "" {
		if _, err := os.Stat(filepath.Join(worktreePath, "go.mod")); err == nil {
			command = "go build ./... && go test ./..."
		} else {
			return "", fmt.Errorf("no verify command given and could not auto-detect; pass an explicit command")
		}
	}
	cmd := exec.Command("sh", "-c", command)
	cmd.Dir = worktreePath
	cmd.Env = os.Environ()
	out, runErr := cmd.CombinedOutput()
	output := string(out)
	if len(output) > 30000 {
		origLen := len(output)
		output = output[:30000] + fmt.Sprintf("\n... [output truncated at 30000 of %d bytes]", origLen)
	}
	var result string
	if runErr == nil {
		result = "VERIFY PASSED\n\n"
	} else {
		result = fmt.Sprintf("VERIFY FAILED (%v)\n\n", runErr)
	}
	return result + output, nil
}

// Revise hands a finished task's branch back to a worker with feedback so it
// can iterate. Reuses a live worker if one exists; otherwise reopens the
// existing worktree+branch. The resulting turn is committed on success.
func (r *Runner) Revise(projectID, slug, message string) error {
	t, err := r.store.GetTask(projectID, slug)
	if err != nil {
		return err
	}
	if t == nil {
		return fmt.Errorf("no task %q", slug)
	}
	if t.Branch == "" {
		return fmt.Errorf("task %q has no branch to revise", slug)
	}
	if t.Status == store.Running {
		return fmt.Errorf("task %q is running", slug)
	}

	r.mu.Lock()
	w := r.workers[slug]
	r.mu.Unlock()
	if w != nil {
		return r.Send(projectID, slug, message)
	}

	spec := worker.Spec{
		ID: slug, Prompt: message, RepoPath: r.repoPath, Base: r.base,
		SystemPrompt: WorkerSystemPrompt, Reopen: true,
	}
	if r.worktreeBase != "" {
		spec.Worktree = filepath.Join(r.worktreeBase, projectID, slug)
	}
	nw := worker.New(spec)
	nw.TurnTimeout = r.turnTimeout

	t.Status = store.Running
	t.Question = ""
	if err := r.store.Update(t); err != nil {
		return err
	}

	go func() {
		if err := nw.Start(context.Background(), r.selfExe); err != nil {
			r.fail(projectID, slug, "revise start: "+err.Error())
			return
		}
		r.mu.Lock()
		r.workers[slug] = nw
		r.mu.Unlock()

		turn, err := nw.Run(context.Background())
		if err != nil {
			r.fail(projectID, slug, "revise run: "+err.Error())
			return
		}
		r.apply(projectID, slug, nw, turn)
	}()
	return nil
}

// OpenPR pushes the task's branch to origin and opens a GitHub PR via gh,
// returning gh's output (the PR URL). Requires gh installed/authed and an
// origin remote pointing at GitHub.
func (r *Runner) OpenPR(projectID, slug, title, body string) (string, error) {
	t, err := r.store.GetTask(projectID, slug)
	if err != nil {
		return "", err
	}
	if t == nil {
		return "", fmt.Errorf("no task %q", slug)
	}
	if t.Branch == "" {
		return "", fmt.Errorf("task %q has no branch to open a PR for", slug)
	}
	if title == "" {
		title = t.Title
	}
	if body == "" {
		body = "Automated by rambl."
	}
	base := r.defaultBase()
	if out, err := worker.PushBranch(r.repoPath, "origin", t.Branch); err != nil {
		return "", fmt.Errorf("git push failed: %v: %s", err, out)
	}
	cmd := exec.Command("gh", "pr", "create", "--base", base, "--head", t.Branch, "--title", title, "--body", body)
	cmd.Dir = r.repoPath
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("gh pr create failed (is gh installed and authenticated, and origin a GitHub remote?): %v: %s", err, string(out))
	}
	return strings.TrimSpace(string(out)), nil
}

// defaultBase resolves the PR base branch from origin's HEAD symbolic ref,
// falling back to "main".
func (r *Runner) defaultBase() string {
	cmd := exec.Command("git", "symbolic-ref", "refs/remotes/origin/HEAD")
	cmd.Dir = r.repoPath
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "main"
	}
	ref := strings.TrimSpace(string(out))
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		ref = strings.TrimSpace(ref[i+1:])
	}
	if ref == "" {
		return "main"
	}
	return ref
}

// apply records the outcome of a completed turn and, on completion, commits and
// retires the worker; on a block it leaves the worker alive for follow-up.
func (r *Runner) apply(projectID, slug string, w *worker.Worker, turn worker.Turn) {
	t, err := r.store.GetTask(projectID, slug)
	if err != nil || t == nil {
		return
	}
	t.SessionID = w.SessionID
	t.Result = strings.TrimSpace(turn.Reply)

	if turn.TimedOut {
		t.Status = store.Failed
		t.Question = ""
		_ = r.store.Update(t)
		r.retire(slug)
		return
	}

	switch status, question := classify(turn.Reply); status {
	case store.Done:
		if err := w.Commit(fmt.Sprintf("rambl(%s): %s", slug, t.Title)); err != nil {
			t.Status = store.Failed
			t.Result = "commit failed: " + err.Error()
		} else {
			t.Status = store.Done
		}
		_ = r.store.Update(t)
		r.retire(slug)
	default: // NeedsInput
		t.Status = store.NeedsInput
		t.Question = question
		_ = r.store.Update(t) // keep the worker alive for a follow-up answer
	}
}

// classify maps a worker's final message to an outcome via the marker protocol.
// The protocol requires the marker to be the final line of the worker's final
// message, so classification inspects ONLY the last non-empty line — a marker
// quoted earlier in the body (e.g. prose describing a test case) is ignored.
func classify(reply string) (store.Status, string) {
	var last string
	lines := strings.Split(reply, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if trimmed := strings.TrimSpace(lines[i]); trimmed != "" {
			last = trimmed
			break
		}
	}
	if strings.HasPrefix(last, "RAMBL_BLOCKED:") {
		q := strings.TrimSpace(strings.TrimPrefix(last, "RAMBL_BLOCKED:"))
		return store.NeedsInput, q
	}
	if last == "RAMBL_DONE" {
		return store.Done, ""
	}
	return store.NeedsInput, "(worker ended its turn without a DONE or BLOCKED marker)"
}

func (r *Runner) fail(projectID, slug, msg string) {
	if t, _ := r.store.GetTask(projectID, slug); t != nil {
		t.Status = store.Failed
		t.Result = msg
		_ = r.store.Update(t)
	}
	r.retire(slug)
}

func (r *Runner) retire(slug string) {
	r.mu.Lock()
	w := r.workers[slug]
	delete(r.workers, slug)
	r.mu.Unlock()
	if w != nil {
		_ = w.Close()
	}
}

// Shutdown closes all live workers (e.g. on environment exit).
func (r *Runner) Shutdown() {
	r.mu.Lock()
	ws := r.workers
	r.workers = map[string]*worker.Worker{}
	r.mu.Unlock()
	for _, w := range ws {
		_ = w.Close()
	}
}
