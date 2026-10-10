# Hardening tickets

These tickets come from the security reviews (G5) of CC-001, CC-002, CC-201 and CC-202. Each finding was deferred, and none of them is in `docs/implementation-tickets.md`. `/spec` takes its ticket text from this file in place of the tickets doc. The graph entries are in `tasks/graph.yaml`.

Status was checked against `main` at `c96642d` on 2026-10-09. Line numbers refer to that commit.

| Ticket | Phase | Owner | Risk | Blocks |
| --- | --- | --- | --- | --- |
| CC-004 Harden the merge gates, CI and guard hook | 0 | implementer | regulated | relying on branch protection |
| CC-005 Close the static analyzer gaps | 0 | implementer | regulated | |
| CC-104 Redact secrets in config | 1 | implementer | data-sensitive | CC-701, CC-904 |
| CC-205 Harden the Frappe client, probe and API-user script | 1 | integration-engineer | data-sensitive | CC-502 |
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

**Findings** (CC-004 G5, attempt 3; `tmp/reports/CC-004-attempt-3.json` in the CC-004 worktree)
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
