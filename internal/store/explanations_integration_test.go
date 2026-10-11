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
	if err := st.SetFindingExplanation(ctx, f.ID, run, "testcorp", "The bank charged ₹5.90.", "accrue", citations, proposal); err != nil {
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
	if err := st.SetFindingExplanation(ctx, f.ID, run, "testcorp", "No booking needed.", "no_action", nil, nil); err != nil {
		t.Fatal(err)
	}
	got = findingByID(t, st, run, f.ID)
	if *got.Explanation != "No booking needed." || *got.Action != "no_action" || got.Proposal != nil || len(got.Citations) != 0 {
		t.Errorf("finding after second update %+v", got)
	}

	if err := st.SetFindingExplanation(ctx, uuid.New(), run, "testcorp", "x", "no_action", nil, nil); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown finding: %v", err)
	}
	if err := st.SetFindingExplanation(ctx, f.ID, run, "testcorp", "", "no_action", nil, nil); err == nil {
		t.Error("empty explanation accepted")
	}
	if err := st.SetFindingExplanation(ctx, f.ID, run, "testcorp", "x", "", nil, nil); err == nil {
		t.Error("empty action accepted")
	}
}

// TestSetFindingExplanationCrossRun: a run can write explanations only to
// its own findings. Another run's ID, or another company's, matches no row,
// and the finding is left unchanged.
func TestSetFindingExplanationCrossRun(t *testing.T) {
	st, _ := setupTestStore(t)
	ctx := context.Background()

	runA, runB := uuid.New(), uuid.New()
	for _, r := range []store.CloseRun{
		{ID: runA, CompanyID: "testcorp", Month: "2026-08", Status: store.RunRunning},
		{ID: runB, CompanyID: "othercorp", Month: "2026-08", Status: store.RunRunning},
	} {
		if err := st.CreateCloseRun(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	amt := money.Paise(590)
	f := store.Finding{ID: uuid.New(), RunID: runA, Type: "unrecorded_bank_charge", Severity: "low", Title: "Synthetic charge",
		AmountPaise: &amt, Keys: map[string]string{"bank_txn_id": "TXN-X"}, Evidence: []store.EvidenceRef{}}
	if err := st.CreateFinding(ctx, f); err != nil {
		t.Fatal(err)
	}
	if err := st.SetFindingExplanation(ctx, f.ID, runA, "testcorp", "Run A's text.", "accrue", nil, nil); err != nil {
		t.Fatalf("own run: %v", err)
	}

	cases := []struct {
		name    string
		run     uuid.UUID
		company string
	}{
		{"another run's ID", runB, "othercorp"},
		{"another run's ID with the right company", runB, "testcorp"},
		{"another company's ID", runA, "othercorp"},
		{"an unknown run", uuid.New(), "testcorp"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := st.SetFindingExplanation(ctx, f.ID, c.run, c.company, "Injected text.", "no_action", nil, nil)
			if !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("err = %v, want ErrNotFound", err)
			}
			got := findingByID(t, st, runA, f.ID)
			if got.Explanation == nil || *got.Explanation != "Run A's text." || got.Action == nil || *got.Action != "accrue" {
				t.Errorf("finding changed: explanation %v action %v", got.Explanation, got.Action)
			}
		})
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
