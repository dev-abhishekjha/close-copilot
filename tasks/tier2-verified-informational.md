# Tier 2 `verified_rate` is informational (temporary)

- **Decided:** 2026-10-13, by the owner.
- **Expires:** when the `baseline-update: suite-skeleton llm tier (6-run noise)` commit lands on `main`. Delete this file in that commit's follow-up, or at the latest the next time `/build` reads it and finds the commit.

## What changes for tickets

While this file exists, a G4-tier2 `FAIL compare: verified_drop ...` line does **not** fail a ticket. `/build` records the run's `verified_rate` in the merge note and moves on.

Still hard gates, unchanged: recall, precision, clean false alarms, failed runs, and every zero-tolerance item (unauthorized writes, out-of-scope calls, tenant leaks, invalid citations).

Any other `FAIL compare:` line (for example `recall_drop` or `precision_drop`) still fails the ticket. `go run ./cmd/eval score ... --compare` exits non-zero on `verified_drop`, so read the FAIL lines and ignore only that one; do not change `gates/**`, `internal/evals` or `evals/baseline.json` to get around it.

## Why

Same-day runs of one commit, `suite-skeleton`, agent on, `--replay`:

- Pre-CC-806 (`976bc6a` and earlier): verified 3, 2, 0, 0, 1, 1 of 8.
- CC-806 (`66c01e0`): verified 1, 3, 0, 0, 0 of 8, plus 0 with the original prompt.
- The baseline is 3/8 with a noise tolerance of 1. Recall, precision, clean false alarms and failed runs were identical in all of them.

`verified_rate` counts findings that are verified and not `needs_review`, so it follows the explainer's own `needs_review` self-flag, which flips between runs (7 or 8 of 8 findings flagged in the low runs). See the investigation ticket CC-020 in `tasks/hardening.md`.

## Ends with

The 6-run re-measure on `main` (owner runs the six commands; the orchestrator drafts the baseline diff with median, min/max and a tolerance derived from the spread, and the owner approves it).
