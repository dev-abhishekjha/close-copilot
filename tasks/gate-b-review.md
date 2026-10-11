# Gate B: owner review of the regulated Phase 1 tickets

Drafted 2026-10-11 from the specs, merge commits and reviewer reports. Each entry says what the ticket does, what to read, and what the history does not prove. Sign off in the last column or send a ticket back.

| Ticket | What it is | Read this | Signed off |
| --- | --- | --- | --- |
| CC-306 | Planter and ground truth | `internal/seed/plant.go`, `truth.go`, `evals/schema/ground_truth.schema.json` | |
| CC-307 | Suite CLI and reset | `cmd/seed/main.go`, `evals/scenarios/suite-skeleton.yaml` | |
| CC-709 | Runtime data plane (steps, artifacts, resume) | `migrations/0004_steps.sql`, `internal/store/steps.go`, `artifacts.go` | |
| CC-705 | Verifier | `internal/agent/verify.go`, `verify_amounts.go`, `fault.go` | |
| CC-902 | Scoring | `internal/evals/score.go` | |
| CC-905 | Fixtures and the CI eval gate | `internal/evals/fixtures.go`, `baseline.go`, `.github/workflows/eval.yml` | |
| CC-007 | Clean-month scoring fix | `internal/evals/score.go` (control-month branch) | |

## CC-306 · Planter and ground truth

- **Does:** applies planted errors to the right views of the true world (Phase 1: only `unrecorded_bank_charge`) and writes the ground truth the scorer reads. Gateway settlements are recorded as investigation items.
- **Evidence it works:** the seed run on 2026-10-11 produced 3 planted charges in `sharma-2026-09` and a clean `sharma-2026-08`; the live agent found all 3 (`BNK-20260904-001`, `BNK-20260910-003`, `BNK-20260919-001`); the scorer matched every key.
- **Check:** the three planted charges are real bank lines (`SMS/ACCT CHARGES INCL GST`, ₹1,180) with no book entry. The ground truth is yours to approve.
- **Not proven by the history:** the merge commit does not record the G5 or G6 outcome. Treat the review as still owed.

## CC-307 · Suite CLI and reset

- **Does:** `make erp-reset` plus `cmd/seed all --suite suite-skeleton` rebuilds ERPNext, the bank CSVs and the ground truth. `suite-skeleton` is one company (sharma), September, 3 charges, August as the clean control, `small: true`.
- **Evidence it works:** the 2026-10-11 reset and seed succeeded end to end; ERPNext, Postgres and the CSVs lined up.
- **Check:** the scenario file (`evals/scenarios/suite-skeleton.yaml`) says what you intend the skeleton to be.
- **Not proven:** the spec requires two reset-and-seed runs to give byte-identical files. I ran the seed once and did not diff two runs. The merge commit does not record G5 or G6.

## CC-709 · Runtime data plane

- **Does:** migration 0004 (`run_steps`, content-addressed `artifacts`, `llm_calls`). Steps are idempotent per (run, kind, subject), so a resume skips finished steps; findings commit only when every evidence ref names a stored artifact.
- **Evidence it works:** the Gate B `kill -9` run resumed to `done` with 0 duplicate steps; `cmd/audit rebuild` reproduced 6 of 6 findings (exit 0).
- **Check:** the migration's Down block drops the audit tables, so it is for development only. `artifacts.run_id` and `produced_by` keep the first writer; provenance must come from `run_steps.output_refs`.
- **Passed:** G5 (attempt 2) and the orchestrator's G6 review.

## CC-705 · Verifier

- **Does:** plain Go, not a model. It checks every amount, citation and journal line of an explanation against the stored evidence (within ₹1), and rejects the rest. A rejected explanation is retried with the violations, up to three times, then goes to `needs_review`.
- **Evidence it works:** with `COPILOT_FAULT=corrupt_explanation` the verifier failed one explanation with `amount_not_in_evidence` (₹9,87,654.32), the explainer retried and attempt 2 passed. The skeleton now has 5 findings, so read the spec's "3 of 3 verified and 1 retry" as "all findings verified and 1 retry".
- **Check:** the fault flag can only add text to an explanation, so it can only cause a failure. It never edits actions, citations, proposals or anything written to ERPNext. The verifier proves numbers and references are grounded, not that the story is true.
- **Deferred items (in `tasks/hardening.md`):** amount-parsing edge cases (`5,000-6000-7000`), and one owner decision about recording evidence refs in the verdict.

## CC-902 · Scoring

- **Does:** grades a run against the ground truth: per-type recall and precision, clean-month false alarms, `--require` count rules (fail unless the denominator matches), `--compare` against a baseline, and `eval noise`.
- **Evidence it works:** it scored the Gate B runs and failed the seeded regression with a per-item report (recall 0/3, E01 to E03 named).
- **Check:** it refuses `evals/baseline.json` as an output path, so only your `baseline-update` commit can write the baseline.
- **Not done:** noise calibration (five explainer runs, then `eval noise`). The committed baseline is replay-tier only and has no noise figure, so the LLM-tier gates have no tolerance yet.

## CC-905 · Fixtures and the CI eval gate

- **Does:** records the books and evidence tool responses once, then replays them in CI with no ERPNext and no model. CI fails closed on a missing baseline or fixtures index.
- **Evidence it works:** `make eval-replay` and the live run both give recall 3/3 and precision 8/8; the local seeded regression was caught by `--compare`.
- **Check:** `evals/baseline.json` is the replay tier of `suite-skeleton`. The fixtures and ground truth were scanned for secrets and real identifiers and found clean.
- **Not proven:** the `eval (G4)` GitHub check has not been seen failing on a real pull request, only the same scorer locally. Also note the baseline commit message says "reviewed by the owner". You approved the commit, but this file is the first full summary you have been given, so read that line as "approved to commit".

## CC-007 · Clean-month scoring fix

- **Does:** a clean control month now matches its findings against its own truth. Gateway findings that the month's truth lists as investigation items are correct. An unmatched or duplicate finding is still a `clean_month` false alarm, so `--compare` still catches it.
- **Evidence it works:** re-scoring the Gate B run moved precision from 5/8 to 8/8 and clean-month false alarms from 3 to 0. The live run and the replay both agree. The security review and the Opus diff review both passed.
- **Check:** "the clean month raises nothing" now means "raises nothing outside its truth file". Investigation items match on `bank_txn_id` alone, so keep clean-month truth keys specific.
- **Deferred:** `checkMonths` does not reject planted items in a clean month (only the truth loader does).
