# rambl

Run a fleet of Claude Code agents in parallel **on your Claude subscription** —
driven by a product-manager agent you talk to.

`rambl` launches one persistent environment: an interactive PM session (a real
Claude Code session, so it uses your subscription — not API billing) that plans
your work, dispatches autonomous coding workers, monitors them, and answers
their questions. Each worker runs in its own git worktree; their results land on
`rambl/<task>` branches for you to review. A separate read-only TUI lets you
watch them.

> Early and experimental. Workers run autonomously with permissions disabled,
> contained to their git worktree — point it at repos where that's acceptable.

## How it works

```
you ⇄ rambl  (one environment process)
        ├─ SQLite state            (~/.rambl/state.db)
        ├─ MCP server (HTTP)       the PM's tools
        └─ PM = interactive Claude (your subscription), wired to those tools:
              plan → dispatch → monitor → resolve blockers → report
                └─ workers: autonomous Claude Code sessions, one git worktree each
```

It deliberately drives the real interactive `claude` CLI (never `claude -p` or
the Agent SDK), which is what keeps usage on your subscription. See
[`ARCHITECTURE.md`](ARCHITECTURE.md) for the design and
[`docs/claude-code-internals.md`](docs/claude-code-internals.md) for the
low-level mechanics.

## Requirements

- Go 1.26+
- The `claude` CLI installed and logged in (`claude` → `/login`) with a Pro/Max plan
- git

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/isaacdll23/rambl/main/install.sh | sh
```

The script detects your platform, downloads the matching binary from the latest
[release](https://github.com/isaacdll23/rambl/releases), verifies its checksum,
and installs to `/usr/local/bin`. Override the target with `BINDIR=…` or pin a
version with `VERSION=v0.1.0`. Linux and macOS, amd64/arm64.

Prefer to do it by hand? Grab an archive from the
[releases page](https://github.com/isaacdll23/rambl/releases):

```sh
tar -xzf rambl_0.1.0_darwin_arm64.tar.gz   # pick your os/arch
sudo mv rambl /usr/local/bin/
rambl version
```

Or build from source (Go 1.26+; clone first — the module path isn't go-installable):

```sh
git clone https://github.com/isaacdll23/rambl && cd rambl
go build -o rambl ./cmd/rambl
```

## Use

```sh
cd /path/to/your/git/repo
echo ".rambl/" >> .gitignore        # workers create worktrees under .rambl/

rambl                                # talk to the PM; it plans and builds
rambl monitor                        # in another pane: watch the workers (read-only)
```

Then review the work:

```sh
git log --oneline --all
git diff main..rambl/<task>
```

### Commands

| command | what it does |
|---|---|
| `rambl` | launch the PM environment in the current repo |
| `rambl pm -repo <path> [-model <m>]` | explicit environment launch |
| `rambl monitor -repo <path> [--once]` | read-only worker dashboard |
| `rambl env-once -repo <path> -brief <text>` | drive the PM through one brief (non-interactive) |

### Tailoring the PM

Drop a `.rambl/pm.md` in your repo; its contents are appended to the PM's
system prompt. The PM also honors your repo's `CLAUDE.md`, settings, and
configured MCP servers, since it's an ordinary Claude Code session.

## Notes & limits

- Workers share one subscription usage pool; keep concurrency modest until
  rate-limit backoff lands.
- Containment is the git worktree by convention — workers can still run
  arbitrary tools. Use accordingly.
- Nothing is pushed for you; your working tree and `main` are never touched.
