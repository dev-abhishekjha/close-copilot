package checks

import (
	"context"
	"testing"
	"time"

	"github.com/abhishekjha/close-copilot/internal/frappe"
	"github.com/abhishekjha/close-copilot/internal/money"
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

func TestComputeRecurringSuppliers(t *testing.T) {
	d1 := time.Date(2026, 6, 5, 0, 0, 0, 0, time.UTC)
	d2 := time.Date(2026, 7, 5, 0, 0, 0, 0, time.UTC)
	d3 := time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC)

	invoices := []frappe.PurchaseInvoice{
		// Landlord: 3 months, ₹50,000 each month (5000000 paise) -> recurring
		{Supplier: "Landlord", PostingDate: d1, GrandTotal: money.Paise(5000000)},
		{Supplier: "Landlord", PostingDate: d2, GrandTotal: money.Paise(5000000)},
		{Supplier: "Landlord", PostingDate: d3, GrandTotal: money.Paise(5000000)},

		// Fluctuating supplier: 3 months, but swing > 20% from median (1000000 vs 5000000)
		{Supplier: "Fluctuating", PostingDate: d1, GrandTotal: money.Paise(1000000)},
		{Supplier: "Fluctuating", PostingDate: d2, GrandTotal: money.Paise(5000000)},
		{Supplier: "Fluctuating", PostingDate: d3, GrandTotal: money.Paise(9000000)},

		// One-off supplier: 1 month -> below minOccurrences 3
		{Supplier: "OneOff", PostingDate: d1, GrandTotal: money.Paise(5000000)},
	}

	got := computeRecurringSuppliers(invoices, 3, 20)
	if len(got) != 1 {
		t.Fatalf("computeRecurringSuppliers len = %d, want 1", len(got))
	}

	rec := got[0]
	if rec.Supplier != "Landlord" {
		t.Errorf("Supplier = %q, want Landlord", rec.Supplier)
	}
	if rec.MedianAmount != 5000000 {
		t.Errorf("MedianAmount = %d, want 5000000", rec.MedianAmount)
	}
	if rec.TypicalDay != 5 {
		t.Errorf("TypicalDay = %d, want 5", rec.TypicalDay)
	}
	wantMonths := []string{"2026-06", "2026-07", "2026-08"}
	if len(rec.MonthsSeen) != 3 || rec.MonthsSeen[0] != wantMonths[0] || rec.MonthsSeen[1] != wantMonths[1] || rec.MonthsSeen[2] != wantMonths[2] {
		t.Errorf("MonthsSeen = %v, want %v", rec.MonthsSeen, wantMonths)
	}
}

func TestDirectBooksNilClient(t *testing.T) {
	ctx := context.Background()
	reader := &DirectBooks{Client: nil}

	if _, err := reader.TrialBalance(ctx, "c", time.Time{}, time.Time{}); err == nil {
		t.Error("expected error with nil client")
	}
	if _, err := reader.GLEntries(ctx, "c", time.Time{}, time.Time{}); err == nil {
		t.Error("expected error with nil client")
	}
	if _, err := reader.PurchaseInvoices(ctx, "c", time.Time{}, time.Time{}); err == nil {
		t.Error("expected error with nil client")
	}
	if _, err := reader.SalesInvoices(ctx, "c", time.Time{}, time.Time{}); err == nil {
		t.Error("expected error with nil client")
	}
	if _, err := reader.PaymentEntries(ctx, "c", time.Time{}, time.Time{}); err == nil {
		t.Error("expected error with nil client")
	}
	if _, err := reader.AccountHistory(ctx, "c", "acc", "2026-09", 3); err == nil {
		t.Error("expected error with nil client")
	}
}
