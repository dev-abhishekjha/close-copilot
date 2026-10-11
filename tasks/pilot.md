# Pilot and Phase 1 measurements

Two tables: the Gate A pilot (still to fill in by hand) and the Phase 1 measurements Gate B asks for (attempts, tokens and review minutes per ticket). Drafted 2026-10-11 from `git log` and the failure reports kept in `tmp/reports`. Nothing here was measured by a clock or a token counter, so read the columns as follows.

## Gate A pilot

Filled in after the pilot tickets (CC-301 and CC-102's `docs/setup.md`) merge through G0–G3. One row per ticket.

| Ticket | Attempts | Tokens | Wall minutes | Owner review minutes | Hand edits | Notes |
| --- | --- | --- | --- | --- | --- | --- |
| CC-301 | 1 commit | not recorded | 14 | | | Merged through G1–G3 |
| CC-102 | 3 commits | not recorded | 55 | | | Gates pass (9/9 acceptance against the live stack); two follow-up commits on the dev site |

## Phase 1 (Gate B, build-time measurements)

How to read the columns:

- **Impl commits** is the number of implementation commits on `main` for the ticket (not spec, merge or note commits). It is a proxy for attempts: a ticket that needed a fix after merge or a retry shows more than one.
- **Failure reports** is how many `tmp/reports/<ID>-attempt-*` files exist. `tmp/` is git-ignored and not complete, so `-` means none was kept, not that none happened.
- **Wall min** is the time from the ticket's spec commit to its merge commit (or, for older tickets with no branch, to its last commit). It includes waiting, owner pauses and overnight gaps. CC-205 (689) includes a stop by the owner and a restart the next day.
- **Tokens** are not recorded in the repository. To fill them, total each ticket's subagent usage from the session transcripts, or add the figure to the end-of-ticket report from now on.
- **Owner review min** is for the owner to fill in.
- **Gates** is what the merge commit message and `tasks/graph.yaml` say passed: G1–G3 for every ticket, plus G4 where the eval gate applied, G5 for data-sensitive and regulated tickets, and the G6 stand-in (Opus diff review) for regulated ones.

| Ticket | Risk | Impl commits | Failure reports | Wall min | Tokens | Owner review min | Gates |
| --- | --- | ---: | ---: | ---: | --- | --- | --- |
| CC-001 | regulated | 4 | 1 | 34 | not recorded | | G1–G3, G5 |
| CC-002 | regulated | 3 | 4 | 16 | not recorded | | G1–G3, G5 |
| CC-003 | standard | 1 | - | 3 | not recorded | | G1–G3, G6 |
| CC-004 | regulated | 1 | 3 | 114 | not recorded | | G1–G3 |
| CC-005 | regulated | 1 | - | 11 | not recorded | | G1–G3, G5, G6 |
| CC-101 | standard | 1 | - | 0 | not recorded | | G1–G3 |
| CC-102 | standard | 3 | - | 55 | not recorded | | G1–G3 |
| CC-201 | data-sensitive | 1 | - | 65 | not recorded | | G1–G3, G5 |
| CC-007 | regulated | 1 | - | 5 | not recorded | | G1–G3, G4 (re-score) |
| CC-104 | data-sensitive | 2 | 1 | 32 | not recorded | | G1–G3, G5 |
| CC-202 | data-sensitive | 2 | 2 | 159 | not recorded | | G1–G3 |
| CC-203 | standard | 1 | - | 13 | not recorded | | G1–G3 |
| CC-204 | standard | 1 | - | 9 | not recorded | | G1–G3 |
| CC-205 | data-sensitive | 1 | - | 689 | not recorded | | G1–G3, G5 |
| CC-301 | standard | 1 | - | 14 | not recorded | | G1–G3 |
| CC-302 | standard | 1 | - | 40 | not recorded | | G1–G3 |
| CC-303 | standard | 1 | - | 47 | not recorded | | G1–G3 |
| CC-304 | standard | 1 | - | 30 | not recorded | | G1–G3 |
| CC-305 | standard | 1 | - | 7 | not recorded | | G1–G3 |
| CC-306 | regulated | 1 | - | 19 | not recorded | | G1–G3 |
| CC-307 | regulated | 1 | - | 9 | not recorded | | G1–G3 |
| CC-401 | data-sensitive | 1 | - | 8 | not recorded | | G1–G3 |
| CC-402 | data-sensitive | 1 | - | 7 | not recorded | | G1–G3 |
| CC-501 | data-sensitive | 1 | - | 7 | not recorded | | G1–G3 |
| CC-502 | data-sensitive | 1 | - | 30 | not recorded | | G1–G3, G5 |
| CC-503 | data-sensitive | 2 | - | 8 | not recorded | | G1–G3 |
| CC-505 | standard | 1 | - | 41 | not recorded | | G1–G3 |
| CC-601 | standard | 3 | 1 | 8 | not recorded | | G1–G3 |
| CC-602 | standard | 2 | 1 | 8 | not recorded | | G1–G3 |
| CC-701 | data-sensitive | 2 | 1 | 12 | not recorded | | G1–G3, G5 |
| CC-702 | data-sensitive | 1 | 1 | 30 | not recorded | | G1–G3, G5 |
| CC-703 | standard | 1 | - | 27 | not recorded | | G1–G3 |
| CC-704 | data-sensitive | 1 | - | 30 | not recorded | | G1–G3, G5 |
| CC-705 | regulated | 1 | - | 158 | not recorded | | G1–G3, G5, G6 stand-in |
| CC-709 | regulated | 1 | 1 | 28 | not recorded | | G1–G3, G5, G6 stand-in |
| CC-901 | standard | 1 | - | 10 | not recorded | | G1–G3 |
| CC-902 | regulated | 1 | - | 34 | not recorded | | G1–G3, G4, G5, G6 stand-in |
| CC-905 | regulated | 2 | - | 139 | not recorded | | G1–G3, G4, G5, G6 |

## What the numbers say so far

- Most standard tickets merged on the first implementation commit in under an hour. The ones with retries are CC-002, CC-004, CC-202 and CC-601.
- Regulated and data-sensitive tickets cost more attempts and more wall time, as the plan expected: CC-004 (3 failure reports), CC-002 (4), CC-202 (2), CC-701 and CC-709 (a G5 retry each).
- The Gate B drill on 2026-10-11 (a seeded regression, one attempt and one fix from the failure report alone) is the evidence for the "worker fixes it from the report" check.

## Items to fill in by hand

- Tokens per ticket.
- Owner review minutes per ticket, starting with the regulated ones: CC-306, CC-307, CC-709, CC-705, CC-902, CC-905 and CC-007.
