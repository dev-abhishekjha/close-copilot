package checks

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/abhishekjha/close-copilot/internal/ledger"
	"github.com/abhishekjha/close-copilot/internal/money"
	"github.com/abhishekjha/close-copilot/internal/store"
)

type dupBooks struct {
	payments    []ledger.PaymentEntry
	invoices    []ledger.PurchaseInvoice
	paymentsErr error
	invoicesErr error
}

func (f *dupBooks) TrialBalance(context.Context, string, time.Time, time.Time) (ledger.TB, error) {
	return ledger.TB{}, nil
}
func (f *dupBooks) GLEntries(context.Context, string, time.Time, time.Time) ([]ledger.GLEntry, error) {
	return nil, nil
}
func (f *dupBooks) PurchaseInvoices(context.Context, string, time.Time, time.Time) ([]ledger.PurchaseInvoice, error) {
	return f.invoices, f.invoicesErr
}
func (f *dupBooks) SalesInvoices(context.Context, string, time.Time, time.Time) ([]ledger.SalesInvoice, error) {
	return nil, nil
}
func (f *dupBooks) PaymentEntries(context.Context, string, time.Time, time.Time) ([]ledger.PaymentEntry, error) {
	return f.payments, f.paymentsErr
}
func (f *dupBooks) AccountHistory(context.Context, string, string, string, int) ([]ledger.MonthTotal, error) {
	return nil, nil
}
func (f *dupBooks) RecurringSuppliers(context.Context, string, string, int, int, int) ([]RecurringSupplier, error) {
	return nil, nil
}

type dupEvidence struct {
	lines []store.BankLine
	err   error
}

func (f *dupEvidence) BankLines(context.Context, string, time.Time, time.Time) ([]store.BankLine, error) {
	return f.lines, f.err
}
func (f *dupEvidence) GSTR2BEntries(context.Context, string, string) ([]store.GSTR2BEntry, error) {
	return nil, nil
}

func dupDay(m time.Month, d int) time.Time { return time.Date(2026, m, d, 0, 0, 0, 0, time.UTC) }

func allocPay(name, supplier string, amt int64, date time.Time, inv string) ledger.PaymentEntry {
	return ledger.PaymentEntry{
		Name: name, PaymentType: "Pay", PartyType: "Supplier", Party: supplier,
		PaidAmount: money.Paise(amt), PostingDate: date,
		References: []ledger.PaymentEntryReference{{ReferenceDoctype: "Purchase Invoice", ReferenceName: inv, AllocatedAmount: money.Paise(amt)}},
	}
}

func advPay(name, supplier string, amt int64, date time.Time, remarks string) ledger.PaymentEntry {
	return ledger.PaymentEntry{
		Name: name, PaymentType: "Pay", PartyType: "Supplier", Party: supplier,
		PaidAmount: money.Paise(amt), UnallocatedAmount: money.Paise(amt), PostingDate: date, Remarks: remarks,
	}
}

func paidInv(name, supplier, bill string) ledger.PurchaseInvoice {
	return ledger.PurchaseInvoice{Name: name, Supplier: supplier, BillNo: bill, PostingDate: dupDay(time.August, 1)}
}

func bankW(id string, amt int64, narr string) store.BankLine {
	return store.BankLine{TxnID: id, TxnDate: dupDay(time.September, 10), Narration: narr, AmountPaise: money.Paise(-amt)}
}

