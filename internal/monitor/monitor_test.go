package monitor

import (
	"strings"
	"testing"
	"time"

	"rambl/internal/store"
)

func TestRenderActiveDashboard(t *testing.T) {
	now := time.Now()
	tasks := []*store.Task{
		{Slug: "api-routes", Status: store.Running, Result: "wiring handlers", UpdatedAt: now.Add(-30 * time.Second), Branch: "feat/api"},
		{Slug: "db-schema", Status: store.Done, Result: "migrations applied", UpdatedAt: now.Add(-5 * time.Minute)},
		{Slug: "auth-flow", Status: store.NeedsInput, Question: "which provider?", UpdatedAt: now.Add(-1 * time.Minute)},
		{Slug: "ci-setup", Status: store.Failed, Result: "lint broke", UpdatedAt: now.Add(-2 * time.Minute)},
	}
	events := []*store.Event{
		{Kind: "dispatch", Slug: "api-routes", Summary: "dispatched api-routes", CreatedAt: now.Add(-10 * time.Second)},
		{Kind: "create", Slug: "db-schema", Summary: "created db-schema", CreatedAt: now.Add(-3 * time.Minute)},
	}

	out := render(view{
		name:      "demo",
		tasks:     tasks,
		events:    events,
		frame:     3,
		selected:  0,
		startedAt: now.Add(-7 * time.Minute),
		width:     100,
		height:    40,
		animate:   true,
	})

	wants := []string{
		"rambl · demo",          // header title
		"running",               // status word
		"done",                  // status word
		"needs_input",           // status word
		"api-routes",            // a slug
		"db-schema",             // a slug
		"▓",                     // a gauge block rune
		"░",                     // a gauge block rune
		"dispatched api-routes", // an event summary
		spinnerFrames[3],        // animated spinner for the running task at frame 3
		"✓",                     // static glyph for done
		"⚠",                     // static glyph for needs_input
		"WORKERS",
		"PM ACTIVITY",
		"branch: feat/api", // expanded detail of the selected (first) task
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("render output missing %q\n---\n%s", w, out)
		}
	}
}

func TestRenderIdleWhenNothingRunning(t *testing.T) {
	now := time.Now()
	tasks := []*store.Task{
		{Slug: "a", Status: store.Done, UpdatedAt: now.Add(-time.Minute)},
		{Slug: "b", Status: store.Done, UpdatedAt: now.Add(-2 * time.Minute)},
	}
	out := render(view{name: "demo", tasks: tasks, startedAt: now, width: 100, height: 40, animate: false})
	if !strings.Contains(out, "idle") {
		t.Errorf("expected idle banner when no task is running, got:\n%s", out)
	}
	if !strings.Contains(out, "waiting for a worker") {
		t.Errorf("expected 'waiting for a worker' idle text, got:\n%s", out)
	}
}

func TestRenderSplashWhenNoTasks(t *testing.T) {
	out := render(view{name: "demo", width: 80, height: 24, animate: false})
	if !strings.Contains(out, "idle · waiting for work") {
		t.Errorf("expected idle splash for an empty project, got:\n%s", out)
	}
	if !strings.Contains(out, "rambl · demo") {
		t.Errorf("expected project name in splash, got:\n%s", out)
	}
}

func TestStaticGlyphWhenNotAnimating(t *testing.T) {
	if g := glyph(store.Running, 5, false); g != spinnerFrames[0] {
		t.Errorf("non-animated running glyph = %q, want %q", g, spinnerFrames[0])
	}
	if g := glyph(store.Failed, 0, true); g != "✗" {
		t.Errorf("failed glyph = %q, want ✗", g)
	}
}

func TestBarProportional(t *testing.T) {
	if got := bar(0, 4, 6); got != strings.Repeat("░", 6) {
		t.Errorf("bar(0,4,6) = %q, want all empty", got)
	}
	if got := bar(4, 4, 6); got != strings.Repeat("▓", 6) {
		t.Errorf("bar(4,4,6) = %q, want all filled", got)
	}
	// a present-but-small count still shows at least one filled cell
	if got := bar(1, 100, 6); !strings.HasPrefix(got, "▓") {
		t.Errorf("bar(1,100,6) = %q, want at least one filled cell", got)
	}
}

// TestSnapshotMode exercises the same path Once() uses (animate=false) end-to-end
// against a real store, ensuring it renders without animation and includes the
// worker table plus PM activity.
func TestSnapshotMode(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	proj, err := st.EnsureProject("/repo/calc", "calc")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	tk, err := st.AddTask(proj, "core", "Core lib", "prompt", nil)
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	tk.Status, tk.Result = store.Running, "building"
	if err := st.Update(tk); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := st.AppendEvent(proj, "dispatch", "core", "dispatched core"); err != nil {
		t.Fatalf("event: %v", err)
	}

	tasks, _ := st.ListTasks(proj)
	events, _ := st.RecentEvents(proj, 20)
	out := render(view{name: "calc", tasks: tasks, events: events, startedAt: time.Now(), width: 100, animate: false})
	t.Logf("\n%s", out)

	for _, want := range []string{"rambl · calc", "core", "running", "WORKERS", "PM ACTIVITY", "dispatched core"} {
		if !strings.Contains(out, want) {
			t.Errorf("snapshot missing %q", want)
		}
	}
}
