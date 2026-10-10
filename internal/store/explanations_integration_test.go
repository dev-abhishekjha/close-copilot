//go:build integration

package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/abhishekjha/close-copilot/internal/money"
	"github.com/abhishekjha/close-copilot/internal/store"
)

func TestFindingExplanation(t *testing.T) {
	st, _ := setupTestStore(t)
	ctx := context.Background()

	run := uuid.New()
	if err := st.CreateCloseRun(ctx, store.CloseRun{ID: run, CompanyID: "testcorp", Month: "2026-08", Status: store.RunRunning}); err != nil {
		t.Fatal(err)
	}
	amt := money.Paise(590)
	action := "book_entry"
	f := store.Finding{ID: uuid.New(), RunID: run, Type: "unrecorded_bank_charge", Severity: "low", Title: "Synthetic charge",
		AmountPaise: &amt, Keys: map[string]string{"bank_txn_id": "TXN-1"}, Evidence: []store.EvidenceRef{}, Action: &action}
	if err := st.CreateFinding(ctx, f); err != nil {
		t.Fatal(err)
	}

	proposal := &store.JournalProposal{FindingID: &f.ID, CompanyID: "testcorp", Status: "proposed", Payload: store.JournalPayload{
		PostingDate: "2026-08-15", Remark: "Synthetic charge",
		Lines: []store.JournalLine{{Account: "Bank Charges - TC", DebitPaise: 590}, {Account: "HDFC Current 0001 - TC", CreditPaise: 590}},
	}}
	citations := []store.Citation{{DocID: "POL-1", Section: "2.1"}}
	if err := st.SetFindingExplanation(ctx, f.ID, "The bank charged ₹5.90.", "accrue", citations, proposal); err != nil {
		t.Fatalf("SetFindingExplanation: %v", err)
	}
	got := findingByID(t, st, run, f.ID)
	if got.Explanation == nil || *got.Explanation != "The bank charged ₹5.90." || got.Action == nil || *got.Action != "accrue" ||
		len(got.Citations) != 1 || got.Citations[0] != citations[0] || got.Proposal == nil ||
		got.Proposal.Payload.Lines[0].DebitPaise != 590 || got.Proposal.Payload.Lines[1].CreditPaise != 590 ||
		*got.Proposal.FindingID != f.ID || got.Status != "open" || got.Title != f.Title {
		t.Errorf("finding after update %+v", got)
	}

	// A second write without a proposal or citations clears them.
	if err := st.SetFindingExplanation(ctx, f.ID, "No booking needed.", "no_action", nil, nil); err != nil {
		t.Fatal(err)
	}
	got = findingByID(t, st, run, f.ID)
	if *got.Explanation != "No booking needed." || *got.Action != "no_action" || got.Proposal != nil || len(got.Citations) != 0 {
		t.Errorf("finding after second update %+v", got)
	}

	if err := st.SetFindingExplanation(ctx, uuid.New(), "x", "no_action", nil, nil); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown finding: %v", err)
	}
	if err := st.SetFindingExplanation(ctx, f.ID, "", "no_action", nil, nil); err == nil {
		t.Error("empty explanation accepted")
	}
	if err := st.SetFindingExplanation(ctx, f.ID, "x", "", nil, nil); err == nil {
		t.Error("empty action accepted")
	}
}

func findingByID(t *testing.T, st *store.Store, run, id uuid.UUID) store.Finding {
	t.Helper()
	fs, err := st.ListFindingsByRun(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fs {
		if f.ID == id {
			return f
		}
	}
	t.Fatalf("finding %s not found", id)
	return store.Finding{}
}
