//go:build integration

package agent

// The investigator (CC-706) against Postgres (testcontainers) and the real
// books and evidence MCP servers over the fake ERPNext's skeleton month
// (fakeerpd). The model is a scripted FakeProvider behind the recording
// and redacting wrappers, as the workflow builds them.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/abhishekjha/close-copilot/internal/checks"
	"github.com/abhishekjha/close-copilot/internal/llm"
	"github.com/abhishekjha/close-copilot/internal/money"
	"github.com/abhishekjha/close-copilot/internal/store"
)

// skeletonCharge is a planted charge of the skeleton month, investigated
// here as if the rules had left it unmatched.
const skeletonCharge = "HDFC-20260915-C1"

const chargeResolution = `{"resolution_type":"unrecorded_charge",
"explanation":"The bank debited ₹5.90 of NEFT charges on 15 Sep 2026; the books have no matching entry.",
"evidence_ids":["` + skeletonCharge + `"],
"proposal":{"posting_date":"2026-09-15","remark":"NEFT charges","lines":[
{"account":"Bank Charges - STPL","debit_paise":590,"credit_paise":0},
{"account":"HDFC Current 0001 - STPL","debit_paise":0,"credit_paise":590}]}}`

// investigateTarget stores a run, a check step's bank-line snapshot and an
// unmatched_bank_line finding for the planted charge.
func investigateTarget(t *testing.T, g *gateB) (uuid.UUID, store.Finding) {
	t.Helper()
	ctx := t.Context()
	runID := uuid.New()
	if err := g.st.CreateCloseRun(ctx, store.CloseRun{ID: runID, CompanyID: skeletonCompany, Month: skeletonMonth, Status: store.RunRunning}); err != nil {
		t.Fatal(err)
	}
	step, _, err := g.st.Begin(ctx, runID, store.StepKindCheckPrefix+"bankrec", "bankrec")
	if err != nil {
		t.Fatal(err)
	}
	snap := NewSnapshots(g.st, runID, step.ID)
	from, to, _ := monthRange(skeletonMonth)
	if _, err := SnapshotEvidence(&MCPEvidence{Registry: g.reg}, snap).BankLines(ctx, skeletonCompany, from, to); err != nil {
		t.Fatal(err)
	}
	amt := money.Paise(590)
	fs := []store.Finding{{
		ID: uuid.New(), RunID: runID, Type: checks.TypeUnmatchedBankLine, Severity: checks.SeverityMedium,
		Title: "Unmatched bank line (synthetic)", AmountPaise: &amt,
		Keys:     map[string]string{"bank_txn_id": skeletonCharge},
		Evidence: []store.EvidenceRef{checks.EvidenceEvidence(toolListBankLines, map[string]string{"company": skeletonCompany, "txn_id": skeletonCharge}, skeletonCharge)},
		Status:   checks.StatusOpen,
	}}
	if err := snap.Annotate(fs); err != nil {
		t.Fatal(err)
	}
	if err := g.st.FinishWithFindings(ctx, step, snap.Refs(), fs); err != nil {
		t.Fatal(err)
	}
	return runID, fs[0]
}

