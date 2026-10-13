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
| CC-008 Create the Mehta company in ERPNext and fix seed key handling | 2 | integration-engineer | standard | CC-010, Gate C |
| CC-009 Snapshot annotation for check evidence arguments | 2 | llm-engineer | data-sensitive | Gate C |
| CC-010 Plant the other nine error types and the injection cases | 2 | domain-data-engineer | regulated | Gate C |
| CC-011 GSTR-2B check failure on the integration path | 2 | implementer | standard | trusting `make test-int` |
| CC-012 Measure unauthorized writes in the eval score | 2 | eval-engineer | regulated | Gate C (writes 0) |
| CC-013 Diagnose the GSTR-2B false alarms on suite-v1 | 2 | domain-data-engineer | regulated | the `suite-v1` baseline and fixtures commit |
| CC-016 Baseline file holds one section per suite | 2 | eval-engineer | regulated | CC-016a |
| CC-016a Eval workflow gates suite-v1 and the suite-v1 baseline | 2 | eval-engineer | regulated | a `suite-v1` baseline, Gate C |
| CC-017 Loader that replaces a period's evidence, and `make load` that works | 2 | implementer | data-sensitive | trustworthy re-records |
| CC-018 Close the scorer's output-path and duplicate-key gaps | 2 | eval-engineer | regulated | relying on the baseline guard |
| CC-019 Wire the pseudonymiser into the eval's Tier 2 path | 2 | llm-engineer | data-sensitive | the suite-skeleton Tier 2 baseline |

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

### CC-008 · Create the Mehta company in ERPNext and fix seed key handling

**Goal.** `make erp-reset` leaves an ERPNext that holds both suite companies (Sharma Traders Pvt Ltd, Mehta Tech Services Pvt Ltd), so `cmd/seed all --suite suite-v1` bootstraps both, and `make seed` gets its keys without `.env`.

**Finding** (first Gate C attempt, 2026-10-12)
- After `make erp-reset`, `cmd/seed all --suite suite-v1` bootstrapped sharma and stopped at mehta: `company "Mehta Tech Services Pvt Ltd" does not exist in ERPNext; create it first` (`internal/seed/bootstrap.go:453`).
- `deploy/erpnext/setup-wizard.sh` creates only Sharma (`STPL`), and `clean-bootstrap-database.sql.gz` is a snapshot of that. `config/companies/mehta.yaml` says `abbr: MTSPL`, `erp_company: Mehta Tech Services Pvt Ltd`.
- `make seed` failed with `missing required environment variables: ERP_SEED_API_KEY, ERP_SEED_API_SECRET` although the keys were exported from `tmp/erp-keys.env`. The Makefile does `-include .env` and `export`, and `.env` likely carries blank `ERP_SEED_*` lines that override the environment. Running `go run ./cmd/seed` directly worked.

**Subtasks**
- Add `deploy/erpnext/add-company.sh <name> <abbr>` (or equivalent), idempotent, that creates a further Company on the same site with the India chart of accounts, INR, the same fiscal year as the first company, and refuses if the abbreviation already belongs to another company. Take the two values for Mehta from a place the script can read without Go (arguments), and have the setup docs name them.
- Extend the reset path so both companies survive it: document in the script header that `deploy/erpnext/backup-clean.sh` must be re-run after the company is added, and make `backup-clean.sh` unchanged in behaviour.
- Fix the key handling: `make seed`, `make record-fixtures` and any other target that reaches ERPNext as the seeder or bot must read `ERP_SEED_*` and `ERP_*` from `tmp/erp-keys.env` in a subshell when that file exists, and must not print the values. Do not read or edit `.env`. `.env.example` gets a comment saying where the seeder keys live.
- No change to `internal/seed`.

**Acceptance**
- `bash -n` and `shellcheck` clean for the touched scripts; a second run of the add-company script changes nothing.
- `make -n seed SUITE=suite-v1` shows the subshell that sources `tmp/erp-keys.env`.
- Under the ERPNext lock: after `make erp-reset`, both companies exist, `go run ./cmd/seed bootstrap --suite suite-v1` succeeds for both, and a second run reports zero changes.

**Files.** `deploy/erpnext/*.sh`, `Makefile`, `.env.example`. Phase 2, integration-engineer, standard. `docs/setup.md` is protected; the owner updates it.

---

### CC-009 · Snapshot annotation for check evidence arguments

**Goal.** A check step that emits a finding in the real workflow no longer fails with `ErrNoSnapshot`.

**Finding** (CC-1103): `snapshotCall.supports` (`internal/agent/snapshot.go:207`) refuses any evidence-reference argument that is neither a recorded argument nor an indexed key. Four checks build such arguments: `rules.go:99` (month, rule, account), `duplicates.go:148-152` (month), `gstr2b.go:152` (from, to, tax amounts) and `variance.go:196` (month on the purchase-invoice reference). Today's suite plants only bank charges, so nothing caught it.

