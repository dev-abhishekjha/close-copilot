package agent

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/abhishekjha/close-copilot/internal/checks"
	"github.com/abhishekjha/close-copilot/internal/company"
	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/llm"
	"github.com/abhishekjha/close-copilot/internal/store"
)

// ---- an in-memory RunStore ----

type stepKey struct {
	run           uuid.UUID
	kind, subject string
}

// memStore is a RunStore in memory with the store's step semantics: one
// row per (run, kind, subject), Begin restarts a step that isn't done, and
// only the live attempt may finish it.
type memStore struct {
	*fakeArtifactStore

	mu        sync.Mutex
	runs      map[uuid.UUID]*store.CloseRun
	history   map[uuid.UUID][]string // run statuses in order
	steps     map[stepKey]*store.Step
	findings  []store.Finding
	exhausted bool
	// finishHook may fail a Finish before it is applied.
	finishHook func(store.Step, string) error
}

var _ RunStore = (*memStore)(nil)

func newMemStore() *memStore {
	return &memStore{
		fakeArtifactStore: newFakeArtifactStore(),
		runs:              map[uuid.UUID]*store.CloseRun{},
		history:           map[uuid.UUID][]string{},
		steps:             map[stepKey]*store.Step{},
	}
}

func (m *memStore) CreateCloseRun(_ context.Context, r store.CloseRun) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.runs[r.ID]; ok {
		return errors.New("mem: duplicate run")
	}
	m.runs[r.ID] = &r
	m.history[r.ID] = append(m.history[r.ID], r.Status)
	return nil
}

func (m *memStore) GetCloseRun(_ context.Context, id uuid.UUID) (store.CloseRun, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[id]
	if !ok {
		return store.CloseRun{}, fmt.Errorf("%w: close run %s", store.ErrNotFound, id)
	}
	return *r, nil
}

func (m *memStore) Begin(ctx context.Context, runID uuid.UUID, kind, subject string) (store.Step, bool, error) {
	if err := ctx.Err(); err != nil {
		return store.Step{}, false, err
	}
	if err := store.ValidateStepKind(kind); err != nil {
		return store.Step{}, false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	k := stepKey{runID, kind, subject}
	s, ok := m.steps[k]
	if ok && s.Status == store.StepDone {
		return *s, true, nil
	}
	if !ok {
		s = &store.Step{ID: uuid.New(), RunID: runID, Kind: kind, Subject: subject}
		m.steps[k] = s
	}
	s.Attempt++
	s.Status = store.StepRunning
	s.Error = nil
	return *s, false, nil
}

// live returns the step row the attempt may finish.
func (m *memStore) live(step store.Step) (*store.Step, error) {
	for _, s := range m.steps {
		if s.ID != step.ID {
			continue
		}
		if s.Status == store.StepDone {
			return nil, store.ErrStepDone
		}
		if s.Status != store.StepRunning || s.Attempt != step.Attempt {
			return nil, store.ErrStaleAttempt
		}
		return s, nil
	}
	return nil, store.ErrNotFound
}

func (m *memStore) checkRefs(refs []string) error {
	for _, r := range refs {
		if _, _, ok := m.getAny(r); !ok {
			return store.ErrMissingArtifact
		}
	}
	return nil
}

func (m *memStore) getAny(sha string) ([]byte, string, bool) {
	m.fakeArtifactStore.mu.Lock()
	defer m.fakeArtifactStore.mu.Unlock()
	b, ok := m.content[sha]
	return b, m.kinds[sha], ok
}

func (m *memStore) Finish(ctx context.Context, step store.Step, status string, refs []string, errText string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.finishHook != nil {
		if err := m.finishHook(step, status); err != nil {
			return err
		}
	}
	if err := m.checkRefs(refs); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s, err := m.live(step)
	if err != nil {
		return err
	}
	s.Status, s.OutputRefs = status, slices.Clone(refs)
	if errText != "" {
		s.Error = &errText
	}
	return nil
}

func (m *memStore) FinishWithFindings(ctx context.Context, step store.Step, refs []string, fs []store.Finding) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.finishHook != nil {
		if err := m.finishHook(step, store.StepDone); err != nil {
			return err
		}
	}
	for _, f := range fs {
		if len(f.Evidence) == 0 {
			return store.ErrMissingArtifact
		}
		for _, e := range f.Evidence {
			if err := m.checkRefs([]string{e.Artifact}); err != nil {
				return err
			}
		}
	}
	if err := m.checkRefs(refs); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s, err := m.live(step)
	if err != nil {
		return err
	}
	s.Status, s.OutputRefs = store.StepDone, slices.Clone(refs)
	m.findings = append(m.findings, fs...)
	return nil
}

