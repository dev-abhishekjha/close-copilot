# Close Copilot

A Go agent for the month-end close on ERPNext, built for Indian companies. Deterministic checks find the problems. An LLM explains each one, and a plain-Go verifier traces every number in the explanation back to stored evidence. Nothing reaches the ledger until a maker proposes the entry and a different checker approves it. The agent reaches ERPNext and the evidence only through two MCP servers. All data in this repository is synthetic.

## Problem

At month end an accountant reconciles the bank statement against the books, matches purchase bills against the GSTR-2B the GST portal publishes, and looks for missing accruals, duplicate payments and postings in the wrong period or account. Most of that work is mechanical comparison across systems that don't talk to each other, and the mistakes it catches (input tax credit at risk, a bill paid twice) cost real money when they are missed. A general-purpose chat assistant can't be trusted here: it makes up numbers, and its claims can't be checked against the books.

## Demo

There is no public demo link yet. [docs/deploy.md](docs/deploy.md) explains how to run the stack on a VM, and its "Reviewer path" section covers running it on a laptop. The demo video and the write-up are a separate ticket (CC-1203), which adds their links here.

## Architecture

```text
 ERPNext (books) ──bot key, read-only──► mcp-books    /mcp  (7 read tools) ──┐
                                                                           │ MCP_TOKEN_AGENT
 Postgres: bank lines, GSTR-2B,  ◄────── mcp-evidence /mcp  (3 read tools) ──┤ + signed X-Run-Scope
           doc_chunks (pgvector), TEI                                      │ (one company, one month)
                                                                           ▼
                        cmd/agent: the close workflow (internal/agent)
                          1. checks          internal/checks, plain Go
                          2. retrieve        search_documents, called by code, never by a model
                          3. explainer       model; its only tool is emit_explanation
                          4. verifier        plain Go; every amount, citation and journal line
                          5. investigator    model with read tools only (opt-in, not wired in cmd/ yet)
                          6. report
                                                                           │ run_steps, artifacts,
                                                                           ▼ llm_calls, findings
                                                                       Postgres
                                                                           │
                                                                           ▼
                        web UI and JSON API (internal/web): viewer, maker, checker
                                                                           │ maker proposes,
                                                                           ▼ another checker approves
                        internal/approvals ──MCP_TOKEN_ADMIN──► mcp-books /mcp-admin
                                                                post_approved_journal_entry
                                                                           │ posting key
                                                                           ▼
                                                                       ERPNext
```

The components, the data flow of one close and the full MCP tool catalog are in [docs/architecture.md](docs/architecture.md).

## Results

The numbers below come from `evals/baseline.json` as committed. All data is synthetic, and the errors were planted by this project's own seeder. A perfect score means the detectors find the error types this project defined. It says nothing about accuracy on a real company's books.

