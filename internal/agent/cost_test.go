package agent

// CC-1104: the cost and latency changes keep the explainer's cached prefix
// stable, keep every record and amount a finding names in EVIDENCE, and
// keep the explain fan-out bounded.

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/abhishekjha/close-copilot/internal/llm"
	"github.com/abhishekjha/close-copilot/internal/money"
	"github.com/abhishekjha/close-copilot/internal/store"
)

// twoFindingRequests explains two different findings of one run with one
// explainer and returns the two model requests.
func twoFindingRequests(t *testing.T) (*explainFixture, []llm.Request) {
	t.Helper()
	fx, model := newExplainFixture(t, answer(validArgs), answer(validArgs))
	second := fx.req
	second.StepID = uuid.MustParse("00000000-0000-4000-8000-000000000003")
	second.Finding.ID = uuid.MustParse("00000000-0000-4000-8000-0000000000a2")
	second.Finding.Title = "Unrecorded bank charge: a second line (₹5.90)"
	second.Finding.Keys = map[string]string{"bank_txn_id": "HDFC-20260910-001"}
	second.Finding.Evidence = slices.Clone(fx.req.Finding.Evidence)
	second.Finding.Evidence[0].IDs = []string{"HDFC-20260910-001"}
	for _, req := range []ExplainRequest{fx.req, second} {
		if res, err := fx.ex.Explain(t.Context(), req); err != nil || res.Status != store.StepDone {
			t.Fatalf("Explain: %+v, %v", res, err)
		}
	}
	calls := model.Calls()
	if len(calls) != 2 {
		t.Fatalf("%d calls, want 2", len(calls))
	}
	return fx, calls
}

func TestExplainCachedPrefixIdenticalAcrossFindings(t *testing.T) {
	fx, calls := twoFindingRequests(t)
	first, second := calls[0], calls[1]
	if len(first.System) != 1 || !first.System[0].Cacheable {
		t.Fatalf("first request system %+v, want one cacheable block", first.System)
	}
	if !slices.Equal(first.System, second.System) {
		t.Error("the cacheable system prefix differs between two findings of one run")
	}
	// The tools sit before the system prompt in the cached prefix.
	a, _ := json.Marshal(first.Tools)
	b, _ := json.Marshal(second.Tools)
	if !bytes.Equal(a, b) {
		t.Error("the tool definitions differ between two findings of one run")
	}
	// Nothing per finding, step or run is in the prefix.
	sys := first.System[0].Text
	for _, s := range []string{
		fx.req.RunID.String(), fx.req.StepID.String(), fx.req.Finding.ID.String(),
		"HDFC-20260915-C1", "HDFC-20260910-001", fx.req.Finding.Title,
	} {
		if strings.Contains(sys, s) {
			t.Errorf("the cached prefix holds per-call text %q", s)
		}
	}
	if first.Messages[0].Content == second.Messages[0].Content {
		t.Error("the two findings sent the same user message")
	}
}

