//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/abhishekjha/close-copilot/internal/money"
	"github.com/abhishekjha/close-copilot/internal/store"
)

func evRef(t *testing.T, st *store.Store, run uuid.UUID, tool string) store.EvidenceRef {
	t.Helper()
	sha, err := st.PutArtifact(context.Background(), store.ArtifactToolResult, run, uuid.Nil,
		map[string]any{"server": "books", "tool": tool, "args": map[string]any{"company": "testcorp"}, "result": []any{}})
	if err != nil {
		t.Fatal(err)
	}
	return store.EvidenceRef{Server: "books", Tool: tool, Args: json.RawMessage(`{"company":"testcorp"}`), Artifact: sha}
}

func TestFindingResolution(t *testing.T) {
	st, _ := setupTestStore(t)
	ctx := context.Background()

	runA, runB := uuid.New(), uuid.New()
	for _, r := range []store.CloseRun{
		{ID: runA, CompanyID: "testcorp", Month: "2026-09", Status: store.RunRunning},
		{ID: runB, CompanyID: "othercorp", Month: "2026-09", Status: store.RunRunning},
	} {
		if err := st.CreateCloseRun(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	base := evRef(t, st, runA, "list_bank_lines")
	base.IDs = []string{"TXN-PG-1"}
	amt := money.Paise(9764000)
	f := store.Finding{ID: uuid.New(), RunID: runA, Type: "unmatched_bank_line", Severity: "medium", Title: "Synthetic settlement",
		AmountPaise: &amt, Keys: map[string]string{"bank_txn_id": "TXN-PG-1", "hint": "gateway"}, Evidence: []store.EvidenceRef{base}}
	if err := st.CreateFinding(ctx, f); err != nil {
		t.Fatal(err)
	}

	gl := evRef(t, st, runA, "list_gl_entries")
	pay := evRef(t, st, runA, "list_payments")
	// The base ref and a repeat inside the list are skipped; order is kept.
	if err := st.SetFindingResolution(ctx, runA, f.ID, "testcorp", "gateway_settlement", []store.EvidenceRef{gl, base, pay, gl}); err != nil {
		t.Fatalf("SetFindingResolution: %v", err)
	}
	got := findingByID(t, st, runA, f.ID)
	if got.Keys["resolution"] != "gateway_settlement" || got.Keys["bank_txn_id"] != "TXN-PG-1" || got.Keys["hint"] != "gateway" {
		t.Errorf("keys %v", got.Keys)
	}
	wantArtifacts := []string{base.Artifact, gl.Artifact, pay.Artifact}
	if len(got.Evidence) != len(wantArtifacts) {
		t.Fatalf("evidence %+v", got.Evidence)
	}
	for i, w := range wantArtifacts {
		if got.Evidence[i].Artifact != w {
			t.Errorf("evidence[%d] = %s, want %s", i, got.Evidence[i].Artifact, w)
		}
	}
	if len(got.Evidence[0].IDs) != 1 || got.Evidence[1].Tool != "list_gl_entries" || got.Evidence[1].IDs == nil {
		t.Errorf("evidence refs %+v", got.Evidence)
	}

	// A re-run with the same trail changes only the resolution.
	if err := st.SetFindingResolution(ctx, runA, f.ID, "testcorp", "unknown", []store.EvidenceRef{gl, pay}); err != nil {
		t.Fatal(err)
	}
	got = findingByID(t, st, runA, f.ID)
	if got.Keys["resolution"] != "unknown" || len(got.Evidence) != 3 {
		t.Errorf("after re-run: keys %v evidence %d", got.Keys, len(got.Evidence))
	}
	// No evidence at all is fine.
	if err := st.SetFindingResolution(ctx, runA, f.ID, "testcorp", "timing_difference", nil); err != nil {
		t.Fatal(err)
	}

	// Another run, company or finding is ErrNotFound and changes nothing.
	for _, c := range []struct {
		name    string
		run     uuid.UUID
		finding uuid.UUID
		company string
	}{
		{"another run's ID", runB, f.ID, "othercorp"},
		{"another run's ID with the right company", runB, f.ID, "testcorp"},
		{"another company's ID", runA, f.ID, "othercorp"},
		{"an unknown run", uuid.New(), f.ID, "testcorp"},
		{"an unknown finding", runA, uuid.New(), "testcorp"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := st.SetFindingResolution(ctx, c.run, c.finding, c.company, "gateway_settlement", []store.EvidenceRef{evRef(t, st, runB, "get_trial_balance")})
			if !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("err = %v, want ErrNotFound", err)
			}
			got := findingByID(t, st, runA, f.ID)
			if got.Keys["resolution"] != "timing_difference" || len(got.Evidence) != 3 {
				t.Errorf("finding changed: keys %v evidence %d", got.Keys, len(got.Evidence))
			}
		})
	}

	// Bad input is refused before the database.
	for name, call := range map[string]func() error{
		"empty resolution": func() error { return st.SetFindingResolution(ctx, runA, f.ID, "testcorp", "", nil) },
		"empty company":    func() error { return st.SetFindingResolution(ctx, runA, f.ID, "", "unknown", nil) },
		"ref without artifact": func() error {
			return st.SetFindingResolution(ctx, runA, f.ID, "testcorp", "unknown", []store.EvidenceRef{{Server: "books", Tool: "x"}})
		},
	} {
		if err := call(); err == nil || errors.Is(err, store.ErrNotFound) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}
