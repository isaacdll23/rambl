# rambl

Run a fleet of Claude Code agents in parallel **on your Claude subscription** —
driven by a product-manager agent you talk to.

`rambl` launches one persistent environment: an interactive PM session (a real
Claude Code session, so it uses your subscription — not API billing) that plans
your work, dispatches autonomous coding workers, monitors them, answers their
questions, reviews their diffs, and — when you want — opens GitHub PRs. Each
worker runs in its own git worktree; their results land on `rambl/<task>`
branches for you to review. Related tasks can be grouped into a **feature** that
lands as a single PR (see [Features](#features)). A separate read-only TUI lets
you watch them.

> Early and experimental. Workers run autonomously with permissions disabled,
> contained to their git worktree — point it at repos where that's acceptable.

## How it works

```
you ⇄ rambl  (one environment process)
        ├─ SQLite state            (~/.rambl/state.db)
        ├─ MCP server (HTTP)       the PM's tools
        └─ PM = interactive Claude (your subscription), wired to those tools:
              plan → dispatch → monitor → resolve blockers → review → open PR
                └─ workers: autonomous Claude Code sessions, one git worktree each
```

It deliberately drives the real interactive `claude` CLI (never `claude -p` or
the Agent SDK), which is what keeps usage on your subscription — see
[ADR 0001](docs/adr/0001-drive-interactive-cli-for-subscription-billing.md).
Workers are isolated in out-of-repo git worktrees
([ADR 0002](docs/adr/0002-worktree-isolated-workers.md)).

## Requirements

- The `claude` CLI installed and logged in (`claude` → `/login`) with a Pro/Max plan
- git

(Building from source additionally needs Go 1.26+.)

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/isaacdll23/rambl/main/install.sh | sh
```

The script detects your platform, downloads the matching prebuilt binary from the
latest [release](https://github.com/isaacdll23/rambl/releases), verifies its
checksum, and installs to `/usr/local/bin`. Override the target with `BINDIR=…`
or pin a version with `VERSION=v0.1.0`. Linux and macOS, amd64/arm64.

<details>
<summary>Prefer to install by hand?</summary>

Prebuilt binaries are published on every tagged release. Grab the archive for
your platform from the [releases page](https://github.com/isaacdll23/rambl/releases),
extract it, and put `rambl` on your `PATH`:

```sh
tar -xzf rambl_<version>_<os>_<arch>.tar.gz   # e.g. rambl_0.1.0_darwin_arm64.tar.gz
sudo mv rambl /usr/local/bin/
rambl version
```
</details>

<details>
<summary>Build from source</summary>

Go 1.26+; clone first — the module path isn't go-installable:

```sh
git clone https://github.com/isaacdll23/rambl && cd rambl
go build -o rambl ./cmd/rambl
```
</details>

## Use

```sh
rambl                # in a git repo: launches here. Elsewhere: pick a repo from the TUI
rambl pick           # always choose a repo from the TUI, then launch
rambl monitor        # in another pane: watch the workers (read-only)
```

Worktrees and state both live under `~/.rambl/`, so the repo stays pristine —
no `.gitignore` changes needed.

Then review the work:

```sh
git log --oneline --all
git diff main..rambl/<task>
```

The PM reviews and verifies each worker's diff before surfacing it as done, and
can open a GitHub PR for you on request. Nothing is pushed until you ask for a
PR.

### Features

A **feature** is a named group of related tasks that land together as a single
pull request, instead of one PR per task. The PM creates the feature, attaches
tasks to it with the right dependencies, then runs it end-to-end: ready tasks
dispatch in parallel, each completed task is squash-merged into a
`rambl/feat/<slug>` branch in dependency order, an integration gate keeps that
branch building/passing, and once everything is merged and green the feat→main
PR is opened automatically. Use a feature when several interdependent tasks
should ship as one reviewable unit; use a standalone task for isolated one-offs.

### Commands

| command | what it does |
|---|---|
| `rambl` | launch the PM in the current repo, or pick one from a TUI if you're not in a repo |
| `rambl pick` | choose a repo from a TUI, then launch the PM environment |
| `rambl pm -repo <path> [-db <p>] [-base <ref>] [-model <m>]` | explicit environment launch |
| `rambl monitor -repo <path> [-db <p>] [--once]` | read-only worker dashboard (`--once` prints a snapshot and exits) |
| `rambl env-once -repo <path> -brief <text> [-base <ref>] [-timeout <dur>]` | drive the PM through one brief (non-interactive) |
| `rambl doctor` | preflight check: `claude` and `git` on `PATH`, `~/.rambl` writable |
| `rambl config [list\|get\|set\|path] [<key>] [<val>]` | view or change settings in `~/.rambl/config.json` |
| `rambl version` | print version, commit, and build date |

#### Settings

`rambl config` reads and writes `~/.rambl/config.json`. Current keys:

| key | default | what it does |
|---|---|---|
| `turn-timeout` | `15m` | per-turn wall-clock cap for a worker's Claude session (also settable via `RAMBL_TURN_TIMEOUT`) |

### Tailoring the PM

Drop a `.rambl/pm.md` in your repo; its contents are appended to the PM's
system prompt. The PM also honors your repo's `CLAUDE.md`, settings, and
configured MCP servers, since it's an ordinary Claude Code session.

## Releasing

<details>
<summary>Cutting a release</summary>

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
</details>

## Notes & limits

- Workers share one subscription usage pool; keep concurrency modest until
  rate-limit backoff lands.
- Containment is the git worktree by convention — workers can still run
  arbitrary tools. Use accordingly.
- Your working tree and `main` are never touched. Branches stay local until you
  ask the PM to open a PR (or an auto-opened feature PR), which pushes that one
  `rambl/<task>` or `rambl/feat/<slug>` branch to `origin`.