func TestDuplicatePaymentCheck(t *testing.T) {
	const amt = 4500000
	tests := []struct {
		name     string
		payments []ledger.PaymentEntry
		invoices []ledger.PurchaseInvoice
		lines    []store.BankLine
		want     map[string]string // invoice -> severity
	}{
		{
			name: "rule A hit",
			payments: []ledger.PaymentEntry{
				allocPay("PE-1", "Cloudly", amt, dupDay(time.September, 2), "PINV-1"),
				advPay("PE-2", "Cloudly", amt, dupDay(time.September, 12), ""),
			},
			want: map[string]string{"PINV-1": SeverityMedium},
		},
		{
			name:     "rule B hit",
			payments: []ledger.PaymentEntry{advPay("PE-2", "Cloudly", amt, dupDay(time.September, 12), "Payment for CLD/2026/0912")},
			invoices: []ledger.PurchaseInvoice{paidInv("PINV-9", "Cloudly", "cld/2026/0912")},
			want:     map[string]string{"PINV-9": SeverityMedium},
		},
		{
			name: "both agree",
			payments: []ledger.PaymentEntry{
				allocPay("PE-1", "Cloudly", amt, dupDay(time.September, 2), "PINV-9"),
				advPay("PE-2", "Cloudly", amt, dupDay(time.September, 12), "Payment for CLD/2026/0912"),
			},
			invoices: []ledger.PurchaseInvoice{paidInv("PINV-9", "Cloudly", "CLD/2026/0912")},
			want:     map[string]string{"PINV-9": SeverityMedium},
		},
		{
			name: "both disagree prefers rule B",
			payments: []ledger.PaymentEntry{
				allocPay("PE-1", "Cloudly", amt, dupDay(time.September, 2), "PINV-A"),
				advPay("PE-2", "Cloudly", amt, dupDay(time.September, 12), "Payment for CLD/2026/0912"),
			},
			invoices: []ledger.PurchaseInvoice{paidInv("PINV-B", "Cloudly", "CLD/2026/0912")},
			want:     map[string]string{"PINV-B": SeverityMedium},
		},
		{
			name: "legitimate second payment for a different invoice",
			payments: []ledger.PaymentEntry{
				allocPay("PE-1", "Cloudly", amt, dupDay(time.September, 2), "PINV-1"),
				allocPay("PE-2", "Cloudly", amt, dupDay(time.September, 12), "PINV-2"),
			},
			want: map[string]string{},
		},
		{
			name: "same amount to a different supplier",
			payments: []ledger.PaymentEntry{
				allocPay("PE-1", "Other", amt, dupDay(time.September, 2), "PINV-1"),
				advPay("PE-2", "Cloudly", amt, dupDay(time.September, 12), ""),
			},
			want: map[string]string{},
		},
		{
			name: "outside 30 days",
			payments: []ledger.PaymentEntry{
				allocPay("PE-1", "Cloudly", amt, dupDay(time.August, 20), "PINV-1"),
				advPay("PE-2", "Cloudly", amt, dupDay(time.September, 25), ""),
			},
			want: map[string]string{},
		},
		{
			name:     "remarks name an invoice with outstanding",
			payments: []ledger.PaymentEntry{advPay("PE-2", "Cloudly", amt, dupDay(time.September, 12), "Payment for CLD/2026/0912")},
			invoices: []ledger.PurchaseInvoice{{Name: "PINV-9", Supplier: "Cloudly", BillNo: "CLD/2026/0912", OutstandingAmount: 100}},
			want:     map[string]string{},
		},
		{
			name: "bank corroboration on",
			payments: []ledger.PaymentEntry{
				allocPay("PE-1", "Cloudly Pvt Ltd", amt, dupDay(time.September, 2), "PINV-1"),
				advPay("PE-2", "Cloudly Pvt Ltd", amt, dupDay(time.September, 12), ""),
			},
			lines: []store.BankLine{bankW("B1", amt, "NEFT CLOUDLY INFRA"), bankW("B2", amt, "NEFT-cloudly-infra")},
			want:  map[string]string{"PINV-1": SeverityHigh},
		},
		{
			name: "bank corroboration off with one withdrawal",
			payments: []ledger.PaymentEntry{
				allocPay("PE-1", "Cloudly Pvt Ltd", amt, dupDay(time.September, 2), "PINV-1"),
				advPay("PE-2", "Cloudly Pvt Ltd", amt, dupDay(time.September, 12), ""),
			},
			lines: []store.BankLine{bankW("B1", amt, "NEFT CLOUDLY INFRA"), bankW("B2", amt, "NEFT SOMEONE ELSE")},
			want:  map[string]string{"PINV-1": SeverityMedium},
		},
		{
			name:     "unresolvable invoice",
			payments: []ledger.PaymentEntry{advPay("PE-2", "Cloudly", amt, dupDay(time.September, 12), "advance")},
			want:     map[string]string{},
		},
		{
			name: "two suspects for one invoice",
			payments: []ledger.PaymentEntry{
				allocPay("PE-1", "Cloudly", amt, dupDay(time.September, 2), "PINV-1"),
				advPay("PE-2", "Cloudly", amt, dupDay(time.September, 12), ""),
				advPay("PE-3", "Cloudly", amt, dupDay(time.September, 14), ""),
			},
			want: map[string]string{"PINV-1": SeverityMedium},
		},
		{
			name: "prior month suspect is not reported",
			payments: []ledger.PaymentEntry{
				allocPay("PE-1", "Cloudly", amt, dupDay(time.August, 20), "PINV-1"),
				advPay("PE-2", "Cloudly", amt, dupDay(time.August, 25), ""),
			},
			want: map[string]string{},
		},
		{name: "clean month", want: map[string]string{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := Inputs{Company: "C", Month: "2026-09", Books: &dupBooks{payments: tc.payments, invoices: tc.invoices}, Evidence: &dupEvidence{lines: tc.lines}}
			got, err := (&DuplicatePaymentCheck{}).Run(context.Background(), in)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %d findings, want %d: %+v", len(got), len(tc.want), got)
			}
			for _, f := range got {
				sev, ok := tc.want[f.Keys["invoice"]]
				if !ok || f.Type != TypeDuplicateVendorPayment || f.Severity != sev || f.AmountPaise == nil || *f.AmountPaise != amt {
					t.Errorf("unexpected finding %+v", f)
				}
			}
		})
	}
}

