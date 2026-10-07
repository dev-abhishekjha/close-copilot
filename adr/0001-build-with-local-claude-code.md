# ADR 0001: Build everything with local Claude Code

- Status: accepted
- Date: 2026-10-07
- Decides: Open questions 1 and 3 in the Orchestrated Implementation Plan

## Context

The plan defines a build-time loop (Layer 2): an orchestrator assigns each ticket to a role, a worker implements it from a spec, and a gate ladder (G0–G6) decides whether it merges. It left two questions open: which parts the owner writes by hand, and which coding agent runs the roles.

## Decision

1. **Claude writes all of the code**, including the verifier (CC-705), the bank matcher (CC-602) and run-scoped binding (CC-506). The owner reviews every regulated ticket (G6) and reads those three line by line, because interviewers will probe them.
2. **The coding agent is the owner's local Claude Code CLI**, configured in this repo:
   - `CLAUDE.md`: project memory, conventions, invariants, protected paths.
   - `.claude/agents/`: one subagent per build role (implementer, integration-engineer, domain-data-engineer, llm-engineer, eval-engineer, security-reviewer). The main session is the orchestrator.
   - `.claude/skills/`: `/next`, `/spec`, `/build`, `/gates` implement the loop.
   - `.claude/settings.json`: allow rules for the toolchain, ask rules for protected paths and history-changing git, and two hooks: a guard that enforces role boundaries and the current spec's declared files, and gofmt after every Go edit.
3. **The product's own LLM calls default to the local CLI during development.** CC-701 adds a `claude-cli` provider that calls `claude -p` with `--json-schema`; `LLM_PROVIDER=anthropic` switches to the API for the public demo and final baselines.
4. **No GitHub yet.** Gates run locally through `make` and the skills; branches merge locally. CI (`.github/workflows/ci.yml`) takes over when a remote exists.

## Consequences

- Build cost is the owner's Claude plan usage, not a dollar budget; the Phase 0 pilot records usage per ticket to set the pace.
- The harness (CC-001, CC-002) matters more, since agents write every line; its Go gate programs replace the manual checks in `/build` as they land.
- `claude -p` calls are slower than the API and the CLI's model aliases can move; record the resolved model on every call (CC-709) and re-baseline on the API before publishing numbers.
- Anthropic has been changing how plan usage outside interactive Claude Code is metered; if `claude -p` usage becomes limiting, switch `LLM_PROVIDER` to `anthropic`.