func (m *memStore) ListSteps(_ context.Context, runID uuid.UUID) ([]store.Step, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []store.Step
	for _, s := range m.steps {
		if s.RunID == runID {
			out = append(out, *s)
		}
	}
	slices.SortFunc(out, func(a, b store.Step) int {
		return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Subject, b.Subject))
	})
	return out, nil
}

func (m *memStore) ListFindingsByRun(_ context.Context, runID uuid.UUID) ([]store.Finding, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []store.Finding
	for _, f := range m.findings {
		if f.RunID == runID {
			out = append(out, f)
		}
	}
	slices.SortFunc(out, func(a, b store.Finding) int { return cmp.Compare(a.ID.String(), b.ID.String()) })
	return out, nil
}

func (m *memStore) GetArtifact(_ context.Context, sha string) (store.Artifact, error) {
	b, kind, ok := m.getAny(sha)
	if !ok {
		return store.Artifact{}, store.ErrNotFound
	}
	return store.Artifact{SHA256: sha, Kind: kind, Content: b}, nil
}

func (m *memStore) SetRunState(ctx context.Context, runID uuid.UUID, status, errText string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := store.ValidateRunStatus(status); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[runID]
	if !ok {
		return store.ErrNotFound
	}
	r.Status = status
	r.Error = nil
	if errText != "" {
		r.Error = &errText
	}
	m.history[runID] = append(m.history[runID], status)
	return nil
}

func (m *memStore) runCalls(runID uuid.UUID) []store.LLMCall {
	m.fakeArtifactStore.mu.Lock()
	defer m.fakeArtifactStore.mu.Unlock()
	var out []store.LLMCall
	for _, c := range m.calls {
		if c.RunID == runID {
			out = append(out, c)
		}
	}
	return out
}

