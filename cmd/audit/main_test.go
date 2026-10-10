package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/abhishekjha/close-copilot/internal/agent"
	"github.com/abhishekjha/close-copilot/internal/checks"
	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/money"
	"github.com/abhishekjha/close-copilot/internal/store"
)

func TestParseArgs(t *testing.T) {
	id := uuid.MustParse("6f1c2a52-7d1b-4c1e-9d1a-2b3c4d5e6f70")
	got, err := parseArgs([]string{"rebuild", id.String()})
	if err != nil || got != (command{name: cmdRebuild, findingID: id}) {
		t.Errorf("rebuild: %+v, %v", got, err)
	}
	for _, args := range [][]string{
		nil,
		{"rebuild"},
		{"rebuild", "not-a-uuid"},
		{"rebuild", uuid.Nil.String()},
		{"rebuild", id.String(), "extra"},
		{"replay", id.String()},
	} {
		if _, err := parseArgs(args); !errors.Is(err, errUsage) {
			t.Errorf("parseArgs(%q): %v, want a usage error", args, err)
		}
	}
}

// memAudit is an in-memory auditStore that also stores artifacts, so the
// verifier can write the verdict the test then audits.
type memAudit struct {
	mu        sync.Mutex
	artifacts map[string]store.Artifact
	findings  map[uuid.UUID]store.Finding
	runs      map[uuid.UUID]store.CloseRun
	steps     map[uuid.UUID][]store.Step
	calls     map[uuid.UUID][]store.LLMCall
}

func newMemAudit() *memAudit {
	return &memAudit{
		artifacts: map[string]store.Artifact{}, findings: map[uuid.UUID]store.Finding{},
		runs: map[uuid.UUID]store.CloseRun{}, steps: map[uuid.UUID][]store.Step{}, calls: map[uuid.UUID][]store.LLMCall{},
	}
}

func (m *memAudit) PutArtifact(_ context.Context, kind string, _, _ uuid.UUID, v any) (string, error) {
	b, sha, err := store.CanonicalHash(v)
	if err != nil {
		return "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.artifacts[sha]; !ok {
		m.artifacts[sha] = store.Artifact{SHA256: sha, Kind: kind, Content: b}
	}
	return sha, nil
}

func (m *memAudit) GetArtifact(_ context.Context, sha string) (store.Artifact, error) {
	m.mu.Lock()
	a, ok := m.artifacts[sha]
	m.mu.Unlock()
	if !ok {
		return store.Artifact{}, fmt.Errorf("%w: artifact %s", store.ErrNotFound, sha)
	}
	b, got, err := store.CanonicalHash(a.Content)
	if err != nil {
		return store.Artifact{}, err
	}
	if got != sha {
		return store.Artifact{}, fmt.Errorf("%w: %s", store.ErrArtifactMismatch, sha)
	}
	a.Content = b
	return a, nil
}

func (m *memAudit) CitationExists(context.Context, string, string, string) (bool, error) {
	return false, nil
}

func (m *memAudit) GetFinding(_ context.Context, id uuid.UUID) (store.Finding, error) {
	f, ok := m.findings[id]
	if !ok {
		return store.Finding{}, fmt.Errorf("%w: finding %s", store.ErrNotFound, id)
	}
	return f, nil
}

func (m *memAudit) GetCloseRun(_ context.Context, id uuid.UUID) (store.CloseRun, error) {
	r, ok := m.runs[id]
	if !ok {
		return store.CloseRun{}, store.ErrNotFound
	}
	return r, nil
}

func (m *memAudit) ListSteps(_ context.Context, runID uuid.UUID) ([]store.Step, error) {
	return m.steps[runID], nil
}

func (m *memAudit) ListLLMCalls(_ context.Context, stepID uuid.UUID) ([]store.LLMCall, error) {
	return m.calls[stepID], nil
}

// auditFixture is one synthetic finding with a verified explanation.
type auditFixture struct {
	st                    *memAudit
	finding               uuid.UUID
	explainStep, verifyID uuid.UUID
	verdict               string
}

