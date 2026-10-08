package admintoken_test

import (
	"path/filepath"
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"github.com/abhishekjha/close-copilot/gates/analyzers/admintoken"
)

func TestAdminToken(t *testing.T) {
	dir, err := filepath.Abs(filepath.Join("..", "testdata"))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		pkgs []string
	}{
		{"field in internal/agent; other struct and test file clean", []string{"internal/agent/token"}},
		{"literal, constant and concatenation in internal/books", []string{"internal/books/admin"}},
		{"auth.go allowed, server.go not", []string{"internal/mcpkit"}},
		{"config and approvals allowed", []string{"internal/config", "internal/approvals"}},
		{"name allowed in cmd/mcp-books only as a []string element; env readers and field not", []string{"cmd/mcp-books"}},
		{"name exception does not reach cmd/mcp-books/sub", []string{"cmd/mcp-books/sub"}},
		{"_test-suffixed dirs inherit no allowance", []string{"internal/approvals_test", "cmd/mcp-books_test"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			analysistest.Run(t, dir, admintoken.Analyzer, tt.pkgs...)
		})
	}
}
