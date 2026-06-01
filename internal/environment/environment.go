// Package environment is the persistent "rambl" environment: it boots the
// in-process MCP server (HTTP) backed by the store + runner, then launches an
// interactive Claude Code PM session wired to that server. You converse with
// the PM; it plans, dispatches, monitors, and resolves workers via the MCP
// tools. The environment owns the server and workers and shuts them down on exit.
package environment

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"rambl/internal/mcpserver"
	"rambl/internal/runner"
	"rambl/internal/session"
	"rambl/internal/store"
)

// Options configure an environment.
type Options struct {
	RepoPath string
	DBPath   string
	Base     string
	Model    string // optional --model override
}

// allowedTools is what the PM may use without a permission prompt: read-only
// inspection of the repo plus the orchestration MCP tools. It deliberately does
// NOT include Write/Edit/Bash — the PM plans and orchestrates; the workers code.
const allowedTools = "Read Glob Grep LS " +
	"mcp__rambl__create_task mcp__rambl__list_tasks mcp__rambl__dispatch " +
	"mcp__rambl__worker_status mcp__rambl__worker_send mcp__rambl__delete_task " +
	"mcp__rambl__read_diff mcp__rambl__verify_task mcp__rambl__revise_task"

type setup struct {
	store     *store.Store
	runner    *runner.Runner
	projectID string
	claude    string
	args      []string
	repo      string
	cleanup   func()
}

