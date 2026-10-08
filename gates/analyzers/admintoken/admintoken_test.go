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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			analysistest.Run(t, dir, admintoken.Analyzer, tt.pkgs...)
		})
	}
}