func TestInvestigateIntegration(t *testing.T) {
	g := setupGateB(t)
	ctx := t.Context()
	runID, f := investigateTarget(t, g)
	step, _, err := g.st.Begin(ctx, runID, store.StepKindInvestigate, f.ID.String())
	if err != nil {
		t.Fatal(err)
	}
	fake := llm.NewFakeProvider(
		turn(1000, toolCall("c1", "evidence__list_bank_lines", `{"company":"sharma","from_date":"2026-09-14","to_date":"2026-09-16"}`),
			toolCall("c2", "evidence__search_documents", `{"query":"charges"}`)),
		turn(1000, emitCall(chargeResolution)),
	)
	key := []byte("0123456789abcdef0123456789abcdef")
	model := WrapRedacting(NewRecordingProvider(fake, g.st, runID, step.ID), NewRedactor(key, runID, nil))
	iv := &LLMInvestigator{
		Store: g.st, Tools: g.reg, Model: invModel, Log: discardLog(),
		Accounts: BooksAccounts{Books: &MCPBooks{Registry: g.reg, Companies: g.st}},
	}
	res, err := iv.Investigate(ctx, InvestigateRequest{
		RunID: runID, StepID: step.ID, Attempt: step.Attempt, Company: skeletonCompany, Month: skeletonMonth,
		Finding: f, EvidenceRefs: evidenceRefs(f), Model: model,
	})
	if err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if res.Status != store.StepDone || res.Reason != "" || len(res.OutputRefs) != 1 {
		t.Fatalf("result %+v", res)
	}
	if err := g.st.Finish(ctx, step, res.Status, res.OutputRefs, res.Reason); err != nil {
		t.Fatal(err)
	}

	// The refused call came back as a fixed error; the real call's result
	// reached the model projected and fenced.
	second := fake.Calls()[1]
	tr := second.Messages[len(second.Messages)-1].ToolResults
	if len(tr) != 2 || tr[1].Content != MsgUnknownTool || !strings.HasPrefix(tr[0].Content, "<<<RESULT\n") ||
		!strings.Contains(tr[0].Content, skeletonCharge) || !strings.Contains(tr[0].Content, "<<<NARRATION ") {
		t.Errorf("tool results %+v", tr)
	}

	// The finding carries the resolution and the trail, in Postgres.
	got, err := g.st.GetFinding(ctx, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Keys[store.ResolutionKey] != ResolutionUnrecordedCharge || len(got.Evidence) != 2 ||
		got.Evidence[1].Server != ServerEvidence || got.Evidence[1].Tool != toolListBankLines {
		t.Fatalf("finding keys %v evidence %+v", got.Keys, got.Evidence)
	}
	a, err := g.st.GetArtifact(ctx, got.Evidence[1].Artifact)
	if err != nil || a.Kind != store.ArtifactToolResult {
		t.Fatalf("trail artifact: %v %s", err, a.Kind)
	}

	// The unchanged CodeVerifier passes it, from stored artifacts only.
	vres, err := (&CodeVerifier{Store: g.st, Citations: g.st}).Verify(ctx, VerifyRequest{
		RunID: runID, StepID: step.ID, Company: skeletonCompany, Month: skeletonMonth,
		Finding: got, EvidenceRefs: evidenceRefs(got), ExplanationRefs: res.OutputRefs, ExplainAttempt: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(vres.Violations) != 0 {
		t.Errorf("verifier rejected the investigation: %s", vres.Violations)
	}
	var art InvestigationArtifact
	ea, err := g.st.GetArtifact(ctx, res.OutputRefs[0])
	if err != nil || json.Unmarshal(ea.Content, &art) != nil || art.Resolution != ResolutionUnrecordedCharge || art.ToolCalls != 2 {
		t.Errorf("artifact %+v, %v", art, err)
	}

	// Both model calls were recorded against the step.
	if n, err := g.st.RunTokensUsed(ctx, runID); err != nil || n != 2*(1000+50) {
		t.Errorf("tokens recorded %d, %v", n, err)
	}
}

// TestInvestigateIntegrationTimeLimit: an investigation over the real
// servers stops at its deadline and leaves the finding needs_review-ready
// with resolution unknown.
func TestInvestigateIntegrationTimeLimit(t *testing.T) {
	g := setupGateB(t)
	ctx := t.Context()
	runID, f := investigateTarget(t, g)
	iv := &LLMInvestigator{
		Store: g.st, Tools: g.reg, Model: invModel, Log: discardLog(),
		Accounts: BooksAccounts{Books: &MCPBooks{Registry: g.reg, Companies: g.st}},
		limits:   investigateLimits{duration: 50 * time.Millisecond},
	}
	res, err := iv.Investigate(ctx, InvestigateRequest{
		RunID: runID, StepID: uuid.New(), Company: skeletonCompany, Month: skeletonMonth,
		Finding: f, EvidenceRefs: evidenceRefs(f), Model: blockingModel{},
	})
	if err != nil || res.Reason != reasonLimitTime || !res.NeedsReview {
		t.Fatalf("result %+v, %v", res, err)
	}
	got, err := g.st.GetFinding(context.Background(), f.ID)
	if err != nil || got.Keys[store.ResolutionKey] != ResolutionUnknown {
		t.Errorf("finding %v, %v", got.Keys, err)
	}
}
