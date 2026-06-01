package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"rambl/internal/runner"
	"rambl/internal/store"
)

// Exercises the PM tool surface over the real streamable-HTTP transport (the
// same path Claude uses). Dispatch is not tested here (it spawns a claude
// worker); this covers schema + create/list against the store.
func TestMCPToolsOverHTTP(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	proj, err := st.EnsureProject("/repo/calc", "calc")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	rn := runner.New(st, "/repo/calc", "HEAD", "/usr/bin/true", "")
	srv := New(st, rn, proj)

	httpSrv := server.NewTestStreamableHTTPServer(srv.mcp)
	defer httpSrv.Close()

	ctx := context.Background()
	cli, err := mcpclient.NewStreamableHttpClient(httpSrv.URL)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	if err := cli.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := cli.Initialize(ctx, mcp.InitializeRequest{
		Params: mcp.InitializeParams{
			ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
			ClientInfo:      mcp.Implementation{Name: "test", Version: "1.0.0"},
		},
	}); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	// Tools advertised.
	tools, err := cli.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	want := map[string]bool{"create_task": false, "list_tasks": false, "dispatch": false, "worker_status": false, "worker_send": false}
	for _, tool := range tools.Tools {
		if _, ok := want[tool.Name]; ok {
			want[tool.Name] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("tool %q not advertised", name)
		}
	}

	// create_task core, then cli depending on core.
	mustCall(t, cli, ctx, "create_task", map[string]any{
		"slug": "core", "title": "Core", "prompt": "build core",
	})
	mustCall(t, cli, ctx, "create_task", map[string]any{
		"slug": "cli", "title": "CLI", "prompt": "build cli", "deps": []string{"core"},
	})

	// list_tasks reflects them.
	res := mustCall(t, cli, ctx, "list_tasks", nil)
	text := textOf(t, res)
	var views []struct {
		Slug   string   `json:"slug"`
		Status string   `json:"status"`
		Deps   []string `json:"deps"`
	}
	if err := json.Unmarshal([]byte(text), &views); err != nil {
		t.Fatalf("list_tasks json: %v\n%s", err, text)
	}
	if len(views) != 2 {
		t.Fatalf("want 2 tasks, got %d: %s", len(views), text)
	}
	byslug := map[string]string{}
	var cliDeps []string
	for _, v := range views {
		byslug[v.Slug] = v.Status
		if v.Slug == "cli" {
			cliDeps = v.Deps
		}
	}
	if byslug["core"] != "todo" || byslug["cli"] != "todo" {
		t.Fatalf("unexpected statuses: %s", text)
	}
	if len(cliDeps) != 1 || cliDeps[0] != "core" {
		t.Fatalf("cli deps = %v, want [core]", cliDeps)
	}
}

// TestWorkerStatusWait covers the bounded long-poll: wait_seconds=0 is an
// instant read, a settled (done) scope returns promptly, and an only-running
// scope blocks to roughly the cap.
func TestWorkerStatusWait(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	proj, err := st.EnsureProject("/repo/calc", "calc")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	done, err := st.AddTask(proj, "done-task", "Done", "x", nil)
	if err != nil {
		t.Fatalf("add done-task: %v", err)
	}
	done.Status = store.Done
	if err := st.Update(done); err != nil {
		t.Fatalf("update done-task: %v", err)
	}
	run, err := st.AddTask(proj, "run-task", "Run", "x", nil)
	if err != nil {
		t.Fatalf("add run-task: %v", err)
	}
	run.Status = store.Running
	if err := st.Update(run); err != nil {
		t.Fatalf("update run-task: %v", err)
	}

	ctx := context.Background()

	// wait_seconds=0 → instant read regardless of scope.
	start := time.Now()
	waitForStatusIf(ctx, st, proj, "run-task", 0)
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("wait_seconds=0 took %v, want instant", d)
	}

	// Settled (done) scope returns well under the cap even with a wait set.
	start = time.Now()
	waitForStatus(ctx, st, proj, "done-task", 2)
	if d := time.Since(start); d > 1500*time.Millisecond {
		t.Fatalf("settled scope took %v, want early return", d)
	}

	// Only-running scope blocks to roughly the cap.
	start = time.Now()
	waitForStatus(ctx, st, proj, "run-task", 2)
	if d := time.Since(start); d < 1500*time.Millisecond {
		t.Fatalf("running scope returned in %v, want ~cap", d)
	}
}

// waitForStatusIf mirrors the handler's gate: only block when wait > 0.
func waitForStatusIf(ctx context.Context, st *store.Store, projectID, slug string, wait int) {
	if wait > 0 {
		waitForStatus(ctx, st, projectID, slug, wait)
	}
}

