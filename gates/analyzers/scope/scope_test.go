package scope

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIn(t *testing.T) {
	tests := []struct {
		name string
		path string
		dirs []string
		want bool
	}{
		{"module package", Module + "/internal/agent", []string{"internal/agent"}, true},
		{"module subpackage", Module + "/internal/agent/sub", []string{"internal/agent"}, true},
		{"module sibling prefix", Module + "/internal/agentx", []string{"internal/agent"}, false},
		{"module external test package", Module + "/internal/agent_test", []string{"internal/agent"}, true},
		{"module test variant", Module + "/internal/agent [" + Module + "/internal/agent.test]", []string{"internal/agent"}, true},
		{"gopath package", "internal/agent", []string{"internal/agent"}, true},
		{"gopath subpackage", "internal/agent/bad", []string{"internal/agent"}, true},
		{"gopath sibling prefix", "internal/agentx", []string{"internal/agent"}, false},
		{"other dir", Module + "/internal/retrieval", []string{"internal/checks", "internal/books"}, false},
		{"second of several", Module + "/internal/books", []string{"internal/checks", "internal/books"}, true},
		{"third-party internal dir", "example.com/x/internal/agent", []string{"internal/agent"}, false},
		{"nested internal dir", Module + "/internal/web/internal/agent", []string{"internal/agent"}, false},
		{"module root", Module, []string{"internal/agent"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := In(tt.path, tt.dirs...); got != tt.want {
				t.Errorf("In(%q, %q) = %v, want %v", tt.path, tt.dirs, got, tt.want)
			}
		})
	}
}

func TestIs(t *testing.T) {
	tests := []struct {
		path, dir string
		want      bool
	}{
		{Module + "/internal/httpx", "internal/httpx", true},
		{"internal/httpx", "internal/httpx", true},
		{Module + "/internal/httpx/sub", "internal/httpx", false},
		{Module + "/internal/httpxy", "internal/httpx", false},
		{Module + "/internal/mcpkit_test", "internal/mcpkit", true},
	}
	for _, tt := range tests {
		if got := Is(tt.path, tt.dir); got != tt.want {
			t.Errorf("Is(%q, %q) = %v, want %v", tt.path, tt.dir, got, tt.want)
		}
	}
}

func TestIsTestFile(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"/a/b/foo_test.go", true},
		{"/a/b/foo.go", false},
		{"/a/b/test.go", false},
	}
	for _, tt := range tests {
		if got := IsTestFile(tt.name); got != tt.want {
			t.Errorf("IsTestFile(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// TestModuleMatchesGoMod keeps Module in sync with go.mod: a renamed module
// would otherwise silently put every package outside every scope.
func TestModuleMatchesGoMod(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if mod, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			if got := strings.TrimSpace(mod); got != Module {
				t.Fatalf("go.mod module is %q, scope.Module is %q", got, Module)
			}
			return
		}
	}
	t.Fatal("go.mod has no module line")
}
