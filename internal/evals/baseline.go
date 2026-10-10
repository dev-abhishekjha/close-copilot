package evals

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// BaselineVersion is the baseline file format version. Version 2 holds
// one section per tier; version 1 (a single section) is rejected.
const BaselineVersion = 2

// Baseline tiers: Tier 1 replays recorded fixtures without the model
// (agent false); Tier 2 runs the explainer and verifier (agent true).
const (
	TierReplay = "replay"
	TierLLM    = "llm"
)

// TierFor is the baseline section a score is judged against.
func TierFor(agent bool) string {
	if agent {
		return TierLLM
	}
	return TierReplay
}

// BaselineFile is evals/baseline.json: one protected file with a section
// per tier, {"version":2,"tiers":{"replay":...,"llm":...}}. The scorer
// only writes candidates; the owner commits the real file in its own
// baseline-update commit.
type BaselineFile struct {
	Version int           `json:"version"`
	Tiers   BaselineTiers `json:"tiers"`
}

// BaselineTiers are the sections; an absent section is null.
type BaselineTiers struct {
	Replay *Baseline `json:"replay"`
	LLM    *Baseline `json:"llm"`
}

// ErrBaselineTier is a baseline file with no section for a score's tier.
var ErrBaselineTier = errors.New("evals: baseline has no section for this tier")

// Section is the section for a score's agent flag; a missing one is
// ErrBaselineTier (fail closed).
func (f BaselineFile) Section(agent bool) (Baseline, error) {
	b := f.Tiers.Replay
	if agent {
		b = f.Tiers.LLM
	}
	if b == nil {
		return Baseline{}, fmt.Errorf("%w %q (agent %t)", ErrBaselineTier, TierFor(agent), agent)
	}
	return *b, nil
}

// Set replaces the section for b's agent flag and keeps the other.
func (f *BaselineFile) Set(b Baseline) {
	c := b
	if b.Agent {
		f.Tiers.LLM = &c
	} else {
		f.Tiers.Replay = &c
	}
}

