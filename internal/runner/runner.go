// Package runner manages live worker sessions on behalf of the PM. It
// dispatches workers (worktree-isolated, autonomous), keeps blocked ones alive
// so the PM can answer them, classifies each turn's outcome via the DONE/BLOCKED
// protocol, and writes status back to the store. The MCP tool layer calls into
// this; it does not spawn workers itself.
package runner

import (
	"context"
	"fmt"
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

Do not emit either marker until you are actually done or actually blocked. Emit only one.`

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
	spec := worker.Spec{
		ID: slug, Prompt: prompt, RepoPath: r.repoPath, Base: r.base,
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
func classify(reply string) (store.Status, string) {
	if i := strings.LastIndex(reply, "RAMBL_BLOCKED:"); i >= 0 {
		q := strings.TrimSpace(reply[i+len("RAMBL_BLOCKED:"):])
		if nl := strings.IndexByte(q, '\n'); nl >= 0 {
			q = strings.TrimSpace(q[:nl])
		}
		return store.NeedsInput, q
	}
	if strings.Contains(reply, "RAMBL_DONE") {
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
