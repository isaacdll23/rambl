# 2. Isolate workers in out-of-repo git worktrees

Status: Accepted

## Context

rambl runs multiple autonomous coding workers concurrently against a single
managed repository. Each worker edits files, runs tools, and commits. Without
isolation they would race on a shared working tree and could corrupt each
other's changes or the user's own in-progress work. The user's working tree
and `main` branch must stay pristine throughout.

## Decision

Run each worker in its own dedicated git worktree, created **outside** the
managed repository, under `~/.rambl/worktrees/<project>/<slug>`. A worker's
results land on a `rambl/<slug>` branch.

When a task depends on other tasks, the dependency branches are merged into the
dependent worker's worktree before it runs, so each worker sees the upstream
results it needs.

Nothing is ever pushed automatically. Branches remain local for the user to
review, merge, or discard.

## Consequences

- The managed repository stays clean: the user's working tree is never touched,
  and because the worktrees live outside the repo, no `.gitignore` changes are
  required to keep worker scratch space out of the user's history.
- Concurrent workers cannot collide on a shared working tree.
- Isolation is by worktree convention only. Workers still run arbitrary
  tools, bash, and network access with permissions disabled; the worktree
  bounds their filesystem scratch space, not their capabilities. Point rambl
  only at repositories where running autonomous agents with that level of
  access is acceptable.
