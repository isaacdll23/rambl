package monitor

import (
	"strings"
	"testing"

	"rambl/internal/store"
)

// Verifies the snapshot render against a store with varied task states. Run with
// -v to see the rendered table.
func TestRenderSnapshot(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/state.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	proj, err := st.EnsureProject("/repo/calc", "calc")
	if err != nil {
		t.Fatalf("project: %v", err)
	}

	add := func(slug, title string, status store.Status, deps []string, q, res string) {
		tk, err := st.AddTask(proj, slug, title, "prompt", deps)
		if err != nil {
			t.Fatalf("add %s: %v", slug, err)
		}
		tk.Status, tk.Question, tk.Result = status, q, res
		if err := st.Update(tk); err != nil {
			t.Fatalf("update %s: %v", slug, err)
		}
	}
	add("scaffold", "Scaffold", store.Done, nil, "", "Created pyproject.toml and package.")
	add("core", "Core lib", store.Running, []string{"scaffold"}, "", "")
	add("cli", "CLI", store.NeedsInput, []string{"core"}, "Paginate the feed or infinite scroll?", "")
	add("tests", "Tests", store.Failed, []string{"core"}, "", "pytest collection error in test_core.py")

	tasks, err := st.ListTasks(proj)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	out := render("calc", tasks, false, 100)
	t.Logf("\n%s", out)

	for _, want := range []string{
		"rambl · calc", "scaffold", "done", "core", "running",
		"cli", "needs_input", "Paginate the feed", "tests", "failed",
		"1 running, 1 need input, 1 done, 1 failed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered snapshot missing %q", want)
		}
	}
}
