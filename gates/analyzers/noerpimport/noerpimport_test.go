package noerpimport_test

import (
	"path/filepath"
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"github.com/abhishekjha/close-copilot/gates/analyzers/noerpimport"
)

func TestNoERPImport(t *testing.T) {
	dir, err := filepath.Abs(filepath.Join("..", "testdata"))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		pkgs []string
	}{
		{"frappe import in internal/agent, test file included", []string{"internal/agent"}},
		{"agent subpackage through MCP is clean", []string{"internal/agent/mcp"}},
		{"internal/agent_test dir stays in scope", []string{"internal/agent_test"}},
		{"internal/agentx is out of scope", []string{"internal/agentx"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			analysistest.Run(t, dir, noerpimport.Analyzer, tt.pkgs...)
		})
	}
}