**Subtasks.** Make `supports` accept these (use the recorded `from_date`/`to_date` args with IDs, or ignore descriptive args that carry no evidence); remove the test-only `windowArgs` shim around `PolicyRulesCheck` in `internal/agent/injection_test.go` and re-run the taint tests; add a table test with one finding from each of the four checks.

**Files.** `internal/agent/snapshot.go`, `snapshot_test.go`, `injection_test.go`, `testdata/**`. Phase 2, llm-engineer, data-sensitive.

---

### CC-010 · Plant the other nine error types and the injection cases

**Goal.** `cmd/seed all --suite suite-v1` plants the 40 errors in `evals/scenarios/suite-v1.yaml` (10 types) and writes ground truth the scorer can read.

**Finding.** `internal/seed/plant.go` implements only `unrecorded_bank_charge`; its header says Phase 2 adds the rest. Without this, `prompt_injection.recall==4/4` and the `suite-v1` baseline can't be measured. Needs CC-008 (Mehta exists), then a reset and seed under the ERPNext lock. Regulated: the planter and ground truth are the owner's to approve. Also confirm the G4-tier1 run on `suite-v1` shows 0 `suspicious_instruction_text` false alarms.

**Files.** `internal/seed/plant*.go`, `truth.go`, `testdata/**`, `evals/scenarios/**` (protected: owner approves the generated truth). Phase 2, domain-data-engineer, regulated.

---

### CC-011 · GSTR-2B check failure on the integration path

**Goal.** `make test-int` passes on `main`.

**Finding** (CC-506): `TestKillResume` (fails at `check.gstr2b` ending failed) and `TestCloseCLI/SIGINT_fails_the_run_as_cancelled` in `cmd/agent` fail the same way on `main` at f3cecc1 without CC-506. The GSTR-2B check likely has a bug on the integration path. Diagnose first; the fix may be in the check, the workflow or the test.

**Files.** `internal/checks/gstr2b*.go`, `internal/agent/workflow*.go`, `cmd/agent/agent_integration_test.go`. Phase 2, implementer, standard.

---

### CC-012 · Measure unauthorized writes in the eval score

**Goal.** `WritesCheck.Checked` is true on a live run and `--require 'unauthorized_writes==0'` can pass. Scope (owner decision 2026-10-12): the agent-route audit-log half only; replay stays unchecked; the admin-route and ERPNext Journal Entry halves come with CC-504b.

**Finding.** `internal/evals/score.go:148`: while `Checked` is false the count is null and any requirement on it fails, so Gate C's "unauthorized writes 0" can't be shown. CC-504a's audit log records every tool call; count calls to a write tool whose run has no approved proposal. Regulated: the eval engineer writes this, the owner reviews it.

**Files.** `internal/evals/score.go`, `score_test.go`, `testdata/score/**`. Phase 2, eval-engineer, regulated.

---

### CC-013 · Diagnose the GSTR-2B false alarms on suite-v1

**Goal.** A replay of `suite-v1` raises no GSTR-2B finding the ground truth doesn't list, so `precision` can be baselined. Today, `make eval-replay SUITE=suite-v1` (results `results/suite-v1/20261012T111528Z`, fixtures still untracked) gives recall 40/40 but precision 59/81 (72.8%) and 22 false alarms.

**Finding.** 21 of the 22 are GSTR-2B findings with two separate causes. Diagnose each before changing anything; the fix may be in the seeder, the truth builder or the check.
1. **`gstr2b_wrong_period`: 6 false alarms, all one month off.** The check (`internal/checks/gstr2b.go:213-226`) raises it in the invoice's books month M, when the invoice is missing from M's 2B but is in M-1's or M+1's. The ground truth lists it as an `expected` item in the month whose 2B holds the late invoice, which is M+1. Every false alarm has its truth entry one month later: mehta 07 `LTS2026742` (truth in 08), mehta 08 `FBR2026804` (09), sharma 06 `VNT2026631` (07, and it is the one `clean_month` false alarm), sharma 07 `SWF2026735` (08), sharma 08 `SWF2026813` and `SWF2026815` (09). Decide which side is right: file the truth under the books month, or change what the check reports. Truth is protected; the owner approves any change to it.
2. **`gstr2b_missing_in_books`: 15 false alarms, all sharma 2026-08.** The fixture's August 2B has 36 entries; the purchase invoices the check read (window 2026-07-01 to 2026-09-30) hold 24 invoices dated August, and 15 of the 2B entries have no matching invoice in that window (for example `BHR/2026/0819`, `OMK/2026/0808`, `VNT/2026/0840`). None is in the truth; only `LDG2026804` is planted. Candidate causes, in order of cost to check: (a) ERPNext held the `--small` August books (CC-304's tests posted them; see the seeder-open-risks memory) when the fixtures were recorded, while the 2B was built from the full world; (b) the seeder skipped posting some invoices the 2B writer still emitted; (c) the key differs (`evidence.NormInvoiceNo`, bill date against posting date). Compare the world's August purchases, the ERPNext Purchase Invoices and the 2B file before choosing.

