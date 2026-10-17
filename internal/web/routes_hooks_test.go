package web

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

var hookFiles = map[string]string{
	"dashboardRoutes":    "routes_dashboard.go",
	"findingsListRoutes": "routes_findings.go",
	"proposalsRoutes":    "routes_proposals.go",
	"documentsRoutes":    "routes_documents.go",
	"systemRoutes":       "routes_system.go",
	"auditRoutes":        "routes_audit.go",
}

func readSrc(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRouteHooksEmpty(t *testing.T) {
	s := &Server{}
	hooks := map[string]func() []route{
		"dashboard": s.dashboardRoutes,
		"documents": s.documentsRoutes,
		"audit":     s.auditRoutes,
	}
	for name, h := range hooks {
		if got := h(); len(got) != 0 {
			t.Errorf("%s hook returned %d routes, want none", name, len(got))
		}
	}
}

func TestRouteHooksTableUnchanged(t *testing.T) {
	s := &Server{}
	seen := map[string]bool{}
	for _, rt := range s.routeTable(nil) {
		key := rt.method + " " + rt.path
		if seen[key] {
			t.Errorf("duplicate route %s", key)
		}
		seen[key] = true
		if rt.handler == nil && rt.path != "/static/" {
			t.Errorf("route %s has a nil handler", key)
		}
	}
}

func TestRouteHooksAppended(t *testing.T) {
	src := readSrc(t, "routes.go")
	decl := regexp.MustCompile(`(?m)^(func|type|var|const)\b`)
	for name, file := range hookFiles {
		if n := strings.Count(src, "s."+name+"()"); n != 1 {
			t.Errorf("routes.go references %s %d times, want 1", name, n)
		}
		hs := readSrc(t, file)
		if got := len(decl.FindAllString(hs, -1)); got != 1 || !strings.Contains(hs, ") "+name+"() []route") {
			t.Errorf("%s must declare only %s", file, name)
		}
	}
}

func TestRouteHooksHomeMoved(t *testing.T) {
	src := readSrc(t, "routes.go")
	home := readSrc(t, "home.go")
	for _, d := range []string{"func (s *Server) handleHome", "var homeActions", "type homeAction "} {
		if strings.Contains(src, d) {
			t.Errorf("routes.go still declares %q", d)
		}
		if !strings.Contains(home, d) {
			t.Errorf("home.go does not declare %q", d)
		}
	}
}
