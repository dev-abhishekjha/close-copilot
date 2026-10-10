---
name: build
description: "Orchestrate one Close Copilot ticket end to end - G0 readiness, worktree branch, delegate to the owner subagent, run the gate ladder, retry with failure reports, security review, commit, merge to main through the merge queue."
argument-hint: CC-xxx
arguments: [id]
disable-model-invocation: true
allowed-tools: Read Grep Glob Write Agent Bash(rm -f tmp/current-task*) Bash(rm -f tmp/erpnext.lock*) Bash(make *) Bash(go *) Bash(git status*) Bash(git diff *) Bash(git log *) Bash(git switch *) Bash(git checkout -b *) Bash(git add *) Bash(git commit *) Bash(git rev-parse *) Bash(git worktree *) Bash(git merge *) Bash(git push*) Bash(git branch *) Bash(git fetch*) Bash(mkdir -p tmp*)
---

# Build $id

You are the orchestrator. You plan, delegate, run gates and commit. You do not write code under `cmd/` or `internal/` yourself; workers do.

Current state:
!`git rev-parse --abbrev-ref HEAD 2>/dev/null || true`
!`git status --porcelain 2>/dev/null | head -30 || true`
!`cat tmp/current-task 2>/dev/null || echo "no build in progress"`
!`cat tmp/erpnext.lock 2>/dev/null || echo "ERPNext lock free"`

## Lanes (owner decision, 2026-10-10)

Up to three tickets build at once, each in its own worktree under `.worktrees/<branch>` (git-ignored; Go tooling skips dot-dirs):

- **ERPNext lane:** at most one ticket that writes to ERPNext (`needs_erpnext: true`) holds `tmp/erpnext.lock` in the **main** checkout. Read-only integration tests don't take the lock.
- **Two code lanes:** tickets with no ERPNext writes, whose declared `files` don't overlap any in-flight ticket (G0 checks overlap).
- The LLM lane stays one ticket at a time: prompts and the verifier format are one contract on one budget.

The guard hook finds the checkout that holds each edited file, so each worktree's own `tmp/current-task` enforces its own spec, and role boundaries apply in every worktree.

## 1. G0 readiness

- `specs/$id.md` exists (else stop: run `/spec $id`). Re-run the G0 checklist from `/spec`.
- Regulated or `human_review: true` without `approved_by`: the owner delegated approval during the build phase (2026-10-10). Review the spec yourself against the ticket and its invariants, then set `approved_by: orchestrator (owner-delegated) <date>`.
- `needs_erpnext: true` and the main checkout's `tmp/erpnext.lock` names another ticket: wait. Otherwise write `$id` into it.
- `owner_role: human`: don't delegate. Tell the owner exactly what to do and how to record it (commit subject `$id: ...`).

## 2. Worktree and branch

- `git fetch`, then `git worktree add -b cc-<number>-<short-slug> .worktrees/cc-<number>-<short-slug> main`.
- In the worktree: `mkdir -p tmp/reports`, write `$id` into its `tmp/current-task`, and commit the spec (`$id: spec`).
- Every command for this ticket runs in the worktree (`cd` with an absolute path, or `git -C`). Give the worker the worktree's absolute path.

## 3. Delegate

Start the subagent named by `owner_role` with a pointer-only brief:

> Work only in <worktree absolute path>. Implement specs/$id.md. Read it first. Edit only its declared files. Run its acceptance commands before you finish. Report files changed and every command you ran with its result.

On a retry, add only: `Previous attempt failed; fix exactly what tmp/reports/<file> lists.`

**Model and effort** (token budget, owner decision 2026-10-10). Each worker's frontmatter sets its model: Sonnet for implementer, integration-engineer, domain-data-engineer and eval-engineer; Opus for llm-engineer and security-reviewer. Override only by risk, never down:

- `risk: regulated`: pass `model: "opus"` and `effort: "high"` to the worker.
- Otherwise: pass neither; the frontmatter model at default effort.
- A worker that failed twice on the same blocking check gets `model: "opus"` on its last attempt (see step 4).

Don't paste the spec, code or logs into the brief; the worker reads them itself.

## 4. Gates

Run the `/gates` procedure yourself in the worktree. Judge only from command output.

- **Standard:** while iterating, run the gates on the changed packages. The full ladder (`make check`, `make vuln`, tidy, declared, protected, acceptance) must pass before the commit, and again on main after the merge.
- **Data-sensitive and regulated:** the full ladder every time.
- Pass: go to step 5.
- Fail: write the failure report, then retry step 3 (max `budget.max_attempts`, default 3).
- **Same failure twice:** if the new report's blocking `check`s match the previous attempt's, the worker isn't converging. Before the last attempt, reread the spec section behind that check: if the spec is ambiguous or wrong, fix the spec (back through G0) instead of retrying; if it is clear, make the last attempt with `model: "opus"`.
- Read gate output from log files (`> tmp/reports/<id>-<gate>.log 2>&1`), only the failing part. The worker's summary plus the gate results are all you need; don't re-read the worker's code for standard and data-sensitive tickets (G5 reads the diff).
- Escalate to the owner immediately, without retrying, if the fix needs a new dependency the spec doesn't mention, a threshold or budget change, or a weaker security control. A file outside `files` sends the spec back through G0.

## 5. Security review (G5)

For `risk: data-sensitive` or `regulated`: start `security-reviewer` in the background with `$id`, the worktree path and the range `main...HEAD` plus uncommitted changes, and start the next ticket in another lane meanwhile. A blocking finding goes back to the worker as a failure report (counts as an attempt). Nothing data-sensitive or regulated merges without a G5 pass.

**Regulated tickets:** in place of the owner's G6 during the build phase, read the whole diff yourself against the spec's acceptance and the CLAUDE.md invariants, and list the ticket in the end-of-day report for the owner's later review.

## 6. Commit

- `git add` exactly the changed files that are in the spec's `files` (plus `go.mod`, `go.sum`); never `tmp/`.
- Message:

```
$id: <title from the spec>

<3-6 lines: what changed and why>

Gates: G1 pass, G2 pass, G3 pass[, G4 ..., G5 ...]
Agent-Run: ${CLAUDE_SESSION_ID}
```

## 7. Merge queue and hand over

Merges go one at a time, from the main checkout:

```sh
git fetch && git switch main && git merge --ff-only origin/main
git merge --no-ff -m "$id: merge <branch>" <branch>
make check && go mod tidy -diff    # re-run on the merged result; if red, fix forward before anything else merges
git push && git worktree remove .worktrees/<branch> && git branch -d <branch>
```

- `rm -f tmp/erpnext.lock` in the main checkout if this ticket took it.
- Report: summary, files, gate table, attempts used, anything deferred. Regulated tickets go in the end-of-day list for the owner.
- When no other build or background review is in flight in this session, end the report with: `Run /clear before the next ticket.` All state lives in git, specs/, tasks/ and tmp/, so a fresh session picks up from `/next`.
