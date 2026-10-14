# ADR 0004: Deterministic checks first, an LLM only to explain and investigate

- Status: accepted
- Date: 2026-10-14
- Decides: what finds a problem and what a model is allowed to do with it (CC-601 to CC-607, CC-704, CC-705)

## Context

Most of a month-end close is mechanical comparison. Bank lines are matched to vouchers, purchase bills to the GSTR-2B, this month's recurring bills to earlier months', and invoice lines to the capitalisation, prepaid and cut-off rules. These comparisons have right answers that an accountant can check. A model reading the ledger may find the same problems, but its arithmetic and recall vary from run to run, and its findings can't be regression-tested cheaply.

At the same time, a list of mismatches isn't a close. The accountant needs to know why each finding matters, what the evidence shows, which policy clause applies and what entry would fix it. Writing that text well is what a language model is good at.

## Options

1. **A model reads the books and finds the problems.** It is flexible and can spot error types nobody wrote a rule for. It lost because a finding's existence would depend on a sampled model output, so recall couldn't be measured against planted errors without a model in every CI run. The numbers it reports would have nothing deterministic to be checked against, and every close would pay for model calls over the whole ledger.
2. **Rules only, no model.** It is cheap, exact and repeatable. It lost because it leaves the accountant to read every snapshot and policy clause and to draft every entry. It also has no way to work through a bank line the rules can't classify, such as a gateway settlement paid out net of fees.
3. **Deterministic checks produce every finding. A model explains each one and a plain-Go verifier checks the explanation. A bounded model loop investigates only what the rules couldn't classify (chosen).**

## Decision

- `internal/checks` holds the checks: bank reconciliation, duplicate payments, GSTR-2B matching, missing accruals, policy rules, variance and suspicious instruction text. They are plain Go over MCP-backed readers, and every finding references stored tool-result snapshots.
- A model never creates a finding. The explainer (`internal/agent/explain.go`) gets one finding at a time and answers only through `emit_explanation`: the explanation, a suggested action, the amounts used, citations and an optional balanced entry.
- The verifier (`internal/agent/verify.go`) is code, not a model. Every stated amount, cited amount and journal line must trace to the evidence within ₹1, and every citation must name a passage the model was shown. An explanation that fails is retried with the violations, and after three attempts the finding goes to `needs_review`. No entry can be proposed for an unverified finding.
- The investigator ([ADR 0005](0005-deterministic-workflow-bounded-agent.md)) is the only place a model chooses tool calls, and only for unmatched bank lines.

## Consequences

- **Recall is bounded by the checks.** An error type no check covers is never found. A 100% Tier 1 score shows the checks find the error types this project planted, not that they cover a real company's close.
- **Detection is gated without a model.** Tier 1 replays the recorded tool responses with no model and gates every relevant pull request ([ADR 0006](0006-count-based-eval-gates.md)). The committed baseline is recall 40/40 and precision 66/66 on suite-v1, and 3/3 and 8/8 on suite-skeleton.
- **Model cost is per finding, not per ledger.** On suite-skeleton a close run costs about 0.003 USD with the fast model ([docs/benchmarks.md](../docs/benchmarks.md), CC-1104).
- **Explanation quality is still variable.** The Tier 2 verified rate moved between 0 and 3 of 8 on identical code, because the explainer flagged its own findings `needs_review`. The verifier proves that numbers and references are grounded. It doesn't prove that the story is right.
- **Separation of roles in the build.** The data engineer plants the errors, the implementer writes the detectors and the eval engineer writes the scorer, so no agent grades its own work (CLAUDE.md).
- **What would change this decision:** evidence from real closes that many material problems fall outside any check's type. That would argue for a wider investigator scope or new checks, not for letting a model create findings unverified.
