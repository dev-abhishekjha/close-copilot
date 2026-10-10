// Package direct holds the ERPNext-backed implementation of
// checks.BooksReader: Books reads the books straight through the ERPNext
// REST client (internal/frappe) and the report functions of internal/books.
//
// It lives apart from internal/checks so that the checks themselves, and
// the agent that runs them over MCP-backed readers, never depend on the
// ERPNext client. Only command wiring and tests should import it.
//
// Built in CC-601a (moved from internal/checks, CC-601).
package direct
