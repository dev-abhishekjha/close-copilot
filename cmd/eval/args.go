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
manifest.json. The suite must already be seeded and loaded
(make erp-reset seed load ingest). Exits non-zero if any run ends failed;
partial is not a failure.

The eval path has no explainer or verifier yet: every run goes without
them (as with --no-agent), ends partial, and the manifest records
"agent": false. The eval command itself calls no model.

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

// requiredFor is the environment a command line needs: only run reaches
// Postgres and the MCP servers; score and noise read files and need none.
func requiredFor(args []string) []string {
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			continue // cli.Run's own flags, such as -version
		}
		if a == cmdRun {
			return required
		}
		return nil
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
	fs.BoolVar(&c.flags.NoAgent, "no-agent", false, "run without the explainer and verifier (always the case until the explainer is wired into the eval path)")
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
	for name, v := range map[string]string{"--results-dir": c.flags.ResultsDir, "--scenarios-dir": c.flags.ScenariosDir, "--config-dir": c.flags.ConfigDir} {
		if v == "" {
			return command{}, fmt.Errorf("eval run: %s must not be empty", name)
		}
	}
	c.flags.Only = []string(only)
	return c, nil
}
