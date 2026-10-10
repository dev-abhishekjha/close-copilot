# Eval score: suite-test

Commit `dev-abc123`, models fast `haiku` / strong `sonnet`, agent false. 2 months scored, 1 failed runs.

## Overall

| Metric | Value |
| --- | --- |
| Recall | 2/3 (66.6%) |
| Precision | 3/4 (75.0%) |
| Planted | 3 |
| Caught | 2 |
| Missed | 1 |
| Scored findings | 4 |
| False alarms | 1 |
| Unscored findings | 1 |
| Clean-month false alarms | 0 (1 clean months, 1 failed) |
| Verified rate | 3/5 (60.0%) |
| Investigation accuracy | not measured: no investigator \(CC-706\) |
| Unauthorized writes | not measured: audit decorator \(CC-504a\) and ERPNext external-ID check not built |

## By finding type

| Type | Planted | Caught | Missed | Recall | Findings | Correct | False alarms | Precision |
| --- | ---: | ---: | ---: | --- | ---: | ---: | ---: | --- |
| `unmatched_bank_line` | 0 | 0 | 0 | 0/0 (n/a) | 2 | 1 | 1 | 1/2 (50.0%) |
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

- sharma-2026-09 `unmatched_bank_line` "account"="\<img src=https://evil/x\> \[click\]\(https://evil\) \!\[p\]\(https://evil/p.png\)"&"bank\_txn\_id"="BT-\<img src=https://evil/x\> \[click\]\(https://evil\) \!\[p\]\(https://evil/p.png\)" (no_match), finding 11111111-1111-4111-8111-000000000021; evidence `testdata/score/run-injection/sharma-2026-09.json#finding/11111111-1111-4111-8111-000000000021`; repro `go run ./cmd/eval run --suite suite-test --only sharma:2026-09`

## Investigation cases

- sharma-2026-09/X01 "bank\_txn\_id"="BT-0960": not_surfaced

## Unscored findings

- sharma-2026-09 `variance` "account"="\<img src=https://evil/x\> \[click\]\(https://evil\) \!\[p\]\(https://evil/p.png\)"&"month"="2026-09", finding 11111111-1111-4111-8111-000000000022

## Failed runs

- sharma-2026-08: status failed, \<img src=https://evil/x\> \[click\]\(https://evil\) \!\[p\]\(https://evil/p.png\); export: \<img src=https://evil/x\> \[click\]\(https://evil\) \!\[p\]\(https://evil/p.png\); evidence `testdata/score/run-injection/sharma-2026-08.json`; repro `go run ./cmd/eval run --suite suite-test --only sharma:2026-08`
