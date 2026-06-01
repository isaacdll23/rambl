package monitor

import (
	"fmt"
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

func TestRenderGroupsTasksByFeature(t *testing.T) {
	now := time.Now()
	features := []*store.Feature{
		{ID: "f1", Slug: "alpha", Title: "Alpha", Branch: "rambl/feat/alpha", Status: store.FeatureRunning},
		{ID: "f2", Slug: "beta", Title: "Beta", Status: store.FeaturePlanning},
	}
	tasks := []*store.Task{
		{Slug: "alpha-one", FeatureID: "f1", Status: store.Running, UpdatedAt: now.Add(-time.Minute)},
		{Slug: "beta-one", FeatureID: "f2", Status: store.Todo, UpdatedAt: now.Add(-2 * time.Minute)},
		{Slug: "loose-task", Status: store.Done, UpdatedAt: now.Add(-3 * time.Minute)},
	}

	out := render(view{
		name:      "demo",
		tasks:     tasks,
		features:  features,
		startedAt: now.Add(-time.Hour),
		width:     100,
		height:    40,
		animate:   false,
	})

	wants := []string{
		"feat",             // a feature header marker
		"alpha",            // first feature slug
		"beta",             // second feature slug
		"standalone",       // standalone header
		"alpha-one",        // feature task slugs...
		"beta-one",         //
		"loose-task",       // ...and the standalone task slug
		"rambl/feat/alpha", // a feature branch in its header
	}
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("grouped render missing %q\n---\n%s", w, out)
		}
	}
}

func TestRenderFlatWhenNoFeatures(t *testing.T) {
	now := time.Now()
	tasks := []*store.Task{
		{Slug: "api-routes", Status: store.Running, UpdatedAt: now.Add(-time.Minute)},
		{Slug: "db-schema", Status: store.Done, UpdatedAt: now.Add(-2 * time.Minute)},
	}
	out := render(view{
		name:      "demo",
		tasks:     tasks,
		startedAt: now.Add(-time.Hour),
		width:     100,
		height:    40,
		animate:   false,
	})

	for _, slug := range []string{"api-routes", "db-schema"} {
		if !strings.Contains(out, slug) {
			t.Errorf("flat render missing slug %q\n---\n%s", slug, out)
		}
	}
	if strings.Contains(out, "feat ") {
		t.Errorf("flat render should have no feature header, got:\n%s", out)
	}
	if strings.Contains(out, "standalone") {
		t.Errorf("flat render should have no standalone header, got:\n%s", out)
	}
}

func TestRenderGroupedSelectionOrder(t *testing.T) {
	now := time.Now()
	// Features are slug-ordered (alpha, beta). The standalone task sorts first by
	// task slug ("aaa-standalone") but must still render LAST in grouped order, so
	// selecting index 0 must expand the first feature's task, not the standalone.
	features := []*store.Feature{
		{ID: "f1", Slug: "alpha", Status: store.FeatureRunning},
		{ID: "f2", Slug: "beta", Status: store.FeatureRunning},
	}
	tasks := []*store.Task{
		{Slug: "aaa-standalone", Status: store.Done, Branch: "feat/loose", UpdatedAt: now.Add(-time.Minute)},
		{Slug: "alpha-task", FeatureID: "f1", Status: store.Running, Branch: "feat/alpha-task", UpdatedAt: now.Add(-time.Minute)},
		{Slug: "beta-task", FeatureID: "f2", Status: store.Running, Branch: "feat/beta-task", UpdatedAt: now.Add(-time.Minute)},
	}

	out := render(view{
		name:      "demo",
		tasks:     tasks,
		features:  features,
		selected:  0,
		startedAt: now.Add(-time.Hour),
		width:     100,
		height:    40,
		animate:   false,
	})

	// Index 0 in grouped order is alpha-task (first feature's first task).
	if !strings.Contains(out, "branch: feat/alpha-task") {
		t.Errorf("expected index 0 to expand the first feature's task, got:\n%s", out)
	}
	if strings.Contains(out, "branch: feat/loose") {
		t.Errorf("index 0 should not expand the standalone task, got:\n%s", out)
	}
}

// TestRenderWindowsToHeight drives render() with far more tasks than fit in a
// small terminal and a selection near the bottom. The selected row must stay on
// screen and an overflow indicator must signal the hidden rows above it.
func TestRenderWindowsToHeight(t *testing.T) {
	now := time.Now()
	var tasks []*store.Task
	for i := 0; i < 50; i++ {
		tasks = append(tasks, &store.Task{
			Slug:      fmt.Sprintf("task-%02d", i),
			Status:    store.Running,
			Result:    "working",
			Branch:    fmt.Sprintf("feat/%02d", i),
			UpdatedAt: now.Add(-time.Duration(i) * time.Minute),
		})
	}

	out := render(view{
		name:      "demo",
		tasks:     tasks,
		selected:  45, // near the bottom of the list
		startedAt: now.Add(-time.Hour),
		width:     100,
		height:    24, // small enough that all 50 rows cannot fit
		animate:   true,
	})

	// The selected row's slug and its expanded panel branch must be visible.
	if !strings.Contains(out, "task-45") {
		t.Errorf("windowed render dropped the selected row task-45:\n%s", out)
	}
	if !strings.Contains(out, "branch: feat/45") {
		t.Errorf("windowed render dropped the selected row's expanded panel:\n%s", out)
	}
	// Rows above the window are hidden behind an overflow affordance.
	if !strings.Contains(out, "↑") || !strings.Contains(out, "more") {
		t.Errorf("expected an overflow indicator for hidden rows, got:\n%s", out)
	}
	// A row from the far top must have scrolled out of the window.
	if strings.Contains(out, "task-00 ") {
		t.Errorf("expected the top row task-00 to be windowed out, got:\n%s", out)
	}
	// Sanity: the windowed output must not exceed the terminal height.
	if got := strings.Count(out, "\n"); got > 24 {
		t.Errorf("windowed render produced %d lines, exceeding height 24:\n%s", got, out)
	}
}

// TestRenderTinyHeightNoPanic exercises degenerate heights to confirm the
// windowing math never panics on negative repeats or slice bounds.
func TestRenderTinyHeightNoPanic(t *testing.T) {
	now := time.Now()
	var tasks []*store.Task
	for i := 0; i < 10; i++ {
		tasks = append(tasks, &store.Task{Slug: fmt.Sprintf("t-%d", i), Status: store.Done, UpdatedAt: now})
	}
	for _, h := range []int{0, 1, 2, 3, 5} {
		render(view{name: "demo", tasks: tasks, selected: 9, startedAt: now, width: 100, height: h, animate: true})
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
