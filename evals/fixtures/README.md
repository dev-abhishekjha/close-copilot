# Recorded fixtures (CC-905)

CI scores every pull request without ERPNext. `eval run --record` runs a
suite live and saves every books and evidence tool response the close
workflow reads; `eval run --replay` serves those responses through the same
reader interfaces (`checks.BooksReader`, `checks.EvidenceReader`), so the
real workflow runs with no ERPNext, no MCP server and no MCP credential.
Postgres is still needed (runs, steps and snapshots are stored as usual).

## Layout

```
evals/fixtures/<suite>/
  index.json                                  every file below with its sha256, plus the
                                              suite file's and each ground-truth file's sha256,
                                              the recording build and time
  <company>-<month>/<tool>-<argshash>.json    {"tool": ..., "args": {...}, "result": ...}
```

`<tool>` is the MCP tool the reader method maps to (`get_trial_balance`,
`list_gl_entries`, `list_purchase_invoices`, `list_sales_invoices`,
`list_payments`, `get_account_history`, `list_recurring_suppliers`,
`list_bank_lines`, `list_gstr2b_entries`). `<argshash>` is the first 16 hex
characters of the sha256 of the canonical arguments (sorted keys, dates as
`YYYY-MM-DD`, months as `YYYY-MM`). Amounts are integer paise.

The `<suite>/` level departs from the ticket's `evals/fixtures/<company>-<month>/`
on purpose: the same company-month holds different data in different suites.

## Bootstrap order

1. `make erp-reset seed load SUITE=suite-skeleton` (the seeded ERPNext; take
   `tmp/erpnext.lock`)
2. `make record-fixtures SUITE=suite-skeleton`
3. commit `evals/fixtures/suite-skeleton/` (run gitleaks over it first)
4. the owner scores a replay and commits `evals/baseline.json` in its own
   `baseline-update` commit, from the candidate a `workflow_dispatch` run of
   the eval workflow uploads (artifact `eval-results`)

Locally, after `make up migrate`: `make eval-replay SUITE=suite-skeleton`
replays without a model and scores the run (against `evals/baseline.json`
when it exists).

## What a replay checks first

Before any month runs, the replay reads `index.json` and fails if:

- a fixture's sha256 differs from the index, a listed file is missing, or
  any file is not listed (an extra file is an error);
- the suite file or a recorded ground-truth file changed since recording:
  "fixtures are stale: re-record after seeding (make record-fixtures)";
- a month to run was not recorded.

A call with no fixture is an error naming the tool and its arguments; it
never falls through to a live reader (the replay readers hold none).

## Recording

- A live error aborts the recording: errors are never recorded, and the old
  fixtures stay in place. The new folder replaces `<suite>/` only once the
  whole recording succeeded.
- The same call returning different results within one month aborts the
  recording too (the replay would not be deterministic).
- Each month's trial balance is recorded even when the run doesn't read it,
  so a recording made with `--no-llm` still serves a Tier 2 replay (the
  explainer lists the month's accounts from it).
- Before the index is written, every file is scanned for secret-looking
  strings and for any GSTIN or PAN outside the synthetic allowlist derived
  from `config/companies/*.yaml` (`Profile.GSTIN`, `SupplierPAN`,
  `SupplierGSTIN`). CI also runs a pinned gitleaks over this folder.
- Fixtures carry the seeded, synthetic GSTINs, PANs and party names
  unmasked: they are tool responses, not model inputs. Pseudonymisation
  (CC-710) applies at the model boundary.

## Limits

- Replay hides an ERPNext behaviour change by design. The stale-hash check
  catches a changed suite or ground truth (a re-seed), not a new ERPNext
  version: re-record after upgrading ERPNext or changing a reader.
- The CI scorer is built from the base ref, so a pull request that changes
  `eval score`'s flags is judged by the old binary: land the flag in one
  pull request and its use in the workflow in the next.
- `TestFixtureSize` fails if this folder exceeds 5 MB.
