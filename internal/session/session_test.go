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

// TestAccessorsZeroValueSafe asserts the diagnostic accessors never panic on a
// zero-value session (one that never made it past construction).
func TestAccessorsZeroValueSafe(t *testing.T) {
	var s Session
	if got := s.Tail(); got != "" {
		t.Fatalf("Tail() on zero value = %q, want empty", got)
	}
	if err := s.ExitErr(); err != nil {
		t.Fatalf("ExitErr() on zero value = %v, want nil", err)
	}
	if ch := s.Exited(); ch != nil {
		t.Fatalf("Exited() on zero value = %v, want nil", ch)
	}
}

// TestAccessorsPartialSession asserts the accessors are safe on a
// partially-constructed session (exited channel present but no process).
func TestAccessorsPartialSession(t *testing.T) {
	s := &Session{exited: make(chan struct{})}
	if got := s.Tail(); got != "" {
		t.Fatalf("Tail() = %q, want empty", got)
	}
	if err := s.ExitErr(); err != nil {
		t.Fatalf("ExitErr() = %v, want nil", err)
	}
	// Close on a session with no cmd/ptmx must not panic.
	go func() { s.setExit(nil) }()
	if err := s.Close(); err != nil {
		t.Fatalf("Close() on partial session = %v, want nil", err)
	}
}

// TestTailTruncates verifies Tail() returns only the last 2000 bytes of a
// directly-populated tailBuf (same-package test reaches the unexported field).
func TestTailTruncates(t *testing.T) {
	s := &Session{}
	s.tailBuf = make([]byte, 5000)
	for i := range s.tailBuf {
		s.tailBuf[i] = 'x'
	}
	// Mark the final byte so we can confirm we got the tail, not the head.
	s.tailBuf[len(s.tailBuf)-1] = 'Z'

	got := s.Tail()
	if len(got) != 2000 {
		t.Fatalf("Tail() len = %d, want 2000", len(got))
	}
	if got[len(got)-1] != 'Z' {
		t.Fatalf("Tail() did not return the most recent bytes")
	}
}
