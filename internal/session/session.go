// Package session drives a real *interactive* Claude Code process under a PTY.
//
// This is the subscription-safe path: it spawns the genuine `claude` binary
// (never `-p`, never the SDK), strips API-key env so it uses subscription
// OAuth, and observes nothing from the TUI itself — callers read state from
// the transcript JSONL instead. The only TUI interaction is keystroke input:
// submitting prompts and dismissing the bypass-permissions acknowledgment.
package session

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/creack/pty"
)

// ansiRE strips terminal escape sequences. The TUI positions each word with
// cursor-move codes, so phrases ("Yes, I accept") are not contiguous in the
// raw byte stream — we must strip escapes before matching text.
var ansiRE = regexp.MustCompile(`\x1b\][^\x07]*\x07|\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b[()][0-9A-Za-z]|\x1b.`)

func stripANSI(b []byte) []byte { return ansiRE.ReplaceAll(b, nil) }

// Config configures a session spawn.
type Config struct {
	ClaudePath   string        // resolved if empty
	Dir          string        // working directory (a trusted folder or a worktree)
	ExtraArgs    []string      // e.g. {"--dangerously-skip-permissions"}
	SettingsPath string        // passed as --settings (e.g. our generated Stop-hook settings)
	AcceptBypass bool          // auto-dismiss the bypass-permissions acknowledgment screen
	DropEnv      []string      // extra env keys to drop (ANTHROPIC_API_KEY is always dropped)
	LogWriter    io.Writer     // optional: raw PTY stream is teed here for debugging
	ReadyWait    time.Duration // how long to wait for the REPL after startup/ack (default 4s/3s)
}

// Session is a live interactive Claude Code process.
type Session struct {
	cmd  *exec.Cmd
	ptmx *os.File
}