func (m *memStore) RollupRunUsage(ctx context.Context, runID uuid.UUID) (store.RunUsage, error) {
	if err := ctx.Err(); err != nil {
		return store.RunUsage{}, err
	}
	u := store.RunUsage{CostUSD: "0"}
	for _, c := range m.runCalls(runID) {
		u.InputTokens += c.InputTokens
		u.OutputTokens += c.OutputTokens
		u.CacheReadTokens += c.CacheReadTokens
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.runs[runID]; ok {
		r.InputTokens, r.OutputTokens = u.InputTokens, u.OutputTokens
	}
	return u, nil
}

func (m *memStore) RunTokensUsed(_ context.Context, runID uuid.UUID) (int64, error) {
	var n int64
	for _, c := range m.runCalls(runID) {
		n += c.InputTokens + c.OutputTokens
	}
	return n, nil
}

func (m *memStore) LLMBudgetExhausted(context.Context, config.Config, time.Time) (bool, error) {
	return m.exhausted, nil
}

func (m *memStore) SetFindingStatus(_ context.Context, id uuid.UUID, status string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.findings {
		if m.findings[i].ID == id {
			m.findings[i].Status = status
			return nil
		}
	}
	return store.ErrNotFound
}

func (m *memStore) stepsByKind(runID uuid.UUID) map[string][]store.Step {
	steps, _ := m.ListSteps(context.Background(), runID)
	out := map[string][]store.Step{}
	for _, s := range steps {
		out[s.Kind] = append(out[s.Kind], s)
	}
	return out
}

// ---- fake explainer and verifier ----

// fakeExplainer makes one model call through the step's model and stores
// an explanation artifact.
type fakeExplainer struct {
	st    ArtifactPutter
	mu    sync.Mutex
	calls int
	// block waits for the context to end instead of explaining.
	block bool
	// fail returns this error for every finding.
	fail error
}

func (e *fakeExplainer) Explain(ctx context.Context, req ExplainRequest) (StepResult, error) {
	e.mu.Lock()
	e.calls++
	e.mu.Unlock()
	if e.block {
		<-ctx.Done()
		return StepResult{}, ctx.Err()
	}
	if e.fail != nil {
		return StepResult{}, e.fail
	}
	if req.Model == nil || len(req.EvidenceRefs) == 0 {
		return StepResult{}, errors.New("fake explainer: no model or no evidence")
	}
	resp, err := req.Model.Complete(ctx, llm.Request{
		Model:     "claude-haiku-5-5",
		Messages:  []llm.Message{llm.NewUserTextMessage("Explain finding " + req.Finding.ID.String())},
		MaxTokens: 128,
	})
	if err != nil {
		return StepResult{}, err
	}
	sha, err := e.st.PutArtifact(ctx, store.ArtifactExplanation, req.RunID, req.StepID,
		map[string]any{"finding": req.Finding.ID.String(), "text": resp.Text, "evidence": req.EvidenceRefs})
	if err != nil {
		return StepResult{}, err
	}
	return StepResult{StepID: req.StepID, Status: store.StepDone, OutputRefs: []string{sha}}, nil
}

// fakeVerifier stores a verdict artifact; findings of type reviewType
// need review.
type fakeVerifier struct {
	st         ArtifactPutter
	reviewType string
	useModel   bool
}

func (v *fakeVerifier) Verify(ctx context.Context, req VerifyRequest) (StepResult, error) {
	if len(req.ExplanationRefs) == 0 {
		return StepResult{}, errors.New("fake verifier: no explanation")
	}
	if v.useModel {
		if _, err := req.Model.Complete(ctx, llm.Request{Model: "claude-haiku-5-5", Messages: []llm.Message{llm.NewUserTextMessage("verify")}, MaxTokens: 64}); err != nil {
			return StepResult{}, err
		}
	}
	verdict := "pass"
	if req.Finding.Type == v.reviewType {
		verdict = "needs_review"
	}
	sha, err := v.st.PutArtifact(ctx, store.ArtifactExplanation, req.RunID, req.StepID,
		map[string]any{"verdict": verdict, "explanation": req.ExplanationRefs[0]})
	if err != nil {
		return StepResult{}, err
	}
	return StepResult{StepID: req.StepID, OutputRefs: []string{sha}, NeedsReview: verdict == "needs_review"}, nil
}

// ---- helpers ----

func synthProfiles() map[string]company.Profile {
	return map[string]company.Profile{synthCompany: {ID: synthCompany, ERPCompany: "Test Co Pvt Ltd", Abbr: "TC",
		Bank: company.Bank{Name: "HDFC Bank", Account: "HDFC Current 0001"}}}
}

func fakeModel() *llm.FakeProvider {
	return llm.NewFakeProvider().WithFallback(llm.Response{
		Model: "claude-haiku-5-5", Text: "A synthetic bank charge not yet booked.", StopReason: "end_turn",
		Usage: llm.Usage{InputTokens: 80, OutputTokens: 20},
	})
}

type testRig struct {
	st    *memStore
	books *synthBooks
	ev    *synthEvidence
	model *llm.FakeProvider
	expl  *fakeExplainer
	ver   *fakeVerifier
	wf    *Workflow
}

func newRig(t *testing.T) *testRig {
	t.Helper()
	st := newMemStore()
	r := &testRig{st: st, books: &synthBooks{}, ev: &synthEvidence{}, model: fakeModel()}
	r.expl = &fakeExplainer{st: st}
	r.ver = &fakeVerifier{st: st}
	r.wf = &Workflow{
		Store: st, Books: r.books, Evidence: r.ev, Profiles: synthProfiles(),
		Explainer: r.expl, Verifier: r.ver, Model: r.model,
		Config:     config.Config{LLMRunTokenCap: 100000, LLMDailyBudgetUSD: 2},
		ResultsDir: t.TempDir(), Log: discardLog(),
	}
	return r
}

// stepStatuses summarises a run's steps as kind -> statuses (sorted).
func (r *testRig) stepStatuses(runID uuid.UUID) map[string][]string {
	out := map[string][]string{}
	for kind, steps := range r.st.stepsByKind(runID) {
		for _, s := range steps {
			out[kind] = append(out[kind], s.Status)
		}
		slices.Sort(out[kind])
	}
	return out
}

func repeat(s string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = s
	}
	return out
}

