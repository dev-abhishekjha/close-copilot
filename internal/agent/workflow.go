package agent

// The close workflow (CC-703). It is a workflow, not an agent: the steps
// are fixed, and only explaining and verifying (later investigating) call
// a model. One run moves through
//
//	queued -> preflight -> checking -> retrieving -> explaining ->
//	verifying -> investigating -> synthesizing -> done | partial | failed
//
// and each state is one or more run_steps rows (CC-709): router/close,
// check.<name>/<name>, retrieve/<finding id> (or one skipped
// retrieve/close), explain/<finding id>,
// verify/<finding id>, investigate/<finding id> with
// verify/investigate/<finding id> (or one skipped investigate/close) and
// synthesize/report. Every step
// is wrapped in store.Begin, so a resumed run skips the steps that are
// done and re-runs the rest. Steps after checking take their subjects from
// the stored findings, never from a re-run of the checks, so finding IDs
// stay stable across a resume.
//
// The workflow passes artifact addresses around and never decodes artifact
// content; the only content it reads back is its own report, to restore a
// missing report file. Nothing here logs prompts, tool results or
// narrations: only run and step identifiers, statuses and counts.
//
// Retrieving (CC-806) runs one retrieve step per finding, Parallel at a
// time, before any finding is explained: a query built from the finding
// (buildQuery) goes to the Retriever under the run's scope, and the top
// passages are stored as a retrieval artifact, the step's output. The
// explain step gets those refs as ExplainRequest.DocumentRefs on every
// attempt (a retry never searches again) and sends the passages as
// DOCUMENTS to the explainer, whose only tool is emit_explanation. A
// tainted finding's retrieve step is skipped with reasonTainted. A failed
// search fails only that step, with a fixed reason; the finding is still
// explained, with empty DOCUMENTS. With no Retriever, explainer or model,
// one retrieve/close step is recorded as skipped. Investigating (CC-706) runs per unmatched_bank_line finding,
// Parallel at a time, after every finding is explained and verified: an
// investigate step, whose model is pseudonymised (WrapRedacting outside
// the recording wrapper, so stored prompts hold tokens only), then a
// verify step with subject investigate/<finding id> that runs the same
// Verifier on the investigation's explanation artifact. A breach, a
// malformed answer or a failed verification marks the finding
// needs_review; none of them changes the run's outcome. With no
// Investigator, model or redactor, or no such finding, one investigate/close
// step is recorded as skipped. With no Explainer configured,
// the explain and verify steps are skipped and the run ends partial:
// findings saved, explanations missing.
//
// Prompt injection (CC-1103): after the checks, every finding that shares
// an invoice or bank_txn_id key, or an evidence record ID, with a
// suspicious_instruction_text finding (and that finding itself) is tainted
// (taintedFindings). A tainted finding
// is marked needs_review and its explain, verify and investigate steps are
// recorded as skipped with a fixed reason, so the record's text never
// reaches a model and no proposal is stored for it. It does not make the
// run partial. The set is rebuilt from the stored findings on every pass.
//
// Explaining and verifying run per finding, Parallel findings at a time,
// as a loop (CC-705): explain, then verify. A failed verdict reopens the
// explain step with the violations as feedback (store.ReopenStep) and ends
// the verify attempt failed with the verdict in its output refs, so both
// steps run again: the next explain attempt gets VERIFIER_FEEDBACK
// (attempt 3 uses the strong model), and the verify step restarts on a
// fresh attempt. After the third failed verification the finding is
// marked needs_review and the verify step ends done with the failing
// verdict and a reason naming the violation codes. A verify step that is
// done without a reason passed. The order of the writes (clear the
// finding's explanation, end the verify attempt failed with its verdict,
// then reopen the explain step) makes a crash anywhere in the loop resume
// correctly: before the reopen, the explain step is still done and the
// verification is simply re-run (it is deterministic); after it, the
// failed verdict is already in the verify step's output_refs.
//
// No attempt's output is lost: ReopenStep and every restart by Begin move
// a step's output refs to the end of its input refs, so a step's
// input_refs also holds, in order, the outputs of its earlier attempts:
// the rejected explanations on the explain step and the failed verdicts on
// the verify step. cmd/audit rebuilds a finding whose last explain retry
// ended without an explanation from the last of those verdicts.

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"golang.org/x/sync/errgroup"

	"github.com/abhishekjha/close-copilot/internal/checks"
	"github.com/abhishekjha/close-copilot/internal/company"
	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/llm"
	"github.com/abhishekjha/close-copilot/internal/mcpkit"
	"github.com/abhishekjha/close-copilot/internal/store"
	"github.com/abhishekjha/close-copilot/internal/telemetry"
)

// Budgets and limits of one run.
const (
	// DefaultRunTimeout is the deadline of one run. Workflow.Timeout may
	// shorten it, never lengthen it.
	DefaultRunTimeout = 10 * time.Minute
	// DefaultExplainParallel is how many findings are explained (and
	// verified) at once. 8 (CC-1104): 16 explain calls through claude-cli at
	// 8 at a time ended with no rate-limit or CLI error and no slower per
	// call than at 4, and a run's findings are explained in one wave
	// instead of two.
	DefaultExplainParallel = 8
	// DefaultRunTokenCap applies when the config carries no
	// LLM_RUN_TOKEN_CAP.
	DefaultRunTokenCap int64 = 200000
	// finalWriteTimeout bounds the writes that end a run, which run on a
	// fresh context so a cancelled run is still marked.
	finalWriteTimeout = 10 * time.Second
	// maxReasonLen caps a step's or a run's error text.
	maxReasonLen = 500
)

// Subjects of the steps that are not per check or per finding.
const (
	SubjectClose  = "close"
	SubjectReport = "report"
)

// Reasons recorded on a run or a step.
const (
	ReasonCancelled     = "cancelled"
	reasonNoExplainer   = "no explainer configured"
	reasonNoVerifier    = "no verifier configured"
	reasonNoExplanation = "no explanation to verify"
	// reasonRetrieve is the reason a pre-CC-806 run recorded on its
	// retrieve/close step; stored runs and report fixtures still hold it.
	reasonRetrieve    = "retrieval arrives with CC-806"
	reasonNoRetriever = "no retriever configured"
	// reasonNothingToRetrieve is the retrieve/close reason of a run with a
	// retriever but no finding.
	reasonNothingToRetrieve = "no findings to retrieve documents for"
	// reasonRetrieveFailed is a failed retrieve step's fixed reason: the
	// search's own error text is never recorded.
	reasonRetrieveFailed = "document search failed"
	// reasonInvestigate is the reason a pre-CC-706 run recorded on its
	// investigate/close step; stored runs and report fixtures still hold it.
	reasonInvestigate    = "investigation arrives with CC-706"
	reasonNoInvestigator = "no investigator configured"
	// Reasons of the investigate/close step and of an investigation's
	// verify step.
	reasonNothingToInvestigate = "no unmatched bank lines to investigate"
	reasonNoRedactor           = "no pseudonymiser configured; investigation refused"
	reasonNoResolution         = "no resolution to verify"
	// reasonTainted is the reason of every explain, verify and investigate
	// step skipped for a tainted finding (CC-1103).
	reasonTainted = "record contains instruction-like text; review by hand"
)

// investigateVerifyPrefix starts the subject of an investigation's verify
// step, so it never collides with the finding's explanation verify step.
const investigateVerifyPrefix = "investigate/"

// MaxExplainAttempts is how many explain attempts a finding gets before a
// failing verification marks it needs_review: the first and two retries.
const MaxExplainAttempts = 3

var (
	// ErrRunDone is returned by ResumeClose for a run that is already done.
	ErrRunDone = errors.New("agent: run is already done")
	// ErrPreflightRefused is wrapped when preflight refuses a run; the
	// *PreflightError carries the reason.
	ErrPreflightRefused = errors.New("agent: preflight refused the run")
	// ErrRunTokenCap is returned by a step's model before a call once the
	// run has used its token cap.
	ErrRunTokenCap = errors.New("agent: run token cap reached")
	// ErrUnknownCompany is returned for a company with no profile.
	ErrUnknownCompany = errors.New("agent: unknown company")
	// ErrRunFailed is wrapped when a run ends failed.
	ErrRunFailed = errors.New("agent: run failed")
)

// PreflightError is a run refused before any check, with the reason.
type PreflightError struct {
	Reason string
}

func (e *PreflightError) Error() string { return "agent: preflight refused the run: " + e.Reason }

