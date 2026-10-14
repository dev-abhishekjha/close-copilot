// Command eval is the eval harness: runs closes on a seeded suite and scores them against ground truth.
//
// Built in CC-901 (runner), CC-902 (scoring) and CC-905 (recorded
// fixtures and the CI gate). Usage:
//
//	eval run --suite suite-v1 [--only sharma:2026-09] [--model-fast X] [--no-llm|--no-agent]
//	         [--checks bankrec|all]
//	         [--record | --replay] [--fixtures-dir evals/fixtures]
//	         [--results-dir results] [--scenarios-dir evals/scenarios] [--config-dir config]
//	eval score <results-dir> [--truth-dir D] [--citations FILE] [--fixtures-dir D] [--out D] [--require EXPR]... [--compare FILE]
//	         [--max-p95-increase-pct N] [--min-latency-delta-ms N] [--max-cost-increase-pct N]
//	         [--baseline-out FILE] [--noise FILE] [--pricing config/pricing.yaml]
//	eval noise <score.json> <score.json>... --out FILE
//	eval refusal --out DIR [--rules-only | --live | --record | --replay] [--golden FILE] [--triage-rules FILE]
//	         [--companies DIR] [--fixtures-dir DIR] [--pricing FILE] [--require EXPR]...
//	eval score ... [--refusal refusal.json] [--golden evals/golden/refusal.yaml]
//
// score and noise read files only and need no environment variables; run
// needs DATABASE_URL and the MCP settings, run --replay DATABASE_URL only.
// With the agent on (no --no-llm or --no-agent, replay or not) run also
// needs PSEUDONYM_KEY, so identifiers are masked before every model call.
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
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/abhishekjha/close-copilot/internal/agent"
	"github.com/abhishekjha/close-copilot/internal/buildinfo"
	"github.com/abhishekjha/close-copilot/internal/cli"
	"github.com/abhishekjha/close-copilot/internal/company"
	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/evals"
	"github.com/abhishekjha/close-copilot/internal/faults"
	"github.com/abhishekjha/close-copilot/internal/llm"
	"github.com/abhishekjha/close-copilot/internal/mcpkit"
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
	case cmdRefusal:
		return runRefusal(ctx, cfg, cmd.refusal)
	}
	return runSuite(ctx, cfg, log, cmd.flags, cmd.fault)
}

