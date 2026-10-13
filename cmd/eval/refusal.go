package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/abhishekjha/close-copilot/internal/company"
	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/evals"
	"github.com/abhishekjha/close-copilot/internal/llm"
)

// requiredRefusalLive is what eval refusal --live and --record need: the
// LLM settings only (the provider and its key are checked by CheckLLM).
var requiredRefusalLive = []string{config.EnvLLMProvider}

// refusalProviderHook, when set, replaces the real provider of --live and
// --record. Only tests set it; no flag reaches it.
var refusalProviderHook func(config.Config) (llm.Provider, error)

// refusalUsage is the help text of eval refusal.
const refusalUsage = `usage: eval refusal --out <dir> [mode] [flags]

Routes every question of the golden refusal file (evals/golden/refusal.yaml)
through the question router (config/triage.yaml) and grades the decisions:
correct refusal per category, wrong category, misses, and over-refusal of
the should-answer questions. Writes <dir>/refusal.json (the decisions),
refusal-score.json and refusal-score.md. The same grading runs inside
eval score --refusal <dir>/refusal.json.

Reports three views: the combined router, the rules alone, and (with a
model) the classifier alone, so the score shows how much rests on the model.

Modes (at most one; default --live):
  --rules-only  no model: an item no rule catches is "no rule fired".
  --live        the real provider (LLM_PROVIDER) and LLM_MODEL_FAST. A model run.
  --record      --live, and writes every classifier request/response pair
                under --fixtures-dir. A model run.
  --replay      serves those recorded responses: no model, no environment.
                An unrecorded request is an error naming the item.

--require <expr> (repeatable) checks the refusal score:
  refusal.other_company.missed==0   refusal.injection.recall>=6/6
  refusal.overall.refused>=40/40    refusal.over_refusal==0
  (refusal_rules.* and refusal_classifier.* name the other views)
Exits non-zero, with each failure on stderr as FAIL <expr>, when one fails.

flags:
`

// refusalFlags are eval refusal's arguments.
type refusalFlags struct {
	Out         string
	Mode        string
	Golden      string
	TriageRules string
	Companies   string
	FixturesDir string
	Pricing     string
	Require     []string
}

// parseRefusalArgs parses "refusal --out DIR [mode] [flags]".
func parseRefusalArgs(args []string, out io.Writer) (command, error) {
	fs := newFlagSet("eval refusal", refusalUsage, out)
	c := command{name: cmdRefusal}
	var req stringList
	var rulesOnly, live, record, replay bool
	fs.StringVar(&c.refusal.Out, "out", "", "folder for refusal.json, refusal-score.json and refusal-score.md (required)")
	fs.BoolVar(&rulesOnly, "rules-only", false, "no model call: grade the rules alone")
	fs.BoolVar(&live, "live", false, "route through the real provider (the default)")
	fs.BoolVar(&record, "record", false, "--live, and record the classifier calls under --fixtures-dir")
	fs.BoolVar(&replay, "replay", false, "serve the recorded classifier calls: no model")
	fs.StringVar(&c.refusal.Golden, "golden", evals.DefaultRefusalGoldenPath, "golden refusal file")
	fs.StringVar(&c.refusal.TriageRules, "triage-rules", filepath.Join("config", "triage.yaml"), "router rules file")
	fs.StringVar(&c.refusal.Companies, "companies", filepath.Join("config", "companies"), "folder of company profiles")
	fs.StringVar(&c.refusal.FixturesDir, "fixtures-dir", evals.DefaultRefusalFixturesDir, "folder of recorded classifier calls for --record and --replay")
	fs.StringVar(&c.refusal.Pricing, "pricing", filepath.Join("config", "pricing.yaml"), "pricing table of the live provider")
	fs.Var(&req, "require", "a requirement such as 'refusal.injection.missed==0' (repeatable)")
	if err := fs.Parse(args[1:]); err != nil {
		return command{}, err
	}
	if fs.NArg() > 0 {
		return command{}, fmt.Errorf("eval refusal: unexpected argument %.40q", fs.Arg(0))
	}
	var modes []string
	for mode, on := range map[string]bool{evals.RefusalModeRulesOnly: rulesOnly, evals.RefusalModeLive: live,
		evals.RefusalModeRecord: record, evals.RefusalModeReplay: replay} {
		if on {
			modes = append(modes, mode)
		}
	}
	switch len(modes) {
	case 0:
		c.refusal.Mode = evals.RefusalModeLive
	case 1:
		c.refusal.Mode = modes[0]
	default:
		return command{}, errors.New("eval refusal: --rules-only, --live, --record and --replay are exclusive; give one")
	}
	if c.refusal.Out == "" {
		return command{}, errors.New("eval refusal: --out is required")
	}
	for name, v := range map[string]string{"--golden": c.refusal.Golden, "--triage-rules": c.refusal.TriageRules,
		"--companies": c.refusal.Companies, "--fixtures-dir": c.refusal.FixturesDir} {
		if v == "" {
			return command{}, fmt.Errorf("eval refusal: %s must not be empty", name)
		}
	}
	c.refusal.Require = []string(req)
	return c, nil
}

