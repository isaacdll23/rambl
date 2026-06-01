# Spawning Claude Code on the subscription — technical findings

## Billing constraint that dictates the approach
After 2026-06-15, billing splits by session classification:
- Interactive Claude Code (terminal, `entrypoint='cli'`) → subscription usage limits.
- Agent SDK and `claude -p` (headless) → separate API-priced credit pool.

To stay on the subscription you must drive the **real interactive `claude` binary**, not the SDK and not `claude -p`.

## Spawn mechanics
- Spawn `claude` with **no `-p` flag** (interactive REPL) under a **PTY** (`github.com/creack/pty`). A PTY is required; the binary behaves as interactive only with a TTY.
- Set PTY size after start (e.g. 120x40); the TUI needs a sane window.
- Strip `ANTHROPIC_API_KEY` and `ANTHROPIC_AUTH_TOKEN` from the child env so auth falls back to the OAuth credentials from `/login` (the subscription token).
- Continuously read from the PTY so the child never blocks on a full output buffer. The PTY output is the TUI render stream — do not parse it for state.

## Submitting a prompt
- The TUI runs in **bracketed-paste mode** (`ESC[?2004h`). Writing the prompt text and the Enter (`\r`) in one write makes the `\r` part of the pasted content, so nothing submits.
- Fix: write the prompt text, wait ~500ms, then write `\r` as a **separate** write.

## Reading the result (do not scrape the TUI)
- Source of truth is the session transcript at:
  `~/.claude/projects/<encoded-cwd>/<session-id>.jsonl`
- `<encoded-cwd>` = absolute cwd with every `/` and `.` replaced by `-`.
  e.g. `/Users/you/Repos/rambl` → `-Users-you-Repos-rambl`.
- To find the new session file: snapshot existing `.jsonl` names before spawn, diff after.
- The file is JSONL, appended as the turn progresses. Tail it by byte offset; only parse complete lines (up to the last `\n`).

## Transcript line types observed
`mode`, `permission-mode`, `file-history-snapshot`, `user`, `assistant`, `attachment`, `system`, `last-prompt`.
- Message lines carry: `type`, `sessionId`, `cwd`, `entrypoint`, `gitBranch`, `userType`, `version`, `timestamp`, `uuid`, `parentUuid`, `message`.
- `assistant` message `content` is an array of blocks: `[{ "type": "text", "text": "..." }]` (also can be a plain string — handle both).
- `system` line is the **turn summary**: carries `durationMs`, `messageCount`, `subtype`. Use it as the authoritative end-of-turn signal rather than inferring from PTY quiescence.

## Billing classification — confirmed
PTY-spawned interactive session recorded `entrypoint='cli'`, `userType='external'` — identical to a normal terminal session (the subscription bucket). Not yet cross-checked against the usage dashboard.

## Environment
- macOS arm64, Go 1.26.3, `claude` 2.1.158 (native installer at `~/.local/bin/claude` → `~/.local/share/claude/versions/<v>`).
- Binary resolution order used: `CLAUDE_PATH` env → `PATH` → `~/.local/bin/claude` → `/opt/homebrew/bin/claude` → `/usr/local/bin/claude`.

## Unattended workers: permissions & folder trust (verified)
- Goal: a worker must edit files / run tools without hanging on an approval prompt.
- `--dangerously-skip-permissions` makes the worker run all tools unattended AND satisfies folder trust (a fresh worktree with `hasTrustDialogAccepted` absent did NOT show the trust dialog).
- BUT it shows a one-time-looking **acknowledgment screen** first: "WARNING: Claude Code running in Bypass Permissions mode … 1. No … 2. Yes, I accept … Enter to confirm · Esc to cancel". The REPL is not usable until this is dismissed.
  - Accept it by sending Down (`\x1b[B`) then Enter (`\r`) to select "Yes, I accept".
  - This acceptance is **not persisted** (no flag in `~/.claude.json`, worktree trust stays absent), so the screen appears on **every** session — handle it dynamically each run.
- Detecting TUI screens requires **stripping ANSI** first: the TUI positions each word with cursor-move escapes, so phrases like "Yes, I accept" are not contiguous in the raw byte stream. Strip escapes, then match tokens ("Bypass" + "accept").
- Folder-trust persistence (for non-bypass modes) lives in `~/.claude.json` → `projects["<abs-path>"].hasTrustDialogAccepted = true`. Pre-seed this if running with `--permission-mode acceptEdits` instead of full bypass.
- Turn-end detection via the `system` line's `durationMs` works for tool-using turns (observed `durationMs=7329` after a Write-tool task completed).

## Known gaps (not yet handled)
- Turn-end detection currently stops at first `assistant` text; should key off the `system` summary line and/or a Claude Code Stop hook.
- Polling (300ms) instead of `fsnotify`.
- No folder-trust handling for first-run directories.
- No git-worktree isolation.
- Exit is a hard kill (`Ctrl-C` x2 then `Process.Kill`), not a graceful `/exit`.
