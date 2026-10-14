package approvals

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

const module = "github.com/abhishekjha/close-copilot"

// adminEnvName is the admin token's variable name, built so this file
// doesn't itself carry it as one literal.
var adminEnvName = "MCP_TOKEN_" + "ADMIN"

// skipDirs are directory names the scan never enters: the analyzer's
// fixtures under gates/analyzers/testdata use the token on purpose.
var skipDirs = map[string]bool{"vendor": true, ".worktrees": true, "tmp": true, "testdata": true, ".git": true}

// TestOnlyApprovalsPresentsAdminToken scans every non-test Go file of the
// module for the admin token's config field, its variable-name constant or
// a string holding its variable name, outside the places allowed to use
// it: internal/config, internal/mcpkit/auth*.go, internal/approvals, and
// the []string element of cmd/mcp-books/main.go that requires it at
// startup. It must find the token in internal/approvals, so it can't pass
// vacuously.
func TestOnlyApprovalsPresentsAdminToken(t *testing.T) {
	root := moduleRoot(t)
	approvalsUses := 0
	scanned := 0
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != root && skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		uses := tokenUses(t, p, rel == "cmd/mcp-books/main.go")
		scanned++
		if len(uses) == 0 {
			return nil
		}
		if strings.HasPrefix(rel, "internal/approvals/") {
			approvalsUses += len(uses)
			return nil
		}
		if allowedTokenFile(rel) {
			return nil
		}
		for _, u := range uses {
			t.Errorf("%s:%s: only internal/approvals may present the MCP admin token", rel, u)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if scanned < 50 {
		t.Fatalf("scanned only %d files under %s", scanned, root)
	}
	if approvalsUses == 0 {
		t.Fatal("found no use of the admin token in internal/approvals: the scan is not working")
	}
}

// allowedTokenFile reports whether a module-relative path may use the
// admin token anywhere.
func allowedTokenFile(rel string) bool {
	if strings.HasPrefix(rel, "internal/config/") {
		return true
	}
	ok, err := path.Match("internal/mcpkit/auth*.go", rel)
	return err == nil && ok
}

func TestAllowedTokenFile(t *testing.T) {
	cases := map[string]bool{
		"internal/config/config.go":       true,
		"internal/mcpkit/auth.go":         true,
		"internal/mcpkit/auth_extra.go":   true,
		"internal/mcpkit/http.go":         false,
		"internal/mcpkit/sub/auth.go":     false,
		"x/internal/config/config.go":     false,
		"internal/configx/config.go":      false,
		"internal/agent/internal/config/": false,
		"cmd/mcp-books/main.go":           false,
	}
	for rel, want := range cases {
		if got := allowedTokenFile(rel); got != want {
			t.Errorf("allowedTokenFile(%q) = %v, want %v", rel, got, want)
		}
	}
}

// tokenUses returns the positions in file p of a selector EnvMCPTokenAdmin
// or MCPTokenAdmin, a composite-literal key MCPTokenAdmin, or a string
// literal containing the admin variable's name. With booksMain, a
// config.EnvMCPTokenAdmin that is a direct element of a []string literal
// is not reported.
func tokenUses(t *testing.T, p string, booksMain bool) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", p, err)
	}
	exempt := map[ast.Node]bool{}
	if booksMain {
		ast.Inspect(f, func(n ast.Node) bool {
			cl, ok := n.(*ast.CompositeLit)
			if !ok || !isStringSlice(cl.Type) {
				return true
			}
			for _, el := range cl.Elts {
				if isConfigEnvAdmin(el) {
					exempt[el] = true
				}
			}
			return true
		})
	}
	var uses []string
	at := func(n ast.Node, what string) {
		uses = append(uses, strconv.Itoa(fset.Position(n.Pos()).Line)+": "+what)
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.SelectorExpr:
			switch n.Sel.Name {
			case "EnvMCPTokenAdmin":
				if !exempt[n] {
					at(n, "EnvMCPTokenAdmin")
				}
			case "MCPTokenAdmin":
				at(n, ".MCPTokenAdmin")
			}
		case *ast.KeyValueExpr:
			if id, ok := n.Key.(*ast.Ident); ok && id.Name == "MCPTokenAdmin" {
				at(n, "MCPTokenAdmin:")
			}
		case *ast.BasicLit:
			if n.Kind == token.STRING {
				if v, err := strconv.Unquote(n.Value); err == nil && strings.Contains(v, adminEnvName) {
					at(n, "string "+n.Value)
				}
			}
		}
		return true
	})
	return uses
}

func isStringSlice(e ast.Expr) bool {
	at, ok := e.(*ast.ArrayType)
	if !ok || at.Len != nil {
		return false
	}
	id, ok := at.Elt.(*ast.Ident)
	return ok && id.Name == "string"
}

func isConfigEnvAdmin(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "EnvMCPTokenAdmin" {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == "config"
}

func TestTokenUsesScanner(t *testing.T) {
	dir := t.TempDir()
	write := func(name, src string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	src := `package x
import "example/config"
var required = []string{config.EnvERPBaseURL, config.EnvMCPTokenAdmin}
var arr = [1]string{config.EnvMCPTokenAdmin}
func f(c config.Config) string { return c.MCPTokenAdmin.Reveal() }
var s = "$` + adminEnvName + `"
var k = config.Config{MCPTokenAdmin: config.NewSecret("x")}
func g() []string { return append([]string{}, config.EnvMCPTokenAdmin) }
`
	p := write("x.go", src)
	if got := tokenUses(t, p, false); len(got) != 6 {
		t.Errorf("not books main: %d uses %v, want 6", len(got), got)
	}
	// In cmd/mcp-books/main.go only the []string element is exempt; the
	// array element and the append argument are not.
	if got := tokenUses(t, p, true); len(got) != 5 {
		t.Errorf("books main: %d uses %v, want 5", len(got), got)
	}
}

// TestApprovalsDependencies checks (b) that the agent, web and store don't
// depend on internal/approvals, and (c) that approvals reaches neither
// the ERPNext client, the books server, the agent nor the seeder.
func TestApprovalsDependencies(t *testing.T) {
	approvals := module + "/internal/approvals"
	for _, pkg := range []string{"internal/agent", "internal/web", "internal/store"} {
		for _, dep := range goListDeps(t, module+"/"+pkg) {
			if dep == approvals || strings.HasPrefix(dep, approvals+"/") {
				t.Errorf("%s depends on %s", pkg, dep)
			}
		}
	}
	forbidden := []string{"internal/frappe", "internal/books", "internal/agent", "internal/seed"}
	for _, dep := range goListDeps(t, approvals) {
		for _, f := range forbidden {
			full := module + "/" + f
			if dep == full || strings.HasPrefix(dep, full+"/") {
				t.Errorf("internal/approvals depends on %s", dep)
			}
		}
	}
}

func goListDeps(t *testing.T, pkg string) []string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), goTool(t), "list", "-deps", pkg)
	cmd.Dir = moduleRoot(t)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps %s: %v\n%s", pkg, err, out)
	}
	return strings.Fields(string(out))
}

// moduleRoot is the directory holding go.mod, found from this file.
func moduleRoot(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("no caller")
	}
	dir := filepath.Dir(here)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
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
