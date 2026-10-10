package evals

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// BaselineVersion is the baseline file format version.
const BaselineVersion = 1

// Baseline is a candidate evals/baseline.json: every item's outcome, the
// aggregates, the noise measured over repeated runs, and what produced it.
// The scorer only writes candidates; the owner commits the real file in
// its own baseline-update commit.
type Baseline struct {
	Version int         `json:"version"`
	Suite   string      `json:"suite"`
	Commit  string      `json:"commit"`
	Agent   bool        `json:"agent"`
	Models  ScoreModels `json:"models"`
	// PricingVersion is the sha256 of config/pricing.yaml.
	PricingVersion string `json:"pricing_version"`

	// Items maps <company>-<month>/<id> to the planted item's outcome.
	Items map[string]string `json:"items"`
	// Investigations maps <company>-<month>/<id> to the case's outcome.
	Investigations map[string]string `json:"investigations"`

	Aggregates BaselineAggregates `json:"aggregates"`
	// Noise is the spread per metric, in items, over repeated runs (from
	// eval noise); empty when not measured.
	Noise map[string]int64 `json:"noise"`
}

// BaselineAggregates are the score's aggregate metrics.
type BaselineAggregates struct {
	Types        map[string]TypeScore `json:"types"`
	Overall      OverallScore         `json:"overall"`
	Clean        CleanScore           `json:"clean"`
	VerifiedRate Ratio                `json:"verified_rate"`
	Runs         RunStats             `json:"runs"`
}

// Noise is eval noise's output: the spread (max - min, in items) of each
// LLM-sensitive metric over repeated runs of one suite.
type Noise struct {
	Suite  string           `json:"suite"`
	Runs   int64            `json:"runs"`
	Spread map[string]int64 `json:"spread"`
}

// Noise metric names: "<type>.caught" per type, plus these.
const (
	NoiseVerified    = "verified"
	NoiseCleanAlarms = "clean.false_alarms"
)

// NewBaseline builds a candidate baseline from a score. noise may be nil.
func NewBaseline(s Score, noise *Noise, pricingVersion string) (Baseline, error) {
	b := Baseline{
		Version:        BaselineVersion,
		Suite:          s.Suite,
		Commit:         s.Commit,
		Agent:          s.Agent,
		Models:         s.Models,
		PricingVersion: pricingVersion,
		Items:          map[string]string{},
		Investigations: map[string]string{},
		Aggregates: BaselineAggregates{Types: s.Types, Overall: s.Overall, Clean: s.Clean,
			VerifiedRate: s.VerifiedRate, Runs: s.Runs},
		Noise: map[string]int64{},
	}
	if b.Aggregates.Types == nil {
		b.Aggregates.Types = map[string]TypeScore{}
	}
	for _, it := range s.Items {
		b.Items[it.Key] = it.Outcome
	}
	for _, x := range s.Investigations {
		b.Investigations[x.Key] = x.Outcome
	}
	if noise != nil {
		if noise.Suite != s.Suite {
			return Baseline{}, fmt.Errorf("evals: noise is for suite %s, the score for %s", noise.Suite, s.Suite)
		}
		for k, v := range noise.Spread {
			b.Noise[k] = v
		}
	}
	return b, nil
}

// ErrProtectedBaseline is returned for a write to evals/baseline.json.
var ErrProtectedBaseline = errors.New("evals: evals/baseline.json is written only by the owner's baseline-update commit")

// ErrProtectedOutput is returned for a write under evals/scenarios/,
// evals/golden/ or gates/: the scorer's inputs and the gates are never its
// outputs.
var ErrProtectedOutput = errors.New("evals: evals/scenarios/, evals/golden/ and gates/ are never written by the scorer")

// IsProtectedBaseline reports whether path resolves to evals/baseline.json
// (case-insensitively, after cleaning and following symlinks), wherever
// the repository is.
func IsProtectedBaseline(path string) bool {
	for _, p := range outputCandidates(path) {
		if isBaselinePath(p) {
			return true
		}
	}
	return false
}

// CheckOutputPath refuses an output path (a file, or the folder score.json
// and score.md go into) that resolves to evals/baseline.json or to, or
// under, evals/scenarios/, evals/golden/ or gates/. It compares path
// components case-insensitively, as given, cleaned to an absolute path,
// and with symlinks followed (through the longest existing ancestor, so
// a not-yet-created file under a symlinked folder is caught). flag names
// the option in the error. An unresolvable path is refused.
func CheckOutputPath(flag, path string) error {
	cands := outputCandidates(path)
	if len(cands) == 0 {
		return fmt.Errorf("%w: refusing %s %s: cannot resolve it", ErrProtectedOutput, flag, path)
	}
	for _, p := range cands {
		if isBaselinePath(p) {
			return fmt.Errorf("%w: refusing %s %s", ErrProtectedBaseline, flag, path)
		}
		if under, ok := protectedDir(p); ok {
			return fmt.Errorf("%w: refusing %s %s (resolves under %s)", ErrProtectedOutput, flag, path, under)
		}
	}
	return nil
}

