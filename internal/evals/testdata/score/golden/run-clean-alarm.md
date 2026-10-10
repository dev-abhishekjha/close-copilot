# Eval score: suite-test

Commit `dev-abc123`, models fast `haiku` / strong `sonnet`, agent false. 2 months scored, 0 failed runs.

## Overall

| Metric | Value |
| --- | --- |
| Recall | 3/3 (100.0%) |
| Precision | 3/4 (75.0%) |
| Planted | 3 |
| Caught | 3 |
| Missed | 0 |
| Scored findings | 4 |
| False alarms | 1 |
| Unscored findings | 1 |
| Clean-month false alarms | 1 (1 clean months, 0 failed) |
| Verified rate | 2/5 (40.0%) |
| Investigation accuracy | not measured: no investigator \(CC-706\) |
| Unauthorized writes | not measured: audit decorator \(CC-504a\) and ERPNext external-ID check not built |

## By finding type

| Type | Planted | Caught | Missed | Recall | Findings | Correct | False alarms | Precision |
| --- | ---: | ---: | ---: | --- | ---: | ---: | ---: | --- |
| `unrecorded_bank_charge` | 3 | 3 | 0 | 3/3 (100.0%) | 4 | 3 | 1 | 3/4 (75.0%) |

## Cost and time per close run

| Metric | Total | Mean | p95 |
| --- | ---: | ---: | ---: |
| Duration (ms) | 2800 | 1400 | 1500 |
| Cost (USD) | 0.75 | 0.375 | 0.5 |

2 runs; tokens: 0 input, 0 output, 0 cache read.

## Planted items

| Item | Type | Keys | Outcome | Finding | Evidence |
| --- | --- | --- | --- | --- | --- |
| sharma-2026-09/E01 | `unrecorded_bank_charge` | "bank\_txn\_id"="BT-0901" | caught | 11111111-1111-4111-8111-000000000001 | - |
| sharma-2026-09/E02 | `unrecorded_bank_charge` | "bank\_txn\_id"="BT-0902" | caught | 11111111-1111-4111-8111-000000000002 | - |
| sharma-2026-09/E03 | `unrecorded_bank_charge` | "bank\_txn\_id"="BT-0903" | caught | 11111111-1111-4111-8111-000000000003 | - |

## Missed

None.

## False alarms

- sharma-2026-08 `unrecorded_bank_charge` "account"="HDFC Current 0001 - STPL"&"bank\_txn\_id"="BT-0801" (clean_month), finding 11111111-1111-4111-8111-000000000008; evidence `testdata/score/run-clean-alarm/sharma-2026-08.json#finding/11111111-1111-4111-8111-000000000008`; repro `go run ./cmd/eval run --suite suite-test --only sharma:2026-08`

## Investigation cases

- sharma-2026-09/X01 "bank\_txn\_id"="BT-0960": not_surfaced

## Unscored findings

- sharma-2026-08 `variance` "account"="Bank Charges - STPL"&"month"="2026-08", finding 11111111-1111-4111-8111-000000000009

## Failed runs

None.
