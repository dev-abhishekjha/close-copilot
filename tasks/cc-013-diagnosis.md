# CC-013 diagnosis: GSTR-2B false alarms on suite-v1

Baseline replay: `results/suite-v1/20261012T111528Z` (main checkout), recall 40/40, precision 59/81, 22 false alarms. 21 of them are GSTR-2B findings: 6 `gstr2b_wrong_period` (symptom 1) and 15 `gstr2b_missing_in_books` (symptom 2). The 22nd, `unrecorded_bank_charge` `BNK-20260904-001`, is out of scope.

Method note: the data engineer does not read `internal/checks` (role boundary: the planter must not see the detectors). What the check reports is taken from the spec's description of `internal/checks/gstr2b.go:213-226` and from the findings in the replay results, not from its code.

## Symptom 1: `gstr2b_wrong_period`, 6 false alarms, all one month off

### Proven

The truth builder filed each late invoice under the wrong month. A late invoice dated in month M is in M's books and only in M+1's GSTR-2B (`GSTR2BOptions.IsLate`). `BuildGroundTruth` built the `expected` items from `GSTR2B.Late`, the previous month's late invoices that the return being built holds, so each item landed in M+1. The check raises the finding in M, where the books hold an invoice that M's own return lacks.

What the check reports, from the worktree replay `tmp/cc-013-results/suite-v1/20261012T113118Z` (same fixtures, same findings as the baseline run):

| Company | Invoice (norm) | Books month M | Check in M | Check in M+1 | Old truth month | New truth month |
| --- | --- | --- | --- | --- | --- | --- |
| mehta | `LTS2026742` | 2026-07 | `gstr2b_wrong_period` | nothing | 2026-08 | 2026-07 |
| mehta | `FBR2026804` | 2026-08 | `gstr2b_wrong_period` | nothing | 2026-09 | 2026-08 |
| sharma | `VNT2026631` | 2026-06 (clean control) | `gstr2b_wrong_period` | nothing | 2026-07 | 2026-06 |
| sharma | `SWF2026735` | 2026-07 | `gstr2b_wrong_period` | nothing | 2026-08 | 2026-07 |
| sharma | `SWF2026813` | 2026-08 | `gstr2b_wrong_period` | nothing | 2026-09 | 2026-08 |
| sharma | `SWF2026815` | 2026-08 | `gstr2b_wrong_period` | nothing | 2026-09 | 2026-08 |
| sharma | `LDG2026803` | 2026-08 | nothing (masked, see symptom 2) | nothing | 2026-09 | 2026-08 |

No month M+1 has any finding, of any type, keyed on these invoices. The old truth also put three of sharma's May invoices (`RDK2026524`, `LKV2026526`, `DCC2026559`) in the clean control month 2026-06; the check never reports them in June either.

The check is right and the truth is wrong. In M's reconciliation the invoice is booked and claimed in M but absent from M's 2B: the input tax credit can't be taken in M, and that is the period mismatch the accountant has to act on. In M+1 the 2B entry matches an invoice already booked in M, so there is nothing to act on. The fix belongs in the truth builder, not in `internal/checks`.

Fix (`internal/seed/truth.go`, `wrongPeriodExpected`): the `expected` items now come from `GSTR2B.Deferred` of the month's own return. That list holds the registered purchases dated in M that were filed late. Keys come from M's books (`supplier_gstin`, `NormInvoiceNo(invoice)`). A deferred invoice that is not a registered purchase in M's books, or not dated in M, is an error, never dropped (planting never touches a late invoice: `monthPlant.free`). The M+1 entries are removed, not duplicated: in M+1 there is no mismatch to find.

Tests: `TestCC013LateInvoiceTruthInBooksMonth` (full-size sharma 2026-05..09 and mehta 2026-06..09 worlds) checks that every expected item is a deferred invoice in M's books dated M, that none is a previous month's late invoice, that the seven invoices above are filed under M, and that none is filed under M+1. It fails on the old builder (39 errors). `TestCC013SeedDeterministic` builds suite-v1's 2B and truth twice and compares the bytes.

