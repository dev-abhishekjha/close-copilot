# Eval under injected faults

Suite `suite-skeleton`, replayed without a model. Faults `erp:error=503:rate=0.1`, seed 1. Written by `eval run --faults`; `evals/baseline.json` is untouched.

| Metric | Faults off | Faults on |
| --- | --- | --- |
| Recall | 100.0% (3/3) | 0.0% (0/3) |
| Precision | 100.0% (8/8) | n/a (0/0) |
| False alarms | 0 | 0 |
| Clean-month false alarms | 0 | 0 |
| Failed runs | 0 | 1 |

## Recall lost

- `sharma-2026-09/E01` (unrecorded_bank_charge) caught without faults, missed with them
- `sharma-2026-09/E02` (unrecorded_bank_charge) caught without faults, missed with them
- `sharma-2026-09/E03` (unrecorded_bank_charge) caught without faults, missed with them

## Runs that ended failed under faults

- sharma 2026-09: preflight: books are unreachable: trial_balance: HTTP 503: ERPNext unavailable: injected fault
