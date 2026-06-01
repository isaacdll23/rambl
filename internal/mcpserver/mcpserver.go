// Package mcpserver exposes the PM's tool surface over MCP (HTTP transport).
// These are the verbs the planning agent uses to *act*: build the task graph,
// dispatch workers, watch them, and answer the ones that get blocked. All state
// lives in the store; workers are managed by the runner.
package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"rambl/internal/runner"
	"rambl/internal/store"
)

// maxWaitSeconds caps how long worker_status will block server-side.
const maxWaitSeconds = 90

// Server wraps the MCP server for one project.
type Server struct {
	mcp *server.MCPServer
}

// New builds the tool server for a single project.
func New(st *store.Store, rn *runner.Runner, projectID string) *Server {
	s := server.NewMCPServer("rambl", "0.1.0")

	s.AddTool(mcp.NewTool("create_task",
		mcp.WithDescription("Create a task in the plan. Each task is later executed by a separate autonomous agent in an isolated worktree, so the prompt must be a complete, standalone brief. deps are slugs of tasks that must finish first (their committed output is merged into this task's worktree)."),
		mcp.WithString("slug", mcp.Required(), mcp.Description("unique kebab-case id, e.g. core-lib")),
		mcp.WithString("title", mcp.Required(), mcp.Description("short human title")),
		mcp.WithString("prompt", mcp.Required(), mcp.Description("the complete self-contained brief for the coding agent")),
		mcp.WithArray("deps", mcp.Description("slugs of prerequisite tasks"), mcp.Items(map[string]any{"type": "string"})),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		slug, err := req.RequireString("slug")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		title, _ := req.RequireString("title")
		prompt, err := req.RequireString("prompt")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		deps := req.GetStringSlice("deps", nil)
		if _, err := st.AddTask(projectID, slug, title, prompt, deps); err != nil {
			return mcp.NewToolResultErrorf("create_task: %v", err), nil
		}
		return mcp.NewToolResultText(fmt.Sprintf("created task %q (deps %v)", slug, deps)), nil
	})

	s.AddTool(mcp.NewTool("list_tasks",
		mcp.WithDescription("List all tasks in the plan with their status, dependencies, blocking question (if any), and latest result."),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return tasksJSON(st, projectID, "")
	})

	s.AddTool(mcp.NewTool("dispatch",
		mcp.WithDescription("Start an autonomous worker for a task. The task must be todo/failed/blocked and all its dependencies must be done. Returns immediately; poll worker_status for progress."),
		mcp.WithString("slug", mcp.Required(), mcp.Description("task slug to dispatch")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		slug, err := req.RequireString("slug")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if err := rn.Dispatch(projectID, slug); err != nil {
			return mcp.NewToolResultErrorf("dispatch: %v", err), nil
		}
		return mcp.NewToolResultText(fmt.Sprintf("dispatched %q; poll worker_status", slug)), nil
	})

	s.AddTool(mcp.NewTool("worker_status",
		mcp.WithDescription("Check worker/task status. With a slug, returns that task; without, returns all. A task in 'needs_input' has a 'question' you should answer (from your own knowledge if possible) via worker_send, or escalate to the human. With wait_seconds > 0, blocks server-side and returns early as soon as a watched task finishes or needs input."),
		mcp.WithString("slug", mcp.Description("optional task slug; omit for all")),
		mcp.WithNumber("wait_seconds", mcp.Description("optional: block up to this many seconds (max 90), returning as soon as a watched task finishes or needs input; omit/0 for an instant snapshot")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		slug := req.GetString("slug", "")
		wait := req.GetInt("wait_seconds", 0)
		if wait > 0 {
			waitForStatus(ctx, st, projectID, slug, wait)
		}
		return tasksJSON(st, projectID, slug)
	})

	s.AddTool(mcp.NewTool("worker_send",
		mcp.WithDescription("Send a message into a live worker session — typically to answer a worker that is in 'needs_input', or to give it additional direction. The worker continues in the same session."),
		mcp.WithString("slug", mcp.Required(), mcp.Description("task slug of the live worker")),
		mcp.WithString("message", mcp.Required(), mcp.Description("the answer or instruction to send")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		slug, err := req.RequireString("slug")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		message, err := req.RequireString("message")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if err := rn.Send(projectID, slug, message); err != nil {
			return mcp.NewToolResultErrorf("worker_send: %v", err), nil
		}
		return mcp.NewToolResultText(fmt.Sprintf("sent to %q; poll worker_status", slug)), nil
	})

	return &Server{mcp: s}
}

// Serve runs the MCP server over streamable HTTP at addr (endpoint /mcp).
func (s *Server) Serve(addr string) error {
	return server.NewStreamableHTTPServer(s.mcp).Start(addr)
}

// waitForStatus blocks up to min(wait, maxWaitSeconds) seconds, polling the
// store about once a second, and returns as soon as the watched scope is
// settled-or-needs-attention, the deadline elapses, or ctx is cancelled.
func waitForStatus(ctx context.Context, st *store.Store, projectID, slug string, wait int) {
	if wait > maxWaitSeconds {
		wait = maxWaitSeconds
	}
	// Check immediately so an already-settled scope returns without a sleep.
	if settledOrNeedsAttention(st, projectID, slug) {
		return
	}
	deadline := time.After(time.Duration(wait) * time.Second)
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-deadline:
			return
		case <-ticker.C:
			if settledOrNeedsAttention(st, projectID, slug) {
				return
			}
		}
	}
}

// settledOrNeedsAttention reports whether the watched scope has reached a state
// worth returning early for. With a slug: that one task is done/failed/blocked/
// needs_input. Without: any task needs_input or failed, or every task is
// done/failed/blocked (nothing left running or todo).
func settledOrNeedsAttention(st *store.Store, projectID, slug string) bool {
	if slug != "" {
		t, err := st.GetTask(projectID, slug)
		if err != nil || t == nil {
			return false
		}
		switch t.Status {
		case store.Done, store.Failed, store.Blocked, store.NeedsInput:
			return true
		}
		return false
	}
	tasks, err := st.ListTasks(projectID)
	if err != nil || len(tasks) == 0 {
		return false
	}
	allSettled := true
	for _, t := range tasks {
		if t.Status == store.NeedsInput || t.Status == store.Failed {
			return true
		}
		switch t.Status {
		case store.Done, store.Failed, store.Blocked:
		default:
			allSettled = false
		}
	}
	return allSettled
}

// taskView is the compact per-task shape returned to the PM.
type taskView struct {
	Slug     string   `json:"slug"`
	Title    string   `json:"title"`
	Status   string   `json:"status"`
	Deps     []string `json:"deps,omitempty"`
	Branch   string   `json:"branch,omitempty"`
	Question string   `json:"question,omitempty"`
	Result   string   `json:"result,omitempty"`
}

func tasksJSON(st *store.Store, projectID, slug string) (*mcp.CallToolResult, error) {
	var tasks []*store.Task
	if slug != "" {
		t, err := st.GetTask(projectID, slug)
		if err != nil {
			return mcp.NewToolResultErrorf("%v", err), nil
		}
		if t == nil {
			return mcp.NewToolResultErrorf("no task %q", slug), nil
		}
		tasks = []*store.Task{t}
	} else {
		var err error
		if tasks, err = st.ListTasks(projectID); err != nil {
			return mcp.NewToolResultErrorf("%v", err), nil
		}
	}
	views := make([]taskView, 0, len(tasks))
	for _, t := range tasks {
		views = append(views, taskView{
			Slug: t.Slug, Title: t.Title, Status: string(t.Status), Deps: t.Deps,
			Branch: t.Branch, Question: t.Question, Result: t.Result,
		})
	}
	data, _ := json.MarshalIndent(views, "", "  ")
	return mcp.NewToolResultText(string(data)), nil
}
