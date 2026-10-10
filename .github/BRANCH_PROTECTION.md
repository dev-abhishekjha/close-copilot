# Branch protection for `main`

The workflows run on `pull_request`, so GitHub runs the pull request's own
copy of `.github/workflows/`. A PR could rewrite a workflow to skip the
base-ref gate build (CC-004 finding 1) or to approve any SHA (finding 2).
Those guarantees hold only with the repository settings below, which live
in GitHub, not in this repo. Set them when the remote is created and
recheck them whenever a workflow or job name changes (`gates/ci_test.go`
fails if a job name here goes missing).

## Required status checks (Settings > Branches > `main`)

Require these checks to pass before merging, and require branches to be up
to date before merging:

| Check (job name) | Workflow | Job |
| --- | --- | --- |
| `protected paths (G1)` | `protected.yml` | `protected` |
| `gates (G1 declared files, task graph, analyzers)` | `gates.yml` | `gates` |
| `build, vet, lint, test` | `ci.yml` | `check` |
| `integration tests` | `ci.yml` | `integration` |

No job in these workflows has an `if:` and none is skipped on any event, so
a required check is never satisfied by a skipped run.

## Reviews

- Require a pull request before merging.
- Require review from Code Owners (`.github/CODEOWNERS` owns every
  protected path, including `.github/` itself, so a PR that edits a
  workflow needs the owner's review).
- Dismiss stale pull request approvals when new commits are pushed.
- Require approval of the most recent reviewable push.
- Do not allow bypassing the above settings, including for administrators.

## The `approved` label

`protected.yml` approves only the head SHA in the payload of the event
that applies the `approved` label. Any later push, any other label event
and any edit of the PR (title, body or base branch) runs with no approved
SHA, so a protected change fails until the owner removes and reapplies the
label at the head they reviewed. Only accounts with triage access or above
can apply labels; keep that set to the owner.

Re-running a workflow run replays that run's original event payload. A
re-run of an old `labeled` run (action `labeled`, label `approved`)
approves the head SHA in that old payload again, even after the owner has
removed the label: the run checks out and judges that same old head, so it
can't approve a newer commit, but it turns a withdrawn approval green
again for that head. Limit the right to re-run workflows (write access and
above) to the owner.

## Bootstrapping this change (CC-004)

The gates are built from the base ref, so the pull request that adds
`--approved-sha` to `gates/cmd/protected` is judged by `main`'s older
`protected` binary, which doesn't know the flag and exits with a usage
error. That PR (and only that one) fails `protected paths (G1)` on every
run. The owner merges it on review of the diff, with the protected-path
check overridden once; from the next PR on, `main`'s binary has the flag.
The same holds for any later PR that adds a flag the workflow passes to a
gate binary: land the flag in one PR and its use in the workflow in the
next.

## Retargeting

Changing a PR's base branch fires `pull_request` `edited`. `gates.yml` and
`protected.yml` listen for it and rerun against the new base. `ci.yml` does
not (an edit must never cancel `check` or `integration`); "require branches
to be up to date" covers it.

## TODO

- Pin third-party actions (`actions/checkout`, `actions/setup-go`,
  `golangci/golangci-lint-action`) by full commit SHA instead of tag, with
  the tag in a trailing comment. The SHAs could not be confirmed offline
  when CC-004 was built.
- Revisit `pull_request_target` (base-ref workflow files) only with a
  design that never checks out or runs PR code with its write token.
