package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/abhishekjha/close-copilot/internal/checks"
	"github.com/abhishekjha/close-copilot/internal/llm"
	"github.com/abhishekjha/close-copilot/internal/money"
	"github.com/abhishekjha/close-copilot/internal/store"
)

// answeringModel is a fake model that answers emit_explanation for the
// finding in the request: it states and cites the finding's amount, so
// the answer is grounded. A finding whose title contains a key of bad
// first gets that many answers with an invented amount (₹4,321.00).
type answeringModel struct {
	mu    sync.Mutex
	bad   map[string]int
	seen  map[string]int
	calls []llm.Request
}

func newAnsweringModel(bad map[string]int) *answeringModel {
	return &answeringModel{bad: bad, seen: map[string]int{}}
}

const inventedSentence = " It also adds ₹4,321.00 of fees."

func (m *answeringModel) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	if err := ctx.Err(); err != nil {
		return llm.Response{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, req)
	f, err := findingSection(req.Messages[0].Content)
	if err != nil {
		return llm.Response{}, err
	}
	m.seen[f.Title]++
	var amt money.Paise
	if f.AmountPaise != nil {
		amt = *f.AmountPaise
	}
	text := fmt.Sprintf("The books lack the %s shown in the evidence.", amt.Format())
	for key, n := range m.bad {
		if strings.Contains(f.Title, key) && m.seen[f.Title] <= n {
			text += inventedSentence
		}
	}
	args, err := json.Marshal(map[string]any{
		"explanation": text, "suggested_action": checks.ActionInvestigate, "action_note": "Check the entry.",
		"cited_amounts_paise": []money.Paise{amt}, "citations": []any{}, "needs_review": false,
	})
	if err != nil {
		return llm.Response{}, err
	}
	return llm.Response{
		Model:      req.Model + "-20261001",
		ToolCalls:  []llm.ToolCall{{ID: "call_0", Name: ExplainToolName, Args: args}},
		StopReason: "tool_use",
		Usage:      llm.Usage{InputTokens: 100, OutputTokens: 20},
	}, nil
}

// callsFor returns the requests made for the finding whose title contains
// key.
func (m *answeringModel) callsFor(t *testing.T, key string) []llm.Request {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []llm.Request
	for _, c := range m.calls {
		f, err := findingSection(c.Messages[0].Content)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(f.Title, key) {
			out = append(out, c)
		}
	}
	return out
}

func (m *answeringModel) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}

// findingSection decodes the FINDING section of a user message.
func findingSection(user string) (findingView, error) {
	_, rest, ok := strings.Cut(user, "\n<<<FINDING\n")
	body, _, ok2 := strings.Cut(rest, "\nFINDING>>>")
	if !ok || !ok2 {
		return findingView{}, errors.New("answering model: no FINDING section")
	}
	var f findingView
	if err := json.Unmarshal([]byte(body), &f); err != nil {
		return findingView{}, fmt.Errorf("answering model: FINDING: %w", err)
	}
	return f, nil
}

// retryErrorModel answers like inner, adds a proposal to the first answer
// for the finding whose title contains key, and fails every later call for
// that finding (a model error on the retry).
type retryErrorModel struct {
	inner *answeringModel
	key   string
	mu    sync.Mutex
	seen  int
}

func (m *retryErrorModel) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	f, err := findingSection(req.Messages[0].Content)
	if err != nil {
		return llm.Response{}, err
	}
	if !strings.Contains(f.Title, m.key) {
		return m.inner.Complete(ctx, req)
	}
	m.mu.Lock()
	m.seen++
	n := m.seen
	m.mu.Unlock()
	if n > 1 {
		return llm.Response{}, errors.New("synthetic model failure on the retry")
	}
	resp, err := m.inner.Complete(ctx, req)
	if err != nil {
		return resp, err
	}
	var args map[string]any
	if err := json.Unmarshal(resp.ToolCalls[0].Args, &args); err != nil {
		return llm.Response{}, err
	}
	args["suggested_action"] = checks.ActionBookEntry
	args["proposal"] = map[string]any{"posting_date": "2026-08-31", "remark": "Fees", "lines": []map[string]any{
		{"account": synthBank, "debit_paise": 432100, "credit_paise": 0},
		{"account": synthBank, "debit_paise": 0, "credit_paise": 432100},
	}}
	b, err := json.Marshal(args)
	if err != nil {
		return llm.Response{}, err
	}
	resp.ToolCalls[0].Args = b
	return resp, nil
}