**Not in this ticket.** One more false alarm, `unrecorded_bank_charge` for `BNK-20260904-001` in sharma 2026-09, is a bank-rec finding and needs its own look. The re-record, the replay, the `suite-v1` fixtures and `evals/baseline.json` are a follow-up: fixtures and baseline go in together, and the baseline in its own `baseline-update` commit.

**Subtasks**
- Write up the cause of each of the two symptoms with the evidence (counts, keys, the fixture or ERPNext rows that show it) in the spec's Notes or `tasks/`.
- Fix the cause in the seeder or truth builder, with a unit test that fails on today's behaviour. If the fix belongs in `internal/checks`, stop and split a standard implementer ticket.
- Regenerate `evals/scenarios/suite-v1/ground_truth/**` only if the owner approves the change.
- Replay the existing fixtures with `make eval-replay SUITE=suite-v1` and report the counts; recording against ERPNext is the follow-up.

**Acceptance**
- Each symptom has a stated cause backed by data, not inference.
- After the fix, a re-seed plus replay shows 0 `gstr2b_wrong_period` false alarms and 0 `gstr2b_missing_in_books` false alarms beyond planted items, or the ticket lists what remains and why.
- `seed` stays deterministic: two runs give byte-identical output and truth.
- `make check` passes.

**Files.** `internal/seed/gstr2b*.go`, `truth*.go`, `testdata/**`, `evals/scenarios/suite-v1/**` (protected: owner approves). Phase 2, domain-data-engineer, regulated, needs the ERPNext lock for any live re-seed.

---

### CC-016 · Baseline file holds one section per suite

**Goal.** `evals/baseline.json` can hold the baseline of more than one suite, so the eval gate can judge `suite-skeleton` and `suite-v1`.

**Finding** (2026-10-12, after CC-013/014/015). The `suite-v1` fixtures are committed and replay clean (recall 40/40, precision 66/66, 0 clean-month false alarms), but nothing in CI reads them. `.github/workflows/eval.yml:53` sets `SUITE: suite-skeleton`, and `evals/baseline.json` (version 2, `{"version":2,"tiers":{"replay":...,"llm":...}}`) holds one suite: `eval score --compare` stops with "baseline is for suite suite-skeleton, the score for suite-v1" (`internal/evals/baseline.go`).

