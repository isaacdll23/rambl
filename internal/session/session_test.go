package session

import (
	"os/exec"
	"testing"
	"time"
)

// TestStartAcceptsTrustDialog is an integration test: it spawns a real claude
// REPL in a brand-new (therefore untrusted) directory and asserts that Start
// gets past the "Quick safety check: Is this a project you trust?" folder-trust
// dialog and reaches the ready REPL. Before the trust-dialog handling in
// readLoop, this would hang until the 45s timeout. Skips when claude is not
// installed (CI without the binary) or in -short mode.
func TestStartAcceptsTrustDialog(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test: spawns a real claude REPL")
	}
	claude, err := ResolveClaude()
	if err != nil {
		t.Skipf("claude binary not found: %v", err)
	}

	// A fresh git repo in a temp dir has never been trusted, so the trust
	// dialog is guaranteed to appear.
	dir := t.TempDir()
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}

	start := time.Now()
	sess, err := Start(Config{
		ClaudePath:   claude,
		Dir:          dir,
		ExtraArgs:    []string{"--dangerously-skip-permissions"},
		AcceptBypass: true,
		ReadyWait:    500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Start in untrusted dir did not reach REPL: %v", err)
	}
	defer sess.Close()

	// Sanity: reaching ready should be fast (a few seconds), nowhere near the
	// 45s startup timeout that the unhandled trust dialog would have caused.
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("Start took %s — suspiciously close to the trust-dialog timeout", elapsed)
	}
}
