// Package ledger holds the ERPNext-free domain types the checks, the store
// and the agent share: the accounting documents (GL Entries, invoices,
// payments, journal entries) with amounts in money.Paise and dates as
// time.Time at UTC midnight, and the report rows built from them (trial
// balance, account history, recurring suppliers).
//
// It imports only internal/money and the standard library. The ERPNext
// client (internal/frappe) converts its raw JSON structs into these types,
// and internal/frappe and internal/books keep type aliases for them, so
// code that reads books through MCP never has to import the client.
//
// Built in CC-601a.
package ledger
