---
name: next
description: "List the Close Copilot tickets that are ready to start now, in phase order, with owner, risk and whether they need the ERPNext lock. Use when deciding what to build next."
argument-hint: "[phase]"
allowed-tools: Read Grep Bash(git log *) Bash(git branch *)
---

# What can be built next

## State from git (a ticket is done when main has a commit starting `CC-xxx:`)

Done:
!`git log main --format=%s 2>/dev/null | grep -oE '^CC-[0-9]+[ab]?' | sort -u | tr '\n' ' ' || true`

Open ticket branches:
!`git branch --list 'cc-*' --format='%(refname:short)' 2>/dev/null || true`

Build in progress:
!`cat tmp/current-task 2>/dev/null || echo none`

ERPNext lock:
!`cat tmp/erpnext.lock 2>/dev/null || echo free`

## Do

1. Read `tasks/graph.yaml`.
2. The current phase is the lowest phase with a ticket that isn't done. If the user passed a phase ($ARGUMENTS), use that instead.
3. A ticket is **ready** when it isn't done, has no open branch, and every `depends_on` ID is done. Only list tickets in the current phase, plus later-phase tickets whose dependencies are all done and that the plan allows early (check the phase table in `docs/implementation-tickets.md`, "Epic overview").
4. Print one table: ID, title, owner, risk, human review, needs ERPNext, and the command to run (`/spec <ID>` if `specs/<ID>.md` is missing, else `/build <ID>`). Mark `owner: human` tickets as "yours" with what to do.
5. Then say which ones can run in parallel: at most three at once, at most one holding the ERPNext lock, and no two whose declared `files` overlap.
6. If the phase is complete, name the phase gate from the plan ("What Gate B checks" etc.) and what must be shown before moving on.

Read only. Don't create branches or files.