func newAuditFixture(t *testing.T) *auditFixture {
	t.Helper()
	ctx := t.Context()
	st := newMemAudit()
	runID, findingID := uuid.New(), uuid.New()
	put := func(kind string, v any) string {
		sha, err := st.PutArtifact(ctx, kind, uuid.Nil, uuid.Nil, v)
		if err != nil {
			t.Fatal(err)
		}
		return sha
	}
	evidence := put(store.ArtifactToolResult, map[string]any{
		"server": "evidence", "tool": "list_bank_lines",
		"args":   map[string]any{"company": "sharma", "from_date": "2026-09-01", "to_date": "2026-09-30"},
		"result": []map[string]any{{"txn_id": "HDFC-20260915-C1", "narration": "SMS CHGS", "amount_paise": -590}},
	})
	accounts := put(store.ArtifactToolResult, map[string]any{
		"server": "books", "tool": "get_trial_balance",
		"args":   map[string]any{"company": "sharma", "from_date": "2026-09-01", "to_date": "2026-09-30"},
		"result": map[string]any{"rows": []map[string]any{{"account": "Bank Charges - STPL"}, {"account": "HDFC Current 0001 - STPL"}}},
	})
	art := agent.ExplanationArtifact{
		RunID: runID.String(), FindingID: findingID.String(), Model: "claude-haiku-5-5",
		Explanation: "The bank debited ₹5.90 that the books lack.", SuggestedAction: "book_entry", ActionNote: "Book it.",
		CitedAmountsPaise: []money.Paise{590}, Citations: []store.Citation{},
		Proposal: &store.JournalPayload{PostingDate: "2026-09-15", Remark: "SMS charges", Lines: []store.JournalLine{
			{Account: "Bank Charges - STPL", DebitPaise: 590}, {Account: "HDFC Current 0001 - STPL", CreditPaise: 590}}},
		AccountsRef: accounts, DocumentRefs: []string{},
	}
	explanation := put(store.ArtifactExplanation, art)
	amt := money.Paise(590)
	// The findings row as the explainer and the workflow leave it.
	text, action := art.Explanation, art.SuggestedAction
	f := store.Finding{ID: findingID, RunID: runID, Type: "unrecorded_bank_charge", Severity: "low", Title: "SMS CHGS", AmountPaise: &amt,
		Evidence:    []store.EvidenceRef{{Server: "evidence", Tool: "list_bank_lines", IDs: []string{"HDFC-20260915-C1"}, Artifact: evidence}},
		Explanation: &text, Action: &action, Citations: []store.Citation{},
		Proposal: &store.JournalProposal{FindingID: &findingID, CompanyID: "sharma", Payload: *art.Proposal, Status: "proposed"},
		Verified: true, Status: "open"}
	st.findings[findingID] = f
	st.runs[runID] = store.CloseRun{ID: runID, CompanyID: "sharma", Month: "2026-09", Status: store.RunDone}

	explainStep, verifyStep := uuid.New(), uuid.New()
	v := &agent.CodeVerifier{Store: st, Citations: st}
	res, err := v.Verify(ctx, agent.VerifyRequest{
		RunID: runID, StepID: verifyStep, Company: "sharma", Month: "2026-09", Finding: f,
		EvidenceRefs: []string{evidence}, ExplanationRefs: []string{explanation}, ExplainAttempt: 1,
	})
	if err != nil || len(res.Violations) != 0 {
		t.Fatalf("verify: %v %s", err, res.Violations)
	}
	st.steps[runID] = []store.Step{
		{ID: explainStep, RunID: runID, Kind: store.StepKindExplain, Subject: findingID.String(), Status: store.StepDone, Attempt: 1, OutputRefs: []string{explanation}},
		{ID: verifyStep, RunID: runID, Kind: store.StepKindVerify, Subject: findingID.String(), Status: store.StepDone, Attempt: 1, OutputRefs: res.OutputRefs},
	}
	st.calls[explainStep] = []store.LLMCall{{
		RunID: runID, StepID: explainStep, Model: "claude-haiku-5-5-20261001",
		PromptSHA256:   put(store.ArtifactPrompt, map[string]any{"system": "rules", "user": "<<<FINDING ..."}),
		ResponseSHA256: put(store.ArtifactResponse, map[string]any{"tool_calls": []any{}}),
		InputTokens:    900, OutputTokens: 120, CostUSD: "0.0012",
	}}
	return &auditFixture{st: st, finding: findingID, explainStep: explainStep, verifyID: verifyStep, verdict: res.OutputRefs[0]}
}

