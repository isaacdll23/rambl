package picker

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestValidateRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}

	// A freshly git-init'd dir validates to its own toplevel.
	repo := t.TempDir()
	cmd := exec.Command("git", "init", repo)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init failed: %v\n%s", err, out)
	}

	top, err := validateRepo(repo)
	if err != nil {
		t.Fatalf("validateRepo(%q) returned error: %v", repo, err)
	}
	// macOS temp dirs live under symlinked /var → /private/var, so compare
	// the resolved real paths rather than the raw strings.
	wantReal, _ := filepath.EvalSymlinks(repo)
	gotReal, _ := filepath.EvalSymlinks(top)
	if gotReal != wantReal {
		t.Errorf("validateRepo(%q) = %q, want toplevel %q", repo, gotReal, wantReal)
	}

	// A non-repo dir is invalid.
	notRepo := t.TempDir()
	if _, err := validateRepo(notRepo); err == nil {
		t.Errorf("validateRepo(%q) on a non-repo dir = nil error, want error", notRepo)
	}
}

func TestNormalizePathTildeExpansion(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home dir: %v", err)
	}

	cases := map[string]string{
		"~":             home,
		"~/foo":         filepath.Join(home, "foo"),
		"~/foo/bar.txt": filepath.Join(home, "foo", "bar.txt"),
	}
	for in, want := range cases {
		got, err := normalizePath(in)
		if err != nil {
			t.Fatalf("normalizePath(%q) error: %v", in, err)
		}
		if got != want {
			t.Errorf("normalizePath(%q) = %q, want %q", in, got, want)
		}
	}

	// A literal "~name" (no slash) is NOT a home expansion — it should be
	// treated as a relative path and made absolute, not joined to home.
	got, err := normalizePath("~bob")
	if err != nil {
		t.Fatalf("normalizePath(~bob) error: %v", err)
	}
	if got == filepath.Join(home, "bob") {
		t.Errorf("normalizePath(~bob) = %q, should not expand a username form", got)
	}
}