// outputCandidates returns path cleaned, as an absolute path, and with
// symlinks resolved through its longest existing ancestor. It returns nil
// when path can't be made absolute (callers fail closed).
func outputCandidates(path string) []string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil
	}
	out := []string{filepath.Clean(path), abs}
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		return append(out, r)
	}
	// Resolve the longest existing ancestor and append the rest.
	rest := ""
	for dir := abs; ; {
		parent := filepath.Dir(dir)
		rest = filepath.Join(filepath.Base(dir), rest)
		if parent == dir {
			break
		}
		if r, err := filepath.EvalSymlinks(parent); err == nil {
			out = append(out, filepath.Join(r, rest))
			break
		}
		dir = parent
	}
	return out
}

// isBaselinePath reports whether p ends in evals/baseline.json.
func isBaselinePath(p string) bool {
	return strings.EqualFold(filepath.Base(p), "baseline.json") &&
		strings.EqualFold(filepath.Base(filepath.Dir(p)), "evals")
}

// protectedDir reports whether p is, or is under, evals/scenarios,
// evals/golden or gates, and which one.
func protectedDir(p string) (string, bool) {
	comps := strings.Split(filepath.ToSlash(p), "/")
	for i, c := range comps {
		if strings.EqualFold(c, "gates") {
			return "gates/", true
		}
		if strings.EqualFold(c, "evals") && i+1 < len(comps) {
			for _, sub := range []string{"scenarios", "golden"} {
				if strings.EqualFold(comps[i+1], sub) {
					return "evals/" + sub + "/", true
				}
			}
		}
	}
	return "", false
}

// WriteBaseline writes a candidate baseline atomically. It refuses any
// path CheckOutputPath refuses, evals/baseline.json above all.
func WriteBaseline(path string, b Baseline) error {
	if err := CheckOutputPath("--baseline-out", path); err != nil {
		return err
	}
	return writeJSON(path, b)
}

// LoadBaseline reads a baseline strictly. A missing or malformed file is
// an error, never an empty baseline.
func LoadBaseline(path string) (Baseline, error) {
	var b Baseline
	if err := readStrict(path, &b); err != nil {
		return Baseline{}, fmt.Errorf("evals: baseline: %w", err)
	}
	switch {
	case b.Version != BaselineVersion:
		return Baseline{}, fmt.Errorf("evals: baseline %s: version %d, want %d", path, b.Version, BaselineVersion)
	case !nameRe.MatchString(b.Suite):
		return Baseline{}, fmt.Errorf("evals: baseline %s: suite %.64q is not a suite name", path, b.Suite)
	case b.Items == nil:
		return Baseline{}, fmt.Errorf("evals: baseline %s: items is missing", path)
	}
	return b, nil
}

