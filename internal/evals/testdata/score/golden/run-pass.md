# Eval score: suite-test

Commit `dev-abc123`, models fast `haiku` / strong `sonnet`, agent false. 2 months scored, 0 failed runs.

## Overall

| Metric | Value |
| --- | --- |
| Recall | 3/3 (100.0%) |
| Precision | 5/5 (100.0%) |
| Planted | 3 |
| Caught | 3 |
| Missed | 0 |
| Scored findings | 5 |
| False alarms | 0 |
| Unscored findings | 2 |
| Clean-month false alarms | 0 (1 clean months, 0 failed) |
| Verified rate | 2/7 (28.5%) |
| Verifier rejects | 0 |
| Explain retries | 0 |
| Investigation accuracy | not measured: no investigator \(CC-706\) |
| Unauthorized writes | not measured: audit decorator \(CC-504a\) and ERPNext external-ID check not built |

## By finding type

| Type | Planted | Caught | Missed | Recall | Findings | Correct | False alarms | Precision |
| --- | ---: | ---: | ---: | --- | ---: | ---: | ---: | --- |
| `unmatched_bank_line` | 0 | 0 | 0 | 0/0 (n/a) | 2 | 2 | 0 | 2/2 (100.0%) |
| `unrecorded_bank_charge` | 3 | 3 | 0 | 3/3 (100.0%) | 3 | 3 | 0 | 3/3 (100.0%) |

## Cost and time per close run

| Metric | Total | Mean | p95 |
| --- | ---: | ---: | ---: |
| Duration (ms) | 2700 | 1350 | 1500 |
| Cost (USD) | 0.512301 | 0.2561505 | 0.512301 |

2 runs; tokens: 1011 input, 206 output, 50 cache read.

## Planted items

| Item | Type | Keys | Outcome | Finding | Evidence |
| --- | --- | --- | --- | --- | --- |
| sharma-2026-09/E01 | `unrecorded_bank_charge` | "bank\_txn\_id"="BT-0901" | caught | 11111111-1111-4111-8111-000000000001 | - |
| sharma-2026-09/E02 | `unrecorded_bank_charge` | "bank\_txn\_id"="BT-0902" | caught | 11111111-1111-4111-8111-000000000002 | - |
| sharma-2026-09/E03 | `unrecorded_bank_charge` | "bank\_txn\_id"="BT-0903" | caught | 11111111-1111-4111-8111-000000000003 | - |

## Missed

None.

## False alarms

None.

## Investigation cases

- sharma-2026-09/X01 "bank\_txn\_id"="BT-0960": surfaced

## Unscored findings

- sharma-2026-09 `unmatched_ledger_entry` "gl\_entry"="ACC-GLE-0001", finding 11111111-1111-4111-8111-000000000007
- sharma-2026-09 `variance` "account"="Bank Charges - STPL"&"month"="2026-09", finding 11111111-1111-4111-8111-000000000006

## Failed runs

None.
