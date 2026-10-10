# Close report: sharma, 2026-09

| | |
|---|---|
| Run | `00000000-0000-4000-8000-000000000001` |
| Company | sharma |
| Month | 2026-09 |
| Outcome | done |
| Findings | 3 |
| Model usage | 2400 input tokens, 300 output tokens, 1200 cache-read tokens, USD 0.0042 |
| Average cost per explained finding | USD 0.0014 (run cost over 3 explained) |
| Verification | verified 2 of 3 explanations (pass rate 66%), 2 retried, 1 needs_review |
| Fault injection | COPILOT_FAULT=corrupt_explanation: one explanation was corrupted on purpose on its first attempt |

## Steps

| Step | Subject | Status | Note |
|---|---|---|---|
| router | close | done |  |
| check.bankrec | bankrec | done |  |
| retrieve | close | skipped | retrieval arrives with CC-806 |
| explain | 3 findings | 3 done |  |
| verify | 3 findings | 3 done | verification failed: amount_not_in_evidence after 3 explain attempts |
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
- Explanation: The bank debited ₹5.90 of NEFT charges on 15 Sep; the books have no matching entry.
- Citations: [POL-BANK §2.1]
- Proposed entry: 2026-09-15; Dr Bank Charges - STPL ₹5.90; Cr HDFC Current 0001 - STPL ₹5.90; NEFT charges per statement
- Verification: verified

### 2. Unrecorded bank charge: DEBIT CARD | ANNUAL 'FEE' # not a heading

- Finding: `00000000-0000-4000-8000-0000000000a2`
- Type: unrecorded_bank_charge
- Severity: medium
- Amount: ₹590.00
- Status: needs_review
- Evidence:
  - evidence/list_bank_lines: HDFC-20260920-C2 (snapshot `abababababab`)
- Action: book_entry
- Explanation: recorded (artifact `cccccccccccc`)
- Verification: needs review

### 3. Unrecorded bank charge: SMS CHGS JUL-SEP 2026 (₹17.70)

- Finding: `00000000-0000-4000-8000-0000000000a3`
- Type: unrecorded_bank_charge
- Severity: low
- Amount: ₹17.70
- Status: open
- Evidence:
  - evidence/list_bank_lines: HDFC-20260930-C3 (snapshot `abababababab`)
- Action: book_entry
- Explanation: recorded (artifact `cccccccccccc`)
- Verification: checked (verdict `dddddddddddd`)