func wantSteps(t *testing.T, got map[string][]string, want map[string][]string) {
	t.Helper()
	gb, _ := json.Marshal(got)
	wb, _ := json.Marshal(want)
	if string(gb) != string(wb) {
		t.Errorf("steps\n got %s\nwant %s", gb, wb)
	}
}

// ---- tests ----

func TestRunCloseDoneWithFakes(t *testing.T) {
	r := newRig(t)
	r.ver.reviewType = checks.TypeUnmatchedLedgerEntry
	r.ver.useModel = true
	res, err := r.wf.RunClose(t.Context(), synthCompany, synthMonth)
	if err != nil {
		t.Fatalf("RunClose: %v", err)
	}
	if res.Status != store.RunDone || res.Findings != 4 || res.Reason != "" {
		t.Fatalf("result %+v, want done with 4 findings", res)
	}
	if got := r.st.history[res.RunID]; !slices.Equal(got, []string{store.RunQueued, store.RunRunning, store.RunDone}) {
		t.Errorf("run states %v", got)
	}
	wantSteps(t, r.stepStatuses(res.RunID), map[string][]string{
		"check.bankrec": {"done"},
		"explain":       repeat("done", 4),
		"investigate":   {"skipped"},
		"retrieve":      {"skipped"},
		"router":        {"done"},
		"synthesize":    {"done"},
		"verify":        repeat("done", 4),
	})

	// Every model call is recorded against its own explain or verify step.
	calls := r.st.runCalls(res.RunID)
	if len(calls) != 8 || r.model.CallCount() != 8 {
		t.Fatalf("llm calls recorded %d, made %d; want 8", len(calls), r.model.CallCount())
	}
	byStep := map[uuid.UUID]string{}
	for _, s := range r.st.stepsByKind(res.RunID)["explain"] {
		byStep[s.ID] = s.Kind
	}
	for _, s := range r.st.stepsByKind(res.RunID)["verify"] {
		byStep[s.ID] = s.Kind
	}
	for _, c := range calls {
		if byStep[c.StepID] == "" {
			t.Errorf("llm call %s recorded against step %s, not an explain or verify step", c.ID, c.StepID)
		}
	}
	if run := r.st.runs[res.RunID]; run.InputTokens != 8*80 || run.OutputTokens != 8*20 {
		t.Errorf("run usage %d/%d, want %d/%d", run.InputTokens, run.OutputTokens, 8*80, 8*20)
	}

	// The needs_review hook marks the finding the verifier flagged.
	fs, _ := r.st.ListFindingsByRun(t.Context(), res.RunID)
	for _, f := range fs {
		want := checks.StatusOpen
		if f.Type == checks.TypeUnmatchedLedgerEntry {
			want = checks.StatusNeedsReview
		}
		if f.Status != want {
			t.Errorf("finding %s status %s, want %s", f.Type, f.Status, want)
		}
	}

	// The report file and the report artifact hold the same Markdown.
	md, err := os.ReadFile(res.ReportPath)
	if err != nil {
		t.Fatal(err)
	}
	syn := r.st.stepsByKind(res.RunID)["synthesize"][0]
	b, kind, ok := r.st.getAny(syn.OutputRefs[0])
	if !ok || kind != store.ArtifactReport {
		t.Fatalf("report artifact %v kind %s", ok, kind)
	}
	var art ReportArtifact
	if err := json.Unmarshal(b, &art); err != nil || art.Markdown != string(md) || art.RunID != res.RunID.String() {
		t.Errorf("report artifact does not match the file (err %v)", err)
	}
	for _, want := range []string{"| Outcome | done |", "₹25,000.00", "₹1,180.00", "TXN-0002", "needs review", "recorded (artifact `"} {
		if !strings.Contains(string(md), want) {
			t.Errorf("report lacks %q:\n%s", want, md)
		}
	}
}