func (fx *auditFixture) run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	load := func(...string) (config.Config, error) { return config.Config{}, nil }
	open := func(context.Context, config.Config) (auditStore, func(), error) { return fx.st, func() {}, nil }
	code := run(t.Context(), args, &stdout, &stderr, load, open)
	return code, stdout.String(), stderr.String()
}

func TestRebuildMatches(t *testing.T) {
	fx := newAuditFixture(t)
	code, out, errOut := fx.run(t, "rebuild", fx.finding.String())
	if code != exitMatch {
		t.Fatalf("exit %d, want 0\nstdout:\n%s\nstderr: %s", code, out, errOut)
	}
	for _, want := range []string{
		"== evidence (as sent to the model)", `"HDFC-20260915-C1"`,
		"== model call 1 of 1: claude-haiku-5-5-20261001, 900 input tokens", "-- prompt ", "-- response ",
		"== explanation ", "The bank debited ₹5.90",
		"== stored verdict " + fx.verdict, "== re-computed verdict " + fx.verdict, "== result: match",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestRebuildMismatch(t *testing.T) {
	t.Run("a tampered verdict row", func(t *testing.T) {
		fx := newAuditFixture(t)
		a := fx.st.artifacts[fx.verdict]
		a.Content = json.RawMessage(strings.Replace(string(a.Content), `"pass":true`, `"pass":false`, 1))
		fx.st.artifacts[fx.verdict] = a
		if code, out, _ := fx.run(t, "rebuild", fx.finding.String()); code != exitMismatch {
			t.Errorf("exit %d, want 1\n%s", code, out)
		}
	})
	t.Run("a verify step pointing at another verdict", func(t *testing.T) {
		fx := newAuditFixture(t)
		other, err := fx.st.PutArtifact(t.Context(), store.ArtifactVerdict, uuid.Nil, uuid.Nil, map[string]any{"pass": true})
		if err != nil {
			t.Fatal(err)
		}
		for runID, steps := range fx.st.steps {
			for i := range steps {
				if steps[i].Kind == store.StepKindVerify {
					fx.st.steps[runID][i].OutputRefs = []string{other}
				}
			}
		}
		code, out, _ := fx.run(t, "rebuild", fx.finding.String())
		if code != exitMismatch || !strings.Contains(out, "== result: MISMATCH") {
			t.Errorf("exit %d, want 1\n%s", code, out)
		}
	})
	t.Run("a verify output that is not a verdict", func(t *testing.T) {
		fx := newAuditFixture(t)
		for runID, steps := range fx.st.steps {
			for i := range steps {
				if steps[i].Kind == store.StepKindVerify {
					fx.st.steps[runID][i].OutputRefs = steps[0].OutputRefs
				}
			}
		}
		if code, _, _ := fx.run(t, "rebuild", fx.finding.String()); code != exitMismatch {
			t.Errorf("exit %d, want 1", code)
		}
	})
}

// TestRebuildRowMismatch: a findings row that differs from the stored
// records makes the rebuild exit 1.
func TestRebuildRowMismatch(t *testing.T) {
	for _, tt := range []struct {
		name   string
		tamper func(*store.Finding)
		want   string
	}{
		{"verified cleared", func(f *store.Finding) { f.Verified = false }, "verified is false"},
		{"explanation edited", func(f *store.Finding) { s := "Edited."; f.Explanation = &s }, "explanation"},
		{"explanation removed", func(f *store.Finding) { f.Explanation = nil }, "explanation"},
		{"action edited", func(f *store.Finding) { s := "no_action"; f.Action = &s }, "action"},
		{"citation added", func(f *store.Finding) { f.Citations = []store.Citation{{DocID: "POL", Section: "1"}} }, "citations"},
		{"proposal removed", func(f *store.Finding) { f.Proposal = nil }, "proposal"},
		{"proposal amount edited", func(f *store.Finding) {
			p := *f.Proposal
			p.Payload.Lines = slices.Clone(p.Payload.Lines)
			p.Payload.Lines[0].DebitPaise = 9999
			f.Proposal = &p
		}, "proposal"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fx := newAuditFixture(t)
			f := fx.st.findings[fx.finding]
			tt.tamper(&f)
			fx.st.findings[fx.finding] = f
			code, out, errOut := fx.run(t, "rebuild", fx.finding.String())
			if code != exitMismatch || !strings.Contains(out, "== result: MISMATCH") || !strings.Contains(errOut, tt.want) {
				t.Errorf("exit %d, want 1 naming %q\n%s\n%s", code, tt.want, out, errOut)
			}
		})
	}
	t.Run("nil and empty citations are equal", func(t *testing.T) {
		fx := newAuditFixture(t)
		f := fx.st.findings[fx.finding]
		f.Citations = nil
		fx.st.findings[fx.finding] = f
		if code, out, _ := fx.run(t, "rebuild", fx.finding.String()); code != exitMatch {
			t.Errorf("exit %d, want 0\n%s", code, out)
		}
	})
	t.Run("a done explain step without a verdict", func(t *testing.T) {
		fx := newAuditFixture(t)
		for runID, steps := range fx.st.steps {
			for i := range steps {
				if steps[i].Kind == store.StepKindVerify {
					fx.st.steps[runID][i].OutputRefs = nil
				}
			}
		}
		if code, _, _ := fx.run(t, "rebuild", fx.finding.String()); code != exitMismatch {
			t.Errorf("exit %d, want 1", code)
		}
		for runID, steps := range fx.st.steps {
			fx.st.steps[runID] = steps[:1]
		}
		if code, _, _ := fx.run(t, "rebuild", fx.finding.String()); code != exitMismatch {
			t.Errorf("no verify step: exit %d, want 1", code)
		}
	})
}

func TestRebuildErrors(t *testing.T) {
	fx := newAuditFixture(t)
	if code, _, _ := fx.run(t, "rebuild"); code != exitError {
		t.Errorf("usage: exit %d, want 2", code)
	}
	if code, _, _ := fx.run(t, "rebuild", uuid.NewString()); code != exitError {
		t.Errorf("unknown finding: exit %d, want 2", code)
	}
	var stdout, stderr bytes.Buffer
	failLoad := func(...string) (config.Config, error) { return config.Config{}, errors.New("missing DATABASE_URL") }
	if code := run(t.Context(), []string{"rebuild", fx.finding.String()}, &stdout, &stderr, failLoad, nil); code != exitError {
		t.Errorf("config error: exit %d, want 2", code)
	}
	okLoad := func(...string) (config.Config, error) { return config.Config{}, nil }
	failOpen := func(context.Context, config.Config) (auditStore, func(), error) {
		return nil, nil, errors.New("no database")
	}
	if code := run(t.Context(), []string{"rebuild", fx.finding.String()}, &stdout, &stderr, okLoad, failOpen); code != exitError {
		t.Errorf("store error: exit %d, want 2", code)
	}
	// A finding whose explain step isn't done can't be rebuilt.
	for runID, steps := range fx.st.steps {
		fx.st.steps[runID] = steps[1:]
	}
	if code, _, _ := fx.run(t, "rebuild", fx.finding.String()); code != exitError {
		t.Errorf("no explain step: exit %d, want 2", code)
	}
}

// rejectedExplanation stores an explanation of the fixture's finding with
// an invented amount and its failing verdict for the given explain
// attempt, and returns both addresses.
func (fx *auditFixture) rejectedExplanation(t *testing.T, attempt int, text string) (string, string) {
	t.Helper()
	ctx := t.Context()
	f := fx.st.findings[fx.finding]
	var explSha string
	for _, s := range fx.st.steps[f.RunID] {
		if s.Kind == store.StepKindExplain {
			explSha = s.OutputRefs[0]
		}
	}
	a, err := fx.st.GetArtifact(ctx, explSha)
	if err != nil {
		t.Fatal(err)
	}
	var art agent.ExplanationArtifact
	if err := json.Unmarshal(a.Content, &art); err != nil {
		t.Fatal(err)
	}
	art.Explanation = text
	bad, err := fx.st.PutArtifact(ctx, store.ArtifactExplanation, uuid.Nil, uuid.Nil, art)
	if err != nil {
		t.Fatal(err)
	}
	run := fx.st.runs[f.RunID]
	v := &agent.CodeVerifier{Store: fx.st, Citations: fx.st}
	res, err := v.Verify(ctx, agent.VerifyRequest{
		RunID: run.ID, StepID: fx.verifyID, Company: run.CompanyID, Month: run.Month, Finding: f,
		EvidenceRefs: agent.EvidenceRefs(f), ExplanationRefs: []string{bad}, ExplainAttempt: attempt,
	})
	if err != nil || len(res.Violations) == 0 {
		t.Fatalf("verify of a rejected explanation: %v, violations %s", err, res.Violations)
	}
	return bad, res.OutputRefs[0]
}

// setSteps replaces the fixture's explain and verify steps.
func (fx *auditFixture) setSteps(explain, verify func(*store.Step)) {
	runID := fx.st.findings[fx.finding].RunID
	for i := range fx.st.steps[runID] {
		s := &fx.st.steps[runID][i]
		switch s.Kind {
		case store.StepKindExplain:
			explain(s)
		case store.StepKindVerify:
			verify(s)
		}
	}
}

func strPtr(s string) *string { return &s }

// finalFailure turns the fixture into a finding whose third explanation
// failed verification: needs_review, the explain step done on attempt 3,
// and the verify step done with the failing verdict and an error.
func finalFailure(t *testing.T) *auditFixture {
	t.Helper()
	fx := newAuditFixture(t)
	bad, verdict := fx.rejectedExplanation(t, agent.MaxExplainAttempts, "The bank debited ₹9,87,654.32 that the books lack.")
	fx.setSteps(func(s *store.Step) {
		s.Attempt, s.OutputRefs = agent.MaxExplainAttempts, []string{bad}
	}, func(s *store.Step) {
		s.Attempt, s.OutputRefs, s.Error = agent.MaxExplainAttempts, []string{verdict}, strPtr("amount_not_in_evidence after 3 explain attempts")
	})
	f := fx.st.findings[fx.finding]
	text := "The bank debited ₹9,87,654.32 that the books lack."
	f.Explanation, f.Verified, f.Status = &text, false, checks.StatusNeedsReview
	fx.st.findings[fx.finding] = f
	return fx
}

// TestRebuildStepState: the finding's status and the verify step must
// match the verdict (R4).
func TestRebuildStepState(t *testing.T) {
	t.Run("a final failure is consistent", func(t *testing.T) {
		fx := finalFailure(t)
		if code, out, errOut := fx.run(t, "rebuild", fx.finding.String()); code != exitMatch {
			t.Fatalf("exit %d, want 0\n%s\n%s", code, out, errOut)
		}
	})
	for _, tt := range []struct {
		name   string
		tamper func(*auditFixture)
		want   string
	}{
		{"a final failure whose finding is open", func(fx *auditFixture) {
			f := fx.st.findings[fx.finding]
			f.Status = "open"
			fx.st.findings[fx.finding] = f
		}, "status is"},
		{"a final failure whose verify step has no error", func(fx *auditFixture) {
			fx.setSteps(func(*store.Step) {}, func(s *store.Step) { s.Error = nil })
		}, "verify step has no error"},
		{"a failing verdict before the last attempt", func(fx *auditFixture) {
			bad, verdict := fx.rejectedExplanation(t, 2, "The bank debited ₹9,87,654.32 that the books lack.")
			fx.setSteps(func(s *store.Step) { s.Attempt, s.OutputRefs = 2, []string{bad} },
				func(s *store.Step) { s.OutputRefs = []string{verdict} })
		}, "explain attempt 2"},
		{"a final failure whose verify step is not done", func(fx *auditFixture) {
			fx.setSteps(func(*store.Step) {}, func(s *store.Step) { s.Status = store.StepFailed })
		}, "not done"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fx := finalFailure(t)
			tt.tamper(fx)
			code, out, errOut := fx.run(t, "rebuild", fx.finding.String())
			if code != exitMismatch || !strings.Contains(errOut, tt.want) {
				t.Errorf("exit %d, want 1 naming %q\n%s\n%s", code, tt.want, out, errOut)
			}
		})
	}
	t.Run("a passing verdict whose verify step has an error", func(t *testing.T) {
		fx := newAuditFixture(t)
		fx.setSteps(func(*store.Step) {}, func(s *store.Step) { s.Error = strPtr("verification failed") })
		code, out, errOut := fx.run(t, "rebuild", fx.finding.String())
		if code != exitMismatch || !strings.Contains(errOut, "verify step has an error") {
			t.Errorf("exit %d, want 1\n%s\n%s", code, out, errOut)
		}
	})
}