// Baseline is one tier's section: every item's outcome, the aggregates,
// the noise measured over repeated runs, and what produced it.
type Baseline struct {
	Suite  string      `json:"suite"`
	Commit string      `json:"commit"`
	Agent  bool        `json:"agent"`
	Models ScoreModels `json:"models"`
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

// WriteBaseline writes a candidate baseline atomically: it replaces only
// b's tier section of the file at path and keeps the other (creating the
// file if absent). It refuses any path CheckOutputPath refuses,
// evals/baseline.json above all, and an existing file it can't read.
func WriteBaseline(path string, b Baseline) error {
	if err := CheckOutputPath("--baseline-out", path); err != nil {
		return err
	}
	f := BaselineFile{Version: BaselineVersion}
	if _, err := os.Stat(path); err == nil {
		if f, err = LoadBaseline(path); err != nil {
			return fmt.Errorf("evals: --baseline-out %s exists but can't be updated: %w", path, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("evals: --baseline-out %s: %w", path, err)
	}
	f.Set(b)
	return writeJSON(path, f)
}

// LoadBaseline reads a baseline file strictly. A missing or malformed
// file, another version, and a section that isn't a valid baseline of
// its tier are errors, never an empty baseline. A file may lack a
// section; Section fails closed on it.
func LoadBaseline(path string) (BaselineFile, error) {
	// The version first, leniently, so an old file says so plainly.
	var v struct {
		Version int `json:"version"`
	}
	if err := readJSONFile(path, &v); err != nil {
		return BaselineFile{}, fmt.Errorf("evals: baseline: %w", err)
	}
	if v.Version != BaselineVersion {
		return BaselineFile{}, fmt.Errorf("evals: baseline %s: version %d, want %d", path, v.Version, BaselineVersion)
	}
	var f BaselineFile
	if err := readStrict(path, &f); err != nil {
		return BaselineFile{}, fmt.Errorf("evals: baseline: %w", err)
	}
	if f.Version != BaselineVersion {
		return BaselineFile{}, fmt.Errorf("evals: baseline %s: version %d, want %d", path, f.Version, BaselineVersion)
	}
	for _, sec := range []struct {
		tier  string
		agent bool
		b     *Baseline
	}{{TierReplay, false, f.Tiers.Replay}, {TierLLM, true, f.Tiers.LLM}} {
		b := sec.b
		switch {
		case b == nil:
		case !nameRe.MatchString(b.Suite):
			return BaselineFile{}, fmt.Errorf("evals: baseline %s: tier %s: suite %.64q is not a suite name", path, sec.tier, b.Suite)
		case b.Items == nil:
			return BaselineFile{}, fmt.Errorf("evals: baseline %s: tier %s: items is missing", path, sec.tier)
		case b.Agent != sec.agent:
			return BaselineFile{}, fmt.Errorf("evals: baseline %s: tier %s has agent %t", path, sec.tier, b.Agent)
		}
	}
	return f, nil
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

	CompareTierMissing     = "baseline_tier_missing" // the baseline file has no section for the score's tier
	CompareEmptyBaseline   = "empty_baseline"        // the section has no items, so nothing could regress
	CompareAgentMismatch   = "agent_mismatch"        // the section's agent flag differs from the score's
	CompareModelMismatch   = "model_mismatch"        // Tier 2: the fast or strong model differs
	CompareLatencyIncrease = "latency_p95_increase"  // Tier 2: p95 close duration rose past the threshold
	CompareCostIncrease    = "cost_increase"         // Tier 2: total cost rose past the threshold
)

// compareRepro re-runs a comparison.
const compareRepro = "go run ./cmd/eval score <results-dir> --compare <baseline.json>"

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

// CompareTier judges a score against its tier's section of a baseline
// file. A missing section is a failure (fail closed), never a pass.
func CompareTier(f BaselineFile, s Score) (Comparison, error) {
	b, err := f.Section(s.Agent)
	if err != nil {
		return Comparison{Failures: []CompareEntry{{Kind: CompareTierMissing, Key: TierFor(s.Agent),
			Detail: err.Error() + "; the owner commits it in a baseline-update commit (evals/fixtures/README.md)",
			Repro:  compareRepro, Evidence: "baseline tiers/" + TierFor(s.Agent)}}, New: []CompareEntry{}}, nil
	}
	return Compare(b, s)
}

// Compare judges a score against one tier's baseline item by item: every
// item caught in the baseline and not caught now, every baseline item
// missing from the score, every clean-month false alarm, a failed clean
// month, and a drop in verified findings larger than the measured noise
// (floor: one item) are failures. Items new since the baseline are
// listed, not failed. An empty baseline, a baseline whose agent flag
// differs from the score's, and (with the agent on) one whose fast or
// strong model differs are failures, and nothing else is compared. A
// baseline for another suite is an error.
func Compare(b Baseline, s Score) (Comparison, error) {
	if b.Suite != s.Suite {
		return Comparison{}, fmt.Errorf("evals: baseline is for suite %s, the score for %s", b.Suite, s.Suite)
	}
	if fs := compareConfig(b, s); len(fs) > 0 {
		return Comparison{Failures: fs, New: []CompareEntry{}}, nil
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
			Repro:  compareRepro, Evidence: "baseline items/" + k})
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
			Repro:  compareRepro, Evidence: ScoreJSONFile + "#verified_rate"})
	}
	return c, nil
}

// compareConfig fails a baseline that can't judge the score: no items, or
// another agent flag, or (agent on) other models.
func compareConfig(b Baseline, s Score) []CompareEntry {
	var out []CompareEntry
	if len(b.Items) == 0 {
		out = append(out, CompareEntry{Kind: CompareEmptyBaseline, Key: TierFor(s.Agent),
			Detail: "the baseline has no items, so no regression could fail; re-measure it", Repro: compareRepro,
			Evidence: "baseline tiers/" + TierFor(s.Agent) + "/items"})
	}
	if b.Agent != s.Agent {
		out = append(out, CompareEntry{Kind: CompareAgentMismatch, Key: "agent", Was: fmt.Sprint(b.Agent), Now: fmt.Sprint(s.Agent),
			Detail: "the baseline and the score ran with a different agent setting", Repro: compareRepro,
			Evidence: ScoreJSONFile + "#agent"})
		return out
	}
	if s.Agent {
		if b.Models.Fast != s.Models.Fast {
			out = append(out, CompareEntry{Kind: CompareModelMismatch, Key: "models.fast", Was: b.Models.Fast, Now: s.Models.Fast,
				Detail: "fast model differs from the baseline's", Repro: compareRepro, Evidence: ScoreJSONFile + "#models"})
		}
		if b.Models.Strong != s.Models.Strong {
			out = append(out, CompareEntry{Kind: CompareModelMismatch, Key: "models.strong", Was: b.Models.Strong, Now: s.Models.Strong,
				Detail: "strong model differs from the baseline's", Repro: compareRepro, Evidence: ScoreJSONFile + "#models"})
		}
	}
	return out
}