// ResolveClaude finds the claude binary: CLAUDE_PATH → PATH → native installer → brew.
func ResolveClaude() (string, error) {
	if p := os.Getenv("CLAUDE_PATH"); p != "" {
		return p, nil
	}
	if p, err := exec.LookPath("claude"); err == nil {
		return p, nil
	}
	home, _ := os.UserHomeDir()
	for _, c := range []string{
		filepath.Join(home, ".local", "bin", "claude"),
		"/opt/homebrew/bin/claude",
		"/usr/local/bin/claude",
	} {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	return "", fmt.Errorf("could not locate the `claude` binary; set CLAUDE_PATH")
}

// Start spawns the interactive session and blocks until the REPL is ready to
// accept a prompt (dismissing the bypass acknowledgment first if configured).
func Start(cfg Config) (*Session, error) {
	path := cfg.ClaudePath
	if path == "" {
		var err error
		if path, err = ResolveClaude(); err != nil {
			return nil, err
		}
	}

	args := append([]string{}, cfg.ExtraArgs...)
	if cfg.SettingsPath != "" {
		args = append(args, "--settings", cfg.SettingsPath)
	}

	cmd := exec.Command(path, args...) // no -p: interactive REPL
	cmd.Dir = cfg.Dir
	cmd.Env = buildEnv(cfg.DropEnv)

	ptmx, err := pty.Start(cmd)
	if err != nil {
		return nil, fmt.Errorf("pty start: %w", err)
	}
	_ = pty.Setsize(ptmx, &pty.Winsize{Rows: 40, Cols: 120})
	s := &Session{cmd: cmd, ptmx: ptmx}

	// One long-lived reader drains the PTY (so claude never blocks on a full
	// output buffer), accepts the bypass acknowledgment screen IF it appears,
	// and signals when the REPL input is live (the "shift+tab" mode hint).
	ready := make(chan struct{})
	go s.readLoop(ready, cfg.AcceptBypass, cfg.LogWriter)

	settle := cfg.ReadyWait
	if settle == 0 {
		settle = time.Second
	}
	select {
	case <-ready:
		time.Sleep(settle) // let the input box finish rendering
	case <-time.After(45 * time.Second):
		_ = s.Close()
		return nil, fmt.Errorf("REPL did not become ready (no input prompt within 45s)")
	}
	return s, nil
}

// readyMarker is the mode-cycle hint shown on the status line whenever the REPL
// input is ready to accept a prompt (both "auto mode" and "bypass permissions").
var readyMarker = []byte("shift+tab")

func (s *Session) readLoop(ready chan struct{}, acceptBypass bool, logw io.Writer) {
	var ackOnce, trustOnce, readyOnce sync.Once
	buf := make([]byte, 4096)
	var acc []byte
	for {
		n, err := s.ptmx.Read(buf)
		if n > 0 {
			if logw != nil {
				_, _ = logw.Write(buf[:n])
			}
			acc = append(acc, stripANSI(buf[:n])...)
			if len(acc) > 32768 {
				acc = acc[len(acc)-32768:]
			}
			// The folder-trust dialog ("Quick safety check: Is this a project you
			// trust?") is shown for any directory not previously trusted — i.e.
			// every fresh worktree — and is NOT suppressed by
			// --dangerously-skip-permissions (it runs before settings load; see
			// CVE-2026-33068). Its default highlighted option is "Yes, I trust
			// this folder", so a bare Enter accepts it. We always answer it: this
			// loop only drives automated, non-interactive sessions, and the
			// worktrees are derived by git from the user's already-trusted repo.
			// stripANSI collapses inter-word spaces, so match the contiguous
			// tokens "safety" + "trust" (both survive stripping).
			if low := bytes.ToLower(acc); bytes.Contains(low, []byte("safety")) && bytes.Contains(low, []byte("trust")) {
				trustOnce.Do(func() {
					time.Sleep(300 * time.Millisecond)
					_, _ = s.ptmx.Write([]byte("\r")) // accept "Yes, I trust this folder"
				})
				acc = acc[:0] // clear so later checks see post-trust output
			}
			// The bypass acknowledgment modal ("…Bypass Permissions mode… Yes,
			// I accept") is shown only the first time per machine; accept it if
			// present. Its presence is distinguished from the lowercase status
			// line by the capitalized "Bypass" + "accept".
			if acceptBypass && bytes.Contains(acc, []byte("Bypass")) && bytes.Contains(acc, []byte("accept")) {
				ackOnce.Do(func() {
					time.Sleep(300 * time.Millisecond)
					_, _ = s.ptmx.Write([]byte("\x1b[B")) // Down → "Yes, I accept"
					time.Sleep(200 * time.Millisecond)
					_, _ = s.ptmx.Write([]byte("\r")) // confirm
				})
				acc = acc[:0] // clear so the next check is against post-accept output
			}
			if bytes.Contains(acc, readyMarker) {
				readyOnce.Do(func() { close(ready) })
			}
		}
		if err != nil {
			return
		}
	}
}

// Send submits a prompt. The TUI runs in bracketed-paste mode, so the text and
// the Enter must be separate writes or the newline is swallowed as paste.
func (s *Session) Send(text string) error {
	if _, err := s.ptmx.Write([]byte(text)); err != nil {
		return fmt.Errorf("write prompt: %w", err)
	}
	time.Sleep(500 * time.Millisecond)
	if _, err := s.ptmx.Write([]byte("\r")); err != nil {
		return fmt.Errorf("submit prompt: %w", err)
	}
	return nil
}

// Close exits the session (Ctrl-C twice, then hard kill as a fallback).
func (s *Session) Close() error {
	if s.ptmx != nil {
		_, _ = s.ptmx.Write([]byte("\x03\x03"))
		time.Sleep(400 * time.Millisecond)
	}
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
		_, _ = s.cmd.Process.Wait()
	}
	if s.ptmx != nil {
		return s.ptmx.Close()
	}
	return nil
}

// Env returns the current environment minus credential keys (forcing
// subscription OAuth) plus a sane TERM — for callers spawning claude outside
// this package (e.g. an interactive planner session).
func Env(extraDrop ...string) []string { return buildEnv(extraDrop) }

// buildEnv copies the environment minus credential keys (forcing subscription
// OAuth) plus a sane TERM.
func buildEnv(extraDrop []string) []string {
	drop := map[string]bool{"ANTHROPIC_API_KEY": true, "ANTHROPIC_AUTH_TOKEN": true}
	for _, k := range extraDrop {
		drop[k] = true
	}
	var out []string
	for _, kv := range os.Environ() {
		k := kv
		if i := bytes.IndexByte([]byte(kv), '='); i >= 0 {
			k = kv[:i]
		}
		if !drop[k] {
			out = append(out, kv)
		}
	}
	return append(out, "TERM=xterm-256color")
}
