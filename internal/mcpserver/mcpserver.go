// Package mcpserver exposes the PM's tool surface over MCP (HTTP transport).
// These are the verbs the planning agent uses to *act*: build the task graph,
// dispatch workers, watch them, and answer the ones that get blocked. All state
// lives in the store; workers are managed by the runner.
package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"rambl/internal/runner"
	"rambl/internal/store"
)

// maxWaitSeconds caps how long worker_status will block server-side.
const maxWaitSeconds = 90

// logEvent records a PM activity event; best-effort, so a store failure never
// changes the tool call's result or error. Called only on handler success paths.
func logEvent(st *store.Store, projectID, kind, slug, summary string) {
	_ = st.AppendEvent(projectID, kind, slug, summary)
}

// createSummary builds the create_task event summary, appending a deps suffix
// only when the task has prerequisites.
func createSummary(slug string, deps []string) string {
	if len(deps) == 0 {
		return fmt.Sprintf("created %s", slug)
	}
	return fmt.Sprintf("created %s (deps: %s)", slug, strings.Join(deps, ","))
}

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
		mcp.WithString("feature", mcp.Description("optional feature slug; when set, the task belongs to that feature and is merged into its branch instead of getting its own PR")),
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
		feature := req.GetString("feature", "")
		if feature == "" {
			if _, err := st.AddTask(projectID, slug, title, prompt, deps); err != nil {
				return mcp.NewToolResultErrorf("create_task: %v", err), nil
			}
		} else {
			f, err := st.GetFeature(projectID, feature)
			if err != nil {
				return mcp.NewToolResultErrorf("create_task: %v", err), nil
			}
			if f == nil {
				return mcp.NewToolResultErrorf("create_task: no feature %q", feature), nil
			}
			if _, err := st.AddTaskToFeature(projectID, f.ID, slug, title, prompt, deps); err != nil {
				return mcp.NewToolResultErrorf("create_task: %v", err), nil
			}
		}
		logEvent(st, projectID, "create", slug, createSummary(slug, deps))
		return mcp.NewToolResultText(fmt.Sprintf("created task %q (deps %v)", slug, deps)), nil
	})

	s.AddTool(mcp.NewTool("create_feature",
		mcp.WithDescription("Create a feature: a named group of tasks that land together as ONE pull request via a dedicated rambl/feat/<slug> branch. Add tasks to it with create_task(feature=<slug>), then run them all with dispatch_feature."),
		mcp.WithString("slug", mcp.Required(), mcp.Description("unique kebab-case feature id, e.g. auth")),
		mcp.WithString("title", mcp.Required(), mcp.Description("short human title")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		slug, err := req.RequireString("slug")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		title, err := req.RequireString("title")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if _, err := st.AddFeature(projectID, slug, title); err != nil {
			return mcp.NewToolResultErrorf("create_feature: %v", err), nil
		}
		logEvent(st, projectID, "create_feature", slug, fmt.Sprintf("created feature %s", slug))
		return mcp.NewToolResultText(fmt.Sprintf("created feature %q (branch rambl/feat/%s on dispatch)", slug, slug)), nil
	})

	s.AddTool(mcp.NewTool("dispatch_feature",
		mcp.WithDescription("Run an entire feature autonomously: dispatches its ready tasks in parallel, squash-merges each completed task into the feature branch in dependency order, runs an integration gate to keep the branch green, and auto-opens the feat→main PR once all tasks are merged and green. Returns immediately; poll feature_status. Requires the feature to exist and have at least one task."),
		mcp.WithString("slug", mcp.Required(), mcp.Description("feature slug to dispatch")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		slug, err := req.RequireString("slug")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		f, err := st.GetFeature(projectID, slug)
		if err != nil {
			return mcp.NewToolResultErrorf("dispatch_feature: %v", err), nil
		}
		if f == nil {
			return mcp.NewToolResultErrorf("dispatch_feature: no feature %q", slug), nil
		}
		tasks, err := st.TasksByFeature(projectID, f.ID)
		if err != nil {
			return mcp.NewToolResultErrorf("dispatch_feature: %v", err), nil
		}
		if len(tasks) == 0 {
			return mcp.NewToolResultErrorf("dispatch_feature: feature %q has no tasks", slug), nil
		}
		go func() {
			if err := rn.RunFeature(projectID, slug); err != nil {
				if ff, _ := st.GetFeature(projectID, slug); ff != nil {
					ff.Status = store.FeatureFailed
					_ = st.UpdateFeature(ff)
				}
				logEvent(st, projectID, "feature", slug, fmt.Sprintf("feature %s failed: %v", slug, err))
			}
		}()
		logEvent(st, projectID, "dispatch_feature", slug, fmt.Sprintf("dispatched feature %s", slug))
		return mcp.NewToolResultText(fmt.Sprintf("dispatching feature %q; poll feature_status", slug)), nil
	})

	s.AddTool(mcp.NewTool("feature_status",
		mcp.WithDescription("Inspect features and their tasks. With a slug, returns that one feature; without, all features. Each feature reports its status (planning/running/integrating/done/failed), branch, and the status of every task under it."),
		mcp.WithString("slug", mcp.Description("optional feature slug; omit for all")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		slug := req.GetString("slug", "")
		return featuresJSON(st, projectID, slug)
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
		if err := rn.DispatchManual(projectID, slug); err != nil {
			return mcp.NewToolResultErrorf("dispatch: %v", err), nil
		}
		logEvent(st, projectID, "dispatch", slug, fmt.Sprintf("dispatched %s", slug))
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
		logEvent(st, projectID, "send", slug, fmt.Sprintf("sent input to %s", slug))
		return mcp.NewToolResultText(fmt.Sprintf("sent to %q; poll worker_status", slug)), nil
	})

	s.AddTool(mcp.NewTool("stop_worker",
		mcp.WithDescription("Stop a live worker mid-run. Terminates its session and marks the task failed (stopped by the PM), leaving its branch intact so it can be re-dispatched later. Use to halt a runaway, stuck, or no-longer-wanted worker. Errors if the task has no live worker."),
		mcp.WithString("slug", mcp.Required(), mcp.Description("task slug of the live worker to stop")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		slug, err := req.RequireString("slug")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if err := rn.Stop(projectID, slug); err != nil {
			return mcp.NewToolResultErrorf("stop_worker: %v", err), nil
		}
		logEvent(st, projectID, "stop", slug, fmt.Sprintf("stopped %s", slug))
		return mcp.NewToolResultText(fmt.Sprintf("stopped %q; branch left intact — re-dispatch to retry", slug)), nil
	})

	s.AddTool(mcp.NewTool("delete_task",
		mcp.WithDescription("Permanently delete a task and reclaim its git worktree and branch. Use to prune stale, duplicate, or superseded tasks, or to tidy a task whose work is already merged. Refuses a task that is currently running. This cannot be undone."),
		mcp.WithString("slug", mcp.Required(), mcp.Description("task slug to delete")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		slug, err := req.RequireString("slug")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if err := rn.Delete(projectID, slug); err != nil {
			return mcp.NewToolResultErrorf("delete_task: %v", err), nil
		}
		logEvent(st, projectID, "delete", slug, fmt.Sprintf("deleted %s", slug))
		return mcp.NewToolResultText(fmt.Sprintf("deleted task %q (worktree and branch reclaimed)", slug)), nil
	})

	s.AddTool(mcp.NewTool("read_diff",
		mcp.WithDescription("Show the diff of a task's rambl/<slug> branch (stat plus patch) so you can review what the worker actually changed before validating or shipping it."),
		mcp.WithString("slug", mcp.Required(), mcp.Description("task slug to diff")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		slug, err := req.RequireString("slug")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		out, err := rn.Diff(projectID, slug)
		if err != nil {
			return mcp.NewToolResultErrorf("read_diff: %v", err), nil
		}
		return mcp.NewToolResultText(out), nil
	})

	s.AddTool(mcp.NewTool("verify_task",
		mcp.WithDescription("Run a build/test command inside the task's worktree and return its PASS/FAIL output, to validate a worker's work. Pass an explicit command (e.g. 'go build ./... && go test ./...'); if omitted, a Go project is auto-detected."),
		mcp.WithString("slug", mcp.Required(), mcp.Description("task slug to verify")),
		mcp.WithString("command", mcp.Description("optional build/test command; if omitted, a Go project is auto-detected")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		slug, err := req.RequireString("slug")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		command := req.GetString("command", "")
		out, err := rn.Verify(projectID, slug, command)
		if err != nil {
			return mcp.NewToolResultErrorf("verify_task: %v", err), nil
		}
		logEvent(st, projectID, "verify", slug, fmt.Sprintf("verified %s", slug))
		return mcp.NewToolResultText(out), nil
	})

	s.AddTool(mcp.NewTool("revise_task",
		mcp.WithDescription("Hand a finished task's branch back to a worker with feedback so it iterates on its own prior output (reuses the live session if present, else reopens the branch). Use after read_diff/verify_task surface issues. Poll worker_status afterward."),
		mcp.WithString("slug", mcp.Required(), mcp.Description("task slug to revise")),
		mcp.WithString("message", mcp.Required(), mcp.Description("feedback for the worker to iterate on")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		slug, err := req.RequireString("slug")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		message, err := req.RequireString("message")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		if err := rn.Revise(projectID, slug, message); err != nil {
			return mcp.NewToolResultErrorf("revise_task: %v", err), nil
		}
		logEvent(st, projectID, "revise", slug, fmt.Sprintf("revised %s", slug))
		return mcp.NewToolResultText(fmt.Sprintf("revising %q; poll worker_status", slug)), nil
	})

	s.AddTool(mcp.NewTool("open_pr",
		mcp.WithDescription("Push the task's rambl/<slug> branch to origin and open a GitHub pull request for the human to review. Only call after you have reviewed the diff (read_diff), validated the work (verify_task), and completed any needed revisions. Requires gh and an origin GitHub remote. Returns the PR URL."),
		mcp.WithString("slug", mcp.Required(), mcp.Description("task slug to open a PR for")),
		mcp.WithString("title", mcp.Description("PR title; defaults to the task title")),
		mcp.WithString("body", mcp.Description("PR body, markdown")),
	), func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		slug, err := req.RequireString("slug")
		if err != nil {
			return mcp.NewToolResultError(err.Error()), nil
		}
		title := req.GetString("title", "")
		body := req.GetString("body", "")
		url, err := rn.OpenPR(projectID, slug, title, body)
		if err != nil {
			return mcp.NewToolResultErrorf("open_pr: %v", err), nil
		}
		logEvent(st, projectID, "open_pr", slug, fmt.Sprintf("opened PR for %s", slug))
		return mcp.NewToolResultText(fmt.Sprintf("opened PR for %q: %s", slug, url)), nil
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

// featureView is the compact per-feature shape returned to the PM, with its
// tasks rendered in the same shape as tasksJSON.
type featureView struct {
	Slug   string     `json:"slug"`
	Title  string     `json:"title"`
	Status string     `json:"status"`
	Branch string     `json:"branch,omitempty"`
	Tasks  []taskView `json:"tasks"`
}

func featuresJSON(st *store.Store, projectID, slug string) (*mcp.CallToolResult, error) {
	var features []*store.Feature
	if slug != "" {
		f, err := st.GetFeature(projectID, slug)
		if err != nil {
			return mcp.NewToolResultErrorf("%v", err), nil
		}
		if f == nil {
			return mcp.NewToolResultErrorf("no feature %q", slug), nil
		}
		features = []*store.Feature{f}
	} else {
		var err error
		if features, err = st.ListFeatures(projectID); err != nil {
			return mcp.NewToolResultErrorf("%v", err), nil
		}
	}
	views := make([]featureView, 0, len(features))
	for _, f := range features {
		tasks, err := st.TasksByFeature(projectID, f.ID)
		if err != nil {
			return mcp.NewToolResultErrorf("%v", err), nil
		}
		taskViews := make([]taskView, 0, len(tasks))
		for _, t := range tasks {
			taskViews = append(taskViews, taskView{
				Slug: t.Slug, Title: t.Title, Status: string(t.Status), Deps: t.Deps,
				Branch: t.Branch, Question: t.Question, Result: t.Result,
			})
		}
		views = append(views, featureView{
			Slug: f.Slug, Title: f.Title, Status: string(f.Status), Branch: f.Branch, Tasks: taskViews,
		})
	}
	data, _ := json.MarshalIndent(views, "", "  ")
	return mcp.NewToolResultText(string(data)), nil
}
