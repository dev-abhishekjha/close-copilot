//go:build integration

package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/pgvector/pgvector-go"

	"github.com/abhishekjha/close-copilot/internal/store"
)

func TestGetFinding(t *testing.T) {
	ctx := context.Background()
	st, _ := setupTestStore(t)
	run := newRun(t, st)
	f := testFinding(t, st, run, "TXN-GET")
	f.Citations = []store.Citation{{DocID: "POL-BANK", Section: "2.1"}}
	if err := st.CreateFinding(ctx, f); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetFinding(ctx, f.ID)
	if err != nil {
		t.Fatalf("GetFinding: %v", err)
	}
	if got.ID != f.ID || got.RunID != run || got.AmountPaise == nil || *got.AmountPaise != *f.AmountPaise ||
		got.Keys["bank_txn_id"] != "TXN-GET" || len(got.Evidence) != 1 || got.Evidence[0].Artifact != f.Evidence[0].Artifact ||
		len(got.Citations) != 1 || got.Status != "open" || got.Verified {
		t.Errorf("finding %+v", got)
	}
	if _, err := st.GetFinding(ctx, uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("missing finding: %v, want ErrNotFound", err)
	}

}

func TestSetFindingVerified(t *testing.T) {
	ctx := context.Background()
	st, _ := setupTestStore(t)
	run := newRun(t, st)
	f := testFinding(t, st, run, "TXN-VERIFIED")
	if err := st.CreateFinding(ctx, f); err != nil {
		t.Fatal(err)
	}
	if err := st.SetFindingVerified(ctx, run, f.ID, true); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetFinding(ctx, f.ID); !got.Verified {
		t.Error("verified flag not set")
	}
	if err := st.SetFindingVerified(ctx, run, uuid.New(), true); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("verify a missing finding: %v", err)
	}
	// Another run's ID matches no row, and changes nothing.
	other := newRun(t, st)
	if err := st.SetFindingVerified(ctx, other, f.ID, false); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("verify from another run: %v, want ErrNotFound", err)
	}
	if got, _ := st.GetFinding(ctx, f.ID); !got.Verified {
		t.Error("another run's call changed the flag")
	}
}

func TestClearFindingExplanation(t *testing.T) {
	ctx := context.Background()
	st, _ := setupTestStore(t)
	run := newRun(t, st)
	f := testFinding(t, st, run, "TXN-CLEAR")
	if err := st.CreateFinding(ctx, f); err != nil {
		t.Fatal(err)
	}
	fid := f.ID
	proposal := &store.JournalProposal{FindingID: &fid, CompanyID: "testcorp", Status: "proposed", Payload: store.JournalPayload{
		PostingDate: "2026-09-30", Lines: []store.JournalLine{{Account: "A", DebitPaise: 590}, {Account: "B", CreditPaise: 590}}}}
	if err := st.SetFindingExplanation(ctx, f.ID, "Rejected text.", "book_entry", []store.Citation{{DocID: "D", Section: "1"}}, proposal); err != nil {
		t.Fatal(err)
	}
	if err := st.SetFindingVerified(ctx, run, f.ID, true); err != nil {
		t.Fatal(err)
	}
	// Another run's ID matches no row.
	if err := st.ClearFindingExplanation(ctx, newRun(t, st), f.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("clear from another run: %v, want ErrNotFound", err)
	}
	for range 2 { // clearing twice is harmless
		if err := st.ClearFindingExplanation(ctx, run, f.ID); err != nil {
			t.Fatalf("ClearFindingExplanation: %v", err)
		}
	}
	got, err := st.GetFinding(ctx, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Explanation != nil || got.Action != nil || len(got.Citations) != 0 || got.Proposal != nil || got.Verified {
		t.Errorf("finding after clear %+v", got)
	}
	if err := st.ClearFindingExplanation(ctx, run, uuid.New()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("clear a missing finding: %v", err)
	}
}

func TestCitationExists(t *testing.T) {
	ctx := context.Background()
	st, _ := setupTestStore(t)
	sec := func(s string) *string { return &s }
	co := func(s string) *string { return &s }
	vec := pgvector.NewVector(make([]float32, 384))
	if err := st.InsertDocChunks(ctx, []store.DocChunk{
		{DocID: "POL-BANK", DocType: "policy", CompanyID: co("testcorp"), Section: sec("2.1"), Content: "Synthetic bank policy.", ContentHash: "h1", Embedding: vec},
		{DocID: "POL-SHARED", DocType: "policy", Section: sec("1"), Content: "Synthetic shared policy.", ContentHash: "h2", Embedding: vec},
		{DocID: "POL-OTHER", DocType: "policy", CompanyID: co("otherco"), Section: sec("4"), Content: "Another company's policy.", ContentHash: "h3", Embedding: vec},
	}); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		company, doc, section string
		want                  bool
	}{
		{"testcorp", "POL-BANK", "2.1", true},
		{"testcorp", "POL-SHARED", "1", true},
		{"otherco", "POL-SHARED", "1", true},
		{"testcorp", "POL-OTHER", "4", false},
		{"otherco", "POL-BANK", "2.1", false},
		{"testcorp", "POL-BANK", "9.9", false},
		{"testcorp", "POL-GONE", "1", false},
	} {
		got, err := st.CitationExists(ctx, tt.company, tt.doc, tt.section)
		if err != nil || got != tt.want {
			t.Errorf("CitationExists(%s, %s, %s) = %v, %v; want %v", tt.company, tt.doc, tt.section, got, err, tt.want)
		}
	}
	if _, err := st.CitationExists(ctx, "", "POL-BANK", "2.1"); err == nil {
		t.Error("empty company: want an error")
	}
}