// PricingVersion is the sha256 (hex) of the pricing file.
func PricingVersion(path string) (string, error) {
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("evals: pricing: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// Comparison kinds. Every kind except CompareNew is a failure.
const (
	CompareRegression       = "regression"        // caught in the baseline, not caught now
	CompareRemoved          = "removed"           // in the baseline, absent from the truth now
	CompareCleanFalseAlarm  = "clean_false_alarm" // any false alarm in a clean month
	CompareCleanMonthFailed = "clean_month_failed"
	CompareVerifiedDrop     = "verified_drop" // verified findings fell by more than the noise
	CompareNew              = "new"           // not in the baseline; listed, not a failure
)

// CompareEntry is one line of a comparison.
type CompareEntry struct {
	Kind     string `json:"kind"`
	Key      string `json:"key"`
	Was      string `json:"was"`
	Now      string `json:"now"`
	Detail   string `json:"detail"`
	Repro    string `json:"repro"`
	Evidence string `json:"evidence"`
}

// Comparison is a score judged against a baseline, item by item.
type Comparison struct {
	Failures []CompareEntry `json:"failures"`
	New      []CompareEntry `json:"new"`
}

// Compare judges a score against a baseline item by item: every item
// caught in the baseline and not caught now, every baseline item missing
// from the score, every clean-month false alarm, a failed clean month, and
// a drop in verified findings larger than the measured noise (floor: one
// item) are failures. Items new since the baseline are listed, not failed.
// A baseline for another suite is an error.
func Compare(b Baseline, s Score) (Comparison, error) {
	if b.Suite != s.Suite {
		return Comparison{}, fmt.Errorf("evals: baseline is for suite %s, the score for %s", b.Suite, s.Suite)
	}
	c := Comparison{Failures: []CompareEntry{}, New: []CompareEntry{}}
	now := map[string]ItemOutcome{}
	for _, it := range s.Items {
		now[it.Key] = it
		was, ok := b.Items[it.Key]
		switch {
		case !ok:
			c.New = append(c.New, CompareEntry{Kind: CompareNew, Key: it.Key, Now: it.Outcome,
				Detail: "not in the baseline", Repro: it.Repro, Evidence: it.Evidence})
		case was == OutcomeCaught && it.Outcome != OutcomeCaught:
			c.Failures = append(c.Failures, CompareEntry{Kind: CompareRegression, Key: it.Key, Was: was, Now: it.Outcome,
				Detail: it.Type + " " + canonicalKeys(it.Keys) + " " + it.Reason, Repro: it.Repro, Evidence: it.Evidence})
		}
	}
	var gone []string
	for k := range b.Items {
		if _, ok := now[k]; !ok {
			gone = append(gone, k)
		}
	}
	sort.Strings(gone)
	for _, k := range gone {
		c.Failures = append(c.Failures, CompareEntry{Kind: CompareRemoved, Key: k, Was: b.Items[k], Now: "absent",
			Detail: "item is in the baseline but not in the ground truth now",
			Repro:  "go run ./cmd/eval score <results-dir> --compare <baseline>", Evidence: "baseline items/" + k})
	}
	for _, fa := range s.FalseAlarms {
		if fa.Reason != ReasonCleanMonth {
			continue
		}
		c.Failures = append(c.Failures, CompareEntry{Kind: CompareCleanFalseAlarm,
			Key: fa.Company + "-" + fa.Month + "/" + fa.FindingID, Now: fa.Type,
			Detail: fa.Type + " " + canonicalKeys(fa.Keys), Repro: fa.Repro, Evidence: fa.Evidence})
	}
	for _, r := range s.FailedRuns {
		if r.Control {
			c.Failures = append(c.Failures, CompareEntry{Kind: CompareCleanMonthFailed, Key: r.Company + "-" + r.Month,
				Now: r.Status, Detail: "clean control month run failed: " + r.Reason, Repro: r.Repro, Evidence: r.Evidence})
		}
	}
	tol := max(b.Noise[NoiseVerified], 1)
	if drop := b.Aggregates.VerifiedRate.Num - s.VerifiedRate.Num; drop > tol {
		c.Failures = append(c.Failures, CompareEntry{Kind: CompareVerifiedDrop, Key: NoiseVerified,
			Was: fmt.Sprint(b.Aggregates.VerifiedRate.Num), Now: fmt.Sprint(s.VerifiedRate.Num),
			Detail: fmt.Sprintf("verified findings fell by %d, more than the noise tolerance %d", drop, tol),
			Repro:  "go run ./cmd/eval score <results-dir> --compare <baseline>", Evidence: ScoreJSONFile + "#verified_rate"})
	}
	return c, nil
}

// String is one line per entry, failures first.
func (e CompareEntry) String() string {
	s := fmt.Sprintf("%s %s", e.Kind, e.Key)
	if e.Was != "" || e.Now != "" {
		s += fmt.Sprintf(" (was %s, now %s)", orDash(e.Was), orDash(e.Now))
	}
	if e.Detail != "" {
		s += ": " + strings.TrimSpace(e.Detail)
	}
	if e.Evidence != "" {
		s += "; evidence " + e.Evidence
	}
	if e.Repro != "" {
		s += "; repro: " + e.Repro
	}
	return s
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// ComputeNoise is the spread (max - min) of each noise metric over two or
// more scores of one suite: per-type caught, verified findings and clean
// false alarms.
func ComputeNoise(scores []Score) (Noise, error) {
	if len(scores) < 2 {
		return Noise{}, fmt.Errorf("evals: noise needs two or more scores, got %d", len(scores))
	}
	suite := scores[0].Suite
	types := map[string]bool{}
	for i, s := range scores {
		if s.Suite != suite {
			return Noise{}, fmt.Errorf("evals: noise: score %d is for suite %s, score 0 for %s", i, s.Suite, suite)
		}
		for t := range s.Types {
			types[t] = true
		}
	}
	metric := map[string]func(Score) int64{
		NoiseVerified:    func(s Score) int64 { return s.VerifiedRate.Num },
		NoiseCleanAlarms: func(s Score) int64 { return s.Clean.FalseAlarms },
	}
	for t := range types {
		metric[t+".caught"] = func(s Score) int64 { return s.Types[t].Caught }
	}
	n := Noise{Suite: suite, Runs: int64(len(scores)), Spread: map[string]int64{}}
	names := make([]string, 0, len(metric))
	for k := range metric {
		names = append(names, k)
	}
	slices.Sort(names)
	for _, k := range names {
		lo, hi := metric[k](scores[0]), metric[k](scores[0])
		for _, s := range scores[1:] {
			v := metric[k](s)
			lo, hi = min(lo, v), max(hi, v)
		}
		n.Spread[k] = hi - lo
	}
	return n, nil
}

// LoadNoise reads a noise file strictly.
func LoadNoise(path string) (Noise, error) {
	var n Noise
	if err := readStrict(path, &n); err != nil {
		return Noise{}, fmt.Errorf("evals: noise: %w", err)
	}
	if !nameRe.MatchString(n.Suite) || n.Runs < 2 || n.Spread == nil {
		return Noise{}, fmt.Errorf("evals: noise %s: needs a suite, two or more runs and a spread", path)
	}
	return n, nil
}

// WriteNoise writes a noise file atomically.
func WriteNoise(path string, n Noise) error {
	if err := CheckOutputPath("noise --out", path); err != nil {
		return err
	}
	return writeJSON(path, n)
}