// rejectedRetryFixture turns the fixture into a finding whose third
// explain attempt errored after two failed verdicts: the explain step
// failed with feedback and both rejected explanations in input_refs, the
// verify step skipped with both verdicts in input_refs, the finding in
// needs_review with its explanation cleared.
func rejectedRetryFixture(t *testing.T) (*auditFixture, string) {
	t.Helper()
	fx := newAuditFixture(t)
	bad1, verdict1 := fx.rejectedExplanation(t, 1, "The bank debited ₹9,87,654.32 that the books lack.")
	bad2, verdict2 := fx.rejectedExplanation(t, 2, "The bank debited ₹1,23,456.00 that the books lack.")
	fx.setSteps(func(s *store.Step) {
		s.Status, s.Attempt, s.OutputRefs, s.InputRefs = store.StepFailed, 3, nil, []string{bad1, bad2}
		s.Feedback, s.Error = json.RawMessage(`{"violations":[{"code":"amount_not_in_evidence"}]}`), strPtr("model error")
	}, func(s *store.Step) {
		s.Status, s.Attempt, s.OutputRefs, s.InputRefs = store.StepSkipped, 3, nil, []string{verdict1, verdict2}
		s.Error = strPtr("no explanation to verify")
	})
	f := fx.st.findings[fx.finding]
	f.Explanation, f.Action, f.Citations, f.Proposal, f.Verified, f.Status = nil, nil, nil, nil, false, checks.StatusNeedsReview
	fx.st.findings[fx.finding] = f
	return fx, verdict2
}

