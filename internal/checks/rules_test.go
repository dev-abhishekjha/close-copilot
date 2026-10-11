package checks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/abhishekjha/close-copilot/internal/company"
	"github.com/abhishekjha/close-copilot/internal/ledger"
	"github.com/abhishekjha/close-copilot/internal/money"
)

type rulesBooks struct {
	invoices []ledger.PurchaseInvoice
	err      error
}

func (f *rulesBooks) TrialBalance(context.Context, string, time.Time, time.Time) (ledger.TB, error) {
	return ledger.TB{}, nil
}
func (f *rulesBooks) GLEntries(context.Context, string, time.Time, time.Time) ([]ledger.GLEntry, error) {
	return nil, nil
}
func (f *rulesBooks) PurchaseInvoices(context.Context, string, time.Time, time.Time) ([]ledger.PurchaseInvoice, error) {
	return f.invoices, f.err
}
func (f *rulesBooks) SalesInvoices(context.Context, string, time.Time, time.Time) ([]ledger.SalesInvoice, error) {
	return nil, nil
}
func (f *rulesBooks) PaymentEntries(context.Context, string, time.Time, time.Time) ([]ledger.PaymentEntry, error) {
	return nil, nil
}
func (f *rulesBooks) AccountHistory(context.Context, string, string, string, int) ([]ledger.MonthTotal, error) {
	return nil, nil
}
func (f *rulesBooks) RecurringSuppliers(context.Context, string, string, int, int, int) ([]RecurringSupplier, error) {
	return nil, nil
}

func rulesDefault() company.Rules {
	var r company.Rules
	r.Capitalisation = company.CapitalisationRules{
		ThresholdINR:           50000,
		AssetKeywords:          []string{"laptop", "printer", "server", "furniture", "air conditioner"},
		WatchedExpenseAccounts: []string{"Repairs and Maintenance"},
	}
	r.Prepaid = company.PrepaidRules{MinAmountINR: 50000, Keywords: []string{"annual", "yearly", "12 months", "renewal"}}
	return r
}

func rulesItem(account, desc string, paise int64) ledger.PurchaseInvoiceItem {
	return ledger.PurchaseInvoiceItem{ExpenseAccount: account, Description: desc, Amount: money.Paise(paise)}
}

func rulesInv(name string, items ...ledger.PurchaseInvoiceItem) ledger.PurchaseInvoice {
	return ledger.PurchaseInvoice{
		Name: name, Supplier: "SUP-1", SupplierName: "Acme",
		PostingDate: time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC), Items: items,
	}
}

func rulesRun(t *testing.T, rules company.Rules, invs ...ledger.PurchaseInvoice) []Finding {
	t.Helper()
	c := &PolicyRulesCheck{}
	got, err := c.Run(context.Background(), Inputs{Company: "Sharma Traders", Month: "2026-09", Books: &rulesBooks{invoices: invs}, Rules: rules})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return got
}

func rulesSummary(fs []Finding) []string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Type+"|"+f.Keys["invoice"])
	}
	return out
}

