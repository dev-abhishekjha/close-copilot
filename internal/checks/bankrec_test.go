package checks

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/abhishekjha/close-copilot/internal/books"
	"github.com/abhishekjha/close-copilot/internal/frappe"
	"github.com/abhishekjha/close-copilot/internal/money"
	"github.com/abhishekjha/close-copilot/internal/seed"
	"github.com/abhishekjha/close-copilot/internal/store"
)

func strPtr(s string) *string {
	return &s
}

func TestBankRecPass1ExactReference(t *testing.T) {
	d := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	amt := money.Paise(-1500000)

	bankLines := []store.BankLine{
		{
			TxnID:       "TXN-01",
			TxnDate:     d,
			AmountPaise: amt,
			Narration:   "NEFT-RENT-OMKAR",
			Ref:         strPtr("REF-12345"),
		},
	}

	vouchers := []BankVoucher{
		{
			VoucherNo:   "ACC-PAY-2026-0001",
			PostingDate: d,
			Net:         amt,
			ReferenceNo: "REF-12345",
			Remarks:     "Rent payment",
			Party:       "Omkar Estates",
		},
	}

	res := MatchBankLines(bankLines, vouchers, 3)
	if len(res.Matched) != 1 {
		t.Fatalf("expected 1 match, got %d", len(res.Matched))
	}
	if res.Matched[0].Pass != 1 {
		t.Errorf("expected Pass 1, got Pass %d", res.Matched[0].Pass)
	}
	if len(res.UnmatchedBank) != 0 {
		t.Errorf("expected 0 unmatched bank lines, got %d", len(res.UnmatchedBank))
	}
	if len(res.UnmatchedVouchers) != 0 {
		t.Errorf("expected 0 unmatched vouchers, got %d", len(res.UnmatchedVouchers))
	}
}

func TestBankRecPass2DateWindowAndTieBreaking(t *testing.T) {
	bankDate := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	amt := money.Paise(-2500000)

	bankLines := []store.BankLine{
		{
			TxnID:       "TXN-COLLISION",
			TxnDate:     bankDate,
			AmountPaise: amt,
			Narration:   "PAYMENT TO BLUE NET BROADBAND",
		},
	}

	// Two vouchers with identical amounts:
	// V1 is 2 days away.
	// V2 is 1 day away (closer date -> should win tie).
	vouchers := []BankVoucher{
		{
			VoucherNo:   "PAY-FAR",
			PostingDate: time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC), // 2 days diff
			Net:         amt,
			Party:       "BlueNet Broadband",
		},
		{
			VoucherNo:   "PAY-CLOSE",
			PostingDate: time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC), // 1 day diff
			Net:         amt,
			Party:       "BlueNet Broadband",
		},
	}

	res := MatchBankLines(bankLines, vouchers, 3)
	if len(res.Matched) != 1 {
		t.Fatalf("expected 1 match, got %d", len(res.Matched))
	}
	if res.Matched[0].Voucher.VoucherNo != "PAY-CLOSE" {
		t.Errorf("expected tie-breaker to pick PAY-CLOSE, got %s", res.Matched[0].Voucher.VoucherNo)
	}
	if res.Matched[0].Pass != 2 {
		t.Errorf("expected Pass 2, got %d", res.Matched[0].Pass)
	}
	if len(res.UnmatchedVouchers) != 1 || res.UnmatchedVouchers[0].VoucherNo != "PAY-FAR" {
		t.Errorf("expected PAY-FAR unmatched, got %+v", res.UnmatchedVouchers)
	}
}

func TestBankRecDateWindowEdges(t *testing.T) {
	bankDate := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)
	amt := money.Paise(500000)

	bankLines := []store.BankLine{
		{
			TxnID:       "TXN-EDGE-PASS",
			TxnDate:     bankDate,
			AmountPaise: amt,
			Narration:   "Deposit 1",
		},
		{
			TxnID:       "TXN-EDGE-FAIL",
			TxnDate:     bankDate,
			AmountPaise: amt * 2,
			Narration:   "Deposit 2",
		},
	}

	vouchers := []BankVoucher{
		{
			VoucherNo:   "V-3-DAYS",
			PostingDate: time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC), // 3 days: within window 3
			Net:         amt,
		},
		{
			VoucherNo:   "V-4-DAYS",
			PostingDate: time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC), // 4 days: outside window 3
			Net:         amt * 2,
		},
	}

	res := MatchBankLines(bankLines, vouchers, 3)
	if len(res.Matched) != 1 || res.Matched[0].BankLine.TxnID != "TXN-EDGE-PASS" {
		t.Fatalf("expected TXN-EDGE-PASS to match, got %v", res.Matched)
	}
	if len(res.UnmatchedBank) != 1 || res.UnmatchedBank[0].TxnID != "TXN-EDGE-FAIL" {
		t.Errorf("expected TXN-EDGE-FAIL unmatched, got %v", res.UnmatchedBank)
	}
	if len(res.UnmatchedVouchers) != 1 || res.UnmatchedVouchers[0].VoucherNo != "V-4-DAYS" {
		t.Errorf("expected V-4-DAYS unmatched, got %v", res.UnmatchedVouchers)
	}
}

