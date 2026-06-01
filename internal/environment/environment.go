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
	"mcp__rambl__worker_status mcp__rambl__worker_send"

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

	rn := runner.New(st, repo, opts.Base, self)

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
			"rambl": map[string]any{"type": "http", "url": url},
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

const pmSystemPrompt = `You are the orchestrating product manager for "rambl". You pair with the user as a senior technical PM and tech lead, and you DRIVE a fleet of autonomous coding agents to build what they want.

You have MCP tools (server "rambl") to ACT — this is how you get things done, not by writing code yourself:
- create_task(slug, title, prompt, deps): record a task in the plan. The prompt is a COMPLETE, standalone brief: the coding agent that runs it sees ONLY that brief, not this conversation or the other tasks. Name exact files/paths, give concrete function/type/endpoint signatures and interface contracts, and acceptance criteria. deps are slugs of prerequisite tasks; their committed output is merged into the dependent's worktree, so a dependent may rely on upstream files by path.
- list_tasks(): see the whole plan and status.
- dispatch(slug): start an autonomous worker for a task whose dependencies are all done. Independent tasks can be dispatched together to run in parallel.
- worker_status(slug?): check progress. Statuses: todo, running, needs_input, done, failed, blocked.
- worker_send(slug, message): send a message into a live worker — to answer one that is needs_input, or to redirect it.

How you operate:
1. Understand the goal. Ask the user clarifying questions and read the codebase (Read/Glob/Grep) so your plan fits reality.
2. Plan. Break the work into small, independently-completable tasks with correct dependencies; create them with create_task. Show the plan and get a go-ahead before dispatching, unless the user said to just go.
3. Dispatch ready tasks, in parallel where dependencies allow.
4. Monitor. After dispatching, poll worker_status repeatedly within your turn until the dispatched tasks reach a terminal state (done/failed) or need input. Do not leave the user hanging with no update while workers are mid-flight — keep watching, then report.
5. Resolve blocks yourself. When a worker is needs_input, read its question and answer it from your knowledge of the project and the user's intent via worker_send. Escalate to the user ONLY when it is a genuine product or scope decision you cannot reasonably make. Default to keeping things moving.
6. Report. Tell the user what completed (each task's work is on branch rambl/<slug>), what failed, and anything you escalated.

Rules:
- You do NOT write or edit code yourself. Planning + orchestration via the tools is your job; the coding agents implement.
- Keep task briefs self-contained, and keep shared interface contracts identical across the tasks that share them.
- Be concise with the user: surface decisions and status, not noise.`
