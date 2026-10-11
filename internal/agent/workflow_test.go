package agent

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/abhishekjha/close-copilot/internal/checks"
	"github.com/abhishekjha/close-copilot/internal/company"
	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/ledger"
	"github.com/abhishekjha/close-copilot/internal/llm"
	"github.com/abhishekjha/close-copilot/internal/mcpkit"
	"github.com/abhishekjha/close-copilot/internal/store"
	"github.com/abhishekjha/close-copilot/internal/telemetry"
	"github.com/abhishekjha/close-copilot/internal/telemetry/llmtrace"
	"github.com/abhishekjha/close-copilot/internal/telemetry/telemetrytest"
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
	// beginHook may fail a Begin before it is applied.
	beginHook func(kind, subject string) error
	// afterReopen runs after a ReopenStep is applied; an error simulates
	// a crash right after the reopen committed.
	afterReopen func(store.Step) error
	// cleared counts ClearFindingExplanation calls.
	cleared int
	// resolutions counts SetFindingResolution calls.
	resolutions int
}

var _ InvestigateStore = (*memStore)(nil)

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

func (m *memStore) UpdateCloseRun(_ context.Context, r store.CloseRun) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.runs[r.ID]; !ok {
		return fmt.Errorf("%w: close run %s", store.ErrNotFound, r.ID)
	}
	*m.runs[r.ID] = r
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
	if m.beginHook != nil {
		if err := m.beginHook(kind, subject); err != nil {
			return store.Step{}, false, err
		}
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
	// As store.Begin: a restart keeps earlier attempts' outputs in
	// input_refs.
	s.InputRefs = append(slices.Clone(s.InputRefs), s.OutputRefs...)
	s.OutputRefs = nil
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

// GetArtifact re-hashes the content, as the store does, so a test can
// tamper with it.
func (m *memStore) GetArtifact(_ context.Context, sha string) (store.Artifact, error) {
	b, kind, ok := m.getAny(sha)
	if !ok {
		return store.Artifact{}, fmt.Errorf("%w: artifact %s", store.ErrNotFound, sha)
	}
	canon, got, err := store.CanonicalHash(json.RawMessage(b))
	if err != nil {
		return store.Artifact{}, err
	}
	if got != sha {
		return store.Artifact{}, fmt.Errorf("%w: %s", store.ErrArtifactMismatch, sha)
	}
	return store.Artifact{SHA256: sha, Kind: kind, Content: canon}, nil
}

// tamper replaces an artifact's stored content.
func (m *memStore) tamper(sha string, content []byte) {
	m.fakeArtifactStore.mu.Lock()
	defer m.fakeArtifactStore.mu.Unlock()
	m.content[sha] = content
}

// ReopenStep has the store's semantics: only the done attempt that
// finished the step may reopen it.
func (m *memStore) ReopenStep(ctx context.Context, step store.Step, feedback json.RawMessage) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(feedback) == 0 || !json.Valid(feedback) {
		return errors.New("mem: feedback must be JSON")
	}
	m.mu.Lock()
	var target *store.Step
	for _, s := range m.steps {
		if s.ID == step.ID {
			target = s
		}
	}
	switch {
	case target == nil:
		m.mu.Unlock()
		return store.ErrNotFound
	case target.Status != store.StepDone || target.Attempt != step.Attempt:
		m.mu.Unlock()
		return store.ErrStaleAttempt
	}
	target.InputRefs = append(slices.Clone(target.InputRefs), target.OutputRefs...)
	target.Status, target.Feedback, target.OutputRefs, target.Error = store.StepPending, slices.Clone(feedback), nil, nil
	m.mu.Unlock()
	if m.afterReopen != nil {
		return m.afterReopen(step)
	}
	return nil
}

func (m *memStore) SetFindingVerified(_ context.Context, runID, id uuid.UUID, verified bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.findings {
		if m.findings[i].ID == id && m.findings[i].RunID == runID {
			m.findings[i].Verified = verified
			return nil
		}
	}
	return store.ErrNotFound
}

// ClearFindingExplanation has the store's semantics: it clears the
// explanation columns and the verified flag of a finding of the run.
func (m *memStore) ClearFindingExplanation(_ context.Context, runID, id uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.findings {
		if m.findings[i].ID == id && m.findings[i].RunID == runID {
			f := &m.findings[i]
			f.Explanation, f.Action, f.Citations, f.Proposal, f.Verified = nil, nil, nil, nil, false
			m.cleared++
			return nil
		}
	}
	return store.ErrNotFound
}

