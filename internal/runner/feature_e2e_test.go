package runner

import (
	"os/exec"
	"strings"
	"sync"
	"testing"

	"rambl/internal/store"
)

// indexOf returns the position of slug in order, or -1 if absent.
func indexOf(order []string, slug string) int {
	for i, s := range order {
		if s == slug {
			return i
		}
	}
	return -1
}

// featSubject parses the task slug out of a "feat(<slug>): <title>" commit
// subject; returns "" if the subject is not a feat() merge commit.
func featSubject(subject string) string {
	if !strings.HasPrefix(subject, "feat(") {
		return ""
	}
	rest := subject[len("feat("):]
	i := strings.Index(rest, ")")
	if i < 0 {
		return ""
	}
	return rest[:i]
}

// TestFeatureLifecycleE2E drives a realistic diamond feature DAG through
// RunFeature using the test seams (no real Claude sessions, no network) and
// asserts the parallel-dispatch + topo-merge + single-PR behavior end-to-end.
func TestFeatureLifecycleE2E(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	h := newHarness(t)

	// Diamond: schema (root) → {api, ui} → wire.
	f, err := h.st.AddFeature(h.projectID, "checkout", "Checkout")
	if err != nil {
		t.Fatalf("AddFeature: %v", err)
	}
	for _, tc := range []struct {
		slug string
		deps []string
	}{
		{"schema", nil},
		{"api", []string{"schema"}},
		{"ui", []string{"schema"}},
		{"wire", []string{"api", "ui"}},
	} {
		if _, err := h.st.AddTaskToFeature(h.projectID, f.ID, tc.slug, tc.slug+" title", "do "+tc.slug, tc.deps); err != nil {
			t.Fatalf("AddTaskToFeature %q: %v", tc.slug, err)
		}
	}

	var order []string
	h.r.runTask = h.fakeRunTask("checkout", nil, &order)

	var mu sync.Mutex
	var prCalls int
	var prSlug string
	h.r.openFeaturePRFn = func(_, featureSlug, _ string) (string, error) {
		mu.Lock()
		prCalls++
		prSlug = featureSlug
		mu.Unlock()
		return "https://example/pr/checkout", nil
	}

	if err := h.r.RunFeature(h.projectID, "checkout"); err != nil {
		t.Fatalf("RunFeature: %v", err)
	}

	// --- parallel + ordered dispatch ---------------------------------------
	if len(order) != 4 {
		t.Fatalf("dispatch order = %v, want 4 entries", order)
	}
	iSchema := indexOf(order, "schema")
	iAPI := indexOf(order, "api")
	iUI := indexOf(order, "ui")
	iWire := indexOf(order, "wire")
	if iSchema < 0 || iAPI < 0 || iUI < 0 || iWire < 0 {
		t.Fatalf("dispatch order missing a task: %v", order)
	}
	// schema first; api and ui both after schema and both before wire (last).
	if iSchema != 0 {
		t.Errorf("schema dispatched at index %d, want 0 (first): %v", iSchema, order)
	}
	if !(iSchema < iAPI && iSchema < iUI) {
		t.Errorf("schema not before api/ui: schema=%d api=%d ui=%d (%v)", iSchema, iAPI, iUI, order)
	}
	if !(iAPI < iWire && iUI < iWire) {
		t.Errorf("api/ui not before wire: api=%d ui=%d wire=%d (%v)", iAPI, iUI, iWire, order)
	}
	if iWire != 3 {
		t.Errorf("wire dispatched at index %d, want 3 (last): %v", iWire, order)
	}

	// --- feature branch: four feat() commits in a valid topo order ---------
	out := runGit(t, h.repo, "log", "--format=%s", featureBranch("checkout"))
	var feats []string // newest-first
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if s := featSubject(strings.TrimSpace(line)); s != "" {
			feats = append(feats, s)
		}
	}
	if len(feats) != 4 {
		t.Fatalf("feature branch has %d feat() commits, want 4: %v (full log: %q)", len(feats), feats, out)
	}
	// Reverse to chronological (oldest merge first).
	chrono := []string{feats[3], feats[2], feats[1], feats[0]}
	if chrono[0] != "schema" {
		t.Errorf("first merged commit = %q, want schema (chrono: %v)", chrono[0], chrono)
	}
	if chrono[3] != "wire" {
		t.Errorf("last merged commit = %q, want wire (chrono: %v)", chrono[3], chrono)
	}
	mid := map[string]bool{chrono[1]: true, chrono[2]: true}
	if !mid["api"] || !mid["ui"] {
		t.Errorf("middle merges = %v, want {api, ui} (chrono: %v)", mid, chrono)
	}

	// --- auto-PR fired exactly once for this feature -----------------------
	if prCalls != 1 {
		t.Errorf("openFeaturePRFn fired %d times, want exactly 1", prCalls)
	}
	if prSlug != "checkout" {
		t.Errorf("auto-PR feature slug = %q, want checkout", prSlug)
	}
}

// TestFeatureLifecycleE2ETaskFailure: when a task fails mid-feature, RunFeature
// returns an error naming the task and "did not complete", and no PR is opened.
func TestFeatureLifecycleE2ETaskFailure(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	h := newHarness(t)

	f, err := h.st.AddFeature(h.projectID, "checkout", "Checkout")
	if err != nil {
		t.Fatalf("AddFeature: %v", err)
	}
	for _, tc := range []struct {
		slug string
		deps []string
	}{
		{"schema", nil},
		{"api", []string{"schema"}},
		{"ui", []string{"schema"}},
		{"wire", []string{"api", "ui"}},
	} {
		if _, err := h.st.AddTaskToFeature(h.projectID, f.ID, tc.slug, tc.slug+" title", "do "+tc.slug, tc.deps); err != nil {
			t.Fatalf("AddTaskToFeature %q: %v", tc.slug, err)
		}
	}

	h.r.runTask = h.fakeRunTask("checkout", map[string]store.Status{"api": store.Failed}, nil)

	var prCalls int
	h.r.openFeaturePRFn = func(_, _, _ string) (string, error) {
		prCalls++
		return "https://example/pr/checkout", nil
	}

	err = h.r.RunFeature(h.projectID, "checkout")
	wantErr(t, err, "did not complete")
	wantErr(t, err, "api")
	if prCalls != 0 {
		t.Errorf("openFeaturePRFn fired %d times on a failed feature, want 0", prCalls)
	}
}
