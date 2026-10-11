# Hardening tickets

These tickets come from the security reviews (G5) of CC-001, CC-002, CC-201 and CC-202. Each finding was deferred, and none of them is in `docs/implementation-tickets.md`. `/spec` takes its ticket text from this file in place of the tickets doc. The graph entries are in `tasks/graph.yaml`.

Status was checked against `main` at `c96642d` on 2026-10-09. Line numbers refer to that commit.

| Ticket | Phase | Owner | Risk | Blocks |
| --- | --- | --- | --- | --- |
| CC-004 Harden the merge gates, CI and guard hook | 0 | implementer | regulated | relying on branch protection |
| CC-005 Close the static analyzer gaps | 0 | implementer | regulated | |
| CC-104 Redact secrets in config | 1 | implementer | data-sensitive | CC-701, CC-904 |
| CC-205 Harden the Frappe client, probe and API-user script | 1 | integration-engineer | data-sensitive | CC-502 |
| CC-007 Score a clean month's expected and investigation findings as correct | 1 | eval-engineer | regulated | Gate B (clean month raises nothing), a usable `evals/baseline.json` |
| CC-006 Guard hook self-protection and path-expansion parity | 0 | implementer | regulated | |

CC-003 (module path and CODEOWNERS) is not a hardening ticket. Its approved spec is in `tmp/held-specs/CC-003.md`, and it's in the graph because CC-004 builds on it.

---

### CC-004 · Harden the merge gates, CI and guard hook

**Goal.** A pull request can't weaken the gates that judge it, and an owner approval covers exactly the commit the owner reviewed.