// Is makes errors.Is(err, ErrPreflightRefused) true.
func (e *PreflightError) Is(target error) bool { return target == ErrPreflightRefused }

// StepResult is what a worker (explainer, verifier) returns for its step.
// The workflow finishes the step with it.
type StepResult struct {
	StepID uuid.UUID
	// Status is the step's terminal status: store.StepDone,
	// store.StepFailed or store.StepSkipped ("" means done).
	Status string
	// OutputRefs are the artifacts the step produced (sha256), such as an
	// explanation or a verdict. Each must be stored before it is returned.
	OutputRefs []string
	// Reason explains a failed or skipped step. It must not carry prompt,
	// response or evidence text.
	Reason string
	// NeedsReview is the verifier's hook: the explanation passed, but the
	// model itself flagged it for review, so the finding is marked
	// needs_review.
	NeedsReview bool
	// Violations is a failed verdict's VerifierFeedback JSON; non-empty
	// means the explanation failed verification. The workflow decides
	// whether to retry and stores it as the explain step's feedback.
	Violations json.RawMessage
}

// ExplainRequest is one finding to explain (CC-704).
type ExplainRequest struct {
	RunID   uuid.UUID
	StepID  uuid.UUID
	Attempt int
	Company string
	Month   string
	Finding store.Finding
	// EvidenceRefs are the addresses of the tool_result snapshots that
	// hold the finding's evidence, in the finding's evidence order.
	EvidenceRefs []string
	// Feedback is the step's stored verifier feedback, if any.
	Feedback []byte
	// DocumentRefs are the retrieval artifacts of the finding's retrieve
	// step, whose passages go in DOCUMENTS (CC-806). Empty means none.
	DocumentRefs []string
	// Model records every call against this step (RecordingProvider) and
	// refuses a call once the run's token cap is reached (ErrRunTokenCap).
	// It is nil when the workflow has no model.
	Model llm.Provider
}

// Explainer explains one finding and stores the explanation as an
// artifact. It retries internally when its own verification fails.
type Explainer interface {
	Explain(ctx context.Context, req ExplainRequest) (StepResult, error)
}

// VerifyRequest is one explanation to verify (CC-705).
type VerifyRequest struct {
	RunID        uuid.UUID
	StepID       uuid.UUID
	Attempt      int
	Company      string
	Month        string
	Finding      store.Finding
	EvidenceRefs []string
	// ExplanationRefs are the explain step's output artifacts.
	ExplanationRefs []string
	// ExplainAttempt is the attempt of the explain step that produced the
	// explanation; it is recorded in the verdict.
	ExplainAttempt int
	// Model is the step's provider. The CodeVerifier never uses it.
	Model llm.Provider
}

// Verifier traces an explanation's numbers back to the evidence and stores
// a verdict artifact.
type Verifier interface {
	Verify(ctx context.Context, req VerifyRequest) (StepResult, error)
}

// RunStore is what the workflow persists through. *store.Store
// implements it.
type RunStore interface {
	LLMCallRecorder
	CreateCloseRun(ctx context.Context, r store.CloseRun) error
	UpdateCloseRun(ctx context.Context, r store.CloseRun) error
	GetCloseRun(ctx context.Context, id uuid.UUID) (store.CloseRun, error)
	Begin(ctx context.Context, runID uuid.UUID, kind, subject string) (store.Step, bool, error)
	Finish(ctx context.Context, step store.Step, status string, outputRefs []string, errText string) error
	FinishWithFindings(ctx context.Context, step store.Step, outputRefs []string, findings []store.Finding) error
	ListSteps(ctx context.Context, runID uuid.UUID) ([]store.Step, error)
	ListFindingsByRun(ctx context.Context, runID uuid.UUID) ([]store.Finding, error)
	GetArtifact(ctx context.Context, sha string) (store.Artifact, error)
	SetRunState(ctx context.Context, runID uuid.UUID, status, errText string) error
	RollupRunUsage(ctx context.Context, runID uuid.UUID) (store.RunUsage, error)
	RunTokensUsed(ctx context.Context, runID uuid.UUID) (int64, error)
	LLMBudgetExhausted(ctx context.Context, cfg config.Config, now time.Time) (bool, error)
	SetFindingStatus(ctx context.Context, findingID uuid.UUID, status string) error
	SetFindingVerified(ctx context.Context, runID, findingID uuid.UUID, verified bool) error
	ClearFindingExplanation(ctx context.Context, runID, findingID uuid.UUID) error
	ReopenStep(ctx context.Context, step store.Step, feedback json.RawMessage) error
}

var _ RunStore = (*store.Store)(nil)

// Workflow runs closes. Store, Books, Evidence and Profiles are required;
// the rest have defaults.
type Workflow struct {
	Store RunStore
	// Books and Evidence are the MCP readers (MCPBooks, MCPEvidence) or
	// fakes. Every result is snapshotted per step.
	Books    checks.BooksReader
	Evidence checks.EvidenceReader
	// Profiles are the company profiles by ID; a run's company must be
	// one of them.
	Profiles map[string]company.Profile
	Rules    company.Rules
	// Checks run in order in the checking state; nil means bank
	// reconciliation only (Phase 1), which is all the recorded
	// suite-skeleton fixtures cover. cmd/agent sets DefaultChecks.
	Checks []checks.Check
	// Explainer and Verifier may be nil: their steps are then skipped and
	// the run ends partial.
	Explainer Explainer
	Verifier  Verifier
	// Investigator investigates unmatched_bank_line findings (CC-706); nil
	// records one skipped investigate/close step.
	Investigator Investigator
	// Retriever finds each finding's passages before it is explained
	// (CC-806). Nil records one skipped retrieve/close step. It is never
	// given to the investigator.
	Retriever Retriever
	// NewRedactor builds the run's Redactor (CC-710) for investigate
	// steps, whose model is WrapRedacting over the recorded provider. Nil
	// refuses investigation: identifiers never reach a tool-using model
	// unmasked.
	NewRedactor func(runID uuid.UUID) *Redactor
	// Model is the provider every explain and verify step's calls go
	// through, wrapped per step in a RecordingProvider.
	Model llm.Provider
	// Config supplies LLM_DAILY_BUDGET_USD and LLM_RUN_TOKEN_CAP.
	Config config.Config
	// ResultsDir holds runs/<run_id>.md ("" means "results").
	ResultsDir string
	// Timeout shortens the run's deadline below DefaultRunTimeout.
	Timeout time.Duration
	// Parallel is how many findings are explained at once (default 4).
	Parallel int
	Now      func() time.Time
	Log      *slog.Logger
}

// Result is how a run ended.
type Result struct {
	RunID      uuid.UUID
	Status     string // done, partial or failed
	Reason     string
	ReportPath string
	Findings   int
}

func (w *Workflow) log() *slog.Logger {
	if w.Log == nil {
		return slog.Default()
	}
	return w.Log
}

func (w *Workflow) now() time.Time {
	if w.Now == nil {
		return time.Now()
	}
	return w.Now()
}

func (w *Workflow) runTimeout() time.Duration {
	if w.Timeout > 0 && w.Timeout < DefaultRunTimeout {
		return w.Timeout
	}
	return DefaultRunTimeout
}

func (w *Workflow) tokenCap() int64 {
	if w.Config.LLMRunTokenCap > 0 {
		return w.Config.LLMRunTokenCap
	}
	return DefaultRunTokenCap
}

func (w *Workflow) parallel() int {
	if w.Parallel > 0 {
		return w.Parallel
	}
	return DefaultExplainParallel
}

// DefaultChecks are bank reconciliation and the Phase 2 detectors, in a
// fixed order; cmd/agent sets them as Workflow.Checks.
// Variance is last because runCheck links each variance finding to a
// finding stored by an earlier check (checks.LinkVariances), so every
// other check must have finished first.
func DefaultChecks() []checks.Check {
	return []checks.Check{
		&checks.BankRecCheck{},
		&checks.DuplicatePaymentCheck{},
		&checks.GSTR2BCheck{},
		&checks.MissingAccrualCheck{},
		&checks.PolicyRulesCheck{},
		&checks.SuspiciousTextCheck{},
		&checks.VarianceCheck{},
	}
}

func (w *Workflow) checks() []checks.Check {
	if w.Checks != nil {
		return w.Checks
	}
	return []checks.Check{&checks.BankRecCheck{}}
}

func (w *Workflow) validate() error {
	switch {
	case w.Store == nil:
		return errors.New("agent: workflow has no store")
	case w.Books == nil || w.Evidence == nil:
		return errors.New("agent: workflow has no books or evidence reader")
	case len(w.Profiles) == 0:
		return errors.New("agent: workflow has no company profiles")
	}
	return nil
}