// SetFindingResolution has the store's semantics: a finding of the run
// and its company gets keys.resolution, and the refs whose artifact it
// doesn't name yet are appended in order.
func (m *memStore) SetFindingResolution(_ context.Context, runID, id uuid.UUID, companyID, resolution string, evidence []store.EvidenceRef) error {
	if resolution == "" || companyID == "" {
		return errors.New("mem: empty resolution or company")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if r, ok := m.runs[runID]; !ok || r.CompanyID != companyID {
		return store.ErrNotFound
	}
	for i := range m.findings {
		f := &m.findings[i]
		if f.ID != id || f.RunID != runID {
			continue
		}
		keys := maps.Clone(f.Keys)
		if keys == nil {
			keys = map[string]string{}
		}
		keys[store.ResolutionKey] = resolution
		f.Keys = keys
		ev := slices.Clone(f.Evidence)
		for _, e := range evidence {
			if !slices.ContainsFunc(ev, func(x store.EvidenceRef) bool { return x.Artifact == e.Artifact }) {
				ev = append(ev, e)
			}
		}
		f.Evidence = ev
		m.resolutions++
		return nil
	}
	return store.ErrNotFound
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
	sha, err := v.st.PutArtifact(ctx, store.ArtifactVerdict, req.RunID, req.StepID,
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

// ---- default checks and variance linking ----

func TestDefaultChecks(t *testing.T) {
	var got []string
	for _, c := range DefaultChecks() {
		got = append(got, c.Name())
	}
	if len(got) != 6 || got[0] != "bankrec" || got[len(got)-1] != checks.TypeVariance {
		t.Fatalf("DefaultChecks %v, want six checks, bankrec first and variance last", got)
	}
	want := []checks.Check{&checks.BankRecCheck{}, &checks.DuplicatePaymentCheck{}, &checks.GSTR2BCheck{},
		&checks.MissingAccrualCheck{}, &checks.PolicyRulesCheck{}, &checks.VarianceCheck{}}
	for i, c := range DefaultChecks() {
		if c.Name() != want[i].Name() {
			t.Errorf("check %d is %s, want %s", i, c.Name(), want[i].Name())
		}
	}
	if got := (&Workflow{}).checks(); len(got) != 1 || got[0].Name() != "bankrec" {
		t.Errorf("a nil Checks runs %d checks, want bank reconciliation only", len(got))
	}
}

func TestRunCloseDoneWithDefaultChecks(t *testing.T) {
	r := newRig(t)
	r.wf.Checks = DefaultChecks()
	r.wf.Explainer, r.wf.Verifier = nil, nil
	var order []string
	r.st.beginHook = func(kind, _ string) error {
		if strings.HasPrefix(kind, store.StepKindCheckPrefix) {
			order = append(order, strings.TrimPrefix(kind, store.StepKindCheckPrefix))
		}
		return nil
	}
	res, err := r.wf.RunClose(t.Context(), synthCompany, synthMonth)
	if err != nil {
		t.Fatalf("RunClose: %v", err)
	}
	var want []string
	for _, c := range DefaultChecks() {
		want = append(want, c.Name())
	}
	if !slices.Equal(order, want) {
		t.Errorf("check steps ran %v, want %v", order, want)
	}
	for _, name := range want {
		if st := r.stepStatuses(res.RunID)[store.StepKindCheckPrefix+name]; !slices.Equal(st, []string{"done"}) {
			t.Errorf("step check.%s %v, want done", name, st)
		}
	}
}

// scriptedCheck returns fixed findings under a fixed name. Each carries
// the evidence of the rig's first bank reconciliation finding, read
// through the step's snapshotting readers so the step can annotate it.
type scriptedCheck struct {
	name string
	fs   []store.Finding
}

func (c *scriptedCheck) Name() string { return c.name }

func (c *scriptedCheck) Run(ctx context.Context, in checks.Inputs) ([]store.Finding, error) {
	real, err := (&checks.BankRecCheck{}).Run(ctx, in)
	if err != nil || len(real) == 0 {
		return nil, fmt.Errorf("scripted check %s: no evidence to borrow: %w", c.name, err)
	}
	out := make([]store.Finding, len(c.fs))
	for i, f := range c.fs {
		f.Keys = maps.Clone(f.Keys)
		f.Evidence = slices.Clone(real[0].Evidence)
		out[i] = f
	}
	return out, nil
}

func linkRig(t *testing.T) *testRig {
	t.Helper()
	r := newRig(t)
	r.wf.Explainer, r.wf.Verifier = nil, nil
	finding := func(typ, account string) store.Finding {
		return store.Finding{Type: typ, Severity: checks.SeverityLow, Title: typ + " " + account,
			Keys: map[string]string{"account": account, "month": synthMonth}}
	}
	r.wf.Checks = []checks.Check{
		&scriptedCheck{name: "rules", fs: []store.Finding{finding(checks.TypeMisclassifiedExpense, "Rent - SYN")}},
		&scriptedCheck{name: checks.TypeVariance, fs: []store.Finding{
			finding(checks.TypeVariance, "Rent - SYN"),
			finding(checks.TypeVariance, "Travel - SYN"),
		}},
	}
	return r
}

// wantLinked checks the run's variances: the Rent one points at the rules
// finding, the Travel one at nothing. It returns the rules finding's ID.
func wantLinked(t *testing.T, fs []store.Finding) uuid.UUID {
	t.Helper()
	var target uuid.UUID
	for _, f := range fs {
		if f.Type == checks.TypeMisclassifiedExpense {
			target = f.ID
		}
	}
	if target == uuid.Nil {
		t.Fatalf("no rules finding stored")
	}
	n := 0
	for _, f := range fs {
		if f.Type != checks.TypeVariance {
			if _, ok := f.Keys[relatedKey]; ok {
				t.Errorf("stored %s finding was rewritten with related", f.Type)
			}
			continue
		}
		n++
		switch f.Keys["account"] {
		case "Rent - SYN":
			if f.Keys[relatedKey] != target.String() {
				t.Errorf("Rent variance related %q, want %s", f.Keys[relatedKey], target)
			}
		default:
			if r, ok := f.Keys[relatedKey]; ok {
				t.Errorf("unmatched variance related %q, want unset", r)
			}
		}
	}
	if n != 2 {
		t.Errorf("%d variance findings stored, want 2", n)
	}
	return target
}

func TestRunCloseLinksVariance(t *testing.T) {
	r := linkRig(t)
	res, err := r.wf.RunClose(t.Context(), synthCompany, synthMonth)
	if err != nil {
		t.Fatalf("RunClose: %v", err)
	}
	before, _ := r.st.ListFindingsByRun(t.Context(), res.RunID)
	if len(before) != 3 {
		t.Fatalf("%d findings, want 3", len(before))
	}
	wantLinked(t, before)

	// A resume skips the finished variance step: nothing is relinked or
	// duplicated.
	r.wf.Explainer, r.wf.Verifier = r.expl, r.ver
	if _, err := r.wf.ResumeClose(t.Context(), res.RunID); err != nil {
		t.Fatalf("ResumeClose: %v", err)
	}
	after, _ := r.st.ListFindingsByRun(t.Context(), res.RunID)
	if !slices.EqualFunc(after, before, func(a, b store.Finding) bool {
		return a.ID == b.ID && a.Keys[relatedKey] == b.Keys[relatedKey]
	}) {
		t.Errorf("findings changed across the resume")
	}
}

func TestRunCloseLinksVarianceAfterKill(t *testing.T) {
	r := linkRig(t)
	killed := false
	r.st.finishHook = func(s store.Step, _ string) error {
		if s.Kind == store.StepKindCheckPrefix+checks.TypeVariance && !killed {
			killed = true
			return errors.New("killed")
		}
		return nil
	}
	if _, err := r.wf.RunClose(t.Context(), synthCompany, synthMonth); err == nil {
		t.Fatalf("RunClose survived the kill")
	}
	var runID uuid.UUID
	for id := range r.st.runs {
		runID = id
	}
	stored, _ := r.st.ListFindingsByRun(t.Context(), runID)
	if len(stored) != 1 {
		t.Fatalf("%d findings stored before the variance step, want 1", len(stored))
	}
	if _, err := r.wf.ResumeClose(t.Context(), runID); err != nil {
		t.Fatalf("ResumeClose: %v", err)
	}
	fs, _ := r.st.ListFindingsByRun(t.Context(), runID)
	if len(fs) != 3 {
		t.Fatalf("%d findings after resume, want 3", len(fs))
	}
	if got := wantLinked(t, fs); got != stored[0].ID {
		t.Errorf("variance linked to %s, want the finding stored before the kill %s", got, stored[0].ID)
	}
}

func TestSortFindingsIgnoresRelated(t *testing.T) {
	mk := func(voucher, related string) store.Finding {
		return store.Finding{ID: uuid.New(), Type: checks.TypeVariance,
			Keys: map[string]string{"account": "Rent - SYN", "month": synthMonth, relatedKey: related, "voucher": voucher}}
	}
	// related sorts before voucher and its values run opposite to it.
	fs := []store.Finding{mk("B", "0000"), mk("A", "ffff")}
	sortFindings(fs)
	if fs[0].Keys["voucher"] != "A" {
		t.Errorf("sortFindings ordered by related: %v first", fs[0].Keys)
	}
}

func TestRunCloseTraces(t *testing.T) {
	rec := telemetrytest.Install(t)
	r := newRig(t)
	r.wf.Model = llmtrace.Provider(r.model)
	const headerSecret = "Authorization=Basic c3VwZXItc2VjcmV0LWtleQ"
	r.wf.Config.OTLPHeaders = config.NewSecret(headerSecret)
	r.ver.useModel = true
	res, err := r.wf.RunClose(t.Context(), synthCompany, synthMonth)
	if err != nil {
		t.Fatalf("RunClose: %v", err)
	}

	seen := map[string]bool{}
	traces := map[string]bool{}
	for _, s := range rec.Ended() {
		name := s.Name()
		if strings.HasPrefix(name, telemetry.SpanCheckPrefix) {
			name = "check.*"
		}
		seen[name] = true
		traces[s.SpanContext().TraceID().String()] = true
	}
	for _, want := range []string{telemetry.SpanCloseRun, "check.*", telemetry.SpanExplain, telemetry.SpanVerify, telemetry.SpanChat} {
		if !seen[want] {
			t.Errorf("span %q missing; saw %v", want, seen)
		}
	}
	if seen[telemetry.SpanInvestigate] {
		t.Error("investigate span emitted before CC-706")
	}
	if len(traces) != 1 {
		t.Errorf("%d traces, want one", len(traces))
	}

	run := r.st.runs[res.RunID]
	if run.TraceID == nil || !traces[*run.TraceID] {
		t.Errorf("stored trace ID %v is not the run's trace %v", run.TraceID, traces)
	}
	md, err := os.ReadFile(res.ReportPath)
	if err != nil {
		t.Fatal(err)
	}
	if run.TraceID != nil && !strings.Contains(string(md), "| Trace | `"+*run.TraceID+"` |") {
		t.Errorf("report lacks the trace ID:\n%s", md)
	}

	gstin := regexp.MustCompile(`\b\d{2}[A-Z]{5}\d{4}[A-Z][1-9A-Z]Z[0-9A-Z]\b`)
	pan := regexp.MustCompile(`\b[A-Z]{5}\d{4}[A-Z]\b`)
	for _, v := range append(telemetrytest.Strings(rec), telemetrytest.EventStrings(rec)...) {
		if gstin.MatchString(v) || pan.MatchString(v) || strings.Contains(v, "c3VwZXItc2VjcmV0LWtleQ") || strings.Contains(v, headerSecret) {
			t.Errorf("span attribute %q holds an identifier or secret", v)
		}
	}
}

func TestRunCloseWithoutTracingHasNoTraceID(t *testing.T) {
	telemetrytest.Off(t)
	r := newRig(t)
	res, err := r.wf.RunClose(t.Context(), synthCompany, synthMonth)
	if err != nil {
		t.Fatal(err)
	}
	if r.st.runs[res.RunID].TraceID != nil {
		t.Error("trace ID stored with tracing off")
	}
	md, _ := os.ReadFile(res.ReportPath)
	if strings.Contains(string(md), "| Trace |") {
		t.Error("report shows a trace row with tracing off")
	}
}

func TestResumeStoresNewTraceID(t *testing.T) {
	telemetrytest.Install(t)
	r := newRig(t)
	res, err := r.wf.RunClose(t.Context(), synthCompany, synthMonth)
	if err != nil {
		t.Fatal(err)
	}
	first := *r.st.runs[res.RunID].TraceID
	// Reopen the run as failed, with usage that a stale write would clobber.
	r.st.mu.Lock()
	r.st.runs[res.RunID].Status = store.RunFailed
	r.st.runs[res.RunID].InputTokens = 777
	r.st.mu.Unlock()
	if _, err := r.wf.ResumeClose(t.Context(), res.RunID); err != nil {
		t.Fatal(err)
	}
	run := r.st.runs[res.RunID]
	if run.TraceID == nil || *run.TraceID == first {
		t.Errorf("resumed run trace ID %v, want a new one (first %s)", run.TraceID, first)
	}
}

// ---- run scope (CC-506) ----

// heldBackEvidence records the deadline of each BankLines call, makes the
// call over MCP (so the server sees the scope), then fails it, so the run
// is refused at preflight and can be resumed.
type heldBackEvidence struct {
	inner     checks.EvidenceReader
	mu        sync.Mutex
	deadlines []time.Time
}

func (h *heldBackEvidence) BankLines(ctx context.Context, company string, from, to time.Time) ([]store.BankLine, error) {
	d, ok := ctx.Deadline()
	if !ok {
		return nil, errors.New("test: the run's context has no deadline")
	}
	h.mu.Lock()
	h.deadlines = append(h.deadlines, d)
	h.mu.Unlock()
	if _, err := h.inner.BankLines(ctx, company, from, to); err != nil {
		return nil, err
	}
	return nil, errors.New("test: evidence held back")
}

func (h *heldBackEvidence) GSTR2BEntries(ctx context.Context, company, period string) ([]store.GSTR2BEntry, error) {
	return h.inner.GSTR2BEntries(ctx, company, period)
}

// TestDriveMintsRunScope: RunClose and ResumeClose each put the run's
// scope on every MCP call: the run's company and window, valid until a
// minute after that drive's deadline. Without it every call would go out
// under the discovery scope and be refused in production.
func TestDriveMintsRunScope(t *testing.T) {
	scoper, err := mcpkit.NewScoper(testScopeKey)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var headers []string
	ev := mcp.NewServer(&mcp.Implementation{Name: "scoped-evidence", Version: "test"}, nil)
	mcp.AddTool(ev, &mcp.Tool{Name: "list_bank_lines", Description: "bank lines", Annotations: ro()},
		func(_ context.Context, req *mcp.CallToolRequest, in rangeArgs) (*mcp.CallToolResult, []wireBankLine, error) {
			mu.Lock()
			headers = append(headers, req.Extra.Header.Get(mcpkit.ScopeHeader))
			mu.Unlock()
			if err := scoper.CheckRange(req, in.Company, in.FromDate, in.ToDate); err != nil {
				return nil, nil, err
			}
			return nil, []wireBankLine{{TxnID: "T1", Date: "2026-08-03", Narration: "NEFT", Amount: -100}}, nil
		})
	mcp.AddTool(ev, &mcp.Tool{Name: "list_gstr2b_entries", Description: "gstr2b", Annotations: ro()},
		func(context.Context, *mcp.CallToolRequest, periodArgs) (*mcp.CallToolResult, []wireGSTR2BEntry, error) {
			return nil, nil, nil
		})
	scoped := mcpkit.WithScoper(scoper, "/mcp")
	books, evidence := startServer(t, fakeBooks(), scoped), startServer(t, ev, scoped)
	cfg := config.Config{
		BooksMCPURL: books.URL + "/mcp", EvidenceMCPURL: evidence.URL + "/mcp",
		MCPTokenAgent: config.NewSecret(testToken), MCPScopeKey: config.NewSecret(testScopeKey),
		LLMRunTokenCap: 100000, LLMDailyBudgetUSD: 2,
	}
	reg, err := NewRegistry(t.Context(), cfg, discardLog())
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	t.Cleanup(reg.Close)

	r := newRig(t)
	held := &heldBackEvidence{inner: &MCPEvidence{Registry: reg}}
	r.wf.Evidence, r.wf.Config = held, cfg

	// verify checks the scope of the n-th call against the run and the
	// n-th recorded deadline.
	verify := func(n int, runID uuid.UUID) int64 {
		t.Helper()
		mu.Lock()
		defer mu.Unlock()
		if len(headers) != n+1 || len(held.deadlines) != n+1 {
			t.Fatalf("%d calls reached the server, %d deadlines recorded; want %d", len(headers), len(held.deadlines), n+1)
		}
		got, err := mcpkit.Verify([]byte(testScopeKey), headers[n], time.Now())
		if err != nil {
			t.Fatalf("call %d: scope does not verify: %v", n, err)
		}
		want, err := mcpkit.Window(synthCompany, runID.String(), synthMonth, held.deadlines[n].Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("call %d: scope %+v, want %+v", n, got, want)
		}
		if got.Exp != held.deadlines[n].Add(time.Minute).Unix() {
			t.Errorf("call %d: exp %d, want the deadline plus a minute", n, got.Exp)
		}
		return got.Exp
	}

	r.wf.Timeout = 9 * time.Minute
	res, err := r.wf.RunClose(t.Context(), synthCompany, synthMonth)
	if !errors.Is(err, ErrPreflightRefused) || res.RunID == uuid.Nil {
		t.Fatalf("RunClose: %+v %v, want a preflight refusal", res, err)
	}
	first := verify(0, res.RunID)

	r.wf.Timeout = 7 * time.Minute // a different deadline, so a fresh exp
	if _, err := r.wf.ResumeClose(t.Context(), res.RunID); !errors.Is(err, ErrPreflightRefused) {
		t.Fatalf("ResumeClose: %v, want a preflight refusal", err)
	}
	if second := verify(1, res.RunID); second == first {
		t.Errorf("the resume reused the first scope's exp %d", first)
	}
}

// scopeCall is one read a detector made: the company and the inclusive
// date range it covers.
type scopeCall struct {
	what, company, from, to string
}

// recordingBooks and recordingEvidence wrap the synthetic readers and
// record the range of every read.
type recordingBooks struct {
	synthBooks
	mu    sync.Mutex
	calls []scopeCall
}

func (b *recordingBooks) add(what, company string, from, to time.Time) {
	b.mu.Lock()
	b.calls = append(b.calls, scopeCall{what, company, from.Format(time.DateOnly), to.Format(time.DateOnly)})
	b.mu.Unlock()
}

func (b *recordingBooks) TrialBalance(ctx context.Context, c string, from, to time.Time) (ledger.TB, error) {
	b.add("trial_balance", c, from, to)
	return b.synthBooks.TrialBalance(ctx, c, from, to)
}

func (b *recordingBooks) GLEntries(ctx context.Context, c string, from, to time.Time) ([]ledger.GLEntry, error) {
	b.add("gl_entries", c, from, to)
	return b.synthBooks.GLEntries(ctx, c, from, to)
}

func (b *recordingBooks) PurchaseInvoices(ctx context.Context, c string, from, to time.Time) ([]ledger.PurchaseInvoice, error) {
	b.add("purchase_invoices", c, from, to)
	return b.synthBooks.PurchaseInvoices(ctx, c, from, to)
}

func (b *recordingBooks) SalesInvoices(ctx context.Context, c string, from, to time.Time) ([]ledger.SalesInvoice, error) {
	b.add("sales_invoices", c, from, to)
	return b.synthBooks.SalesInvoices(ctx, c, from, to)
}

func (b *recordingBooks) PaymentEntries(ctx context.Context, c string, from, to time.Time) ([]ledger.PaymentEntry, error) {
	b.add("payments", c, from, to)
	return b.synthBooks.PaymentEntries(ctx, c, from, to)
}

// AccountHistory reads months-1 months before through to through's end.
func (b *recordingBooks) AccountHistory(ctx context.Context, c, account, through string, months int) ([]ledger.MonthTotal, error) {
	last, err := time.Parse("2006-01", through)
	if err != nil {
		return nil, err
	}
	b.add("account_history", c, last.AddDate(0, -(months-1), 0), last.AddDate(0, 1, -1))
	return b.synthBooks.AccountHistory(ctx, c, account, through, months)
}

// RecurringSuppliers reads the lookback months before before_month (the
// books tool's RecurringWindow, computed here: internal/agent must not
// import internal/books).
func (b *recordingBooks) RecurringSuppliers(ctx context.Context, c, before string, lookback, minOcc, band int) ([]checks.RecurringSupplier, error) {
	m, err := time.Parse("2006-01", before)
	if err != nil {
		return nil, err
	}
	b.add("recurring_suppliers", c, m.AddDate(0, -lookback, 0), m.AddDate(0, 0, -1))
	return b.synthBooks.RecurringSuppliers(ctx, c, before, lookback, minOcc, band)
}

type recordingEvidence struct {
	synthEvidence
	books *recordingBooks // shares the call log
}

func (e *recordingEvidence) BankLines(ctx context.Context, c string, from, to time.Time) ([]store.BankLine, error) {
	e.books.add("bank_lines", c, from, to)
	return e.synthEvidence.BankLines(ctx, c, from, to)
}

// GSTR2BEntries reads the first to the last day of the period.
func (e *recordingEvidence) GSTR2BEntries(ctx context.Context, c, period string) ([]store.GSTR2BEntry, error) {
	m, err := time.Parse("2006-01", period)
	if err != nil {
		return nil, err
	}
	e.books.add("gstr2b_entries", c, m, m.AddDate(0, 1, -1))
	return e.synthEvidence.GSTR2BEntries(ctx, c, period)
}

// TestScopedCloseRunAllChecks runs a close with every default check and
// requires every read the detectors make to sit inside the run's scope
// window, so the scoped servers never refuse a real run. A future detector
// that reads outside the window breaks this test.
func TestScopedCloseRunAllChecks(t *testing.T) {
	r := newRig(t)
	books := &recordingBooks{}
	r.wf.Books, r.wf.Evidence = books, &recordingEvidence{books: books}
	r.wf.Checks = DefaultChecks()
	r.wf.Explainer, r.wf.Verifier = nil, nil
	res, err := r.wf.RunClose(t.Context(), synthCompany, synthMonth)
	if err != nil {
		t.Fatalf("RunClose: %v", err)
	}
	sc, err := mcpkit.Window(synthCompany, res.RunID.String(), synthMonth, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	books.mu.Lock()
	defer books.mu.Unlock()
	kinds := map[string]bool{}
	for _, c := range books.calls {
		kinds[c.what] = true
		if err := sc.Allows(c.company, c.from, c.to); err != nil {
			t.Errorf("%s %s %s..%s: %v", c.what, c.company, c.from, c.to, err)
		}
	}
	for _, k := range []string{"bank_lines", "trial_balance", "gl_entries", "purchase_invoices", "gstr2b_entries", "recurring_suppliers", "payments"} {
		if !kinds[k] {
			t.Errorf("no %s read recorded; the test no longer covers it (reads: %v)", k, slices.Sorted(maps.Keys(kinds)))
		}
	}
	gstr2b := (&checks.GSTR2BCheck{}).Name()
	if st := r.stepStatuses(res.RunID)[store.StepKindCheckPrefix+gstr2b]; !slices.Equal(st, []string{"done"}) {
		t.Errorf("GSTR-2B check step %v, want done", st)
	}
}

// ---- investigating (CC-706) ----

// routeModel sends the investigator's calls (those offering
// emit_resolution) to investigate and every other call to explain.
type routeModel struct {
	explain, investigate llm.Provider
}

func (m routeModel) Complete(ctx context.Context, req llm.Request) (llm.Response, error) {
	if slices.ContainsFunc(req.Tools, func(s llm.ToolSpec) bool { return s.Name == InvestigateToolName }) {
		return m.investigate.Complete(ctx, req)
	}
	return m.explain.Complete(ctx, req)
}

// synthGSTIN is a made-up, well-formed GSTIN a tool result carries.
const synthGSTIN = "27AABCV5678B1Z8"

const wfTimingArgs = `{"resolution_type":"timing_difference","explanation":"The customer's receipt is booked in the next days.","evidence_ids":["TXN-0004"]}`

// newInvestigateRig is the workflow rig with the LLMInvestigator: its
// model answers after one GSTR-2B call.
func newInvestigateRig(t *testing.T, script ...llm.ScriptedCall) (*testRig, *llm.FakeProvider, *fakeTools) {
	t.Helper()
	r := newRig(t)
	if len(script) == 0 {
		script = []llm.ScriptedCall{
			turn(1000, toolCall("g1", "evidence__list_gstr2b_entries", `{"company":"testco","period":"2026-08"}`)),
			turn(1000, emitCall(wfTimingArgs)),
		}
	}
	inv := llm.NewFakeProvider(script...)
	tools := newFakeTools()
	tools.results["evidence__list_gstr2b_entries"] = `[{"supplier_gstin":"` + synthGSTIN + `","invoice_no":"INV-1","taxable":100000}]`
	r.wf.Model = routeModel{explain: r.model, investigate: inv}
	r.wf.Investigator = &LLMInvestigator{Store: r.st, Tools: tools, Accounts: &staticAccounts{list: []string{synthBank, "Sales - TC"}}, Model: invModel, Log: discardLog()}
	key := []byte("0123456789abcdef0123456789abcdef")
	r.wf.NewRedactor = func(runID uuid.UUID) *Redactor { return NewRedactor(key, runID, nil) }
	return r, inv, tools
}

func unmatchedBankLine(t *testing.T, r *testRig, runID uuid.UUID) store.Finding {
	t.Helper()
	fs, _ := r.st.ListFindingsByRun(t.Context(), runID)
	for _, f := range fs {
		if f.Type == checks.TypeUnmatchedBankLine {
			return f
		}
	}
	t.Fatal("no unmatched_bank_line finding")
	return store.Finding{}
}

func TestWorkflowInvestigatesUnmatchedBankLines(t *testing.T) {
	r, inv, tools := newInvestigateRig(t)
	res, err := r.wf.RunClose(t.Context(), synthCompany, synthMonth)
	if err != nil {
		t.Fatalf("RunClose: %v", err)
	}
	if res.Status != store.RunDone {
		t.Fatalf("result %+v", res)
	}
	wantSteps(t, r.stepStatuses(res.RunID), map[string][]string{
		"check.bankrec": {"done"},
		"explain":       repeat("done", 4),
		"investigate":   {"done"},
		"retrieve":      {"skipped"},
		"router":        {"done"},
		"synthesize":    {"done"},
		"verify":        repeat("done", 5),
	})
	f := unmatchedBankLine(t, r, res.RunID)
	if f.Keys[store.ResolutionKey] != ResolutionTimingDifference || f.Status != checks.StatusOpen {
		t.Errorf("finding keys %v status %s", f.Keys, f.Status)
	}
	if n := len(f.Evidence); n != 2 || f.Evidence[1].Tool != toolListGSTR2BEntries {
		t.Errorf("evidence %+v", f.Evidence)
	}
	if got := tools.executedNames(); !slices.Equal(got, []string{"evidence__list_gstr2b_entries"}) {
		t.Errorf("executed %v", got)
	}

	// The investigate step's output is verified under its own subject.
	steps := r.st.stepsByKind(res.RunID)
	istep := steps["investigate"][0]
	if istep.Subject != f.ID.String() || len(istep.OutputRefs) != 1 || istep.Error != nil {
		t.Fatalf("investigate step %+v", istep)
	}
	var vstep store.Step
	for _, s := range steps["verify"] {
		if s.Subject == investigateVerifyPrefix+f.ID.String() {
			vstep = s
		}
	}
	if vstep.ID == uuid.Nil || vstep.Status != store.StepDone || vstep.Error != nil || len(vstep.OutputRefs) != 1 {
		t.Fatalf("investigation verify step %+v", vstep)
	}
	vb, _, _ := r.st.getAny(vstep.OutputRefs[0])
	if !strings.Contains(string(vb), istep.OutputRefs[0]) {
		t.Errorf("verdict %s does not name the investigation", vb)
	}

	// Both investigator calls are recorded against the investigate step,
	// and identifiers are pseudonymised before the model and the recorder.
	var invCalls int
	for _, c := range r.st.runCalls(res.RunID) {
		if c.StepID == istep.ID {
			invCalls++
			p, _, _ := r.st.getAny(c.PromptSHA256)
			if strings.Contains(string(p), synthGSTIN) {
				t.Error("a recorded investigator prompt holds a raw GSTIN")
			}
		}
	}
	if invCalls != 2 || inv.CallCount() != 2 {
		t.Errorf("investigator calls recorded %d, made %d; want 2", invCalls, inv.CallCount())
	}
	second := inv.Calls()[1]
	tr := second.Messages[len(second.Messages)-1].ToolResults
	if len(tr) != 1 || strings.Contains(tr[0].Content, synthGSTIN) || !strings.Contains(tr[0].Content, "GSTIN_") {
		t.Errorf("tool result reached the model unmasked: %+v", tr)
	}
}

func TestWorkflowInvestigateBreachNeedsReview(t *testing.T) {
	gl := toolCall("c", "books__get_trial_balance", `{"company":"testco","from_date":"2026-08-01","to_date":"2026-08-31"}`)
	r, inv, _ := newInvestigateRig(t, turn(100, gl))
	inv.WithFallback(turn(100, gl).Response)
	res, err := r.wf.RunClose(t.Context(), synthCompany, synthMonth)
	if err != nil || res.Status != store.RunDone {
		t.Fatalf("result %+v, %v", res, err)
	}
	f := unmatchedBankLine(t, r, res.RunID)
	if f.Status != checks.StatusNeedsReview || f.Keys[store.ResolutionKey] != ResolutionUnknown {
		t.Errorf("finding status %s keys %v", f.Status, f.Keys)
	}
	steps := r.st.stepsByKind(res.RunID)
	if s := steps["investigate"][0]; s.Status != store.StepDone || s.Error == nil || *s.Error != reasonLimitCalls {
		t.Errorf("investigate step %+v", s)
	}
	for _, s := range steps["verify"] {
		if s.Subject == investigateVerifyPrefix+f.ID.String() && (s.Status != store.StepSkipped || *s.Error != reasonNoResolution) {
			t.Errorf("investigation verify step %+v", s)
		}
	}
}

func TestWorkflowInvestigateFailedVerificationNeedsReview(t *testing.T) {
	r, _, _ := newInvestigateRig(t)
	r.ver.reviewType = checks.TypeUnmatchedBankLine // the fake verifier flags it
	res, err := r.wf.RunClose(t.Context(), synthCompany, synthMonth)
	if err != nil || res.Status != store.RunDone {
		t.Fatalf("result %+v, %v", res, err)
	}
	if f := unmatchedBankLine(t, r, res.RunID); f.Status != checks.StatusNeedsReview {
		t.Errorf("status %s", f.Status)
	}
}

func TestWorkflowInvestigateRefusedWithoutRedactor(t *testing.T) {
	r, inv, _ := newInvestigateRig(t)
	r.wf.NewRedactor = nil
	res, err := r.wf.RunClose(t.Context(), synthCompany, synthMonth)
	if err != nil {
		t.Fatal(err)
	}
	if inv.CallCount() != 0 {
		t.Errorf("investigator model called %d times without a redactor", inv.CallCount())
	}
	s := r.st.stepsByKind(res.RunID)["investigate"]
	if len(s) != 1 || s[0].Subject != SubjectClose || s[0].Status != store.StepSkipped || *s[0].Error != reasonNoRedactor {
		t.Errorf("investigate steps %+v", s)
	}
}

func TestWorkflowInvestigateSkippedWithoutTargets(t *testing.T) {
	r, inv, _ := newInvestigateRig(t)
	r.wf.Checks = []checks.Check{&checks.DuplicatePaymentCheck{}}
	res, err := r.wf.RunClose(t.Context(), synthCompany, synthMonth)
	if err != nil {
		t.Fatal(err)
	}
	s := r.st.stepsByKind(res.RunID)["investigate"]
	if len(s) != 1 || s[0].Status != store.StepSkipped || *s[0].Error != reasonNothingToInvestigate || inv.CallCount() != 0 {
		t.Errorf("investigate steps %+v", s)
	}
}

// TestWorkflowInvestigateResume: a crash after the investigation ran but
// before its step ended re-runs it on resume; the trail is not added twice.
func TestWorkflowInvestigateResume(t *testing.T) {
	first := []llm.ScriptedCall{
		turn(1000, toolCall("g1", "evidence__list_gstr2b_entries", `{"company":"testco","period":"2026-08"}`)),
		turn(1000, emitCall(wfTimingArgs)),
	}
	r, inv, tools := newInvestigateRig(t, slices.Concat(first, first)...)
	errCrash := errors.New("synthetic crash")
	crashed := false
	r.st.finishHook = func(s store.Step, _ string) error {
		if s.Kind == store.StepKindInvestigate && !crashed {
			crashed = true
			return errCrash
		}
		return nil
	}
	res, err := r.wf.RunClose(t.Context(), synthCompany, synthMonth)
	if !errors.Is(err, errCrash) || res.Status != store.RunFailed {
		t.Fatalf("first run %+v, %v", res, err)
	}
	f := unmatchedBankLine(t, r, res.RunID)
	if len(f.Evidence) != 2 {
		t.Fatalf("evidence after the crash %d", len(f.Evidence))
	}

	res, err = r.wf.ResumeClose(t.Context(), res.RunID)
	if err != nil || res.Status != store.RunDone {
		t.Fatalf("resume %+v, %v", res, err)
	}
	istep := r.st.stepsByKind(res.RunID)["investigate"][0]
	if istep.Attempt != 2 || istep.Status != store.StepDone || len(istep.OutputRefs) != 1 {
		t.Errorf("investigate step %+v", istep)
	}
	f = unmatchedBankLine(t, r, res.RunID)
	if len(f.Evidence) != 2 || f.Keys[store.ResolutionKey] != ResolutionTimingDifference {
		t.Errorf("finding after resume: evidence %d keys %v", len(f.Evidence), f.Keys)
	}
	if inv.CallCount() != 4 || len(tools.executedNames()) != 2 {
		t.Errorf("model calls %d, tool calls %d", inv.CallCount(), len(tools.executedNames()))
	}
	// A resume after the run is done re-runs nothing.
	if _, err := r.wf.ResumeClose(t.Context(), res.RunID); !errors.Is(err, ErrRunDone) {
		t.Errorf("second resume: %v", err)
	}
}
