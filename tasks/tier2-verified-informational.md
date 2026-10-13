# Tier 2 `verified_rate` is informational (temporary)

- **Decided:** 2026-10-13, by the owner.
- **Expires:** at the CC-020 re-baseline, or 2026-11-13, whichever comes first.
- **Updated 2026-10-13:** the `baseline-update: suite-skeleton llm tier (6-run noise)` commit (`bf2f236`) landed, set `verified_rate` to 1/8 with a noise tolerance of 2, and did **not** end this file. The compare fails only when `baseline - actual > max(noise, 1)`, so with baseline 1 and tolerance 2 it cannot fail on `verified_rate`. It stays informational until CC-020 raises the level and re-baselines.

## What changes for tickets

While this file exists, a G4-tier2 `FAIL compare: verified_drop ...` line does **not** fail a ticket. `/build` records the run's `verified_rate` in the merge note and moves on.

Still hard gates, unchanged: recall, precision, clean false alarms, failed runs, items, investigations, and every zero-tolerance item (unauthorized writes, out-of-scope calls, tenant leaks, invalid citations).

Any other `FAIL compare:` line (for example `recall_drop` or `precision_drop`) still fails the ticket. `go run ./cmd/eval score ... --compare` exits non-zero on `verified_drop`, so read the FAIL lines and ignore only that one; do not change `gates/**`, `internal/evals` or `evals/baseline.json` to get around it.

## Why

Same-day runs of one commit, `suite-skeleton`, agent on, `--replay`:

- Pre-CC-806 (`976bc6a` and earlier): verified 3, 2, 0, 0, 1, 1 of 8.
- CC-806 (`66c01e0`): verified 1, 3, 0, 0, 0 of 8, plus 0 with the original prompt.
- The baseline is 3/8 with a noise tolerance of 1. Recall, precision, clean false alarms and failed runs were identical in all of them.

`verified_rate` counts findings that are verified and not `needs_review`, so it follows the explainer's own `needs_review` self-flag, which flips between runs (7 or 8 of 8 findings flagged in the low runs). See the investigation ticket CC-020 in `tasks/hardening.md`.

## Ends with

The CC-020 re-baseline (after its self-flag fix and a hard `--require verified_rate>=N/8` check), or 2026-11-13, whichever comes first. The six-run re-measure is done: 0, 0, 1, 1, 0, 2 of 8 (median 0.5).
