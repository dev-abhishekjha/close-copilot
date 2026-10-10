package evals

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/abhishekjha/close-copilot/internal/agent"
	"github.com/abhishekjha/close-copilot/internal/store"
)

// Closer runs one close; *agent.Workflow satisfies it. The runner calls
// the same RunClose as the app, so a suite run evaluates the real system.
type Closer interface {
	RunClose(ctx context.Context, company, month string) (agent.Result, error)
}

// RunReader reads what a result file exports about a run; *store.Store
// satisfies it.
type RunReader interface {
	GetCloseRun(ctx context.Context, id uuid.UUID) (store.CloseRun, error)
	ListFindingsByRun(ctx context.Context, runID uuid.UUID) ([]store.Finding, error)
	ListSteps(ctx context.Context, runID uuid.UUID) ([]store.Step, error)
	ListLLMCalls(ctx context.Context, stepID uuid.UUID) ([]store.LLMCall, error)
}

// ErrRunsFailed is returned when at least one month's run ended failed;
// every month still ran and has its result file.
var ErrRunsFailed = errors.New("evals: runs failed")

// Runner runs a suite's months one after another and writes their result
// files and the manifest under ResultsDir/<suite>/<timestamp>/.
type Runner struct {
	Closer Closer
	Reader RunReader

	ResultsDir string
	Config     ManifestConfig
	Flags      Flags
	// Agent records whether the explainer and verifier ran (false until
	// the explainer is wired into the eval path).
	Agent  bool
	Commit string

	Now func() time.Time
	Log *slog.Logger
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

func (r *Runner) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.New(slog.DiscardHandler)
}

// Run closes every month in order, exporting each month's result file as
// soon as its run ends, then writes the manifest. A failed month doesn't
// stop the suite: Run returns the folder it wrote and an error wrapping
// ErrRunsFailed when any run ended failed. A setup error (no folder)
// returns before any close.
func (r *Runner) Run(ctx context.Context, suite Suite, months []SuiteMonth) (string, Manifest, error) {
	if r.Closer == nil || r.Reader == nil {
		return "", Manifest{}, errors.New("evals: runner has no closer or reader")
	}
	if len(months) == 0 {
		return "", Manifest{}, errors.New("evals: no months to run")
	}
	started := r.now()
	dir := filepath.Join(r.ResultsDir, suite.Name, started.Format(TimestampLayout))
	if err := os.MkdirAll(filepath.Dir(dir), 0o750); err != nil {
		return "", Manifest{}, fmt.Errorf("evals: results folder: %w", err)
	}
	// Mkdir, not MkdirAll: an existing folder is another run's.
	if err := os.Mkdir(dir, 0o750); err != nil {
		return "", Manifest{}, fmt.Errorf("evals: results folder: %w", err)
	}

	man := Manifest{
		Suite:     ManifestSuite{Name: suite.Name, Path: suite.Path, SHA256: suite.SHA256},
		Config:    r.Config,
		Flags:     r.Flags,
		Agent:     r.Agent,
		Commit:    r.Commit,
		StartedAt: started,
		Results:   []ManifestEntry{},
	}
	if man.Flags.Only == nil {
		man.Flags.Only = []string{}
	}

	var failed []string
	for _, m := range months {
		if ctx.Err() != nil {
			// The suite was cancelled: the months left never ran.
			failed = append(failed, m.Key())
			man.Results = append(man.Results, ManifestEntry{
				Company: m.Company, Month: m.Month, Control: m.Control,
				Status: store.RunFailed, Reason: agent.ReasonCancelled, Failed: true,
			})
			continue
		}
		res := r.runMonth(ctx, suite.Name, m)
		file := m.FileName()
		if err := writeJSON(filepath.Join(dir, file), res); err != nil {
			res.Status = store.RunFailed
			res.Error = joinText(res.Error, err.Error())
			file = ""
		}
		fail := isFailed(res)
		if fail {
			failed = append(failed, m.Key())
		}
		man.Results = append(man.Results, ManifestEntry{
			Company: m.Company, Month: m.Month, Control: m.Control,
			File: file, RunID: res.RunID, Status: res.Status, Reason: res.Reason, Failed: fail,
		})
		r.log().InfoContext(ctx, "eval month ended", "company", m.Company, "month", m.Month,
			"status", res.Status, "findings", len(res.Findings), "failed", fail)
	}

	man.Failed = len(failed)
	man.FinishedAt = r.now()
	if err := writeJSON(filepath.Join(dir, ManifestFile), man); err != nil {
		return dir, man, err
	}
	if len(failed) > 0 {
		return dir, man, fmt.Errorf("%w: %d of %d: %v", ErrRunsFailed, len(failed), len(months), failed)
	}
	return dir, man, nil
}