// countingVerifier counts Verify calls.
type countingVerifier struct {
	inner Verifier
	mu    sync.Mutex
	calls int
}

func (c *countingVerifier) Verify(ctx context.Context, req VerifyRequest) (StepResult, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return c.inner.Verify(ctx, req)
}

// newVerifyRig is the synthetic month with the LLMExplainer and the
// CodeVerifier over the answering model, one finding at a time.
func newVerifyRig(t *testing.T, model llm.Provider) (*testRig, *LLMExplainer, *countingVerifier) {
	t.Helper()
	r := newRig(t)
	st := &explainStore{memStore: r.st, set: map[uuid.UUID]setExplanation{}}
	ex := &LLMExplainer{Store: st, Accounts: BooksAccounts{Books: r.books}, FastModel: explainFast, StrongModel: explainStrong, Log: discardLog()}
	ver := &countingVerifier{inner: &CodeVerifier{Store: r.st}}
	r.wf.Model, r.wf.Explainer, r.wf.Verifier, r.wf.Parallel = model, ex, ver, 1
	return r, ex, ver
}

// stepFor returns the run's step of kind for the finding whose title
// contains key.
func (r *testRig) stepFor(t *testing.T, runID uuid.UUID, kind, key string) (store.Step, store.Finding) {
	t.Helper()
	fs, _ := r.st.ListFindingsByRun(t.Context(), runID)
	for _, f := range fs {
		if !strings.Contains(f.Title, key) {
			continue
		}
		for _, s := range r.st.stepsByKind(runID)[kind] {
			if s.Subject == f.ID.String() {
				return s, f
			}
		}
	}
	t.Fatalf("no %s step for a finding titled %q", kind, key)
	return store.Step{}, store.Finding{}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestWorkflowVerifyRetry(t *testing.T) {
	const key = "SMS CHGS"

	t.Run("fails once, passes on the retry", func(t *testing.T) {
		model := newAnsweringModel(map[string]int{key: 1})
		r, _, _ := newVerifyRig(t, model)
		res, err := r.wf.RunClose(t.Context(), synthCompany, synthMonth)
		if err != nil {
			t.Fatalf("RunClose: %v", err)
		}
		if res.Status != store.RunDone || res.Findings != 4 {
			t.Fatalf("result %+v, want done", res)
		}
		if n := model.count(); n != 5 {
			t.Errorf("%d model calls, want 5 (4 findings, one retry)", n)
		}
		calls := model.callsFor(t, key)
		if len(calls) != 2 {
			t.Fatalf("%d calls for the retried finding, want 2", len(calls))
		}
		if strings.Contains(calls[0].Messages[0].Content, "VERIFIER_FEEDBACK") {
			t.Error("the first attempt had feedback")
		}
		second := calls[1].Messages[0].Content
		if !strings.Contains(second, "\n<<<VERIFIER_FEEDBACK\n") || !strings.Contains(second, ViolationAmountNotInEvidence) ||
			!strings.Contains(second, `"value_paise":432100`) || calls[1].Model != explainFast {
			t.Errorf("second request lacks the violations or uses %s:\n%s", calls[1].Model, second)
		}
		assertAllowlist(t, model.calls)

		explain, f := r.stepFor(t, res.RunID, store.StepKindExplain, key)
		verify, _ := r.stepFor(t, res.RunID, store.StepKindVerify, key)
		if explain.Attempt != 2 || explain.Status != store.StepDone || len(explain.Feedback) == 0 {
			t.Errorf("explain step %+v", explain)
		}
		if verify.Attempt != 2 || verify.Status != store.StepDone || verify.Error != nil {
			t.Errorf("verify step %+v", verify)
		}
		if f.Status != checks.StatusOpen || !f.Verified {
			t.Errorf("finding status %s verified %v, want open and verified", f.Status, f.Verified)
		}
		md := readFile(t, res.ReportPath)
		if !strings.Contains(md, "| Verification | verified 4 of 4 explanations (pass rate 100%), 1 retried, 0 needs_review |") ||
			strings.Contains(md, "Fault injection") {
			t.Errorf("report:\n%s", md)
		}
	})

	t.Run("fails three times: strong model, then needs review", func(t *testing.T) {
		model := newAnsweringModel(map[string]int{key: 10})
		r, _, ver := newVerifyRig(t, model)
		res, err := r.wf.RunClose(t.Context(), synthCompany, synthMonth)
		if err != nil {
			t.Fatalf("RunClose: %v", err)
		}
		if res.Status != store.RunPartial || res.Reason != "1 of 4 explanations not verified" {
			t.Fatalf("result %+v, want partial", res)
		}
		calls := model.callsFor(t, key)
		if len(calls) != MaxExplainAttempts {
			t.Fatalf("%d calls for the failing finding, want %d", len(calls), MaxExplainAttempts)
		}
		if calls[0].Model != explainFast || calls[1].Model != explainFast || calls[2].Model != explainStrong {
			t.Errorf("models %s, %s, %s", calls[0].Model, calls[1].Model, calls[2].Model)
		}
		if ver.calls != 4+2 {
			t.Errorf("%d verifications, want 6", ver.calls)
		}
		explain, f := r.stepFor(t, res.RunID, store.StepKindExplain, key)
		verify, _ := r.stepFor(t, res.RunID, store.StepKindVerify, key)
		if explain.Attempt != 3 || explain.Status != store.StepDone {
			t.Errorf("explain step %+v", explain)
		}
		if verify.Status != store.StepDone || verify.Error == nil ||
			*verify.Error != "verification failed: amount_not_in_evidence after 3 explain attempts" || len(verify.OutputRefs) != 1 {
			t.Errorf("verify step %+v", verify)
		}
		if f.Status != checks.StatusNeedsReview || f.Verified {
			t.Errorf("finding status %s verified %v", f.Status, f.Verified)
		}
		// The stored verdict is the failing one of attempt 3.
		var v VerdictArtifact
		b, _, _ := r.st.getAny(verify.OutputRefs[0])
		if err := json.Unmarshal(b, &v); err != nil || v.Pass || v.ExplainAttempt != 3 {
			t.Errorf("verdict %+v, %v", v, err)
		}
		md := readFile(t, res.ReportPath)
		if !strings.Contains(md, "| Verification | verified 3 of 4 explanations (pass rate 75%), 1 retried, 1 needs_review |") {
			t.Errorf("report:\n%s", md)
		}
		// A resume runs nothing more: the failure is final.
		before := model.count()
		if _, err := r.wf.ResumeClose(t.Context(), res.RunID); err != nil {
			t.Fatal(err)
		}
		if model.count() != before {
			t.Error("a resume explained the needs-review finding again")
		}
	})

	t.Run("a crash between the reopen and the re-explain resumes", func(t *testing.T) {
		model := newAnsweringModel(map[string]int{key: 1})
		r, _, _ := newVerifyRig(t, model)
		errCrashed := errors.New("synthetic crash after the reopen")
		r.st.afterReopen = func(store.Step) error { return errCrashed }
		res, err := r.wf.RunClose(t.Context(), synthCompany, synthMonth)
		if !errors.Is(err, errCrashed) {
			t.Fatalf("RunClose err %v, want the crash", err)
		}
		explain, _ := r.stepFor(t, res.RunID, store.StepKindExplain, key)
		if explain.Status != store.StepPending || len(explain.Feedback) == 0 || explain.Attempt != 1 {
			t.Fatalf("explain step after the crash %+v, want pending with feedback", explain)
		}
		// The attempt-1 verdict is already on the verify step.
		verdict1 := failedVerdictAfterCrash(t, r, res.RunID, key)

		r.st.afterReopen = nil
		res2, err := r.wf.ResumeClose(t.Context(), res.RunID)
		if err != nil || res2.Status != store.RunDone {
			t.Fatalf("resume %+v: %v", res2, err)
		}
		calls := model.callsFor(t, key)
		if len(calls) != 2 || !strings.Contains(calls[1].Messages[0].Content, ViolationAmountNotInEvidence) {
			t.Errorf("%d calls; the re-explain must carry the feedback", len(calls))
		}
		explain, f := r.stepFor(t, res.RunID, store.StepKindExplain, key)
		verify, _ := r.stepFor(t, res.RunID, store.StepKindVerify, key)
		if explain.Attempt != 2 || explain.Status != store.StepDone || verify.Status != store.StepDone || verify.Error != nil || !f.Verified {
			t.Errorf("explain %+v verify %+v verified %v", explain, verify, f.Verified)
		}
		if !slices.Contains(verify.InputRefs, verdict1) {
			t.Errorf("verify input_refs %v lost the attempt-1 verdict %s", verify.InputRefs, verdict1)
		}
	})

	t.Run("a crash after the reopen, then the re-explain errors", func(t *testing.T) {
		model := &retryErrorModel{inner: newAnsweringModel(map[string]int{key: 1}), key: key}
		r, _, _ := newVerifyRig(t, model)
		errCrashed := errors.New("synthetic crash after the reopen")
		r.st.afterReopen = func(store.Step) error { return errCrashed }
		res, err := r.wf.RunClose(t.Context(), synthCompany, synthMonth)
		if !errors.Is(err, errCrashed) {
			t.Fatalf("RunClose err %v, want the crash", err)
		}
		verdict1 := failedVerdictAfterCrash(t, r, res.RunID, key)

		r.st.afterReopen = nil
		res2, err := r.wf.ResumeClose(t.Context(), res.RunID)
		if err != nil || res2.Status != store.RunPartial {
			t.Fatalf("resume %+v: %v", res2, err)
		}
		explain, f := r.stepFor(t, res.RunID, store.StepKindExplain, key)
		verify, _ := r.stepFor(t, res.RunID, store.StepKindVerify, key)
		if explain.Attempt != 2 || explain.Status != store.StepFailed || len(explain.Feedback) == 0 || len(explain.InputRefs) != 1 {
			t.Errorf("explain step %+v", explain)
		}
		// What cmd/audit rebuilds from: the skipped verify step keeps the
		// attempt-1 verdict in input_refs.
		if verify.Status != store.StepSkipped || !slices.Equal(verify.InputRefs, []string{verdict1}) {
			t.Errorf("verify step %+v, want skipped with input_refs [%s]", verify, verdict1)
		}
		if f.Status != checks.StatusNeedsReview || f.Verified || f.Explanation != nil {
			t.Errorf("finding status %s verified %v explanation %v", f.Status, f.Verified, f.Explanation != nil)
		}
	})

	t.Run("a passing verdict is not re-run on resume", func(t *testing.T) {
		model := newAnsweringModel(nil)
		r, _, ver := newVerifyRig(t, model)
		res, err := r.wf.RunClose(t.Context(), synthCompany, synthMonth)
		if err != nil || res.Status != store.RunDone {
			t.Fatalf("RunClose %+v: %v", res, err)
		}
		verifications, calls := ver.calls, model.count()
		// Pretend the run stopped before it was marked done.
		if err := r.st.SetRunState(t.Context(), res.RunID, store.RunRunning, ""); err != nil {
			t.Fatal(err)
		}
		res2, err := r.wf.ResumeClose(t.Context(), res.RunID)
		if err != nil || res2.Status != store.RunDone {
			t.Fatalf("resume %+v: %v", res2, err)
		}
		if ver.calls != verifications || model.count() != calls {
			t.Errorf("resume re-ran %d verifications and %d model calls", ver.calls-verifications, model.count()-calls)
		}
		for _, s := range r.st.stepsByKind(res.RunID)[store.StepKindVerify] {
			if s.Attempt != 1 || s.Status != store.StepDone {
				t.Errorf("verify step %+v", s)
			}
		}
	})

	t.Run("verify fails, then the explain retry errors", func(t *testing.T) {
		model := &retryErrorModel{inner: newAnsweringModel(map[string]int{key: 1}), key: key}
		r, _, _ := newVerifyRig(t, model)
		res, err := r.wf.RunClose(t.Context(), synthCompany, synthMonth)
		if err != nil {
			t.Fatalf("RunClose: %v", err)
		}
		if res.Status != store.RunPartial {
			t.Errorf("result %+v, want partial", res)
		}
		explain, f := r.stepFor(t, res.RunID, store.StepKindExplain, key)
		verify, _ := r.stepFor(t, res.RunID, store.StepKindVerify, key)
		if explain.Attempt != 2 || explain.Status != store.StepFailed || len(explain.Feedback) == 0 {
			t.Errorf("explain step %+v", explain)
		}
		if verify.Status != store.StepSkipped {
			t.Errorf("verify step %+v", verify)
		}
		if f.Status != checks.StatusNeedsReview || f.Verified {
			t.Errorf("finding status %s verified %v, want needs_review and not verified", f.Status, f.Verified)
		}
		if f.Explanation != nil || f.Action != nil || f.Proposal != nil || f.Citations != nil {
			t.Errorf("the rejected explanation stayed on the finding: %+v", f)
		}
		md := readFile(t, res.ReportPath)
		if strings.Contains(md, "Proposed entry") || strings.Contains(md, "4,321.00") {
			t.Errorf("the report shows the rejected explanation or proposal:\n%s", md)
		}
		if !strings.Contains(md, "1 needs_review |") {
			t.Errorf("report:\n%s", md)
		}
	})

	t.Run("a stale reopen stops the run", func(t *testing.T) {
		model := newAnsweringModel(map[string]int{key: 1})
		r, _, _ := newVerifyRig(t, model)
		r.st.afterReopen = func(store.Step) error { return fmt.Errorf("%w: synthetic", store.ErrStaleAttempt) }
		res, err := r.wf.RunClose(t.Context(), synthCompany, synthMonth)
		if !errors.Is(err, store.ErrStaleAttempt) {
			t.Fatalf("err %v, want ErrStaleAttempt", err)
		}
		if run := r.st.runs[res.RunID]; run.Status != store.RunRunning {
			t.Errorf("run status %s, want running (another attempt owns it)", run.Status)
		}
	})
}

// failedVerdictAfterCrash checks that a crash right after the reopen left
// the verify step failed with the attempt-1 failing verdict in its
// output_refs, and returns the verdict's address.
func failedVerdictAfterCrash(t *testing.T, r *testRig, runID uuid.UUID, key string) string {
	t.Helper()
	verify, _ := r.stepFor(t, runID, store.StepKindVerify, key)
	if verify.Status != store.StepFailed || len(verify.OutputRefs) != 1 {
		t.Fatalf("verify step after the crash %+v, want failed with its verdict", verify)
	}
	var v VerdictArtifact
	b, kind, _ := r.st.getAny(verify.OutputRefs[0])
	if err := json.Unmarshal(b, &v); err != nil || kind != store.ArtifactVerdict || v.Pass || v.ExplainAttempt != 1 {
		t.Fatalf("verify output after the crash: kind %s verdict %+v, %v", kind, v, err)
	}
	return verify.OutputRefs[0]
}

// TestWorkflowVerifierNeverCallsTheModel runs the real verifier with a
// step model that fails the test when called.
func TestWorkflowVerifierNeverCallsTheModel(t *testing.T) {
	model := newAnsweringModel(nil)
	r, _, _ := newVerifyRig(t, model)
	r.wf.Verifier = &CodeVerifier{Store: r.st}
	res, err := r.wf.RunClose(t.Context(), synthCompany, synthMonth)
	if err != nil || res.Status != store.RunDone {
		t.Fatalf("RunClose %+v: %v", res, err)
	}
	// Every recorded call is on an explain step.
	explainSteps := map[uuid.UUID]bool{}
	for _, s := range r.st.stepsByKind(res.RunID)[store.StepKindExplain] {
		explainSteps[s.ID] = true
	}
	for _, c := range r.st.runCalls(res.RunID) {
		if !explainSteps[c.StepID] {
			t.Errorf("model call recorded on step %s, not an explain step", c.StepID)
		}
	}
	if n := len(r.st.runCalls(res.RunID)); n != 4 {
		t.Errorf("%d model calls, want 4", n)
	}
}