// TestProjectEvidenceTrimKeepsNamedRecordsAndAmounts: trimming drops only
// empty, derived and run-company fields; every record the refs name and
// every amount the verifier traces (moneyLeaves) stay.
func TestProjectEvidenceTrimKeepsNamedRecordsAndAmounts(t *testing.T) {
	result := []any{
		map[string]any{
			"Name": "ACC-GLE-0001", "Company": "sharma", "Account": "Bank Charges - STPL", "Party": "", "PartyType": nil,
			"FiscalYear": "", "Debit": json.Number("590"), "Credit": json.Number("0"), "Remarks": "NEFT charges",
			"Items": []any{map[string]any{"item": "fee", "Amount": json.Number("590"), "note": ""}},
			"Taxes": []any{}, "Docstatus": json.Number("1"),
		},
		map[string]any{
			"txn_id": "HDFC-20260915-C1", "company_id": "sharma", "source_file": "", "narration": "NEFT CHGS",
			"amount_paise": json.Number("-590"), "balance_paise": json.Number("123456"), "invoice_no_norm": "inv1",
		},
		map[string]any{
			// Another company's name is not the run's: it stays.
			"txn_id": "HDFC-20260916-X", "company_id": "other", "amount_paise": json.Number("100"),
		},
		map[string]any{"txn_id": "HDFC-20260917-NOT-NAMED", "amount_paise": json.Number("7")},
	}
	s := snapshotContent{
		Server: ServerEvidence, Tool: toolListBankLines,
		Args:   map[string]any{"company": "sharma", "from_date": "2026-09-01", "to_date": "2026-09-30"},
		Result: result,
	}
	named := []string{"ACC-GLE-0001", "HDFC-20260915-C1", "HDFC-20260916-X"}
	refs := []store.EvidenceRef{{Artifact: "sha-1", IDs: named}}

	view := projectEvidence(s, "sha-1", refs)

	// Every named record, and only those.
	if !slices.Equal(view.IDs, named) || len(view.Records) != len(named) || view.Omitted != 0 {
		t.Fatalf("view ids %v, %d records, %d omitted", view.IDs, len(view.Records), view.Omitted)
	}
	text, err := json.Marshal(view.Records)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range named {
		if !bytes.Contains(text, []byte(`"`+id+`"`)) {
			t.Errorf("named record %s missing", id)
		}
	}
	if bytes.Contains(text, []byte("NOT-NAMED")) {
		t.Error("an unnamed record was sent")
	}

	// Every amount the verifier traces is still there.
	var before, after evidenceAmounts
	for _, r := range selectRecords(result, named) {
		moneyLeaves(r, &before)
	}
	for _, r := range view.Records {
		moneyLeaves(r, &after)
	}
	sortPaise := func(p []money.Paise) []money.Paise { p = slices.Clone(p); slices.Sort(p); return p }
	if !slices.Equal(sortPaise(before.singles), sortPaise(after.singles)) || !slices.Equal(sortPaise(before.pairable), sortPaise(after.pairable)) {
		t.Errorf("traced amounts changed: before %+v, after %+v", before, after)
	}

	// What goes, and what stays.
	for _, gone := range []string{`"Company"`, `"Party"`, `"PartyType"`, `"FiscalYear"`, `"Taxes"`, `"note"`, `"source_file"`, `"invoice_no_norm"`, `"company_id":"sharma"`} {
		if bytes.Contains(text, []byte(gone)) {
			t.Errorf("trimmed evidence still holds %s", gone)
		}
	}
	for _, kept := range []string{`"Account":"Bank Charges - STPL"`, `"Remarks":"NEFT charges"`, `"narration":"NEFT CHGS"`, `"company_id":"other"`, `"Docstatus":1`, `"Credit":0`} {
		if !bytes.Contains(text, []byte(kept)) {
			t.Errorf("trimmed evidence lost %s", kept)
		}
	}
}

func TestTrimRecordKeepsEmptyMoneyFields(t *testing.T) {
	got := trimRecord(map[string]any{"amount": nil, "net_total": json.Number("0"), "x": ""}, "sharma")
	m, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("got %T", got)
	}
	if _, ok := m["amount"]; !ok {
		t.Error("a money field was dropped")
	}
	if _, ok := m["net_total"]; !ok {
		t.Error("a zero money field was dropped")
	}
	if _, ok := m["x"]; ok {
		t.Error("an empty field was kept")
	}
}

func TestExplainParallelDefault(t *testing.T) {
	if DefaultExplainParallel < 4 || DefaultExplainParallel > 8 {
		t.Errorf("DefaultExplainParallel %d, want 4 to 8 (CC-1104)", DefaultExplainParallel)
	}
	if got := (&Workflow{}).parallel(); got != DefaultExplainParallel {
		t.Errorf("default parallel %d, want %d", got, DefaultExplainParallel)
	}
	if got := (&Workflow{Parallel: 2}).parallel(); got != 2 {
		t.Errorf("Parallel 2 gives %d", got)
	}
}
