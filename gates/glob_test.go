package gates

import (
	"errors"
	"testing"
)

func TestMatch(t *testing.T) {
	tests := []struct {
		pattern, name string
		want          bool
	}{
		{"gates/*.go", "gates/glob.go", true},
		{"gates/*.go", "gates/glob_test.go", true},
		{"gates/*.go", "gates/analyzers/a.go", false},
		{"gates/*.go", "gates/glob.yaml", false},
		{"gates/**", "gates/analyzers/a/b.go", true},
		{"gates/**", "gates", true},
		{"gates/**", "gatesx/a.go", false},
		{"**/testdata/**", "internal/seed/testdata/x.json", true},
		{"**/testdata/**", "testdata/x.json", true},
		{"**", "anything/at/all", true},
		{"internal/*/testdata/schemas/**", "internal/books/testdata/schemas/a.json", true},
		{"internal/*/testdata/schemas/**", "internal/books/x/testdata/schemas/a.json", false},
		{"internal/**/doc.go", "internal/doc.go", true},
		{"internal/**/doc.go", "internal/a/b/doc.go", true},
		{"internal/mcpkit/auth*.go", "internal/mcpkit/auth.go", true},
		{"internal/mcpkit/auth*.go", "internal/mcpkit/auth_token.go", true},
		{"internal/mcpkit/auth*.go", "internal/mcpkit/server.go", false},
		{"internal/mcpkit/auth*.go", "internal/mcpkit/auth/x.go", false},
		{"cmd/?/main.go", "cmd/a/main.go", true},
		{"cmd/?/main.go", "cmd/ab/main.go", false},
		{"deploy/", "deploy/erpnext/apps.json", true},
		{"deploy/", "deployx", false},
		{".github/**", ".github/workflows/ci.yml", true},
		{"*", ".env", true},
		{"*", "a/b", false},
		{"a**b", "axxb", true},
		{"a**b", "ax/xb", false},
		{"docs/[x].md", "docs/[x].md", true},
		{"docs/[x].md", "docs/x.md", false},
		{"CLAUDE.md", "CLAUDE.md", true},
		{"CLAUDE.md", "docs/CLAUDE.md", false},
		{"évals/*.json", "évals/b.json", true},
	}
	for _, tt := range tests {
		t.Run(tt.pattern+"~"+tt.name, func(t *testing.T) {
			got, err := Match(tt.pattern, tt.name)
			if err != nil {
				t.Fatalf("Match: %v", err)
			}
			if got != tt.want {
				t.Errorf("Match(%q, %q) = %v, want %v", tt.pattern, tt.name, got, tt.want)
			}
		})
	}
}

func TestInvalidPatterns(t *testing.T) {
	tests := []struct {
		pattern    string
		wantBraces bool
	}{
		{"", false},
		{"internal/{a,b}/x.go", true},
		{"/abs/path", false},
		{"a//b", false},
		{"./a", false},
		{"a/../b", false},
	}
	for _, tt := range tests {
		t.Run(tt.pattern, func(t *testing.T) {
			err := ValidatePattern(tt.pattern)
			if err == nil {
				t.Fatalf("ValidatePattern(%q) = nil, want an error", tt.pattern)
			}
			if got := errors.Is(err, ErrBraces); got != tt.wantBraces {
				t.Errorf("errors.Is(err, ErrBraces) = %v, want %v (err %v)", got, tt.wantBraces, err)
			}
			if _, err := Match(tt.pattern, "a/b"); err == nil {
				t.Errorf("Match(%q) accepted an invalid pattern", tt.pattern)
			}
			if _, err := MatchAny([]string{"x", tt.pattern}, "a/b"); err == nil {
				t.Errorf("MatchAny accepted an invalid pattern %q", tt.pattern)
			}
		})
	}
}

func TestOverlaps(t *testing.T) {
	tests := []struct {
		a, b []string
		want bool
	}{
		// Required by the spec.
		{[]string{"gates/*.go"}, []string{"gates/analyzers/**"}, false},
		{[]string{"internal/checks/**"}, []string{"internal/checks/bankrec.go"}, true},
		{[]string{"tasks/**"}, []string{"tasks/graph.yaml"}, true},
		{[]string{"internal/mcpkit/auth*.go"}, []string{"internal/mcpkit/server.go"}, false},
		{[]string{"deploy/**"}, []string{"docs/setup.md"}, false},
		// More.
		{[]string{"a/*.go"}, []string{"a/x*"}, true},
		{[]string{"a/*_test.go"}, []string{"a/*.yaml"}, false},
		{[]string{"a/?.go"}, []string{"a/xy.go"}, false},
		{[]string{"a/?.go"}, []string{"a/*"}, true},
		{[]string{"**/testdata/**"}, []string{"internal/seed/x.go"}, false},
		{[]string{"**/testdata/**"}, []string{"internal/*/testdata/x.go"}, true},
		{[]string{"**/testdata/*.json"}, []string{"internal/seed/**"}, true},
		{[]string{"internal/*/doc.go"}, []string{"internal/**"}, true},
		{[]string{"deploy/"}, []string{"deploy/erpnext/apps.json"}, true},
		{[]string{"x/a.go", "y/**"}, []string{"z/b.go", "y/c/d.go"}, true},
		{[]string{"x/a.go"}, nil, false},
		{[]string{"internal/{a,b}/x.go"}, []string{"cmd/z.go"}, true}, // invalid counts as overlap
		{[]string{"*"}, []string{"a/b"}, false},
		{[]string{"**"}, []string{"a/b"}, true},
	}
	for _, tt := range tests {
		t.Run("", func(t *testing.T) {
			if got := Overlaps(tt.a, tt.b); got != tt.want {
				t.Errorf("Overlaps(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
			if got := Overlaps(tt.b, tt.a); got != tt.want {
				t.Errorf("Overlaps(%q, %q) = %v, want %v (not symmetric)", tt.b, tt.a, got, tt.want)
			}
		})
	}
}