func TestDuplicateTwoSuspectsEvidence(t *testing.T) {
	in := Inputs{Company: "C", Month: "2026-09", Books: &dupBooks{payments: []ledger.PaymentEntry{
		allocPay("PE-1", "Cloudly", 100, dupDay(time.September, 2), "PINV-1"),
		advPay("PE-2", "Cloudly", 100, dupDay(time.September, 12), ""),
		advPay("PE-3", "Cloudly", 100, dupDay(time.September, 14), ""),
	}}, Evidence: &dupEvidence{}}
	got, err := (&DuplicatePaymentCheck{}).Run(context.Background(), in)
	if err != nil || len(got) != 1 {
		t.Fatalf("got %v, %v", got, err)
	}
	ids := got[0].Evidence[0].IDs
	if fmt.Sprint(ids) != "[PE-2 PE-1 PE-3]" {
		t.Errorf("payment ids = %v", ids)
	}
}

func TestDuplicateReaderErrors(t *testing.T) {
	boom := errors.New("boom")
	tests := []struct {
		name string
		b    *dupBooks
		e    *dupEvidence
	}{
		{"payments", &dupBooks{paymentsErr: boom}, &dupEvidence{}},
		{"invoices", &dupBooks{invoicesErr: boom}, &dupEvidence{}},
		{"bank lines", &dupBooks{}, &dupEvidence{err: boom}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := (&DuplicatePaymentCheck{}).Run(context.Background(), Inputs{Company: "C", Month: "2026-09", Books: tc.b, Evidence: tc.e})
			if !errors.Is(err, boom) {
				t.Errorf("err = %v, want wrapping boom", err)
			}
		})
	}
}

func TestDuplicateSixPlanted(t *testing.T) {
	var payments []ledger.PaymentEntry
	var invoices []ledger.PurchaseInvoice
	want := map[string]int64{}
	for i := 0; i < 6; i++ {
		sup := fmt.Sprintf("Supplier %d", i)
		inv := fmt.Sprintf("ACC-PINV-2026-%05d", 40+i)
		bill := fmt.Sprintf("CLD/2026/%04d", 900+i)
		amt := int64(1000000 + i*250000)
		invoices = append(invoices, paidInv(inv, sup, bill))
		payments = append(payments, allocPay(fmt.Sprintf("PE-%d", i), sup, amt, dupDay(time.September, 3+i), inv))
		payments = append(payments, advPay(fmt.Sprintf("PE-D%d", i), sup, amt, dupDay(time.September, 15+i), "Payment for "+bill))
		want[inv] = amt
	}
	// A legitimate payment of the same amount for another invoice.
	payments = append(payments, allocPay("PE-OK", "Supplier 0", 1000000, dupDay(time.September, 20), "ACC-PINV-2026-00099"))
	in := Inputs{Company: "C", Month: "2026-09", Books: &dupBooks{payments: payments, invoices: invoices}, Evidence: &dupEvidence{}}
	got, err := (&DuplicatePaymentCheck{}).Run(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 6 {
		t.Fatalf("got %d findings, want 6", len(got))
	}
	for i, f := range got {
		if i > 0 && got[i-1].Keys["invoice"] >= f.Keys["invoice"] {
			t.Errorf("output not sorted at %d", i)
		}
		if a, ok := want[f.Keys["invoice"]]; !ok || int64(*f.AmountPaise) != a {
			t.Errorf("unexpected finding %+v", f)
		}
	}
}

func TestDuplicateNormInvoiceRef(t *testing.T) {
	if got := normInvoiceRef("cld/2026-09 12"); got != "CLD20260912" {
		t.Errorf("got %q", got)
	}
}
