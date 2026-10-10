package fakeerp_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const module = "github.com/abhishekjha/close-copilot"

// TestOnlyTestsImportTestSupport checks that no production package imports
// internal/testsupport/...: the fakes link the ERPNext client and serve
// synthetic data, so only tests (and the helper binaries inside
// internal/testsupport) may use them. go list's Imports excludes test
// files, which are free to import it.
func TestOnlyTestsImportTestSupport(t *testing.T) {
	const support = module + "/internal/testsupport"
	for _, tags := range []string{"", "integration"} {
		cmd := exec.CommandContext(t.Context(), goTool(t), "list", "-tags="+tags,
			"-f", `{{.ImportPath}}{{range .Imports}} {{.}}{{end}}`, module+"/...")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("go list -tags=%q: %v", tags, err)
		}
		checked := 0
		for line := range strings.Lines(string(out)) {
			fields := strings.Fields(line)
			if len(fields) == 0 {
				continue
			}
			pkg := fields[0]
			checked++
			if pkg == support || strings.HasPrefix(pkg, support+"/") {
				continue
			}
			for _, imp := range fields[1:] {
				if imp == support || strings.HasPrefix(imp, support+"/") {
					t.Errorf("%s imports %s (tags %q): only tests may import internal/testsupport", pkg, imp, tags)
				}
			}
		}
		if checked < 20 {
			t.Fatalf("go list -tags=%q listed only %d packages", tags, checked)
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
