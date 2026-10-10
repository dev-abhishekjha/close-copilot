# Close Copilot

A Go agent that runs the month-end close on ERPNext for Indian companies. Deterministic checks find the problems, an LLM explains each one, a verifier traces every number back to the evidence, and nothing reaches the ledger without maker-checker approval. All data is synthetic.

Everything in this repo is built by Claude Code on the owner's machine (ADR 0001). The owner (abhi) owns `main`. During the build phase (owner decision, 2026-10-10) the orchestrator merges on the owner's behalf once every gate passes; subagents never merge.

## Where things are decided

- `docs/implementation-tickets.md`: **what** to build. 69 tickets with goals, subtasks, acceptance criteria and files, plus the shared specs (env vars, Postgres schema, MCP tools, HTTP API). Exported from a living doc; don't edit the copy here.
- `docs/orchestrated-implementation-plan.md`: **who** builds each ticket, in which phase, and **how** a merge is checked (gate ladder G0–G6, guardrails, RAG eval design).
- `tasks/graph.yaml`: every ticket's phase, owner role, risk class, dependencies, declared files and gate check.
- `specs/<ID>.md`: the contract for one ticket. A worker gets its spec, never the whole doc.
- `adr/`: decisions and why.

## How a ticket gets built

1. `/next` lists the tickets whose dependencies are merged, in phase order.
2. `/spec CC-xxx` writes `specs/CC-xxx.md` from the ticket and the graph. A regulated spec waits for the owner's approval.
3. `/build CC-xxx` creates the branch, delegates to the owner subagent, runs the gates, retries up to three times with a failure report, runs the security review for data-sensitive and regulated tickets, and commits on the branch.
4. During the build phase the orchestrator merges with `git merge --no-ff` through the merge queue in `/build`, after the gates (and G5 where required) pass; regulated tickets go on the owner's review list. A ticket is done when `main` has a commit whose subject starts with `CC-xxx:`.

Work in **phase order** (0 → 5), not epic order. Phase 1 is a thin end-to-end slice on `suite-skeleton` (one company, one month, three planted bank charges); nothing widens until Gate B passes. `/gates` runs the ladder on the current branch at any time.

## Roles

The main session is the orchestrator: it reads, plans, delegates and runs gates. It does not write code under `cmd/` or `internal/` itself. Workers are subagents in `.claude/agents/`:

| Subagent | Owns | Never touches |
| --- | --- | --- |
| implementer | standard Go: money, store, checks, retrieval, web, config, CLI plumbing | secrets, ERPNext, files outside the spec |
| integration-engineer | frappe, books, evidence, mcpkit, deploy/erpnext, docs/erpnext-schema | the admin write tool unless the spec is owner-approved |
| domain-data-engineer | seed, config/companies, evals/scenarios, corpus generator | internal/checks, internal/agent, internal/evals |
| llm-engineer | llm, agent (explainer, verifier, investigator, router), prompts, pricing | internal/evals, golden sets, baselines, budget caps |
| eval-engineer | internal/evals, judge drafts, golden-set drafts | code under test in the same ticket, the baseline |
| security-reviewer | nothing (reports only) | everything (read-only) |

No agent grades its own output: the data engineer plants the errors, the implementer writes the detectors, the eval engineer writes the scorer. The guard hook enforces these boundaries.

## Token budget (owner decision, 2026-10-10)

Spend tokens where judgment is needed and nowhere else. These rules never loosen a gate.

