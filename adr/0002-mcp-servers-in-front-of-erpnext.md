# ADR 0002: MCP servers in front of ERPNext instead of direct calls

- Status: accepted
- Date: 2026-10-14
- Decides: how the agent reaches the books and the evidence (CC-501 to CC-506, CC-504b)

## Context

The agent needs the ledger (trial balance, GL Entries, invoices, payments, account history) and the evidence (bank lines, GSTR-2B entries, documents). Some of those reads are chosen by a model: the investigator and the ask pipeline pick tools and arguments. A model can be steered by text in the data it reads, such as an invoice remark or a bank narration. So the boundary has to hold even when the caller is wrong or hostile.

The constraints were:

- The agent must not be able to write to the ledger. The one write, posting an approved journal entry, must sit behind maker-checker approval.
- A run may read only its own company and month (CC-506).
- Every read must be auditable after the fact.
- ERPNext free text must not reach a model unfenced, and amounts must stay exact (`int64` paise).

## Options

1. **Direct REST calls from the agent through `internal/frappe`.** This is the fewest moving parts. It lost because the agent process would hold ERPNext keys, and arguments a model chose would reach ERPNext filters with nothing in between to enforce scope. Audit, rate limits and argument validation would live in the same process as the model loop. It would also give the write path no separate credential.
2. **A bespoke internal HTTP API in front of ERPNext.** It gives the same separation as MCP. It lost because the tool descriptions, JSON schemas and annotations a model needs would have to be invented and kept in sync by hand. Standard clients (MCP Inspector, a desktop client over stdio) couldn't drive it, and the contract tests would need their own client.
3. **Two MCP servers: books (ERPNext) and evidence (Postgres), each with a scoped read route, plus a separate admin route for the single write (chosen).**

## Decision

- `cmd/mcp-books` serves seven read tools on `/mcp` with the ERPNext bot key (an Accounts User). `cmd/mcp-evidence` serves `list_bank_lines`, `list_gstr2b_entries` and `search_documents` from Postgres. Every read tool carries `readOnlyHint`.
- The only write tool, `post_approved_journal_entry`, is on the books server's `/mcp-admin` route. That route has its own bearer token (`MCP_TOKEN_ADMIN`), which only `internal/approvals` uses, and its own ERPNext user, which can only create and submit Journal Entries. The tool takes only a proposal ID and reads everything else from the approved proposal.
- The servers enforce scope, not the agent. Each run sends a short-lived company-and-month scope signed with `MCP_SCOPE_KEY`, and a call outside it is refused before any ERPNext or SQL request (`internal/mcpkit/scope.go`).
- `internal/mcpkit` holds the shared pieces: bearer auth, the scope check, an audit row per call (a failed insert fails the call), per-token rate limits, and Streamable HTTP and stdio transports.
- `internal/agent` never imports `internal/frappe`. The `noerpimport` analyzer enforces this, and `cmd/agent/deps_test.go` checks that the agent binary doesn't link the client.

## Consequences

- **Costs.** There are two more processes to run, configure and secure, and every read takes an extra local hop. A close makes many calls in a burst, so the default per-token limit is generous (50 requests a second, burst 100).
- **Lost detail.** The agent's registry turns every tool error into fixed text, which drops the field-named input errors a model could use to correct its arguments. This was accepted as the safer default.
- **SDK limits.** The Go SDK's server-side output validation decodes generically, so an amount above 2^53 paise would be rounded before it left a server. That is about ₹90 lakh crore, a theoretical limit for these companies. The evidence tools return a JSON array as structured content, which some strict clients flag ([docs/mcp.md](../docs/mcp.md)).
- **Test surface.** Each tool's listing is snapshotted under `internal/books/testdata/schemas/` and `internal/evidence/testdata/schemas/`, so a schema change is a reviewed diff. The contract tests drive both servers through the real SDK client.
- **Audit.** Every call, including each investigator step, is an `audit_log` row whose actor is set server-side (`agent:run/<run_id>`, or the checker for a post).
- **What would change this decision:** MCP protocol or SDK churn that breaks the transports faster than it can be tracked, or a latency budget the extra hop can't meet. Neither has shown up yet. In benchmark 1, two live suite-skeleton closes through both MCP servers took 324 to 606 ms in total without a model ([docs/benchmarks.md](../docs/benchmarks.md)). Time goes to model calls, not tool calls.
