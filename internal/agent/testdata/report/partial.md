# Close report: sharma, 2026-09

| | |
|---|---|
| Run | `00000000-0000-4000-8000-000000000001` |
| Company | sharma |
| Month | 2026-09 |
| Outcome | partial |
| Note | explanations missing for 3 of 3 findings |
| Findings | 3 |
| Model usage | 0 input tokens, 0 output tokens, 0 cache-read tokens, USD 0 |
| Average cost per explained finding | none explained |

## Steps

| Step | Subject | Status | Note |
|---|---|---|---|
| router | close | done |  |
| check.bankrec | bankrec | done |  |
| retrieve | close | skipped | retrieval arrives with CC-806 |
| explain | 3 findings | 3 skipped | no explainer configured |
| verify | 3 findings | 3 skipped | no explanation to verify |
| investigate | close | skipped | investigation arrives with CC-706 |

## Findings

### 1. Unrecorded bank charge: NEFT CHARGES INCL GST (₹5.90)

- Finding: `00000000-0000-4000-8000-0000000000a1`
- Type: unrecorded_bank_charge
- Severity: low
- Amount: ₹5.90
- Status: open
- Evidence:
  - evidence/list_bank_lines: HDFC-20260915-C1 (snapshot `abababababab`)
- Action: book_entry
- Explanation: missing (skipped: no explainer configured)
- Verification: not verified (skipped: no explanation to verify)

### 2. Unrecorded bank charge: DEBIT CARD | ANNUAL 'FEE' # not a heading

- Finding: `00000000-0000-4000-8000-0000000000a2`
- Type: unrecorded_bank_charge
- Severity: medium
- Amount: ₹590.00
- Status: open
- Evidence:
  - evidence/list_bank_lines: HDFC-20260920-C2 (snapshot `abababababab`)
- Action: book_entry
- Explanation: missing (skipped: no explainer configured)
- Verification: not verified (skipped: no explanation to verify)

### 3. Unrecorded bank charge: SMS CHGS JUL-SEP 2026 (₹17.70)

- Finding: `00000000-0000-4000-8000-0000000000a3`
- Type: unrecorded_bank_charge
- Severity: low
- Amount: ₹17.70
- Status: open
- Evidence:
  - evidence/list_bank_lines: HDFC-20260930-C3 (snapshot `abababababab`)
- Action: book_entry
- Explanation: missing (skipped: no explainer configured)
- Verification: not verified (skipped: no explanation to verify)