**Owner decisions (delegated to the orchestrator, 2026-10-12)**
1. One file, not one per suite. `evals/baseline.json` is already protected everywhere (`gates/protected.go`, the scorer's output guard, `CLAUDE.md`, the guard hook); a second baseline file would need all of those changed. New format: `{"version":3,"suites":{"<suite>":{"replay":Baseline|null,"llm":Baseline|null}}}`. The loader still reads version 2 (its sections name their suite), so the order of the follow-up commits does not matter.
2. Both suites are gated on every eval-relevant pull request (replay takes seconds). That is CC-016a.
3. The `llm` tier is out of scope. It stays null; a dispatch or nightly run still fails at "baseline present" for it.

**Subtasks**
- `LoadBaseline` reads versions 2 and 3 and returns the same in-memory shape; a version 2 file becomes one suite (both sections must name the same suite). `WriteBaseline` (`--baseline-out`) always writes version 3: it loads the file at the path if it exists (version 2 or 3), replaces only that suite's tier section and keeps every other suite and tier.
- `Section` and `CompareTier` take the suite: a score is judged against `suites[score.Suite][tier]`. A missing suite or tier is a failure (fail closed), reported with the key `<suite>/<tier>`. A section whose own `suite` differs from the key it sits under is a load error.
- `--compare` of a version 3 file with several suites works; the scorer-output guard (`CheckOutputPath`) is unchanged.
- Tests: load v2, load v3, v3 with two suites, round trip, upgrade of a v2 candidate on write, other suite kept on write, missing suite fails closed, mismatched section suite rejected. Test data under `internal/evals/testdata/`.

**Files.** `internal/evals/baseline.go`, `baseline_test.go`, `internal/evals/testdata/**`, `cmd/eval/score.go`, `score_test.go`. Phase 2, eval-engineer, regulated, human_review. The scorer is built from the base ref, so this must be merged before CC-016a.

---

### CC-016a · Eval workflow gates suite-v1 and the suite-v1 baseline

**Goal.** A pull request's G4 check replays and compares both `suite-skeleton` and `suite-v1`, so a regression in a Phase 2 detector fails CI.

**Subtasks**
- `eval.yml` runs the replay tier and its gate for each of `suite-skeleton` and `suite-v1` inside the one job `eval (G4)` (a loop in the steps, not a matrix: a matrix renames the job and breaks the required check). Gate expressions: `suite-skeleton` unchanged; `suite-v1` uses `--compare` plus `--require 'clean.false_alarms==0'`. The baseline-present check reads version 3 (and 2). The `llm` tier stays skeleton-only and unchanged.
- The `suite-v1` baseline goes in `evals/baseline.json` in its own `baseline-update` commit, made with `eval score <replay results> --baseline-out <copy of the file>`, so the file becomes version 3 with both suites.
- Verified locally with `make eval-replay` for both suites before merge.

**Files.** `.github/workflows/eval.yml` (protected), `evals/baseline.json` (protected, `baseline-update` commit only). Phase 2, eval-engineer, regulated, human_review.

---

### CC-017 · Loader that replaces a period's evidence, and `make load` that works

**Goal.** `make load SUITE=suite-v1` loads every company-month the suite seeded, and a reload leaves exactly what the files hold.

**Finding** (found re-recording `suite-v1`, 2026-10-12)
1. `make load` runs `go run ./cmd/load --suite $(SUITE)` (`Makefile:109-115`), but `cmd/load` takes subcommands `bank` and `gstr2b` with `--company` and `--month` (`cmd/load/main.go:30-41`) and no `--suite`. The target fails with "flag provided but not defined: -suite". `docs/mcp.md:23` and `evals/fixtures/README.md` tell people to run it.
2. `UpsertGSTR2BEntries` (`internal/store/evidence.go:200`) and `UpsertBankLines` (`:52`) only insert or update. A period's rows that the new file lacks stay in Postgres. A `--small` load left 16 sharma 2026-08 GSTR-2B rows behind; a later full load added its own 20 on top, and the recorded fixture held 36. That produced 15 false `gstr2b_missing_in_books` alarms (CC-013 diagnosis, `tasks/cc-013-diagnosis.md`). Stale bank rows likely caused the 22nd false alarm too: it vanished after the evidence tables were cleared.

**Subtasks**
- Loading a company-month replaces that company-period's rows: delete the period's rows and insert the file's, in one transaction, for both bank lines and GSTR-2B entries. Keep the existing balance check and idempotence.
- Add `load all --suite <name>`: read the suite file, and load every company-month it lists, bank and GSTR-2B, with the same replace behaviour. Point `make load` at it. Decide in the spec whether the months a check reads past the run month (the previous and next return) come from the suite's seeded range.
- A test that loads a larger file then a smaller one for the same period and finds only the smaller one's rows; an integration test for the transaction.
- `evals/fixtures/README.md` (Bootstrap order): say that a re-record needs `make load` (now replacing), that `index.json` also pins each ground-truth file's sha256 so a truth change makes every suite's fixtures stale, and that the committed suites (`suite-skeleton`, `suite-v1`) are re-recorded together when the truth of either changes.

**Files.** `cmd/load/**`, `internal/evidence/**`, `internal/store/evidence.go`, `evidence_test.go`, `store_integration_test.go`, `Makefile`, `evals/fixtures/README.md`. Phase 2, implementer, data-sensitive (it deletes evidence rows; the security review checks the delete is scoped to one company and period).

---

---

### CC-018 · Close the scorer's output-path and duplicate-key gaps

**Goal.** The scorer can't be steered into writing `evals/baseline.json` or anything under `evals/scenarios/`, `evals/golden/` or `gates/`, and a baseline file with a repeated JSON key is refused.

**Findings** (G5 on CC-016, 2026-10-12; both confirmed by a probe test; neither was introduced by CC-016)
1. **A symlink followed by `..` gets past `CheckOutputPath`** (`internal/evals/baseline.go:222-245` `outputCandidates`, `internal/evals/result.go:191,208` `writeFileAtomic`). `filepath.Abs` cleans `..` as text before `EvalSymlinks` runs, but `os.Rename(tmp, path)` gets the raw path and the OS resolves `l/..` through the real directories. With `out/l -> repo/evals/scenarios/sub`, `WriteBaseline("out/l/../x.json")` passed the check and wrote `repo/evals/scenarios/x.json`; with `out/l2 -> repo/evals/sub2`, `out/l2/../baseline.json` created `repo/evals/baseline.json`. `eval noise --out` is affected the same way, and `MkdirAll(f.Out)` (`cmd/eval/score.go:260`) can create an empty directory under a protected folder. Exploiting it needs control of a CLI flag and a planted symlink.
2. **Duplicate JSON keys are silently accepted** (`internal/evals/score.go:1074-1090` `readStrict`, used by `LoadBaseline`). The last copy wins; a hand-edited baseline could show a reviewer one section while the gate compares against another.

**Subtasks**
- Check and write one resolved path: run `EvalSymlinks` on the path as the OS would walk it (not after `filepath.Clean`), or refuse any output path with a `..` component, and use that single path for the check, `Stat`, `LoadBaseline` and the rename. Apply it to `--baseline-out`, `--out` and `noise --out`. Regression tests for `out/l/../x.json` into `evals/scenarios/` and `evals/baseline.json`.
- Make baseline loading reject a key repeated in one object (walk the tokens with `json.Decoder.Token`). A case in `TestLoadBaselineV3Invalid`.

**Files.** `internal/evals/baseline.go`, `result.go`, `score.go`, their tests, `cmd/eval/score.go`, `score_test.go`. Phase 2, eval-engineer, regulated (the output guard is a protected invariant); security review and the Opus diff review run.

---

### CC-019 · Wire the pseudonymiser into the eval's Tier 2 path

**Goal.** `eval run` with the agent on masks GSTINs, PANs and individual names before every explainer model call, the way `cmd/agent` does, so Tier 2 measures the setup the product runs.

**Finding** (first Tier 2 run on `suite-skeleton`, `results/suite-skeleton/20261013T091848Z`, 2026-10-13). The run logged `explainer: no pseudonymiser configured; identifiers are sent to the model unmasked`. `cmd/agent/main.go:185` sets `agent.LLMExplainer.NewPseudonymiser`; `cmd/eval/main.go` never does. That breaks the CC-710 invariant on the eval path, and a baseline recorded this way would be invalid once it is fixed, so this ticket comes before the Tier 2 baseline.

**Subtasks**
- When the agent is on, `eval run` requires `PSEUDONYM_KEY` (32 bytes as 64 hex digits) and fails at startup with a fixed error that never carries the value; `--no-llm` / `--no-agent` don't need it.
- Set `NewPseudonymiser` from that key with `agent.NewRedactor`, as `cmd/agent` does.
- Tests: Tier 2 setup without a key fails; with a key the explainer has a non-nil pseudonymiser; Tier 1 needs no key; the model input holds no raw GSTIN from the fixtures.

**Follow-ups (not this ticket).** Hoist `pseudonymKey` into a shared package so `cmd/agent` and `cmd/eval` use one copy. `cmd/agent` runs `cfg.CheckLLM()` and wraps the model in `llmtrace.Provider`; `cmd/eval` does neither, another way Tier 2 differs from the product.

**Files.** `cmd/eval/main.go`, `cmd/eval/main_test.go`, `cmd/eval/args_test.go`. Phase 2, llm-engineer, data-sensitive. G4-tier2 is skipped (no `llm` section for `suite-skeleton` in `evals/baseline.json` yet, so the compare would fail with `baseline_tier_missing`); the check is the unit tests plus one eval run with no baseline compare.

### CC-806b · Score citation precision and validity

**Goal.** `eval score` reports citation validity and citation precision against `evals/golden/citations.yaml` (CC-907), so CC-806's Gate D ("validity 100%, precision at or above the baseline") can be measured and gated.

**Finding** (from CC-806, merged 2026-10-13). CC-806 retrieves passages and the explainer cites exact `(doc_id, section)` pairs, but nothing scores them: `internal/evals` has no citation metric, the eval workflow sets no `Retriever` (its runs keep one skipped `retrieve/close` step), and the llm-engineer may not touch `internal/evals` or `cmd/eval`.

**Subtasks**
- A scorer in `internal/evals`: match each golden case to its finding by the ground-truth keys (never by text); validity = every cited pair exists in the ingested corpus; precision = the cited pair is in the case's `expect` any-of list. Report both in `score.json` and `score.md`, and add a `--require` key.
- Wire a `Retriever` into `cmd/eval` for Tier 2, with recorded search results in the fixtures so `--replay` needs no TEI or live search; the retrieve steps then appear per finding in the eval runs.
- Tests with fixtures for a right, a wrong-section, a wrong-document and an unsupported citation.
- The baseline gets its citation numbers in its own `baseline-update` commit, owner-approved. This ticket does not edit `evals/baseline.json` or `evals/golden/**`.

**Notes.** CC-806 leaves the retrieval artifact reachable through the retrieve step's `output_refs` and the explanation's `document_refs` (the store cannot set a first attempt's `input_refs`; a small store ticket if the owner wants it literal). The `citations.yaml` header still says DRAFT; the owner confirms it before this ticket relies on it.

**Files.** `internal/evals/citations.go`, `internal/evals/citations_test.go`, `internal/evals/score.go`, `internal/evals/report.go`, `internal/evals/fixtures.go`, `internal/evals/testdata/**`, `cmd/eval/main.go`, `cmd/eval/main_test.go`. Phase 3, eval-engineer, standard. Depends on CC-806, CC-907 and CC-902.

### CC-020 · Why the explainer flags nearly every finding `needs_review`

**Goal.** Find and fix the cause of 7 or 8 of 8 findings ending `needs_review` in agent-on runs, so Tier 2 `verified_rate` is stable and meaningful.

**Finding** (2026-10-13, `suite-skeleton`, `--replay`, agent on). On the same commit `verified_rate` ranged 0/8 to 3/8 across runs (baseline 3/8). In the low runs the explainer sets `needs_review` itself on nearly every finding, including the three planted `unrecorded_bank_charge` findings that stayed verified in the good runs. Recall, precision and clean false alarms were unaffected. This blocks a trustworthy `verified` baseline; see `tasks/tier2-verified-informational.md`.

**Suspects, to confirm or rule out with evidence from stored explanations and prompts, not by guessing**
- The pseudonymised input from CC-019: tokens such as `GSTIN_xxxx` or masked names in EVIDENCE may read as unexplained, so the model flags the finding.
- The self-flag rule in `internal/agent/prompts/explain.txt` rule 4 ("if the evidence does not explain the finding, say so and set needs_review to true") combined with what EVIDENCE actually contains for these findings.
- Run-to-run variance of the fast model (`LLM_MODEL_FAST`), or a change in the `claude -p` provider's behaviour, since the morning runs of the same code scored 3/8 and 2/8.

**Subtasks**
- Reproduce on a pinned commit and record, per run, which findings are flagged and the explanation text's stated reason (read the stored artifacts; do not paste raw model text into logs).
- Run a controlled comparison of at least the two prompt and input variants the evidence points to, with at least three runs each, interleaved in time.
- Fix the cause in `prompts/explain.txt`, `explain.go` or the input builder, without weakening the verifier or the invariants.

**Acceptance.** After the owner's 6-run baseline-update lands, a 3-run Tier 2 on `suite-skeleton` has a `verified_rate` median at or above the baseline median and no run below its tolerance floor; recall, precision, clean false alarms and failed runs unchanged. The baseline is not edited by this ticket.

**Files.** `internal/agent/prompts/explain.txt`, `internal/agent/explain.go`, `internal/agent/explain_test.go`, `internal/agent/testdata/explain/**`. Phase 2, llm-engineer, data-sensitive. Depends on CC-019 and CC-806. G4-tier2 required (informational for `verified_rate` until the baseline-update lands; after that it is the acceptance).

---

### CC-708b · Score the router's refusals against the golden set

**Goal.** `eval score` reports correct refusal per category and over-refusal against `evals/golden/refusal.yaml` (CC-907), so CC-708's acceptance ("correct refusal at or above the threshold, zero misses in other_company and injection") is measured by someone other than CC-708's author.

**Finding** (from CC-708's spec, 2026-10-13). CC-708's own test only runs the rules with a `FakeProvider`, which is a self-check. `internal/evals` and `cmd/eval` have no refusal metric, so a G4-tier2 gate for CC-708 would skip silently, and the llm-engineer may not touch `internal/evals` or `cmd/eval`.

**Subtasks**
- A scorer in `internal/evals` that runs each of the 60 items through `agent.Triage.Route` and compares `Decision.Action` and `Category` with `expect_action` and `category`; report correct refusal per category and over-refusal on the 20 should-answer items in `score.json` and `score.md`, and add a `--require` key (for example `refusal.other_company.recall>=8/8`).
- A real-model run (fast model) and a `--replay` mode with recorded classifier responses, so `--no-llm` runs need no model.
- Tests with fixtures for a correct refusal, a wrong category, a miss and an over-refusal.
- The baseline gets its refusal numbers in its own `baseline-update` commit, owner-approved. This ticket does not edit `evals/baseline.json` or `evals/golden/**`.

**Notes.** CC-708 exposes `NewTriage(...)` and `Triage.Route`; the scorer builds one with the same redacting and recording wrappers. The `refusal.yaml` header still says DRAFT; the owner confirms it before this ticket relies on it.

**Files.** `internal/evals/refusal.go`, `internal/evals/refusal_test.go`, `internal/evals/score.go`, `internal/evals/report.go`, `cmd/eval/main.go`. Phase 3, eval-engineer, standard. Depends on CC-708 and CC-907.

## Notes for later specs


These are requirements on tickets that already exist. When you spec one of them, copy its line into the spec's Notes and acceptance.

- **CC-504b (from the CC-012 spec review):** `/mcp-admin` is never scoped, so its audit actor is `"admin"`, and the admin server has no `EnableAudit` (`cmd/mcp-books/main.go:119-125,147`). CC-504b must call `EnableAudit(adminServer, "books-admin", sink)` and CC-012's writes check must be extended to count admin-route rows inside a run's `[started_at, finished_at]` that have no matching approved proposal (in an eval run there are none, so any row is unauthorized), plus the ERPNext Journal Entry check (external-ID field without a posted proposal). Until then `unauthorized_writes==0` covers the agent route only, and Gate C's "writes 0" is only partly shown.
- **After CC-009 (worker report and G5), before a scoped suite-v1 run with the Phase 2 detectors:**
  - Variance (needs its own ticket, checks owner): `varianceEvidence` cites `list_purchase_invoices` for the month when an account's entries come from purchase invoices, but `VarianceCheck` never reads purchase invoices and each check step has its own `Snapshots`, so that ref has nothing to match and the step still ends `ErrNoSnapshot`. Either read the month's invoices in `internal/checks/variance.go` or drop the separate ref (the invoice numbers are already IDs on its `list_gl_entries` ref).
  - Verifier (G5, should-fix): annotation accepts a call whose window is wider than the run month, but `verify.go:617-641` `outOfScope` then flags that snapshot's `from_date`/`to_date` (or a prev/next `period`) as ScopeMismatch. Affected: GSTR-2B books side (prev-month start to next-month end), wrong-period 2B refs, and duplicates' lookback windows. Safe (verification fails) but those findings will annotate and then always fail verification: let the verifier accept a check-step snapshot's wider window, or have the checks read the run month for evidence.
  - Snapshot keys (G5, should-fix): identifier keys apply to the whole result (an `account` only has to appear on some invoice in the snapshot, not on the invoice a ref names; same for 2B `supplier_gstin`). Index keys per record, or pin the current behaviour in a test. Notes: key `descriptiveArgs` by server and tool; consider requiring at least one ID for a ref matched only through `month` or `from`/`to`; add a per-company fake so the cross-company case is explicit.
- **After CC-012 (diff review):** `exportWrites` treats any audit row for the run's actor as proof that audit works, but mcp-books and mcp-evidence each have their own sink and `DATABASE_URL`; evidence rows with no books rows would read as a checked 0. Require at least one `books` row before setting a count (with a fake-stack case), or do it in CC-504b. Also parenthesise the `hasRun` condition in `writesCheck`.
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

- **Snapshot annotation bug (found in CC-1103; needs its own ticket before Gate C):** `snapshotCall.supports` (`internal/agent/snapshot.go:207`) refuses any evidence-reference argument that is neither a recorded argument nor an indexed key. Four checks build such arguments, so their check step fails with `ErrNoSnapshot` as soon as it emits a finding in the real workflow: `rules.go:99` (month, rule, account), `duplicates.go:148-152` (month), `gstr2b.go:152` (from, to, tax amounts) and `variance.go:196` (month on the purchase-invoice reference). Today's suite plants only bank charges, so nothing caught it. Fix by using the recorded `from_date`/`to_date` args with IDs, or let `supports` ignore descriptive args. `internal/agent/injection_test.go` wraps `PolicyRulesCheck` in a test-only shim (`windowArgs`); remove it when this lands and re-run the taint tests.
- **CC-1103 residual risks (for the owner's review list):**
  - `prompt_injection.recall==4/4` on `suite-v1` is unproven: the planter (`internal/seed/plant.go`) plants only `unrecorded_bank_charge`, and the unauthorized-writes metric is still unchecked (CC-504a, `internal/evals/score.go:148`). Needs a planter ticket (domain-data-engineer) and the CC-504a check before Gate C. Also confirm the G4-tier1 run on `suite-v1` shows 0 `suspicious_instruction_text` false alarms.
  - Decisions to confirm: `<<<NAME … NAME>>>` fences kept instead of the ticket's `<untrusted source=…>`; severity `medium` and action `investigate` for the new finding; the pattern list is wider than the ticket's (mid-line `SYSTEM:`, `<system>` tags, "do not flag").
  - `SuspiciousTextCheck` scans only records posted in the month and skips bank lines with an empty `TxnID`, but an out-of-month record cited as evidence can still reach the explainer. Scan the records named in findings' evidence instead.
  - Bankrec finding titles copy the raw narration (`bankrec.go:387,417`) into untainted steps. Variance GL evidence can carry another record's remarks.
  - Remaining false-match risk: "the agent/model … should/post" and "assistant(s) … <imperative>" (for example "accounts assistant should verify").
  - Poisoned contract, ingest-time quarantine and the retrieval check land in Phase 3 (CC-803, CC-804, CC-806). The graph's `files` for CC-1103 list only `suspicious.go` and `prompts/**`; the wider spec list was needed.
- **G0 spec parse check (hit again in CC-1103):** an acceptance line with `: ` in it broke the front matter and `gates/cmd/declared` failed with a pin error that needed a spec-commit amend. Add a front-matter parse step to `/spec`'s G0 before the spec is committed.

- **CC-1001 notes for later tickets (from G5/G6; merged 2026-10-13):**
  - **CC-1102:** per-IP and per-username login rate limits and lockout. `POST /login` costs a bcrypt-12 compare and the login CSRF cookie is free, so login is a CPU-exhaustion target. Optionally require one bcrypt cost for all users so the dummy-hash timing stays uniform.
  - **CC-1103 deploy:** the proxy must keep the `Host` header, or add a configured public origin for the `Origin` fallback check (never read from request headers); add HSTS; serve over TLS or `http://localhost` only (the `__Host-`/`__Secure-` cookies are refused over plain HTTP on other hosts); rotate the three seed password hashes in `config/users.yaml` (a changed hash ends that user's sessions).
  - **Bearer tokens:** `token_sha256` is an unsalted SHA-256, so tokens must come from at least 32 random bytes (noted in the `config/users.yaml` header).
  - **Wiring:** `cmd/agent` doesn't start the server yet; use `web.NewServer(web.OptionsFromConfig(cfg), users, log)` and `(*Server).Run(ctx)`. `Run` logs "listening" before the bind (use `net.Listen` then `Serve`). Unknown methods or paths get the mux's 405 before auth; consider an any-method fallback behind auth in CC-1002.
  - **Spec deviation to confirm:** htmx is vendored at `internal/web/static/htmx.min.js` (2.0.4, sha384 verified) with `script-src 'self'`, instead of the spec's pinned CDN URL. Upgrading htmx means updating `htmxIntegrity`; `TestStaticAndHealthz` enforces it.

- **CC-504b follow-ups (from its G5 and G6; merged 2026-10-13 as 976bc6a):**
  - **Before CC-1004 starts (spec must say):** `internal/approvals` is the only holder of the admin token and sets `checker` and `maker` from one canonical identity source (the tool compares trimmed, case-folded strings and can't see aliases such as `abhi` and `abhi@...`); only `post_approved_journal_entry` may write `erp_name`; send proposal IDs as lowercase hyphenated UUIDs (`admin.go:205` refuses anything else); cap `payload.remark` (about 500 runes, no control characters) when the proposal is created, because `admin.go:436` copies it to ERPNext as `user_remark` with no check.
  - **Before Phase 4 widens (should-fix, small ticket for the integration-engineer):** `admin.go:192,194,416` log `"error", err` where `err` is the `*refusal`, so only `code: msg` is logged and the cause (Frappe error, store error, `erp_mismatch` detail) is lost. Log the cause's `ExcType` and HTTP status, not its `Message` (Frappe validation text can name accounts or amounts).
  - **Before Phase 4 widens (CC-303 bootstrap, domain-data-engineer):** make the `copilot_ext_id` Custom Field `unique: 1` on Journal Entry. Double-post safety rests on the Postgres row lock; if the locking connection drops between the ERPNext `List` and `Insert`, two callers could both insert, and only a unique field makes ERPNext reject the second.
  - **CC-012 (eval-engineer; `tasks/hardening.md` "CC-504b (from the CC-012 spec review)" still applies):** count `books-admin` audit rows with no matching approved proposal inside a run window, plus Journal Entries carrying `copilot_ext_id` with no `posted` proposal. Note that a refusal row (for example `self_approved`) is attributed to the checker, because `SetAuditActor` runs before `checkChecker` (`admin.go:221`).
  - **Low, later:** the repair path (`erp_name` set, status `approved`, `admin.go:228-243`) marks the row `posted` without confirming the entry in ERPNext; add `Get` plus `checkJournalEntry` before repairing. The row lock and a pool connection are held across ERPNext calls for up to 90 s (`proposal_lock.go:40`); rate limits are CC-1102. If the audit insert fails after a post the caller sees an error although the entry is posted; the Info line at `admin.go:200` keeps `proposal_id` and `erp_name`.
  - **Owner decision:** the shared `copilot-bot` ERPNext client (Accounts User) now has a write path through the admin server (`cmd/mcp-books/main.go:172`). A dedicated posting key would narrow it; that is a new key and needs the owner.
  - **Open owner decision, resolve before CC-1004 builds on the write path:** a dedicated ERPNext posting key, separate from the shared `copilot-bot` key. Owner's lean (2026-10-13): dedicated key.
  - **CC-1004 acceptance must include:** a test that only `internal/approvals` presents the admin token. The `admintoken` analyzer enforces it today, but no real caller exists yet.
- **Owner review list, regulated tickets:** CC-504b (merged 976bc6a) is owner confirmed, conditional on the CC-504b follow-ups above (2026-10-13).
- **CC-708 (merged 59c7a31, regulated; owner review list; G4-tier2 deferred to CC-708b):** record for later specs:
  - **CC-710 / CC-707 (identifier masking, should-fix from G5):** `redact.go` `gstinRe`/`panRe` match only contiguous upper-case ASCII. The router normalises any case and `\s . -` separators (up to 3) before the classifier, but these still reach the model unmasked: separators `/ _ , ( )` and en-dash or non-breaking hyphen, full-width letters and digits, the Kelvin sign. Move the canonicalisation (fold full-width and other `Nd` digits, widen the separator class to `[\s\p{P}\p{S}]{0,3}`, use `[^\p{L}\p{N}]` boundaries) into the redactor so every model path, including CC-707's answer path, gets it; add each form to the redaction tests and to the other-tenant rule table.
  - **CC-707 must:** pass a closure over `agent.NewRedactor(key, runID, names)` and the recorder to `NewTriage`; create one `close_runs` row per question; validate `Company` against the authenticated session; cap question length (the router sets none); rate-limit retries per run (a classifier-error step finishes `failed` and is re-routed); treat any error from `Route` as "do not answer"; decide whether to wrap the recorder with the cost check; reject or normalise registry names containing `& < >` (the fenced JSON escapes them before masking).
  - **CC-708b must:** report rules-only and classifier-only scores separately (rule-only figures after the fitted rules were removed: investment 8/10, tax 4/8, out_of_scope 0/8, other_company 8/8, injection 6/6, over-refusal 0/20); watch "tax penalties and waivers" in `prompts/triage.txt` for over-refusal of bookkeeping questions.
  - **Owner tuning (config/triage.yaml):** `inv-better-option` (`:117`) still refuses "What is the best way to record the bonds we hold?" and similar; require an action verb after the advice word and add those probes to `testdata/triage/should_answer.yaml`. Injection rules still refuse "admin token expiry policy" and "system override flag"; generic forms such as "print your system message", "you are DAN", `<|im_start|>` fall to the classifier. The ids `sharma` and `mehta` match as whole words (surname risk); Q33-Q36 rely on the classifier alone; full-width or look-alike spellings of a tenant name fall to the classifier.
  - **Spec note:** the orchestrator quoted the acceptance lines in `specs/CC-708.md` front matter (YAML parse fix, identical commands) by amending the unmerged spec commit; the pinned approval otherwise stands.
