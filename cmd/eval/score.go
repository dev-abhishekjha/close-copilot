package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/abhishekjha/close-copilot/internal/evals"
)

// scoreUsage is the help text of eval score.
const scoreUsage = `usage: eval score <results-dir> [flags]

Scores a suite run (results/<suite>/<timestamp>, written by eval run)
against the seeder's ground truth and writes score.json, score.md and
summary.md (the gate verdict, every comparison failure with its repro and
evidence, then score.md). Matching is by the truth item's keys, one
finding per item. A month with no ground truth file is an error, never
clean.

--require <subject>.<metric><op><value> checks a count or ratio, e.g.
  unrecorded_bank_charge.recall>=3/3   (the denominator must be exactly 3)
  clean.false_alarms==0
  unauthorized_writes==0               (fails until writes are checked)
A caught or missed count on a type with no planted item fails (a renamed
or empty type can't pass vacuously); <type>.false_alarms==0 is allowed
there, since a clean type is a real claim.

--compare <baseline.json> judges the score against the baseline file's
section for its tier ("replay" for agent false, "llm" for agent true). A
missing section, an empty one, and one whose agent flag (or, with the
agent on, fast or strong model) differs all fail. Then any item caught in
the baseline and not now, and any clean-month false alarm, fails.
--max-p95-increase-pct N fails a p95 close duration more than N percent
above the baseline's, ignoring increases under --min-latency-delta-ms;
--max-cost-increase-pct N does the same for total cost (a zero baseline
cost fails any cost). A threshold not given is skipped, and summary.md
says so. Tier 1 (replay, no model) passes no threshold.

--baseline-out writes a candidate baseline: it replaces only the score's
tier section of that file and keeps the other. It refuses
evals/baseline.json, which only the owner commits. No output (--out,
--baseline-out) may resolve to evals/baseline.json or under
evals/scenarios/, evals/golden/ or gates/.

Needs no environment variables. Exits non-zero, with each failure on
stderr, when a requirement or the comparison fails.

flags:
`

// noiseUsage is the help text of eval noise.
const noiseUsage = `usage: eval noise <score.json> <score.json>... --out <file>

Writes the spread (max - min, in items) of per-type caught, verified
findings and clean-month false alarms over two or more score.json files of
one suite: the noise a baseline tolerates on LLM metrics.

flags:
`

// scoreFlags are eval score's arguments.
type scoreFlags struct {
	ResultsDir  string
	TruthDir    string
	Out         string
	Require     []string
	Compare     string
	BaselineOut string
	Noise       string
	Pricing     string

	MaxP95IncreasePct  optInt
	MinLatencyDeltaMS  optInt
	MaxCostIncreasePct optInt
}

// optInt is a non-negative integer flag that may be absent.
type optInt struct {
	set bool
	v   int64
}

func (o *optInt) String() string {
	if o == nil || !o.set {
		return ""
	}
	return strconv.FormatInt(o.v, 10)
}

func (o *optInt) Set(v string) error {
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 || n > 1_000_000 {
		return fmt.Errorf("%.20q is not an integer from 0 to 1000000", v)
	}
	o.set, o.v = true, n
	return nil
}

// noiseFlags are eval noise's arguments.
type noiseFlags struct {
	Inputs []string
	Out    string
}

// stringList collects a repeatable flag.
type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, " ") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

// parseInterleaved parses flags that may come before, between or after
// the positional arguments, and returns the positionals.
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func newFlagSet(name, usage string, out io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(out)
	fs.Usage = func() {
		_, _ = io.WriteString(out, usage)
		fs.PrintDefaults()
	}
	return fs
}

