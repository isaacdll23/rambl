// Package mcpserver exposes the PM's tool surface over MCP (HTTP transport).
// These are the verbs the planning agent uses to *act*: build the task graph,
// dispatch workers, watch them, and answer the ones that get blocked. All state
// lives in the store; workers are managed by the runner.
package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"rambl/internal/runner"
	"rambl/internal/store"
)

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
		mcp.WithDescription("Check worker/task status. With a slug, returns that task; without, returns all. A task in 'needs_input' has a 'question' you should answer (from your own knowledge if possible) via worker_send, or escalate to the human."),
		mcp.WithString("slug", mcp.Description("optional task slug; omit for all")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return tasksJSON(st, projectID, req.GetString("slug", ""))
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
