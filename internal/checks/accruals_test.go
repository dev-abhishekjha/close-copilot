package checks

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/abhishekjha/close-copilot/internal/company"
	"github.com/abhishekjha/close-copilot/internal/ledger"
	"github.com/abhishekjha/close-copilot/internal/money"
)

type missingAccrualArgs struct {
	company                         string
	before                          string
	lookback, minOccurrences, bandP int
}

// missingAccrualBooks applies the real recurring rule to history invoices and
// serves the month's invoices and GL rows.
type missingAccrualBooks struct {
	history   []ledger.PurchaseInvoice
	month     []ledger.PurchaseInvoice
	gl        []ledger.GLEntry
	recurErr  error
	invoErr   error
	glErr     error
	gotArgs   *missingAccrualArgs
	invCalls  int
	glCalls   int
	recurCall int
}

func (f *missingAccrualBooks) TrialBalance(context.Context, string, time.Time, time.Time) (ledger.TB, error) {
	return ledger.TB{}, nil
}
func (f *missingAccrualBooks) GLEntries(context.Context, string, time.Time, time.Time) ([]ledger.GLEntry, error) {
	f.glCalls++
	return f.gl, f.glErr
}
func (f *missingAccrualBooks) PurchaseInvoices(context.Context, string, time.Time, time.Time) ([]ledger.PurchaseInvoice, error) {
	f.invCalls++
	return f.month, f.invoErr
}
func (f *missingAccrualBooks) SalesInvoices(context.Context, string, time.Time, time.Time) ([]ledger.SalesInvoice, error) {
	return nil, nil
}
func (f *missingAccrualBooks) PaymentEntries(context.Context, string, time.Time, time.Time) ([]ledger.PaymentEntry, error) {
	return nil, nil
}
func (f *missingAccrualBooks) AccountHistory(context.Context, string, string, string, int) ([]ledger.MonthTotal, error) {
	return nil, nil
}
func (f *missingAccrualBooks) RecurringSuppliers(_ context.Context, co, before string, lookback, minOcc, band int) ([]RecurringSupplier, error) {
	f.recurCall++
	f.gotArgs = &missingAccrualArgs{co, before, lookback, minOcc, band}
	if f.recurErr != nil {
		return nil, f.recurErr
	}
	return ledger.RecurringSuppliers(f.history, minOcc, band), nil
}

func missingAccrualInv(supplier string, m time.Month, amt int64) ledger.PurchaseInvoice {
	return ledger.PurchaseInvoice{
		Name: fmt.Sprintf("PINV-%s-%d", supplier, m), Supplier: supplier,
		PostingDate: time.Date(2026, m, 5, 0, 0, 0, 0, time.UTC), GrandTotal: money.Paise(amt),
	}
}

// missingAccrualHistory gives a supplier a steady bill in May-Aug 2026.
func missingAccrualHistory(supplier string, amt int64) []ledger.PurchaseInvoice {
	var out []ledger.PurchaseInvoice
	for m := time.May; m <= time.August; m++ {
		out = append(out, missingAccrualInv(supplier, m, amt))
	}
	return out
}

func missingAccrualInputs(b *missingAccrualBooks) Inputs {
	return Inputs{
		Company: "Sharma Traders", Month: "2026-09", Books: b,
		Rules: company.Rules{Accruals: company.AccrualRules{LookbackMonths: 4, MinOccurrences: 3, AmountBandPct: 20}},
	}
}

func missingAccrualJE(party, partyType string, cancelled, opening bool) ledger.GLEntry {
	return ledger.GLEntry{
		Name: "GL-1", VoucherType: "Journal Entry", VoucherNo: "JV-1", PartyType: partyType, Party: party,
		PostingDate: time.Date(2026, time.September, 30, 0, 0, 0, 0, time.UTC),
		IsCancelled: cancelled, IsOpening: opening,
	}
}

