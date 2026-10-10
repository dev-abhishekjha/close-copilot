# MCP servers

Close Copilot has two MCP servers. The agent reaches ERPNext and the evidence store only through them (CC-501 to CC-505).

| Server | Command | Default HTTP address | Tools on `/mcp` |
| --- | --- | --- | --- |
| Books | `cmd/mcp-books` | `:7001` | `get_trial_balance`, `list_gl_entries`, `list_purchase_invoices`, `list_sales_invoices`, `list_payments`, `get_account_history`, `list_recurring_suppliers` |
| Evidence | `cmd/mcp-evidence` | `:7002` | `list_bank_lines`, `list_gstr2b_entries` |

Every tool here is read-only and carries the `readOnlyHint` annotation. Amounts are integers in paise (1 rupee = 100 paise), dates are `YYYY-MM-DD` and months `YYYY-MM`. Each tool takes a company ID such as `sharma`, not the ERPNext company name. The books server also serves `/mcp-admin` for the approved-write tool (CC-504b). It uses a separate token, and this guide doesn't cover it.

## Running the servers

Run every command from the repository root. The servers read their configuration only from environment variables, so export the ones below in your shell first. They live in your `.env`, and `set -a; . ./.env; set +a` exports them. `.env.example` lists every variable.

| Server | Environment variables |
| --- | --- |
| Books | `ERP_BASE_URL`, `ERP_SITE`, `ERP_API_KEY`, `ERP_API_SECRET`, `MCP_TOKEN_AGENT`, `MCP_TOKEN_ADMIN` |
| Evidence | `DATABASE_URL`, `MCP_TOKEN_AGENT` |

The books server uses the bot key (`ERP_API_KEY`, `ERP_API_SECRET`, an Accounts User), never the seeder key. It maps company IDs to ERPNext company names through the profiles in `config/companies` (flag `--companies`).

For the tools to return anything, ERPNext must be seeded (`make up`, then `make seed`) and the bank and GSTR-2B files loaded into Postgres (`make migrate`, then `make load`).

### Streamable HTTP (what the agent uses)

```sh
make build                                   # writes bin/mcp-books and bin/mcp-evidence
bin/mcp-books --transport=http --addr=:7001
bin/mcp-evidence --transport=http --addr=:7002
```

`go run ./cmd/mcp-books --transport=http` works too. Each server answers on `/mcp`, and every request needs `Authorization: Bearer <MCP_TOKEN_AGENT>`. A request without the header, or with the wrong token, gets HTTP 401. `GET /healthz` needs no token:

```sh
curl -s http://localhost:7001/healthz        # ok
curl -s -o /dev/null -w '%{http_code}\n' -X POST http://localhost:7001/mcp   # 401
```

### stdio (for a desktop client)

```sh
bin/mcp-books --transport=stdio
bin/mcp-evidence --transport=stdio
```

stdio has no bearer check: the process that starts the server owns it. The server logs JSON to stderr. stdout carries only the protocol.

## Checking them with MCP Inspector (Gate 1)

