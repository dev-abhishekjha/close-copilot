package agent

// The close workflow (CC-703). It is a workflow, not an agent: the steps
// are fixed, and only explaining and verifying (later investigating) call
// a model. One run moves through
//
//	queued -> preflight -> checking -> retrieving -> explaining ->
//	verifying -> investigating -> synthesizing -> done | partial | failed
//
// and each state is one or more run_steps rows (CC-709): router/close,
// check.<name>/<name>, retrieve/close, explain/<finding id>,
// verify/<finding id>, investigate/close and synthesize/report. Every step
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
// Phase 1 runs preflight, the bank reconciliation check, explaining,
// verifying and synthesizing; retrieving (CC-806) and investigating
// (CC-706) are recorded as skipped steps. With no Explainer configured,
// the explain and verify steps are skipped and the run ends partial:
// findings saved, explanations missing.
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
	"golang.org/x/sync/errgroup"

	"github.com/abhishekjha/close-copilot/internal/checks"
	"github.com/abhishekjha/close-copilot/internal/company"
	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/llm"
	"github.com/abhishekjha/close-copilot/internal/store"
)

// Budgets and limits of one run.
const (
	// DefaultRunTimeout is the deadline of one run. Workflow.Timeout may
	// shorten it, never lengthen it.
	DefaultRunTimeout = 10 * time.Minute
	// DefaultExplainParallel is how many findings are explained (and
	// verified) at once.
	DefaultExplainParallel = 4
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
	reasonRetrieve      = "retrieval arrives with CC-806"
	reasonInvestigate   = "investigation arrives with CC-706"
)

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
	// reconciliation only (Phase 1).
	Checks []checks.Check
	// Explainer and Verifier may be nil: their steps are then skipped and
	// the run ends partial.
	Explainer Explainer
	Verifier  Verifier
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

	var out outcome
	err := w.Store.SetRunState(ctx, run.ID, store.RunRunning, "")
	if err == nil {
		out, err = w.stages(ctx, run)
	}
	return w.end(parent, ctx, run, out, err)
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

	for _, c := range w.checks() {
		if err := w.runCheck(ctx, run, profile, c); err != nil {
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

	if err := w.skipStep(ctx, run.ID, store.StepKindRetrieve, SubjectClose, reasonRetrieve); err != nil {
		return outcome{}, err
	}

	explained, verified, err := w.explainAndVerifyAll(ctx, run, findings)
	if err != nil {
		return outcome{}, err
	}
	if err := w.progress(ctx, run.ID); err != nil {
		return outcome{}, err
	}

	if err := w.skipStep(ctx, run.ID, store.StepKindInvestigate, SubjectClose, reasonInvestigate); err != nil {
		return outcome{}, err
	}

	out := outcome{status: store.RunDone, findings: len(findings)}
	var notes []string
	if n := len(findings) - explained; n > 0 {
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
		return cmp.Or(cmp.Compare(checks.DedupeKey(a), checks.DedupeKey(b)), cmp.Compare(a.ID.String(), b.ID.String()))
	})
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
		return w.failStep(ctx, step, fmt.Errorf("agent: check %s: %w", name, err))
	}
	if err := snap.Annotate(fs); err != nil {
		return w.failStep(ctx, step, err)
	}
	if err := w.Store.FinishWithFindings(ctx, step, snap.Refs(), fs); err != nil {
		return err
	}
	w.log().InfoContext(ctx, "step finished", "run_id", run.ID, "kind", step.Kind, "subject", name, "status", store.StepDone, "findings", len(fs))
	return nil
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
// Parallel findings at a time. It returns how many findings have an
// explanation and how many of those passed verification.
func (w *Workflow) explainAndVerifyAll(ctx context.Context, run store.CloseRun, findings []store.Finding) (int, int, error) {
	var mu sync.Mutex
	explained, verified := 0, 0
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(w.parallel())
	for _, f := range findings {
		g.Go(func() error {
			hasExplanation, passed, err := w.explainAndVerify(gctx, run, f)
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
func (w *Workflow) explainAndVerify(ctx context.Context, run store.CloseRun, f store.Finding) (bool, bool, error) {
	for {
		explainStep, refs, ok, err := w.explainOne(ctx, run, f)
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
func (w *Workflow) explainOne(ctx context.Context, run store.CloseRun, f store.Finding) (store.Step, []string, bool, error) {
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
	res, err := w.Explainer.Explain(ctx, ExplainRequest{
		RunID: run.ID, StepID: step.ID, Attempt: step.Attempt,
		Company: run.CompanyID, Month: run.Month,
		Finding: f, EvidenceRefs: evidenceRefs(f), Feedback: step.Feedback,
		Model: w.stepModel(run.ID, step.ID),
	})
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
	res, err := w.Verifier.Verify(ctx, VerifyRequest{
		RunID: run.ID, StepID: step.ID, Attempt: step.Attempt,
		Company: run.CompanyID, Month: run.Month,
		Finding: f, EvidenceRefs: evidenceRefs(f), ExplanationRefs: explanation,
		ExplainAttempt: explainStep.Attempt,
		Model:          w.stepModel(run.ID, step.ID),
	})
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
	md := RenderReport(ReportData{
		RunID: run.ID, Company: run.CompanyID, Month: run.Month,
		Outcome: out.status, Reason: out.reason, Usage: usage,
		Findings: findings, Steps: steps, Fault: fault,
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
