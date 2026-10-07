---
name: build
description: "Orchestrate one Close Copilot ticket end to end - G0 readiness, branch, delegate to the owner subagent, run the gate ladder, retry with failure reports, security review, commit on the ticket branch. Never merges."
argument-hint: CC-xxx
arguments: [id]
disable-model-invocation: true
allowed-tools: Read Grep Glob Write Agent Bash(rm -f tmp/current-task*) Bash(rm -f tmp/erpnext.lock*) Bash(make *) Bash(go *) Bash(git status*) Bash(git diff *) Bash(git log *) Bash(git switch *) Bash(git checkout -b *) Bash(git add *) Bash(git commit *) Bash(git rev-parse *) Bash(mkdir -p tmp*)
---

# Build $id

You are the orchestrator. You plan, delegate, run gates and commit. You do not write code under `cmd/` or `internal/` yourself; workers do.

Current state:
!`git rev-parse --abbrev-ref HEAD 2>/dev/null || true`
!`git status --porcelain 2>/dev/null | head -30 || true`
!`cat tmp/current-task 2>/dev/null || echo "no build in progress"`
!`cat tmp/erpnext.lock 2>/dev/null || echo "ERPNext lock free"`

## 1. G0 readiness

- `specs/$id.md` exists (else stop: run `/spec $id`). Re-run the G0 checklist from `/spec`.
- Regulated or `human_review: true` without `approved_by`: stop and ask the owner.
- Another build in progress (`tmp/current-task` holds a different ID): stop.
- `needs_erpnext: true` and `tmp/erpnext.lock` names another ticket: stop. Otherwise write `$id` into `tmp/erpnext.lock` with the Write tool (before step 2, while no task is active).
- `owner_role: human`: don't delegate. Tell the owner exactly what to do and how to record it (commit subject `$id: ...`).
- Working tree: only the spec may be uncommitted.

## 2. Branch

- `git switch main`, then `git switch -c cc-<number>-<short-slug>` (e.g. `cc-602-bank-rec`).
- `mkdir -p tmp/reports`, then write `$id` into `tmp/current-task` with the Write tool. While this file exists the guard hook enforces the spec's `files` (it always allows `tmp/`, `go.mod`, `go.sum` and the spec).
- Commit the spec: `git add specs/$id.md && git commit -m "$id: spec"`.

## 3. Delegate

Start the subagent named by `owner_role` with a pointer-only brief:

> Implement specs/$id.md. Read it first. Edit only its declared files. Run its acceptance commands before you finish. Report files changed and every command you ran with its result.

On a retry, add only: `Previous attempt failed; fix exactly what tmp/reports/<file> lists.`

## 4. Gates

Run the `/gates` procedure yourself on the branch. Judge only from command output.

- Pass: go to step 5.
- Fail: write the failure report, then retry step 3 (max `budget.max_attempts`, default 3).
- Escalate to the owner immediately, without retrying, if the fix needs a file outside `files`, a protected path, a new dependency the spec doesn't mention, or a threshold change. Escalate after the last failed attempt with all reports.

## 5. Security review (G5)

For `risk: data-sensitive` or `regulated`: start `security-reviewer` with `$id` and the range `main...HEAD` plus uncommitted changes. A blocking finding goes back to the worker as a failure report (counts as an attempt).

## 6. Commit

- `git add` exactly the changed files that are in the spec's `files` (plus `go.mod`, `go.sum`); never `tmp/`.
- Message:

```
$id: <title from the spec>

<3-6 lines: what changed and why>

Gates: G1 pass, G2 pass, G3 pass[, G4 ..., G5 ...]
Agent-Run: ${CLAUDE_SESSION_ID}
```

## 7. Hand over

- `rm -f tmp/current-task`, and `rm -f tmp/erpnext.lock` if you took it. `git status` must be clean apart from ignored files.
- Report to the owner: summary, files, gate table, attempts used, anything deferred.
- Regulated or `human_review: true`: "G6: please review `git diff main...<branch>`". Point at the lines most worth reading (for CC-602, CC-705 and CC-506, all of them).
- Give the merge commands for the owner to run; never run them yourself:

```sh
git switch main && git merge --no-ff <branch> && git branch -d <branch>
```