// TestRebuildRejectedRetry: a finding whose last explain retry ended
// without an explanation is rebuilt from the last stored verdict (N-B).
func TestRebuildRejectedRetry(t *testing.T) {
	t.Run("consistent", func(t *testing.T) {
		fx, verdict := rejectedRetryFixture(t)
		code, out, errOut := fx.run(t, "rebuild", fx.finding.String())
		if code != exitMatch || !strings.Contains(out, "== stored verdict "+verdict) || !strings.Contains(out, "₹1,23,456.00") {
			t.Fatalf("exit %d, want 0 on the last verdict\n%s\n%s", code, out, errOut)
		}
	})
	for _, tt := range []struct {
		name   string
		tamper func(*auditFixture, string)
	}{
		{"the finding set back to open", func(fx *auditFixture, _ string) {
			f := fx.st.findings[fx.finding]
			f.Status = "open"
			fx.st.findings[fx.finding] = f
		}},
		{"the finding marked verified", func(fx *auditFixture, _ string) {
			f := fx.st.findings[fx.finding]
			f.Verified = true
			fx.st.findings[fx.finding] = f
		}},
		{"a rejected explanation shown again", func(fx *auditFixture, _ string) {
			f := fx.st.findings[fx.finding]
			s := "Restored."
			f.Explanation = &s
			fx.st.findings[fx.finding] = f
		}},
		{"a tampered last verdict", func(fx *auditFixture, verdict string) {
			a := fx.st.artifacts[verdict]
			a.Content = json.RawMessage(strings.Replace(string(a.Content), `"pass":false`, `"pass":true`, 1))
			fx.st.artifacts[verdict] = a
		}},
		{"the last verdict about an earlier explanation", func(fx *auditFixture, _ string) {
			fx.setSteps(func(s *store.Step) { s.InputRefs = s.InputRefs[1:] }, func(s *store.Step) { s.InputRefs = s.InputRefs[:1] })
		}},
		{"a verify step that is not skipped", func(fx *auditFixture, _ string) {
			fx.setSteps(func(*store.Step) {}, func(s *store.Step) { s.Status = store.StepDone })
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fx, verdict := rejectedRetryFixture(t)
			tt.tamper(fx, verdict)
			if code, out, errOut := fx.run(t, "rebuild", fx.finding.String()); code != exitMismatch {
				t.Errorf("exit %d, want 1\n%s\n%s", code, out, errOut)
			}
		})
	}
}