func prepare(opts Options) (*setup, error) {
	repo, err := filepath.Abs(opts.RepoPath)
	if err != nil {
		return nil, err
	}
	if opts.DBPath == "" {
		home, _ := os.UserHomeDir()
		opts.DBPath = filepath.Join(home, ".rambl", "state.db")
	}
	if err := os.MkdirAll(filepath.Dir(opts.DBPath), 0o755); err != nil {
		return nil, err
	}
	worktreeBase := filepath.Join(filepath.Dir(opts.DBPath), "worktrees")
	st, err := store.Open(opts.DBPath)
	if err != nil {
		return nil, err
	}
	projectID, err := st.EnsureProject(repo, filepath.Base(repo))
	if err != nil {
		return nil, err
	}
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	claude, err := session.ResolveClaude()
	if err != nil {
		return nil, err
	}

	rn := runner.New(st, repo, opts.Base, self, worktreeBase)

	port, err := freePort()
	if err != nil {
		return nil, err
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	srv := mcpserver.New(st, rn, projectID)
	go func() { _ = srv.Serve(addr) }()
	if err := waitListening(addr, 5*time.Second); err != nil {
		return nil, err
	}

	cfgPath, err := writeMCPConfig(fmt.Sprintf("http://%s/mcp", addr))
	if err != nil {
		return nil, err
	}

	args := []string{
		"--mcp-config", cfgPath,
		"--append-system-prompt", systemPrompt(repo),
		"--allowedTools", allowedTools,
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}

	cleanup := func() {
		rn.Shutdown()
		_ = st.Close()
		_ = os.Remove(cfgPath)
	}
	return &setup{store: st, runner: rn, projectID: projectID, claude: claude, args: args, repo: repo, cleanup: cleanup}, nil
}

// Run launches the interactive PM session (native TUI) and blocks until exit.
func Run(opts Options) error {
	s, err := prepare(opts)
	if err != nil {
		return err
	}
	defer s.cleanup()

	cmd := exec.Command(s.claude, s.args...)
	cmd.Dir = s.repo
	cmd.Env = session.Env()
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// RunOnce drives a single PM turn from a brief (no human interaction), then
// polls the store until tasks settle. Used to verify the MCP-driven loop
// end-to-end without an interactive session. Returns the final task states.
func RunOnce(opts Options, brief string, timeout time.Duration) ([]*store.Task, error) {
	s, err := prepare(opts)
	if err != nil {
		return nil, err
	}
	defer s.cleanup()

	sess, err := session.Start(session.Config{
		ClaudePath: s.claude,
		Dir:        s.repo,
		ExtraArgs:  s.args,
	})
	if err != nil {
		return nil, err
	}
	defer sess.Close()

	msg := brief + "\n\nThis is a non-interactive run: do not ask me questions. " +
		"Create the task(s), dispatch, and keep polling worker_status until everything " +
		"is done, failed, or genuinely needs a product decision. Then report."
	if err := sess.Send(msg); err != nil {
		return nil, err
	}

	deadline := time.Now().Add(timeout)
	for {
		tasks, err := s.store.ListTasks(s.projectID)
		if err != nil {
			return nil, err
		}
		if len(tasks) > 0 && settled(tasks) {
			return tasks, nil
		}
		if time.Now().After(deadline) {
			return tasks, fmt.Errorf("timed out before tasks settled")
		}
		time.Sleep(2 * time.Second)
	}
}

func settled(tasks []*store.Task) bool {
	for _, t := range tasks {
		if t.Status == store.Todo || t.Status == store.Running {
			return false
		}
	}
	return true
}

func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

func waitListening(addr string, d time.Duration) error {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("MCP server did not come up on %s", addr)
}

func writeMCPConfig(url string) (string, error) {
	cfg := map[string]any{
		"mcpServers": map[string]any{
			// timeout (ms) is Claude Code's hard per-tool-call wall-clock cap for
			// this server; must stay above the worker_status long-poll wait cap (90s).
			"rambl": map[string]any{"type": "http", "url": url, "timeout": 120000},
		},
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp("", "rambl-mcp-*.json")
	if err != nil {
		return "", err
	}
	defer f.Close()
	_, err = f.Write(data)
	return f.Name(), err
}

// systemPrompt builds the PM-as-driver prompt, appending an optional per-project
// .rambl/pm.md so the user can tailor the PM's behavior.
func systemPrompt(repo string) string {
	p := pmSystemPrompt
	if extra, err := os.ReadFile(filepath.Join(repo, ".rambl", "pm.md")); err == nil && len(extra) > 0 {
		p += "\n\n## Project-specific guidance (from .rambl/pm.md)\n\n" + string(extra)
	}
	return p
}

const pmSystemPrompt = `You are the orchestrating product manager for "rambl". You pair with the user as a senior technical PM and tech lead, and you drive a fleet of autonomous coding agents to build what they want.

Your MCP tools (server "rambl") are how you act — you plan and orchestrate; the coding agents write the code:
- create_task(slug, title, prompt, deps): record a task. The prompt is a COMPLETE, standalone brief — the agent that runs it sees ONLY that brief, never this conversation or other tasks. Name exact files/paths, give concrete function/type/endpoint signatures and interface contracts, and acceptance criteria. deps are slugs of prerequisite tasks; each dependency's committed output is merged into this task's worktree before it runs, so a dependent may rely on upstream files by path (but not on the upstream brief). Briefs cannot be edited after creation — get them right, or supersede with a new task.
- list_tasks(): the whole plan and status.
- dispatch(slug): start an autonomous worker. Requires status todo/failed/blocked and all deps done. Re-dispatching a failed or blocked task retries it from a fresh worktree. Independent ready tasks can be dispatched together to run in parallel.
- worker_status(slug?, wait_seconds?): inspect status. Statuses: todo, running, needs_input, done, failed, blocked. With wait_seconds (up to 90) the call blocks server-side and returns the moment a worker finishes or needs input (or when the time elapses) — use it to wait efficiently instead of calling repeatedly in a tight loop. Omit wait_seconds for an instant snapshot.
- worker_send(slug, message): send into a live worker — to answer a needs_input question or redirect it. Only works while the worker is alive (running or needs_input).
- delete_task(slug): permanently delete a task and reclaim its worktree and branch. Use to prune stale, duplicate, or superseded tasks (and to tidy a task once its work is merged). Refuses a running task.
- read_diff(slug): show the diff (stat plus patch) of the task's rambl/SLUG branch, so you can review what the worker actually changed before validating or shipping it.
- verify_task(slug, command?): run a build/test command inside the task's worktree and get its PASS/FAIL output. Pass an explicit command (e.g. 'go build ./... && go test ./...'); if omitted, a Go project is auto-detected.
- revise_task(slug, message): hand a finished task's branch back to a worker with feedback so it iterates on its prior output (reuses the live session if present, else reopens the branch). Use after read_diff/verify_task surface issues, then poll worker_status.

How you operate:
1. Understand. Ask clarifying questions and read the codebase (Read/Glob/Grep) so the plan fits reality.
2. Plan. Break the work into small, independently-completable tasks with correct deps. Show the plan and get a go-ahead before dispatching, unless the user said to just go.
3. Dispatch ready tasks, in parallel where deps allow.
4. Monitor. To wait on running workers, call worker_status with wait_seconds rather than spinning in a tight poll loop — there is no client-side sleep and tight polling just burns the turn. A blocking call holds your attention, so use a moderate wait, then report state and re-check. Workers keep progressing in the background between your turns.
5. Resolve outcomes.
   - needs_input: read the question and answer it via worker_send from your knowledge of the project and the user's intent. Escalate to the user only for a genuine product or scope call you cannot reasonably make.
   - failed: read the result, then either fix the brief's assumptions and re-dispatch to retry, or escalate.
   - blocked: a dependency failed or could not be integrated. Resolve the upstream task first, then re-dispatch this one.
   Default to keeping things moving.
   - Keep the plan clean: proactively prune duplicate, superseded, or abandoned tasks with delete_task so the task list reflects only work that still matters.
6. Report. Tell the user what is done (each task's work is on branch rambl/SLUG, where SLUG is the task slug), what failed or is blocked, and anything you escalated.

Rules:
- You do NOT write or edit code yourself. Planning and orchestration are your job.
- Every brief must stand alone, and shared interface contracts must be IDENTICAL across the tasks that share them — a mismatch will not surface until integration.
- Be concise with the user: surface decisions and status, not noise.`