func TestBankRecClassifyLeftovers(t *testing.T) {
	d := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)

	res := BankRecResult{
		UnmatchedBank: []store.BankLine{
			// 1. Bank charge: withdrawal under ₹10,000 with SMS/fee narration
			{
				TxnID:       "BNK-CHG",
				TxnDate:     d,
				AmountPaise: money.Paise(-118000), // -₹1,180
				Narration:   "SMS/ACCT CHARGES INCL GST",
			},
			// 2. Gateway settlement: deposit with PG SETTL
			{
				TxnID:       "BNK-PG",
				TxnDate:     d,
				AmountPaise: money.Paise(9800000),
				Narration:   "PG SETTL 0915 BATCH 7781",
			},
			// 3. Unexplained large withdrawal
			{
				TxnID:       "BNK-LARGE",
				TxnDate:     d,
				AmountPaise: money.Paise(-50000000),
				Narration:   "UNKNOWN WIRE TRANSFER",
			},
		},
		UnmatchedVouchers: []BankVoucher{
			{
				VoucherNo: "ACC-PAY-UNMATCHED",
				Net:       money.Paise(-1000000),
				EntryIDs:  []string{"GLE-01"},
			},
		},
	}

	findings := classifyFindings(res, "sharma")
	if len(findings) != 4 {
		t.Fatalf("expected 4 findings, got %d", len(findings))
	}

	byType := make(map[string][]Finding)
	for _, f := range findings {
		byType[f.Type] = append(byType[f.Type], f)
	}

	// 1. unrecorded_bank_charge
	charges := byType[TypeUnrecordedBankCharge]
	if len(charges) != 1 {
		t.Fatalf("expected 1 unrecorded_bank_charge, got %d", len(charges))
	}
	if charges[0].Keys["bank_txn_id"] != "BNK-CHG" {
		t.Errorf("bank charge txn_id = %q, want BNK-CHG", charges[0].Keys["bank_txn_id"])
	}
	if charges[0].AmountPaise == nil || *charges[0].AmountPaise != money.Paise(118000) {
		t.Errorf("bank charge amount = %v, want 118000", charges[0].AmountPaise)
	}

	// 2. unmatched_bank_line
	unmatchedBank := byType[TypeUnmatchedBankLine]
	if len(unmatchedBank) != 2 {
		t.Fatalf("expected 2 unmatched_bank_line, got %d", len(unmatchedBank))
	}
	var pgFound bool
	for _, ub := range unmatchedBank {
		if ub.Keys["bank_txn_id"] == "BNK-PG" {
			pgFound = true
			if ub.Keys["hint"] != "gateway" {
				t.Errorf("expected hint=gateway, got %q", ub.Keys["hint"])
			}
		}
	}
	if !pgFound {
		t.Error("expected BNK-PG in unmatched bank lines")
	}

	// 3. unmatched_ledger_entry
	unmatchedGL := byType[TypeUnmatchedLedgerEntry]
	if len(unmatchedGL) != 1 {
		t.Fatalf("expected 1 unmatched_ledger_entry, got %d", len(unmatchedGL))
	}
	if unmatchedGL[0].Keys["gl_entry"] != "ACC-PAY-UNMATCHED" {
		t.Errorf("gl_entry = %q, want ACC-PAY-UNMATCHED", unmatchedGL[0].Keys["gl_entry"])
	}
}

type fakeBooksReaderForBankRec struct {
	glEntries []frappe.GLEntry
	payments  []frappe.PaymentEntry
}