// runSuite is eval run.
func runSuite(ctx context.Context, cfg config.Config, log *slog.Logger, f evals.Flags, ft faultFlags) error {
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

	// The explainer masks identifiers before every model call (CC-710), so
	// with the agent on a missing or malformed key stops the run here,
	// before the fixtures, the store, the MCP registry or the model.
	agentOn := !f.NoAgent
	pseudoKey, err := explainerKey(cfg, agentOn)
	if err != nil {
		return err
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
	if f.Checks == checksAll {
		wf.Checks = agent.DefaultChecks()
	}
	var (
		closer evals.Closer = wf
		rec    *evals.Recorder
	)
	switch {
	case f.Replay:
		wf.Books, wf.Evidence = replayer.Books(), replayer.Evidence()
		if agentOn {
			wf.Retriever = replayRetriever(replayer, log, suite.Name)
		}
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
		if agentOn {
			wf.Retriever = &agent.MCPDocuments{Registry: reg}
		}
		if f.Record {
			if rec, err = evals.NewRecorder(f.FixturesDir, suite.Name); err != nil {
				return err
			}
			defer rec.Abort()
			rb := rec.Books(books)
			wf.Books, wf.Evidence = rb, rec.Evidence(evidence)
			if wf.Retriever != nil {
				wf.Retriever = rec.Documents(wf.Retriever)
			}
			prime := primeScope(cfg.MCPScopeKey.Reveal(), time.Now, agent.WithRunScope, rb.PrimeMonth)
			corpus := recordCorpus(rec, st.Pool())
			closer = &evals.ScopedCloser{Closer: wf, Scope: rec, After: func(ctx context.Context, companyID, month string) error {
				return errors.Join(prime(ctx, companyID, month), corpus(ctx, companyID))
			}}
		}
	}

	if agentOn {
		model, err := newProvider(cfg, filepath.Join(f.ConfigDir, "pricing.yaml"))
		if err != nil {
			return err
		}
		wf.Model = model
		names := suiteIndividualNames(byID, months)
		// Counts only: names are personal data, never logged.
		log.Info("pseudonymisation on", "individual_names", len(names))
		wf.Explainer = newExplainer(st, agent.BooksAccounts{Books: wf.Books}, cfg, pseudoKey, names, log)
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
	if len(ft.Specs) > 0 {
		if replayer == nil {
			return errors.New("eval run: --faults needs --replay")
		}
		return runWithFaults(ctx, log, wf, replayer, r, suite, months, truthDir, ft)
	}
	log.Info("eval suite starting", "suite", suite.Name, "sha256", suite.SHA256, "months", len(months),
		"agent", agentOn, "checks", f.Checks, "replay", f.Replay, "record", f.Record)
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

// runWithFaults is eval run --faults (CC-1101). It runs the replayed suite
// twice over the same fixtures: clean into a temporary folder, then with the
// erp faults injected into the books reader into r.ResultsDir itself (so
// eval score <results-dir> finds the manifest). It scores both and writes
// the comparison, recall lost included, to ft.Out. A faulted month that ends
// failed is an expected outcome, not an error; the faults-off run must pass.
func runWithFaults(ctx context.Context, log *slog.Logger, wf *agent.Workflow, replayer *evals.Replayer, r *evals.Runner,
	suite evals.Suite, months []evals.SuiteMonth, truthDir string, ft faultFlags) error {
	ctrlDir, err := os.MkdirTemp("", "eval-faults-off-")
	if err != nil {
		return fmt.Errorf("eval run --faults: %w", err)
	}
	defer func() { _ = os.RemoveAll(ctrlDir) }()

	log.Info("eval faults: running the suite without faults", "suite", suite.Name)
	off := *r
	off.ResultsDir, off.Flags.ResultsDir = ctrlDir, ctrlDir
	wf.ResultsDir = ctrlDir
	offDir, _, err := off.Run(ctx, suite, months)
	if err != nil {
		return fmt.Errorf("eval run --faults: the run without faults failed: %w", err)
	}
	clean, err := evals.ScoreRun(evals.ScoreOptions{ResultsDir: offDir, TruthDir: truthDir})
	if err != nil {
		return err
	}

	specs := make([]string, len(ft.Specs))
	for i, sp := range ft.Specs {
		specs[i] = sp.String()
	}
	log.Info("eval faults: running the suite with faults", "suite", suite.Name, "faults", strings.Join(specs, ","), "seed", ft.Seed)
	wf.Books = &faults.Books{Reader: replayer.Books(), Inj: faults.NewInjector(ft.Specs, ft.Seed)}
	wf.ResultsDir = r.ResultsDir
	on := *r
	on.FlatDir = true
	onDir, man, err := on.Run(ctx, suite, months)
	if onDir != "" {
		_, _ = fmt.Fprintf(stdout, "%s\n", filepath.Join(onDir, evals.ManifestFile))
	}
	if err != nil && !errors.Is(err, evals.ErrRunsFailed) {
		return err
	}
	log.Info("eval faults: suite ended", "suite", suite.Name, "results", onDir, "failed", man.Failed)
	faulted, err := evals.ScoreRun(evals.ScoreOptions{ResultsDir: onDir, TruthDir: truthDir})
	if err != nil {
		return err
	}

	if err := evals.CheckOutputPath("--faults-out", ft.Out); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(ft.Out), 0o750); err != nil {
		return fmt.Errorf("eval run --faults: %w", err)
	}
	md := faultsReport(suite.Name, strings.Join(specs, ","), ft.Seed, clean, faulted)
	if err := os.WriteFile(ft.Out, []byte(md), 0o600); err != nil {
		return fmt.Errorf("eval run --faults: %w", err)
	}
	_, _ = fmt.Fprintf(stdout, "%s\n", ft.Out)
	return nil
}

// faultsReport is the Markdown of evals/faults/results.md: the scores with
// faults off and on, and every planted item the faults made the run miss.
func faultsReport(suite, specs string, seed int64, clean, faulted evals.Score) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Eval under injected faults\n\n")
	fmt.Fprintf(&b, "Suite `%s`, replayed without a model. Faults `%s`, seed %d. Written by `eval run --faults`; `evals/baseline.json` is untouched.\n\n", suite, specs, seed)
	b.WriteString("| Metric | Faults off | Faults on |\n| --- | --- | --- |\n")
	row := func(name, off, on string) { fmt.Fprintf(&b, "| %s | %s | %s |\n", name, off, on) }
	row("Recall", fmt.Sprintf("%s (%s)", clean.Overall.Recall.Percent, ratioText(clean.Overall.Recall)), fmt.Sprintf("%s (%s)", faulted.Overall.Recall.Percent, ratioText(faulted.Overall.Recall)))
	row("Precision", fmt.Sprintf("%s (%s)", clean.Overall.Precision.Percent, ratioText(clean.Overall.Precision)), fmt.Sprintf("%s (%s)", faulted.Overall.Precision.Percent, ratioText(faulted.Overall.Precision)))
	row("False alarms", strconv.FormatInt(clean.Overall.FalseAlarms, 10), strconv.FormatInt(faulted.Overall.FalseAlarms, 10))
	row("Clean-month false alarms", strconv.FormatInt(clean.Clean.FalseAlarms, 10), strconv.FormatInt(faulted.Clean.FalseAlarms, 10))
	row("Failed runs", strconv.Itoa(len(clean.FailedRuns)), strconv.Itoa(len(faulted.FailedRuns)))
	b.WriteString("\n")

	caught := map[string]bool{}
	for _, it := range faulted.Items {
		if it.Outcome == evals.OutcomeCaught {
			caught[it.Key] = true
		}
	}
	var lost []string
	for _, it := range clean.Items {
		if it.Outcome == evals.OutcomeCaught && !caught[it.Key] {
			lost = append(lost, fmt.Sprintf("- `%s` (%s) caught without faults, missed with them", it.Key, it.Type))
		}
	}
	slices.Sort(lost)
	b.WriteString("## Recall lost\n\n")
	if len(lost) == 0 {
		b.WriteString("None: every item caught without faults was caught with them.\n")
	} else {
		b.WriteString(strings.Join(lost, "\n") + "\n")
	}
	if len(faulted.FailedRuns) > 0 {
		b.WriteString("\n## Runs that ended failed under faults\n\n")
		for _, r := range faulted.FailedRuns {
			reason, _, _ := strings.Cut(r.Reason, "; agent: run failed")
			fmt.Fprintf(&b, "- %s %s: %s\n", r.Company, r.Month, reason)
		}
	}
	return b.String()
}

func ratioText(r evals.Ratio) string { return fmt.Sprintf("%d/%d", r.Num, r.Den) }

// errPseudonymKey is the startup error for a missing or malformed
// PSEUDONYM_KEY when the explainer runs. It never carries the value.
var errPseudonymKey = errors.New("eval: " + config.EnvPseudonymKey +
	" must be 32 random bytes as 64 hex digits to explain findings (see .env.example), or pass --no-llm")

// pseudonymKey decodes PSEUDONYM_KEY: 32 bytes as 64 hex digits, as the
// agent binary requires. Any other value is errPseudonymKey.
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

// explainerKey is the pseudonym key when the explainer runs (agentOn);
// with the agent off the key is neither read nor required (nil, nil).
func explainerKey(cfg config.Config, agentOn bool) ([]byte, error) {
	if !agentOn {
		return nil, nil
	}
	return pseudonymKey(cfg)
}

// newExplainer builds the explainer as the agent binary does: every model
// call goes through a per-run Redactor keyed by key, which masks GSTINs,
// PANs and the individual names (CC-710).
func newExplainer(st agent.ExplainStore, accounts agent.AccountLister, cfg config.Config, key []byte, names []string, log *slog.Logger) *agent.LLMExplainer {
	return &agent.LLMExplainer{
		Store:       st,
		Accounts:    accounts,
		FastModel:   cfg.LLMModelFast,
		StrongModel: cfg.LLMModelStrong,
		NewPseudonymiser: func(runID uuid.UUID) agent.Pseudonymiser {
			return agent.NewRedactor(key, runID, names)
		},
		Log: log,
	}
}

// suiteIndividualNames is the union, without repeats, of the individual
// names of every company the months cover. The pseudonymiser is per run,
// not per company, so a name is masked in every company's run: masking
// another company's name is harmless, leaving one unmasked is not.
func suiteIndividualNames(byID map[string]company.Profile, months []evals.SuiteMonth) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range months {
		if seen[m.Company] {
			continue
		}
		seen[m.Company] = true
		for _, n := range byID[m.Company].IndividualNames() {
			if !slices.Contains(out, n) {
				out = append(out, n)
			}
		}
	}
	return out
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

// primeTTL is how long a priming scope lives: one trial-balance call.
const primeTTL = 5 * time.Minute

// primeScope wraps a recording's priming call (RecordingBooks.PrimeMonth)
// so it runs inside a run scope, as the workflow's own calls do (CC-506):
// a fresh run ID, the company and the window of the month only
// (mcpkit.Window), valid for primeTTL. withScope puts the signed header on
// the context (agent.WithRunScope). With an empty key (unscoped test
// servers) ctx passes through unchanged. A scope that can't be minted is
// an error, never a skipped call, so the recording aborts.
func primeScope(key string, now func() time.Time, withScope func(context.Context, string) context.Context,
	prime func(ctx context.Context, company, month string) error,
) func(ctx context.Context, company, month string) error {
	return func(ctx context.Context, companyID, month string) error {
		if key == "" {
			return prime(ctx, companyID, month)
		}
		sc, err := mcpkit.Window(companyID, "eval-prime-"+uuid.NewString(), month, now().Add(primeTTL))
		if err != nil {
			return fmt.Errorf("eval: prime scope: %w", err)
		}
		header, err := mcpkit.Mint([]byte(key), sc)
		if err != nil {
			return fmt.Errorf("eval: prime scope: %w", err)
		}
		return prime(withScope(ctx, header), companyID, month)
	}
}

// corpusQuerier is the part of the pgx pool recordCorpus reads doc_chunks with.
type corpusQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// recordCorpus returns a hook that records, once per company, the
// (doc_id, section) pairs the company may cite: its own chunks plus the
// shared ones (company_id IS NULL), as Store.CitationExists reads them.
// Pairs only; no passage text is read.
func recordCorpus(rec *evals.Recorder, db corpusQuerier) func(ctx context.Context, companyID string) error {
	done := map[string]bool{}
	return func(ctx context.Context, companyID string) error {
		if done[companyID] {
			return nil
		}
		rows, err := db.Query(ctx, `SELECT DISTINCT doc_id, section FROM doc_chunks
			WHERE company_id = $1 OR company_id IS NULL ORDER BY doc_id, section`, companyID)
		if err != nil {
			return fmt.Errorf("eval: record corpus: %w", err)
		}
		defer rows.Close()
		var sections []evals.CitationRef
		for rows.Next() {
			var c evals.CitationRef
			if err := rows.Scan(&c.DocID, &c.Section); err != nil {
				return fmt.Errorf("eval: record corpus: %w", err)
			}
			sections = append(sections, c)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("eval: record corpus: %w", err)
		}
		done[companyID] = true
		return rec.RecordCorpus(companyID, sections)
	}
}

// replayRetriever is the retriever of an agent-on replay: the recorded
// searches, or none (the workflow's single skipped retrieve step) when the
// suite was recorded without any, as the committed suites were. It returns
// a nil interface in that case, never a typed nil.
func replayRetriever(rp *evals.Replayer, log *slog.Logger, suite string) agent.Retriever {
	if !rp.HasDocuments() {
		log.Info("no search_documents fixtures: citations are unmeasured until the suite is re-recorded", "suite", suite)
		return nil
	}
	return rp.Documents()
}
