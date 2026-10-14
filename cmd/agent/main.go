// Command agent is the agent service: the close workflow, explainer, verifier and investigator, plus the web UI and JSON API.
//
// Built in CC-703 (workflow) and CC-1001 (web app). Usage:
//
//	agent close --company sharma --month 2026-09 [--results-dir results] [--timeout 10m] [--config-dir config] [--no-explain]
//	agent resume <run_id> [--results-dir results] [--timeout 10m] [--config-dir config] [--no-explain]
//	agent mint-scope --company sharma --month 2026-09 [--ttl 1h]
//	agent serve [--results-dir results] [--config-dir config] [--no-explain]
//
// Each finding is explained by the LLM explainer (CC-704) through
// LLM_PROVIDER: claude-cli (the default, the local claude -p, no API key)
// or anthropic, and each explanation is checked by the code verifier
// (CC-705), which traces every number, citation and journal line back to
// the stored evidence; a failed check is retried with the violations, at
// most twice, and then left for review. Model prices come from
// <config-dir>/pricing.yaml. --no-explain calls no model: the explain and
// verify steps are skipped and the run ends partial.
//
// Explaining needs PSEUDONYM_KEY (32 random bytes, hex): GSTINs, PANs and
// the names of suppliers marked individual in the company profile are
// replaced by per-run tokens before any model call and restored in what
// the model writes (CC-710). close and resume refuse to start without a
// valid key unless --no-explain is given.
//
// COPILOT_FAULT=corrupt_explanation (testing only) corrupts one
// explanation on its first attempt so the verifier's retry can be seen;
// the run report says when it is on.
//
// close runs one month-end close and resume finishes a run that stopped
// (a crash or kill -9 leaves it running). Ctrl-C (SIGINT or SIGTERM)
// cancels the run, which is marked failed with reason "cancelled", and
// the command exits non-zero. On success it prints one JSON line with the
// run ID, its status and the report path.
//
// serve starts the web UI and JSON API (CC-1001 to CC-1003, and POST
// /api/ask of CC-1006) on APP_ADDR (default :8000). Closes started from the
// UI run inside this process; Ctrl-C stops the server and cancels them (a
// cancelled run stays running and resume finishes it). serve needs
// APP_SESSION_KEY and MCP_SCOPE_KEY, and PSEUDONYM_KEY unless --no-explain
// (which also turns /api/ask off); users come from <config-dir>/users.yaml.
//
// The agent reaches ERPNext and the evidence store only through the
// read-only MCP servers; this binary must not link the ERPNext client
// (deps_test.go). Each run's MCP calls carry a scope signed with
// MCP_SCOPE_KEY that binds them to the run's company and months (CC-506).
// mint-scope prints such a scope (an X-Run-Scope header value, valid for
// --ttl, at most 24h) and nothing else, for a manual MCP Inspector session;
// it needs MCP_SCOPE_KEY, and, like every command, the variables cli.Main
// requires, though it uses none of them.
package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/abhishekjha/close-copilot/internal/agent"
	"github.com/abhishekjha/close-copilot/internal/cli"
	"github.com/abhishekjha/close-copilot/internal/company"
	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/llm"
	"github.com/abhishekjha/close-copilot/internal/mcpkit"
	"github.com/abhishekjha/close-copilot/internal/store"
	"github.com/abhishekjha/close-copilot/internal/telemetry"
	"github.com/abhishekjha/close-copilot/internal/telemetry/llmtrace"
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
	if cmd.name == cmdMintScope {
		return mintScope(cfg, cmd, time.Now(), stdout)
	}
	if cmd.name == cmdServe {
		return serve(ctx, cfg, log, cmd)
	}
	stopTrace, err := telemetry.Init(ctx, "agent", cfg, log)
	if err != nil {
		return err
	}
	defer stopTrace()

	pseudonymKeyBytes, err := explainKey(cfg, log, cmd)
	if err != nil {
		return err
	}
	d, err := openDeps(ctx, cfg, log, cmd)
	if err != nil {
		return err
	}
	defer d.close()

	var model llm.Provider
	var names []string
	if !cmd.noExplain {
		if model, err = newProvider(cfg, filepath.Join(cmd.configDir, "pricing.yaml")); err != nil {
			return err
		}
		companyID := cmd.company
		if cmd.name == cmdResume {
			r, err := d.st.GetCloseRun(ctx, cmd.runID)
			if err != nil {
				return err
			}
			companyID = r.CompanyID
		}
		names = d.byID[companyID].IndividualNames()
	}
	wf, err := newWorkflow(cfg, log, cmd, d, model, pseudonymKeyBytes, names)
	if err != nil {
		return err
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

// explainKey checks what explaining needs (the LLM settings and
// PSEUDONYM_KEY) unless cmd.noExplain, and logs the LLM settings. It
// returns the decoded pseudonym key, nil with --no-explain.
func explainKey(cfg config.Config, log *slog.Logger, cmd command) ([]byte, error) {
	var key []byte
	if !cmd.noExplain {
		if err := cfg.CheckLLM(); err != nil {
			return nil, err
		}
		var err error
		if key, err = pseudonymKey(cfg); err != nil {
			return nil, err
		}
	}
	log.Info("llm", "provider", cfg.LLMProvider, "fast", cfg.LLMModelFast, "strong", cfg.LLMModelStrong, "run_token_cap", cfg.LLMRunTokenCap)
	return key, nil
}

// deps are what the workflow (and the web server) are built on.
type deps struct {
	st       *store.Store
	reg      *agent.Registry
	profiles []company.Profile
	byID     map[string]company.Profile
	rules    company.Rules
}

func (d deps) close() {
	d.reg.Close()
	d.st.Close()
}

// openDeps loads the profiles and rules from cmd.configDir, opens the store
// and connects the MCP registry. The caller closes the result.
func openDeps(ctx context.Context, cfg config.Config, log *slog.Logger, cmd command) (deps, error) {
	profiles, err := company.LoadProfiles(filepath.Join(cmd.configDir, "companies"))
	if err != nil {
		return deps{}, err
	}
	rules, err := company.LoadRules(filepath.Join(cmd.configDir, "rules.yaml"))
	if err != nil {
		return deps{}, err
	}
	byID := make(map[string]company.Profile, len(profiles))
	for _, p := range profiles {
		byID[p.ID] = p
	}
	st, err := store.Open(ctx, cfg.DatabaseURL.Reveal())
	if err != nil {
		return deps{}, err
	}
	reg, err := agent.NewRegistry(ctx, cfg, log)
	if err != nil {
		st.Close()
		return deps{}, err
	}
	return deps{st: st, reg: reg, profiles: profiles, byID: byID, rules: rules}, nil
}

// newWorkflow builds the close workflow. With cmd.noExplain it has no
// explainer, verifier or retriever; otherwise model is the provider and
// names the individuals' names to pseudonymise.
func newWorkflow(cfg config.Config, log *slog.Logger, cmd command, d deps, model llm.Provider, key []byte, names []string) (*agent.Workflow, error) {
	books := &agent.MCPBooks{Registry: d.reg, Companies: d.st}
	wf := &agent.Workflow{
		Store:      d.st,
		Books:      books,
		Evidence:   &agent.MCPEvidence{Registry: d.reg},
		Profiles:   d.byID,
		Rules:      d.rules,
		Checks:     agent.DefaultChecks(),
		Config:     cfg,
		ResultsDir: cmd.resultsDir,
		Timeout:    cmd.timeout,
		Log:        log,
	}
	if cmd.noExplain {
		log.Info("explaining disabled (--no-explain)")
		return wf, nil
	}
	fault, err := agent.NewFault(cfg.Fault)
	if err != nil {
		return nil, err
	}
	if fault != nil {
		log.Warn("COPILOT_FAULT is on: one explanation will be corrupted on its first attempt", "fault", fault.Mode())
	}
	// Counts only: names are personal data.
	log.Info("pseudonymisation on", "individual_names", len(names))
	wf.Model = llmtrace.Provider(model)
	wf.Explainer = &agent.LLMExplainer{
		Store:       d.st,
		Accounts:    agent.BooksAccounts{Books: books},
		FastModel:   cfg.LLMModelFast,
		StrongModel: cfg.LLMModelStrong,
		NewPseudonymiser: func(runID uuid.UUID) agent.Pseudonymiser {
			return agent.NewRedactor(key, runID, names)
		},
		Fault: fault,
		Log:   log,
	}
	wf.Verifier = &agent.CodeVerifier{Store: d.st, Citations: d.st}
	// Retrieval is a workflow step (CC-806): its passages reach only the
	// explainer, never the investigator.
	wf.Retriever = &agent.MCPDocuments{Registry: d.reg}
	return wf, nil
}

// mintScope prints the X-Run-Scope header value of a manual session of
// cmd.company for cmd.month, valid for cmd.ttl from now. The run ID is
// manual-<uuid>, which the audit log shows as agent:run/manual-<uuid>.
func mintScope(cfg config.Config, cmd command, now time.Time, out io.Writer) error {
	if cfg.MCPScopeKey.IsZero() {
		return errors.New("agent: mint-scope needs " + config.EnvMCPScopeKey + " (the key the MCP servers verify with)")
	}
	sc, err := mcpkit.Window(cmd.company, "manual-"+uuid.NewString(), cmd.month, now.Add(cmd.ttl))
	if err != nil {
		return fmt.Errorf("agent: mint-scope: %w", err)
	}
	header, err := mcpkit.Mint([]byte(cfg.MCPScopeKey.Reveal()), sc)
	if err != nil {
		return fmt.Errorf("agent: mint-scope: %w", err)
	}
	_, err = fmt.Fprintln(out, header)
	return err
}

// errPseudonymKey is the fixed error for a missing or malformed
// PSEUDONYM_KEY; it never carries the value.
var errPseudonymKey = errors.New("agent: " + config.EnvPseudonymKey +
	" must be 32 random bytes as 64 hex digits to explain findings (see .env.example), or pass --no-explain")

// pseudonymKey decodes PSEUDONYM_KEY: 32 bytes of hex, or errPseudonymKey.
func pseudonymKey(cfg config.Config) ([]byte, error) {
	if cfg.PseudonymKey.IsZero() {
		return nil, errPseudonymKey
	}
	key, err := hex.DecodeString(strings.TrimSpace(cfg.PseudonymKey.Reveal()))
	if err != nil || len(key) != 32 {
		return nil, errPseudonymKey
	}
	return key, nil
}

// newProvider builds the model provider LLM_PROVIDER names, priced from
// the pricing table at pricingPath. The workflow wraps it per step in a
// recording, token-capped provider.
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
	return nil, fmt.Errorf("agent: unknown %s %.40q", config.EnvLLMProvider, cfg.LLMProvider)
}
