# ADR 0006: Count-based eval gates

- Status: accepted
- Date: 2026-10-14
- Decides: how a change is judged against the evals before it merges (CC-902, CC-905, CC-016a)

## Context

Agents write every line of this repository ([ADR 0001](0001-build-with-local-claude-code.md)), so the eval gate (G4) is what stops a change that quietly breaks a detector. The suites are small. `suite-skeleton` has 3 planted errors and 8 findings. `suite-v1` has 40 planted errors over 7 close runs. The model-dependent numbers are noisy: the faithfulness judge moved by 1 to 2 of 12 between passes, and the Tier 2 verified rate moved between 0 and 3 of 8 on identical code ([docs/benchmarks.md](../docs/benchmarks.md), `tasks/tier2-verified-informational.md`).

## Options

1. **Percentage thresholds**, for example "recall at least 95%". This is the familiar form. It lost because with 3 planted errors one miss moves recall by 33 points, a threshold doesn't say which item regressed, and a shrinking denominator can pass unnoticed.
2. **A model-graded quality score as the gate.** It covers explanation quality. It lost because the judge's own noise is about the size of the effects it would gate. The judge is also the same model family as the strong model, so it may not be neutral. And every pull request would pay for model calls.
3. **Item-by-item counts compared with a committed baseline, with explicit denominators and zero-tolerance counts (chosen).**

## Decision

- `cmd/eval score` grades each run against the ground truth item by item: planted items caught or missed, false alarms, clean-month false alarms, and investigation items surfaced. It reports counts as `num/den`. `--compare evals/baseline.json` (`internal/evals/baseline.go`) fails on any of these:

  - a baseline item that was caught is now missed
  - a baseline item has disappeared from the truth
  - a clean month has any false alarm or a failed run
  - the baseline section is empty or missing for the tier
  - the agent flag or the models differ from the baseline's
  - verified findings fell by more than the recorded noise
  - on Tier 2, p95 duration or cost rose past the thresholds

  A finding the baseline doesn't have is listed but is not a failure.
- `--require` rules state the denominator, for example `unrecorded_bank_charge.recall>=3/3` and `clean.false_alarms==0`, and fail unless it matches. Unauthorized writes, out-of-scope calls, tenant leaks and invalid citations have zero tolerance.
- Tier 1 replays the recorded tool responses with no model and no ERPNext, and gates every pull request that touches checks, seed, evidence, books, store or evals (`.github/workflows/eval.yml`). Tier 2 runs the same replay with the explainer and verifier, nightly and on dispatch.
- The scorer and the baseline come from the base branch, never from the pull request, so a change can't loosen the yardstick that judges it. The scorer refuses to write `evals/baseline.json`. A baseline changes only in its own `baseline-update` commit, which the owner approves.
- Model-tier counts get a noise tolerance measured from repeated runs. The suite-skeleton Tier 2 verified rate has a baseline of 1/8 with a tolerance of 2, from six runs.

## Consequences

- **Precise failures.** A failing gate names the item (for example `sharma-2026-09/E02`), so a worker can fix it from the failure report alone. The seeded-regression drill at Gate B was fixed that way.
- **Cheap and deterministic.** Tier 1 needs only Postgres, and a suite-skeleton replay run takes tens of milliseconds.
- **Fixtures must follow the truth.** When the seeder or the ground truth changes, the fixtures have to be re-recorded from ERPNext, and the replay refuses fixtures recorded against different truth (CC-013).
- **Some numbers aren't gated yet.** The Tier 2 verified rate is informational until the CC-020 re-baseline. suite-v1 has no Tier 2 baseline, so explanation quality there isn't gated.
- **Counts say nothing about unseen error types.** A perfect count means the planted items were found. It doesn't mean real books would score the same.
- **What would change this decision:** suites large enough for percentages with confidence intervals to be meaningful, or a judge whose noise is measured to be well below the effects that matter.
