package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestNoERPClientDependency checks that the production eval binary
// reaches neither the ERPNext client nor the packages built on it, nor the
// seeder, directly or transitively: the eval runner reaches ERPNext only
// through MCP. go list -deps without -test leaves test-only imports (the
// fake ERPNext of the integration tests) out.
func TestNoERPClientDependency(t *testing.T) {
	const (
		module = "github.com/abhishekjha/close-copilot"
		pkg    = module + "/cmd/eval"
	)
	forbidden := []string{
		module + "/internal/frappe",
		module + "/internal/books",
		module + "/internal/seed",
		module + "/internal/testsupport",
	}
	for _, tags := range []string{"", "integration"} {
		out, err := exec.CommandContext(t.Context(), goTool(t), "list", "-deps", "-tags="+tags, pkg).CombinedOutput()
		if err != nil {
			t.Fatalf("go list -deps -tags=%q %s: %v\n%s", tags, pkg, err, out)
		}
		deps := strings.Fields(string(out))
		if len(deps) < 10 {
			t.Fatalf("go list -deps listed only %d packages", len(deps))
		}
		for _, dep := range deps {
			for _, f := range forbidden {
				if dep == f || strings.HasPrefix(dep, f+"/") {
					t.Errorf("%s depends on %s (tags %q); it must not reach the ERPNext client", pkg, dep, tags)
				}
			}
		}
	}
}

// goTool returns the go command of the toolchain running this test.
func goTool(t *testing.T) string {
	t.Helper()
	//nolint:staticcheck // SA1019: a test binary runs on the machine that built it, so its GOROOT is the toolchain to ask.
	if root := runtime.GOROOT(); root != "" {
		p := filepath.Join(root, "bin", "go")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	p, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("no go command: %v", err)
	}
	return p
}
