package evals

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReportGolden renders score.md for each fixture run and compares it
// byte for byte with testdata/score/golden/<run>.md.
func TestReportGolden(t *testing.T) {
	for _, run := range []string{"run-pass", "run-miss", "run-clean-alarm", "run-failed", "run-injection"} {
		t.Run(run, func(t *testing.T) {
			got := RenderReport(scoreDir(t, run))
			if again := RenderReport(scoreDir(t, run)); again != got {
				t.Fatal("two renders differ")
			}
			golden := filepath.Join(scoreTestdata, "golden", run+".md")
			if *update {
				if err := os.MkdirAll(filepath.Dir(golden), 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(golden, []byte(got), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("%v (run with -update to create it)", err)
			}
			if got != string(want) {
				t.Errorf("score.md differs from %s:\n%s", golden, got)
			}
		})
	}
}

func TestReportContent(t *testing.T) {
	md := RenderReport(scoreDir(t, "run-miss"))
	for _, s := range []string{
		"# Eval score: suite-test",
		"| `unrecorded_bank_charge` | 3 | 2 | 1 | 2/3 (66.6%) |",
		`| Unauthorized writes | not measured: audit decorator \(CC-504a\)`,
		`| Investigation accuracy | not measured: no investigator \(CC-706\) |`,
		"## Missed",
		"- sharma-2026-09/E02 `unrecorded_bank_charge`",
		"repro `go run ./cmd/eval run --suite suite-test --only sharma:2026-09`",
		"## False alarms\n\nNone.",
	} {
		if !strings.Contains(md, s) {
			t.Errorf("score.md lacks %q:\n%s", s, md)
		}
	}
	alarm := RenderReport(scoreDir(t, "run-clean-alarm"))
	if !strings.Contains(alarm, "(clean_month)") || !strings.Contains(alarm, "#finding/11111111-1111-4111-8111-000000000008") {
		t.Errorf("clean-alarm report:\n%s", alarm)
	}
}

func TestReportEscapes(t *testing.T) {
	s := mustScore(t, monthIn("sharma", "2026-09", false, GroundTruth{}, fnd(1, "weird|type", k("a", "x|y`z\nq"))))
	md := RenderReport(s)
	if strings.Contains(md, "x|y") || strings.Contains(md, "weird|type") || strings.Contains(md, "y`z") {
		t.Errorf("unescaped cell:\n%s", md)
	}
}

// TestReportInjection renders a run whose key values and failure reasons
// carry Markdown and HTML: none of it may survive as a live tag, link or
// image.
func TestReportInjection(t *testing.T) {
	md := RenderReport(scoreDir(t, "run-injection"))
	// Drop every escaped character; what's left must hold no markup.
	bare := strings.NewReplacer(`\<`, "", `\>`, "", `\[`, "", `\]`, "", `\(`, "", `\)`, "", `\!`, "").Replace(md)
	for _, bad := range []string{"<img", "<", "](", "![", "[click"} {
		if strings.Contains(bare, bad) {
			t.Errorf("score.md holds live markup %q:\n%s", bad, md)
		}
	}
	for _, want := range []string{`\<img src=https://evil/x\>`, `\[click\]\(https://evil\)`, `\!\[p\]\(https://evil/p.png\)`} {
		if !strings.Contains(md, want) {
			t.Errorf("score.md lacks the escaped %q:\n%s", want, md)
		}
	}
}