// isFailed reports whether a month counts as failed: its run ended failed,
// or the close or the export returned an error. partial is not a failure.
func isFailed(res Result) bool {
	return res.Status == store.RunFailed || res.Status == "" || res.Error != ""
}

// runMonth runs one close and builds its result. It never returns an
// error: a failure is recorded in the result.
func (r *Runner) runMonth(ctx context.Context, suiteName string, m SuiteMonth) Result {
	res := Result{
		Suite: suiteName, Company: m.Company, Month: m.Month, Control: m.Control,
		Findings: []store.Finding{}, CostUSD: "0",
		Models: Models{Fast: r.Config.LLMModelFast, Strong: r.Config.LLMModelStrong, Called: []string{}},
		Commit: r.Commit,
	}
	start := r.now()
	out, err := r.Closer.RunClose(ctx, m.Company, m.Month)
	res.DurationMS = r.now().Sub(start).Milliseconds()
	res.Status, res.Reason, res.ReportPath = out.Status, out.Reason, out.ReportPath
	if err != nil {
		res.Error = err.Error()
		if res.Status == "" {
			res.Status = store.RunFailed
		}
	}
	if out.RunID == uuid.Nil {
		return res
	}
	id := out.RunID.String()
	res.RunID = &id

	// The export reads on a context that outlives a cancelled suite, so a
	// run cancelled by Ctrl-C still gets its result file.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), exportTimeout)
	defer cancel()
	if err := r.export(rctx, out.RunID, &res); err != nil {
		res.Error = joinText(res.Error, err.Error())
	}
	return res
}

// exportTimeout bounds the reads of one run's export.
const exportTimeout = 30 * time.Second

// export fills res from the stored run: its row, findings and model calls.
func (r *Runner) export(ctx context.Context, runID uuid.UUID, res *Result) error {
	run, err := r.Reader.GetCloseRun(ctx, runID)
	if err != nil {
		return fmt.Errorf("evals: export run %s: %w", runID, err)
	}
	res.Status = run.Status
	res.StartedAt, res.FinishedAt, res.TraceID = run.StartedAt, run.FinishedAt, run.TraceID
	if run.Error != nil && res.Reason == "" {
		res.Reason = *run.Error
	}

	findings, err := r.Reader.ListFindingsByRun(ctx, runID)
	if err != nil {
		return fmt.Errorf("evals: export run %s findings: %w", runID, err)
	}
	if findings != nil {
		res.Findings = findings
	}

	steps, err := r.Reader.ListSteps(ctx, runID)
	if err != nil {
		return fmt.Errorf("evals: export run %s steps: %w", runID, err)
	}
	var costs []string
	models := map[string]bool{}
	for _, st := range steps {
		calls, err := r.Reader.ListLLMCalls(ctx, st.ID)
		if err != nil {
			return fmt.Errorf("evals: export run %s step %s model calls: %w", runID, st.ID, err)
		}
		for _, c := range calls {
			res.Tokens.Input += c.InputTokens
			res.Tokens.Output += c.OutputTokens
			res.Tokens.CacheRead += c.CacheReadTokens
			costs = append(costs, c.CostUSD)
			if c.Model != "" {
				models[c.Model] = true
			}
		}
	}
	cost, err := sumDecimals(costs)
	if err != nil {
		return fmt.Errorf("evals: export run %s: %w", runID, err)
	}
	res.CostUSD = cost
	for m := range models {
		res.Models.Called = append(res.Models.Called, m)
	}
	slices.Sort(res.Models.Called)
	return nil
}

// joinText joins two error texts with "; ".
func joinText(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}