- **Suites.** `suite-skeleton` has one company (`sharma`). It evaluates September 2026, which has 3 planted unrecorded bank charges, and uses August 2026 as a clean control month. `suite-v1` has two companies (`sharma`, `mehta`). It evaluates July to September 2026 for both, which hold 40 planted errors across 10 types, and uses sharma's June 2026 as the clean control. That makes 7 close runs.
- **Tiers.** Tier 1 replays the tool responses recorded from the seeded ERPNext. It runs the checks and calls no model, and CI gates every relevant pull request on it. Tier 2 replays the same fixtures with the explainer and verifier on. It was recorded locally through `claude -p` (`LLM_PROVIDER=claude-cli`; the baseline file doesn't store the provider), with `haiku` as the fast model and `sonnet` as the strong one. No recorded number used the Anthropic API provider.

### Overall

| Suite | Tier | Recall | Precision | Planted | Caught | Missed | False alarms | Clean-month false alarms | Verified rate | Verifier rejects |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| suite-skeleton | Tier 1 (replay, no model) | 3/3 (100.0%) | 8/8 (100.0%) | 3 | 3 | 0 | 0 | 0 | n/a (no model) | not in the baseline |
| suite-skeleton | Tier 2 (replay, explainer and verifier) | 3/3 (100.0%) | 8/8 (100.0%) | 3 | 3 | 0 | 0 | 0 | 1/8 (12.5%), unstable (see below) | not in the baseline |
| suite-v1 | Tier 1 (replay, no model) | 40/40 (100.0%) | 66/66 (100.0%) | 40 | 40 | 0 | 0 | 0 | n/a (no model) ¹ | not in the baseline |
| suite-v1 | Tier 2 | not measured | not measured | not measured | not measured | not measured | not measured | not measured | not measured | not measured |

¹ suite-v1's Tier 1 run also raised 17 findings of the types the scorer doesn't grade (`variance` and `unmatched_ledger_entry`, which are informational). They count in neither precision nor false alarms. The verified-rate denominator in the baseline is 83 (66 + 17), with 0 verified, because no model ran.

**The Tier 2 verified rate is unstable.** On identical code, six runs of suite-skeleton gave 0, 0, 1, 1, 0 and 2 verified findings out of 8. The cause was the explainer marking its own findings `needs_review`. CC-020 changed the prompt rule, but the re-baseline that would make this a hard gate hasn't happened. Until then the figure is informational, with a noise tolerance of 2 (`tasks/tier2-verified-informational.md`). Recall, precision and clean-month false alarms were identical in every run.

### By finding type

| Suite, tier | Finding type | Planted | Caught | Missed | Findings | False alarms |
| --- | --- | --- | --- | --- | --- | --- |
| suite-skeleton, Tier 1 and Tier 2 | `unrecorded_bank_charge` | 3 | 3 | 0 | 3 | 0 |
| suite-skeleton, Tier 1 and Tier 2 | `unmatched_bank_line` | 0 | 0 | 0 | 5 | 0 ² |
| suite-v1, Tier 1 | `duplicate_vendor_payment` | 6 | 6 | 0 | 6 | 0 |
| suite-v1, Tier 1 | `gstr2b_amount_mismatch` | 2 | 2 | 0 | 2 | 0 |
| suite-v1, Tier 1 | `gstr2b_missing_in_2b` | 2 | 2 | 0 | 2 | 0 |
| suite-v1, Tier 1 | `gstr2b_missing_in_books` | 2 | 2 | 0 | 2 | 0 |
| suite-v1, Tier 1 | `gstr2b_wrong_period` | 0 | 0 | 0 | 7 | 0 ² |
| suite-v1, Tier 1 | `misclassified_expense` | 4 | 4 | 0 | 4 | 0 |
| suite-v1, Tier 1 | `missing_accrual` | 6 | 6 | 0 | 6 | 0 |
| suite-v1, Tier 1 | `prepaid_not_spread` | 4 | 4 | 0 | 4 | 0 |
| suite-v1, Tier 1 | `suspicious_instruction_text` | 4 | 4 | 0 | 4 | 0 |
| suite-v1, Tier 1 | `unmatched_bank_line` | 0 | 0 | 0 | 19 | 0 ² |
| suite-v1, Tier 1 | `unrecorded_bank_charge` | 6 | 6 | 0 | 6 | 0 |
| suite-v1, Tier 1 | `wrong_period_posting` | 4 | 4 | 0 | 4 | 0 |

² These findings match items the ground truth expects without planting them: genuinely late GST filings, and gateway settlements that are left for investigation. They count as correct.

### Cost and latency

These figures are quoted from [docs/benchmarks.md](docs/benchmarks.md). They were measured with `claude-cli` on one laptop, with small samples, and are good for ranking options, not for budgeting a production bill.

- **suite-skeleton, Tier 2, after CC-1104:** 0.00280 to 0.00305 USD per close run with a warm prompt cache (0.00373 USD on a cold cache). p95 run duration was 8.6 to 9.4 s.
- **suite-v1, sharma 2026-07 replayed with retrieval (13 findings):** 0.087 to 0.093 USD and about 76 s per close with haiku explaining. With sonnet it was 0.322 to 0.348 USD and 52 to 59 s, and no more accurate.
- **The investigator (benchmark 1):** it added 0.08 to 0.15 USD and 34 to 36 s per two-month close for three bank lines. Its accuracy could not be measured.

## Quick start

Prerequisites: Go 1.26+, Docker with about 10 GB of memory for the full stack, and [golangci-lint v2](https://golangci-lint.run/welcome/install/). templ and govulncheck run through `go tool` and goose through `go run`, so golangci-lint is the only global install. `make help` lists every target.

### Without ERPNext: tests and a Tier 1 replay

```sh
make deps                               # first time only: resolves modules, writes go.sum
cp .env.example .env                    # fill in keys as .env.example describes
make check                              # build, vet, lint, unit tests
make up                                 # Postgres + pgvector and the two TEI servers, then ERPNext
make migrate                            # goose migrations
make eval-replay SUITE=suite-skeleton   # replay the recorded fixtures, score against the baseline
```

A replay needs only Postgres. If you haven't built the ERPNext image, the ERPNext half of `make up` fails, but Postgres and TEI are started first, so the replay still works. On Apple Silicon, set `TEI_IMAGE_TAG=cpu-arm64-1.9` in `.env`.

### The full local stack

[docs/setup.md](docs/setup.md) covers each step: building the ERPNext image with India Compliance, creating the site, the setup wizard and troubleshooting. In order:

```sh
deploy/erpnext/build-image.sh           # once, 15 to 30 minutes
make up
deploy/erpnext/new-site.sh              # once
deploy/erpnext/setup-wizard.sh          # once
deploy/erpnext/api-users.sh             # once: writes tmp/erp-keys.env; copy the bot and poster keys into .env
make migrate
(set -a; . tmp/erp-keys.env; set +a; go run ./cmd/seed bootstrap --company sharma)
deploy/erpnext/backup-clean.sh          # the clean state that make erp-reset restores
(set -a; . tmp/erp-keys.env; set +a; make erp-reset seed load SUITE=suite-skeleton)
make up-ingest ingest                   # docling, then chunk, embed and store the document corpus
```

The seeder runs with the Administrator keys from `tmp/erp-keys.env` in a subshell, so they never sit in `.env`. One ERPNext is shared by every session, and `make erp-reset` wipes it. `CLAUDE.md` explains the lock.

Then start the two MCP servers, each in its own terminal. They read only environment variables, so export `.env` first ([docs/mcp.md](docs/mcp.md)):

```sh
make build
set -a; . ./.env; set +a
bin/mcp-books --transport=http --addr=:7001
bin/mcp-evidence --transport=http --addr=:7002
```

and run a close or the web app:

```sh
go run ./cmd/agent close --company sharma --month 2026-09   # one close; prints the run ID and report path
go run ./cmd/agent serve                                    # web UI and API on APP_ADDR (default :8000)
go run ./cmd/audit rebuild <finding-id>                     # rebuild a finding from stored artifacts
```

`make run-agent` runs `cmd/agent` with no subcommand, so it only prints usage. Pass a subcommand as above. With `LLM_PROVIDER=claude-cli`, the default, the model calls go through your local `claude -p` and need no API key. Explaining needs `PSEUDONYM_KEY`, and `--no-explain` runs the close without any model.

**Deploying:** [docs/deploy.md](docs/deploy.md) is the production VM guide. It uses its own compose files and must not be mixed with `make up`.

## How it works

A close is a fixed workflow, not a free agent loop ([ADR 0005](adr/0005-deterministic-workflow-bounded-agent.md)). A run moves through preflight, checking, retrieving, explaining, verifying, investigating and synthesizing. Each step is a `run_steps` row in Postgres, so a run killed mid-way resumes without repeating finished work (`agent resume <run_id>`).

**Checks** (`internal/checks`). Plain Go, no model ([ADR 0004](adr/0004-deterministic-checks-first-llm-explains.md)). The checks are bank reconciliation, which finds unrecorded bank charges, unmatched bank lines and unmatched ledger entries. Duplicate vendor payments. GSTR-2B against purchase bills, which finds bills missing from 2B, 2B entries missing from the books, amount mismatches, filings in the wrong period and ineligible credit. Missing accruals for recurring suppliers. Policy rules from `config/rules.yaml` (capitalisation, prepaid spread, cut-off). Month-on-month variance. Suspicious instruction text in invoices and narrations. Each check reads the books and the evidence through the MCP tools and stores the tool results as snapshots. Every finding points at those stored artifacts.

**Explainer** (`internal/agent`, `explain.go`). One model call per finding. It returns a short explanation, a suggested action, the amounts it used, citations and an optional balanced journal entry, all through one forced tool, `emit_explanation`. The model can call nothing else. Its input is the finding, the evidence projected from the stored snapshots and up to three passages from the policy manual, contracts and close notes retrieved for the finding, each fenced as data. GSTINs, PANs and individuals' names are replaced by per-run tokens before the call and restored afterwards (`internal/agent/redact.go`). The fast model handles attempts 1 and 2, and the strong model takes attempt 3.

**Verifier** (`internal/agent`, `verify.go`). Plain Go, not a model. It rejects an explanation if any amount it states, any `cited_amounts_paise` value or any journal line is not within ₹1 of an evidence amount, the sum or difference of two of them, or the finding's amount. It also rejects a citation that doesn't name a passage the model was shown, and evidence outside the run's company and month. A rejected explanation is retried with the violations as feedback, up to three attempts in all. After that the finding goes to `needs_review`, and no entry can be proposed for it. The verifier reads only stored artifacts, so `cmd/audit rebuild` gets the same verdict offline. It proves that numbers and references are grounded. It does not prove that the story is true.

**Investigator** (`internal/agent`, `investigate.go`). A bounded tool-using loop for unmatched bank lines the rules couldn't classify, such as a gateway settlement paid out net of fees. It sees only the nine read tools and `emit_resolution`, never `search_documents` and never the admin tool. A call to anything else is refused unexecuted. It is capped at 8 tool calls, 90 seconds and 30,000 tokens per finding. Tool results are projected, with free text dropped or fenced, before the model sees them, and the result is verified like an explanation. The workflow supports it, but no binary wires it in yet, so production closes skip the step (see Known limitations).

The same pieces answer free-text questions on `POST /api/ask`. A router (rules from `config/triage.yaml`, then a fast-model classifier) refuses out-of-scope questions with fixed text. A bounded read-tool loop gathers numbers, code retrieves passages, and one forced-tool call writes the answer. The answer is then verified. If it fails twice, the caller gets a fixed message, never the unverified answer.

## Security model

The invariants are listed in [CLAUDE.md](CLAUDE.md). Tests, the analyzers in `gates/analyzers` and the build hooks enforce them:

- **The agent never touches ERPNext directly.** `internal/agent` doesn't import `internal/frappe` (the `noerpimport` analyzer), and the `agent` binary doesn't link the ERPNext client (`cmd/agent/deps_test.go`). It reaches the books only through the books MCP server.
- **One write path.** All seven books tools and the three evidence tools carry `readOnlyHint` and only read. The only write tool, `post_approved_journal_entry`, lives on the books server's separate `/mcp-admin` route, behind `MCP_TOKEN_ADMIN`. Only `internal/approvals` uses that token (the `admintoken` analyzer). The tool takes only a proposal ID and reads the lines, company and date from the approved proposal, never from the call. It posts with a dedicated ERPNext user that can only create and submit Journal Entries.
- **Maker-checker.** A maker (`asha`) proposes an entry for a verified, open finding. A checker (`ravi`) approves it, and only then is it posted. A checker can't approve their own proposal (`ErrSelfApproval`, HTTP 403). Roles are explicit in `config/users.yaml`: viewer, maker, checker.
- **Retrieved text never reaches a model that can call tools.** `search_documents` is called by code, in the workflow's retrieve step and the Asker's retrieval stage, and is never offered to a model. The explainer's only tool is `emit_explanation`. The investigator has neither `search_documents` nor any admin tool.
- **Run scope.** The agent signs a short-lived scope (company, month) with `MCP_SCOPE_KEY` for each run and sends it on every MCP request. The servers refuse a call outside it before reading anything (CC-506). Document search filters by company inside the SQL, before ranking (CC-804).
- **Pseudonymisation.** GSTINs, PANs and individuals' names are replaced by keyed per-run tokens before any model call, and stored prompts hold only tokens (CC-710).
- **Rebuildable findings.** Steps, content-addressed artifacts and model calls are stored, and `cmd/audit rebuild` reproduces a finding and its verdict from them (CC-709).
- **Audit log.** Both MCP servers write an `audit_log` row for every tool call and fail the call if the write fails. The application's database role can't update or delete those rows (migration 0005).
- **Money and egress.** Amounts are `int64` paise end to end, and only `internal/money` touches floating point (the `nofloat` analyzer). All outbound HTTP goes through `internal/httpx`, which refuses hosts outside the configured services (the `egress` analyzer). Prompt-injection text in the data is detected as a finding (`suspicious_instruction_text`), and an investigation that a refused call steered can't leave a proposal (CC-1103).

## Known limitations

What isn't built, isn't measured or is known to fail:

- **Synthetic data only.** Two invented companies and one seeder planted every error. The 100% Tier 1 scores show that the detectors match this project's own error definitions. No real company's books have been run.
- **Tier 2 verified rate is unstable**, at 0 to 3 of 8 on identical code. It stays informational until the CC-020 re-baseline or 2026-11-13, whichever comes first.
- **suite-v1 has no Tier 2 baseline**, so explanation quality has been measured only on suite-skeleton's 8 findings and in the small benchmarks.
- **The investigator isn't wired into any binary.** Neither `cmd/agent` nor `cmd/eval` sets it, so closes record a skipped investigate step. Its accuracy has never been measured, because the seeded ERPNext didn't match the suite's ground truth when benchmark 1 ran. In those runs, 4 of 12 investigations hit a `claude -p` `api_error`.
- **Citation accuracy is weak.** Top-3 retrieval cited an expected section in 2 of 6 golden cases. Giving the model the whole policy manual and contracts got 6 of 6 (benchmark 2).
- **One company and one month per live run.** The UI and API start runs one company-month at a time, and closes started from the UI run inside the web process.
- **No per-user company lists.** `config/users.yaml` gives roles but no company list, so every signed-in user can see every configured company.
- **Pseudonymiser gaps.** The GSTIN and PAN patterns use word boundaries, so an identifier glued to other letters or digits, or written in lower case, is not masked. Evidence strings are cut to 500 characters before masking, so an identifier cut at the boundary can leak. Both must be fixed before any non-synthetic data.
- **`MCP_SCOPE_KEY` length.** The servers refuse only an empty key, so a short key is accepted.
- **Approval workflow edges (CC-1004 follow-ups).** A dismiss can race a propose-and-approve, which could leave a posted entry on a dismissed finding. A finding stays `open` after its entry posts. A proposal stuck at `approved` after a permanent post refusal needs manual SQL.
- **Verifier amount parsing.** Some chains of amounts in prose are not fully checked, such as `5,000-6000-7000`, `5,000/6000` and `₹500 and 600`.
- **The laptop fixed-asset line** was listed as unproven on a live ERPNext. It now posts: the suite-v1 fixtures recorded from ERPNext hold the quarterly laptop bill on `Office Equipment` for sharma's July. No dedicated live test pins it.
- **[docs/mcp.md](docs/mcp.md) is stale:** it lists two evidence tools and leaves out `search_documents`. The catalog in [docs/architecture.md](docs/architecture.md) is current.
- **Customer discovery hasn't happened.** The accountant interviews (CC-103) are owner work and still open, so nothing here is validated against a practising accountant's close.

Failure cases seen in evals:

- **GSTR-2B false alarms on suite-v1 (CC-013).** The first replay scored precision 59/81, with 22 false alarms. 21 came from the ground truth filing late invoices one month late, plus stale GSTR-2B rows left in Postgres by an earlier `--small` load. The fixes went into the truth builder and the loader, which now replaces a period's rows (CC-013, CC-017). The baseline is now 66/66.
- **The explainer marked nearly every finding `needs_review` (CC-020).** In the low runs, 7 or 8 of 8 findings were flagged, including fully explained bank charges. The prompt rule was rewritten, but the verified rate is still informational.
- **An `unrecorded_bank_charge` false alarm** on suite-v1 `sharma` 2026-09 (`BNK-20260904-001`) appeared in the first replay. It is absent from the current baseline, which has 0 false alarms.
- **Retries from length overshoot (CC-1104).** The explainer overshot its 600-character limit on 6 of 14 direct calls, and each overshoot cost a retry turn. Setting the prompt target to 500 characters brought that to 0 of 14.

## Docs

| Doc | What it holds |
| --- | --- |
| [docs/architecture.md](docs/architecture.md) | Components, the data flow of one close, the MCP tool catalog, trust boundaries |
| [docs/mcp.md](docs/mcp.md) | Running and inspecting the MCP servers, contract tests and schema goldens |
| [docs/setup.md](docs/setup.md) | Local stack: ERPNext with India Compliance, Postgres, TEI |
| [docs/deploy.md](docs/deploy.md) | Deployment on a VM, and the reviewer path on a laptop |
| [docs/benchmarks.md](docs/benchmarks.md) | Measured trade-offs: investigator, RAG against whole documents, retrieval modes, model choice, cost and latency |
| [docs/deploying-at-a-customer.md](docs/deploying-at-a-customer.md) | Discovery questions, read-only first, bank and GST feeds, data residency, shadow mode, success metrics |
| [adr/](adr/) | Decision records: [0001](adr/0001-build-with-local-claude-code.md) build with local Claude Code, [0002](adr/0002-mcp-servers-in-front-of-erpnext.md) MCP servers in front of ERPNext, [0003](adr/0003-postgres-pgvector-not-a-vector-db.md) Postgres and pgvector, [0004](adr/0004-deterministic-checks-first-llm-explains.md) checks first, [0005](adr/0005-deterministic-workflow-bounded-agent.md) workflow with a bounded agent, [0006](adr/0006-count-based-eval-gates.md) count-based eval gates |
| [docs/implementation-tickets.md](docs/implementation-tickets.md) | What to build: the tickets, plus the shared specs (env vars, schema, MCP tools, API) |
| [docs/orchestrated-implementation-plan.md](docs/orchestrated-implementation-plan.md) | How it gets built: roles, task graph by phase, gate ladder G0 to G6, RAG evaluation and guardrails |

The two planning docs are exports of living docs on claude.ai (links at the top of each file).

## How it gets built

Claude Code builds every ticket on the owner's machine ([ADR 0001](adr/0001-build-with-local-claude-code.md)). Open `claude` in the repository root; `CLAUDE.md` gives it the project rules.

```text
/next            tickets ready to start, in phase order
/spec CC-xxx     write specs/CC-xxx.md (regulated specs wait for the owner's approval)
/build CC-xxx    branch, delegate to the role subagent, run the gates, commit, merge
/gates           run the gate ladder on the current branch
```

Role subagents live in `.claude/agents/`. A hook blocks edits outside the ticket's declared files and the role's boundaries, and no agent grades its own output. During the build phase the orchestrator merges a ticket to `main` with `git merge --no-ff`, through the merge queue in `/build`, once every gate passes. Regulated tickets also go on the owner's review list. Subagents never merge.

## Layout

```text
CLAUDE.md        project rules for Claude Code
.claude/         subagents (build roles), skills (/next /spec /build /gates), settings and hooks
adr/             decision records
specs/           one spec per ticket; _template.md is the shape
tasks/           graph.yaml (phase, owner, risk, deps, files per ticket), review and hardening notes
gates/           gate programs (ready, declared, protected, graph, lint) and the static analyzers
cmd/
  agent/         close, resume, mint-scope, serve (web UI and API)
  audit/         rebuild a finding from stored artifacts and re-run the verifier
  eval/          run suites (live, record, replay) and score them against ground truth
  ingest/        docling, chunk, embed and store the document corpus
  load/          bank statements and GSTR-2B files into Postgres
  mcp-books/     books MCP server: read tools on /mcp, the approved-write tool on /mcp-admin
  mcp-evidence/  evidence MCP server: bank lines, GSTR-2B entries, document search
  probe/         check the ERPNext API keys and dump DocType schemas
  seed/          synthetic world, ERPNext books, bank and GSTR-2B files, ground truth
internal/
  agent/         workflow, explainer, verifier, investigator, router, ask, pseudonymisation
  approvals/     maker-checker; the only user of the admin token
  books/         books MCP tools and the admin write tool
  buildinfo/     version string
  checks/        deterministic month-end checks
  cli/           shared main: -version, JSON logs, config, signals
  company/       company profiles and check rules
  config/        env vars to typed config; secrets
  deps/          pins dependencies not yet imported
  evals/         runner, scorer, fixtures, baseline compare, judge
  evidence/      bank and GSTR-2B loaders, evidence MCP tools
  faults/        fault injection for tests and eval runs
  frappe/        typed ERPNext REST client
  httpx/         the only outbound HTTP client (egress allowlist)
  ledger/        ERPNext-free accounting types shared by checks, store and agent
  llm/           provider interface: claude-cli and Anthropic, caching, cost
  mcpkit/        MCP server scaffold: transports, bearer auth, run scope, audit, rate limits
  money/         int64 paise and Indian-grouped formatting
  retrieval/     TEI and docling clients, chunking, hybrid search with rerank
  seed/          synthetic world generator, planter, ground truth
  store/         Postgres layer and the run data plane
  telemetry/     OpenTelemetry tracing to Langfuse
  testsupport/   fake ERPNext for tests
  web/           templ + htmx UI and JSON API, login and roles
config/          company profiles, rules, triage, pricing, users
deploy/          compose files for local and production, ERPNext build scripts
docs/            guides and the two planning docs
evals/           scenarios, ground truth, fixtures, golden sets, baseline
migrations/      goose SQL migrations
```

## Dependencies

| Area | Module |
| --- | --- |
| MCP servers and clients | `github.com/modelcontextprotocol/go-sdk` |
| LLM | `github.com/anthropics/anthropic-sdk-go` |
| Postgres, vectors, migrations | `github.com/jackc/pgx/v5`, `github.com/pgvector/pgvector-go`, `github.com/pressly/goose/v3` |
| Tracing | `go.opentelemetry.io/otel` (+ sdk, OTLP/HTTP exporter, `otelhttp`) |
| UI | `github.com/a-h/templ` (htmx from a pinned CDN URL) |
| Config files | `go.yaml.in/yaml/v3` |
| Auth and limits | `golang.org/x/crypto` (bcrypt), `golang.org/x/time` (rate) |
| Static analyzers | `golang.org/x/tools/go/analysis` |
| Tests | `github.com/testcontainers/testcontainers-go` (+ postgres module) |
| Tools | templ and govulncheck via `go tool`; goose via `go run` |

## Licence

MIT, see [LICENSE](LICENSE). All data in this repository is synthetic.
