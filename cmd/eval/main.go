// Command eval is the eval harness: runs closes on a seeded suite and scores them against ground truth.
//
// Built in CC-901 (runner) and CC-902 (scoring); the CI gate comes in
// CC-905. Usage:
//
//	eval run --suite suite-v1 [--only sharma:2026-09] [--model-fast X] [--no-agent]
//	         [--results-dir results] [--scenarios-dir evals/scenarios] [--config-dir config]
//	eval score <results-dir> [--truth-dir D] [--out D] [--require EXPR]... [--compare FILE]
//	         [--baseline-out FILE] [--noise FILE] [--pricing config/pricing.yaml]
//	eval noise <score.json> <score.json>... --out FILE
//
// score and noise read files only and need no environment variables; run
// needs DATABASE_URL and the MCP settings.
//
// run closes every evaluated company-month of the suite and its clean
// control month, one after another, through the same agent.Workflow as
// the agent binary, and writes results/<suite>/<timestamp>/<company>-<month>.json
// plus manifest.json. It exits non-zero if any run ends failed.
//
// Like the agent, eval reaches ERPNext and the evidence store only through
// the read-only MCP servers; this binary must not link the ERPNext client
// (deps_test.go). It calls no model itself.
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
	if cmd.flags.ModelFast != "" {
		cfg.LLMModelFast = cmd.flags.ModelFast // this run only
	}

	path, err := evals.SuitePath(cmd.flags.ScenariosDir, cmd.flags.Suite)
	if err != nil {
		return err
	}
	suite, err := evals.LoadSuite(path)
	if err != nil {
		return err
	}
	months, err := suite.Filter(cmd.flags.Only)
	if err != nil {
		return err
	}

	profiles, err := company.LoadProfiles(filepath.Join(cmd.flags.ConfigDir, "companies"))
	if err != nil {
		return err
	}
	rules, err := company.LoadRules(filepath.Join(cmd.flags.ConfigDir, "rules.yaml"))
	if err != nil {
		return err
	}
	byID := make(map[string]company.Profile, len(profiles))
	for _, p := range profiles {
		byID[p.ID] = p
	}

	st, err := store.Open(ctx, cfg.DatabaseURL.Reveal())
	if err != nil {
		return err
	}
	defer st.Close()

	reg, err := agent.NewRegistry(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer reg.Close()

	wf := &agent.Workflow{
		Store:    st,
		Books:    &agent.MCPBooks{Registry: reg, Companies: st},
		Evidence: &agent.MCPEvidence{Registry: reg},
		Profiles: byID,
		Rules:    rules,
		// No explainer or verifier: the eval path runs without the agent
		// until the explainer is wired in (after CC-704), so every run
		// ends partial and the manifest records agent: false.
		Config:     cfg,
		ResultsDir: cmd.flags.ResultsDir,
		Log:        log,
	}

	r := &evals.Runner{
		Closer:     wf,
		Reader:     st,
		ResultsDir: cmd.flags.ResultsDir,
		Config:     manifestConfig(cfg),
		Flags:      cmd.flags,
		Agent:      false,
		Commit:     buildinfo.String(),
		Log:        log,
	}
	log.Info("eval suite starting", "suite", suite.Name, "sha256", suite.SHA256, "months", len(months), "agent", false)
	dir, man, err := r.Run(ctx, suite, months)
	if dir != "" {
		_, _ = fmt.Fprintf(stdout, "%s\n", filepath.Join(dir, evals.ManifestFile))
	}
	log.Info("eval suite ended", "suite", suite.Name, "results", dir, "runs", len(man.Results), "failed", man.Failed)
	return err
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
