//go:build integration

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/abhishekjha/close-copilot/internal/store"
)

func TestAuditListIntegration(t *testing.T) {
	st, _ := setupTestStore(t)
	ctx := context.Background()

	// A unique actor keeps this test off rows it did not create.
	actor := "test:" + uuid.NewString()
	other := "test:" + uuid.NewString()
	str := func(s string) *string { return &s }
	base := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	var ids []int64
	for i := range 5 {
		e := store.AuditLogEntry{Actor: actor, Server: str("books"), Tool: str("get_gl_entries"), Args: []byte(`{"i":1}`)}
		if i%2 == 1 {
			e.Server, e.Tool = str("approvals"), str("proposal.create")
		}
		id, err := st.InsertAuditLog(ctx, e)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.Pool().Exec(ctx, `UPDATE audit_log SET at = $2 WHERE id = $1`, id, base.Add(time.Duration(i)*24*time.Hour)); err != nil {
			// audit_log is append-only for copilot_app; the test owner may be
			// able to update. Skip the date part when it cannot.
			t.Skipf("cannot set audit_log.at: %v", err)
		}
		ids = append(ids, id)
	}
	if _, err := st.InsertAuditLog(ctx, store.AuditLogEntry{Actor: other, Args: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}

	// Newest first, paged by keyset.
	p1, next, err := st.ListAuditPage(ctx, store.AuditFilter{Actor: actor, Limit: 2})
	if err != nil || len(p1) != 2 || next != p1[1].ID || p1[0].ID != ids[4] || p1[1].ID != ids[3] {
		t.Fatalf("page 1 = %+v next=%d err=%v", p1, next, err)
	}
	p2, next, err := st.ListAuditPage(ctx, store.AuditFilter{Actor: actor, Limit: 2, Before: next})
	if err != nil || len(p2) != 2 || p2[0].ID != ids[2] || next == 0 {
		t.Fatalf("page 2 = %+v next=%d err=%v", p2, next, err)
	}
	p3, next, err := st.ListAuditPage(ctx, store.AuditFilter{Actor: actor, Limit: 2, Before: next})
	if err != nil || len(p3) != 1 || p3[0].ID != ids[0] || next != 0 {
		t.Fatalf("page 3 = %+v next=%d err=%v", p3, next, err)
	}

	// Filters.
	got, _, err := st.ListAuditPage(ctx, store.AuditFilter{Actor: actor, Server: "approvals", Tool: "proposal.create"})
	if err != nil || len(got) != 2 {
		t.Errorf("server+tool filter = %d rows, err=%v", len(got), err)
	}
	from, to := base.Add(24*time.Hour), base.Add(3*24*time.Hour)
	got, _, err = st.ListAuditPage(ctx, store.AuditFilter{Actor: actor, From: &from, To: &to})
	if err != nil || len(got) != 2 || got[0].ID != ids[2] || got[1].ID != ids[1] {
		t.Errorf("date filter = %+v, err=%v", got, err)
	}
	// SQL metacharacters are data.
	got, _, err = st.ListAuditPage(ctx, store.AuditFilter{Actor: "x' OR '1'='1"})
	if err != nil || len(got) != 0 {
		t.Errorf("injection filter = %d rows, err=%v", len(got), err)
	}

	actors, servers, tools, err := st.ListAuditFacets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) == 0 || len(tools) == 0 || len(actors) == 0 {
		t.Errorf("facets empty: %v %v %v", actors, servers, tools)
	}
}
