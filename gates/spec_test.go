package gates

import (
	"bufio"
	"os"
	"slices"
	"strings"
	"testing"
)

func TestParseSpec(t *testing.T) {
	s, err := ParseSpec([]byte(validSpec(t)))
	if err != nil {
		t.Fatal(err)
	}
	if s.ID != "CC-500" || s.Phase == nil || *s.Phase != 1 || s.Risk != RiskRegulated ||
		s.ApprovedBy != "owner 2026-10-07" || !slices.Equal(s.DependsOn, []string{"CC-101"}) ||
		len(s.Acceptance) != 5 || s.Acceptance[1] != "! go run ./cmd/foo --bad-flag" ||
		s.Budget == nil || s.Budget.MaxWallMinutes != 60 || s.MaxAttempts() != 3 {
		t.Errorf("unexpected spec: %+v", s)
	}
}

func TestParseSpecErrors(t *testing.T) {
	tests := map[string]string{
		"no front matter":    "# CC-500\n",
		"not on line one":    "\n---\nid: CC-500\n---\n",
		"unclosed":           "---\nid: CC-500\n",
		"empty":              "---\n---\n",
		"unknown field":      "---\nid: CC-500\nowner: implementer\n---\n",
		"wrong type":         "---\nphase: one\n---\n",
		"invalid yaml":       "---\nfiles: [a\n---\n",
		"empty file":         "",
		"closing at the end": "---\nid: CC-500",
	}
	for name, text := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseSpec([]byte(text)); err == nil {
				t.Errorf("ParseSpec accepted %q", text)
			}
		})
	}
}

func TestParseSpecCRLF(t *testing.T) {
	s, err := ParseSpec([]byte("---\r\nid: CC-500\r\nphase: 0\r\n---\r\nbody\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if s.ID != "CC-500" || s.Phase == nil || *s.Phase != 0 {
		t.Errorf("spec = %+v", s)
	}
}

func TestRepoTemplateAndSpecParse(t *testing.T) {
	for _, p := range []string{"../specs/_template.md", "../specs/CC-001.md"} {
		if _, err := LoadSpec(p); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
}

// TestCodeownersCoversProtectedPaths keeps .github/CODEOWNERS in step with
// ProtectedPaths.
func TestCodeownersCoversProtectedPaths(t *testing.T) {
	const owner = "@dev-abhishekjha"
	f, err := os.Open("../.github/CODEOWNERS")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	owned := make(map[string]bool)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) >= 2 && !strings.HasPrefix(fields[0], "#") && slices.Contains(fields[1:], owner) {
			owned[fields[0]] = true
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	for _, p := range ProtectedPaths {
		want := "/" + p
		if dir, ok := strings.CutSuffix(p, "/**"); ok {
			want = "/" + dir + "/"
		}
		if !owned[want] {
			t.Errorf("CODEOWNERS has no line %q %s for protected path %s", want, owner, p)
		}
	}
}
