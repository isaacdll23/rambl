package transcript

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
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

func TestTailerRunTracksRecentActivities(t *testing.T) {
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

	time.Sleep(50 * time.Millisecond)
	content := `{"type":"assistant","sessionId":"s1","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"go test ./..."}}]}}` + "\n" +
		`{"type":"assistant","sessionId":"s1","message":{"content":[{"type":"text","text":"working"},{"type":"tool_use","name":"Edit","input":{"file_path":"/tmp/x.go"}}]}}` + "\n" +
		`{"type":"assistant","sessionId":"s1","message":{"content":[{"type":"tool_use","name":"Grep","input":{"pattern":"func main"}}]}}` + "\n"
	if err := os.WriteFile(filepath.Join(tmp, "sess.jsonl"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(3 * time.Second)
	var recent []Activity
	for time.Now().Before(deadline) {
		recent = tl.Recent()
		if len(recent) == 3 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	want := []Activity{
		{Kind: "tool", Tool: "Bash", Detail: "go test ./..."},
		{Kind: "tool", Tool: "Edit", Detail: "/tmp/x.go"},
		{Kind: "tool", Tool: "Grep", Detail: "func main"},
	}
	if len(recent) != len(want) {
		t.Fatalf("Recent() len = %d, want %d (%+v)", len(recent), len(want), recent)
	}
	for i := range want {
		if recent[i] != want[i] {
			t.Errorf("Recent()[%d] = %+v, want %+v", i, recent[i], want[i])
		}
	}

	cancel()
}

func TestTailerRecentRingCaps(t *testing.T) {
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

	time.Sleep(50 * time.Millisecond)
	// Build content with distinct, recoverable file paths per line.
	var content strings.Builder
	for i := 0; i < 15; i++ {
		content.WriteString(`{"type":"assistant","sessionId":"s1","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"/f/` + strconv.Itoa(i) + `"}}]}}` + "\n")
	}
	if err := os.WriteFile(filepath.Join(tmp, "sess.jsonl"), []byte(content.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(3 * time.Second)
	var recent []Activity
	for time.Now().Before(deadline) {
		recent = tl.Recent()
		if len(recent) == 12 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if len(recent) != 12 {
		t.Fatalf("Recent() len = %d, want 12", len(recent))
	}
	// Should keep the LAST 12: indices 3..14.
	for i, a := range recent {
		want := "/f/" + strconv.Itoa(i+3)
		if a.Detail != want {
			t.Errorf("Recent()[%d].Detail = %q, want %q", i, a.Detail, want)
		}
	}

	cancel()
}

func TestSummarizeToolInput(t *testing.T) {
	tests := []struct {
		name  string
		tool  string
		input string
		want  string
	}{
		{"bash command", "Bash", `{"command":"go build ./..."}`, "go build ./..."},
		{"edit file_path", "Edit", `{"file_path":"/a/b.go"}`, "/a/b.go"},
		{"write file_path", "Write", `{"file_path":"/a/c.go"}`, "/a/c.go"},
		{"read file_path", "Read", `{"file_path":"/a/d.go"}`, "/a/d.go"},
		{"notebookedit file_path", "NotebookEdit", `{"file_path":"/a/e.ipynb"}`, "/a/e.ipynb"},
		{"grep pattern", "Grep", `{"pattern":"func main"}`, "func main"},
		{"glob pattern", "Glob", `{"pattern":"**/*.go"}`, "**/*.go"},
		{"task description", "Task", `{"description":"do the thing"}`, "do the thing"},
		{"webfetch url", "WebFetch", `{"url":"https://example.com"}`, "https://example.com"},
		{"websearch query", "WebSearch", `{"query":"golang json"}`, "golang json"},
		{"unmapped default", "MysteryTool", `{"command":"whatever"}`, ""},
		{"missing field", "Bash", `{"foo":"bar"}`, ""},
		{"empty field", "Bash", `{"command":""}`, ""},
		{"empty input", "Bash", ``, ""},
		{"collapses whitespace", "Bash", `{"command":"a\n\tb   c"}`, "a b c"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := summarizeToolInput(tt.tool, json.RawMessage(tt.input)); got != tt.want {
				t.Fatalf("summarizeToolInput(%q, %q) = %q, want %q", tt.tool, tt.input, got, tt.want)
			}
		})
	}

	// Truncation to 100 runes with ellipsis.
	long := strings.Repeat("x", 250)
	input := json.RawMessage(`{"command":"` + long + `"}`)
	got := summarizeToolInput("Bash", input)
	gotRunes := []rune(got)
	if len(gotRunes) != 101 { // 100 runes + the "…"
		t.Fatalf("truncated length = %d runes, want 101", len(gotRunes))
	}
	if gotRunes[100] != '…' {
		t.Fatalf("truncated detail must end with '…', got %q", string(gotRunes[100]))
	}
	if string(gotRunes[:100]) != strings.Repeat("x", 100) {
		t.Fatalf("truncated prefix = %q, want 100 x's", string(gotRunes[:100]))
	}
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