[MCP Inspector](https://github.com/modelcontextprotocol/inspector) is the reference MCP client. It needs Node 22.19 or later, and `npx` fetches it on first use. Gate 1's manual check pulls ledger data from the books server and bank lines from the evidence server.

### Web UI

Start a server over HTTP as above, then:

```sh
npx @modelcontextprotocol/inspector \
  --server-url http://localhost:7001/mcp --transport http \
  --header "Authorization: Bearer $MCP_TOKEN_AGENT"
```

The UI opens at `http://127.0.0.1:6274`. Press **Connect**, open **Tools**, then **List Tools**, and call each tool with the arguments below. For the evidence server, use `http://localhost:7002/mcp`.

### CLI (scriptable)

The target (URL or command) comes first, then the flags:

```sh
B=http://localhost:7001/mcp
AUTH="Authorization: Bearer $MCP_TOKEN_AGENT"

npx @modelcontextprotocol/inspector --cli "$B" --transport http --header "$AUTH" --method tools/list

npx @modelcontextprotocol/inspector --cli "$B" --transport http --header "$AUTH" \
  --method tools/call --tool-name get_trial_balance \
  --tool-args-json '{"company":"sharma","from_date":"2026-09-01","to_date":"2026-09-30"}'
```

`--tool-args-json` passes the arguments verbatim. `--tool-arg key=value` also works, but it JSON-parses each value. Add `--format json` to pipe the result into `jq`.

To drive a stdio server, put the command first. Pass its environment with `-e`, and put the Inspector's own flags after `--`:

```sh
npx @modelcontextprotocol/inspector --cli bin/mcp-evidence --transport=stdio \
  -- -e DATABASE_URL="$DATABASE_URL" -e MCP_TOKEN_AGENT="$MCP_TOKEN_AGENT" --method tools/list
```

### Arguments to try (company `sharma`, September 2026)

Books server:

| Tool | Arguments |
| --- | --- |
| `get_trial_balance` | `{"company":"sharma","from_date":"2026-09-01","to_date":"2026-09-30"}` |
| `list_gl_entries` | `{"company":"sharma","from_date":"2026-09-01","to_date":"2026-09-30","account":"Bank Charges - STPL"}`. If `next_cursor` is non-empty, call again with `"cursor"` set to it. |
| `list_purchase_invoices` | `{"company":"sharma","from_date":"2026-09-01","to_date":"2026-09-30"}`, optionally with `"supplier"` |
| `list_sales_invoices` | `{"company":"sharma","from_date":"2026-09-01","to_date":"2026-09-30"}` |
| `list_payments` | `{"company":"sharma","from_date":"2026-09-01","to_date":"2026-09-30"}`, optionally with `"party"` |
| `get_account_history` | `{"company":"sharma","account":"Bank Charges - STPL","months":6,"through_month":"2026-09"}` |
| `list_recurring_suppliers` | `{"company":"sharma","before_month":"2026-09","lookback_months":6,"min_occurrences":3}` |

Evidence server:

| Tool | Arguments |
| --- | --- |
| `list_bank_lines` | `{"company":"sharma","from_date":"2026-09-01","to_date":"2026-09-30"}`. Add `"min_amount":100000` to keep lines of at least 1,000 rupees either way. |
| `list_gstr2b_entries` | `{"company":"sharma","period":"2026-09"}`. Add `"supplier_gstin"` (a GSTIN from the first call) to filter. |

Bad input, such as `"from_date":"2026-9-01"` or an unknown argument, comes back as a tool error (`isError: true`) that names the field. It is not a protocol error.

The evidence tools return a JSON array as `structuredContent`, and their `outputSchema` has type `array`, not `object`. The Go SDK allows this, and every result also carries the same JSON as text content. A client that requires object-typed output schemas will flag it.

## Contract tests and schema goldens

The contract tests drive both servers through the real SDK client over Streamable HTTP, behind the bearer-auth middleware:

| Test | Where | Runs in |
| --- | --- | --- |
| `TestContractBooks`, `TestContractBooksSchemas` | `internal/books/contract_test.go` (fake ERPNext) | `make check` |
| `TestContractEvidenceSchemas` | `internal/evidence/contract_test.go` | `make check` |
| `TestContractEvidence` | same file. It needs Postgres, so it skips without the tag. | `make test-int` |

```sh
go test ./internal/books/... -run TestContract
go test -tags=integration ./internal/evidence/... -run TestContract
```

Each tool's listing (name, description, annotations, `inputSchema` and `outputSchema`) is snapshotted as JSON with sorted keys and a 2-space indent in `internal/books/testdata/schemas/<tool>.json` and `internal/evidence/testdata/schemas/<tool>.json`. A changed struct, `jsonschema` tag, description or annotation fails the test and prints a line diff. A golden file without a matching tool fails too.

After a deliberate change, regenerate the goldens and review the diff with the code change:

```sh
go test ./internal/books -run TestContractBooksSchemas -update
go test ./internal/evidence -run TestContractEvidenceSchemas -update
git diff -- internal/*/testdata/schemas
```
