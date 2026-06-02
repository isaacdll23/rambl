// Package transcript reads Claude Code's session JSONL files — the source of
// truth for what happened in a session. We never scrape the TUI; we tail the
// transcript that claude writes to ~/.claude/projects/<encoded-cwd>/<id>.jsonl.
package transcript

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Dir returns the transcript directory for a working dir. Claude encodes the
// absolute, symlink-resolved path with every "/" and "." replaced by "-".
func Dir(workdir string) string {
	abs, _ := filepath.Abs(workdir)
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	encoded := strings.NewReplacer("/", "-", ".", "-").Replace(abs)
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "projects", encoded)
}

type line struct {
	Type       string `json:"type"`
	SessionID  string `json:"sessionId"`
	DurationMs *int   `json:"durationMs"`
	Message    struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// Activity is one compact record of a tool the worker invoked in its session.
type Activity struct {
	Kind   string // "tool" or "text" (currently always "tool")
	Tool   string // tool name, e.g. "Edit", "Bash", "Read"
	Detail string // compact arg summary (file path, command, pattern), may be ""
}

// Tailer watches a working dir's transcript directory, detects the new session
// file created after it was constructed, and tracks the latest state.
type Tailer struct {
	dir    string
	before map[string]bool

	mu             sync.Mutex
	sessionID      string
	lastAssistant  string
	lastDurationMs int
	file           string
	recent         []Activity
}

// NewTailer snapshots the existing transcript files so Run can identify the
// new session file this run creates. Construct it BEFORE spawning the session.
func NewTailer(workdir string) *Tailer {
	t := &Tailer{dir: Dir(workdir), before: map[string]bool{}}
	for _, e := range readDirNames(t.dir) {
		t.before[e] = true
	}
	return t
}

// Run tails the new session file until ctx is cancelled.
func (t *Tailer) Run(ctx context.Context) {
	// Wait for the new session file to appear.
	for t.currentFile() == "" {
		if ctx.Err() != nil {
			return
		}
		if f := t.findNew(); f != "" {
			t.setFile(f)
		} else {
			time.Sleep(250 * time.Millisecond)
		}
	}
	// Tail it.
	var offset int64
	for {
		if ctx.Err() != nil {
			return
		}
		lines, newOffset := readNewLines(t.currentFile(), offset)
		offset = newOffset
		for _, raw := range lines {
			var l line
			if json.Unmarshal([]byte(raw), &l) != nil {
				continue
			}
			t.mu.Lock()
			if l.SessionID != "" {
				t.sessionID = l.SessionID
			}
			if l.Type == "assistant" {
				if txt := extractText(l.Message.Content); txt != "" {
					t.lastAssistant = txt
				}
				for _, a := range extractActivities(l.Message.Content) {
					t.recent = append(t.recent, a)
					if len(t.recent) > 12 {
						t.recent = t.recent[len(t.recent)-12:]
					}
				}
			}
			if l.Type == "system" && l.DurationMs != nil {
				t.lastDurationMs = *l.DurationMs
			}
			t.mu.Unlock()
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// Latest returns the most recent observed session id, assistant text, and
// turn-summary durationMs.
func (t *Tailer) Latest() (sessionID, assistant string, durationMs int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sessionID, t.lastAssistant, t.lastDurationMs
}

// Recent returns a copy of the rolling tool-activity ring, oldest first.
func (t *Tailer) Recent() []Activity {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.recent) == 0 {
		return nil
	}
	out := make([]Activity, len(t.recent))
	copy(out, t.recent)
	return out
}

func (t *Tailer) currentFile() string { t.mu.Lock(); defer t.mu.Unlock(); return t.file }
func (t *Tailer) setFile(f string)    { t.mu.Lock(); t.file = f; t.mu.Unlock() }

func (t *Tailer) findNew() string {
	for _, name := range readDirNames(t.dir) {
		if strings.HasSuffix(name, ".jsonl") && !t.before[name] {
			return filepath.Join(t.dir, name)
		}
	}
	return ""
}

func readDirNames(dir string) []string {
	entries, _ := os.ReadDir(dir)
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func readNewLines(path string, offset int64) ([]string, int64) {
	f, err := os.Open(path)
	if err != nil {
		return nil, offset
	}
	defer f.Close()
	fi, _ := f.Stat()
	if fi.Size() <= offset {
		return nil, offset
	}
	if _, err := f.Seek(offset, 0); err != nil {
		return nil, offset
	}
	data := make([]byte, fi.Size()-offset)
	n, _ := f.Read(data)
	text := string(data[:n])
	lastNL := strings.LastIndexByte(text, '\n')
	if lastNL < 0 {
		return nil, offset
	}
	var lines []string
	for _, l := range strings.Split(text[:lastNL], "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	return lines, offset + int64(lastNL) + 1
}

// extractText handles content as either a plain string or an array of blocks.
func extractText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) == nil {
		var sb strings.Builder
		for _, b := range blocks {
			if b.Type == "text" {
				sb.WriteString(b.Text)
			}
		}
		return sb.String()
	}
	return ""
}

// extractActivities pulls tool_use blocks out of an assistant content payload.
// Plain-string content (no blocks) yields no activities.
func extractActivities(raw json.RawMessage) []Activity {
	if len(raw) == 0 {
		return nil
	}
	var blocks []struct {
		Type  string          `json:"type"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
		Text  string          `json:"text"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return nil
	}
	var out []Activity
	for _, b := range blocks {
		if b.Type == "tool_use" {
			out = append(out, Activity{
				Kind:   "tool",
				Tool:   b.Name,
				Detail: summarizeToolInput(b.Name, b.Input),
			})
		}
	}
	return out
}

// summarizeToolInput renders a tool's input as a compact one-line detail.
func summarizeToolInput(name string, input json.RawMessage) string {
	if len(input) == 0 {
		return ""
	}
	var field string
	switch name {
	case "Bash":
		field = "command"
	case "Edit", "Write", "Read", "NotebookEdit":
		field = "file_path"
	case "Grep", "Glob":
		field = "pattern"
	case "Task":
		field = "description"
	case "WebFetch":
		field = "url"
	case "WebSearch":
		field = "query"
	default:
		return ""
	}
	var m map[string]any
	if json.Unmarshal(input, &m) != nil {
		return ""
	}
	v, _ := m[field].(string)
	return compactDetail(v)
}

// compactDetail collapses whitespace to single spaces, trims, and truncates to
// at most 100 runes (appending "…" when truncated).
func compactDetail(s string) string {
	s = strings.TrimSpace(strings.Join(strings.Fields(s), " "))
	if s == "" {
		return ""
	}
	r := []rune(s)
	if len(r) > 100 {
		return string(r[:100]) + "…"
	}
	return s
}
