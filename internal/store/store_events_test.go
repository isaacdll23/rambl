package store

import (
	"path/filepath"
	"testing"
)

func TestEventsLog(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	proj, err := s.EnsureProject("/repo/calc", "calc")
	if err != nil {
		t.Fatalf("ensure project: %v", err)
	}

	// No events yet: empty slice, no error.
	if evs, err := s.RecentEvents(proj, 10); err != nil {
		t.Fatalf("recent (empty): %v", err)
	} else if len(evs) != 0 {
		t.Fatalf("want 0 events, got %d", len(evs))
	}

	appended := []struct {
		kind, slug, summary string
	}{
		{"create", "core", "created core"},
		{"dispatch", "core", "dispatched core"},
		{"send", "cli", "sent message to cli"},
	}
	for _, a := range appended {
		if err := s.AppendEvent(proj, a.kind, a.slug, a.summary); err != nil {
			t.Fatalf("append %s: %v", a.kind, err)
		}
	}

	// Newest first.
	evs, err := s.RecentEvents(proj, 10)
	if err != nil {
		t.Fatalf("recent: %v", err)
	}
	if len(evs) != 3 {
		t.Fatalf("want 3 events, got %d", len(evs))
	}
	wantOrder := []string{"send", "dispatch", "create"}
	for i, want := range wantOrder {
		if evs[i].Kind != want {
			t.Fatalf("event[%d].Kind = %q, want %q", i, evs[i].Kind, want)
		}
	}
	// Fields round-trip.
	if evs[0].ProjectID != proj || evs[0].Slug != "cli" || evs[0].Summary != "sent message to cli" {
		t.Fatalf("unexpected newest event: %+v", evs[0])
	}
	if evs[0].CreatedAt.IsZero() {
		t.Fatalf("created_at not parsed: %+v", evs[0])
	}
	// Monotonically descending ids.
	if !(evs[0].ID > evs[1].ID && evs[1].ID > evs[2].ID) {
		t.Fatalf("ids not descending: %d %d %d", evs[0].ID, evs[1].ID, evs[2].ID)
	}

	// limit respected.
	limited, err := s.RecentEvents(proj, 2)
	if err != nil {
		t.Fatalf("recent limit: %v", err)
	}
	if len(limited) != 2 {
		t.Fatalf("want 2 events with limit=2, got %d", len(limited))
	}
	if limited[0].Kind != "send" || limited[1].Kind != "dispatch" {
		t.Fatalf("unexpected limited order: %q %q", limited[0].Kind, limited[1].Kind)
	}

	// limit <= 0 defaults to 50 (returns all 3 here).
	def, err := s.RecentEvents(proj, 0)
	if err != nil {
		t.Fatalf("recent default: %v", err)
	}
	if len(def) != 3 {
		t.Fatalf("want 3 events with limit=0, got %d", len(def))
	}
}