// CompareLatencyP95 fails when the score's p95 close duration exceeds the
// baseline's by more than maxPct percent. An increase below minDeltaMS
// milliseconds is ignored (an absolute floor). Integer math only: the
// increase fails when delta*100 > baseline*maxPct, so exactly maxPct
// passes.
func CompareLatencyP95(b Baseline, s Score, maxPct, minDeltaMS int64) []CompareEntry {
	was, now := b.Aggregates.Runs.DurationMS.P95, s.Runs.DurationMS.P95
	delta := now - was
	if delta <= 0 || delta < minDeltaMS {
		return nil
	}
	lhs := new(big.Int).Mul(big.NewInt(delta), big.NewInt(100))
	if lhs.Cmp(new(big.Int).Mul(big.NewInt(was), big.NewInt(maxPct))) <= 0 {
		return nil
	}
	return []CompareEntry{{Kind: CompareLatencyIncrease, Key: "runs.duration_ms.p95", Was: fmt.Sprint(was), Now: fmt.Sprint(now),
		Detail:   fmt.Sprintf("p95 close duration rose by %d ms, more than %d%% of the baseline (floor %d ms)", delta, maxPct, minDeltaMS),
		Repro:    compareRepro + fmt.Sprintf(" --max-p95-increase-pct %d --min-latency-delta-ms %d", maxPct, minDeltaMS),
		Evidence: ScoreJSONFile + "#runs.duration_ms.p95"}}
}

// CompareCost fails when the score's total cost exceeds the baseline's by
// more than maxPct percent, in exact decimal math. A zero baseline cost
// passes a zero cost and fails any cost above it.
func CompareCost(b Baseline, s Score, maxPct int64) ([]CompareEntry, error) {
	was, err := parseCost(b.Aggregates.Runs.CostUSD.Total)
	if err != nil {
		return nil, fmt.Errorf("evals: baseline cost: %w", err)
	}
	now, err := parseCost(s.Runs.CostUSD.Total)
	if err != nil {
		return nil, fmt.Errorf("evals: score cost: %w", err)
	}
	delta := new(big.Rat).Sub(now, was)
	if delta.Sign() <= 0 {
		return nil, nil
	}
	limit := new(big.Rat).Mul(was, new(big.Rat).SetInt64(maxPct))
	if was.Sign() != 0 && new(big.Rat).Mul(delta, big.NewRat(100, 1)).Cmp(limit) <= 0 {
		return nil, nil
	}
	return []CompareEntry{{Kind: CompareCostIncrease, Key: "runs.cost_usd.total",
		Was: b.Aggregates.Runs.CostUSD.Total, Now: s.Runs.CostUSD.Total,
		Detail:   fmt.Sprintf("total cost rose by more than %d%% of the baseline", maxPct),
		Repro:    compareRepro + fmt.Sprintf(" --max-cost-increase-pct %d", maxPct),
		Evidence: ScoreJSONFile + "#runs.cost_usd.total"}}, nil
}

// parseCost reads exact decimal cost text ("" is zero).
func parseCost(v string) (*big.Rat, error) {
	if v == "" {
		return new(big.Rat), nil
	}
	norm, err := sumDecimals([]string{v})
	if err != nil {
		return nil, err
	}
	r, ok := new(big.Rat).SetString(norm)
	if !ok {
		return nil, fmt.Errorf("cost %.40q is not a decimal", v)
	}
	return r, nil
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
