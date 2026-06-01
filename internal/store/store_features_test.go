package store

import (
	"path/filepath"
	"testing"
)

func openTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	proj, err := s.EnsureProject("/repo/feat", "feat")
	if err != nil {
		t.Fatalf("ensure project: %v", err)
	}
	return s, proj
}

func TestAddGetFeature(t *testing.T) {
	s, proj := openTestStore(t)

	f, err := s.AddFeature(proj, "checkout", "Checkout flow")
	if err != nil {
		t.Fatalf("add feature: %v", err)
	}
	if f.Status != FeaturePlanning {
		t.Fatalf("want status %q, got %q", FeaturePlanning, f.Status)
	}
	if f.ID == "" || f.CreatedAt.IsZero() || f.UpdatedAt.IsZero() {
		t.Fatalf("missing generated fields: %+v", f)
	}

	got, err := s.GetFeature(proj, "checkout")
	if err != nil {
		t.Fatalf("get feature: %v", err)
	}
	if got == nil {
		t.Fatal("get feature returned nil")
	}
	if got.ID != f.ID || got.ProjectID != proj || got.Slug != "checkout" ||
		got.Title != "Checkout flow" || got.Branch != "" || got.Status != FeaturePlanning {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if got.CreatedAt.Unix() != f.CreatedAt.Unix() {
		t.Fatalf("created_at mismatch: %v vs %v", got.CreatedAt, f.CreatedAt)
	}

	// Missing slug -> (nil, nil).
	missing, err := s.GetFeature(proj, "nope")
	if err != nil {
		t.Fatalf("get missing: %v", err)
	}
	if missing != nil {
		t.Fatalf("want nil for missing feature, got %+v", missing)
	}

	// Duplicate (project_id, slug) errors.
	if _, err := s.AddFeature(proj, "checkout", "dup"); err == nil {
		t.Fatal("want error on duplicate feature slug, got nil")
	}
}

func TestListFeaturesOrdered(t *testing.T) {
	s, proj := openTestStore(t)

	for _, slug := range []string{"gamma", "alpha", "beta"} {
		if _, err := s.AddFeature(proj, slug, slug); err != nil {
			t.Fatalf("add %s: %v", slug, err)
		}
	}
	feats, err := s.ListFeatures(proj)
	if err != nil {
		t.Fatalf("list features: %v", err)
	}
	if len(feats) != 3 {
		t.Fatalf("want 3 features, got %d", len(feats))
	}
	want := []string{"alpha", "beta", "gamma"}
	for i, w := range want {
		if feats[i].Slug != w {
			t.Fatalf("order[%d] = %q, want %q", i, feats[i].Slug, w)
		}
	}
}

func TestUpdateFeature(t *testing.T) {
	s, proj := openTestStore(t)

	f, err := s.AddFeature(proj, "search", "Search")
	if err != nil {
		t.Fatalf("add feature: %v", err)
	}
	f.Branch = "rambl/feat/search"
	f.Status = FeatureRunning
	if err := s.UpdateFeature(f); err != nil {
		t.Fatalf("update feature: %v", err)
	}

	got, err := s.GetFeature(proj, "search")
	if err != nil {
		t.Fatalf("get feature: %v", err)
	}
	if got.Branch != "rambl/feat/search" || got.Status != FeatureRunning {
		t.Fatalf("update not persisted: %+v", got)
	}
}

func TestTasksByFeature(t *testing.T) {
	s, proj := openTestStore(t)

	f, err := s.AddFeature(proj, "billing", "Billing")
	if err != nil {
		t.Fatalf("add feature: %v", err)
	}

	if _, err := s.AddTaskToFeature(proj, f.ID, "invoice", "Invoice", "build invoice", nil); err != nil {
		t.Fatalf("add invoice: %v", err)
	}
	if _, err := s.AddTaskToFeature(proj, f.ID, "charge", "Charge", "build charge", []string{"invoice"}); err != nil {
		t.Fatalf("add charge: %v", err)
	}
	// A standalone task must not appear under the feature.
	if _, err := s.AddTask(proj, "loose", "Loose", "standalone", nil); err != nil {
		t.Fatalf("add standalone: %v", err)
	}

	tasks, err := s.TasksByFeature(proj, f.ID)
	if err != nil {
		t.Fatalf("tasks by feature: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("want 2 tasks, got %d", len(tasks))
	}
	// Ordered by slug: charge, invoice.
	if tasks[0].Slug != "charge" || tasks[1].Slug != "invoice" {
		t.Fatalf("order: %q, %q", tasks[0].Slug, tasks[1].Slug)
	}
	for _, tk := range tasks {
		if tk.FeatureID != f.ID {
			t.Fatalf("task %q feature_id = %q, want %q", tk.Slug, tk.FeatureID, f.ID)
		}
	}
	// Deps populated via depsOf.
	if len(tasks[0].Deps) != 1 || tasks[0].Deps[0] != "invoice" {
		t.Fatalf("charge deps = %v, want [invoice]", tasks[0].Deps)
	}

	// Standalone task has empty FeatureID and is not returned for the feature.
	loose, err := s.GetTask(proj, "loose")
	if err != nil {
		t.Fatalf("get loose: %v", err)
	}
	if loose.FeatureID != "" {
		t.Fatalf("standalone FeatureID = %q, want empty", loose.FeatureID)
	}
	for _, tk := range tasks {
		if tk.Slug == "loose" {
			t.Fatal("standalone task returned by TasksByFeature")
		}
	}
}

func TestDeleteFeature(t *testing.T) {
	s, proj := openTestStore(t)

	f, err := s.AddFeature(proj, "reports", "Reports")
	if err != nil {
		t.Fatalf("add feature: %v", err)
	}
	if _, err := s.AddTaskToFeature(proj, f.ID, "export", "Export", "build export", nil); err != nil {
		t.Fatalf("add task: %v", err)
	}

	// Errors while a task references the feature.
	if err := s.DeleteFeature(proj, "reports"); err == nil {
		t.Fatal("want error deleting feature with tasks, got nil")
	}

	// Remove the task, then delete succeeds.
	if err := s.DeleteTask(proj, "export"); err != nil {
		t.Fatalf("delete task: %v", err)
	}
	if err := s.DeleteFeature(proj, "reports"); err != nil {
		t.Fatalf("delete feature: %v", err)
	}
	got, err := s.GetFeature(proj, "reports")
	if err != nil {
		t.Fatalf("get after delete: %v", err)
	}
	if got != nil {
		t.Fatalf("feature still present after delete: %+v", got)
	}

	// Deleting a missing feature errors.
	if err := s.DeleteFeature(proj, "ghost"); err == nil {
		t.Fatal("want error deleting missing feature, got nil")
	}
}
