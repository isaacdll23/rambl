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

Prebuilt binaries for Linux and macOS (amd64/arm64) are published on every
tagged release. Download the archive for your platform from the
[releases page](https://github.com/isaacdll23/rambl/releases), extract it, and
put `rambl` on your `PATH`:

```sh
tar -xzf rambl_*_$(uname -s)_$(uname -m).tar.gz
sudo mv rambl /usr/local/bin/
rambl version
```

Or build from source:

```sh
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
| `rambl version` | print version, commit, and build date |

### Tailoring the PM

Drop a `.rambl/pm.md` in your repo; its contents are appended to the PM's
system prompt. The PM also honors your repo's `CLAUDE.md`, settings, and
configured MCP servers, since it's an ordinary Claude Code session.

## Releasing

Releases are cut by [Woodpecker CI](.woodpecker.yml): every push runs
`go vet` / `go test -race` / `go build`, and pushing a `v*` tag triggers
[GoReleaser](.goreleaser.yaml), which cross-compiles the linux/darwin ×
amd64/arm64 binaries, archives them with checksums, and publishes a GitHub
release. The version, commit, and build date are stamped into the binary via
`-ldflags` and surfaced by `rambl version`.

```sh
git tag v0.1.0
git push origin v0.1.0   # Woodpecker builds and publishes the release
```

CI needs a `github_token` secret (a GitHub token with `repo` scope) configured on
the Woodpecker repo.

## Notes & limits

- Workers share one subscription usage pool; keep concurrency modest until
  rate-limit backoff lands.
- Containment is the git worktree by convention — workers can still run
  arbitrary tools. Use accordingly.
- Nothing is pushed for you; your working tree and `main` are never touched.