func TestRunClosePartialWithoutExplainer(t *testing.T) {
	r := newRig(t)
	r.wf.Explainer, r.wf.Verifier, r.wf.Model = nil, nil, nil
	res, err := r.wf.RunClose(t.Context(), synthCompany, synthMonth)
	if err != nil {
		t.Fatalf("RunClose: %v", err)
	}
	if res.Status != store.RunPartial || res.Reason != "explanations missing for 4 of 4 findings" {
		t.Fatalf("result %+v, want partial", res)
	}
	wantSteps(t, r.stepStatuses(res.RunID), map[string][]string{
		"check.bankrec": {"done"},
		"explain":       repeat("skipped", 4),
		"investigate":   {"skipped"},
		"retrieve":      {"skipped"},
		"router":        {"done"},
		"synthesize":    {"done"},
		"verify":        repeat("skipped", 4),
	})
	if fs, _ := r.st.ListFindingsByRun(t.Context(), res.RunID); len(fs) != 4 {
		t.Errorf("findings %d, want 4 saved", len(fs))
	}
	md, err := os.ReadFile(res.ReportPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"| Outcome | partial |", "| explain | 4 findings | 4 skipped | no explainer configured |", "missing (skipped: no explainer configured)"} {
		if !strings.Contains(string(md), want) {
			t.Errorf("report lacks %q:\n%s", want, md)
		}
	}
}

func TestRunCloseExplainerWithoutVerifierIsPartial(t *testing.T) {
	r := newRig(t)
	r.wf.Verifier = nil
	res, err := r.wf.RunClose(t.Context(), synthCompany, synthMonth)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != store.RunPartial || res.Reason != "4 of 4 explanations not verified" {
		t.Fatalf("result %+v", res)
	}
}

func TestRunClosePreflightRefuses(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(r *testRig)
		reason string
	}{
		{"no evidence for the month", func(r *testRig) { r.wf.Evidence = &emptyEvidence{} }, "evidence for testco 2026-08 is not loaded"},
		{"evidence unreachable", func(r *testRig) { r.ev.err = ErrUnavailable }, "evidence is unreachable"},
		{"books unreachable", func(r *testRig) { r.books.err = ErrUnavailable }, "books are unreachable"},
		{"daily budget exhausted", func(r *testRig) { r.st.exhausted = true }, "the daily LLM budget (LLM_DAILY_BUDGET_USD) is exhausted"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRig(t)
			tt.setup(r)
			res, err := r.wf.RunClose(t.Context(), synthCompany, synthMonth)
			if !errors.Is(err, ErrPreflightRefused) || !errors.Is(err, ErrRunFailed) {
				t.Fatalf("err %v, want a preflight refusal", err)
			}
			if res.Status != store.RunFailed || !strings.HasPrefix(res.Reason, "preflight: "+tt.reason) {
				t.Errorf("result %+v, want failed with %q", res, tt.reason)
			}
			run := r.st.runs[res.RunID]
			if run.Status != store.RunFailed || run.Error == nil || *run.Error != res.Reason {
				t.Errorf("stored run %+v", run)
			}
			// Refused before any check: only the router step exists.
			wantSteps(t, r.stepStatuses(res.RunID), map[string][]string{"router": {"failed"}})
			if r.expl.calls != 0 || r.model.CallCount() != 0 {
				t.Error("a refused run explained something")
			}
		})
	}
}

