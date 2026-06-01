package hook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestListenerReceivesPayload(t *testing.T) {
	l, err := NewListener()
	if err != nil {
		t.Fatalf("NewListener: %v", err)
	}
	t.Cleanup(func() { l.Close() })

	body := `{"session_id":"abc","hook_event_name":"Stop","cwd":"/tmp/x"}`
	if err := Client(l.Path, strings.NewReader(body)); err != nil {
		t.Fatalf("Client: %v", err)
	}

	select {
	case p := <-l.C:
		if p.SessionID != "abc" {
			t.Errorf("SessionID = %q, want %q", p.SessionID, "abc")
		}
		if p.HookEventName != "Stop" {
			t.Errorf("HookEventName = %q, want %q", p.HookEventName, "Stop")
		}
		if p.CWD != "/tmp/x" {
			t.Errorf("CWD = %q, want %q", p.CWD, "/tmp/x")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for payload")
	}
}

func TestListenerEmptyPayloadStillSignals(t *testing.T) {
	l, err := NewListener()
	if err != nil {
		t.Fatalf("NewListener: %v", err)
	}
	t.Cleanup(func() { l.Close() })

	if err := Client(l.Path, strings.NewReader("")); err != nil {
		t.Fatalf("Client: %v", err)
	}

	select {
	case p := <-l.C:
		if p != (Payload{}) {
			t.Errorf("expected zero-value Payload, got %+v", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for empty payload signal")
	}
}

func TestCommandFormat(t *testing.T) {
	l, err := NewListener()
	if err != nil {
		t.Fatalf("NewListener: %v", err)
	}
	t.Cleanup(func() { l.Close() })

	got := l.Command("/usr/local/bin/rambl")
	want := "'/usr/local/bin/rambl' __hook '" + l.Path + "'"
	if got != want {
		t.Errorf("Command = %q, want %q", got, want)
	}
}

func TestClientMissingSocketReturnsNil(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "nope.sock")
	if err := Client(sock, strings.NewReader("x")); err != nil {
		t.Errorf("Client on missing socket returned %v, want nil", err)
	}
}

func TestCloseRemovesSocketDir(t *testing.T) {
	l, err := NewListener()
	if err != nil {
		t.Fatalf("NewListener: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	dir := filepath.Dir(l.Path)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("os.Stat(%q) error = %v, want IsNotExist", dir, err)
	}
}
