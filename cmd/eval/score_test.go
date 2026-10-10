package main

import (
	"bytes"
	"encoding/json"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/abhishekjha/close-copilot/internal/cli"
	"github.com/abhishekjha/close-copilot/internal/config"
)

// scoreData is the scorer's synthetic testdata.
var scoreData = filepath.Join("..", "..", "internal", "evals", "testdata", "score")

// runCLI runs the eval binary's entry point with an empty environment (no
// DATABASE_URL), as main would, and returns the exit code and output.
func runCLI(t *testing.T, args ...string) (code int, out, errOut string) {
	t.Helper()
	var o, e bytes.Buffer
	oldOut, oldErr := stdout, stderr
	stdout, stderr = &o, &e
	defer func() { stdout, stderr = oldOut, oldErr }()
	load := func(req ...string) (config.Config, error) {
		return config.Load(func(string) (string, bool) { return "", false }, req...)
	}
	code = cli.Run("eval", args, &o, &e, load, requiredFor(args), run)
	return code, o.String(), e.String()
}

func TestScoreCLI(t *testing.T) {
	truth := filepath.Join(scoreData, "truth")
	reqs := []string{"--require", "unrecorded_bank_charge.recall>=3/3", "--require", "clean.false_alarms==0"}

	t.Run("pass", func(t *testing.T) {
		out := t.TempDir()
		code, stdoutText, errText := runCLI(t, append([]string{"score", filepath.Join(scoreData, "run-pass"), "--truth-dir", truth, "--out", out}, reqs...)...)
		if code != 0 {
			t.Fatalf("exit %d\nstdout %s\nstderr %s", code, stdoutText, errText)
		}
		for _, f := range []string{"score.json", "score.md"} {
			b, err := os.ReadFile(filepath.Join(out, f))
			if err != nil || len(b) == 0 {
				t.Errorf("%s: %d bytes, %v", f, len(b), err)
			}
		}
		var s map[string]any
		b, _ := os.ReadFile(filepath.Join(out, "score.json"))
		if err := json.Unmarshal(b, &s); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(stdoutText, filepath.Join(out, "score.json")) || !strings.Contains(stdoutText, "recall 3/3") {
			t.Errorf("stdout %q", stdoutText)
		}
	})

	t.Run("miss", func(t *testing.T) {
		out := t.TempDir()
		code, _, errText := runCLI(t, append([]string{"score", filepath.Join(scoreData, "run-miss"), "--truth-dir", truth, "--out", out}, reqs...)...)
		if code == 0 {
			t.Fatal("exit 0 on 2 of 3 caught")
		}
		if !strings.Contains(errText, "unrecorded_bank_charge.recall>=3/3") || !strings.Contains(errText, "is 2/3") {
			t.Errorf("stderr does not name the requirement:\n%s", errText)
		}
		if strings.Contains(errText, "clean.false_alarms==0 failed") {
			t.Errorf("clean requirement failed:\n%s", errText)
		}
		if _, err := os.Stat(filepath.Join(out, "score.json")); err != nil {
			t.Errorf("score.json not written on failure: %v", err)
		}
	})

	t.Run("no denominator fails before scoring", func(t *testing.T) {
		out := t.TempDir()
		code, _, errText := runCLI(t, "score", filepath.Join(scoreData, "run-pass"), "--truth-dir", truth, "--out", out,
			"--require", "unrecorded_bank_charge.recall>=3")
		if code == 0 || !strings.Contains(errText, "needs N/M") {
			t.Fatalf("exit %d, stderr %s", code, errText)
		}
		if _, err := os.Stat(filepath.Join(out, "score.json")); !os.IsNotExist(err) {
			t.Errorf("scored despite a malformed requirement: %v", err)
		}
	})

	t.Run("unauthorized writes unchecked", func(t *testing.T) {
		code, _, errText := runCLI(t, "score", filepath.Join(scoreData, "run-pass"), "--truth-dir", truth, "--out", t.TempDir(),
			"--require", "unauthorized_writes==0")
		if code == 0 || !strings.Contains(errText, "unauthorized_writes==0") || !strings.Contains(errText, "not checked") {
			t.Fatalf("exit %d, stderr %s", code, errText)
		}
	})

	t.Run("missing truth", func(t *testing.T) {
		code, _, errText := runCLI(t, "score", filepath.Join(scoreData, "run-pass"), "--truth-dir", t.TempDir(), "--out", t.TempDir())
		if code == 0 || !strings.Contains(errText, "no ground truth file") {
			t.Fatalf("exit %d, stderr %s", code, errText)
		}
	})

	t.Run("baseline out, then compare", func(t *testing.T) {
		dir := t.TempDir()
		pricing := filepath.Join(dir, "pricing.yaml")
		if err := os.WriteFile(pricing, []byte("models: {}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		base := filepath.Join(dir, "candidate.json")
		code, _, errText := runCLI(t, "score", filepath.Join(scoreData, "run-pass"), "--truth-dir", truth, "--out", dir,
			"--baseline-out", base, "--pricing", pricing)
		if code != 0 {
			t.Fatalf("baseline-out: exit %d, %s", code, errText)
		}
		var b struct {
			Version int `json:"version"`
			Tiers   map[string]*struct {
				PricingVersion string `json:"pricing_version"`
			} `json:"tiers"`
		}
		raw, _ := os.ReadFile(base)
		if err := json.Unmarshal(raw, &b); err != nil || b.Version != 2 || b.Tiers["replay"] == nil ||
			len(b.Tiers["replay"].PricingVersion) != 64 || b.Tiers["llm"] != nil {
			t.Fatalf("candidate %s: %v", raw, err)
		}
		code, _, errText = runCLI(t, "score", filepath.Join(scoreData, "run-pass"), "--truth-dir", truth, "--out", t.TempDir(), "--compare", base)
		if code != 0 {
			t.Errorf("compare with itself: exit %d, %s", code, errText)
		}
		code, _, errText = runCLI(t, "score", filepath.Join(scoreData, "run-miss"), "--truth-dir", truth, "--out", t.TempDir(), "--compare", base)
		if code == 0 || !strings.Contains(errText, "regression sharma-2026-09/E02") {
			t.Errorf("compare regression: exit %d, %s", code, errText)
		}
		code, _, errText = runCLI(t, "score", filepath.Join(scoreData, "run-pass"), "--truth-dir", truth, "--out", t.TempDir(),
			"--compare", filepath.Join(dir, "missing.json"))
		if code == 0 || !strings.Contains(errText, "baseline") {
			t.Errorf("missing baseline: exit %d, %s", code, errText)
		}
	})

	t.Run("baseline out refuses evals/baseline.json", func(t *testing.T) {
		for _, p := range []string{"evals/baseline.json", "./evals/../evals/baseline.json"} {
			code, _, errText := runCLI(t, "score", filepath.Join(scoreData, "run-pass"), "--truth-dir", truth, "--out", t.TempDir(),
				"--baseline-out", p)
			if code == 0 || !strings.Contains(errText, "refusing") {
				t.Errorf("%s: exit %d, %s", p, code, errText)
			}
		}
	})

	t.Run("bad arguments", func(t *testing.T) {
		for _, args := range [][]string{
			{"score"},
			{"score", "a", "b"},
			{"score", "a", "--noise", "n.json"},
			{"score", "a", "--bogus"},
		} {
			if code, _, _ := runCLI(t, args...); code == 0 {
				t.Errorf("%q exited 0", args)
			}
		}
	})
}

func TestNoiseCLI(t *testing.T) {
	truth := filepath.Join(scoreData, "truth")
	a, b := t.TempDir(), t.TempDir()
	for dir, run := range map[string]string{a: "run-pass", b: "run-miss"} {
		if code, _, errText := runCLI(t, "score", filepath.Join(scoreData, run), "--truth-dir", truth, "--out", dir); code != 0 {
			t.Fatalf("score %s: %s", run, errText)
		}
	}
	out := filepath.Join(t.TempDir(), "noise.json")
	code, _, errText := runCLI(t, "noise", filepath.Join(a, "score.json"), filepath.Join(b, "score.json"), "--out", out)
	if code != 0 {
		t.Fatalf("noise with two inputs: exit %d, %s", code, errText)
	}
	var n struct {
		Suite  string           `json:"suite"`
		Runs   int              `json:"runs"`
		Spread map[string]int64 `json:"spread"`
	}
	raw, _ := os.ReadFile(out)
	if err := json.Unmarshal(raw, &n); err != nil {
		t.Fatal(err)
	}
	if n.Runs != 2 || n.Suite != "suite-test" || n.Spread["unrecorded_bank_charge.caught"] != 1 || n.Spread["verified"] != 1 {
		t.Errorf("noise %s", raw)
	}

	code, _, errText = runCLI(t, "noise", filepath.Join(a, "score.json"), "--out", filepath.Join(t.TempDir(), "n.json"))
	if code == 0 || !strings.Contains(errText, "two or more") {
		t.Errorf("noise with one input: exit %d, %s", code, errText)
	}
	if code, _, _ := runCLI(t, "noise", filepath.Join(a, "score.json"), filepath.Join(b, "score.json")); code == 0 {
		t.Error("noise without --out exited 0")
	}

	// A candidate baseline records the noise.
	dir := t.TempDir()
	pricing := filepath.Join(dir, "pricing.yaml")
	if err := os.WriteFile(pricing, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cand := filepath.Join(dir, "cand.json")
	if code, _, errText := runCLI(t, "score", filepath.Join(scoreData, "run-pass"), "--truth-dir", truth, "--out", dir,
		"--baseline-out", cand, "--noise", out, "--pricing", pricing); code != 0 {
		t.Fatalf("baseline with noise: %s", errText)
	}
	raw, _ = os.ReadFile(cand)
	if !strings.Contains(string(raw), `"unrecorded_bank_charge.caught": 1`) {
		t.Errorf("candidate lacks the noise:\n%s", raw)
	}
}

func TestScoreCLINoEnvForRunOnly(t *testing.T) {
	if got := requiredFor([]string{"score", "x"}); got != nil {
		t.Errorf("score requires %v", got)
	}
	if got := requiredFor([]string{"noise", "x"}); got != nil {
		t.Errorf("noise requires %v", got)
	}
	if got := requiredFor([]string{"run", "--suite", "s"}); len(got) != len(required) {
		t.Errorf("run requires %v", got)
	}
	if got := requiredFor([]string{"-version"}); got != nil {
		t.Errorf("-version requires %v", got)
	}
	// run without the environment fails on the missing variables.
	code, _, errText := runCLI(t, "run", "--suite", "s")
	if code == 0 || !strings.Contains(errText, config.EnvDatabaseURL) {
		t.Errorf("run with no env: exit %d, %s", code, errText)
	}
}

// TestDepsScorer keeps the scorer's own files independent of the code it
// grades: the binary links internal/checks through eval run's workflow,
// so the check is on the scorer's imports, not the binary's.
func TestDepsScorer(t *testing.T) {
	forbidden := []string{"/internal/checks", "/internal/agent", "/internal/seed", "/internal/frappe", "/internal/books"}
	files := []string{"score.go"}
	for _, f := range []string{"score.go", "truth.go", "require.go", "baseline.go", "report.go"} {
		files = append(files, filepath.Join("..", "..", "internal", "evals", f))
	}
	for _, name := range files {
		f, err := parser.ParseFile(token.NewFileSet(), name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			for _, fb := range forbidden {
				if strings.HasSuffix(path, fb) || strings.Contains(path, fb+"/") {
					t.Errorf("%s imports %s", name, path)
				}
			}
		}
	}
}

// TestScoreCLIProtectedOutputs checks that score --out, --baseline-out and
// noise --out all refuse evals/baseline.json and anything under
// evals/scenarios/, evals/golden/ or gates/, through .. and symlinks, and
// write nothing there.
func TestScoreCLIProtectedOutputs(t *testing.T) {
	truth := filepath.Join(scoreData, "truth")
	root := t.TempDir()
	for _, d := range []string{"evals/scenarios/suite-test", "evals/golden", "gates", "tmp"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(root, "evals", "scenarios"), filepath.Join(root, "scen-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "gates"), filepath.Join(root, "gates-link")); err != nil {
		t.Fatal(err)
	}
	pricing := filepath.Join(root, "tmp", "pricing.yaml")
	if err := os.WriteFile(pricing, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	protected := []string{
		filepath.Join(root, "evals", "scenarios", "suite-test"),
		filepath.Join(root, "tmp", "..", "evals", "scenarios", "suite-test", "ground_truth"),
		filepath.Join(root, "evals", "golden"),
		filepath.Join(root, "EVALS", "Golden", "x"),
		filepath.Join(root, "gates"),
		filepath.Join(root, "tmp", "..", "gates", "new"),
		filepath.Join(root, "scen-link", "suite-test"),
		filepath.Join(root, "gates-link", "not-yet"),
	}
	run := filepath.Join(scoreData, "run-pass")
	for _, p := range protected {
		code, _, errText := runCLI(t, "score", run, "--truth-dir", truth, "--out", p)
		if code == 0 || !strings.Contains(errText, "refusing --out") {
			t.Errorf("score --out %s: exit %d, %s", p, code, errText)
		}
		code, _, errText = runCLI(t, "score", run, "--truth-dir", truth, "--out", filepath.Join(root, "tmp"),
			"--baseline-out", filepath.Join(p, "b.json"), "--pricing", pricing)
		if code == 0 || !strings.Contains(errText, "refusing --baseline-out") {
			t.Errorf("--baseline-out %s: exit %d, %s", p, code, errText)
		}
	}
	for _, p := range []string{filepath.Join(root, "evals", "baseline.json"), filepath.Join(root, "evals", "..", "evals", "baseline.json")} {
		code, _, errText := runCLI(t, "score", run, "--truth-dir", truth, "--out", filepath.Join(root, "tmp"),
			"--baseline-out", p, "--pricing", pricing)
		if code == 0 || !strings.Contains(errText, "refusing --baseline-out") {
			t.Errorf("--baseline-out %s: exit %d, %s", p, code, errText)
		}
	}

	// noise --out.
	a, b := filepath.Join(root, "tmp", "a"), filepath.Join(root, "tmp", "b")
	for _, d := range []string{a, b} {
		if code, _, errText := runCLI(t, "score", run, "--truth-dir", truth, "--out", d); code != 0 {
			t.Fatalf("score --out %s: %s", d, errText)
		}
	}
	outs := []string{filepath.Join(root, "evals", "baseline.json")}
	for _, p := range protected {
		outs = append(outs, filepath.Join(p, "n.json"))
	}
	for _, p := range outs {
		code, _, errText := runCLI(t, "noise", filepath.Join(a, "score.json"), filepath.Join(b, "score.json"), "--out", p)
		if code == 0 || !strings.Contains(errText, "refusing --out") {
			t.Errorf("noise --out %s: exit %d, %s", p, code, errText)
		}
	}

	// Nothing was written under the protected folders.
	for _, d := range []string{"evals/scenarios", "evals/golden", "gates"} {
		err := filepath.WalkDir(filepath.Join(root, d), func(path string, e os.DirEntry, err error) error {
			if err == nil && !e.IsDir() {
				t.Errorf("written: %s", path)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "evals", "baseline.json")); !os.IsNotExist(err) {
		t.Errorf("evals/baseline.json: %v", err)
	}
}
