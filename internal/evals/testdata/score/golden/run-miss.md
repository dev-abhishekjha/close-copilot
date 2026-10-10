# Eval score: suite-test

Commit `dev-abc123`, models fast `haiku` / strong `sonnet`, agent false. 2 months scored, 0 failed runs.

## Overall

| Metric | Value |
| --- | --- |
| Recall | 2/3 (66.6%) |
| Precision | 3/3 (100.0%) |
| Planted | 3 |
| Caught | 2 |
| Missed | 1 |
| Scored findings | 3 |
| False alarms | 0 |
| Unscored findings | 0 |
| Clean-month false alarms | 0 (1 clean months, 0 failed) |
| Verified rate | 1/3 (33.3%) |
| Investigation accuracy | not measured: no investigator \(CC-706\) |
| Unauthorized writes | not measured: audit decorator \(CC-504a\) and ERPNext external-ID check not built |

## By finding type

| Type | Planted | Caught | Missed | Recall | Findings | Correct | False alarms | Precision |
| --- | ---: | ---: | ---: | --- | ---: | ---: | ---: | --- |
| `unmatched_bank_line` | 0 | 0 | 0 | 0/0 (n/a) | 1 | 1 | 0 | 1/1 (100.0%) |
| `unrecorded_bank_charge` | 3 | 2 | 1 | 2/3 (66.6%) | 2 | 2 | 0 | 2/2 (100.0%) |

## Cost and time per close run

| Metric | Total | Mean | p95 |
| --- | ---: | ---: | ---: |
| Duration (ms) | 3000 | 1500 | 1800 |
| Cost (USD) | 0.4 | 0.2 | 0.4 |

2 runs; tokens: 0 input, 0 output, 0 cache read.

## Planted items

| Item | Type | Keys | Outcome | Finding | Evidence |
| --- | --- | --- | --- | --- | --- |
| sharma-2026-09/E01 | `unrecorded_bank_charge` | "bank\_txn\_id"="BT-0901" | caught | 11111111-1111-4111-8111-000000000001 | - |
| sharma-2026-09/E02 | `unrecorded_bank_charge` | "bank\_txn\_id"="BT-0902" | missed (no\_match) | - | testdata/score/truth/sharma-2026-09.json\#E02 |
| sharma-2026-09/E03 | `unrecorded_bank_charge` | "bank\_txn\_id"="BT-0903" | caught | 11111111-1111-4111-8111-000000000003 | - |

## Missed

- sharma-2026-09/E02 `unrecorded_bank_charge` "bank\_txn\_id"="BT-0902": missed (no\_match); evidence `testdata/score/truth/sharma-2026-09.json#E02`; repro `go run ./cmd/eval run --suite suite-test --only sharma:2026-09`

## False alarms

None.

## Investigation cases

- sharma-2026-09/X01 "bank\_txn\_id"="BT-0960": not_surfaced

## Unscored findings

None.

## Failed runs

None.
