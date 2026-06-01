# rambl — architecture

A persistent CLI environment that runs Claude Code agents in parallel **on your
Claude subscription**. You converse with a **PM agent** that plans, dispatches,
monitors, and resolves a fleet of autonomous coding workers. A separate
read-only TUI lets you watch them.

Module: `rambl` (Go 1.26). Low-level mechanics of driving Claude Code are
documented in `docs/claude-code-internals.md`.

Dependencies: `creack/pty` (PTY), `modernc.org/sqlite` (pure-Go SQLite),
`mark3labs/mcp-go` (MCP server, HTTP transport), `charmbracelet/bubbletea` +
`lipgloss` (monitor TUI).

## The flow

```
you ⇄ rambl (the environment, one process)
        ├─ SQLite store (~/.rambl/state.db): projects · tasks · deps · status
        ├─ MCP server (HTTP, ephemeral port): the PM's tools
        └─ PM session = interactive claude (subscription, real TUI), wired to
             the MCP server + a PM-as-driver prompt. It:
               create_task → plan into the store
               dispatch    → runner starts a worker
               worker_status / worker_send → monitor + answer blocked workers
                 └─ runner → autonomous workers (bypass, git worktree each)
                      DONE / BLOCKED protocol; dep branches merged in; commit on done

rambl monitor  → separate read-only TUI over the same store (second surface)
```

## Packages

Engine (subscription-safe Claude Code driving):
- `internal/session` — drive one interactive `claude` under a PTY (no `-p`, no SDK; strips API-key env). Bypass-ack handling, REPL-readiness, prompt submit.
- `internal/transcript` — tail the session JSONL (source of truth for replies / durationMs).
- `internal/hook` — push turn-completion via a Stop hook over a unix socket.
- `internal/worker` — one worktree-isolated autonomous worker: merge dep branches in, run, multi-turn, commit on success.

PM-driven environment (current architecture):
- `internal/store` — SQLite state (projects, tasks, deps, status/question/result). WAL for concurrent monitor reads. UUID ids + timestamps (sync-friendly).
- `internal/runner` — worker manager: dispatch (background), classify the DONE/BLOCKED outcome protocol, keep blocked workers alive, `Send` follow-ups, commit + retire on done.
- `internal/mcpserver` — the PM's tools over MCP/HTTP: `create_task`, `list_tasks`, `dispatch`, `worker_status`, `worker_send`.
- `internal/environment` — boots the MCP server in-process and launches the interactive PM session wired to it (`--mcp-config` + PM prompt + pre-approved tools). Tailor via per-project `.rambl/pm.md`.
- `internal/monitor` — read-only dashboard (bubbletea) polling the store; `--once` prints a plain snapshot.

## Commands

```
rambl                      launch the PM environment in the current repo
rambl pm        -repo …    explicit environment launch (+ -model)
rambl monitor   -repo …    read-only dashboard  (--once for a snapshot)
rambl env-once  -brief …   drive the PM through one brief (verification)
```

## Verified working
- Engine: autonomous multi-turn worker in an isolated worktree; push completion; dep data-flow (downstream worktree merges upstream branch).
- Store + MCP server: tools over real HTTP transport (mcp-go client test); `serve` answers MCP initialize.
- Full environment loop (zero human input): `env-once` → PM (Claude) called `create_task` + `dispatch` over MCP → runner ran a real worker → file committed to `rambl/<slug>`; store reflected `done`.
- Monitor: snapshot renders all statuses + blocking questions.

## Known gaps / next
- **Rate-limit backoff** — all workers share one subscription pool; a usage-limit hit currently surfaces as failure, not graceful pause/resume. The key gap for scale.
- Branch integration/merge back to main; PM session resume (`--resume`); richer intra-turn worker liveness; worker-side `done()`/`blocked()` MCP tools (vs sentinel markers).
- Containment of bypass workers is the worktree by convention only (they can still run bash/network).
