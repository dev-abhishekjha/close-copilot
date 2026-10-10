// Command eval is the eval harness: runs closes on a seeded suite and scores them against ground truth.
//
// Built in CC-901 (runner), CC-902 (scoring) and CC-905 (recorded
// fixtures and the CI gate). Usage:
//
//	eval run --suite suite-v1 [--only sharma:2026-09] [--model-fast X] [--no-llm|--no-agent]
//	         [--record | --replay] [--fixtures-dir evals/fixtures]
//	         [--results-dir results] [--scenarios-dir evals/scenarios] [--config-dir config]
//	eval score <results-dir> [--truth-dir D] [--out D] [--require EXPR]... [--compare FILE]
//	         [--max-p95-increase-pct N] [--min-latency-delta-ms N] [--max-cost-increase-pct N]
//	         [--baseline-out FILE] [--noise FILE] [--pricing config/pricing.yaml]
//	eval noise <score.json> <score.json>... --out FILE
//
// score and noise read files only and need no environment variables; run
// needs DATABASE_URL and the MCP settings, run --replay DATABASE_URL only.
//
// run closes every evaluated company-month of the suite and its clean
// control month, one after another, through the same agent.Workflow as
// the agent binary, and writes results/<suite>/<timestamp>/<company>-<month>.json
// plus manifest.json. It exits non-zero if any run ends failed.
//
// Like the agent, eval reaches ERPNext and the evidence store only through
// the read-only MCP servers, or through fixtures recorded from them; this
// binary must not link the ERPNext client (deps_test.go). A replay never
// builds the MCP registry and never reads an MCP token.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"

	"github.com/abhishekjha/close-copilot/internal/agent"
	"github.com/abhishekjha/close-copilot/internal/buildinfo"
	"github.com/abhishekjha/close-copilot/internal/cli"
	"github.com/abhishekjha/close-copilot/internal/company"
	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/evals"
	"github.com/abhishekjha/close-copilot/internal/llm"
	"github.com/abhishekjha/close-copilot/internal/store"
)

// stdout receives the result line; stderr the help text.
var (
	stdout io.Writer = os.Stdout
	stderr io.Writer = os.Stderr
)

// required are the variables eval run can't start without; score and
// noise need none (requiredFor).
var required = []string{
	config.EnvDatabaseURL,
	config.EnvBooksMCPURL,
	config.EnvEvidenceMCPURL,
	config.EnvMCPTokenAgent,
}

// requiredReplay is what eval run --replay needs: Postgres only.
var requiredReplay = []string{config.EnvDatabaseURL}

// workflowHook, when set, adjusts the workflow before the suite runs.
// Only tests set it (to swap in a sabotaged check); no flag reaches it.
var workflowHook func(*agent.Workflow)

// recordScanExtra extends the fixture scan's identifier allowlist. The
// CLI passes none (nil); only the integration tests set it, to the fake
// ERPNext's made-up GSTINs.
var recordScanExtra []string

func main() {
	cli.Main("eval", requiredFor(os.Args[1:]), run)
}