func TestMissingAccrualCheck(t *testing.T) {
	const amt = 1200000
	sepInv := missingAccrualInv("Acme", time.September, amt)
	sepReturn := sepInv
	sepReturn.IsReturn = true

	tests := []struct {
		name    string
		history []ledger.PurchaseInvoice
		month   []ledger.PurchaseInvoice
		gl      []ledger.GLEntry
		want    []string
	}{
		{"skips month is flagged", missingAccrualHistory("Acme", amt), nil, nil, []string{"Acme"}},
		{"billed this month", missingAccrualHistory("Acme", amt), []ledger.PurchaseInvoice{sepInv}, nil, nil},
		{"journal entry names supplier", missingAccrualHistory("Acme", amt), nil,
			[]ledger.GLEntry{missingAccrualJE("Acme", "Supplier", false, false)}, nil},
		{"journal entry other party", missingAccrualHistory("Acme", amt), nil,
			[]ledger.GLEntry{missingAccrualJE("Other", "Supplier", false, false)}, []string{"Acme"}},
		{"journal entry customer party", missingAccrualHistory("Acme", amt), nil,
			[]ledger.GLEntry{missingAccrualJE("Acme", "Customer", false, false)}, []string{"Acme"}},
		{"cancelled journal row", missingAccrualHistory("Acme", amt), nil,
			[]ledger.GLEntry{missingAccrualJE("Acme", "Supplier", true, false)}, []string{"Acme"}},
		{"opening journal row", missingAccrualHistory("Acme", amt), nil,
			[]ledger.GLEntry{missingAccrualJE("Acme", "Supplier", false, true)}, []string{"Acme"}},
		{"purchase return alone", missingAccrualHistory("Acme", amt), []ledger.PurchaseInvoice{sepReturn}, nil, []string{"Acme"}},
		{"new supplier one month", []ledger.PurchaseInvoice{missingAccrualInv("Newco", time.August, amt)}, nil, nil, nil},
		{"amounts outside band", []ledger.PurchaseInvoice{
			missingAccrualInv("Wild", time.May, 100000), missingAccrualInv("Wild", time.June, 900000),
			missingAccrualInv("Wild", time.July, 100000), missingAccrualInv("Wild", time.August, 900000),
		}, nil, nil, nil},
		{"clean month", nil, nil, nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := &missingAccrualBooks{history: tt.history, month: tt.month, gl: tt.gl}
			got, err := (&MissingAccrualCheck{}).Run(context.Background(), missingAccrualInputs(b))
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %d findings, want %d: %+v", len(got), len(tt.want), got)
			}
			for i, f := range got {
				if f.Keys["supplier"] != tt.want[i] || f.Keys["month"] != "2026-09" {
					t.Errorf("finding %d keys = %v", i, f.Keys)
				}
				if f.Type != TypeMissingAccrual || f.Severity != SeverityMedium || f.Status != StatusOpen {
					t.Errorf("finding %d type/severity/status = %s/%s/%s", i, f.Type, f.Severity, f.Status)
				}
				if f.AmountPaise == nil || *f.AmountPaise != amt {
					t.Errorf("finding %d amount = %v, want %d", i, f.AmountPaise, amt)
				}
				if f.Action == nil || *f.Action != ActionAccrue {
					t.Errorf("finding %d action = %v", i, f.Action)
				}
				if len(f.Evidence) != 3 {
					t.Errorf("finding %d evidence = %d refs, want 3", i, len(f.Evidence))
				}
			}
		})
	}
}

func TestMissingAccrualRulesReachReader(t *testing.T) {
	b := &missingAccrualBooks{}
	in := missingAccrualInputs(b)
	in.Rules.Accruals = company.AccrualRules{LookbackMonths: 6, MinOccurrences: 2, AmountBandPct: 35}
	if _, err := (&MissingAccrualCheck{}).Run(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	want := missingAccrualArgs{"Sharma Traders", "2026-09", 6, 2, 35}
	if b.gotArgs == nil || *b.gotArgs != want {
		t.Errorf("reader args = %+v, want %+v", b.gotArgs, want)
	}
	if b.invCalls != 0 || b.glCalls != 0 {
		t.Errorf("invoice/GL readers called for empty recurring list: %d/%d", b.invCalls, b.glCalls)
	}
}

func TestMissingAccrualInvalidMonth(t *testing.T) {
	in := missingAccrualInputs(&missingAccrualBooks{})
	in.Month = "September"
	var perr *time.ParseError
	_, err := (&MissingAccrualCheck{}).Run(context.Background(), in)
	if !errors.As(err, &perr) {
		t.Fatalf("err = %v, want wrapped *time.ParseError", err)
	}
}

func TestMissingAccrualReaderErrors(t *testing.T) {
	boom := errors.New("boom")
	tests := []struct {
		name string
		b    *missingAccrualBooks
	}{
		{"recurring", &missingAccrualBooks{recurErr: boom}},
		{"invoices", &missingAccrualBooks{history: missingAccrualHistory("Acme", 100000), invoErr: boom}},
		{"gl", &missingAccrualBooks{history: missingAccrualHistory("Acme", 100000), glErr: boom}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := (&MissingAccrualCheck{}).Run(context.Background(), missingAccrualInputs(tt.b))
			if !errors.Is(err, boom) {
				t.Fatalf("err = %v, want wrapped boom", err)
			}
			if len(got) != 0 {
				t.Errorf("findings = %d on error, want 0", len(got))
			}
		})
	}
}

func TestMissingAccrualSixPlanted(t *testing.T) {
	var history []ledger.PurchaseInvoice
	want := map[string]int64{}
	for i := 1; i <= 6; i++ {
		s := fmt.Sprintf("Supplier %d", i)
		amt := int64(i) * 150000
		want[s] = amt
		history = append(history, missingAccrualHistory(s, amt)...)
	}
	// A seventh supplier bills this month and must not be flagged.
	history = append(history, missingAccrualHistory("Steady", 500000)...)
	month := []ledger.PurchaseInvoice{missingAccrualInv("Steady", time.September, 500000)}

	b := &missingAccrualBooks{history: history, month: month}
	got, err := (&MissingAccrualCheck{}).Run(context.Background(), missingAccrualInputs(b))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 6 {
		t.Fatalf("got %d findings, want 6", len(got))
	}
	keys := map[string]bool{}
	for i, f := range got {
		s := f.Keys["supplier"]
		if a, ok := want[s]; !ok || f.AmountPaise == nil || int64(*f.AmountPaise) != a {
			t.Errorf("unexpected finding %v amount %v", f.Keys, f.AmountPaise)
		}
		k := DedupeKey(f)
		if keys[k] {
			t.Errorf("duplicate dedupe key %q", k)
		}
		keys[k] = true
		if i > 0 && got[i-1].Keys["supplier"] >= s {
			t.Errorf("output not sorted by supplier at %d", i)
		}
	}
}
