package evidence_test

// The evidence tool tests that need no database. The end-to-end tool tests
// over Postgres are in tools_integration_test.go behind the integration
// build tag; the MCP contract tests are in contract_test.go.

import (
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/abhishekjha/close-copilot/internal/evidence"
)

func TestRegisterTools_Registration(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "test-evidence", Version: "1.0.0"}, nil)
	evidence.RegisterTools(server, nil)
	// Server now holds registered tools; client listing will be verified via client handshake.
}