// monthRange is the first and last day of a YYYY-MM month at UTC midnight.
func monthRange(month string) (time.Time, time.Time, error) {
	from, err := company.ParseMonth(month)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("agent: month: %w", err)
	}
	return from, from.AddDate(0, 1, -1), nil
}

// RunClose starts a new run for one company and month and drives it to
// done, partial or failed. A run refused at preflight ends failed with
// the reason, and the error wraps ErrPreflightRefused. A run that ends
// failed returns an error wrapping ErrRunFailed; partial is not an error.
func (w *Workflow) RunClose(ctx context.Context, companyID, month string) (Result, error) {
	if err := w.validate(); err != nil {
		return Result{}, err
	}
	if _, ok := w.Profiles[companyID]; !ok {
		return Result{}, fmt.Errorf("%w %.40q", ErrUnknownCompany, companyID)
	}
	if _, _, err := monthRange(month); err != nil {
		return Result{}, err
	}
	run := store.CloseRun{ID: uuid.New(), CompanyID: companyID, Month: month, Status: store.RunQueued}
	if err := w.Store.CreateCloseRun(ctx, run); err != nil {
		return Result{}, err
	}
	w.log().InfoContext(ctx, "close run queued", "run_id", run.ID, "company", companyID, "month", month)
	return w.drive(ctx, run)
}

// ResumeClose re-runs every step of an existing run that isn't done and
// finishes the run. It refuses a run that is already done (ErrRunDone).
func (w *Workflow) ResumeClose(ctx context.Context, runID uuid.UUID) (Result, error) {
	if err := w.validate(); err != nil {
		return Result{}, err
	}
	run, err := w.Store.GetCloseRun(ctx, runID)
	if err != nil {
		return Result{}, err
	}
	if run.Status == store.RunDone {
		return Result{RunID: runID, Status: run.Status}, fmt.Errorf("%w: %s", ErrRunDone, runID)
	}
	if _, ok := w.Profiles[run.CompanyID]; !ok {
		return Result{}, fmt.Errorf("%w %.40q", ErrUnknownCompany, run.CompanyID)
	}
	if _, _, err := monthRange(run.Month); err != nil {
		return Result{}, err
	}
	w.log().InfoContext(ctx, "close run resumed", "run_id", run.ID, "from_status", run.Status)
	return w.drive(ctx, run)
}

// outcome is how the stages ended when nothing failed.
type outcome struct {
	status     string // done or partial
	reason     string
	reportPath string
	findings   int
}

// drive runs the stages under the run's deadline and records the end.
func (w *Workflow) drive(parent context.Context, run store.CloseRun) (Result, error) {
	ctx, cancel := context.WithTimeout(parent, w.runTimeout())
	defer cancel()
	ctx, err := w.scopeRun(ctx, run)
	if err != nil {
		return w.end(parent, ctx, run, outcome{}, err)
	}

	// A resumed run is a new trace; its ID replaces the stored one. The
	// span holds the run ID and month only, never names or amounts.
	ctx, span := telemetry.Start(ctx, telemetry.SpanCloseRun,
		attribute.String("close_run.id", run.ID.String()), attribute.String("close_run.month", run.Month))
	defer span.End()
	if tid := telemetry.TraceID(ctx); tid != "" {
		// Re-read the row and change only the trace ID: the run snapshot
		// may be stale, and UpdateCloseRun writes every column.
		cur, err := w.Store.GetCloseRun(ctx, run.ID)
		if err == nil {
			cur.TraceID = &tid
			err = w.Store.UpdateCloseRun(ctx, cur)
		}
		if err != nil {
			w.log().WarnContext(ctx, "close run trace id not stored", "run_id", run.ID, "err", err)
		}
	}

	var out outcome
	err = w.Store.SetRunState(ctx, run.ID, store.RunRunning, "")
	if err == nil {
		out, err = w.stages(ctx, run)
	}
	res, err := w.end(parent, ctx, run, out, err)
	span.SetAttributes(attribute.String("close_run.status", res.Status))
	if res.Status == store.RunFailed {
		telemetry.Fail(span, "close run failed")
	}
	return res, err
}

// scopeRun puts the run's MCP scope (CC-506) on ctx: its company, the
// window of its month (mcpkit.Window), valid until a minute after the
// run's deadline. Every MCP call of the run sends it, and the servers
// refuse anything outside it. A resume mints a new scope for its new
// deadline. Without MCP_SCOPE_KEY (unscoped test servers) ctx is returned
// as it is.
func (w *Workflow) scopeRun(ctx context.Context, run store.CloseRun) (context.Context, error) {
	if w.Config.MCPScopeKey.IsZero() {
		return ctx, nil
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return ctx, errors.New("agent: run scope: the run has no deadline")
	}
	sc, err := mcpkit.Window(run.CompanyID, run.ID.String(), run.Month, deadline.Add(time.Minute))
	if err != nil {
		return ctx, fmt.Errorf("agent: run scope: %w", err)
	}
	header, err := mcpkit.Mint([]byte(w.Config.MCPScopeKey.Reveal()), sc)
	if err != nil {
		return ctx, fmt.Errorf("agent: run scope: %w", err)
	}
	return WithRunScope(ctx, header), nil
}

// end records the run's final state on a fresh context, so a cancelled or
// timed-out run is still marked. A stale step attempt means another
// attempt owns the run: the run is left alone.
func (w *Workflow) end(parent, ctx context.Context, run store.CloseRun, out outcome, err error) (Result, error) {
	res := Result{RunID: run.ID, ReportPath: out.reportPath, Findings: out.findings}
	if errors.Is(err, store.ErrStaleAttempt) {
		w.log().WarnContext(parent, "close run stopped: another attempt owns a step", "run_id", run.ID)
		return res, fmt.Errorf("agent: run %s: %w", run.ID, err)
	}

	status, reason := out.status, out.reason
	if err != nil {
		status, reason = store.RunFailed, failureReason(parent, ctx, err, w.runTimeout())
	}

	fctx, cancel := context.WithTimeout(context.WithoutCancel(parent), finalWriteTimeout)
	defer cancel()
	if _, uerr := w.Store.RollupRunUsage(fctx, run.ID); uerr != nil {
		w.log().ErrorContext(fctx, "close run usage rollup failed", "run_id", run.ID, "err", uerr)
	}
	if serr := w.Store.SetRunState(fctx, run.ID, status, reason); serr != nil {
		w.log().ErrorContext(fctx, "close run state write failed", "run_id", run.ID, "status", status, "err", serr)
		if err == nil {
			err = serr
		} else {
			err = errors.Join(err, serr)
		}
	}
	res.Status, res.Reason = status, reason
	w.log().InfoContext(fctx, "close run ended", "run_id", run.ID, "status", status, "findings", out.findings)
	if status == store.RunFailed {
		return res, fmt.Errorf("%w: %s: %s: %w", ErrRunFailed, run.ID, reason, err)
	}
	return res, err
}

// failureReason is the run's error text for err.
func failureReason(parent, ctx context.Context, err error, timeout time.Duration) string {
	var pre *PreflightError
	switch {
	case errors.Is(parent.Err(), context.Canceled):
		return ReasonCancelled
	case errors.Is(parent.Err(), context.DeadlineExceeded), errors.Is(ctx.Err(), context.DeadlineExceeded):
		return fmt.Sprintf("timeout: the run exceeded its %s deadline", timeout)
	case errors.As(err, &pre):
		return clip("preflight: " + pre.Reason)
	case errors.Is(err, store.ErrUnstorableContent):
		return clip("unstorable content: " + err.Error())
	}
	return clip(err.Error())
}

