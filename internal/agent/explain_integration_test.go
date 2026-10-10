//go:build integration

package agent

// CC-704 against Postgres and the real MCP servers over the fake
// ERPNext's skeleton month: the LLMExplainer, with a scripted fake model
// (never a real one), explains every finding through the workflow.

import (
	"os"
	"strings"
	"testing"

	"github.com/abhishekjha/close-copilot/internal/llm"
	"github.com/abhishekjha/close-copilot/internal/store"
)

// skeletonAnswer is a valid answer for a skeleton bank charge, with a
// balanced proposal on two of the skeleton's accounts.
const skeletonAnswer = `{"explanation":"The bank debited a charge that the books have not recorded.",
"suggested_action":"book_entry","action_note":"Book the charge to Bank Charges.","cited_amounts_paise":[],"citations":[],
"needs_review":false,"proposal":{"posting_date":"2026-09-30","lines":[
{"account":"Bank Charges - STPL","debit_paise":100,"credit_paise":0},
{"account":"HDFC Current 0001 - STPL","debit_paise":0,"credit_paise":100}],"remark":"Synthetic bank charge"}}`

func TestExplainSkeletonMonth(t *testing.T) {
	g := setupGateB(t)
	ctx := t.Context()

	valid := answer(skeletonAnswer).Response
	valid.Usage.CostUSD = 0.0012
	malformedFirst := answer(withField(t, "suggested_action", `"pay_now"`))
	malformedFirst.Response.Usage.CostUSD = 0.0012
	model := llm.NewFakeProvider(malformedFirst).WithFallback(valid)

	wf, _ := g.workflow(t)
	wf.Parallel = 1
	wf.Model = model
	wf.Explainer = &LLMExplainer{
		Store: g.st, Accounts: BooksAccounts{Books: wf.Books},
		FastModel: explainFast, StrongModel: explainStrong, Log: discardLog(),
	}
	res, err := wf.RunClose(ctx, skeletonCompany, skeletonMonth)
	if err != nil {
		t.Fatalf("RunClose: %v", err)
	}
	if res.Status != store.RunDone || res.Findings != 3 {
		t.Fatalf("result %+v, want done with 3 findings", res)
	}
	assertAllowlist(t, model.Calls())
	if n := model.CallCount(); n != 4 {
		t.Errorf("%d model calls, want 4 (3 findings, one retry)", n)
	}

	// Every finding has an explanation artifact and a filled row.
	steps, err := g.st.ListSteps(ctx, res.RunID)
	if err != nil {
		t.Fatal(err)
	}
	explained := map[string]string{}
	for _, s := range steps {
		if s.Kind != store.StepKindExplain {
			continue
		}
		if s.Status != store.StepDone || len(s.OutputRefs) != 1 {
			t.Fatalf("explain step %s: %s %v", s.Subject, s.Status, s.OutputRefs)
		}
		a, err := g.st.GetArtifact(ctx, s.OutputRefs[0])
		if err != nil || a.Kind != store.ArtifactExplanation {
			t.Fatalf("explanation artifact of %s: %v %s", s.Subject, err, a.Kind)
		}
		explained[s.Subject] = s.OutputRefs[0]
	}
	fs, err := g.st.ListFindingsByRun(ctx, res.RunID)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fs {
		if _, ok := explained[f.ID.String()]; !ok {
			t.Errorf("finding %s has no explain step", f.ID)
		}
		if f.Explanation == nil || *f.Explanation == "" || f.Action == nil || *f.Action != "book_entry" ||
			f.Proposal == nil || len(f.Proposal.Payload.Lines) != 2 || f.Citations == nil {
			t.Errorf("finding %s row not filled: %+v", f.ID, f)
		}
	}

	// Every model call has an llm_calls row on an explain step.
	var rows int
	if err := g.st.Pool().QueryRow(ctx, `
		SELECT count(*) FROM llm_calls c JOIN run_steps s ON s.id = c.step_id
		WHERE s.run_id = $1 AND s.kind = 'explain'`, res.RunID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != model.CallCount() {
		t.Errorf("%d llm_calls rows for %d calls", rows, model.CallCount())
	}

	// The report shows explanations and the average cost per finding:
	// 4 calls at USD 0.0012 over 3 explained findings.
	md, err := os.ReadFile(res.ReportPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"| Average cost per explained finding | USD 0.0016 (run cost over 3 explained) |",
		"- Explanation: The bank debited a charge that the books have not recorded.",
		"- Proposed entry: 2026-09-30; Dr Bank Charges - STPL ₹1.00; Cr HDFC Current 0001 - STPL ₹1.00",
	} {
		if !strings.Contains(string(md), want) {
			t.Errorf("report lacks %q:\n%s", want, md)
		}
	}
}