// parseScoreArgs parses "score <results-dir> [flags]".
func parseScoreArgs(args []string, out io.Writer) (command, error) {
	fs := newFlagSet("eval score", scoreUsage, out)
	c := command{name: cmdScore}
	var req stringList
	fs.StringVar(&c.score.TruthDir, "truth-dir", "", "ground truth folder (default evals/scenarios/<suite>/ground_truth)")
	fs.StringVar(&c.score.Out, "out", "", "folder for score.json and score.md (default the results folder)")
	fs.Var(&req, "require", "a requirement such as 'unrecorded_bank_charge.recall>=3/3' (repeatable)")
	fs.StringVar(&c.score.Compare, "compare", "", "baseline file to compare with, item by item")
	fs.StringVar(&c.score.BaselineOut, "baseline-out", "", "write a candidate baseline here (never evals/baseline.json)")
	fs.StringVar(&c.score.Noise, "noise", "", "noise file (from eval noise) to record in the candidate baseline")
	fs.StringVar(&c.score.Pricing, "pricing", filepath.Join("config", "pricing.yaml"), "pricing file whose sha256 the candidate baseline records")
	fs.Var(&c.score.MaxP95IncreasePct, "max-p95-increase-pct", "fail when p95 close duration rises more than N percent over the baseline (Tier 2)")
	fs.Var(&c.score.MinLatencyDeltaMS, "min-latency-delta-ms", "ignore p95 increases under N ms (needs --max-p95-increase-pct)")
	fs.Var(&c.score.MaxCostIncreasePct, "max-cost-increase-pct", "fail when total cost rises more than N percent over the baseline (Tier 2)")
	pos, err := parseInterleaved(fs, args[1:])
	if err != nil {
		return command{}, err
	}
	switch len(pos) {
	case 0:
		return command{}, errors.New("eval score: missing <results-dir>")
	case 1:
		c.score.ResultsDir = pos[0]
	default:
		return command{}, fmt.Errorf("eval score: unexpected argument %.40q", pos[1])
	}
	if c.score.ResultsDir == "" {
		return command{}, errors.New("eval score: <results-dir> must not be empty")
	}
	if c.score.Out == "" {
		c.score.Out = c.score.ResultsDir
	}
	if c.score.Noise != "" && c.score.BaselineOut == "" {
		return command{}, errors.New("eval score: --noise is recorded only in a --baseline-out candidate")
	}
	if (c.score.MaxP95IncreasePct.set || c.score.MaxCostIncreasePct.set) && c.score.Compare == "" {
		return command{}, errors.New("eval score: --max-p95-increase-pct and --max-cost-increase-pct need --compare")
	}
	if c.score.MinLatencyDeltaMS.set && !c.score.MaxP95IncreasePct.set {
		return command{}, errors.New("eval score: --min-latency-delta-ms needs --max-p95-increase-pct")
	}
	c.score.Require = []string(req)
	return c, nil
}

// parseNoiseArgs parses "noise <score.json>... --out FILE".
func parseNoiseArgs(args []string, out io.Writer) (command, error) {
	fs := newFlagSet("eval noise", noiseUsage, out)
	c := command{name: cmdNoise}
	fs.StringVar(&c.noise.Out, "out", "", "noise file to write (required)")
	pos, err := parseInterleaved(fs, args[1:])
	if err != nil {
		return command{}, err
	}
	if len(pos) < 2 {
		return command{}, fmt.Errorf("eval noise: needs two or more score.json files, got %d", len(pos))
	}
	if c.noise.Out == "" {
		return command{}, errors.New("eval noise: --out is required")
	}
	c.noise.Inputs = pos
	return c, nil
}

// errChecksFailed is returned when a requirement or the comparison fails;
// each failure is already on stderr.
var errChecksFailed = errors.New("eval score: checks failed")