// clip caps a reason at maxReasonLen bytes, on a rune boundary.
func clip(s string) string {
	if len(s) <= maxReasonLen {
		return s
	}
	cut := maxReasonLen
	for cut > 0 && !isRuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// stages runs every state of the run in order.
func (w *Workflow) stages(ctx context.Context, run store.CloseRun) (outcome, error) {
	profile := w.Profiles[run.CompanyID]
	from, to, err := monthRange(run.Month)
	if err != nil {
		return outcome{}, err
	}

	if err := w.preflight(ctx, run, from, to); err != nil {
		return outcome{}, err
	}
	if err := w.progress(ctx, run.ID); err != nil {
		return outcome{}, err
	}

	// A check that errors fails its own step and the run goes on with the
	// others: the run ends partial and names it (CC-1101).
	var failedChecks []string
	for _, c := range w.checks() {
		if err := w.runCheck(ctx, run, profile, c); err != nil {
			var cf *checkFailure
			if errors.As(err, &cf) && !errors.Is(err, store.ErrStaleAttempt) {
				w.log().WarnContext(ctx, "check failed; the run continues without it", "run_id", run.ID, "check", cf.name, "err", cf.err)
				failedChecks = append(failedChecks, cf.Error())
				continue
			}
			return outcome{}, err
		}
	}
	if err := w.progress(ctx, run.ID); err != nil {
		return outcome{}, err
	}

	findings, err := w.Store.ListFindingsByRun(ctx, run.ID)
	if err != nil {
		return outcome{}, err
	}
	sortFindings(findings)

	tainted := taintedFindings(findings)
	docs, err := w.retrieveAll(ctx, run, findings, tainted)
	if err != nil {
		return outcome{}, err
	}

	explained, verified, err := w.explainAndVerifyAll(ctx, run, findings, tainted, docs)
	if err != nil {
		return outcome{}, err
	}
	if err := w.progress(ctx, run.ID); err != nil {
		return outcome{}, err
	}

	if err := w.investigateAll(ctx, run, findings, tainted); err != nil {
		return outcome{}, err
	}
	if err := w.progress(ctx, run.ID); err != nil {
		return outcome{}, err
	}

	out := outcome{status: store.RunDone, findings: len(findings)}
	var notes []string
	notes = append(notes, failedChecks...)
	// A tainted finding is left unexplained on purpose (needs_review), so
	// it doesn't make the run partial.
	if n := len(findings) - len(tainted) - explained; n > 0 {
		notes = append(notes, fmt.Sprintf("explanations missing for %d of %d findings", n, len(findings)))
	}
	if n := explained - verified; n > 0 {
		notes = append(notes, fmt.Sprintf("%d of %d explanations not verified", n, explained))
	}
	if len(notes) > 0 {
		out.status, out.reason = store.RunPartial, strings.Join(notes, "; ")
	}

	path, err := w.synthesize(ctx, run, out)
	if err != nil {
		return outcome{}, err
	}
	out.reportPath = path
	return out, nil
}

// sortFindings orders findings by type and keys, so every run of the same
// month lists them alike.
func sortFindings(fs []store.Finding) {
	slices.SortStableFunc(fs, func(a, b store.Finding) int {
		return cmp.Or(cmp.Compare(unlinkedKey(a), unlinkedKey(b)),
			cmp.Compare(checks.DedupeKey(a), checks.DedupeKey(b)), cmp.Compare(a.ID.String(), b.ID.String()))
	})
}

// unlinkedKey is f's dedupe key without keys.related, which holds a random
// finding ID (checks.LinkVariances), and without keys.resolution, which an
// investigation adds (CC-706), so report order depends on neither.
func unlinkedKey(f store.Finding) string {
	_, related := f.Keys[relatedKey]
	_, resolution := f.Keys[store.ResolutionKey]
	if !related && !resolution {
		return checks.DedupeKey(f)
	}
	keys := make(map[string]string, len(f.Keys))
	for k, v := range f.Keys {
		if k != relatedKey && k != store.ResolutionKey {
			keys[k] = v
		}
	}
	f.Keys = keys
	return checks.DedupeKey(f)
}

// relatedKey is the finding key checks.LinkVariances fills.
const relatedKey = "related"

// taintKeys are the record keys a suspicious_instruction_text finding
// carries (checks.SuspiciousTextCheck).
var taintKeys = []string{"invoice", "bank_txn_id"}

// taintedFindings is the detection layer's hand-off (CC-1103): every
// suspicious_instruction_text finding is tainted, and so is every other
// finding that names the same record, either by its invoice or
// bank_txn_id key or by an evidence ref of the same server and tool that
// shares a record ID with the suspicious finding's evidence (a variance or
// unmatched ledger entry citing an injected journal entry's GL lines). A
// tainted finding is never explained, verified or investigated, so the
// record's free text never reaches a model that could propose an entry;
// it is marked needs_review and keeps no proposal. The set is rebuilt from
// the run's stored findings on every pass, so a resumed run taints the
// same findings (CC-709).
func taintedFindings(fs []store.Finding) map[uuid.UUID]bool {
	type mark struct{ key, value string }
	type record struct{ server, tool, id string }
	marks := map[mark]bool{}
	records := map[record]bool{}
	out := map[uuid.UUID]bool{}
	for _, f := range fs {
		if f.Type != checks.TypeSuspiciousInstruction {
			continue
		}
		out[f.ID] = true
		for _, k := range taintKeys {
			if v := f.Keys[k]; v != "" {
				marks[mark{k, v}] = true
			}
		}
		for _, e := range f.Evidence {
			for _, id := range e.IDs {
				if id != "" {
					records[record{e.Server, e.Tool, id}] = true
				}
			}
		}
	}
	if len(out) == 0 {
		return out
	}
	for _, f := range fs {
		for _, k := range taintKeys {
			if v := f.Keys[k]; v != "" && marks[mark{k, v}] {
				out[f.ID] = true
			}
		}
		for _, e := range f.Evidence {
			for _, id := range e.IDs {
				if records[record{e.Server, e.Tool, id}] {
					out[f.ID] = true
				}
			}
		}
	}
	return out
}

// holdTainted handles a tainted finding in place of its explain and
// verify loop: it marks the finding needs_review, then records both steps
// as skipped with reasonTainted. Marked first, so a crash in between marks
// it again on resume; every write is idempotent.
func (w *Workflow) holdTainted(ctx context.Context, run store.CloseRun, f store.Finding) error {
	if err := w.Store.SetFindingStatus(ctx, f.ID, checks.StatusNeedsReview); err != nil {
		return err
	}
	w.log().WarnContext(ctx, "finding's record holds instruction-like text; not sent to a model", "run_id", run.ID, "finding_id", f.ID)
	if err := w.skipStep(ctx, run.ID, store.StepKindExplain, f.ID.String(), reasonTainted); err != nil {
		return err
	}
	return w.skipVerify(ctx, run, f, reasonTainted)
}

// progress writes the run's token and cost rollup after a state.
func (w *Workflow) progress(ctx context.Context, runID uuid.UUID) error {
	if _, err := w.Store.RollupRunUsage(ctx, runID); err != nil {
		return err
	}
	return nil
}

// begin starts a step; done reports that an earlier attempt finished it.
func (w *Workflow) begin(ctx context.Context, runID uuid.UUID, kind, subject string) (store.Step, bool, error) {
	if err := ctx.Err(); err != nil {
		return store.Step{}, false, err
	}
	st, done, err := w.Store.Begin(ctx, runID, kind, subject)
	if err != nil {
		return store.Step{}, false, err
	}
	if done {
		w.log().DebugContext(ctx, "step already done", "run_id", runID, "kind", kind, "subject", subject)
	} else {
		w.log().InfoContext(ctx, "step begun", "run_id", runID, "kind", kind, "subject", subject, "attempt", st.Attempt)
	}
	return st, done, nil
}

// finish ends a step. A stale attempt comes back as an error wrapping
// store.ErrStaleAttempt, which stops the run.
func (w *Workflow) finish(ctx context.Context, step store.Step, status string, refs []string, reason string) error {
	if err := w.Store.Finish(ctx, step, status, refs, clip(reason)); err != nil {
		return err
	}
	w.log().InfoContext(ctx, "step finished", "run_id", step.RunID, "kind", step.Kind, "subject", step.Subject, "status", status)
	return nil
}

// failStep marks a step failed on a fresh context, so a cancelled step is
// marked too, and returns cause (joined with a stale-attempt error).
func (w *Workflow) failStep(ctx context.Context, step store.Step, cause error) error {
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finalWriteTimeout)
	defer cancel()
	reason := cause.Error()
	if ctx.Err() != nil && errors.Is(cause, ctx.Err()) {
		reason = ReasonCancelled
	}
	if err := w.finish(fctx, step, store.StepFailed, nil, reason); err != nil {
		if errors.Is(err, store.ErrStaleAttempt) {
			return errors.Join(cause, err)
		}
		w.log().ErrorContext(fctx, "step failure write failed", "run_id", step.RunID, "kind", step.Kind, "err", err)
	}
	return cause
}

// skipStep records a step the run does not perform, unless it is done.
func (w *Workflow) skipStep(ctx context.Context, runID uuid.UUID, kind, subject, reason string) error {
	step, done, err := w.begin(ctx, runID, kind, subject)
	if err != nil || done {
		return err
	}
	return w.finish(ctx, step, store.StepSkipped, nil, reason)
}