**Findings**
1. `.github/workflows/ci.yml:71-108` runs `go run ./gates/cmd/...` from the PR's own merge ref. A PR can edit the gate code that checks it. (CC-001 #1)
2. `gates/protected.go:61` accepts any `approved` label. Approving one commit also covers anything pushed after it. (CC-001 #3)
3. `gates/protected.go:41` only lowercases before matching. `docſ/x` and other non-ASCII or Unicode-fold paths slip past `docs/**`. (CC-001 N1)
4. `gates/git.go` `PinnedSpec` reads the spec from the branch when `main` doesn't have it, which is the normal case for a new ticket. `gates/declared.go:72-80` pins only `files`, `risk` and `approved_by`. (CC-001 N3/N4)
5. `ci.yml:13-15` has `cancel-in-progress: true`, and `:21` and `:50` skip `check` and `integration` on `labeled`/`unlabeled`. A label event cancels the real run, and the skipped run counts as green. (CC-001 N5)
6. `tasks/graph.yaml`, `specs/` and `.golangci.yml` are not protected. **Owner decision:** include this in the ticket or not. (CC-001 #10)
7. `.claude/hooks/guard-edit.sh:29` never checks that a path is under the project root. `$root/tmp/../../x` passes the `tmp/*` allowance at `:72`, nothing normalises `..`, and a spec glob that starts with `*` matches across `/` at `:95`. While a build is active, the hook also blocks every edit outside the repo, memory files included. It should allow paths outside the root, check only paths under it, and normalise first. (CC-202 note)

**Subtasks**
- CI checks out the base ref into a separate directory, builds `gates/cmd/*` from there and runs those binaries against the PR tree.
- Record the head SHA when `approved` is applied, for example from the `labeled` event payload. Fail `protected` when the PR head differs from that SHA.
- Reject any changed path that isn't plain ASCII, or fold it with NFKC and casefold before matching. Test with `docſ/`, full-width letters and `DOCS/`.
- Pin the spec from the branch's first `<ID>: spec` commit, not from its tip, and pin every front-matter field. Report each field the branch changed.
- Label events stop cancelling `check`: give them their own concurrency group, or have `protected` run as a separate workflow that never skips `check`.
- If the owner says yes to finding 6, add the paths to `gates.ProtectedPaths`, `.github/CODEOWNERS` and the list in `CLAUDE.md` (owner edit).
- `guard-edit.sh`: resolve `..`, exit 0 for paths outside the project root, and stop a leading `*` from matching across `/`.

**Acceptance**
- `go test ./gates/... -race -count=1` has a case for each finding.
- `go run ./gates/cmd/graph check tasks/graph.yaml` passes.
- A test PR that edits `gates/protected.go` to always pass is still judged by the base-ref gate.
- Hook tests: an edit to `tmp/../../x` is blocked, and an edit to a path outside the root is allowed.

**Files.** `gates/*.go`, `gates/cmd/**`, `gates/testdata/**`, `.github/**`, `.claude/hooks/guard-edit.sh`. Every one is protected, so the owner approves the spec.

---

### CC-005 · Close the static analyzer gaps

**Goal.** The analyzers catch the indirect forms of the patterns they already ban.

**Findings**
1. `gates/analyzers/egress/egress.go:34-96` catches only the package-level Get/Head/Post/PostForm functions, DefaultClient/DefaultTransport, `http.Client{}` and `new(http.Client)`. It misses `var c http.Client`, `http.Transport{}`, `httputil.ReverseProxy`, and a struct that embeds `http.Client`. (CC-002 S1)
2. `gates/analyzers/noerpimport/noerpimport.go:33-42` checks only direct imports. `internal/agent` can reach `internal/frappe` through any package in between. (CC-002 S3)
3. `gates/analyzers/admintoken/admintoken.go:112-118` accepts the allowed name in any `[]string` literal. It should accept only the literal passed to `cli.Main` in `cmd/mcp-books`. (CC-002 N2)

**Subtasks**
- egress: flag zero-value `http.Client` declarations, `http.Transport` literals and `new(http.Transport)`, any use of `httputil.ReverseProxy` or `NewSingleHostReverseProxy`, and struct fields that embed or hold `http.Client` or `http.Transport` by value. `internal/httpx` stays allowed.
- noerpimport: walk `pass.Pkg.Imports()` transitively, or use package facts, and report the import chain.
- admintoken: accept the literal only when it is the argument at the right position of a `cli.Main` call.
- Add a `testdata` case for every new rule, positive and negative.

**Acceptance**
- `go test ./gates/analyzers/... -race -count=1`
- `go run ./gates/cmd/lint ./...` stays green on `main`. Fix real hits under `internal/` only if the owner agrees; otherwise report them.

**Files.** `gates/analyzers/**` (protected).

---

### CC-104 · Redact secrets in config

**Goal.** Logging, printing or JSON-encoding a `config.Config` never shows a secret.

**Finding.** `internal/config/config.go:61-93` holds every token and key as a plain `string`, with no `LogValue`, `String`, `GoString` or `MarshalJSON`. A `slog.Info("config", "cfg", cfg)` leaks them all. (CC-002 N3)

**Subtasks**
- Add a `Secret` type in `internal/config`: a struct with an unexported field, so `string(s)` doesn't compile. Give it `Reveal() string`, and have `String`, `GoString`, `Format`, `LogValue`, `MarshalJSON` and `MarshalText` all print `[redacted]`, or `""` when it is empty.
- Change every secret field of `Config` to `Secret` and update the callers. Existing callers outside `internal/config` may get a one-line `.Reveal()`; list them in the PR summary.
- Give `Config` a `LogValue` that lists the non-secret fields.

**Acceptance**
- `go test ./internal/config/... -race -count=1`, with a test that formats a populated `Config` through `%v`, `%+v`, `%#v`, `slog` JSON and `json.Marshal` and finds no secret value in the output.
- `make check`

**Files.** `internal/config/**`, plus the call sites the compiler reports. The spec lists them.

---

### CC-205 · Harden the Frappe client, probe and API-user script

**Goal.** The Frappe client refuses unsafe queries and calls, every request has a deadline, and the probe and setup script handle the API secrets as carefully as the client does.

**Findings: internal/frappe** (CC-202 G5)
1. N1: `client_api.go:53-69` passes `Query.Fields` and `OrderBy` to Frappe without checking them. Frappe has a history of SQL injection through `fields` and `order_by`.
2. N5: `client_api.go:269-293` accepts GET on any dotted method path, `frappe.client.*` write methods included, and the client retries GETs.
3. N3: the repeat-page guard at `client_api.go:107` compares only the raw bytes of consecutive pages. The only total bound is `MaxPages` × `PageSize`, about 5M rows. The error text also wrongly blames "ignoring limit_start".
4. N4: `errors.go:55` makes `IsNotFound` true for any 404, whatever the `exc_type`.
5. N6: there is no overall deadline. Four attempts at 15 s plus three Retry-After waits of up to 30 s come to about 150 s for a call whose context has no deadline.
6. N9: `client.go:48` declares `type Secret string`, so `string(s)` works anywhere.

**Findings: cmd/probe and the setup script** (CC-201 G5)

7. `cmd/probe/client.go` is a second client with the old problems:
   - the secret is a plain `string` (`:31`) with no `GoString` or `Format`;
   - it has no `CheckRedirect` (`:38`), so it follows redirects across hosts and ports;
   - `isRefused` (`:79-85`) counts any 403 as a refusal.
8. `cmd/probe/perms.go:54-94` runs check (d), the System Settings PUT, even when check (c) wasn't refused.
9. `.env.example:13-14` lists `ERP_SEED_API_KEY` and `ERP_SEED_API_SECRET`, and `api-users.sh:155` says to append every `^ERP_` line to `.env`. Seed keys belong only in `tmp/erp-keys.env`. Also, `docs/erpnext-schema/README.md:9` uses `env $(... | xargs)`, which puts the secrets on argv.
10. `deploy/erpnext/api-users.sh` has no xtrace guard and writes `ERP_BASE_URL` into the keys file at `:135` without validating it at `:34`.

**Subtasks**
- `Query`: accept only `^[A-Za-z_][A-Za-z0-9_ ]*$` field names, optionally with a `` `tab<DocType>`. `` prefix, plus a closed list of aggregates if a current caller needs one. `OrderBy` is `<field> asc|desc`, possibly several of them comma-separated. Reject anything else before the request is sent.
- `Call`: refuse GET for `frappe.client.*` methods other than an allowlist of reads (`get`, `get_list`, `get_count`, `get_value`), and never retry a non-idempotent method.
- Pagination: also compare the first row's `name` with the previous page's, add a `MaxRows` cap (default 200k) and fix the error text.
- `IsNotFound`: also require `exc_type` `DoesNotExistError`. Audit the cleanup paths that call it, including `internal/seed`.
- When the caller's context has no deadline, the client sets a default one (`Options.Deadline`, default 60 s). Document it on `New`.
- `Secret`: make it a struct with an unexported field and `Reveal()`. Keep all current redaction methods and add `GoString` and `Format`.
- Probe: switch it to `internal/frappe` instead of its own client (preferred). Otherwise give it the same Secret, redirect and `PermissionError` rules. Run check (d) only when check (c) was refused.
- Setup script: `set +x` and refuse to run under xtrace, require `ERP_BASE_URL` to match `^https?://[A-Za-z0-9.-]+(:[0-9]+)?$`, stop advising that `ERP_SEED_*` be appended to `.env`, and change the README example to `set -a; . tmp/erp-keys.env; set +a`.

**Acceptance**
- `go test ./internal/frappe/... ./cmd/probe/... -race -count=1`, with a rejection test for each injection shape (`name; drop`, `name)`, `sleep(5)`, `name desc, (select ...)`).
- `go test -tags=integration ./internal/frappe/... -count=1`. This needs the ERPNext lock.
- `shellcheck deploy/erpnext/api-users.sh deploy/erpnext/lib.sh`
- `make check`

**Files.** `internal/frappe/**`, `cmd/probe/**`, `deploy/erpnext/api-users.sh`, `deploy/erpnext/lib.sh`, `.env.example`, `docs/erpnext-schema/README.md`. That last one is protected, so `human_review` is true.

---

### CC-006 · Guard hook self-protection and path-expansion parity

**Goal.** No worker can edit the files that enforce its own limits, and the hook judges exactly the path Claude Code writes to.

**Findings** (CC-004 G5, attempt 3; `tmp/reports/CC-004-attempt-3.json`)
1. **Self-protection.** A worker whose cwd is a worktree can edit the main checkout's `.claude/hooks/guard-edit.sh`, `.claude/settings*.json`, `.claude/agents/*`, `gates/`, `.github/` or `CLAUDE.md` by absolute path. During a worktree build the main checkout has no `tmp/current-task`, and no role pattern covers these paths. Hooks are re-read on every call, so such an edit takes effect at once and never appears in a PR diff. The gap predates CC-004.
2. **Path-expansion parity.** The hook neither trims whitespace nor expands `~`, as Claude Code's own path expansion does. When the tool input isn't rewritten before hooks run, `<wt>/tmp/current-task ` (trailing space) passes the `tmp/*` exemption, and `<main>/evals/baseline.json ` slips past the implementer rule.
3. **Flaky timing test.** `TestGuardHookFailsClosed` has 3 s and 5 s wall-clock limits. It failed once under `-race` while `make check` ran at the same time.

**Subtasks**
- When a role is given, block every `gates.ProtectedPaths` match unless the root's active spec declares it. Block `.claude/hooks/*` and `.claude/settings*.json` for every role, with no exception.
- Refuse a path with leading or trailing whitespace or a leading `~`, or expand them exactly as Claude Code does.
- Raise the timing limits to about 8 s, or skip the timing assertion under the race detector.
- `TestGuardHook` cases: from a worktree cwd against the main checkout's copies, and both whitespace and `~` forms.

**Acceptance**
- `go test ./gates/... -race -count=1`
- `bash -n .claude/hooks/guard-edit.sh`
- `make check`

**Files.** `.claude/hooks/guard-edit.sh`, `gates/hook_test.go` (both protected). Phase 0, implementer, regulated.

---

### CC-007 · Score a clean month's expected and investigation findings as correct

**Goal.** A clean control month stops counting a finding as a false alarm when that month's own ground truth lists the finding as an `expected` or `investigations` item, so `clean.false_alarms==0` can hold on `suite-skeleton` and a baseline can be taken.

**Finding** (Gate B run, 2026-10-11, `results/suite-skeleton/20261011T095154Z`)
- `suite-skeleton` clean month `sharma 2026-08` raised 3 `unmatched_bank_line` findings (`hint=gateway`, bank lines `BNK-20260811-002`, `BNK-20260818-002`, `BNK-20260825-002`). The scorer called all three `clean_month` false alarms: clean month false alarms 3, precision 5/8.
- The detector is right. CC-602 raises a `PG SETTL` line the books don't record as `unmatched_bank_line` with `hint=gateway`. The seeder is right: CC-306 records every gateway settlement batch under `investigations` (`expected_resolution: "gateway_settlement"`), and `ground_truth/sharma-2026-08.json` lists them as X01 to X03.
- The scorer is the odd one out: in `ScoreMonths` (`internal/evals/score.go`, the `if e.Control` branch) every scored finding of a control month is a false alarm, and the comment says the month's investigations "are not graded there". The same truth says they are expected, so the report contradicts its own truth file.
- Left as is, `--compare` lists every clean-month false alarm as a regression, so no baseline can pass on this suite.

**Subtasks**
- In the control-month branch, match the scored findings to the month's truth with `matchMonth` (a clean month has no planted items, so the targets are `expected` and `investigations`). A finding that matches a target counts as correct for its type; one that matches nothing stays a `clean_month` false alarm.
- Grade the clean month's investigations like any other month (`surfaced`, `not_surfaced`, `unmatchable`) so the report shows them.
- `Clean.Findings` stays the number of scored findings in control months; `Clean.FalseAlarms` counts only the unmatched ones.
- Unit tests: a clean month with a finding matching an investigation item (no false alarm, `Correct` up, precision up); the same finding with no matching item (false alarm); an `expected` item match; a duplicate finding for one investigation item (the second is a `clean_month` false alarm); an unscored finding type (unchanged).
- Keep every existing clean-month test and golden file that doesn't involve a truth item passing as is.

**Acceptance**
- `go test ./internal/evals/... -race -count=1`
- `make check`
- Human check: re-score the recorded run with `go run ./cmd/eval score results/suite-skeleton/20261011T095154Z --require 'clean.false_alarms==0' --require 'unrecorded_bank_charge.recall>=3/3'`; the three gateway lines show as surfaced investigations and the clean month has 0 false alarms.

**Files.** `internal/evals/score.go`, `internal/evals/score_test.go`, `internal/evals/testdata/score/**`. Phase 1, eval-engineer, regulated (scoring code, owner review).

**Not in this ticket.** The seeder, the checks and the Gate B checklist wording stay as they are. Taking `evals/baseline.json` is a separate `baseline-update` commit by the owner.

---

## Notes for later specs

These are requirements on tickets that already exist. When you spec one of them, copy its line into the spec's Notes and acceptance.

- **CC-701, CC-904:** build every HTTP client through `internal/httpx` (`option.WithHTTPClient(httpx.New(...))`, `otlptracehttp.WithHTTPClient(...)`). Read secrets only through CC-104's `config.Secret`.
- **CC-502:** the books tools validate `fields` and `order_by` against identifier and `asc|desc` allowlists at the tool boundary. CC-205's client check is the second line of defence, not the only one.
- **CC-1201:** the dev site has `delete_linked_ledger_entries=1`, a CC-102 follow-up the owner approved. That is a compliance-relevant setting. The deploy must set it to 0 and check that it is 0 on every non-dev site.
- **Any ticket that calls `internal/frappe` before CC-205 lands:** set a context deadline yourself.
- **CC-506 (from the CC-502 review):** bind `company` and every date and month argument of all seven books tools server-side, including `before_month` and `through_month`.
- **CC-710 (from the CC-502 review):** the books tools return raw GSTINs (`supplier_gstin`, `company_gstin`), party and customer names, and GL `party` and `against`. Pseudonymise them before any model call.
- **CC-706 and CC-1103 (from the CC-502 review):**
  - Books tool results carry untrusted ERPNext free text. These fields are capped at 500 runes, but not fenced: `remarks`, `description`, `against`, `bill_no`, `reference_no`, `supplier_name`, `customer_name` and `party_name`.
  - The investigator's tool-result projection must fence or drop these fields. Tool-error text is fixed per error class, so it isn't free text.
- **CC-703 (from CC-709 scoping and the CC-702 review):**
  - CC-709 builds the data plane and a test-workflow resume. CC-703 owns `cmd/agent resume <run_id>` and the real `kill -9` mid-run check for Gate B.
  - Put a run-level deadline on the workflow's ctx. `MCPBooks.GLEntries` can page up to 1000 times at 10 s per call.
- **CC-706 (from the CC-702 review):**
  - The registry turns every MCP tool error into fixed text (`MsgToolError`). This drops the books server's field-named input errors that would let the model correct its arguments. Decide whether to pass the server's fixed per-class text through.
  - Tool `Description`s from the servers go to the model unchanged. They are server-controlled text, which is acceptable only while the servers are first-party.
- **MCP SDK (from the CC-702 review):** the SDK's server-side output validation decodes generically, so amounts above 2^53 paise are rounded before they leave the books or evidence server. The client decode is exact. This is a theoretical limit (about ₹90 lakh crore); revisit if the servers ever stop validating output through the SDK.
- **CC-703 (from the CC-709 G5):**
  - Use the fenced signatures `Finish(ctx, step, ...)` and `FinishWithFindings(ctx, step, ...)`, and treat `ErrStaleAttempt` as "another attempt owns this step".
  - A genuine NUL or U+FFFD in ERPNext, bank or document text stops a snapshot with `ErrUnstorableContent`. Decide the reader-boundary policy (sanitise, or reject the row with a typed error) and test it.
  - `llm_calls.cost_usd` is `numeric(10,6)`: rounded to 1e-6 USD, and a cost of 10000 USD or more overflows. Reject negative or out-of-range costs in Go before the insert, and fix the "exact" wording on `store.LLMCall`. Optionally require `kind = 'tool_result'` for the evidence artifacts that `FinishWithFindings` checks.
- **CC-703 and CC-705 (from the CC-709 G5):** `artifacts.run_id` and `produced_by` keep the first writer, because artifacts are content-addressed. Never use them for tenancy or provenance; use `run_steps.output_refs`.
- **Next ticket that touches `internal/store/artifacts.go` (from CC-703):** move the cost range check (reject negative or ≥10000 USD) into `InsertLLMCall`. CC-703 checks it only on the workflow's own path.
- **CC-710 (from the CC-704 post-merge G5):** the explainer masks only the user message. ACCOUNTS (trial-balance names from ERPNext, `internal/agent/explain.go` system block) must go through the pseudonymiser too. Validate proposal accounts against the masked list, unmask them before storing, and test with an account name the fake masker rewrites.
- **CC-706 and CC-1103 (from the CC-704 G5):** ACCOUNTS is untrusted ERPNext text placed uncapped in the explainer's *system* prompt. Drop names longer than `maxAccountRunes` or outside a safe character set, cap the list (for example 500), and label or fence it as data. Also cap `Finding.Keys` with `capStrings` (`explain.go:435`).
- **CC-506 (from the CC-704 G5):** `store.SetFindingExplanation` updates `WHERE id = $1` only. Add run and company predicates (join `close_runs`), return `ErrNotFound` on a mismatch, and add a cross-run integration test.
- **CC-1001 (from the CC-704 G5):** model-written fields (explanation, action_note, remark, citations) reach the Markdown report escaped only by `oneLine`. Escape `[ ] ( ) ! < >` in `RenderReport` (`internal/agent/report.go`), and render `findings.explanation` as plain text in the UI, never as HTML or Markdown.
- **CC-705 (from the CC-704 G5):** the explainer checks cited amounts only as integers ≥ 0. The verifier must trace every `cited_amounts_paise` value and every proposal line amount back to EVIDENCE.
- **CC-806 (from the CC-704 G5):** `validator.documents` is never filled, so every citation is rejected today. Fill it from the same DOCUMENTS slice sent to the model and check `section` as well as `doc_id`.
- **Ticket that promotes `findings.proposal` to `journal_proposals` or `/mcp-admin` (from the CC-704 G5):** don't trust the embedded status, company, maker or accounts. Re-validate (balance, accounts, date, company from the run) inside `internal/approvals` and set maker and status server-side.
- **Web or long-lived explainer (from the CC-704 G5):** the ACCOUNTS cache is keyed by company and month only. Key it by run ID, or make a new explainer per run.
- **CC-905 (from the CC-902 G5):**
  - Run error text (`Result.Error`/`Reason`) is republished in `score.json` and `score.md`. Cap its length and redact URLs and DSNs, or print only the status plus a pointer to the result file.
  - `--compare` passes an empty but well-formed baseline (`items: {}`), and doesn't check that the baseline's agent and model IDs match the score. Make both fail.
  - Count requirements (`X.missed==0`, `X.false_alarms==0`) have no denominator guard and pass when type X has no planted items. Have the CI gate use `recall>=N/M`, or make `caught`/`missed` fail when `planted==0`.
  - `ScoreMonths` is exported but skips the control/clean and result-control checks, which live only in `LoadMonths`. Repeat them there.
  - The scorer shares `internal/evals` with the runner, so its independence from checks/agent/seed is enforced per file, not by the compiler. Consider moving it to `internal/evals/score` with a `go list -deps` test.
  - Wire the explainer into `eval run` (deferred from CC-901 and CC-902), using recorded fixtures.
- **Seeder and Phase 2 suites (from the CC-902 G5):** the scorer rejects ground truth where two items of the same mapped type have equal or subset keys (`internal/evals/truth.go` `overlappingTruth`). Example: a planted `gstr2b_wrong_period` on the same invoice as the "late" expected entry (`internal/seed/truth.go:128-150`) stops scoring with a load error. The domain data engineer must keep truth items distinct.
- **Owner decision, then the next ticket that touches `cmd/audit` or `internal/agent/verify.go` (from the CC-705 G6 review):** the verdict records `explanation_ref` but not the evidence or `accounts_ref` it checked, so re-pointing `findings.evidence` at another snapshot that still passes rebuilds as a match. Recommended: have `cmd/audit` compare `ProjectEvidence` with the EVIDENCE in the stored prompt (no spec change). The alternative, adding `evidence_refs` and `accounts_ref` to the verdict, changes CC-705's field list. In the same ticket, map a missing evidence or accounts artifact to exit 1, as `getNamed` does for explanations and verdicts.
- **Next ticket that touches `internal/agent/verify_amounts.go` (from the CC-705 G5):**
  - Should-fix, fails open: an ungrouped bare tail too large to parse (`5,000-99999999999999999999`) is dropped silently. Report it as `amount_unparseable`, as the prefixed path does, and add a test row.
  - Still unchecked: a third ungrouped number in a bare chain (`5,000-6000-7000`), an ungrouped number after `/` in bare text (`5,000/6000`), `₹500 to-600`, and bare amounts with no ₹ and no comma (`₹500 and 600`).
  - Add a unit subtest where `ReopenStep` fails without applying, then resume; assert the verify step's `input_refs` holds the verdict.
- **CC-706 (from the CC-705 G5):** tell the explainer prompt to put ₹ before every amount, so the verifier reads them all.
- **CC-905 and CC-906 (from the CC-705 G5):** count verifier false rejects (for example "₹590 to 3 vendors" reads ₹3) in the eval results, and revisit the extractor only if the retry rate shows it.
- **Ticket that promotes `findings.proposal` (from the CC-705 G6 review):** after three failed verifications the rejected explanation and proposal stay on the finding with `verified=false` and status `needs_review`. Approvals must refuse any proposal whose finding is not verified.
- **Schema doc (from the CC-705 G6 review):** `store.Begin` now appends a restarted step's `output_refs` to its `input_refs` for every step kind, so `input_refs` also holds earlier attempts' outputs. Only `storedFault` and `cmd/audit` read it today; record this in the living schema doc.
- **Seeder and truth files, then the next ticket that touches `internal/evals/score.go` (from the CC-007 G5 and G6 reviews):**
  - `checkMonths` doesn't reject planted items in a clean month (only `LoadGroundTruth` does, `internal/evals/truth.go`). Add `case m.Truth.Clean && len(m.Truth.Planted) > 0` as an error with a `TestScoreClean...` case, so a direct `ScoreMonths` caller can't hide a finding.
  - Investigation targets match on `bank_txn_id` alone, so any `unmatched_bank_line` on that bank line is accepted in a clean month whatever its hint. Keep clean-month truth keys specific; consider requiring `hint=gateway` when `expected_resolution` is `gateway_settlement` (a seeder and truth change).
  - Reword the `CompareCleanFalseAlarm` comment at `internal/evals/baseline.go:326` to "an unmatched or duplicate clean-month finding".
  - The next `baseline-update` commit for `suite-skeleton` will gain the clean-month X01 to X03 investigation rows and a higher `unmatched_bank_line` precision.
- **Before the public demo or any non-synthetic data, a CC-710 follow-up ticket (from the CC-710 G5; all four become blocking then):**
  - `cmd/eval/main.go:193-199` builds an `LLMExplainer` without `NewPseudonymiser`, so tier-2 eval prompts and their stored artifacts go out unmasked. Build the same Redactor there, with a fail-closed `PSEUDONYM_KEY` check whenever the agent is on.
  - A nil `NewPseudonymiser` falls back to identity with one warning (`explain.go:186-196`), which fails open for any new caller. Make it an error unless an explicit opt-out field is set (tests only), or add a gates lint for `LLMExplainer{}` literals without the factory.
  - The `\b` patterns miss an identifier glued to letters or digits (`NEFT29ABCPK1234F1Z5` leaks the GSTIN) and lower-case identifiers. Drop `\b`, match case-insensitively with GSTIN checksum validation, and add glued and lower-case cases to `TestExplainPromptScan` (it uses the same regexes, so it cannot see this today).
  - Evidence strings are capped at 500 runes before masking (`explain.go:687-694`), so a GSTIN or PAN cut at the cap leaks. Mask before capping, or cut at a non-alphanumeric boundary.
  - Smaller: `NewRedactor` accepts an empty or short key (error when under 32 bytes) and `Redactor` needs a `LogValue()` returning only `Len()`; re-check `maxRemarkRunes` after unmasking; detect duplicate masked account names; normalise whitespace and punctuation in individual names.
- **CC-706 and CC-707 (from the CC-710 G5):** wrap `WrapRedacting` outermost, inside the recording wrapper, so stored prompts stay token-only. `Workflow.stepModel` does not apply it today, so investigator and router tool output is unmasked until those tickets.
- **Owner action (CC-710):** `cmd/agent` now refuses to explain unless `PSEUDONYM_KEY` is 32 hex bytes (generate with `openssl rand -hex 32`); `--no-explain` is exempt. Run the G4-tier2 model run at the milestone to compare explanation quality with masking on.

- **Next ticket that touches `internal/mcpkit/scope.go` (from the CC-506 G5):**
  - Should-fix: `NewScoper`, the registry and `drive` reject only an empty `MCP_SCOPE_KEY`. Refuse keys under 32 bytes (a short key lets one captured header be brute-forced offline into any company's scope), say `openssl rand -hex 32` in `.env.example`, and test it. Check the owner's local `.env` key length first, because this turns a working setup into a startup error.
  - Should-fix: `Verify` sets no upper bound on `Exp`; the 24h cap lives only in `mint-scope`. Reject `Exp - now` above a constant (24h plus the longest run timeout) and add a `TestVerifyRejects` subtest.
  - Note: write into the `scope.go` threat-model comment that `get_trial_balance` returns opening balances aggregated from before `From` (same company, so no tenant leak). Add a comment that no MCP call belongs in `Workflow.end` (it runs under `WithoutCancel(parent)`, which carries no scope). Cap the invalid-date echo in `internal/evidence/tools.go:104-113` with `%.40q`.
- **CC-706 (from the CC-506 diff review):** the registry turns every tool error into `MsgToolError`, so the model never sees the `out_of_scope` reason. Decide whether to pass that one fixed class through; it holds only the run's own window and a capped echo. The scope window ends the month after the run month, because `internal/checks/gstr2b.go` reads that far ahead; a new detector that reads further must widen it, and `TestScopedCloseRunAllChecks` fails when one does.
- **Next ticket that touches `internal/books/testdata/schemas` (from CC-506):** `get_account_history`'s description still says an empty `through_month` defaults to the current month; a scoped call defaults to the run's month.
- **Owner, before relying on `make test-int` (found in CC-506):** `TestKillResume` (fails at `check.gstr2b` ending failed) and `TestCloseCLI/SIGINT_fails_the_run_as_cancelled` in `cmd/agent` fail the same way on `main` at f3cecc1 without CC-506. The GSTR-2B check likely has a bug on the integration path; no ticket owns it yet.
- **Next ticket that adds a spec with shell acceptance lines:** quote any acceptance line containing `: ` (such as `grep -e '--- PASS: X'`), or the front matter does not parse and `gates/cmd/declared` fails. `/spec` and `ticket-reviewer` did not catch this in CC-506; add a parse check to G0.

- **CC-706 wiring ticket (from the CC-706 G5 and build):** `cmd/agent/main.go` never sets `Workflow.Investigator` or `Workflow.NewRedactor`, so production runs skip investigation (fails closed). Wire `agent.LLMInvestigator` and a `Redactor` with the fail-closed `PSEUDONYM_KEY` check, then measure the gateway success rate (G4-tier2). `suite-skeleton` as seeded has no `PG SETTL` line, so the baseline needs a month with one. `cmd/eval run` doesn't wire the investigator either.
- **Next ticket that touches `internal/agent/investigate.go` (from the CC-706 G5):**
  - Should-fix: add `trdnm` to `droppedFields`; cap `party`, `supplier`, `customer`, `invoice_no`, `inum` and `reference_name` at 140 runes and strip control characters (or fence them); extend `TestInvestigateProjection`.
  - The 30,000-token limit is checked after each response, so one call can overshoot by its input plus 1,024 output tokens. Pre-check or accept.
  - `noteIDs` accepts any top-level string up to 140 runes as an evidence ID; limit to ID keys.
  - `workflow_test.go:1594` holds a well-formed synthetic GSTIN; build it at runtime so GSTIN scans don't trip.
- **CC-709, `report.go`, `internal/evals` (from CC-706):** the investigator's tool results are appended to `finding.evidence`, so `cmd/audit` re-verification sees more evidence than the explainer did. `report.go` doesn't show `verify/investigate/<id>` rows. `countVerification` counts investigation rejects as `VerifierRejects`.
- **Verifier (from CC-706):** a proposal's total debit must equal one evidence amount, so a settlement covering several receipts fails verification and ends `needs_review`. The prompt books fee plus GST as one line because the verifier needs each amount in evidence.
- **`SetFindingStatus` (from the CC-706 G5):** no run or company predicate; add a scoped variant.