type emptyEvidence struct{ synthEvidence }

func (*emptyEvidence) BankLines(context.Context, string, time.Time, time.Time) ([]store.BankLine, error) {
	return nil, nil
}

func TestRunCloseRejectsBadInput(t *testing.T) {
	r := newRig(t)
	if _, err := r.wf.RunClose(t.Context(), "nobody", synthMonth); !errors.Is(err, ErrUnknownCompany) {
		t.Errorf("unknown company: %v", err)
	}
	if _, err := r.wf.RunClose(t.Context(), synthCompany, "2026-13"); err == nil {
		t.Error("bad month: want an error")
	}
	if len(r.st.runs) != 0 {
		t.Errorf("bad input created %d runs", len(r.st.runs))
	}
}

func TestResumeCloseSkipsDoneSteps(t *testing.T) {
	r := newRig(t)
	r.wf.Explainer, r.wf.Verifier = nil, nil
	first, err := r.wf.RunClose(t.Context(), synthCompany, synthMonth)
	if err != nil || first.Status != store.RunPartial {
		t.Fatalf("first run %+v: %v", first, err)
	}
	before, _ := r.st.ListFindingsByRun(t.Context(), first.RunID)
	booksCalls := r.books.calls

	// Resume with an explainer: only the steps that aren't done run.
	r.wf.Explainer, r.wf.Verifier = r.expl, r.ver
	res, err := r.wf.ResumeClose(t.Context(), first.RunID)
	if err != nil {
		t.Fatalf("ResumeClose: %v", err)
	}
	if res.RunID != first.RunID || res.Status != store.RunDone {
		t.Fatalf("resume result %+v, want done", res)
	}
	after, _ := r.st.ListFindingsByRun(t.Context(), first.RunID)
	if len(after) != len(before) || !slices.EqualFunc(after, before, func(a, b store.Finding) bool { return a.ID == b.ID }) {
		t.Errorf("finding IDs changed across the resume")
	}
	if r.books.calls != booksCalls {
		t.Errorf("resume read the books again (%d -> %d calls)", booksCalls, r.books.calls)
	}
	for kind, steps := range r.st.stepsByKind(first.RunID) {
		for _, s := range steps {
			want := 1
			if kind == store.StepKindExplain || kind == store.StepKindVerify || kind == store.StepKindRetrieve || kind == store.StepKindInvestigate {
				want = 2 // skipped steps are re-run
			}
			if kind == store.StepKindSynthesize {
				want = 1 // done: kept, report restored only if missing
			}
			if s.Attempt != want {
				t.Errorf("%s/%s attempt %d, want %d", s.Kind, s.Subject, s.Attempt, want)
			}
		}
	}
	// The report was done in the first run; the step stays done.
	if st := r.stepStatuses(first.RunID)["synthesize"]; !slices.Equal(st, []string{"done"}) {
		t.Errorf("synthesize %v", st)
	}

	if _, err := r.wf.ResumeClose(t.Context(), first.RunID); !errors.Is(err, ErrRunDone) {
		t.Errorf("resume of a done run: %v, want ErrRunDone", err)
	}
}

