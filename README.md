# Close Copilot

A Go agent that runs the month-end close on ERPNext: deterministic checks find
the problems (bank reconciliation, duplicate payments, GSTR-2B mismatches,
missing accruals, policy breaches), an LLM explains each one with numbers a
verifier traces back to the evidence, and nothing touches the ledger without
maker-checker approval. ERPNext and the evidence are reached only through two
MCP servers.

> Status: initial setup (CC-101). Every binary starts, checks its
> configuration and exits; the tickets fill them in.

## Docs

| Doc | What it holds |
| --- | --- |
| [Implementation Tickets](docs/implementation-tickets.md) | What to build: 69 tickets with goals, subtasks, acceptance criteria and files, plus the shared specs (env vars, schema, MCP tools, API) |
| [Orchestrated Implementation Plan](docs/orchestrated-implementation-plan.md) | How it gets built: runtime and build-time roles, task graph by phase, gate ladder G0–G6, RAG evaluation and guardrails |

Both are exports of living docs on claude.ai (links at the top of each file);
re-export when those change.

## Quick start

Prerequisites: Go 1.26+, Docker (about 10 GB of memory for the full stack),
and [golangci-lint v2](https://golangci-lint.run/welcome/install/).

```sh
make deps            # first time only: resolves modules and writes go.sum
cp .env.example .env # then fill in keys as the tickets ask for them
make check           # build, vet, lint, unit tests
make up              # Postgres + pgvector and the two TEI model servers
go run ./cmd/agent -version
```

`make help` lists every target. templ and govulncheck run through `go tool`
and goose through `go run`, so nothing needs a global install except
golangci-lint.

## How it gets built

Claude Code on your machine builds every ticket ([ADR 0001](adr/0001-build-with-local-claude-code.md)). Open `claude` in the repo root; `CLAUDE.md` gives it the project rules.

```text
/next            tickets ready to start, in phase order
/spec CC-xxx     write specs/CC-xxx.md (regulated specs wait for your approval)
/build CC-xxx    branch, delegate to the role subagent, run gates G1-G5, commit; never merges
/gates           run the gate ladder on the current branch
```

You review the branch and merge it yourself. Role subagents live in `.claude/agents/`; `.claude/settings.json` asks before anything touches a protected path, and a hook blocks edits outside the ticket's declared files. The product's own LLM calls also use the local CLI by default (`LLM_PROVIDER=claude-cli`); switch to `anthropic` with an API key for the public demo.

## Layout

```
CLAUDE.md       project rules for Claude Code
.claude/        subagents (build roles), skills (/next /spec /build /gates),
                settings and hooks
adr/            decision records
specs/          one spec per ticket; _template.md is the shape
tasks/          graph.yaml: phase, owner, risk, deps and files for all 69 tickets
cmd/            one binary per process: agent, mcp-books, mcp-evidence,
                seed, load, ingest, eval, probe
internal/       all logic; one package per area (see each doc.go)
  config/       env vars -> typed Config, fails fast listing every missing one
  cli/          shared main: -version, JSON logs, config, signals
  deps/         pins the initial dependencies until code imports them
deploy/         docker compose for app services; ERPNext apps.json (CC-102)
docs/           the two planning docs
migrations/     goose SQL migrations (CC-401, CC-709)
```

## Dependencies

| Area | Module |
| --- | --- |
| MCP servers and clients | `github.com/modelcontextprotocol/go-sdk` |
| LLM | `github.com/anthropics/anthropic-sdk-go` |
| Postgres, vectors, migrations | `github.com/jackc/pgx/v5`, `github.com/pgvector/pgvector-go`, `github.com/pressly/goose/v3` |
| Tracing | `go.opentelemetry.io/otel` (+ sdk, OTLP/HTTP exporter, `otelhttp`) |
| UI | `github.com/a-h/templ` (htmx from a pinned CDN URL) |
| Config files | `go.yaml.in/yaml/v3` |
| Auth and limits | `golang.org/x/crypto` (bcrypt), `golang.org/x/time` (rate) |
| Static analyzers | `golang.org/x/tools/go/analysis` |
| Tests | `github.com/testcontainers/testcontainers-go` (+ postgres module) |
| Tools | templ and govulncheck via `go tool`; goose via `go run` |

## Licence

MIT, see [LICENSE](LICENSE). All data in this repository is synthetic.