- **Models:** each subagent's frontmatter sets its model: Sonnet for implementer, integration-engineer, domain-data-engineer and eval-engineer; Opus for llm-engineer and security-reviewer. `/build` raises a worker to Opus for regulated tickets and for a last attempt after the same failure twice. Run the main session on Sonnet (`/model sonnet`) and switch to Opus to build a regulated ticket. Search-only questions go to the Explore agent with `model: "haiku"`; Haiku never writes code.
- **One ticket per session:** `/clear` after a ticket merges, once nothing else is in flight. State lives in git, `specs/`, `tasks/` and `tmp/`; `/next` picks it up.
- **Specs stand alone:** a spec quotes what the worker needs from `docs/` and has a Code map, so workers don't read the 69-ticket doc or explore the repo.
- **Logs go to files:** commands write to `tmp/reports/*.log`, and agents read only the failing lines. Retries get the failure report, never a raw log.
- **Model calls in the product:** with `LLM_PROVIDER=claude-cli`, Close Copilot's own runtime calls use this same plan. While building, run evals without the model (`--no-agent` today; `--replay --no-llm` once CC-905 lands). Use the model only where a spec lists G4-tier2, or to set a baseline.

## Commands

```sh
make check       # build, vet, lint, unit tests (G1–G3 core)
make gates       # tidy check + check + govulncheck
make test-int    # integration tests (Docker, testcontainers)
make up / down   # Postgres + TEI (+ ERPNext once CC-102 lands)
make migrate     # goose migrations
make help        # everything else
```

## Conventions

- Go 1.26+. `context.Context` first on every function that does I/O; wrap errors with `%w`; log with `log/slog` as JSON.
- Money is `int64` paise (`money.Paise`). Only `internal/money` converts to or from floating point; decode ERPNext amounts as `json.Number`.
- Dates are `YYYY-MM-DD` at the edges and `time.Time` at UTC midnight inside; months are `YYYY-MM`.
- Secrets come only from environment variables. `.env.example` lists every variable; never read `.env`.
- Synthetic data only. Never use real company data, real GSTINs or employer code.
- Unit tests never call ERPNext, Docker or a real model: use fakes and recorded fixtures. Integration tests carry the `integration` build tag.
- New dependencies go in the spec's notes and the PR summary; never edit `go.sum` by hand (`go mod tidy`).

## Invariants (tests, analyzers and hooks enforce these)

- `internal/agent` never imports `internal/frappe`; the agent reaches ERPNext only through MCP.
- The MCP admin token is used only by `internal/approvals`; write tools exist only on `/mcp-admin`.
- Retrieved document text never reaches a model that can call tools. The explainer's only tool is `emit_explanation`; the investigator has no `search_documents` and no admin tool.
- Every MCP call is scoped to the run's company and month (CC-506); the company filter sits inside the SQL, before ranking (CC-804).
- GSTINs, PANs and person names are pseudonymised before any model call (CC-710).
- Every finding can be rebuilt from stored artifacts (CC-709).

## Protected paths (ask the owner before editing)

`evals/golden/**`, `evals/scenarios/**`, `evals/baseline.json`, `gates/**`, `.github/**`, `internal/books/admin.go`, `internal/approvals/**`, `internal/mcpkit/auth*.go`, `config/users.yaml`, `docs/**`, `.claude/**`, `CLAUDE.md`. A baseline changes only in its own `baseline-update` commit.

## LLM calls at runtime

`LLM_PROVIDER=claude-cli` (the default) runs the product's model calls through the local `claude -p`, so development needs no API key. `LLM_PROVIDER=anthropic` uses `ANTHROPIC_API_KEY` and is required for the public demo and for final baselines. Never raise `LLM_DAILY_BUDGET_USD` or add tools to an allowlist without the owner.

## Git

- One branch per ticket: `cc-602-bank-rec`. Commit subjects start with the ticket ID: `CC-602: bank reconciliation matcher`. Add the trailer `Agent-Run: <session id>`.
- Only the orchestrator merges to `main` and pushes (`--no-ff`, never `--force`); never rebase or reset `main`. Parallel tickets use worktrees under `.worktrees/`.
- `tmp/` (git-ignored) holds per-build state: `tmp/current-task`, `tmp/erpnext.lock`, `tmp/reports/`.

## ERPNext lock

There is one shared ERPNext. A spec with `needs_erpnext: true` takes `tmp/erpnext.lock`; only one such ticket runs at a time, and a reset wipes whatever another session wrote.