func TestResumeCloseRestoresMissingReport(t *testing.T) {
	r := newRig(t)
	r.wf.Explainer, r.wf.Verifier = nil, nil
	first, err := r.wf.RunClose(t.Context(), synthCompany, synthMonth)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(first.ReportPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(first.ReportPath); err != nil {
		t.Fatal(err)
	}
	res, err := r.wf.ResumeClose(t.Context(), first.RunID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(res.ReportPath)
	if err != nil || string(got) != string(want) {
		t.Errorf("restored report differs (err %v)", err)
	}
}

func TestRunCloseStopsOnStaleAttempt(t *testing.T) {
	r := newRig(t)
	r.st.finishHook = func(s store.Step, _ string) error {
		if s.Kind == store.StepKindExplain {
			return fmt.Errorf("%w: step %s", store.ErrStaleAttempt, s.ID)
		}
		return nil
	}
	res, err := r.wf.RunClose(t.Context(), synthCompany, synthMonth)
	if !errors.Is(err, store.ErrStaleAttempt) {
		t.Fatalf("err %v, want ErrStaleAttempt", err)
	}
	// Another attempt owns the run: it is left running, not failed.
	if run := r.st.runs[res.RunID]; run.Status != store.RunRunning {
		t.Errorf("run status %s, want running", run.Status)
	}
	if st := r.stepStatuses(res.RunID); len(st["synthesize"]) != 0 || len(st["verify"]) != 0 {
		t.Errorf("the run went on after a stale attempt: %v", st)
	}
	// Not retried in a loop: one Explain per finding at most.
	if r.expl.calls > 4 {
		t.Errorf("explainer called %d times", r.expl.calls)
	}
}

func TestRunCloseTokenCap(t *testing.T) {
	r := newRig(t)
	r.wf.Parallel = 1
	r.wf.Config.LLMRunTokenCap = 150 // each call uses 100 tokens
	res, err := r.wf.RunClose(t.Context(), synthCompany, synthMonth)
	if err != nil {
		t.Fatalf("RunClose: %v", err)
	}
	if res.Status != store.RunPartial || !strings.Contains(res.Reason, "explanations missing for 2 of 4 findings") {
		t.Fatalf("result %+v, want partial with 2 explanations missing", res)
	}
	wantSteps(t, r.stepStatuses(res.RunID), map[string][]string{
		"check.bankrec": {"done"},
		"explain":       {"done", "done", "skipped", "skipped"},
		"investigate":   {"skipped"},
		"retrieve":      {"skipped"},
		"router":        {"done"},
		"synthesize":    {"done"},
		"verify":        {"done", "done", "skipped", "skipped"},
	})
	if r.model.CallCount() != 2 {
		t.Errorf("model calls %d, want 2 before the cap", r.model.CallCount())
	}
	for _, s := range r.st.stepsByKind(res.RunID)["explain"] {
		if s.Status == store.StepSkipped && (s.Error == nil || !strings.Contains(*s.Error, "run token cap reached")) {
			t.Errorf("skipped explain step reason %v", s.Error)
		}
	}
}

func TestCappedProviderRefusesOverCap(t *testing.T) {
	st := newMemStore()
	run, step := uuid.New(), uuid.New()
	st.calls = append(st.calls, store.LLMCall{RunID: run, StepID: step, InputTokens: 90, OutputTokens: 10})
	model := fakeModel()
	p := &cappedProvider{inner: model, tokens: st, runID: run, limit: 100}
	if _, err := p.Complete(t.Context(), llm.Request{}); !errors.Is(err, ErrRunTokenCap) {
		t.Errorf("at the cap: %v, want ErrRunTokenCap", err)
	}
	p.limit = 101
	if _, err := p.Complete(t.Context(), llm.Request{}); err != nil {
		t.Errorf("below the cap: %v", err)
	}
	if model.CallCount() != 1 {
		t.Errorf("model calls %d, want 1", model.CallCount())
	}
}

func TestCostCheckedRecorder(t *testing.T) {
	st := newFakeArtifactStore()
	rec := costCheckedRecorder{LLMCallRecorder: st}
	p, _ := st.PutArtifact(t.Context(), store.ArtifactPrompt, uuid.Nil, uuid.Nil, "p")
	for _, tt := range []struct {
		cost string
		ok   bool
	}{{"", true}, {"0", true}, {"0.000123", true}, {"9999.999999", true}, {"-0.01", false}, {"10000", false}, {"12345.6", false}, {"1e5", false}, {"abc", false}} {
		_, err := rec.InsertLLMCall(t.Context(), store.LLMCall{RunID: uuid.New(), StepID: uuid.New(), PromptSHA256: p, ResponseSHA256: p, CostUSD: tt.cost})
		if (err == nil) != tt.ok {
			t.Errorf("cost %q: err %v, want ok=%v", tt.cost, err, tt.ok)
		}
	}
}

func TestRunCloseCancelled(t *testing.T) {
	r := newRig(t)
	r.expl.block = true
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		for {
			r.expl.mu.Lock()
			n := r.expl.calls
			r.expl.mu.Unlock()
			if n > 0 {
				cancel()
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	res, err := r.wf.RunClose(ctx, synthCompany, synthMonth)
	if !errors.Is(err, ErrRunFailed) || !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v, want a failed, cancelled run", err)
	}
	run := r.st.runs[res.RunID]
	if run.Status != store.RunFailed || run.Error == nil || *run.Error != ReasonCancelled {
		t.Errorf("run %s %v, want failed with reason cancelled", run.Status, run.Error)
	}
	for _, s := range r.st.stepsByKind(res.RunID)["explain"] {
		if s.Status != store.StepFailed || s.Error == nil || *s.Error != ReasonCancelled {
			t.Errorf("explain step %s %v, want failed (cancelled)", s.Status, s.Error)
		}
	}
}

func TestRunCloseTimeout(t *testing.T) {
	r := newRig(t)
	r.expl.block = true
	r.wf.Timeout = 50 * time.Millisecond
	res, err := r.wf.RunClose(t.Context(), synthCompany, synthMonth)
	if !errors.Is(err, ErrRunFailed) {
		t.Fatalf("err %v, want failed", err)
	}
	if !strings.HasPrefix(res.Reason, "timeout") {
		t.Errorf("reason %q, want a timeout", res.Reason)
	}
}

func TestRunTimeoutNeverLengthens(t *testing.T) {
	w := &Workflow{Timeout: time.Hour}
	if w.runTimeout() != DefaultRunTimeout {
		t.Errorf("timeout %s, want %s", w.runTimeout(), DefaultRunTimeout)
	}
	w.Timeout = time.Minute
	if w.runTimeout() != time.Minute {
		t.Errorf("timeout %s, want 1m", w.runTimeout())
	}
}

// nulEvidence returns a bank line with a NUL in its narration.
type nulEvidence struct{ synthEvidence }

func (*nulEvidence) BankLines(context.Context, string, time.Time, time.Time) ([]store.BankLine, error) {
	return []store.BankLine{{CompanyID: synthCompany, TxnID: "TXN-NUL", TxnDate: synthDay(3), Narration: "SMS\x00CHGS", AmountPaise: -100}}, nil
}

func TestRunCloseUnstorableContentFails(t *testing.T) {
	r := newRig(t)
	r.wf.Evidence = &nulEvidence{}
	res, err := r.wf.RunClose(t.Context(), synthCompany, synthMonth)
	if !errors.Is(err, store.ErrUnstorableContent) {
		t.Fatalf("err %v, want ErrUnstorableContent", err)
	}
	if res.Status != store.RunFailed || !strings.HasPrefix(res.Reason, "unstorable content") {
		t.Errorf("result %+v", res)
	}
}

func TestRunCloseExplainerFailureIsPartial(t *testing.T) {
	r := newRig(t)
	r.expl.fail = errors.New("synthetic explainer failure")
	res, err := r.wf.RunClose(t.Context(), synthCompany, synthMonth)
	if err != nil {
		t.Fatalf("RunClose: %v", err)
	}
	if res.Status != store.RunPartial {
		t.Fatalf("result %+v, want partial", res)
	}
	if got := r.stepStatuses(res.RunID)["explain"]; !slices.Equal(got, repeat("failed", 4)) {
		t.Errorf("explain steps %v", got)
	}
}

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
