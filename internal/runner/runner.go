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
	"sort"
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

	maxResolveAttempts int // integration-gate resolve-session budget

	pollInterval time.Duration // dispatchAndWait store-poll cadence
	// runTask dispatches a task and blocks until it reaches a terminal status.
	// nil in production (falls back to dispatchAndWait); overridden in tests to
	// drive scheduling without spawning real Claude sessions.
	runTask func(projectID, slug string) (store.Status, error)
	// openFeaturePRFn opens the feature's PR once it is merged and green. nil in
	// production (falls back to OpenFeaturePR); overridden in tests to assert the
	// auto-PR step fires without a real push/gh call.
	openFeaturePRFn func(projectID, featureSlug, body string) (string, error)

	mu      sync.Mutex
	workers map[string]*worker.Worker     // keyed by task slug
	cancels map[string]context.CancelFunc // per-worker run-cancel, keyed by task slug
	// activeFeatures tracks feature slugs whose RunFeature loop is currently
	// driving them, so manual dispatch can refuse to collide with the engine's
	// own scheduling. Guarded by r.mu.
	activeFeatures map[string]bool
}

// New constructs a Runner. selfExe is this binary's path (for workers' Stop hook).
func New(st *store.Store, repoPath, base, selfExe, worktreeBase string) *Runner {
	if base == "" {
		base = "HEAD"
	}
	return &Runner{
		store: st, repoPath: repoPath, base: base, selfExe: selfExe,
		worktreeBase:       worktreeBase,
		turnTimeout:        15 * time.Minute,
		maxResolveAttempts: 2,
		pollInterval:       500 * time.Millisecond,
		workers:            map[string]*worker.Worker{},
		cancels:            map[string]context.CancelFunc{},
		activeFeatures:     map[string]bool{},
	}
}

