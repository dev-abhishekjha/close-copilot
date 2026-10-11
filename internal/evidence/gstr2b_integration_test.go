//go:build integration

package evidence_test

import (
	"bytes"
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/abhishekjha/close-copilot/internal/evidence"
	"github.com/abhishekjha/close-copilot/internal/seed"
	"github.com/abhishekjha/close-copilot/internal/store"
)

func TestLoadGSTR2B_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	ctx := context.Background()
	st := setupTestStore(t)

	g := seedGSTR2B(t)
	want := 0
	for _, s := range g.Data.DocData.B2B {
		want += len(s.Inv)
	}
	if want == 0 {
		t.Fatal("seed return has no invoices")
	}
	path := filepath.Join(t.TempDir(), "gstr2b.json")
	if err := seed.WriteGSTR2B(path, g); err != nil {
		t.Fatalf("WriteGSTR2B: %v", err)
	}

	r1, err := evidence.LoadGSTR2BFile(ctx, st, "sharma", "2026-09", path)
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	if r1.Total != want || r1.Inserted != want || r1.Updated != 0 {
		t.Errorf("first result = %+v, want %d inserted", r1, want)
	}
	rows1, err := st.ListGSTR2BEntries(ctx, "sharma", "2026-09")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows1) != want {
		t.Fatalf("table has %d rows, want %d", len(rows1), want)
	}

	// Loaded rows equal what the parser produced.
	b, err := g.JSON()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := evidence.ParseGSTR2B(bytes.NewReader(b), "sharma", "2026-09")
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed) != len(rows1) {
		t.Fatalf("parsed %d, listed %d", len(parsed), len(rows1))
	}
	for _, p := range parsed {
		found := slices.ContainsFunc(rows1, func(r store.GSTR2BEntry) bool {
			return r.SupplierGSTIN == p.SupplierGSTIN && r.InvoiceNoNorm == p.InvoiceNoNorm &&
				r.InvoiceNo == p.InvoiceNo && r.InvoiceDate.Equal(p.InvoiceDate) &&
				r.TaxablePaise == p.TaxablePaise && r.IGSTPaise == p.IGSTPaise &&
				r.CGSTPaise == p.CGSTPaise && r.SGSTPaise == p.SGSTPaise && r.ITCAvailable == p.ITCAvailable
		})
		if !found {
			t.Errorf("parsed row %s/%s not in table", p.SupplierGSTIN, p.InvoiceNo)
		}
	}

	r2, err := evidence.LoadGSTR2BFile(ctx, st, "sharma", "2026-09", path)
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	if r2.Inserted != 0 || r2.Updated != want {
		t.Errorf("second result = %+v", r2)
	}
	rows2, err := st.ListGSTR2BEntries(ctx, "sharma", "2026-09")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows2) != want {
		t.Errorf("rows after reload = %d, want %d", len(rows2), want)
	}
}
