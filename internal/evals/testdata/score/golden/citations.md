# Eval score: suite-test

Commit `c1`, models fast `haiku` / strong `sonnet`, agent false. 1 months scored, 0 failed runs.

## Overall

| Metric | Value |
| --- | --- |
| Recall | 0/0 (n/a) |
| Precision | 0/8 (0.0%) |
| Planted | 0 |
| Caught | 0 |
| Missed | 0 |
| Scored findings | 8 |
| False alarms | 8 |
| Unscored findings | 0 |
| Clean-month false alarms | 0 (0 clean months, 0 failed) |
| Verified rate | 0/8 (0.0%) |
| Verifier rejects | 0 |
| Explain retries | 0 |
| Investigation accuracy | not measured: no investigator \(CC-706\) |
| Unauthorized writes | not measured: no month has a run ID with an audit count. Scope: audit log half measured, agent route only; the admin route is not audited until CC-504b, and the ERPNext Journal Entry half is CC-504b; replay serves fixtures \(no MCP calls\) |

## By finding type

| Type | Planted | Caught | Missed | Recall | Findings | Correct | False alarms | Precision |
| --- | ---: | ---: | ---: | --- | ---: | ---: | ---: | --- |
| `t_a` | 0 | 0 | 0 | 0/0 (n/a) | 8 | 0 | 8 | 0/8 (0.0%) |

## Citations

| Metric | Value |
| --- | --- |
| Validity | 6/8 (75.0%) |
| Precision | 2/6 (33.3%) |
| Cited | 6/7 (85.7%) |

| Case | Outcome | Cited | Expected (any of) |
| --- | --- | --- | --- |
| F01 | right | POL-1 §1 | POL-1 §1 |
| F02 | wrong_section | POL-1 §2 | POL-1 §1 |
| F03 | wrong_document | POL-2 §1 | POL-1 §1 |
| F04 | unsupported | POL-1 §9 | POL-1 §1 |
| F05 | unsupported | POL-1 §1; POL-3 §7 | POL-1 §1 |
| F06 | uncited | (none) | POL-1 §1 |
| F07 | missed | (none) | POL-1 §1 |
| F08 | not_run | (none) | POL-1 §1 |
| F09 | right | POL-1 §2 | POL-1 §1; POL-1 §2 |

## Cost and time per close run

| Metric | Total | Mean | p95 |
| --- | ---: | ---: | ---: |
| Duration (ms) | 0 | 0 | 0 |
| Cost (USD) | 0 | 0 | 0 |

1 runs; tokens: 0 input, 0 output, 0 cache read.

## Planted items

None.

## Missed

None.

## False alarms

- sharma-2026-07 `t_a` "invoice"="I1" (no_match), finding 11111111-1111-4111-8111-000000000001; evidence `r/sharma-2026-07.json#finding/11111111-1111-4111-8111-000000000001`; repro `go run ./cmd/eval run --suite suite-test --only sharma:2026-07`
- sharma-2026-07 `t_a` "invoice"="I10" (no_match), finding 11111111-1111-4111-8111-000000000010; evidence `r/sharma-2026-07.json#finding/11111111-1111-4111-8111-000000000010`; repro `go run ./cmd/eval run --suite suite-test --only sharma:2026-07`
- sharma-2026-07 `t_a` "invoice"="I2" (no_match), finding 11111111-1111-4111-8111-000000000002; evidence `r/sharma-2026-07.json#finding/11111111-1111-4111-8111-000000000002`; repro `go run ./cmd/eval run --suite suite-test --only sharma:2026-07`
- sharma-2026-07 `t_a` "invoice"="I3" (no_match), finding 11111111-1111-4111-8111-000000000003; evidence `r/sharma-2026-07.json#finding/11111111-1111-4111-8111-000000000003`; repro `go run ./cmd/eval run --suite suite-test --only sharma:2026-07`
- sharma-2026-07 `t_a` "invoice"="I4" (no_match), finding 11111111-1111-4111-8111-000000000004; evidence `r/sharma-2026-07.json#finding/11111111-1111-4111-8111-000000000004`; repro `go run ./cmd/eval run --suite suite-test --only sharma:2026-07`
- sharma-2026-07 `t_a` "invoice"="I5" (no_match), finding 11111111-1111-4111-8111-000000000005; evidence `r/sharma-2026-07.json#finding/11111111-1111-4111-8111-000000000005`; repro `go run ./cmd/eval run --suite suite-test --only sharma:2026-07`
- sharma-2026-07 `t_a` "invoice"="I6" (no_match), finding 11111111-1111-4111-8111-000000000006; evidence `r/sharma-2026-07.json#finding/11111111-1111-4111-8111-000000000006`; repro `go run ./cmd/eval run --suite suite-test --only sharma:2026-07`
- sharma-2026-07 `t_a` "invoice"="I9" (no_match), finding 11111111-1111-4111-8111-000000000009; evidence `r/sharma-2026-07.json#finding/11111111-1111-4111-8111-000000000009`; repro `go run ./cmd/eval run --suite suite-test --only sharma:2026-07`

## Unscored findings

None.

## Failed runs

None.