func (f *fakeBooksReaderForBankRec) TrialBalance(_ context.Context, _ string, _, _ time.Time) (books.TB, error) {
	return books.TB{}, nil
}
func (f *fakeBooksReaderForBankRec) GLEntries(_ context.Context, _ string, _, _ time.Time) ([]frappe.GLEntry, error) {
	return f.glEntries, nil
}
func (f *fakeBooksReaderForBankRec) PurchaseInvoices(_ context.Context, _ string, _, _ time.Time) ([]frappe.PurchaseInvoice, error) {
	return nil, nil
}
func (f *fakeBooksReaderForBankRec) SalesInvoices(_ context.Context, _ string, _, _ time.Time) ([]frappe.SalesInvoice, error) {
	return nil, nil
}
func (f *fakeBooksReaderForBankRec) PaymentEntries(_ context.Context, _ string, _, _ time.Time) ([]frappe.PaymentEntry, error) {
	return f.payments, nil
}
func (f *fakeBooksReaderForBankRec) AccountHistory(_ context.Context, _, _ string, _ string, _ int) ([]books.MonthTotal, error) {
	return nil, nil
}
func (f *fakeBooksReaderForBankRec) RecurringSuppliers(_ context.Context, _ string, _ string, _, _, _ int) ([]RecurringSupplier, error) {
	return nil, nil
}

type fakeEvidenceReaderForBankRec struct {
	bankLines []store.BankLine
}

func (f *fakeEvidenceReaderForBankRec) BankLines(_ context.Context, _ string, _, _ time.Time) ([]store.BankLine, error) {
	return f.bankLines, nil
}
func (f *fakeEvidenceReaderForBankRec) GSTR2BEntries(_ context.Context, _ string, _ string) ([]store.GSTR2BEntry, error) {
	return nil, nil
}

func TestBankRecCheckEndToEnd(t *testing.T) {
	ctx := context.Background()
	date := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)

	bReader := &fakeBooksReaderForBankRec{
		glEntries: []frappe.GLEntry{
			// Legitimate payment matched via reference
			{
				Name:        "GLE-1",
				Company:     "sharma",
				Account:     "HDFC Current 0001 - STPL",
				Credit:      money.Paise(5000000), // withdrawal 50k
				PostingDate: date,
				VoucherType: "Payment Entry",
				VoucherNo:   "PAY-001",
			},
		},
		payments: []frappe.PaymentEntry{
			{
				Name:        "PAY-001",
				ReferenceNo: "CHQ-888",
			},
		},
	}

	eReader := &fakeEvidenceReaderForBankRec{
		bankLines: []store.BankLine{
			// Matched by reference
			{
				TxnID:       "TXN-MATCH",
				TxnDate:     date,
				AmountPaise: money.Paise(-5000000),
				Narration:   "CHEQUE PAYMENT",
				Ref:         strPtr("CHQ-888"),
			},
			// Planted unrecorded bank charge
			{
				TxnID:       "TXN-CHG",
				TxnDate:     date,
				AmountPaise: money.Paise(-100000), // -₹1,000
				Narration:   "BANK CHARGES FOR SEPTEMBER",
			},
		},
	}

	in := Inputs{
		Company:  "sharma",
		Month:    "2026-09",
		Books:    bReader,
		Evidence: eReader,
		Rules: seed.Rules{
			BankMatch: seed.BankMatchRules{DateWindowDays: 3},
		},
	}

	check := &BankRecCheck{}
	findings, err := check.Run(ctx, in)
	if err != nil {
		t.Fatalf("check.Run: %v", err)
	}

	// Only the unrecorded bank charge should remain as a finding
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if findings[0].Type != TypeUnrecordedBankCharge {
		t.Errorf("finding type = %q, want unrecorded_bank_charge", findings[0].Type)
	}
	if findings[0].Keys["bank_txn_id"] != "TXN-CHG" {
		t.Errorf("bank_txn_id = %q, want TXN-CHG", findings[0].Keys["bank_txn_id"])
	}
}

func BenchmarkMatchBankLines5000(b *testing.B) {
	n := 5000
	baseDate := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

	bankLines := make([]store.BankLine, n)
	vouchers := make([]BankVoucher, n)

	for i := 0; i < n; i++ {
		d := baseDate.AddDate(0, 0, i%25)
		amt := money.Paise(-((i + 1) * 100))
		ref := fmt.Sprintf("REF-%05d", i)

		bankLines[i] = store.BankLine{
			TxnID:       fmt.Sprintf("TXN-%05d", i),
			TxnDate:     d,
			AmountPaise: amt,
			Narration:   fmt.Sprintf("SUPPLIER PAYMENT %d", i),
			Ref:         &ref,
		}

		vouchers[i] = BankVoucher{
			VoucherNo:   fmt.Sprintf("PAY-%05d", i),
			PostingDate: d,
			Net:         amt,
			ReferenceNo: ref,
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res := MatchBankLines(bankLines, vouchers, 3)
		if len(res.Matched) != n {
			b.Fatalf("expected %d matches, got %d", n, len(res.Matched))
		}
	}
}
