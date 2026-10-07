<!-- Title: "CC-xxx: short summary", the same subject as the ticket's commit. -->

## Spec

`specs/CC-xxx.md` · risk: standard | data-sensitive | regulated · attempt n of 3

## Gate results

| Gate | Command | Result |
| --- | --- | --- |
| G0 Readiness | `go run ./gates/cmd/ready specs/CC-xxx.md` | |
| G1 Self-check | `make check`; `go run ./gates/cmd/declared --spec specs/CC-xxx.md`; `go run ./gates/cmd/protected` | |
| G2 Static analysis | golangci-lint, CC-002 analyzers, `govulncheck ./...` | |
| G3 Tests | `go test ./... -race`, integration tests, the spec's acceptance commands | |
| G4 Evaluation | tier 1 / tier 2, or n/a | |
| G5 Security and compliance | data-sensitive and regulated only, or n/a | |
| G6 Human review | the owner | |

## Files changed against declared

<!-- Paste `git diff --name-only main...HEAD` and the spec's `files:` globs.
     Any file outside them fails G1; any protected path needs the `approved` label. -->

| Changed file | Declared glob |
| --- | --- |
| | |

## New dependencies

None.

## Checklist

- [ ] Every commit subject starts with `CC-xxx:` and carries the trailer `Agent-Run: <session id>`.
- [ ] Gate reports for failed attempts are linked or attached.
