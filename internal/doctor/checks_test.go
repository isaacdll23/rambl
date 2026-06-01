package doctor

import "testing"

func TestChecksOrderAndShape(t *testing.T) {
	checks := Checks()

	wantNames := []string{"claude CLI", "git", "~/.rambl writable"}
	if len(checks) != len(wantNames) {
		t.Fatalf("Checks() returned %d checks, want %d", len(checks), len(wantNames))
	}

	for i, want := range wantNames {
		if checks[i].Name != want {
			t.Errorf("check[%d].Name = %q, want %q", i, checks[i].Name, want)
		}
		if checks[i].Run == nil {
			t.Errorf("check[%d] (%q) has nil Run", i, want)
		}
	}
}

func TestRunChecksStructuralInvariants(t *testing.T) {
	checks := Checks()
	results := Run(checks)

	if len(results) != len(checks) {
		t.Fatalf("Run returned %d results, want %d", len(results), len(checks))
	}

	for i, r := range results {
		if r.Name == "" {
			t.Errorf("result[%d] has empty Name", i)
		}
	}

	// The "git" result must be one of OK/Fail and carry a Detail, without
	// asserting which outcome (depends on the host's PATH).
	git := results[1]
	if git.Name != "git" {
		t.Fatalf("result[1].Name = %q, want %q", git.Name, "git")
	}
	if git.Status != OK && git.Status != Fail {
		t.Errorf("git status = %v, want OK or Fail", git.Status)
	}
	if git.Detail == "" {
		t.Errorf("git result has empty Detail")
	}
}

func TestRamblWritableNeverFails(t *testing.T) {
	results := Run(Checks())
	rambl := results[2]
	if rambl.Name != "~/.rambl writable" {
		t.Fatalf("result[2].Name = %q, want %q", rambl.Name, "~/.rambl writable")
	}
	if rambl.Status != OK && rambl.Status != Warn {
		t.Errorf("~/.rambl writable status = %v, want OK or Warn", rambl.Status)
	}
}
