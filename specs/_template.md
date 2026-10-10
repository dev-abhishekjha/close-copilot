---
id: CC-000
title: Short title from the tickets doc
phase: 1                    # 0-5; tickets with a Phase 2 pass get a section below
owner_role: implementer     # implementer | integration-engineer | domain-data-engineer | llm-engineer | eval-engineer | human
risk: standard              # standard | data-sensitive | regulated
approved_by:                # regulated only: the owner's name and date, added after review
depends_on: []              # ticket IDs that must be merged into main first
needs_erpnext: false        # true takes tmp/erpnext.lock; one holder at a time
# files globs (gates/glob.go): paths from the repo root; * and ? stay within one segment,
# ** spans zero or more whole segments, a trailing / means everything under it, no {a,b} braces.
files:                      # every file the ticket may change
  - internal/example/example.go
  - internal/example/example_test.go
consumes: []                # interfaces or tables this ticket reads
produces: []                # interfaces, tables, finding types or tools it adds
acceptance:                 # each line is a shell command that exits 0 when met
  - go test ./internal/example/... -race -count=1
gates: [G1, G2, G3]         # add G4-tier1 / G4-tier2 / G5 / G6 per the plan's gate ladder
budget: {max_attempts: 3, max_wall_minutes: 90}
---

# CC-000 · Short title

## Goal

One sentence from the ticket.

## Context

- Ticket: docs/implementation-tickets.md, section `CC-000`.
- Shared specs it relies on: name the sections (Finding, schema, MCP tool catalog...).
- Plan row: phase, owner, gate-run acceptance from tasks/graph.yaml.
- Quoted from the shared specs: the exact fields, columns, signatures and rules this ticket needs, so the worker doesn't open `docs/`.

## Code map

Existing code to call or implement, with signatures. Read these and nothing else unless they point further.

- `internal/example/types.go:12`: `func Example(ctx context.Context, in Input) (Output, error)`
- Style to copy: `internal/other/other_test.go` (table-driven test with fakes).

## Subtasks

- [ ] Copied from the ticket, cut to this phase's scope.

## Acceptance (human-readable)

- What the commands above prove, plus anything that needs a person to check.

## Out of scope

- Work deferred to a later phase or ticket.

## Notes

- Design choices, new dependencies, risks.

## Phase 2 pass

Only for tickets the plan widens in Phase 2 (CC-302 to CC-307, CC-502, CC-503, CC-602): what changes for `suite-v1`.