// ---- preflight ----

// preflight is the router step for a close run. It refuses, with a
// reason, a month whose evidence isn't loaded, books that can't be
// reached, or a day whose LLM budget is spent.
func (w *Workflow) preflight(ctx context.Context, run store.CloseRun, from, to time.Time) error {
	step, done, err := w.begin(ctx, run.ID, store.StepKindRouter, SubjectClose)
	if err != nil || done {
		return err
	}
	snap := NewSnapshots(w.Store, run.ID, step.ID)
	reason, err := w.preflightReason(ctx, run, snap, from, to)
	if err != nil {
		return w.failStep(ctx, step, err)
	}
	if reason != "" {
		if err := w.finish(ctx, step, store.StepFailed, snap.Refs(), reason); err != nil {
			return err
		}
		w.log().WarnContext(ctx, "close run refused at preflight", "run_id", run.ID, "reason", reason)
		return &PreflightError{Reason: reason}
	}
	return w.finish(ctx, step, store.StepDone, snap.Refs(), "")
}

// preflightReason returns why the run must not start ("" for none). An
// error is a failure of the run itself (cancelled, unstorable content,
// the store), not a refusal.
func (w *Workflow) preflightReason(ctx context.Context, run store.CloseRun, snap *Snapshots, from, to time.Time) (string, error) {
	operational := func(err error) bool {
		return ctx.Err() != nil || errors.Is(err, store.ErrUnstorableContent)
	}
	lines, err := SnapshotEvidence(w.Evidence, snap).BankLines(ctx, run.CompanyID, from, to)
	if err != nil {
		if operational(err) {
			return "", err
		}
		return "evidence is unreachable: " + err.Error(), nil
	}
	if len(lines) == 0 {
		return fmt.Sprintf("evidence for %s %s is not loaded: no bank lines for the month", run.CompanyID, run.Month), nil
	}
	if _, err := SnapshotBooks(w.Books, snap).TrialBalance(ctx, run.CompanyID, from, to); err != nil {
		if operational(err) {
			return "", err
		}
		return "books are unreachable: " + err.Error(), nil
	}
	exhausted, err := w.Store.LLMBudgetExhausted(ctx, w.Config, w.now())
	if err != nil {
		return "", err
	}
	if exhausted {
		return "the daily LLM budget (" + config.EnvLLMDailyBudget + ") is exhausted", nil
	}
	return "", nil
}

// ---- checking ----

// runCheck runs one check as step check.<name>: over snapshotting
// readers, with each finding's evidence annotated with its snapshot, and
// its findings stored in the transaction that marks the step done.
func (w *Workflow) runCheck(ctx context.Context, run store.CloseRun, profile company.Profile, c checks.Check) error {
	name := c.Name()
	step, done, err := w.begin(ctx, run.ID, store.StepKindCheckPrefix+name, name)
	if err != nil || done {
		return err
	}
	ctx, span := telemetry.Start(ctx, telemetry.SpanCheckPrefix+name)
	defer span.End()
	snap := NewSnapshots(w.Store, run.ID, step.ID)
	in := checks.Inputs{
		Company:      run.CompanyID,
		Month:        run.Month,
		Books:        SnapshotBooks(w.Books, snap),
		Evidence:     SnapshotEvidence(w.Evidence, snap),
		Rules:        w.Rules,
		BankAccounts: checks.BankAccountsFor(profile),
	}
	fs, err := checks.NewRunner(nil).Run(ctx, run.ID, in, c)
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, store.ErrUnstorableContent) || errors.Is(err, store.ErrStaleAttempt) {
			return w.failStep(ctx, step, fmt.Errorf("agent: check %s: %w", name, err))
		}
		// The check could not finish: it stores no finding and the run
		// goes on without it.
		return w.failStep(ctx, step, &checkFailure{name: name, err: err})
	}
	if err := snap.Annotate(fs); err != nil {
		return w.failStep(ctx, step, err)
	}
	if name == checks.TypeVariance {
		if fs, err = w.linkVariances(ctx, run.ID, fs); err != nil {
			return w.failStep(ctx, step, err)
		}
	}
	if err := w.Store.FinishWithFindings(ctx, step, snap.Refs(), fs); err != nil {
		return err
	}
	w.log().InfoContext(ctx, "step finished", "run_id", run.ID, "kind", step.Kind, "subject", name, "status", store.StepDone, "findings", len(fs))
	return nil
}

// checkFailure is a check that errored for a reason other than the run's
// own cancellation or a store failure. Its text is the step's reason.
type checkFailure struct {
	name string
	err  error
}

func (e *checkFailure) Error() string { return fmt.Sprintf("check %s failed: %v", e.name, e.err) }
func (e *checkFailure) Unwrap() error { return e.err }

// linkVariances links the variance step's findings fs to the run's
// already-stored findings (and to each other's non-variance peers, should
// the step raise any) and returns only fs, linked. Stored findings are
// read, never rewritten.
func (w *Workflow) linkVariances(ctx context.Context, runID uuid.UUID, fs []store.Finding) ([]store.Finding, error) {
	if len(fs) == 0 {
		return fs, nil
	}
	stored, err := w.Store.ListFindingsByRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("agent: link variances: %w", err)
	}
	// Stored findings go first so a variance links to an earlier check's
	// finding in stored order; variances among them are left alone.
	all := make([]store.Finding, 0, len(stored)+len(fs))
	all = append(all, stored...)
	all = append(all, fs...)
	linked := checks.LinkVariances(all)
	return linked[len(stored):], nil
}

// ---- retrieving ----