// runRefusal is eval refusal: it routes the golden items, writes the
// decisions and their grade, then checks every requirement. Malformed
// requirements and a refused output path fail before any work.
func runRefusal(ctx context.Context, cfg config.Config, f refusalFlags) error {
	reqs, err := evals.ParseRequires(f.Require)
	if err != nil {
		return err
	}
	if err := evals.CheckOutputPath("--out", f.Out); err != nil {
		return err
	}
	profiles, err := company.LoadProfiles(f.Companies)
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(profiles))
	for _, p := range profiles {
		ids = append(ids, p.ID)
	}
	golden, hash, err := evals.LoadRefusalGolden(f.Golden, ids)
	if err != nil {
		return err
	}

	opts := evals.RefusalRunOptions{Mode: f.Mode, RulesPath: f.TriageRules, FixturesDir: f.FixturesDir,
		Profiles: profiles, Golden: golden, GoldenHash: hash}
	if f.Mode == evals.RefusalModeLive || f.Mode == evals.RefusalModeRecord {
		if err := cfg.CheckLLM(); err != nil {
			return err
		}
		if refusalProviderHook != nil {
			opts.Provider, err = refusalProviderHook(cfg)
		} else {
			opts.Provider, err = newProvider(cfg, f.Pricing)
		}
		if err != nil {
			return err
		}
		opts.Model = cfg.LLMModelFast
	}
	res, err := evals.RunRefusal(ctx, opts)
	if err != nil {
		return err
	}
	graded, err := evals.GradeRefusal(golden, hash, res)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(f.Out, 0o750); err != nil {
		return fmt.Errorf("eval refusal: --out: %w", err)
	}
	if err := evals.WriteRefusalResults(f.Out, res); err != nil {
		return err
	}
	if err := evals.WriteRefusalScore(f.Out, graded); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "%s\n", filepath.Join(f.Out, evals.RefusalFile))
	r, c := graded.Router.Overall, graded.Router.OverRefusal
	_, _ = fmt.Fprintf(stdout, "mode %s: refusal recall %s, over-refusal %d of %d\n", res.Mode, r.Recall, c.Count, c.Of)

	var failures []string
	for _, rq := range reqs {
		if err := rq.Check(evals.Score{Refusal: graded}); err != nil {
			failures = append(failures, fmt.Sprintf("%s (%v)", rq.Expr, err))
		}
	}
	for _, msg := range failures {
		_, _ = fmt.Fprintf(stderr, "FAIL %s\n", msg)
	}
	if len(failures) > 0 {
		return fmt.Errorf("eval refusal: checks failed: %d", len(failures))
	}
	return nil
}
