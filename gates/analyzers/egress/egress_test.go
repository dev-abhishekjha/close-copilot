package egress_test

import (
	"path/filepath"
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"github.com/abhishekjha/close-copilot/gates/analyzers/egress"
)

func TestEgress(t *testing.T) {
	dir, err := filepath.Abs(filepath.Join("..", "testdata"))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		pkgs []string
	}{
		{"default client and literals in internal/frappe; test file exempt", []string{"internal/frappe"}},
		{"internal/httpx may build clients", []string{"internal/httpx"}},
		{"injected client is clean", []string{"internal/web"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			analysistest.Run(t, dir, egress.Analyzer, tt.pkgs...)
		})
	}
}
