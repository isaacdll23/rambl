package environment

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"rambl/internal/store"
)

func TestSystemPrompt(t *testing.T) {
	const header = "Project-specific guidance"

	// No .rambl/pm.md -> base prompt only, no project-specific header.
	repo := t.TempDir()
	got := systemPrompt(repo)
	if got != pmSystemPrompt {
		t.Fatalf("systemPrompt with no pm.md should equal the base prompt verbatim")
	}
	if strings.Contains(got, header) {
		t.Fatalf("systemPrompt with no pm.md should not contain %q header", header)
	}

	// With <repo>/.rambl/pm.md -> base prompt + the file contents under the header.
	repo2 := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo2, ".rambl"), 0o755); err != nil {
		t.Fatalf("mkdir .rambl: %v", err)
	}
	const extra = "Always prefer Dapper over EF Core in this repo."
	if err := os.WriteFile(filepath.Join(repo2, ".rambl", "pm.md"), []byte(extra), 0o644); err != nil {
		t.Fatalf("write pm.md: %v", err)
	}
	got2 := systemPrompt(repo2)
	if !strings.Contains(got2, pmSystemPrompt) {
		t.Fatalf("systemPrompt with pm.md should still contain the base prompt")
	}
	if !strings.Contains(got2, header) {
		t.Fatalf("systemPrompt with pm.md should contain %q header", header)
	}
	if !strings.Contains(got2, extra) {
		t.Fatalf("systemPrompt with pm.md should contain the file contents")
	}
}

func TestSettled(t *testing.T) {
	tests := []struct {
		name  string
		tasks []*store.Task
		want  bool
	}{
		{"empty is settled", nil, true},
		{"single todo not settled", []*store.Task{{Status: store.Todo}}, false},
		{"single running not settled", []*store.Task{{Status: store.Running}}, false},
		{"all terminal settled", []*store.Task{
			{Status: store.Done}, {Status: store.Failed},
			{Status: store.Blocked}, {Status: store.NeedsInput},
		}, true},
		{"one todo among terminal not settled", []*store.Task{
			{Status: store.Done}, {Status: store.Todo}, {Status: store.Failed},
		}, false},
		{"one running among terminal not settled", []*store.Task{
			{Status: store.Done}, {Status: store.Running},
		}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := settled(tc.tasks); got != tc.want {
				t.Fatalf("settled(%v) = %v, want %v", tc.tasks, got, tc.want)
			}
		})
	}
}

func TestWriteMCPConfig(t *testing.T) {
	const url = "http://127.0.0.1:54321/mcp"
	path, err := writeMCPConfig(url)
	if err != nil {
		t.Fatalf("writeMCPConfig: %v", err)
	}
	defer os.Remove(path)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("config is not valid JSON: %v\n%s", err, data)
	}
	servers, ok := cfg["mcpServers"].(map[string]any)
	if !ok {
		t.Fatalf("mcpServers missing/wrong type: %T", cfg["mcpServers"])
	}
	rambl, ok := servers["rambl"].(map[string]any)
	if !ok {
		t.Fatalf("mcpServers.rambl missing/wrong type: %T", servers["rambl"])
	}
	if rambl["url"] != url {
		t.Fatalf("url = %v, want %q", rambl["url"], url)
	}
	// JSON numbers decode to float64.
	timeout, ok := rambl["timeout"].(float64)
	if !ok {
		t.Fatalf("timeout missing/non-numeric: %T (%v)", rambl["timeout"], rambl["timeout"])
	}
	if timeout != 120000 {
		t.Fatalf("timeout = %v, want 120000", timeout)
	}
}

func TestFreePort(t *testing.T) {
	p1, err := freePort()
	if err != nil {
		t.Fatalf("freePort: %v", err)
	}
	if p1 <= 0 {
		t.Fatalf("freePort returned non-positive port %d", p1)
	}
	// Calling it again succeeds (the listener is closed before returning).
	p2, err := freePort()
	if err != nil {
		t.Fatalf("freePort second call: %v", err)
	}
	if p2 <= 0 {
		t.Fatalf("freePort second call returned non-positive port %d", p2)
	}
}

func TestAllowedTools(t *testing.T) {
	wantPresent := []string{
		"mcp__rambl__create_task",
		"mcp__rambl__list_tasks",
		"mcp__rambl__dispatch",
		"mcp__rambl__worker_status",
		"mcp__rambl__worker_send",
		"mcp__rambl__delete_task",
		"mcp__rambl__read_diff",
		"mcp__rambl__verify_task",
		"mcp__rambl__revise_task",
		"mcp__rambl__open_pr",
		"Read", "Glob", "Grep",
	}
	for _, name := range wantPresent {
		if !strings.Contains(allowedTools, name) {
			t.Errorf("allowedTools missing %q: %s", name, allowedTools)
		}
	}
	for _, name := range []string{"Bash", "Edit", "Write"} {
		if strings.Contains(allowedTools, name) {
			t.Errorf("allowedTools should NOT contain %q: %s", name, allowedTools)
		}
	}
}
