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

// TestReportReasonRedacted is the golden case for run error text: URLs
// and DSNs are redacted, the text is capped at 200 runes, and it points
// to the result file.
func TestReportReasonRedacted(t *testing.T) {
	m := monthIn("sharma", "2026-09", false, threeCharges())
	m.Result.Status = "failed"
	m.Result.Reason = "checking: list_bank_lines"
	m.Result.Error = "dial postgres://copilot:sekret@db:5432/copilot?sslmode=disable: connection refused; " +
		"retry copilot:hunter2@db failed; host=db password=sekret2 user=copilot; see https://evil.example/x?token=abc"
	s := mustScore(t, m)
	got := s.FailedRuns[0].Reason
	want := "checking: list_bank_lines; dial [redacted-url] connection refused; retry [redacted-url] failed; host=db [redacted-url] user=copilot; " +
		"see [redacted-url] (full text: r/sharma-2026-09.json)"
	if got != want {
		t.Errorf("reason\n got %q\nwant %q", got, want)
	}
	for _, leak := range []string{"sekret", "hunter2", "evil.example", "postgres://"} {
		if strings.Contains(RenderReport(s), leak) {
			t.Errorf("score.md carries %q", leak)
		}
	}

	long := monthIn("sharma", "2026-09", false, threeCharges())
	long.Result.Status = "failed"
	long.Result.Error = strings.Repeat("é", 300)
	r := mustScore(t, long).FailedRuns[0].Reason
	if want := strings.Repeat("é", 200) + "... (full text: r/sharma-2026-09.json)"; r != want {
		t.Errorf("capped reason %q", r)
	}
	if RunErrorText("", "x") != "" {
		t.Error("empty text got a pointer")
	}
}

// TestReportSummary checks summary.md: the verdict, each comparison
// failure with its item, outcome, repro and evidence, the failed
// requirements, the skipped rules, then score.md.
func TestReportSummary(t *testing.T) {
	pass, miss := scoreDir(t, "run-pass"), scoreDir(t, "run-miss")
	b, _ := NewBaseline(pass, nil, "")
	c, err := Compare(b, miss)
	if err != nil {
		t.Fatal(err)
	}
	sm := Summary{Requirements: []string{"requirement unrecorded_bank_charge.recall>=3/3 failed: is 2/3"}, Compare: &c,
		Notes: []string{"p95 latency rule skipped: --max-p95-increase-pct was not passed"}}
	md := RenderSummary(miss, sm)
	for _, want := range []string{"# Eval gate (G4): FAIL", "| sharma-2026-09/E02 | regression | caught | missed |",
		"`go run ./cmd/eval run --suite suite-test --only sharma:2026-09`", "#E02`", "recall\\>=3/3", "## Skipped rules",
		"--max-p95-increase-pct was not passed", "# Eval score: suite-test"} {
		if !strings.Contains(md, want) {
			t.Errorf("summary lacks %q:\n%s", want, md)
		}
	}
	ok := RenderSummary(pass, Summary{Compare: &Comparison{}})
	if !strings.Contains(ok, "# Eval gate (G4): PASS") || !strings.Contains(ok, "No regressions.") {
		t.Errorf("passing summary:\n%s", ok)
	}
	dir := t.TempDir()
	if err := WriteSummary(dir, miss, sm); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, SummaryFile)); string(got) != md {
		t.Error("summary.md differs from the render")
	}
}
