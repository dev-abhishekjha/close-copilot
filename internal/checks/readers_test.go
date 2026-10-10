package checks

import (
	"context"
	"testing"
	"time"

	"github.com/abhishekjha/close-copilot/internal/store"
)

type fakeEvidenceStore struct {
	bankLines []store.BankLine
	gstr2b    []store.GSTR2BEntry
}

func (f *fakeEvidenceStore) ListBankLines(_ context.Context, filter store.BankLineFilter) ([]store.BankLine, error) {
	var out []store.BankLine
	for _, l := range f.bankLines {
		if filter.CompanyID != "" && l.CompanyID != filter.CompanyID {
			continue
		}
		if filter.FromDate != nil && l.TxnDate.Before(*filter.FromDate) {
			continue
		}
		if filter.ToDate != nil && l.TxnDate.After(*filter.ToDate) {
			continue
		}
		out = append(out, l)
	}
	return out, nil
}

func (f *fakeEvidenceStore) ListGSTR2BEntries(_ context.Context, companyID, period string) ([]store.GSTR2BEntry, error) {
	var out []store.GSTR2BEntry
	for _, e := range f.gstr2b {
		if e.CompanyID == companyID && e.Period == period {
			out = append(out, e)
		}
	}
	return out, nil
}

func TestDirectEvidence(t *testing.T) {
	ctx := context.Background()
	date1 := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	date2 := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)

	fakeStore := &fakeEvidenceStore{
		bankLines: []store.BankLine{
			{CompanyID: "sharma", TxnID: "TXN-1", TxnDate: date1, AmountPaise: 1000},
			{CompanyID: "sharma", TxnID: "TXN-2", TxnDate: date2, AmountPaise: 2000},
			{CompanyID: "other", TxnID: "TXN-3", TxnDate: date1, AmountPaise: 3000},
		},
		gstr2b: []store.GSTR2BEntry{
			{CompanyID: "sharma", Period: "2026-09", InvoiceNo: "INV-1"},
			{CompanyID: "sharma", Period: "2026-08", InvoiceNo: "INV-OLD"},
		},
	}

	reader := &DirectEvidence{Store: fakeStore}

	lines, err := reader.BankLines(ctx, "sharma", date1, date2)
	if err != nil {
		t.Fatalf("BankLines: %v", err)
	}
	if len(lines) != 2 {
		t.Errorf("BankLines len = %d, want 2", len(lines))
	}

	entries, err := reader.GSTR2BEntries(ctx, "sharma", "2026-09")
	if err != nil {
		t.Fatalf("GSTR2BEntries: %v", err)
	}
	if len(entries) != 1 || entries[0].InvoiceNo != "INV-1" {
		t.Errorf("GSTR2BEntries got %+v, want INV-1", entries)
	}
}

func TestDirectEvidenceNilStore(t *testing.T) {
	reader := &DirectEvidence{Store: nil}
	ctx := context.Background()

	if _, err := reader.BankLines(ctx, "sharma", time.Time{}, time.Time{}); err == nil {
		t.Error("expected error with nil store")
	}
	if _, err := reader.GSTR2BEntries(ctx, "sharma", "2026-09"); err == nil {
		t.Error("expected error with nil store")
	}
}