Replay results scored against the new truth (`eval score --truth-dir evals/scenarios/suite-v1/ground_truth`): `gstr2b_wrong_period` false alarms go from 6 to 0 (6/6 correct), overall precision goes from 59/81 to 65/81, false alarms from 22 to 16, clean-month false alarms from 1 to 0, and recall stays 40/40.

### Inferred

- `LDG2026803` will be reported in sharma 2026-08 once the stale 2B rows are gone (symptom 2), because the stale `--small` August 2B happens to hold an `LDG/2026/0803` that matches it today. The new truth already lists it under 2026-08. This needs the re-record to confirm.
- The full suite has no invoice deferred out of 2026-09, the last evaluated month, so no item needs an October return. The `--small` suite does have one (mehta `LDG2026959` in `testdata/ground_truth-v1/mehta-2026-09.json`), and no October 2B is seeded. If a full-size run ever defers a September invoice, the check (going by the spec's description) would find it in no neighbouring return. It would then report something other than `gstr2b_wrong_period`. Worth a seeder follow-up (seed the M+1 return of the last month, or never make the last month's invoices late) if it ever happens.

### Options for the owner

- **(i) File the truth under the books month M (chosen: the owner approved it on 2026-10-12; code and truth files are applied).** Clean-control month sharma 2026-06 gets one `expected` item (`VNT2026631`). It keeps `clean: true` and no planted errors. The scorer accepts the finding, so clean-month false alarms drop to 0. But "clean" then means "no planted errors", not "no findings". The M+1 entries disappear (they matched nothing). The fixtures' findings don't change, but `eval run --replay` refuses fixtures recorded against different ground truth. So until the re-record, a replay from this branch fails its truth check. Score the existing results with `eval score --truth-dir` instead, as done here (the owner accepted this).
- **(ii) Never make the clean-control month's invoices late.** This is a seeder change in `BuildGSTR2B`/`IsLate` that would make the clean month truly finding-free. It changes the seeded 2B for 2026-06 and 2026-07, so it needs a re-seed and re-record, and on its own it fixes only 1 of the 6 false alarms. It is compatible with (i) and can follow later. Recommendation: (i) now; (ii) only if the owner wants "clean" to mean zero findings.

## Symptom 2: `gstr2b_missing_in_books`, 15 false alarms, all sharma 2026-08

### Proven

None of the candidate causes (a), (b) or (c) is it. The cause is stale rows in Postgres `gstr2b_entries`. A `--small` load of sharma's 2026-08 GSTR-2B left 16 rows behind. The later full-size load upserted its own 20 rows on top and deleted nothing. The fixture recorded the union: 36 rows.

- ERPNext holds the full books, not the `--small` ones (so not (a)). The recorded `list_purchase_invoices` (window 2026-07-01..2026-09-30) holds 24 August bills. They are exactly the full world's 24 August purchases, except one planted substitution (`CLD/2026/0831`). The `--small` August world has 16 purchases, and 14 of its invoice numbers are absent from ERPNext (for example ERPNext's August rent is `OMK/2026/0819`, the small world's is `OMK/2026/0808`). July likewise: 25 bills, all in the full world.
- The seeder's full return does not emit them (so not (b)). `seed evidence` (full) builds a 19-invoice August return. `data/external/sharma/2026-08/gstr2b.json`, the planted file `seed all` wrote and `load` read, holds 20 (19 − `LKV2026823` + `CLD2026831` + planted `LDG2026804`). None of the 15 false-alarm keys is in either. All 15 are in the `--small` August return (`seed evidence --small`), and so is a 16th row, `LDG2026803`.
- Postgres, queried read-only today: `gstr2b_entries` for sharma 2026-08 has 36 rows, while the file on disk has 20. Every other company-period matches its file exactly (mehta 04..09: 20/21/16/22/18/22; sharma 04..07, 09: 16/21/22/21/26). The 36 rows are the file's 20 plus the 16 `--small`-only invoices.
- The key isn't the cause (so not (c)). The fixture's stored `invoice_no_norm` matches the seed's normalisation for all 36 rows, and `internal/evidence` `TestNormInvoiceNoParityWithSeed` holds the two copies equal. The mismatches are different invoice numbers, not differently normalised ones. Bill date and posting date fall in the same month for all 75 bills in the window.
- The loader writes with `UpsertGSTR2BEntries` (`internal/store/evidence.go:200`, `INSERT ... ON CONFLICT (company_id, period, supplier_gstin, invoice_no_norm) DO UPDATE`). It never removes a period's rows that the new file lacks, so a full-size reload can't clear an earlier `--small` load.

