//go:build integration

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/abhishekjha/close-copilot/internal/store"
)

func TestListCloseRuns(t *testing.T) {
	st, _ := setupTestStore(t)
	ctx := context.Background()

	base := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	mk := func(started *time.Time, status string) uuid.UUID {
		id := uuid.New()
		if err := st.CreateCloseRun(ctx, store.CloseRun{ID: id, CompanyID: "testcorp", Month: "2026-08", Status: status, StartedAt: started}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	t1, t2 := base, base.Add(time.Hour)
	oldest := mk(&t1, store.RunDone)
	newest := mk(&t2, store.RunRunning)
	queued := mk(nil, store.RunQueued)

	for i := 0; i < 3; i++ {
		if err := st.CreateFinding(ctx, store.Finding{
			ID: uuid.New(), RunID: oldest, Type: "variance", Severity: "low", Title: "t", Status: "open",
			Keys: map[string]string{}, Evidence: []store.EvidenceRef{},
		}); err != nil {
			t.Fatal(err)
		}
	}

	got, err := st.ListCloseRuns(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	var ids []uuid.UUID
	counts := map[uuid.UUID]int{}
	for _, r := range got {
		ids = append(ids, r.ID)
		counts[r.ID] = r.Findings
	}
	if len(ids) < 3 || ids[0] != queued || ids[1] != newest || ids[2] != oldest {
		t.Fatalf("order = %v, want queued, newest, oldest first", ids)
	}
	if counts[oldest] != 3 || counts[newest] != 0 || counts[queued] != 0 {
		t.Errorf("finding counts = %v", counts)
	}

	for _, limit := range []int{0, -5, 1} {
		got, err := st.ListCloseRuns(ctx, limit)
		if err != nil || len(got) != 1 {
			t.Errorf("limit %d: %d rows, %v; want 1", limit, len(got), err)
		}
	}
	if got, err := st.ListCloseRuns(ctx, 100000); err != nil || len(got) < 3 {
		t.Errorf("large limit: %d rows, %v", len(got), err)
	}
}