// TestRebuildRunNotFinished: an unfinished run is a usage error (exit 2),
// not an integrity failure (N-C).
func TestRebuildRunNotFinished(t *testing.T) {
	for _, status := range []string{store.RunQueued, store.RunRunning, ""} {
		fx := newAuditFixture(t)
		runID := fx.st.findings[fx.finding].RunID
		r := fx.st.runs[runID]
		r.Status = status
		fx.st.runs[runID] = r
		// Even with no verify step yet.
		fx.setSteps(func(*store.Step) {}, func(s *store.Step) { s.OutputRefs = nil })
		code, _, errOut := fx.run(t, "rebuild", fx.finding.String())
		if code != exitError || !strings.Contains(errOut, "not finished") {
			t.Errorf("run %q: exit %d, want 2 with 'not finished'\n%s", status, code, errOut)
		}
	}
	for _, status := range []string{store.RunPartial, store.RunFailed} {
		fx := newAuditFixture(t)
		runID := fx.st.findings[fx.finding].RunID
		r := fx.st.runs[runID]
		r.Status = status
		fx.st.runs[runID] = r
		if code, out, errOut := fx.run(t, "rebuild", fx.finding.String()); code != exitMatch {
			t.Errorf("run %q: exit %d, want 0\n%s\n%s", status, code, out, errOut)
		}
	}
}