// runScore scores a results folder, writes score.json and score.md (and a
// candidate baseline), then checks every requirement and the comparison.
// Malformed requirements, a refused baseline path, and a missing or
// malformed noise or baseline file fail before any scoring.
func runScore(f scoreFlags) error {
	reqs, err := evals.ParseRequires(f.Require)
	if err != nil {
		return err
	}
	// Every output is checked before anything is read or written.
	if err := evals.CheckOutputPath("--out", f.Out); err != nil {
		return err
	}
	var pricing string
	if f.BaselineOut != "" {
		if err := evals.CheckOutputPath("--baseline-out", f.BaselineOut); err != nil {
			return err
		}
		if pricing, err = evals.PricingVersion(f.Pricing); err != nil {
			return err
		}
	}
	var noise *evals.Noise
	if f.Noise != "" {
		n, err := evals.LoadNoise(f.Noise)
		if err != nil {
			return err
		}
		noise = &n
	}
	var base *evals.BaselineFile
	if f.Compare != "" {
		b, err := evals.LoadBaseline(f.Compare)
		if err != nil {
			return err
		}
		base = &b
	}

	s, err := evals.ScoreRun(evals.ScoreOptions{ResultsDir: f.ResultsDir, TruthDir: f.TruthDir})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(f.Out, 0o750); err != nil {
		return fmt.Errorf("eval score: --out: %w", err)
	}
	if err := evals.WriteScore(f.Out, s); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "%s\n", filepath.Join(f.Out, evals.ScoreJSONFile))
	_, _ = fmt.Fprintf(stdout, "recall %s, precision %s, clean false alarms %d, failed runs %d\n",
		s.Overall.Recall, s.Overall.Precision, s.Clean.FalseAlarms, len(s.FailedRuns))

	if f.BaselineOut != "" {
		b, err := evals.NewBaseline(s, noise, pricing)
		if err != nil {
			return err
		}
		if err := evals.WriteBaseline(f.BaselineOut, b); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(stdout, "candidate baseline %s (tier %s)\n", f.BaselineOut, evals.TierFor(s.Agent))
	}

	var sm evals.Summary
	for _, r := range reqs {
		if err := r.Check(s); err != nil {
			sm.Requirements = append(sm.Requirements, err.Error())
		}
	}
	if base != nil {
		c, err := compareAll(*base, s, f, &sm)
		if err != nil {
			return err
		}
		sm.Compare = &c
		for _, e := range c.New {
			_, _ = fmt.Fprintf(stdout, "%s\n", e)
		}
	}
	if err := evals.WriteSummary(f.Out, s, sm); err != nil {
		return err
	}
	var failures []string
	failures = append(failures, sm.Requirements...)
	if sm.Compare != nil {
		for _, e := range sm.Compare.Failures {
			failures = append(failures, "compare: "+e.String())
		}
	}
	for _, msg := range failures {
		_, _ = fmt.Fprintf(stderr, "FAIL %s\n", msg)
	}
	if len(failures) > 0 {
		return fmt.Errorf("%w: %d", errChecksFailed, len(failures))
	}
	return nil
}

// compareAll compares the score with its tier's section of the baseline,
// then applies the latency and cost thresholds that were passed and notes
// the ones that weren't.
func compareAll(base evals.BaselineFile, s evals.Score, f scoreFlags, sm *evals.Summary) (evals.Comparison, error) {
	c, err := evals.CompareTier(base, s)
	if err != nil {
		return evals.Comparison{}, err
	}
	sec, serr := base.Section(s.Agent)
	if f.MaxP95IncreasePct.set {
		if serr == nil {
			c.Failures = append(c.Failures, evals.CompareLatencyP95(sec, s, f.MaxP95IncreasePct.v, f.MinLatencyDeltaMS.v)...)
		}
	} else {
		sm.Notes = append(sm.Notes, "p95 latency rule skipped: --max-p95-increase-pct was not passed")
	}
	if f.MaxCostIncreasePct.set {
		if serr == nil {
			fs, err := evals.CompareCost(sec, s, f.MaxCostIncreasePct.v)
			if err != nil {
				return evals.Comparison{}, err
			}
			c.Failures = append(c.Failures, fs...)
		}
	} else {
		sm.Notes = append(sm.Notes, "cost rule skipped: --max-cost-increase-pct was not passed")
	}
	return c, nil
}

// runNoise writes the spread of two or more score files.
func runNoise(f noiseFlags) error {
	if err := evals.CheckOutputPath("--out", f.Out); err != nil {
		return err
	}
	scores := make([]evals.Score, 0, len(f.Inputs))
	for _, p := range f.Inputs {
		s, err := evals.LoadScore(p)
		if err != nil {
			return err
		}
		scores = append(scores, s)
	}
	n, err := evals.ComputeNoise(scores)
	if err != nil {
		return err
	}
	if err := evals.WriteNoise(f.Out, n); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "%s\n", f.Out)
	return nil
}
