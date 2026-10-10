package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/abhishekjha/close-copilot/internal/agent"
	"github.com/abhishekjha/close-copilot/internal/evals"
)

// Subcommands.
const (
	cmdRun   = "run"
	cmdScore = "score"
	cmdNoise = "noise"
)

// commandsUsage names every subcommand, for errors.
const commandsUsage = "usage: eval run --suite <name> | eval score <results-dir> | eval noise <score.json>... --out <file>"

// usage is the help text of eval run.
const usage = `usage: eval run --suite <name> [flags]

Runs a close for every evaluated company-month of evals/scenarios/<name>.yaml
and its clean control month, one after another, through the same workflow
as the agent. Each month's findings and run metadata go to
<results-dir>/<suite>/<UTC timestamp>/<company>-<month>.json, then
manifest.json, whose path is printed. Exits non-zero if any run ends
failed; partial is not a failure.

Modes:
  (default)  live: reads ERPNext and the evidence store through the MCP
             servers. Needs DATABASE_URL, BOOKS_MCP_URL, EVIDENCE_MCP_URL
             and MCP_TOKEN_AGENT, and a seeded, loaded suite.
  --record   live, and records every books and evidence tool response to
             <fixtures-dir>/<suite>/<company>-<month>/<tool>-<hash>.json
             plus index.json. Needs the seeded ERPNext (take
             tmp/erpnext.lock); never run in CI. A live error aborts the
             recording and leaves the old fixtures in place.
  --replay   serves the recorded responses instead: no ERPNext, no MCP
             server, no MCP token. Needs DATABASE_URL only. Fails before
             any month runs if a fixture's hash, the suite or its ground
             truth changed since recording (re-record).

--no-llm (or --no-agent) runs without the explainer and verifier: runs
end partial and the manifest records "agent": false. Without it the
explainer and verifier run through LLM_PROVIDER, the manifest records
"agent": true and the models, and LLM_DAILY_BUDGET_USD caps the spend.
COPILOT_FAULT must be unset.

Bootstrap order for a suite's fixtures and baseline:
  1. make erp-reset seed load SUITE=<suite>
  2. make record-fixtures SUITE=<suite>
  3. commit evals/fixtures/<suite>/
  4. the owner scores a replay and commits evals/baseline.json in its own
     baseline-update commit (see evals/fixtures/README.md)

flags:
`

// command is a parsed eval command line. flags is set for run, score for
// score, noise for noise.
type command struct {
	name  string
	flags evals.Flags
	score scoreFlags
	noise noiseFlags
}

// onlyList collects --only values; each may be comma-separated.
type onlyList []string

func (o *onlyList) String() string { return strings.Join(*o, ",") }

var onlyRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}:\d{4}-\d{2}$`)

func (o *onlyList) Set(v string) error {
	for p := range strings.SplitSeq(v, ",") {
		p = strings.TrimSpace(p)
		if !onlyRe.MatchString(p) {
			return fmt.Errorf("%.80q is not company:YYYY-MM", p)
		}
		*o = append(*o, p)
	}
	return nil
}

// errHelp asks for the help text and a zero exit.
var errHelp = flag.ErrHelp

// parseArgs parses "run --suite ...", "score <dir> ..." or "noise ..." and
// writes help to out on -h.
func parseArgs(args []string, out io.Writer) (command, error) {
	if len(args) == 0 {
		return command{}, errors.New("eval: missing command; " + commandsUsage)
	}
	switch args[0] {
	case cmdRun:
		return parseRunArgs(args, out)
	case cmdScore:
		return parseScoreArgs(args, out)
	case cmdNoise:
		return parseNoiseArgs(args, out)
	}
	return command{}, fmt.Errorf("eval: unknown command %.40q; %s", args[0], commandsUsage)
}

// requiredFor is the environment a command line needs: run reaches
// Postgres and the MCP servers, run --replay only Postgres; score and
// noise read files and need none. A run line that doesn't parse (or asks
// for help) needs nothing: it stops on its arguments, before any I/O.
func requiredFor(args []string) []string {
	for i, a := range args {
		if strings.HasPrefix(a, "-") {
			continue // cli.Run's own flags, such as -version
		}
		if a != cmdRun {
			return nil
		}
		c, err := parseRunArgs(args[i:], io.Discard)
		switch {
		case err != nil:
			return nil
		case c.flags.Replay:
			return requiredReplay
		}
		return required
	}
	return nil
}

// parseRunArgs parses "run --suite ...".
func parseRunArgs(args []string, out io.Writer) (command, error) {
	fs := flag.NewFlagSet("eval run", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.Usage = func() {
		_, _ = io.WriteString(out, usage)
		fs.PrintDefaults()
	}
	var only onlyList
	c := command{name: cmdRun}
	fs.StringVar(&c.flags.Suite, "suite", "", "suite name: evals/scenarios/<name>.yaml (required)")
	fs.Var(&only, "only", "run only these months, company:YYYY-MM (repeatable or comma-separated)")
	fs.StringVar(&c.flags.ModelFast, "model-fast", "", "override LLM_MODEL_FAST for this run only")
	fs.BoolVar(&c.flags.NoAgent, "no-agent", false, "run without the explainer and verifier (same as --no-llm)")
	fs.BoolVar(&c.flags.NoAgent, "no-llm", false, "run without the explainer and verifier: no model call (Tier 1)")
	fs.BoolVar(&c.flags.Record, "record", false, "live run that records every tool response under --fixtures-dir (needs ERPNext; never in CI)")
	fs.BoolVar(&c.flags.Replay, "replay", false, "serve recorded tool responses from --fixtures-dir: no ERPNext, no MCP")
	fs.StringVar(&c.flags.FixturesDir, "fixtures-dir", evals.DefaultFixturesDir, "fixtures folder for --record and --replay")
	fs.StringVar(&c.flags.ResultsDir, "results-dir", agent.DefaultResultsDir, "results folder")
	fs.StringVar(&c.flags.ScenariosDir, "scenarios-dir", "evals/scenarios", "folder of suite files")
	fs.StringVar(&c.flags.ConfigDir, "config-dir", "config", "folder of companies/ and rules.yaml")
	if err := fs.Parse(args[1:]); err != nil {
		return command{}, err
	}
	if fs.NArg() > 0 {
		return command{}, fmt.Errorf("eval run: unexpected argument %.40q", fs.Arg(0))
	}
	if c.flags.Suite == "" {
		return command{}, errors.New("eval run: --suite is required")
	}
	for name, v := range map[string]string{"--results-dir": c.flags.ResultsDir, "--scenarios-dir": c.flags.ScenariosDir,
		"--config-dir": c.flags.ConfigDir, "--fixtures-dir": c.flags.FixturesDir} {
		if v == "" {
			return command{}, fmt.Errorf("eval run: %s must not be empty", name)
		}
	}
	if c.flags.Record && c.flags.Replay {
		return command{}, errors.New("eval run: --record and --replay can't be used together")
	}
	if !c.flags.Record && !c.flags.Replay {
		c.flags.FixturesDir = "" // a live run reads no fixtures
	}
	c.flags.Only = []string(only)
	return c, nil
}