// TestPMReviewToolsOverHTTP exercises the PM review/lifecycle tools (delete_task,
// read_diff, verify_task, revise_task) over the real HTTP transport, backed by a
// Runner wired to a genuine temp git repo. It only touches validation/error paths
// and the no-worker delete happy path — none of these spawn a claude worker.
func TestPMReviewToolsOverHTTP(t *testing.T) {
	// Real temp git repo with identity + one commit, as the runner's repoPath.
	repo := t.TempDir()
	runGit(t, repo, "init")
	runGit(t, repo, "config", "user.email", "rambl@test")
	runGit(t, repo, "config", "user.name", "rambl")
	if err := writeFile(t, filepath.Join(repo, "README.md"), "init\n"); err != nil {
		t.Fatalf("write README: %v", err)
	}
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-m", "init")

	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	proj, err := st.EnsureProject(repo, "test")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	worktreeBase := t.TempDir()
	// selfExe "" — no worker is ever spawned by these tools/paths.
	rn := runner.New(st, repo, "HEAD", "", worktreeBase)
	srv := New(st, rn, proj)

	httpSrv := server.NewTestStreamableHTTPServer(srv.mcp)
	defer httpSrv.Close()

	ctx := context.Background()
	cli, err := mcpclient.NewStreamableHttpClient(httpSrv.URL)
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	if err := cli.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := cli.Initialize(ctx, mcp.InitializeRequest{
		Params: mcp.InitializeParams{
			ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION,
			ClientInfo:      mcp.Implementation{Name: "test", Version: "1.0.0"},
		},
	}); err != nil {
		t.Fatalf("initialize: %v", err)
	}

	// delete_task on an unknown slug -> error result.
	if res := errCall(t, cli, ctx, "delete_task", map[string]any{"slug": "ghost"}); !strings.Contains(textOf(t, res), "no task") {
		t.Fatalf("delete_task ghost: want 'no task' error, got %q", textOf(t, res))
	}

	// delete_task happy path: a Todo task with no branch deletes cleanly and
	// disappears from list_tasks.
	mustCall(t, cli, ctx, "create_task", map[string]any{
		"slug": "doomed", "title": "Doomed", "prompt": "build doomed",
	})
	mustCall(t, cli, ctx, "delete_task", map[string]any{"slug": "doomed"})
	res := mustCall(t, cli, ctx, "list_tasks", nil)
	if strings.Contains(textOf(t, res), "doomed") {
		t.Fatalf("delete_task: 'doomed' still present in list_tasks: %s", textOf(t, res))
	}

	// read_diff on a task with no branch -> error result.
	mustCall(t, cli, ctx, "create_task", map[string]any{
		"slug": "nobranch", "title": "No branch", "prompt": "x",
	})
	if res := errCall(t, cli, ctx, "read_diff", map[string]any{"slug": "nobranch"}); !strings.Contains(textOf(t, res), "no branch") {
		t.Fatalf("read_diff nobranch: want 'no branch' error, got %q", textOf(t, res))
	}

	// verify_task on a task with no worktree -> error result.
	mustCall(t, cli, ctx, "create_task", map[string]any{
		"slug": "noworktree", "title": "No worktree", "prompt": "x",
	})
	if res := errCall(t, cli, ctx, "verify_task", map[string]any{"slug": "noworktree", "command": "true"}); !strings.Contains(textOf(t, res), "no worktree") {
		t.Fatalf("verify_task noworktree: want 'no worktree' error, got %q", textOf(t, res))
	}

	// revise_task on an unknown slug -> error result.
	if res := errCall(t, cli, ctx, "revise_task", map[string]any{"slug": "ghost", "message": "fix it"}); !strings.Contains(textOf(t, res), "no task") {
		t.Fatalf("revise_task ghost: want 'no task' error, got %q", textOf(t, res))
	}
}

func mustCall(t *testing.T, cli *mcpclient.Client, ctx context.Context, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	req := mcp.CallToolRequest{}
	req.Params.Name = name
	if args != nil {
		req.Params.Arguments = args
	}
	res, err := cli.CallTool(ctx, req)
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	if res.IsError {
		t.Fatalf("call %s returned error: %s", name, textOf(t, res))
	}
	return res
}

func textOf(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if len(res.Content) == 0 {
		t.Fatal("empty tool result")
	}
	tc, ok := mcp.AsTextContent(res.Content[0])
	if !ok {
		t.Fatalf("first content is not text: %T", res.Content[0])
	}
	return tc.Text
}

// errCall calls a tool and asserts the result is an error result (IsError),
// returning it so the caller can inspect the message.
func errCall(t *testing.T, cli *mcpclient.Client, ctx context.Context, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	req := mcp.CallToolRequest{}
	req.Params.Name = name
	if args != nil {
		req.Params.Arguments = args
	}
	res, err := cli.CallTool(ctx, req)
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	if !res.IsError {
		t.Fatalf("call %s: expected error result, got %s", name, textOf(t, res))
	}
	return res
}

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

func writeFile(t *testing.T, path, content string) error {
	t.Helper()
	return os.WriteFile(path, []byte(content), 0o644)
}