`TestCC013FullReturnHasNoSmallWorldInvoices` pins the data side: the full August return has 19 invoices and none of the 15 keys, and the `--small` one has all 15.

### Inferred

- The `--small` August 2B most likely came from CC-304/CC-305 era test runs or a `seed all --small` against the shared stack. Project memory records that ERPNext once held sharma's 2026-08 and 2026-09 `--small` books. September's stale rows are gone (26 = 26), so a later September load or reset replaced them, but no reset cleared August's evidence rows. Which command wrote them can't be proven from what is left.
- No seeder or truth change fixes this, and the check is right to flag 2B rows with no booked invoice. The fix is operational plus a loader guard:
  1. Re-record follow-up: clear sharma 2026-08's rows (or the whole `gstr2b_entries`/`bank_lines` evidence for the suite), run `make erp-reset seed load SUITE=suite-v1`, then `make record-fixtures SUITE=suite-v1`. Expected result: 0 of these 15 false alarms, and `LDG2026803` reported and matched as expected in 2026-08.
  2. Hardening ticket (implementer or integration-engineer, not this role): make the GSTR-2B load replace a company-period's rows (delete then insert in one transaction) instead of upserting, so a stale `--small` load can't survive. The bank-line load probably needs the same review.

## Ground-truth change (owner approved 2026-10-12; applied)

The owner approved this change on 2026-10-12, as recorded in the spec, and the files are applied. They are produced by `go run ./cmd/seed plant --suite suite-v1 --maps <main checkout>/data/out`, which reproduces today's committed files byte for byte before the fix. Planted and investigation items are unchanged in every file; only `expected` moves:

| File | Before | After |
| --- | --- | --- |
| suite-v1 mehta-2026-07 | (none) | `27XKYCL8632Z1ZI/LTS2026742` |
| suite-v1 mehta-2026-08 | `LTS2026742` | `27OIDCF5719X1ZS/FBR2026804` |
| suite-v1 mehta-2026-09 | `FBR2026804` | (none) |
| suite-v1 sharma-2026-06 (clean) | `RDK2026524`, `LKV2026526`, `DCC2026559` | `27DYLCV3017V1Z0/VNT2026631` |
| suite-v1 sharma-2026-07 | `VNT2026631` | `29PHGCS2431D1ZS/SWF2026735` |
| suite-v1 sharma-2026-08 | `SWF2026735` | `SWF2026813`, `SWF2026815`, `29SYJCL1381E1ZL/LDG2026803` |
| suite-v1 sharma-2026-09 | `SWF2026813`, `SWF2026815`, `LDG2026803` | (none) |
| suite-skeleton sharma-2026-08 | `VNT2026729`, `TCH2026743` | (none) |
| suite-skeleton sharma-2026-09 | (none) | (none), unchanged |

The spec was widened to include the suite-skeleton file. The regenerated file is byte-equal to what `TestSkeletonUnchanged` builds, and that test passes. The two July invoices it drops are July's late filings, whose books month (July) is not in suite-skeleton. The skeleton eval runs `bankrec` only, so its score doesn't move. The suite-v1 truth change also changes the ground truth the replay checks against its recording, so `eval run --replay` refuses the current fixtures until the re-record. The owner accepted that.

## What remains

- Re-record suite-v1 (the follow-up): the new truth makes `eval run --replay` refuse today's fixtures until then. Clean sharma 2026-08's stale GSTR-2B rows first (symptom 2). Then `evals/baseline.json` in its own `baseline-update` commit.
- Loader hardening: replace a period's GSTR-2B rows on load (a standard ticket; not `internal/checks`).
- Out of scope and still open: `unrecorded_bank_charge` `BNK-20260904-001` (sharma 2026-09).
