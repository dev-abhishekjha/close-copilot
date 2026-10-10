package evals

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBaselineRefusesProtectedPath(t *testing.T) {
	b, err := NewBaseline(scoreDir(t, "run-pass"), nil, "abc")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "evals"), 0o750); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	for _, p := range []string{
		"evals/baseline.json",
		"./evals/../evals/baseline.json",
		filepath.Join(root, "evals", "baseline.json"),
		"EVALS/Baseline.JSON",
		"elsewhere/../evals/baseline.json",
	} {
		if err := WriteBaseline(p, b); !errors.Is(err, ErrProtectedBaseline) {
			t.Errorf("WriteBaseline(%q) = %v, want ErrProtectedBaseline", p, err)
		}
	}
	// A symlink to evals/baseline.json is refused too.
	if err := os.WriteFile(filepath.Join(root, "evals", "baseline.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "evals", "baseline.json"), filepath.Join(root, "link.json")); err != nil {
		t.Fatal(err)
	}
	if err := WriteBaseline("link.json", b); !errors.Is(err, ErrProtectedBaseline) {
		t.Errorf("symlink = %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "evals", "baseline.json")); string(got) != "{}" {
		t.Errorf("evals/baseline.json was changed: %s", got)
	}
	if err := WriteBaseline("candidate.json", b); err != nil {
		t.Errorf("candidate: %v", err)
	}
	if err := WriteNoise("evals/baseline.json", Noise{Suite: "s", Runs: 2}); !errors.Is(err, ErrProtectedBaseline) {
		t.Errorf("noise to evals/baseline.json = %v", err)
	}
}

func TestBaselineWriteAndLoad(t *testing.T) {
	s := scoreDir(t, "run-pass")
	noise := Noise{Suite: "suite-test", Runs: 5, Spread: map[string]int64{"verified": 1, "unrecorded_bank_charge.caught": 0}}
	b, err := NewBaseline(s, &noise, "deadbeef")
	if err != nil {
		t.Fatal(err)
	}
	if b.Items["sharma-2026-09/E02"] != OutcomeCaught || len(b.Items) != 3 || b.PricingVersion != "deadbeef" ||
		b.Commit != "dev-abc123" || b.Models.Fast != "haiku" || b.Noise["verified"] != 1 || b.Investigations["sharma-2026-09/X01"] != OutcomeSurfaced {
		t.Errorf("baseline %+v", b)
	}
	path := filepath.Join(t.TempDir(), "baseline.json")
	if err := WriteBaseline(path, b); err != nil {
		t.Fatal(err)
	}
	got, err := LoadBaseline(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Items["sharma-2026-09/E01"] != OutcomeCaught || got.Aggregates.Types[bc].Caught != 3 {
		t.Errorf("loaded %+v", got)
	}
	if _, err := NewBaseline(s, &Noise{Suite: "other"}, ""); err == nil {
		t.Error("noise for another suite was accepted")
	}
	empty, err := NewBaseline(s, nil, "")
	if err != nil || empty.Noise == nil || len(empty.Noise) != 0 {
		t.Errorf("no noise: %+v %v", empty.Noise, err)
	}
}

func TestBaselinePricingVersion(t *testing.T) {
	p := filepath.Join(t.TempDir(), "pricing.yaml")
	if err := os.WriteFile(p, []byte("a"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := PricingVersion(p)
	if err != nil || got != "ca978112ca1bbdcafac231b39a23dc4da786eff8147c4e72b9807785afee48bb" {
		t.Errorf("PricingVersion = %s, %v", got, err)
	}
	if _, err := PricingVersion(filepath.Join(t.TempDir(), "none.yaml")); err == nil {
		t.Error("missing pricing file accepted")
	}
}

func TestBaselineLoadErrors(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"malformed.json": `{"version":1,`,
		"unknown.json":   `{"version":1,"suite":"suite-test","items":{},"bogus":1}`,
		"version.json":   `{"version":2,"suite":"suite-test","items":{}}`,
		"noitems.json":   `{"version":1,"suite":"suite-test"}`,
		"nosuite.json":   `{"version":1,"items":{}}`,
		"trailing.json":  `{"version":1,"suite":"suite-test","items":{}} {}`,
	} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadBaseline(p); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := LoadBaseline(filepath.Join(dir, "missing.json")); err == nil {
		t.Error("missing baseline accepted")
	}
}

func TestCompare(t *testing.T) {
	pass := scoreDir(t, "run-pass")
	base, err := NewBaseline(pass, nil, "x")
	if err != nil {
		t.Fatal(err)
	}

	t.Run("no change", func(t *testing.T) {
		c, err := Compare(base, pass)
		if err != nil || len(c.Failures) != 0 || len(c.New) != 0 {
			t.Errorf("compare = %+v, %v", c, err)
		}
	})
	t.Run("regression", func(t *testing.T) {
		c, err := Compare(base, scoreDir(t, "run-miss"))
		if err != nil {
			t.Fatal(err)
		}
		if len(c.Failures) != 1 || c.Failures[0].Kind != CompareRegression || c.Failures[0].Key != "sharma-2026-09/E02" ||
			c.Failures[0].Was != OutcomeCaught || c.Failures[0].Now != OutcomeMissed || c.Failures[0].Repro == "" || c.Failures[0].Evidence == "" {
			t.Errorf("failures %+v", c.Failures)
		}
		if line := c.Failures[0].String(); !strings.Contains(line, "regression sharma-2026-09/E02 (was caught, now missed)") || !strings.Contains(line, "repro:") {
			t.Errorf("line %q", line)
		}
	})
	t.Run("new item", func(t *testing.T) {
		b := base
		b.Items = map[string]string{"sharma-2026-09/E01": OutcomeCaught, "sharma-2026-09/E02": OutcomeCaught}
		c, err := Compare(b, pass)
		if err != nil || len(c.Failures) != 0 || len(c.New) != 1 || c.New[0].Key != "sharma-2026-09/E03" || c.New[0].Kind != CompareNew {
			t.Errorf("compare = %+v, %v", c, err)
		}
	})
	t.Run("missed in the baseline, still missed", func(t *testing.T) {
		miss := scoreDir(t, "run-miss")
		b, _ := NewBaseline(miss, nil, "")
		c, err := Compare(b, miss)
		if err != nil || len(c.Failures) != 0 {
			t.Errorf("compare = %+v, %v", c, err)
		}
		c, err = Compare(b, pass) // an improvement is not a failure
		if err != nil || len(c.Failures) != 0 {
			t.Errorf("improvement = %+v, %v", c, err)
		}
	})
	t.Run("removed item", func(t *testing.T) {
		b := base
		b.Items = map[string]string{"sharma-2026-09/E01": OutcomeCaught, "sharma-2026-09/E02": OutcomeCaught,
			"sharma-2026-09/E03": OutcomeCaught, "sharma-2026-09/E04": OutcomeCaught}
		c, _ := Compare(b, pass)
		if len(c.Failures) != 1 || c.Failures[0].Kind != CompareRemoved || c.Failures[0].Key != "sharma-2026-09/E04" {
			t.Errorf("failures %+v", c.Failures)
		}
	})
	t.Run("clean month false alarm", func(t *testing.T) {
		c, _ := Compare(base, scoreDir(t, "run-clean-alarm"))
		var kinds []string
		for _, f := range c.Failures {
			kinds = append(kinds, f.Kind)
		}
		if len(c.Failures) != 1 || c.Failures[0].Kind != CompareCleanFalseAlarm || c.Failures[0].Key != "sharma-2026-08/"+fid(8).String() {
			t.Errorf("failures %v %+v", kinds, c.Failures)
		}
	})
	t.Run("failed clean month", func(t *testing.T) {
		m := monthIn("sharma", "2026-09", false, threeCharges())
		cm := monthIn("sharma", "2026-08", true, GroundTruth{Clean: true})
		cm.Entry.Failed = true
		s := mustScore(t, m, cm)
		b, _ := NewBaseline(s, nil, "")
		c, _ := Compare(b, s)
		if len(c.Failures) != 1 || c.Failures[0].Kind != CompareCleanMonthFailed {
			t.Errorf("failures %+v", c.Failures)
		}
	})
	t.Run("verified noise tolerance", func(t *testing.T) {
		b := base
		b.Aggregates.VerifiedRate = NewRatio(3, 7) // one more than now: within the one-item floor
		if c, _ := Compare(b, pass); len(c.Failures) != 0 {
			t.Errorf("drop of 1: %+v", c.Failures)
		}
		b.Aggregates.VerifiedRate = NewRatio(4, 7)
		if c, _ := Compare(b, pass); len(c.Failures) != 1 || c.Failures[0].Kind != CompareVerifiedDrop {
			t.Errorf("drop of 2: %+v", c.Failures)
		}
		b.Noise = map[string]int64{NoiseVerified: 2}
		if c, _ := Compare(b, pass); len(c.Failures) != 0 {
			t.Errorf("drop of 2 with noise 2: %+v", c.Failures)
		}
	})
	t.Run("other suite", func(t *testing.T) {
		b := base
		b.Suite = "suite-v1"
		if _, err := Compare(b, pass); err == nil {
			t.Error("baseline for another suite accepted")
		}
	})
}

func TestNoise(t *testing.T) {
	a, b, c := scoreDir(t, "run-pass"), scoreDir(t, "run-miss"), scoreDir(t, "run-clean-alarm")
	n, err := ComputeNoise([]Score{a, b, c})
	if err != nil {
		t.Fatal(err)
	}
	// caught: 3, 2, 3; verified: 2, 1, 2 (run-clean-alarm: E01, E02 verified); clean alarms: 0, 0, 1.
	want := map[string]int64{"unrecorded_bank_charge.caught": 1, "unmatched_bank_line.caught": 0, NoiseVerified: 1, NoiseCleanAlarms: 1}
	for k, v := range want {
		if n.Spread[k] != v {
			t.Errorf("spread[%s] = %d, want %d (all %v)", k, n.Spread[k], v, n.Spread)
		}
	}
	if n.Runs != 3 || n.Suite != "suite-test" {
		t.Errorf("noise %+v", n)
	}
	if _, err := ComputeNoise([]Score{a}); err == nil || !strings.Contains(err.Error(), "two or more") {
		t.Errorf("one input = %v", err)
	}
	other := b
	other.Suite = "suite-v1"
	if _, err := ComputeNoise([]Score{a, other}); err == nil || !strings.Contains(err.Error(), "suite") {
		t.Errorf("mixed suites = %v", err)
	}
	p := filepath.Join(t.TempDir(), "noise.json")
	if err := WriteNoise(p, n); err != nil {
		t.Fatal(err)
	}
	if got, err := LoadNoise(p); err != nil || got.Spread[NoiseVerified] != 1 {
		t.Errorf("LoadNoise = %+v, %v", got, err)
	}
}

func TestBaselineOutputGuard(t *testing.T) {
	s := scoreDir(t, "run-pass")
	b, err := NewBaseline(s, nil, "abc")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	for _, d := range []string{"evals/scenarios/suite-test/ground_truth", "evals/golden", "gates", "tmp", "evals/dev"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	// Symlinked folders pointing into protected ones.
	for link, target := range map[string]string{
		"scen-link":  "evals/scenarios",
		"gates-link": "gates",
		"gold-link":  "evals/golden",
		"evals-link": "evals",
	} {
		if err := os.Symlink(filepath.Join(root, target), filepath.Join(root, link)); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(root)
	tests := []struct {
		path string
		want error // nil: allowed
	}{
		{"evals/baseline.json", ErrProtectedBaseline},
		{"./evals/../evals/baseline.json", ErrProtectedBaseline},
		{"evals-link/baseline.json", ErrProtectedBaseline},
		{"evals/scenarios", ErrProtectedOutput},
		{"evals/scenarios/suite-test/ground_truth/sharma-2026-09.json", ErrProtectedOutput},
		{"EVALS/Scenarios/x.json", ErrProtectedOutput},
		{"tmp/../evals/scenarios/new/score", ErrProtectedOutput},
		{"evals/golden/x.json", ErrProtectedOutput},
		{"evals/GOLDEN", ErrProtectedOutput},
		{"gates", ErrProtectedOutput},
		{"gates/thresholds.yaml", ErrProtectedOutput},
		{"Gates/new/dir", ErrProtectedOutput},
		{"tmp/../gates/x.json", ErrProtectedOutput},
		{filepath.Join(root, "gates", "x.json"), ErrProtectedOutput},
		{"scen-link", ErrProtectedOutput},
		{"scen-link/suite-test/x.json", ErrProtectedOutput},
		{"scen-link/not-yet/deeper/score", ErrProtectedOutput},
		{"gates-link/x.json", ErrProtectedOutput},
		{"gold-link/new.json", ErrProtectedOutput},
		{"evals-link/scenarios/x.json", ErrProtectedOutput},
		{"tmp/score", nil},
		{"tmp/baseline.json", nil},
		{"evals/dev/noise.json", nil},
		{"evals/score.json", nil},
		{"evals/golden-drafts/x.json", nil},
		{filepath.Join(root, "tmp", "new", "score"), nil},
	}
	for _, tt := range tests {
		err := CheckOutputPath("--out", tt.path)
		switch {
		case tt.want == nil && err != nil:
			t.Errorf("CheckOutputPath(%q) = %v, want nil", tt.path, err)
		case tt.want != nil && !errors.Is(err, tt.want):
			t.Errorf("CheckOutputPath(%q) = %v, want %v", tt.path, err, tt.want)
		}
	}

	// Every writer applies the guard, and nothing is written.
	if err := WriteScore("scen-link/suite-test", s); !errors.Is(err, ErrProtectedOutput) {
		t.Errorf("WriteScore = %v", err)
	}
	if err := WriteBaseline("gates/b.json", b); !errors.Is(err, ErrProtectedOutput) {
		t.Errorf("WriteBaseline = %v", err)
	}
	if err := WriteNoise("gold-link/n.json", Noise{Suite: "s", Runs: 2}); !errors.Is(err, ErrProtectedOutput) {
		t.Errorf("WriteNoise = %v", err)
	}
	for _, p := range []string{"evals/scenarios/suite-test/score.json", "gates/b.json", "evals/golden/n.json"} {
		if _, err := os.Stat(filepath.Join(root, p)); !os.IsNotExist(err) {
			t.Errorf("%s was written: %v", p, err)
		}
	}
	if err := os.MkdirAll("tmp/score", 0o750); err != nil {
		t.Fatal(err)
	}
	if err := WriteScore("tmp/score", s); err != nil {
		t.Errorf("WriteScore tmp/score: %v", err)
	}
}
