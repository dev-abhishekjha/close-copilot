---
name: spec
description: "Design mode. Write specs/<ID>.md for one Close Copilot ticket from the tickets doc, the plan and tasks/graph.yaml, then run the G0 readiness check. Regulated specs stop for the owner's approval."
argument-hint: CC-xxx
arguments: [id]
disable-model-invocation: true
model: sonnet
allowed-tools: Read Grep Glob Write Edit Bash(git log *) Bash(git status*)
---

# Write the spec for $id

You are the orchestrator in design mode. The spec is the only thing the worker will get, so it must stand on its own.

## Gather

1. `tasks/graph.yaml`: the entry for `$id` (phase, owner, risk, depends_on, needs_erpnext, files, check, phase_2_pass).
2. `docs/implementation-tickets.md`: the section that starts `### $id ·` up to the next `### CC-`. Use `grep -n "^### $id " docs/implementation-tickets.md` to find it. Also read the shared-spec sections it refers to.
3. `docs/orchestrated-implementation-plan.md`: the task-graph row for `$id`, and for regulated or data-sensitive tickets the matching guardrail rows.
4. If `specs/$id.md` exists, update it instead of starting over.

## Write `specs/$id.md` from `specs/_template.md`

- `files`: the union of the ticket's **Files** line and the graph's `files`, plus each package's `_test.go` and `testdata/`. Exact paths or globs, one per line, no `{a,b}` braces. This list is enforced by the guard hook, so include everything the work needs and nothing else.
- `acceptance`: turn the ticket's acceptance criteria and the graph's `check` into shell commands that exit 0 when met (`go test ./internal/x/... -run TestY -race -count=1`, `go run ./cmd/eval score ... --require 'type.recall>=3/3'`). Criteria that need a person go in the body under "Acceptance (human-readable)".
- `gates`: always G1, G2, G3. Add `G4-tier1` if the ticket touches checks, seed, evidence, books, store or evals and the eval harness exists (CC-905 merged); `G4-tier2` if it touches agent, llm, prompts, retrieval or corpus; G5 for data-sensitive and regulated; G6 for regulated or `human_review: true`.
- Phase scope: for `phase_2_pass: true` tickets in Phase 1, write the `suite-skeleton` scope in Subtasks and the rest under "Phase 2 pass".
- `owner: human` tickets: write the spec as a checklist for the owner; no worker will run it.
- **Context:** quote the exact rows, fields and rules the worker needs from the shared specs (Finding fields, table columns, tool signatures, env vars), not just section names. A worker reads `docs/` only for something the spec doesn't quote, so a missing quote costs a whole doc read.
- **Code map:** find the existing code the ticket calls or implements (`grep -rn` for the types and functions) and list each as `path:line` with its signature, plus one existing test or fixture to copy the style from. Keep it to what the worker needs, usually 3 to 8 entries. Note anything it should *not* open.

## G0 readiness check (report each line as pass or fail)

- Front matter parses and every template field is filled.
- Every `depends_on` ticket is done (`git log main --format=%s | grep -E '^CC-xxx:'`).
- No declared file overlaps a ticket that is in progress (`tmp/current-task` and open `cc-*` branches).
- Every acceptance line is a runnable command.
- Context quotes what the worker needs and the Code map points at real `path:line` entries (spot-check two).
- Regulated: `approved_by` is set.

## Finish

- Regulated or `human_review: true`: start `ticket-reviewer` (Opus) in mode `spec` with the ID and checkout path, and fix every blocking finding before you finish. During the build phase `/build` records the delegated approval; otherwise show the owner a short summary (scope, files, acceptance, risks, the reviewer's verdict) and ask for approval, and on approval set `approved_by: abhi <date>`. Don't start the build.
- Otherwise: say the spec is ready and suggest `/build $id`.

Don't commit; `/build` commits the spec as the branch's first commit.