// needsReviewFixture turns the fixture into a passing explanation that
// asked for review: the verdict passes and the workflow set the finding to
// needs_review.
func needsReviewFixture(t *testing.T) *auditFixture {
	t.Helper()
	ctx := t.Context()
	fx := newAuditFixture(t)
	f := fx.st.findings[fx.finding]
	var explSha string
	for _, s := range fx.st.steps[f.RunID] {
		if s.Kind == store.StepKindExplain {
			explSha = s.OutputRefs[0]
		}
	}
	a, err := fx.st.GetArtifact(ctx, explSha)
	if err != nil {
		t.Fatal(err)
	}
	var art agent.ExplanationArtifact
	if err := json.Unmarshal(a.Content, &art); err != nil {
		t.Fatal(err)
	}
	art.NeedsReview = true
	expl, err := fx.st.PutArtifact(ctx, store.ArtifactExplanation, uuid.Nil, uuid.Nil, art)
	if err != nil {
		t.Fatal(err)
	}
	run := fx.st.runs[f.RunID]
	v := &agent.CodeVerifier{Store: fx.st, Citations: fx.st}
	res, err := v.Verify(ctx, agent.VerifyRequest{
		RunID: run.ID, StepID: fx.verifyID, Company: run.CompanyID, Month: run.Month, Finding: f,
		EvidenceRefs: agent.EvidenceRefs(f), ExplanationRefs: []string{expl}, ExplainAttempt: 1,
	})
	if err != nil || len(res.Violations) != 0 || !res.NeedsReview {
		t.Fatalf("verify: %v, violations %s, needs review %v", err, res.Violations, res.NeedsReview)
	}
	fx.setSteps(func(s *store.Step) { s.OutputRefs = []string{expl} }, func(s *store.Step) { s.OutputRefs = res.OutputRefs })
	fx.verdict = res.OutputRefs[0]
	f.Status = checks.StatusNeedsReview
	fx.st.findings[fx.finding] = f
	return fx
}

