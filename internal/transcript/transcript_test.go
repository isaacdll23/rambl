package transcript

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// expectedEncoded computes the encoded final path element the same way Dir does.
func expectedEncoded(workdir string) string {
	abs, _ := filepath.Abs(workdir)
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	return strings.NewReplacer("/", "-", ".", "-").Replace(abs)
}

func TestDirEncoding(t *testing.T) {
	const input = "/some/dir.path"
	got := Dir(input)

	wantSub := filepath.Join(".claude", "projects")
	if !strings.Contains(got, wantSub) {
		t.Fatalf("Dir(%q) = %q, want it to contain %q", input, got, wantSub)
	}

	base := filepath.Base(got)
	if strings.ContainsAny(base, "/.") {
		t.Fatalf("encoded base %q must not contain '/' or '.'", base)
	}

	if want := expectedEncoded(input); base != want {
		t.Fatalf("encoded base = %q, want %q", base, want)
	}
}

func TestExtractText(t *testing.T) {
	tests := []struct {
		name string
		raw  json.RawMessage
		want string
	}{
		{"plain string", json.RawMessage(`"hello"`), "hello"},
		{
			"array of blocks",
			json.RawMessage(`[{"type":"text","text":"a"},{"type":"tool_use"},{"type":"text","text":"b"}]`),
			"ab",
		},
		{"empty/nil", json.RawMessage(nil), ""},
		{"malformed", json.RawMessage(`{not json`), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractText(tt.raw); got != tt.want {
				t.Fatalf("extractText() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestReadNewLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(path, []byte("l1\nl2\nl3\npartial"), 0o644); err != nil {
		t.Fatal(err)
	}

	lines, offset := readNewLines(path, 0)
	wantLines := []string{"l1", "l2", "l3"}
	if !equalStrings(lines, wantLines) {
		t.Fatalf("lines = %v, want %v", lines, wantLines)
	}
	// Offset should point just after "l3\n" — i.e. the byte length of "l1\nl2\nl3\n".
	if wantOffset := int64(len("l1\nl2\nl3\n")); offset != wantOffset {
		t.Fatalf("offset = %d, want %d", offset, wantOffset)
	}

	// No new complete data: same offset, no lines.
	lines2, offset2 := readNewLines(path, offset)
	if len(lines2) != 0 {
		t.Fatalf("expected no lines on re-read, got %v", lines2)
	}
	if offset2 != offset {
		t.Fatalf("offset changed on re-read: %d -> %d", offset, offset2)
	}

	// Append "l4\n" and read from the prior offset. The earlier "partial" was
	// never consumed (it had no trailing newline), so completing the line by
	// appending "l4\n" yields the single line "partiall4".
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("l4\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	lines3, _ := readNewLines(path, offset)
	if !equalStrings(lines3, []string{"partiall4"}) {
		t.Fatalf("lines after append = %v, want [partiall4]", lines3)
	}
}

func TestTailerRunTracksLatest(t *testing.T) {
	tmp := t.TempDir()
	tl := &Tailer{dir: tmp, before: map[string]bool{}}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		tl.Run(ctx)
	}()
	t.Cleanup(wg.Wait)

	// Give Run a moment to take its "before" snapshot of the empty dir, then
	// create the new session file.
	time.Sleep(50 * time.Millisecond)
	content := `{"type":"assistant","sessionId":"s1","message":{"content":[{"type":"text","text":"hello"}]}}` + "\n" +
		`{"type":"system","sessionId":"s1","durationMs":4242}` + "\n"
	if err := os.WriteFile(filepath.Join(tmp, "sess.jsonl"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(3 * time.Second)
	var sessionID, assistant string
	var durationMs int
	for time.Now().Before(deadline) {
		sessionID, assistant, durationMs = tl.Latest()
		if sessionID != "" && assistant != "" && durationMs != 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if sessionID != "s1" {
		t.Errorf("sessionID = %q, want %q", sessionID, "s1")
	}
	if assistant != "hello" {
		t.Errorf("assistant = %q, want %q", assistant, "hello")
	}
	if durationMs != 4242 {
		t.Errorf("durationMs = %d, want %d", durationMs, 4242)
	}

	cancel()
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
