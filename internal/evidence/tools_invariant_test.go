package evidence_test

// Invariants around search_documents (CC-805): no agent loop gets it.
//
// The behavioural checks live in the agent package (TestInvestigatorAllowlist
// and the injection test's assertAllowlist) and run in make check. The source
// scan below covers only investigate.go and explain.go, because CC-806 adds
// the workflow's retrieve step in workflow.go and explain.go's input
// handling; if that ticket legitimately touches explain.go it updates this
// test with the owner's agreement.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/abhishekjha/close-copilot/internal/agent"
)

func TestSearchDocumentsInvariantAllowlist(t *testing.T) {
	for _, name := range agent.InvestigatorAllowlist() {
		l := strings.ToLower(name)
		for _, bad := range []string{"search", "document", "retriev", "admin"} {
			if strings.Contains(l, bad) {
				t.Errorf("investigator allowlist entry %q contains %q", name, bad)
			}
		}
		if name == "evidence__search_documents" {
			t.Error("investigator may call evidence__search_documents")
		}
	}
	if agent.ExplainToolName != "emit_explanation" {
		t.Errorf("ExplainToolName = %q, want emit_explanation", agent.ExplainToolName)
	}
}

func TestSearchDocumentsInvariantSourceScan(t *testing.T) {
	for _, f := range []string{"investigate.go", "explain.go"} {
		b, err := os.ReadFile(filepath.Join("..", "agent", f))
		if err != nil {
			t.Fatal(err)
		}
		// Comments may name the tool to forbid it; only code counts.
		var code []string
		for _, line := range strings.Split(string(b), "\n") {
			if !strings.HasPrefix(strings.TrimSpace(line), "//") {
				code = append(code, line)
			}
		}
		src := strings.Join(code, "\n")
		for _, bad := range []string{"search_documents", "close-copilot/internal/retrieval", "close-copilot/internal/evidence"} {
			if strings.Contains(src, bad) {
				t.Errorf("internal/agent/%s mentions %q", f, bad)
			}
		}
	}
}
