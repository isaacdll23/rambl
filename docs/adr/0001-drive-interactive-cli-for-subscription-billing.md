# 1. Drive the interactive Claude Code CLI for subscription billing

Status: Accepted

## Context

Claude Code sessions are billed according to how the session is classified.
An interactive Claude Code session — a real `claude` REPL attached to a
terminal — counts against the Claude subscription's (Pro/Max) usage limits.
The Agent SDK and headless `claude -p` are classified differently and billed
from a separate, API-priced credit pool.

rambl's entire premise is running a fleet of agents on the user's existing
Claude subscription rather than on API credits. The billing classification of
each spawned session is therefore not an implementation detail — it is the
constraint that the design exists to satisfy.

## Decision

Drive the **real interactive `claude` binary** under a PTY for every session
(both the PM and the workers). Never invoke `claude -p`, and never use the
Agent SDK — either would move usage off the subscription and onto the
API-priced pool.

Strip `ANTHROPIC_API_KEY` and `ANTHROPIC_AUTH_TOKEN` from the child process
environment so that authentication falls back to the OAuth subscription
credentials established by `claude` → `/login`. An inherited API key would
re-route billing regardless of how the session is launched.

Treat the session transcript JSONL (written under `~/.claude/projects/...`) as
the source of truth for a turn's output. Never scrape the rendered TUI: the
terminal output is a render stream, not a data interface, and parsing it is
both fragile and lossy.

## Consequences

- Usage stays on the user's Claude subscription, which is the whole point of
  the tool.
- rambl is coupled to interactive-CLI and transcript behavior that is internal
  to Claude Code and can change between versions. This coupling is accepted
  deliberately.
- The PTY plumbing and transcript-tailing machinery exist solely to make
  driving an interactive CLI programmatic. That complexity is the price paid
  for subscription billing, and is worth it as long as the billing distinction
  holds.
