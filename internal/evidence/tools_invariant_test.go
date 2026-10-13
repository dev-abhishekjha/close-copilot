package evidence_test

// Invariants around search_documents (CC-805): no agent loop gets it.
//
// The behavioural checks live in the agent package (TestInvestigatorAllowlist
// and the injection test's assertAllowlist) and run in make check. The source
// scan below forbids the tool in investigate.go and explain.go: the
// workflow's retrieve step (CC-806) lives in workflow.go and retrieve.go, and
// explain.go only reads the stored retrieval artifacts. The positive check
// requires retrieve.go to be the only non-test file of the agent package
// that names the tool.

import (
	"os"
	"path/filepath"
	"slices"
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

func TestSearchDocumentsInvariantOnlyRetrieve(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "agent", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	var naming []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(b), "\n") {
			if !strings.HasPrefix(strings.TrimSpace(line), "//") && strings.Contains(line, "search_documents") {
				naming = append(naming, filepath.Base(f))
				break
			}
		}
	}
	if !slices.Equal(naming, []string{"retrieve.go"}) {
		t.Errorf("non-test agent files naming search_documents in code: %v, want only retrieve.go", naming)
	}
}
