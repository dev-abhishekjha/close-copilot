// Command agent is the agent service: the close workflow, explainer, verifier and investigator, plus the web UI and JSON API.
//
// Built in CC-703 (workflow) and CC-1001 (web app). Usage:
//
//	agent close --company sharma --month 2026-09 [--results-dir results] [--timeout 10m] [--config-dir config]
//	agent resume <run_id> [--results-dir results] [--timeout 10m] [--config-dir config]
//
// close runs one month-end close and resume finishes a run that stopped
// (a crash or kill -9 leaves it running). Ctrl-C (SIGINT or SIGTERM)
// cancels the run, which is marked failed with reason "cancelled", and
// the command exits non-zero. On success it prints one JSON line with the
// run ID, its status and the report path.
//
// The agent reaches ERPNext and the evidence store only through the
// read-only MCP servers; this binary must not link the ERPNext client
// (deps_test.go).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/google/uuid"

	"github.com/abhishekjha/close-copilot/internal/agent"
	"github.com/abhishekjha/close-copilot/internal/cli"
	"github.com/abhishekjha/close-copilot/internal/company"
	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/store"
)

// stdout receives the result line.
var stdout io.Writer = os.Stdout

func main() {
	cli.Main("agent", []string{
		config.EnvDatabaseURL,
		config.EnvBooksMCPURL,
		config.EnvEvidenceMCPURL,
		config.EnvMCPTokenAgent,
	}, run)
}

// resultLine is the JSON line printed when a command ends.
type resultLine struct {
	RunID    string `json:"run_id"`
	Status   string `json:"status"`
	Reason   string `json:"reason,omitempty"`
	Report   string `json:"report,omitempty"`
	Findings int    `json:"findings"`
}

func run(ctx context.Context, cfg config.Config, log *slog.Logger, args []string) error {
	cmd, err := parseArgs(args)
	if err != nil {
		return err
	}
	if err := cfg.CheckLLM(); err != nil {
		return err
	}
	log.Info("llm", "provider", cfg.LLMProvider, "fast", cfg.LLMModelFast, "strong", cfg.LLMModelStrong, "run_token_cap", cfg.LLMRunTokenCap)

	profiles, err := company.LoadProfiles(filepath.Join(cmd.configDir, "companies"))
	if err != nil {
		return err
	}
	rules, err := company.LoadRules(filepath.Join(cmd.configDir, "rules.yaml"))
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
		// No explainer or verifier until CC-704 and CC-705: their steps
		// are skipped and a run ends partial.
		Config:     cfg,
		ResultsDir: cmd.resultsDir,
		Timeout:    cmd.timeout,
		Log:        log,
	}

	var res agent.Result
	switch cmd.name {
	case cmdClose:
		res, err = wf.RunClose(ctx, cmd.company, cmd.month)
	case cmdResume:
		res, err = wf.ResumeClose(ctx, cmd.runID)
	}
	if res.RunID != uuid.Nil {
		line, merr := json.Marshal(resultLine{
			RunID: res.RunID.String(), Status: res.Status, Reason: res.Reason,
			Report: res.ReportPath, Findings: res.Findings,
		})
		if merr == nil {
			_, _ = fmt.Fprintln(stdout, string(line))
		}
	}
	return err
}