// TestRebuildNeedsReviewStatus: a passing explanation that asked for
// review leaves the finding in needs_review; setting it back to open makes
// the rebuild exit 1.
func TestRebuildNeedsReviewStatus(t *testing.T) {
	fx := needsReviewFixture(t)
	if code, out, errOut := fx.run(t, "rebuild", fx.finding.String()); code != exitMatch {
		t.Fatalf("exit %d, want 0\n%s\n%s", code, out, errOut)
	}
	f := fx.st.findings[fx.finding]
	f.Status = "open"
	fx.st.findings[fx.finding] = f
	code, out, errOut := fx.run(t, "rebuild", fx.finding.String())
	if code != exitMismatch || !strings.Contains(errOut, "asked for review") {
		t.Errorf("exit %d, want 1 naming the status\n%s\n%s", code, out, errOut)
	}
}

// TestRebuildMissingArtifact: an explanation or verdict that run_steps
// names but the artifacts table no longer holds is an integrity failure
// (exit 1), not a store error.
func TestRebuildMissingArtifact(t *testing.T) {
	t.Run("the verdict", func(t *testing.T) {
		fx := newAuditFixture(t)
		delete(fx.st.artifacts, fx.verdict)
		code, out, errOut := fx.run(t, "rebuild", fx.finding.String())
		if code != exitMismatch || !strings.Contains(errOut, "is missing") {
			t.Errorf("exit %d, want 1\n%s\n%s", code, out, errOut)
		}
	})
	t.Run("the explanation", func(t *testing.T) {
		fx := newAuditFixture(t)
		for _, s := range fx.st.steps[fx.st.findings[fx.finding].RunID] {
			if s.Kind == store.StepKindExplain {
				delete(fx.st.artifacts, s.OutputRefs[0])
			}
		}
		code, out, errOut := fx.run(t, "rebuild", fx.finding.String())
		if code != exitMismatch || !strings.Contains(errOut, "is missing") {
			t.Errorf("exit %d, want 1\n%s\n%s", code, out, errOut)
		}
	})
	t.Run("the last verdict of a rejected retry", func(t *testing.T) {
		fx, verdict := rejectedRetryFixture(t)
		delete(fx.st.artifacts, verdict)
		code, out, errOut := fx.run(t, "rebuild", fx.finding.String())
		if code != exitMismatch || !strings.Contains(errOut, "is missing") {
			t.Errorf("exit %d, want 1\n%s\n%s", code, out, errOut)
		}
	})
	t.Run("a rejected explanation", func(t *testing.T) {
		fx, _ := rejectedRetryFixture(t)
		for _, s := range fx.st.steps[fx.st.findings[fx.finding].RunID] {
			if s.Kind == store.StepKindExplain {
				delete(fx.st.artifacts, s.InputRefs[len(s.InputRefs)-1])
			}
		}
		code, out, errOut := fx.run(t, "rebuild", fx.finding.String())
		if code != exitMismatch || !strings.Contains(errOut, "is missing") {
			t.Errorf("exit %d, want 1\n%s\n%s", code, out, errOut)
		}
	})
}
