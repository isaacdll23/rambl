// Package hook delivers push-based turn-completion signals from a worker's
// Claude Code session back to the orchestrator.
//
// We register a Stop hook (via a generated --settings file) whose command is
// this same binary invoked as `__hook <socket>`. When Claude finishes a turn
// it runs the hook; the hook dials a per-worker unix socket and forwards the
// hook payload. The worker's Listener turns each connection into an event on C.
// Unix socket keeps the dependency footprint at zero beyond the stdlib.
package hook

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"
)

// Payload is the subset of the Stop hook's stdin JSON we care about.
type Payload struct {
	SessionID     string `json:"session_id"`
	HookEventName string `json:"hook_event_name"`
	TranscriptPath string `json:"transcript_path"`
	CWD           string `json:"cwd"`
}

// Listener owns a unix socket and emits a Payload for every hook invocation.
type Listener struct {
	Path string
	C    chan Payload
	dir  string
	ln   net.Listener
}

// NewListener creates the socket in a fresh temp dir and starts accepting.
func NewListener() (*Listener, error) {
	dir, err := os.MkdirTemp("", "rambl-hook-")
	if err != nil {
		return nil, err
	}
	sock := filepath.Join(dir, "hook.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	l := &Listener{Path: sock, C: make(chan Payload, 8), dir: dir, ln: ln}
	go l.accept()
	return l, nil
}

func (l *Listener) accept() {
	for {
		conn, err := l.ln.Accept()
		if err != nil {
			return // listener closed
		}
		go func(c net.Conn) {
			defer c.Close()
			_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
			data, _ := io.ReadAll(c)
			var p Payload
			_ = json.Unmarshal(data, &p) // empty payload still signals "a turn ended"
			select {
			case l.C <- p:
			default: // never block the accept loop
			}
		}(conn)
	}
}

// Command returns the hook command string to embed in settings, invoking
// selfExe as the hook client. Paths are single-quoted for the shell.
func (l *Listener) Command(selfExe string) string {
	return fmt.Sprintf("'%s' __hook '%s'", selfExe, l.Path)
}

// Close stops the listener and removes the socket dir.
func (l *Listener) Close() error {
	err := l.ln.Close()
	os.RemoveAll(l.dir)
	return err
}

// Client (run by `__hook <socket>`) forwards the hook's stdin payload to the
// worker's socket. It must be fast and must never fail in a way that blocks
// claude — a dead socket is ignored.
func Client(sockPath string, stdin io.Reader) error {
	data, _ := io.ReadAll(stdin)
	conn, err := net.DialTimeout("unix", sockPath, 2*time.Second)
	if err != nil {
		return nil // worker gone; do not disrupt claude
	}
	defer conn.Close()
	_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	_, _ = conn.Write(data)
	return nil
}
