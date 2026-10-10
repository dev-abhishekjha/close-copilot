# Eval score: suite-test

Commit `dev-abc123`, models fast `haiku` / strong `sonnet`, agent false. 2 months scored, 1 failed runs.

## Overall

| Metric | Value |
| --- | --- |
| Recall | 0/3 (0.0%) |
| Precision | 0/0 (n/a) |
| Planted | 3 |
| Caught | 0 |
| Missed | 3 |
| Scored findings | 0 |
| False alarms | 0 |
| Unscored findings | 0 |
| Clean-month false alarms | 0 (1 clean months, 0 failed) |
| Verified rate | 0/0 (n/a) |
| Verifier rejects | 0 |
| Explain retries | 0 |
| Investigation accuracy | not measured: no investigator \(CC-706\) |
| Unauthorized writes | not measured: audit decorator \(CC-504a\) and ERPNext external-ID check not built |

## By finding type

| Type | Planted | Caught | Missed | Recall | Findings | Correct | False alarms | Precision |
| --- | ---: | ---: | ---: | --- | ---: | ---: | ---: | --- |
| `unrecorded_bank_charge` | 3 | 0 | 3 | 0/3 (0.0%) | 0 | 0 | 0 | 0/0 (n/a) |

## Cost and time per close run

| Metric | Total | Mean | p95 |
| --- | ---: | ---: | ---: |
| Duration (ms) | 1200 | 1200 | 1200 |
| Cost (USD) | 0 | 0 | 0 |

1 runs; tokens: 0 input, 0 output, 0 cache read.

## Planted items

| Item | Type | Keys | Outcome | Finding | Evidence |
| --- | --- | --- | --- | --- | --- |
| sharma-2026-09/E01 | `unrecorded_bank_charge` | "bank\_txn\_id"="BT-0901" | missed (run\_failed) | - | testdata/score/truth/sharma-2026-09.json\#E01 |
| sharma-2026-09/E02 | `unrecorded_bank_charge` | "bank\_txn\_id"="BT-0902" | missed (run\_failed) | - | testdata/score/truth/sharma-2026-09.json\#E02 |
| sharma-2026-09/E03 | `unrecorded_bank_charge` | "bank\_txn\_id"="BT-0903" | missed (run\_failed) | - | testdata/score/truth/sharma-2026-09.json\#E03 |

## Missed

- sharma-2026-09/E01 `unrecorded_bank_charge` "bank\_txn\_id"="BT-0901": missed (run\_failed); evidence `testdata/score/truth/sharma-2026-09.json#E01`; repro `go run ./cmd/eval run --suite suite-test --only sharma:2026-09`
- sharma-2026-09/E02 `unrecorded_bank_charge` "bank\_txn\_id"="BT-0902": missed (run\_failed); evidence `testdata/score/truth/sharma-2026-09.json#E02`; repro `go run ./cmd/eval run --suite suite-test --only sharma:2026-09`
- sharma-2026-09/E03 `unrecorded_bank_charge` "bank\_txn\_id"="BT-0903": missed (run\_failed); evidence `testdata/score/truth/sharma-2026-09.json#E03`; repro `go run ./cmd/eval run --suite suite-test --only sharma:2026-09`

## False alarms

None.

## Investigation cases

- sharma-2026-09/X01 "bank\_txn\_id"="BT-0960": not_surfaced

## Unscored findings

None.

## Failed runs

- sharma-2026-09: status failed, -; evidence `manifest.json#sharma-2026-09`; repro `go run ./cmd/eval run --suite suite-test --only sharma:2026-09`