func TestPolicyRules(t *testing.T) {
	const (
		repairs = "Repairs and Maintenance - STPL"
		sw      = "Software Subscriptions - STPL"
		prepaid = "Prepaid Expenses - STPL"
		office  = "Office Equipment - STPL"
	)
	custom := rulesDefault()
	custom.Capitalisation.ThresholdINR = 10000
	custom.Capitalisation.AssetKeywords = []string{"scanner"}
	custom.Prepaid.MinAmountINR = 10000
	custom.Prepaid.Keywords = []string{"licence"}

	tests := []struct {
		name  string
		rules company.Rules
		invs  []ledger.PurchaseInvoice
		want  []string
	}{
		{"capitalisation fires", rulesDefault(), []ledger.PurchaseInvoice{rulesInv("I1", rulesItem(repairs, "Laptop for office", 6200000))}, []string{"misclassified_expense|I1"}},
		{"capitalisation at threshold", rulesDefault(), []ledger.PurchaseInvoice{rulesInv("I1", rulesItem(repairs, "Printer", 5000000))}, []string{"misclassified_expense|I1"}},
		{"capitalisation one paisa below", rulesDefault(), []ledger.PurchaseInvoice{rulesInv("I1", rulesItem(repairs, "Printer", 4999999))}, nil},
		{"capitalisation 49,999 near miss", rulesDefault(), []ledger.PurchaseInvoice{rulesInv("I1", rulesItem(repairs, "Server rack", 4999900))}, nil},
		{"asset keyword in non-watched account", rulesDefault(), []ledger.PurchaseInvoice{rulesInv("I1", rulesItem(office, "Laptop, 14-inch business notebook for office use (capital asset)", 8000000))}, nil},
		{"keyword case-insensitive", rulesDefault(), []ledger.PurchaseInvoice{rulesInv("I1", rulesItem(repairs, "LAPTOP", 6000000))}, []string{"misclassified_expense|I1"}},
		{"abbreviation suffix matches", rulesDefault(), []ledger.PurchaseInvoice{rulesInv("I1", rulesItem("Repairs and Maintenance", "laptop", 6000000))}, []string{"misclassified_expense|I1"}},
		{"similar account does not match", rulesDefault(), []ledger.PurchaseInvoice{rulesInv("I1", rulesItem("Repairs and Maintenance Reserve - STPL", "laptop", 6000000))}, nil},
		{"empty account never capitalisation", rulesDefault(), []ledger.PurchaseInvoice{rulesInv("I1", rulesItem("", "laptop", 6000000))}, nil},
		{"prepaid fires", rulesDefault(), []ledger.PurchaseInvoice{rulesInv("I1", rulesItem(sw, "Annual licence", 6000000))}, []string{"prepaid_not_spread|I1"}},
		{"prepaid at threshold", rulesDefault(), []ledger.PurchaseInvoice{rulesInv("I1", rulesItem(sw, "Yearly plan", 5000000))}, []string{"prepaid_not_spread|I1"}},
		{"prepaid one paisa below", rulesDefault(), []ledger.PurchaseInvoice{rulesInv("I1", rulesItem(sw, "Yearly plan", 4999999))}, nil},
		{"annual maintenance visit under threshold", rulesDefault(), []ledger.PurchaseInvoice{rulesInv("I1", rulesItem(repairs, "Annual maintenance visit", 1200000))}, nil},
		{"annual maintenance visit over threshold", rulesDefault(), []ledger.PurchaseInvoice{rulesInv("I1", rulesItem(repairs, "Annual maintenance visit", 7000000))}, []string{"prepaid_not_spread|I1"}},
		{"genuine annual bill in prepaid", rulesDefault(), []ledger.PurchaseInvoice{rulesInv("I1", rulesItem(prepaid, "Annual insurance renewal, 12 months. Service period: 01-09-2026 to 31-08-2027", 9000000))}, nil},
		{"genuine prepaid starting next month", rulesDefault(), []ledger.PurchaseInvoice{rulesInv("I1", rulesItem(prepaid, "Annual renewal, 12 months. Service period: 01-10-2026 to 30-09-2027", 9000000))}, nil},
		{"cutoff fires", rulesDefault(), []ledger.PurchaseInvoice{rulesInv("I1", rulesItem("Rent - STPL", "Rent. Service period: 01-10-2026 to 31-10-2026", 3000000))}, []string{"wrong_period_posting|I1"}},
		{"cutoff flexible spacing and case", rulesDefault(), []ledger.PurchaseInvoice{rulesInv("I1", rulesItem("Rent - STPL", "service  period:  01-10-2026   TO 31-10-2026", 100))}, []string{"wrong_period_posting|I1"}},
		{"cutoff start on last day ok", rulesDefault(), []ledger.PurchaseInvoice{rulesInv("I1", rulesItem("Rent - STPL", "Service period: 30-09-2026 to 31-10-2026", 100))}, nil},
		{"cutoff own month ok", rulesDefault(), []ledger.PurchaseInvoice{rulesInv("I1", rulesItem("Rent - STPL", "Rent. Service period: 01-09-2026 to 30-09-2026", 3000000))}, nil},
		{"cutoff malformed date", rulesDefault(), []ledger.PurchaseInvoice{rulesInv("I1", rulesItem("Rent - STPL", "Service period: 31-02-2026 to 31-10-2026", 100))}, nil},
		{"cutoff garbled", rulesDefault(), []ledger.PurchaseInvoice{rulesInv("I1", rulesItem("Rent - STPL", "Service period: next month", 100))}, nil},
		{"cutoff missing", rulesDefault(), []ledger.PurchaseInvoice{rulesInv("I1", rulesItem("Rent - STPL", "Rent", 100))}, nil},
		{"custom rules change outcome", custom, []ledger.PurchaseInvoice{
			rulesInv("I1", rulesItem(repairs, "Scanner", 1500000)),
			rulesInv("I2", rulesItem(sw, "Licence", 1500000)),
			rulesInv("I3", rulesItem(repairs, "Laptop", 6000000)),
		}, []string{"misclassified_expense|I1", "prepaid_not_spread|I2"}},
		{"return skipped", rulesDefault(), []ledger.PurchaseInvoice{func() ledger.PurchaseInvoice {
			i := rulesInv("I1", rulesItem(repairs, "laptop", 6000000))
			i.IsReturn = true
			return i
		}()}, nil},
		{"one invoice two rules", rulesDefault(), []ledger.PurchaseInvoice{rulesInv("I1",
			rulesItem(repairs, "laptop", 6000000),
			rulesItem(sw, "Annual plan. Service period: 01-10-2026 to 30-09-2027", 6000000))},
			[]string{"misclassified_expense|I1", "prepaid_not_spread|I1", "wrong_period_posting|I1"}},
		{"sorted by type then invoice", rulesDefault(), []ledger.PurchaseInvoice{
			rulesInv("B", rulesItem(sw, "annual", 6000000)),
			rulesInv("A", rulesItem(sw, "annual", 6000000)),
			rulesInv("C", rulesItem(repairs, "laptop", 6000000)),
		}, []string{"misclassified_expense|C", "prepaid_not_spread|A", "prepaid_not_spread|B"}},
		{"clean month", rulesDefault(), []ledger.PurchaseInvoice{rulesInv("I1", rulesItem("Rent - STPL", "Monthly rent. Service period: 01-09-2026 to 30-09-2026", 3000000))}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := rulesSummary(rulesRun(t, tc.rules, tc.invs...))
			if !slices.Equal(got, tc.want) {
				t.Fatalf("findings = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPolicyRulesSumAndEvidence(t *testing.T) {
	inv := rulesInv("ACC-PINV-2026-00044",
		rulesItem("Repairs and Maintenance - STPL", "Laptop A", 6000000),
		rulesItem("Repairs and Maintenance - STPL", "Laptop B", 7000000))
	fs := rulesRun(t, rulesDefault(), inv)
	if len(fs) != 1 {
		t.Fatalf("got %d findings, want 1", len(fs))
	}
	f := fs[0]
	if f.AmountPaise == nil || *f.AmountPaise != 13000000 {
		t.Fatalf("amount = %v, want 13000000", f.AmountPaise)
	}
	if f.Status != StatusOpen || f.Severity != SeverityMedium || f.Action == nil || *f.Action != ActionReclassify {
		t.Fatalf("unexpected fields: %+v", f)
	}
	if !strings.Contains(f.Title, "ACC-PINV-2026-00044") || !strings.Contains(f.Title, "Acme") {
		t.Fatalf("title %q lacks invoice or supplier", f.Title)
	}
	if len(f.Evidence) != 1 {
		t.Fatalf("evidence = %d", len(f.Evidence))
	}
	var args map[string]string
	if err := json.Unmarshal(f.Evidence[0].Args, &args); err != nil {
		t.Fatal(err)
	}
	if args["rule"] != "rules.capitalisation" || args["account"] != "Repairs and Maintenance - STPL" {
		t.Fatalf("args = %v", args)
	}

	cf := rulesRun(t, rulesDefault(), rulesInv("X", rulesItem("Rent - STPL", "Service period: 01-10-2026 to 31-10-2026", 100)))
	if err := json.Unmarshal(cf[0].Evidence[0].Args, &args); err != nil {
		t.Fatal(err)
	}
	if args["rule"] != "rules.cutoff" || args["service_period_start"] != "2026-10-01" {
		t.Fatalf("cutoff args = %v", args)
	}
}

func TestPolicyRulesErrors(t *testing.T) {
	c := &PolicyRulesCheck{}
	if _, err := c.Run(context.Background(), Inputs{Month: "bad", Books: &rulesBooks{}, Rules: rulesDefault()}); err == nil {
		t.Fatal("want error for invalid month")
	}
	sentinel := errors.New("boom")
	_, err := c.Run(context.Background(), Inputs{Month: "2026-09", Books: &rulesBooks{err: sentinel}, Rules: rulesDefault()})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want wrapped sentinel", err)
	}
	if c.Name() != "rules" {
		t.Fatalf("name = %q", c.Name())
	}
}

func TestPolicyRulesFourPlanted(t *testing.T) {
	var invs []ledger.PurchaseInvoice
	want := map[string]int64{}
	for i := 1; i <= 4; i++ {
		sub := rulesInv(fmt.Sprintf("SUB-%d", i), rulesItem("Software Subscriptions - STPL", "Annual subscription, 12 months", int64(6000000+i*100000)))
		lap := rulesInv(fmt.Sprintf("LAP-%d", i), rulesItem("Repairs and Maintenance - STPL", "Laptop, 14-inch business notebook", int64(7000000+i*100000)))
		cut := rulesInv(fmt.Sprintf("CUT-%d", i), rulesItem("Rent - STPL", "Office rent. Service period: 01-10-2026 to 31-10-2026", int64(2000000+i*100000)))
		invs = append(invs, sub, lap, cut)
		want["prepaid_not_spread|"+sub.Name] = int64(sub.Items[0].Amount)
		want["misclassified_expense|"+lap.Name] = int64(lap.Items[0].Amount)
		want["wrong_period_posting|"+cut.Name] = int64(cut.Items[0].Amount)
	}
	for i := 1; i <= 10; i++ {
		invs = append(invs, rulesInv(fmt.Sprintf("ORD-%d", i), rulesItem("Rent - STPL", "Monthly bill. Service period: 01-09-2026 to 30-09-2026", 1500000)))
	}
	invs = append(invs,
		rulesInv("GEN-1", rulesItem("Prepaid Expenses - STPL", "Annual insurance renewal, 12 months. Service period: 01-09-2026 to 31-08-2027", 9000000)),
		rulesInv("GEN-2", rulesItem("Office Equipment - STPL", "Laptop, 14-inch business notebook for office use (capital asset)", 8000000)),
	)
	fs := rulesRun(t, rulesDefault(), invs...)
	if len(fs) != 12 {
		t.Fatalf("got %d findings %v, want 12", len(fs), rulesSummary(fs))
	}
	for _, f := range fs {
		k := f.Type + "|" + f.Keys["invoice"]
		w, ok := want[k]
		if !ok {
			t.Fatalf("unexpected finding %s", k)
		}
		if int64(*f.AmountPaise) != w {
			t.Fatalf("%s amount = %d, want %d", k, *f.AmountPaise, w)
		}
		delete(want, k)
	}
	if len(want) != 0 {
		t.Fatalf("missing findings: %v", want)
	}
}
