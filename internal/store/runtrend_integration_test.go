//go:build integration

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/abhishekjha/close-copilot/internal/store"
)

func TestRecentRunTrendIntegration(t *testing.T) {
	st, _ := setupTestStore(t)
	ctx := context.Background()
	base := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Second)

	mk := func(status string, off time.Duration, cost float64) uuid.UUID {
		id := uuid.New()
		at := base.Add(off)
		if err := st.CreateCloseRun(ctx, store.CloseRun{ID: id, CompanyID: "trendco", Month: "2026-08", Status: status,
			StartedAt: &at, InputTokens: 100, OutputTokens: 10, CostUSD: cost}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	finding := func(run uuid.UUID, verified bool) {
		f := store.Finding{ID: uuid.New(), RunID: run, Type: "unrecorded_bank_charge", Severity: "high", Title: "t",
			Keys: map[string]string{"k": uuid.NewString()}, Evidence: []store.EvidenceRef{}, Status: "open", Verified: verified}
		if err := st.CreateFinding(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	oldest := mk(store.RunDone, 1*time.Minute, 0.5)
	mk("running", 2*time.Minute, 9)
	mk("failed", 3*time.Minute, 9)
	mid := mk(store.RunPartial, 4*time.Minute, 1.25)
	newest := mk(store.RunDone, 5*time.Minute, 2)
	finding(newest, true)
	finding(newest, false)
	finding(newest, true)
	_ = oldest
	_ = mid

	got, err := st.RecentRunTrend(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].RunID != newest || got[1].RunID != mid || got[2].RunID != oldest {
		t.Fatalf("trend order = %+v", got)
	}
	if p := got[0]; p.Findings != 3 || p.Verified != 2 || p.CostUSD != 2 || p.InputTokens != 100 || p.OutputTokens != 10 || p.CompanyID != "trendco" {
		t.Errorf("newest = %+v", p)
	}
	if p := got[1]; p.Findings != 0 || p.Verified != 0 || p.CostUSD != 1.25 {
		t.Errorf("mid = %+v", p)
	}
	one, err := st.RecentRunTrend(ctx, -4)
	if err != nil || len(one) != 1 || one[0].RunID != newest {
		t.Errorf("limit clamp low = %+v, %v", one, err)
	}
	many, err := st.RecentRunTrend(ctx, 10000)
	if err != nil || len(many) > 50 {
		t.Errorf("limit clamp high = %d, %v", len(many), err)
	}
	for _, p := range many {
		if p.CompanyID == "trendco" && (p.RunID != newest && p.RunID != mid && p.RunID != oldest) {
			t.Errorf("non-finished run in trend: %+v", p)
		}
	}
}