func run(ctx context.Context, cfg config.Config, log *slog.Logger, args []string) error {
	cmd, err := parseArgs(args, stderr)
	if errors.Is(err, errHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	switch cmd.name {
	case cmdScore:
		return runScore(cmd.score)
	case cmdNoise:
		return runNoise(cmd.noise)
	}
	return runSuite(ctx, cfg, log, cmd.flags)
}

// runSuite is eval run.
func runSuite(ctx context.Context, cfg config.Config, log *slog.Logger, f evals.Flags) error {
	if cfg.Fault != "" {
		return fmt.Errorf("eval run: %s is set; an eval run never injects faults, unset it", config.EnvCopilotFault)
	}
	if f.ModelFast != "" {
		cfg.LLMModelFast = f.ModelFast // this run only
	}

	path, err := evals.SuitePath(f.ScenariosDir, f.Suite)
	if err != nil {
		return err
	}
	suite, err := evals.LoadSuite(path)
	if err != nil {
		return err
	}
	months, err := suite.Filter(f.Only)
	if err != nil {
		return err
	}
	truthDir := filepath.Join(f.ScenariosDir, suite.Name, "ground_truth")

	profiles, err := company.LoadProfiles(filepath.Join(f.ConfigDir, "companies"))
	if err != nil {
		return err
	}
	rules, err := company.LoadRules(filepath.Join(f.ConfigDir, "rules.yaml"))
	if err != nil {
		return err
	}
	byID := make(map[string]company.Profile, len(profiles))
	for _, p := range profiles {
		byID[p.ID] = p
	}

	// A replay checks its fixtures before it touches anything else.
	var (
		replayer *evals.Replayer
		indexSHA string
	)
	if f.Replay {
		replayer, indexSHA, err = evals.OpenReplay(evals.ReplayOptions{Dir: f.FixturesDir, Suite: suite, TruthDir: truthDir, Months: months})
		if err != nil {
			return err
		}
	}

	st, err := store.Open(ctx, cfg.DatabaseURL.Reveal())
	if err != nil {
		return err
	}
	defer st.Close()

	wf := &agent.Workflow{
		Store:      st,
		Profiles:   byID,
		Rules:      rules,
		Config:     cfg,
		ResultsDir: f.ResultsDir,
		Log:        log,
	}
	var (
		closer evals.Closer = wf
		rec    *evals.Recorder
	)
	switch {
	case f.Replay:
		wf.Books, wf.Evidence = replayer.Books(), replayer.Evidence()
		closer = &evals.ScopedCloser{Closer: wf, Scope: replayer}
	default:
		reg, err := agent.NewRegistry(ctx, cfg, log)
		if err != nil {
			return err
		}
		defer reg.Close()
		books := &agent.MCPBooks{Registry: reg, Companies: st}
		evidence := &agent.MCPEvidence{Registry: reg}
		wf.Books, wf.Evidence = books, evidence
		if f.Record {
			if rec, err = evals.NewRecorder(f.FixturesDir, suite.Name); err != nil {
				return err
			}
			defer rec.Abort()
			rb := rec.Books(books)
			wf.Books, wf.Evidence = rb, rec.Evidence(evidence)
			closer = &evals.ScopedCloser{Closer: wf, Scope: rec, After: rb.PrimeMonth}
		}
	}

	agentOn := !f.NoAgent
	if agentOn {
		model, err := newProvider(cfg, filepath.Join(f.ConfigDir, "pricing.yaml"))
		if err != nil {
			return err
		}
		wf.Model = model
		wf.Explainer = &agent.LLMExplainer{
			Store:       st,
			Accounts:    agent.BooksAccounts{Books: wf.Books},
			FastModel:   cfg.LLMModelFast,
			StrongModel: cfg.LLMModelStrong,
			Log:         log,
		}
		wf.Verifier = &agent.CodeVerifier{Store: st, Citations: st}
	}
	if workflowHook != nil {
		workflowHook(wf)
	}

	r := &evals.Runner{
		Closer:              closer,
		Reader:              st,
		ResultsDir:          f.ResultsDir,
		Config:              manifestConfig(cfg),
		Flags:               f,
		Agent:               agentOn,
		Commit:              buildinfo.String(),
		FixturesIndexSHA256: indexSHA,
		Log:                 log,
	}
	log.Info("eval suite starting", "suite", suite.Name, "sha256", suite.SHA256, "months", len(months),
		"agent", agentOn, "replay", f.Replay, "record", f.Record)
	dir, man, err := r.Run(ctx, suite, months)
	if dir != "" {
		_, _ = fmt.Fprintf(stdout, "%s\n", filepath.Join(dir, evals.ManifestFile))
	}
	log.Info("eval suite ended", "suite", suite.Name, "results", dir, "runs", len(man.Results), "failed", man.Failed)

	if rec != nil {
		if ctx.Err() != nil {
			return errors.Join(err, fmt.Errorf("eval run --record: cancelled; fixtures not written: %w", ctx.Err()))
		}
		idx, sum, ferr := rec.Finish(ctx, evals.RecordMeta{
			Suite: suite, TruthDir: truthDir, Months: months, SeederCommit: buildinfo.String(),
			RecordedAt: man.FinishedAt, Profiles: profiles, ExtraAllowed: recordScanExtra,
		})
		if ferr != nil {
			return errors.Join(err, ferr)
		}
		log.Info("fixtures recorded", "dir", filepath.Join(f.FixturesDir, suite.Name), "files", len(idx.Files),
			"months", len(idx.Months), "index_sha256", sum)
	}
	return err
}

// newProvider builds the model provider LLM_PROVIDER names, priced from
// the pricing table at pricingPath, as the agent binary does. The
// workflow wraps it per step in a recording, budget-checked provider.
func newProvider(cfg config.Config, pricingPath string) (llm.Provider, error) {
	pricing, err := llm.LoadPricing(pricingPath)
	if err != nil {
		return nil, err
	}
	switch cfg.LLMProvider {
	case config.ProviderAnthropic:
		return llm.NewAnthropicProvider(cfg, pricing)
	case config.ProviderClaudeCLI, "":
		return llm.NewClaudeCLI("", nil, pricing), nil
	}
	return nil, fmt.Errorf("eval: unknown %s %.40q", config.EnvLLMProvider, cfg.LLMProvider)
}

// manifestConfig picks the non-secret settings that shape a run's
// results. No config.Secret is read, and no URL (one can carry a token).
func manifestConfig(cfg config.Config) evals.ManifestConfig {
	return evals.ManifestConfig{
		LLMProvider:       cfg.LLMProvider,
		LLMModelFast:      cfg.LLMModelFast,
		LLMModelStrong:    cfg.LLMModelStrong,
		LLMRunTokenCap:    cfg.LLMRunTokenCap,
		LLMDailyBudgetUSD: strconv.FormatFloat(cfg.LLMDailyBudgetUSD, 'f', -1, 64),
	}
}
