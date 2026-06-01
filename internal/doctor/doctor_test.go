package doctor

import "testing"

func TestStatusString(t *testing.T) {
	tests := []struct {
		name   string
		status Status
		want   string
	}{
		{"ok", OK, "ok"},
		{"warn", Warn, "warn"},
		{"fail", Fail, "fail"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.status.String(); got != tt.want {
				t.Errorf("Status(%d).String() = %q, want %q", tt.status, got, tt.want)
			}
		})
	}
}

func TestRunOrderAndNameFill(t *testing.T) {
	// Synthetic in-memory checks: one returns its own Name, one leaves it
	// empty (so Run should fill it from Check.Name), preserving order.
	checks := []Check{
		{Name: "alpha", Run: func() Result { return Result{Name: "alpha", Status: OK, Detail: "a"} }},
		{Name: "beta", Run: func() Result { return Result{Status: Warn, Detail: "b"} }}, // empty Name
		{Name: "gamma", Run: func() Result { return Result{Name: "explicit", Status: Fail} }},
	}

	got := Run(checks)

	if len(got) != 3 {
		t.Fatalf("Run returned %d results, want 3", len(got))
	}

	want := []Result{
		{Name: "alpha", Status: OK, Detail: "a"},
		{Name: "beta", Status: Warn, Detail: "b"}, // filled from Check.Name
		{Name: "explicit", Status: Fail},          // explicit Name preserved
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("result[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestRenderText(t *testing.T) {
	results := []Result{
		{Name: "git", Status: OK, Detail: "/usr/bin/git"},
		{Name: "config", Status: Warn, Detail: "using defaults"},
		{Name: "network", Status: Fail}, // no detail
	}

	want := "[ok]   git — /usr/bin/git\n" +
		"[warn] config — using defaults\n" +
		"[fail] network\n"

	if got := RenderText(results); got != want {
		t.Errorf("RenderText() =\n%q\nwant\n%q", got, want)
	}
}

func TestRenderTextEmpty(t *testing.T) {
	if got := RenderText(nil); got != "" {
		t.Errorf("RenderText(nil) = %q, want empty string", got)
	}
}

func TestHasFailure(t *testing.T) {
	tests := []struct {
		name    string
		results []Result
		want    bool
	}{
		{"empty", nil, false},
		{"all ok", []Result{{Status: OK}, {Status: OK}}, false},
		{"warn only", []Result{{Status: OK}, {Status: Warn}}, false},
		{"contains fail", []Result{{Status: OK}, {Status: Warn}, {Status: Fail}}, true},
		{"only fail", []Result{{Status: Fail}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := HasFailure(tt.results); got != tt.want {
				t.Errorf("HasFailure() = %v, want %v", got, tt.want)
			}
		})
	}
}
