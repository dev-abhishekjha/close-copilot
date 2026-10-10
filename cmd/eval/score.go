package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/abhishekjha/close-copilot/internal/evals"
)

// scoreUsage is the help text of eval score.
const scoreUsage = `usage: eval score <results-dir> [flags]

Scores a suite run (results/<suite>/<timestamp>, written by eval run)
against the seeder's ground truth and writes score.json and score.md.
Matching is by the truth item's keys, one finding per item. A month with
no ground truth file is an error, never clean.

--require <subject>.<metric><op><value> checks a count or ratio, e.g.
  unrecorded_bank_charge.recall>=3/3   (the denominator must be exactly 3)
  clean.false_alarms==0
  unauthorized_writes==0               (fails until writes are checked)
--compare <baseline.json> fails on any item caught in the baseline and not
now, and on any clean-month false alarm. --baseline-out writes a candidate
baseline; it refuses evals/baseline.json, which only the owner commits.
No output (--out, --baseline-out) may resolve to evals/baseline.json or
under evals/scenarios/, evals/golden/ or gates/.

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
	var base *evals.Baseline
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
		_, _ = fmt.Fprintf(stdout, "candidate baseline %s\n", f.BaselineOut)
	}

	var failures []string
	for _, r := range reqs {
		if err := r.Check(s); err != nil {
			failures = append(failures, err.Error())
		}
	}
	if base != nil {
		c, err := evals.Compare(*base, s)
		if err != nil {
			return err
		}
		for _, e := range c.New {
			_, _ = fmt.Fprintf(stdout, "%s\n", e)
		}
		for _, e := range c.Failures {
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
