package nofloat_test

import (
	"path/filepath"
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"

	"github.com/abhishekjha/close-copilot/gates/analyzers/nofloat"
)

func TestNoFloat(t *testing.T) {
	dir, err := filepath.Abs(filepath.Join("..", "testdata"))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		pkgs []string
	}{
		{"float field in internal/checks; test file exempt", []string{"internal/checks"}},
		{"untyped float in internal/books", []string{"internal/books"}},
		{"floats in internal/agent/sub", []string{"internal/agent/sub"}},
		{"local type named float64 is clean", []string{"internal/evidence/localtype"}},
		{"retrieval and llm are out of scope", []string{"internal/retrieval", "internal/llm"}},
		{"internal/agentx is out of scope", []string{"internal/agentx"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			analysistest.Run(t, dir, nofloat.Analyzer, tt.pkgs...)
		})
	}
}
