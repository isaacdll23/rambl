package worker

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiffBranch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()

	run := func(args ...string) {
		t.Helper()
		if out, err := gitID(repo, args...); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	write := func(content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, "file.txt"), []byte(content), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	run("init", "-b", "base")
	write("one\n")
	run("add", "-A")
	run("commit", "-m", "initial")

	run("checkout", "-b", "feature")
	write("one\ntwo\n")
	run("add", "-A")
	run("commit", "-m", "change")

	stat, patch, err := DiffBranch(repo, "base", "feature")
	if err != nil {
		t.Fatalf("DiffBranch: %v", err)
	}
	if stat == "" {
		t.Errorf("expected non-empty stat")
	}
	if patch == "" {
		t.Errorf("expected non-empty patch")
	}
	if !strings.Contains(stat, "file.txt") {
		t.Errorf("stat does not mention file.txt: %q", stat)
	}
	if !strings.Contains(patch, "file.txt") {
		t.Errorf("patch does not mention file.txt: %q", patch)
	}
}