// SetTurnTimeout overrides the per-turn worker timeout. Non-positive values
// are ignored, preserving the existing value.
func (r *Runner) SetTurnTimeout(d time.Duration) {
	if d > 0 {
		r.turnTimeout = d
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

// DispatchManual is the entrypoint for human/PM-initiated dispatch. It refuses
// to start a task that belongs to a feature whose RunFeature loop is currently
// driving it (which would collide with the engine's own scheduling); otherwise
// it behaves exactly like Dispatch.
func (r *Runner) DispatchManual(projectID, slug string) error {
	t, err := r.store.GetTask(projectID, slug)
	if err != nil {
		return err
	}
	if t == nil {
		return fmt.Errorf("no task %q", slug)
	}
	if t.FeatureID != "" {
		// Resolve the feature slug for t.FeatureID and check if its loop is active.
		feats, err := r.store.ListFeatures(projectID)
		if err != nil {
			return err
		}
		for _, f := range feats {
			if f.ID == t.FeatureID {
				r.mu.Lock()
				active := r.activeFeatures[f.Slug]
				r.mu.Unlock()
				if active {
					return fmt.Errorf("task %q is managed by the running feature %q; let the engine schedule it (poll feature_status) — or wait for the feature to finish/fail before dispatching it manually", slug, f.Slug)
				}
				break
			}
		}
	}
	return r.Dispatch(projectID, slug)
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
	// Make the run cancelable so Stop can terminate it mid-turn; register the
	// worker and its cancel func together (exactly one place either is set).
	runCtx, runCancel := context.WithCancel(context.Background())
	r.mu.Lock()
	r.workers[slug] = w
	r.cancels[slug] = runCancel
	r.mu.Unlock()

	turn, err := w.Run(runCtx)

	// If we were retired out from under us (by Stop or Delete) while blocked in
	// Run, the terminal status is already set — bow out without clobbering it.
	r.mu.Lock()
	_, live := r.workers[slug]
	r.mu.Unlock()
	if !live {
		return
	}

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

// MergeConflictError reports that a task branch could not be cleanly squash-merged
// into its feature branch. Detect with errors.As.
type MergeConflictError struct {
	Feature string
	Task    string
	Files   []string
}

func (e *MergeConflictError) Error() string {
	return fmt.Sprintf("merge of task %q into feature %q conflicted in: %s",
		e.Task, e.Feature, strings.Join(e.Files, ", "))
}

// MergeTaskIntoFeature squash-merges rambl/<taskSlug> into the feature branch as one
// commit "feat(<taskSlug>): <task title>", run inside the feature integration worktree.
// Returns *MergeConflictError on conflict.
func (r *Runner) MergeTaskIntoFeature(projectID, featureSlug, taskSlug string) error {
	f, err := r.store.GetFeature(projectID, featureSlug)
	if err != nil {
		return err
	}
	if f == nil {
		return fmt.Errorf("no feature %q", featureSlug)
	}
	t, err := r.store.GetTask(projectID, taskSlug)
	if err != nil {
		return err
	}
	if t == nil {
		return fmt.Errorf("no task %q", taskSlug)
	}

	featureWorktree := r.featureWorktree(projectID, featureSlug)
	message := fmt.Sprintf("feat(%s): %s", taskSlug, t.Title)
	conflict, err := worker.SquashMerge(featureWorktree, "rambl/"+taskSlug, message)
	if conflict {
		// SquashMerge already aborted and cleaned the worktree, so the unmerged
		// paths are no longer queryable; recover them from the error it named.
		return &MergeConflictError{Feature: featureSlug, Task: taskSlug, Files: conflictedFilesFromErr(err)}
	}
	return err
}

// conflictedFilesFromErr extracts the comma-separated paths SquashMerge names in
// its conflict error ("... conflicted in: a, b"). Returns nil if absent.
func conflictedFilesFromErr(err error) []string {
	if err == nil {
		return nil
	}
	const marker = "conflicted in: "
	i := strings.Index(err.Error(), marker)
	if i < 0 {
		return nil
	}
	var files []string
	for _, p := range strings.Split(err.Error()[i+len(marker):], ",") {
		if p = strings.TrimSpace(p); p != "" {
			files = append(files, p)
		}
	}
	return files
}

// TopoOrder returns tasks in a deterministic dependency order (Kahn's algorithm over
// Task.Deps that reference slugs WITHIN the given set; deps to slugs not in the set are
// ignored). Ties among ready nodes break by ascending slug. Errors on a dependency cycle.
func TopoOrder(tasks []*store.Task) ([]*store.Task, error) {
	bySlug := make(map[string]*store.Task, len(tasks))
	for _, t := range tasks {
		bySlug[t.Slug] = t
	}

	// indegree counts only deps that reference slugs within the set; dependents
	// maps an upstream slug to the downstream slugs that depend on it.
	indegree := make(map[string]int, len(tasks))
	dependents := make(map[string][]string, len(tasks))
	for _, t := range tasks {
		for _, dep := range t.Deps {
			if _, ok := bySlug[dep]; !ok {
				continue // dep outside the set is ignored
			}
			indegree[t.Slug]++
			dependents[dep] = append(dependents[dep], t.Slug)
		}
	}

	// Seed the ready set with every zero-indegree node, kept sorted by slug.
	var ready []string
	for _, t := range tasks {
		if indegree[t.Slug] == 0 {
			ready = append(ready, t.Slug)
		}
	}
	sort.Strings(ready)

	ordered := make([]*store.Task, 0, len(tasks))
	for len(ready) > 0 {
		slug := ready[0]
		ready = ready[1:]
		ordered = append(ordered, bySlug[slug])

		var freed []string
		for _, down := range dependents[slug] {
			indegree[down]--
			if indegree[down] == 0 {
				freed = append(freed, down)
			}
		}
		if len(freed) > 0 {
			ready = append(ready, freed...)
			sort.Strings(ready)
		}
	}

	if len(ordered) != len(tasks) {
		return nil, fmt.Errorf("dependency cycle among tasks")
	}
	return ordered, nil
}

// dispatchAndWait dispatches a task and blocks until it reaches a terminal status
// (Done, Failed, Blocked, or NeedsInput), polling the store every r.pollInterval.
func (r *Runner) dispatchAndWait(projectID, slug string) (store.Status, error) {
	if err := r.Dispatch(projectID, slug); err != nil {
		return store.Failed, err
	}
	for {
		t, err := r.store.GetTask(projectID, slug)
		if err != nil {
			return store.Failed, err
		}
		if t != nil {
			switch t.Status {
			case store.Done, store.Failed, store.Blocked, store.NeedsInput:
				return t.Status, nil
			}
		}
		time.Sleep(r.pollInterval)
	}
}

// execRunTask runs a task to a terminal status using r.runTask when set (tests),
// else the real dispatchAndWait path (production).
func (r *Runner) execRunTask(projectID, slug string) (store.Status, error) {
	if r.runTask != nil {
		return r.runTask(projectID, slug)
	}
	return r.dispatchAndWait(projectID, slug)
}

// DispatchableFeatureTasks returns the feature's tasks that are ready to dispatch now:
// status Todo, and every in-feature dependency (a dep whose slug names another task in
// the same feature) already present in `merged`. Deps that name tasks outside the feature
// are ignored. Result is ordered by slug.
func (r *Runner) DispatchableFeatureTasks(projectID, featureSlug string, merged map[string]bool) ([]*store.Task, error) {
	f, err := r.store.GetFeature(projectID, featureSlug)
	if err != nil {
		return nil, err
	}
	if f == nil {
		return nil, fmt.Errorf("no feature %q", featureSlug)
	}
	tasks, err := r.store.TasksByFeature(projectID, f.ID)
	if err != nil {
		return nil, err
	}
	inFeature := make(map[string]bool, len(tasks))
	for _, t := range tasks {
		inFeature[t.Slug] = true
	}
	var ready []*store.Task
	for _, t := range tasks {
		if t.Status != store.Todo {
			continue
		}
		ok := true
		for _, dep := range t.Deps {
			if inFeature[dep] && !merged[dep] {
				ok = false
				break
			}
		}
		if ok {
			ready = append(ready, t)
		}
	}
	sort.Slice(ready, func(i, j int) bool { return ready[i].Slug < ready[j].Slug })
	return ready, nil
}

// RunFeature drives a feature end-to-end. It starts the feature branch+worktree, then
// repeatedly (a) dispatches every not-yet-dispatched task whose in-feature deps are all
// merged, in parallel, and (b) squash-merges completed tasks into the feature branch in
// TopoOrder, running the integration gate after each merge. It blocks until all the
// feature's tasks are merged and the branch is green, returning nil; or returns an error
// on the first task that fails/blocks, the first merge conflict, or a gate escalation.
func (r *Runner) RunFeature(projectID, featureSlug string) error {
	if _, err := r.StartFeature(projectID, featureSlug); err != nil {
		return err
	}
	f, err := r.store.GetFeature(projectID, featureSlug)
	if err != nil {
		return err
	}
	if f == nil {
		return fmt.Errorf("no feature %q", featureSlug)
	}

	// Mark this feature's loop active so manual (human/PM) dispatch of its tasks
	// is refused while the engine is scheduling them; clear it on every return.
	r.mu.Lock()
	r.activeFeatures[featureSlug] = true
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.activeFeatures, featureSlug)
		r.mu.Unlock()
	}()

	tasks, err := r.store.TasksByFeature(projectID, f.ID)
	if err != nil {
		return err
	}
	if len(tasks) == 0 {
		return nil
	}
	ordered, err := TopoOrder(tasks)
	if err != nil {
		return err
	}

	// In-feature dependency set, to distinguish deps that gate scheduling from
	// deps referencing tasks outside this feature (which are ignored).
	inFeature := make(map[string]bool, len(ordered))
	for _, t := range ordered {
		inFeature[t.Slug] = true
	}
	depsReady := func(t *store.Task, merged map[string]bool) bool {
		for _, dep := range t.Deps {
			if inFeature[dep] && !merged[dep] {
				return false
			}
		}
		return true
	}

	merged := map[string]bool{}
	dispatched := map[string]bool{}
	mergeIdx := 0

	for mergeIdx < len(ordered) {
		// (a) Dispatch wave: every not-yet-dispatched task whose in-feature deps
		// are all merged, concurrently.
		type result struct {
			slug   string
			status store.Status
			err    error
		}
		var wave []*store.Task
		for _, t := range ordered {
			if !dispatched[t.Slug] && depsReady(t, merged) {
				// Resumability: a task already Done from a prior run (a timed-out
				// loop, or a human completing/merging by hand) must NOT be
				// re-dispatched — Dispatch only accepts todo/failed/blocked, so
				// dispatching a Done task would spuriously fail the whole feature.
				// Mark it dispatched (so we don't reconsider it) but leave it out
				// of the wave; phase (b) will squash-merge it in topo order, which
				// is a no-op for an already-merged branch.
				cur, err := r.store.GetTask(projectID, t.Slug)
				if err != nil {
					return err
				}
				if cur != nil && cur.Status == store.Done {
					dispatched[t.Slug] = true
					continue
				}
				wave = append(wave, t)
				dispatched[t.Slug] = true
			}
		}
		var results []result
		if len(wave) > 0 {
			var wg sync.WaitGroup
			var mu sync.Mutex
			for _, t := range wave {
				wg.Add(1)
				go func(slug string) {
					defer wg.Done()
					status, err := r.execRunTask(projectID, slug)
					mu.Lock()
					results = append(results, result{slug: slug, status: status, err: err})
					mu.Unlock()
				}(t.Slug)
			}
			wg.Wait()
			for _, res := range results {
				if res.status != store.Done || res.err != nil {
					return fmt.Errorf("feature %q task %q did not complete (status %s): %w",
						featureSlug, res.slug, res.status, res.err)
				}
			}
		}

		// (b) Merge as far as possible in topo order.
		mergedThisRound := false
		for mergeIdx < len(ordered) {
			slug := ordered[mergeIdx].Slug
			t, err := r.store.GetTask(projectID, slug)
			if err != nil {
				return err
			}
			if t == nil || t.Status != store.Done {
				break // not ready to merge yet — go dispatch the next wave
			}
			if err := r.MergeTaskIntoFeature(projectID, featureSlug, slug); err != nil {
				return err
			}
			if err := r.IntegrateFeature(projectID, featureSlug); err != nil {
				return err
			}
			merged[slug] = true
			mergeIdx++
			mergedThisRound = true
		}

		// (c) Safety: a full iteration with no progress and not all merged is a stall.
		if len(wave) == 0 && !mergedThisRound && mergeIdx < len(ordered) {
			return fmt.Errorf("feature %q stalled: no dispatchable or mergeable tasks", featureSlug)
		}
	}

	// All tasks merged: run a final integration gate so we never open a PR on a
	// red branch, then auto-open the feature PR.
	if err := r.IntegrateFeature(projectID, featureSlug); err != nil {
		return err
	}
	if _, err := r.execOpenFeaturePR(projectID, featureSlug, ""); err != nil {
		return fmt.Errorf("feature %q merged and green but opening PR failed: %w", featureSlug, err)
	}
	return nil
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

// Stop terminates a live worker mid-run, marks its task failed (stopped by the
// PM) leaving the branch intact for re-dispatch, and retires the worker.
func (r *Runner) Stop(projectID, slug string) error {
	r.mu.Lock()
	w := r.workers[slug]
	if w == nil {
		r.mu.Unlock()
		return fmt.Errorf("task %q has no live worker to stop", slug)
	}
	delete(r.workers, slug)
	cancel := r.cancels[slug]
	delete(r.cancels, slug)
	r.mu.Unlock()

	if t, _ := r.store.GetTask(projectID, slug); t != nil {
		t.Status = store.Failed
		t.Result = "⏹ stopped by PM before completion"
		t.Question = ""
		_ = r.store.Update(t)
	}
	if cancel != nil {
		cancel() // unblock start's w.Run; it will see the worker gone and return
	}
	return w.Close()
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

// IntegrationEscalation reports that the integration gate could not make the
// feature branch build and test green within its attempt budget. Detect with errors.As.
type IntegrationEscalation struct {
	Feature  string
	Attempts int
	Output   string // last build/test output
}

func (e *IntegrationEscalation) Error() string {
	return fmt.Sprintf("integration gate could not make feature %q green after %d attempt(s)", e.Feature, e.Attempts)
}

// detectVerifyCommand returns the auto-detected build/test command for dir. It
// recognises a Go module (go.mod) and returns (cmd, true); otherwise ("", false).
func detectVerifyCommand(dir string) (string, bool) {
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
		return "go build ./... && go test ./...", true
	}
	return "", false
}

// runVerify runs command in dir and returns its combined output (truncated to
// 30000 bytes like Verify) and ok = (the command exited zero).
func runVerify(dir, command string) (output string, ok bool) {
	cmd := exec.Command("sh", "-c", command)
	cmd.Dir = dir
	cmd.Env = os.Environ()
	out, runErr := cmd.CombinedOutput()
	output = string(out)
	if len(output) > 30000 {
		origLen := len(output)
		output = output[:30000] + fmt.Sprintf("\n... [output truncated at 30000 of %d bytes]", origLen)
	}
	return output, runErr == nil
}

// IntegrateFeature ensures the feature's integration worktree builds and tests green.
// It runs the detected build/test command in the feature worktree; if it fails, it runs
// up to r.maxResolveAttempts resolve-only autonomous sessions (each instructed to fix
// ONLY build/test breakage — no new features, no behavior changes), committing each
// session's changes to the feature branch and re-running the command. Returns nil once
// green; *IntegrationEscalation when the attempt budget is exhausted; a plain error on
// infrastructure failure (e.g. missing worktree). Sets Feature.Status = FeatureIntegrating
// while running and restores it to FeatureRunning on success; leaves status unchanged on
// escalation (the caller decides how to surface it).
func (r *Runner) IntegrateFeature(projectID, featureSlug string) error {
	f, err := r.store.GetFeature(projectID, featureSlug)
	if err != nil {
		return err
	}
	if f == nil {
		return fmt.Errorf("no feature %q", featureSlug)
	}
	wt := r.featureWorktree(projectID, featureSlug)
	if _, err := os.Stat(wt); err != nil {
		return fmt.Errorf("no integration worktree for feature %q (start it first)", featureSlug)
	}

	f.Status = store.FeatureIntegrating
	_ = r.store.UpdateFeature(f)

	markRunning := func() error {
		f.Status = store.FeatureRunning
		_ = r.store.UpdateFeature(f)
		return nil
	}

	cmd, ok := detectVerifyCommand(wt)
	if !ok {
		// Nothing to verify — vacuously green.
		return markRunning()
	}

	output, green := runVerify(wt, cmd)
	if green {
		return markRunning()
	}

	for attempt := 1; attempt <= r.maxResolveAttempts; attempt++ {
		resolvePrompt := fmt.Sprintf(`The feature integration branch currently FAILS the command:

    %s

Here is the failing build/test output:

%s

Fix ONLY what is necessary to make `+"`%s`"+` pass. Do NOT add features, change public
behavior, or touch unrelated files. Keep the change minimal and focused on the breakage.`,
			cmd, output, cmd)

		spec := worker.Spec{
			ID:           "feat-" + featureSlug + "-integrate",
			Prompt:       resolvePrompt,
			RepoPath:     r.repoPath,
			Worktree:     wt,
			Branch:       featureBranch(featureSlug),
			Reopen:       true,
			SystemPrompt: WorkerSystemPrompt,
		}
		w := worker.New(spec)
		w.TurnTimeout = r.turnTimeout
		if err := w.Start(context.Background(), r.selfExe); err != nil {
			// A failed session is an unproductive attempt, not a hard error.
			output += fmt.Sprintf("\n\n[resolve attempt %d: session start failed: %v]", attempt, err)
			continue
		}
		_, _ = w.Run(context.Background())
		_ = w.Commit(fmt.Sprintf("fix(%s): integration gate resolve", featureSlug))
		_ = w.Close()

		output, green = runVerify(wt, cmd)
		if green {
			return markRunning()
		}
	}

	return &IntegrationEscalation{Feature: featureSlug, Attempts: r.maxResolveAttempts, Output: output}
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

// OpenFeaturePR pushes the feature's branch to origin and opens a GitHub PR
// (feature branch → default base) via gh, titled "feat(<slug>): <feature title>".
// On success it sets Feature.Status = FeatureDone, persists, and returns gh's output
// (the PR URL). Mirrors OpenPR's push+gh behavior exactly.
func (r *Runner) OpenFeaturePR(projectID, featureSlug, body string) (string, error) {
	f, err := r.store.GetFeature(projectID, featureSlug)
	if err != nil {
		return "", err
	}
	if f == nil {
		return "", fmt.Errorf("no feature %q", featureSlug)
	}
	if f.Branch == "" {
		return "", fmt.Errorf("feature %q has no branch yet (start it first)", featureSlug)
	}
	title := fmt.Sprintf("feat(%s): %s", featureSlug, f.Title)
	if body == "" {
		body = "Automated by rambl."
	}
	base := r.defaultBase()
	if out, err := worker.PushBranch(r.repoPath, "origin", f.Branch); err != nil {
		return "", fmt.Errorf("git push failed: %v: %s", err, out)
	}
	cmd := exec.Command("gh", "pr", "create", "--base", base, "--head", f.Branch, "--title", title, "--body", body)
	cmd.Dir = r.repoPath
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("gh pr create failed (is gh installed and authenticated, and origin a GitHub remote?): %v: %s", err, string(out))
	}
	f.Status = store.FeatureDone
	_ = r.store.UpdateFeature(f)
	return strings.TrimSpace(string(out)), nil
}

// execOpenFeaturePR opens the feature PR using r.openFeaturePRFn when set (tests),
// else the real OpenFeaturePR path (production).
func (r *Runner) execOpenFeaturePR(projectID, featureSlug, body string) (string, error) {
	if r.openFeaturePRFn != nil {
		return r.openFeaturePRFn(projectID, featureSlug, body)
	}
	return r.OpenFeaturePR(projectID, featureSlug, body)
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
	delete(r.cancels, slug)
	r.mu.Unlock()
	if w != nil {
		_ = w.Close()
	}
}

// Shutdown closes all live workers (e.g. on environment exit).
func (r *Runner) Shutdown() {
	r.mu.Lock()
	ws := r.workers
	cancels := r.cancels
	r.workers = map[string]*worker.Worker{}
	r.cancels = map[string]context.CancelFunc{}
	r.mu.Unlock()
	for _, cancel := range cancels {
		if cancel != nil {
			cancel()
		}
	}
	for _, w := range ws {
		_ = w.Close()
	}
}