// retrieveAll runs each finding's retrieve step, Parallel at a time, and
// returns the retrieval artifact refs per finding ID. A tainted finding's
// step is skipped and gets none. With no Retriever, explainer or model, or
// no finding, it records one skipped retrieve/close step instead.
func (w *Workflow) retrieveAll(ctx context.Context, run store.CloseRun, findings []store.Finding, tainted map[uuid.UUID]bool) (map[uuid.UUID][]string, error) {
	reason := ""
	switch {
	case w.Retriever == nil:
		reason = reasonNoRetriever
	case w.Explainer == nil:
		reason = reasonNoExplainer
	case w.Model == nil:
		reason = reasonExplainNoModel
	case len(findings) == 0:
		reason = reasonNothingToRetrieve
	}
	if reason != "" {
		return nil, w.skipStep(ctx, run.ID, store.StepKindRetrieve, SubjectClose, reason)
	}
	var mu sync.Mutex
	out := make(map[uuid.UUID][]string, len(findings))
	failed := 0
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(w.parallel())
	for _, f := range findings {
		g.Go(func() error {
			if tainted[f.ID] {
				return w.skipStep(gctx, run.ID, store.StepKindRetrieve, f.ID.String(), reasonTainted)
			}
			refs, ok, err := w.retrieveOne(gctx, run, f)
			if err != nil {
				return err
			}
			mu.Lock()
			defer mu.Unlock()
			if !ok {
				failed++
			}
			out[f.ID] = refs
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	if failed > 0 {
		// Counts only: never queries, passages or the search's error text.
		w.log().WarnContext(ctx, "document search failed; findings explained without documents", "run_id", run.ID, "failed", failed, "findings", len(findings))
	}
	return out, nil
}

// retrieveOne runs the finding's retrieve step unless it is done (a done
// step's refs are reused, so a resume never searches again). It reports
// whether the step ended done; a failed search fails the step with a fixed
// reason and is not an error.
func (w *Workflow) retrieveOne(ctx context.Context, run store.CloseRun, f store.Finding) ([]string, bool, error) {
	step, done, err := w.begin(ctx, run.ID, store.StepKindRetrieve, f.ID.String())
	if err != nil {
		return nil, false, err
	}
	if done {
		return step.OutputRefs, true, nil
	}
	query := buildQuery(f)
	passages, err := w.Retriever.Retrieve(ctx, RetrieveRequest{Company: run.CompanyID, Month: run.Month, Finding: f, Query: query})
	if err != nil {
		if workerFatal(ctx, err) {
			return nil, false, w.failStep(ctx, step, err)
		}
		if ferr := w.failStep(ctx, step, errors.New(reasonRetrieveFailed)); errors.Is(ferr, store.ErrStaleAttempt) {
			return nil, false, ferr
		}
		w.log().WarnContext(ctx, "retrieve step failed", "run_id", run.ID, "step_id", step.ID)
		return nil, false, nil
	}
	if len(passages) > maxPassages {
		passages = passages[:maxPassages]
	}
	art := RetrievalArtifact{Query: query, Passages: make([]Passage, 0, len(passages))}
	for _, p := range passages {
		// The hash is always of the stored text.
		art.Passages = append(art.Passages, newPassage(p.DocID, p.DocType, p.Section, p.Title, p.Text))
	}
	sha, err := w.Store.PutArtifact(ctx, store.ArtifactRetrieval, run.ID, step.ID, art)
	if err != nil {
		return nil, false, w.failStep(ctx, step, err)
	}
	refs := []string{sha}
	if err := w.finish(ctx, step, store.StepDone, refs, ""); err != nil {
		return nil, false, err
	}
	w.log().DebugContext(ctx, "documents retrieved", "run_id", run.ID, "step_id", step.ID, "passages", len(art.Passages))
	return refs, true, nil
}

// ---- explaining and verifying ----

// evidenceRefs are the distinct snapshot addresses of a finding's
// evidence, in order.
func evidenceRefs(f store.Finding) []string {
	out := make([]string, 0, len(f.Evidence))
	for _, e := range f.Evidence {
		if e.Artifact != "" && !slices.Contains(out, e.Artifact) {
			out = append(out, e.Artifact)
		}
	}
	return out
}

// workerFatal reports whether a worker's error stops the run rather than
// leaving one finding without its explanation or verdict.
func workerFatal(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, store.ErrStaleAttempt) ||
		errors.Is(err, store.ErrUnstorableContent) || errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded)
}

// finishWorker ends a worker's step with its result. It returns whether
// the step ended done.
func (w *Workflow) finishWorker(ctx context.Context, step store.Step, res StepResult) (bool, error) {
	status := res.Status
	if status == "" {
		status = store.StepDone
	}
	if status != store.StepDone && status != store.StepFailed && status != store.StepSkipped {
		return false, w.failStep(ctx, step, fmt.Errorf("agent: %s worker returned status %.20q", step.Kind, status))
	}
	if res.StepID != uuid.Nil && res.StepID != step.ID {
		return false, w.failStep(ctx, step, fmt.Errorf("agent: %s worker answered for another step", step.Kind))
	}
	if err := w.finish(ctx, step, status, res.OutputRefs, res.Reason); err != nil {
		return false, err
	}
	return status == store.StepDone, nil
}

// explainAndVerifyAll runs each finding's explain and verify loop,
// Parallel findings at a time; a tainted finding is held instead
// (holdTainted). It returns how many findings have an explanation and how
// many of those passed verification.
func (w *Workflow) explainAndVerifyAll(ctx context.Context, run store.CloseRun, findings []store.Finding, tainted map[uuid.UUID]bool, docs map[uuid.UUID][]string) (int, int, error) {
	var mu sync.Mutex
	explained, verified := 0, 0
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(w.parallel())
	for _, f := range findings {
		g.Go(func() error {
			if tainted[f.ID] {
				return w.holdTainted(gctx, run, f)
			}
			hasExplanation, passed, err := w.explainAndVerify(gctx, run, f, docs[f.ID])
			if err != nil {
				return err
			}
			mu.Lock()
			defer mu.Unlock()
			if hasExplanation {
				explained++
			}
			if passed {
				verified++
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return 0, 0, err
	}
	return explained, verified, nil
}

// explainAndVerify is one finding's loop: explain, verify, and on a failed
// verdict reopen both steps and go again, up to MaxExplainAttempts explain
// attempts. It reports whether the finding has an explanation and whether
// it passed verification.
func (w *Workflow) explainAndVerify(ctx context.Context, run store.CloseRun, f store.Finding, docRefs []string) (bool, bool, error) {
	for {
		explainStep, refs, ok, err := w.explainOne(ctx, run, f, docRefs)
		if err != nil {
			return false, false, err
		}
		if !ok {
			// No explanation: the verify step has nothing to check. When an
			// earlier attempt failed verification (the step carries
			// feedback), the finding needs review; its rejected explanation
			// was cleared before the reopen. Marked before the verify step
			// is skipped, so a crash in between marks it on resume. The
			// skip restarts the verify step (Begin), which keeps the last
			// failed verdict in its input_refs for cmd/audit.
			if len(bytes.TrimSpace(explainStep.Feedback)) > 0 {
				if err := w.Store.SetFindingStatus(ctx, f.ID, checks.StatusNeedsReview); err != nil {
					return false, false, err
				}
				w.log().WarnContext(ctx, "explanation retry ended without an explanation; finding needs review", "run_id", run.ID, "finding_id", f.ID, "explain_attempt", explainStep.Attempt)
			}
			return false, false, w.skipVerify(ctx, run, f, reasonNoExplanation)
		}
		again, passed, err := w.verifyOne(ctx, run, f, explainStep, refs)
		if err != nil || !again {
			return true, passed, err
		}
	}
}

// explainOne runs the finding's explain step unless it is done. It returns
// the step (for its attempt) and its output refs when it ends done.
// docRefs are the finding's retrieval artifacts, the same on every attempt.
func (w *Workflow) explainOne(ctx context.Context, run store.CloseRun, f store.Finding, docRefs []string) (store.Step, []string, bool, error) {
	step, done, err := w.begin(ctx, run.ID, store.StepKindExplain, f.ID.String())
	if err != nil {
		return store.Step{}, nil, false, err
	}
	if done {
		return step, step.OutputRefs, true, nil
	}
	if w.Explainer == nil {
		return step, nil, false, w.finish(ctx, step, store.StepSkipped, nil, reasonNoExplainer)
	}
	if reason, err := w.overTokenCap(ctx, run.ID); err != nil || reason != "" {
		if err != nil {
			return step, nil, false, w.failStep(ctx, step, err)
		}
		return step, nil, false, w.finish(ctx, step, store.StepSkipped, nil, reason)
	}
	ectx, span := telemetry.Start(ctx, telemetry.SpanExplain)
	res, err := w.Explainer.Explain(ectx, ExplainRequest{
		RunID: run.ID, StepID: step.ID, Attempt: step.Attempt,
		Company: run.CompanyID, Month: run.Month,
		Finding: f, EvidenceRefs: evidenceRefs(f), Feedback: step.Feedback,
		DocumentRefs: slices.Clone(docRefs),
		Model:        w.stepModel(run.ID, step.ID),
	})
	if err != nil {
		telemetry.Fail(span, "explain failed")
	}
	span.End()
	if err != nil {
		return step, nil, false, w.workerError(ctx, step, err)
	}
	ok, err := w.finishWorker(ctx, step, res)
	if err != nil || !ok {
		return step, nil, false, err
	}
	return step, res.OutputRefs, true, nil
}

// workerError ends a worker's step after an error: skipped at the token
// cap, failed otherwise. Only a fatal error (workerFatal) is returned.
func (w *Workflow) workerError(ctx context.Context, step store.Step, err error) error {
	if errors.Is(err, ErrRunTokenCap) && !workerFatal(ctx, err) {
		return w.finish(ctx, step, store.StepSkipped, nil, "run token cap reached")
	}
	if workerFatal(ctx, err) {
		return w.failStep(ctx, step, err)
	}
	// One finding without an explanation or verdict: the run goes on and
	// ends partial.
	if ferr := w.failStep(ctx, step, err); errors.Is(ferr, store.ErrStaleAttempt) {
		return ferr
	}
	return nil
}

// skipVerify records the finding's verify step as skipped, unless it is
// done.
func (w *Workflow) skipVerify(ctx context.Context, run store.CloseRun, f store.Finding, reason string) error {
	return w.skipStep(ctx, run.ID, store.StepKindVerify, f.ID.String(), reason)
}

// verifyOne runs the finding's verify step against the explain step's
// output. It returns again=true when the verdict failed and the explain
// step was reopened for another attempt, and passed=true when the verdict
// passed. A verify step that is already done is not re-run: done without
// a reason passed, done with one is a final failure.
func (w *Workflow) verifyOne(ctx context.Context, run store.CloseRun, f store.Finding, explainStep store.Step, explanation []string) (again, passed bool, err error) {
	step, done, err := w.begin(ctx, run.ID, store.StepKindVerify, f.ID.String())
	if err != nil {
		return false, false, err
	}
	if done {
		return false, step.Error == nil, nil
	}
	if w.Verifier == nil {
		return false, false, w.finish(ctx, step, store.StepSkipped, nil, reasonNoVerifier)
	}
	vctx, span := telemetry.Start(ctx, telemetry.SpanVerify)
	res, err := w.Verifier.Verify(vctx, VerifyRequest{
		RunID: run.ID, StepID: step.ID, Attempt: step.Attempt,
		Company: run.CompanyID, Month: run.Month,
		Finding: f, EvidenceRefs: evidenceRefs(f), ExplanationRefs: explanation,
		ExplainAttempt: explainStep.Attempt,
		Model:          w.stepModel(run.ID, step.ID),
	})
	if err != nil {
		telemetry.Fail(span, "verify failed")
	}
	span.End()
	if err != nil {
		return false, false, w.workerError(ctx, step, err)
	}
	if res.StepID != uuid.Nil && res.StepID != step.ID {
		return false, false, w.failStep(ctx, step, fmt.Errorf("agent: %s worker answered for another step", step.Kind))
	}
	if res.Status != "" && res.Status != store.StepDone {
		_, err := w.finishWorker(ctx, step, res)
		return false, false, err
	}

	if len(bytes.TrimSpace(res.Violations)) == 0 {
		// Passed. Marked before the step is done, so a crash in between
		// re-runs the (idempotent) verification rather than losing a mark.
		if err := w.Store.SetFindingVerified(ctx, run.ID, f.ID, true); err != nil {
			return false, false, w.failStep(ctx, step, err)
		}
		if res.NeedsReview {
			if err := w.Store.SetFindingStatus(ctx, f.ID, checks.StatusNeedsReview); err != nil {
				return false, false, w.failStep(ctx, step, err)
			}
		}
		res.Reason = ""
		ok, err := w.finishWorker(ctx, step, res)
		return false, ok, err
	}

	reason := cmp.Or(res.Reason, "verification failed")
	if explainStep.Attempt < MaxExplainAttempts {
		// Retry: clear the rejected explanation and proposal from the
		// finding, end this verify attempt failed (restartable) with its
		// verdict in output_refs, then reopen the explain step with the
		// violations. A crash after the clear or after the finish re-runs
		// this verification (the explain step is still done), which fails
		// the same way and goes on from the clear; a crash after the
		// reopen re-explains with the feedback, and the failed verdict is
		// already on the verify step, so the next Begin or skip keeps it in
		// input_refs for cmd/audit. Either way the finding never shows a
		// rejected explanation.
		if err := w.Store.ClearFindingExplanation(ctx, run.ID, f.ID); err != nil {
			return false, false, w.failStep(ctx, step, err)
		}
		if err := w.finish(ctx, step, store.StepFailed, res.OutputRefs, fmt.Sprintf("%s (explain attempt %d); retrying", reason, explainStep.Attempt)); err != nil {
			return false, false, err
		}
		if err := w.Store.ReopenStep(ctx, explainStep, res.Violations); err != nil {
			// The verify step already ended failed with its verdict, so
			// it is not failed again: a resume re-runs it.
			return false, false, err
		}
		w.log().InfoContext(ctx, "explanation failed verification; retrying", "run_id", run.ID, "finding_id", f.ID, "explain_attempt", explainStep.Attempt)
		return true, false, nil
	}

	// The last attempt failed: needs_review, then the step ends done with
	// the failing verdict.
	if err := w.Store.SetFindingStatus(ctx, f.ID, checks.StatusNeedsReview); err != nil {
		return false, false, w.failStep(ctx, step, err)
	}
	w.log().WarnContext(ctx, "explanation failed verification on its last attempt; finding needs review", "run_id", run.ID, "finding_id", f.ID, "explain_attempt", explainStep.Attempt)
	res.Reason = fmt.Sprintf("%s after %d explain attempts", reason, explainStep.Attempt)
	_, err = w.finishWorker(ctx, step, res)
	return false, false, err
}

// ---- investigating ----

// investigateAll investigates each unmatched_bank_line finding, Parallel at
// a time: an investigate step, then a verify step. A tainted one is never
// investigated: its investigate step is recorded as skipped with
// reasonTainted (holdTainted already marked it needs_review). With nothing
// else to investigate, or no investigator, model or redactor, it records
// one skipped investigate/close step instead.
func (w *Workflow) investigateAll(ctx context.Context, run store.CloseRun, findings []store.Finding, tainted map[uuid.UUID]bool) error {
	var targets []store.Finding
	held := 0
	for _, f := range findings {
		if f.Type != checks.TypeUnmatchedBankLine {
			continue
		}
		if tainted[f.ID] {
			held++
			if err := w.skipStep(ctx, run.ID, store.StepKindInvestigate, f.ID.String(), reasonTainted); err != nil {
				return err
			}
			continue
		}
		targets = append(targets, f)
	}
	reason := ""
	switch {
	case len(targets) == 0 && held > 0:
		return nil
	case len(targets) == 0:
		reason = reasonNothingToInvestigate
	case w.Investigator == nil:
		reason = reasonNoInvestigator
	case w.Model == nil:
		reason = reasonInvestigateNoModel
	case w.NewRedactor == nil:
		reason = reasonNoRedactor
	}
	if reason != "" {
		return w.skipStep(ctx, run.ID, store.StepKindInvestigate, SubjectClose, reason)
	}
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(w.parallel())
	for _, f := range targets {
		g.Go(func() error { return w.investigateOne(gctx, run, f) })
	}
	return g.Wait()
}

// investigateOne runs the finding's investigate step unless it is done,
// then verifies its answer. A step that ended done without a reason has
// one output: the investigation's explanation artifact. One that ended
// with a reason (a breach) or failed has none to verify, and the finding
// was marked needs_review before the step ended, so a crash in between
// marks it on resume.
func (w *Workflow) investigateOne(ctx context.Context, run store.CloseRun, f store.Finding) error {
	step, done, err := w.begin(ctx, run.ID, store.StepKindInvestigate, f.ID.String())
	if err != nil {
		return err
	}
	answered := done && step.Error == nil && len(step.OutputRefs) == 1
	if !done {
		if answered, err = w.runInvestigation(ctx, run, f, &step); err != nil {
			return err
		}
	}
	subject := investigateVerifyPrefix + f.ID.String()
	if !answered {
		return w.skipStep(ctx, run.ID, store.StepKindVerify, subject, reasonNoResolution)
	}
	return w.verifyInvestigation(ctx, run, f.ID, step, subject)
}

// runInvestigation runs the investigator for a begun step and ends the
// step. It reports whether the step ended done with an answer to verify.
func (w *Workflow) runInvestigation(ctx context.Context, run store.CloseRun, f store.Finding, step *store.Step) (bool, error) {
	if reason, err := w.overTokenCap(ctx, run.ID); err != nil || reason != "" {
		if err != nil {
			return false, w.failStep(ctx, *step, err)
		}
		return false, w.finish(ctx, *step, store.StepSkipped, nil, reason)
	}
	model, err := w.investigateModel(run.ID, step.ID)
	if err != nil {
		return false, w.finish(ctx, *step, store.StepSkipped, nil, reasonNoRedactor)
	}
	ictx, span := telemetry.Start(ctx, telemetry.SpanInvestigate)
	res, err := w.Investigator.Investigate(ictx, InvestigateRequest{
		RunID: run.ID, StepID: step.ID, Attempt: step.Attempt,
		Company: run.CompanyID, Month: run.Month,
		Finding: f, EvidenceRefs: evidenceRefs(f), Model: model,
	})
	if err != nil {
		telemetry.Fail(span, "investigate failed")
	}
	span.End()
	if err != nil {
		return false, w.workerError(ctx, *step, err)
	}
	if res.NeedsReview {
		if err := w.Store.SetFindingStatus(ctx, f.ID, checks.StatusNeedsReview); err != nil {
			return false, w.failStep(ctx, *step, err)
		}
	}
	ok, err := w.finishWorker(ctx, *step, res)
	if err != nil || !ok {
		return false, err
	}
	step.OutputRefs = res.OutputRefs
	return res.Reason == "" && len(res.OutputRefs) == 1, nil
}

// verifyInvestigation runs the investigation's verify step: the Verifier
// on the investigate step's explanation artifact, against the finding's
// evidence as stored now (the check's snapshots plus the investigation's
// trail). A failed verdict marks the finding needs_review and ends the
// step done with the verdict and a reason; investigations are not retried.
func (w *Workflow) verifyInvestigation(ctx context.Context, run store.CloseRun, findingID uuid.UUID, investigateStep store.Step, subject string) error {
	step, done, err := w.begin(ctx, run.ID, store.StepKindVerify, subject)
	if err != nil || done {
		return err
	}
	if w.Verifier == nil {
		return w.finish(ctx, step, store.StepSkipped, nil, reasonNoVerifier)
	}
	fs, err := w.Store.ListFindingsByRun(ctx, run.ID)
	if err != nil {
		return w.failStep(ctx, step, err)
	}
	i := slices.IndexFunc(fs, func(f store.Finding) bool { return f.ID == findingID })
	if i < 0 {
		return w.failStep(ctx, step, fmt.Errorf("agent: verify investigation: %w: finding %s", store.ErrNotFound, findingID))
	}
	f := fs[i]
	vctx, span := telemetry.Start(ctx, telemetry.SpanVerify)
	res, err := w.Verifier.Verify(vctx, VerifyRequest{
		RunID: run.ID, StepID: step.ID, Attempt: step.Attempt,
		Company: run.CompanyID, Month: run.Month,
		Finding: f, EvidenceRefs: evidenceRefs(f), ExplanationRefs: investigateStep.OutputRefs,
		ExplainAttempt: investigateStep.Attempt,
		Model:          w.stepModel(run.ID, step.ID),
	})
	if err != nil {
		telemetry.Fail(span, "verify failed")
	}
	span.End()
	if err != nil {
		return w.workerError(ctx, step, err)
	}
	if res.StepID != uuid.Nil && res.StepID != step.ID {
		return w.failStep(ctx, step, fmt.Errorf("agent: %s worker answered for another step", step.Kind))
	}
	if res.Status != "" && res.Status != store.StepDone {
		_, err := w.finishWorker(ctx, step, res)
		return err
	}
	failed := len(bytes.TrimSpace(res.Violations)) > 0
	if failed || res.NeedsReview {
		if err := w.Store.SetFindingStatus(ctx, f.ID, checks.StatusNeedsReview); err != nil {
			return w.failStep(ctx, step, err)
		}
	}
	if failed {
		res.Reason = cmp.Or(res.Reason, "verification failed")
		w.log().WarnContext(ctx, "investigation failed verification; finding needs review", "run_id", run.ID, "finding_id", f.ID)
	} else {
		res.Reason = ""
	}
	_, err = w.finishWorker(ctx, step, res)
	return err
}

// investigateModel is an investigate step's provider: the step's recorded,
// capped provider wrapped by WrapRedacting, so identifiers are masked
// before the call and before the recording wrapper stores the prompt. It
// is nil when the workflow has no model, and an error without a redactor.
func (w *Workflow) investigateModel(runID, stepID uuid.UUID) (llm.Provider, error) {
	base := w.stepModel(runID, stepID)
	if base == nil {
		return nil, nil
	}
	if w.NewRedactor == nil {
		return nil, errors.New("agent: investigation needs a redactor")
	}
	r := w.NewRedactor(runID)
	if r == nil {
		return nil, errors.New("agent: investigation needs a redactor")
	}
	return WrapRedacting(base, r), nil
}

// ---- model budget ----

// overTokenCap returns a reason when the run has used its token cap.
func (w *Workflow) overTokenCap(ctx context.Context, runID uuid.UUID) (string, error) {
	used, err := w.Store.RunTokensUsed(ctx, runID)
	if err != nil {
		return "", err
	}
	if limit := w.tokenCap(); used >= limit {
		return fmt.Sprintf("run token cap reached: %d of %d tokens used", used, limit), nil
	}
	return "", nil
}

// stepModel is the provider a worker's step calls: every call recorded
// against the step, refused once the run reaches its token cap. It is nil
// when the workflow has no model.
func (w *Workflow) stepModel(runID, stepID uuid.UUID) llm.Provider {
	if w.Model == nil {
		return nil
	}
	rec := costCheckedRecorder{LLMCallRecorder: w.Store}
	return &cappedProvider{
		inner:  NewRecordingProvider(w.Model, rec, runID, stepID),
		tokens: w.Store,
		runID:  runID,
		limit:  w.tokenCap(),
	}
}

// tokenCounter reports a run's tokens so far. *store.Store implements it.
type tokenCounter interface {
	RunTokensUsed(ctx context.Context, runID uuid.UUID) (int64, error)
}

// cappedProvider refuses a call once the run's recorded tokens reach the
// cap. The check runs before each call, from the run's llm_calls.
type cappedProvider struct {
	inner  llm.Provider
	tokens tokenCounter
	runID  uuid.UUID
	limit  int64
}

func (p *cappedProvider) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	used, err := p.tokens.RunTokensUsed(ctx, p.runID)
	if err != nil {
		return llm.Response{}, fmt.Errorf("agent: run token count: %w", err)
	}
	if used >= p.limit {
		return llm.Response{}, fmt.Errorf("%w: %d of %d tokens used", ErrRunTokenCap, used, p.limit)
	}
	return p.inner.Complete(ctx, req)
}

// costCheckedRecorder rejects a call cost that llm_calls can't hold
// (negative, or 10000 USD or more) before the insert.
type costCheckedRecorder struct {
	LLMCallRecorder
}

func (r costCheckedRecorder) InsertLLMCall(ctx context.Context, c store.LLMCall) (uuid.UUID, error) {
	if err := store.ValidateCallCost(c.CostUSD); err != nil {
		return uuid.Nil, fmt.Errorf("agent: record llm call: %w", err)
	}
	return r.LLMCallRecorder.InsertLLMCall(ctx, c)
}

// ---- synthesizing ----

// synthesize renders the report, stores it as a report artifact and
// writes results/runs/<run_id>.md. When an earlier attempt finished the
// step, it only restores the report file if it is missing.
func (w *Workflow) synthesize(ctx context.Context, run store.CloseRun, out outcome) (string, error) {
	step, done, err := w.begin(ctx, run.ID, store.StepKindSynthesize, SubjectReport)
	if err != nil {
		return "", err
	}
	if done {
		return restoreReport(ctx, w.Store, w.ResultsDir, run.ID, step.OutputRefs)
	}
	findings, err := w.Store.ListFindingsByRun(ctx, run.ID)
	if err != nil {
		return "", w.failStep(ctx, step, err)
	}
	sortFindings(findings)
	steps, err := w.Store.ListSteps(ctx, run.ID)
	if err != nil {
		return "", w.failStep(ctx, step, err)
	}
	usage, err := w.Store.RollupRunUsage(ctx, run.ID)
	if err != nil {
		return "", w.failStep(ctx, step, err)
	}
	fault, err := storedFault(ctx, w.Store, steps)
	if err != nil {
		return "", w.failStep(ctx, step, err)
	}
	// The trace ID goes into the stored report artifact as well as the file:
	// it differs per run (empty with tracing off), so a report is not
	// byte-identical across two traced runs of the same month.
	md := RenderReport(ReportData{
		RunID: run.ID, Company: run.CompanyID, Month: run.Month,
		Outcome: out.status, Reason: out.reason, Usage: usage,
		Findings: findings, Steps: steps, Fault: fault, TraceID: telemetry.TraceID(ctx),
	})
	sha, err := w.Store.PutArtifact(ctx, store.ArtifactReport, run.ID, step.ID, ReportArtifact{RunID: run.ID.String(), Markdown: md})
	if err != nil {
		return "", w.failStep(ctx, step, err)
	}
	path, err := writeReport(w.ResultsDir, run.ID, md)
	if err != nil {
		return "", w.failStep(ctx, step, err)
	}
	if err := w.finish(ctx, step, store.StepDone, []string{sha}, ""); err != nil {
		return "", err
	}
	return path, nil
}

// storedFault returns the COPILOT_FAULT mode the run's stored explanations
// show ("" for none): config.FaultCorruptExplanation when any explanation
// artifact the run's explain steps point to (output refs, and the input
// refs a reopen keeps from earlier attempts) is marked fault_injected. It
// reads stored data only, never this process's setting.
func storedFault(ctx context.Context, st ArtifactGetter, steps []store.Step) (string, error) {
	for _, s := range steps {
		if s.Kind != store.StepKindExplain {
			continue
		}
		for _, ref := range slices.Concat(s.InputRefs, s.OutputRefs) {
			a, err := st.GetArtifact(ctx, ref)
			if err != nil {
				return "", fmt.Errorf("agent: report fault: %w", err)
			}
			if a.Kind != store.ArtifactExplanation {
				continue
			}
			var art struct {
				FaultInjected bool `json:"fault_injected"`
			}
			if err := json.Unmarshal(a.Content, &art); err != nil {
				return "", fmt.Errorf("agent: report fault: decode explanation %s: %w", shortSHA(ref), err)
			}
			if art.FaultInjected {
				return config.FaultCorruptExplanation, nil
			}
		}
	}
	return "", nil
}
