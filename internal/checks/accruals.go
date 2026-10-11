package checks

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/abhishekjha/close-copilot/internal/ledger"
)

// MissingAccrualCheck flags suppliers that bill every month (by the shared
// recurring-supplier rule) but have no invoice or Journal Entry in the month
// being closed. The expense is probably missing and should be accrued at the
// supplier's median amount.
type MissingAccrualCheck struct{}

var _ Check = (*MissingAccrualCheck)(nil)

// Name returns the identifier for this check.
func (c *MissingAccrualCheck) Name() string {
	return "accruals"
}

// Run reports one missing_accrual finding per recurring supplier with no
// non-return Purchase Invoice and no Journal Entry GL row naming it as party
// in the month.
//
// A Journal Entry "names" a supplier only through the Party on its GL rows.
// There is no Journal Entry reader, so an entry that mentions the supplier
// only in its remarks does not suppress the finding.
func (c *MissingAccrualCheck) Run(ctx context.Context, in Inputs) ([]Finding, error) {
	start, err := time.Parse("2006-01", in.Month)
	if err != nil {
		return nil, fmt.Errorf("checks: accruals invalid month %q: %w", in.Month, err)
	}
	end := start.AddDate(0, 1, -1)

	a := in.Rules.Accruals
	recurring, err := in.Books.RecurringSuppliers(ctx, in.Company, in.Month, a.LookbackMonths, a.MinOccurrences, a.AmountBandPct)
	if err != nil {
		return nil, fmt.Errorf("checks: accruals load recurring suppliers: %w", err)
	}
	if len(recurring) == 0 {
		return nil, nil
	}

	invoices, err := in.Books.PurchaseInvoices(ctx, in.Company, start, end)
	if err != nil {
		return nil, fmt.Errorf("checks: accruals load purchase invoices: %w", err)
	}
	gl, err := in.Books.GLEntries(ctx, in.Company, start, end)
	if err != nil {
		return nil, fmt.Errorf("checks: accruals load gl entries: %w", err)
	}

	accrBilled := accrBilledSet(invoices, start, end)
	accrJournaled := accrJournalSet(gl, start, end)

	recurringArgs := map[string]any{
		"company":         in.Company,
		"before_month":    in.Month,
		"lookback_months": a.LookbackMonths,
		"min_occurrences": a.MinOccurrences,
		"amount_band_pct": a.AmountBandPct,
	}
	rangeArgs := map[string]string{
		"company":   in.Company,
		"from_date": start.Format("2006-01-02"),
		"to_date":   end.Format("2006-01-02"),
	}

	var findings []Finding
	seen := make(map[string]bool)
	for _, r := range recurring {
		if r.Supplier == "" || seen[r.Supplier] || accrBilled[r.Supplier] || accrJournaled[r.Supplier] {
			continue
		}
		seen[r.Supplier] = true
		amount := r.MedianAmount
		action := ActionAccrue
		findings = append(findings, Finding{
			Type:        TypeMissingAccrual,
			Severity:    SeverityMedium,
			Title:       fmt.Sprintf("Missing accrual: %s (%s)", r.Supplier, amount.Format()),
			AmountPaise: &amount,
			Keys:        map[string]string{"supplier": r.Supplier, "month": in.Month},
			Evidence: []EvidenceRef{
				BooksEvidence("list_recurring_suppliers", recurringArgs, r.Supplier),
				BooksEvidence("list_purchase_invoices", rangeArgs),
				BooksEvidence("list_gl_entries", rangeArgs),
			},
			Action: &action,
			Status: StatusOpen,
		})
	}
	sort.Slice(findings, func(i, j int) bool {
		return findings[i].Keys["supplier"] < findings[j].Keys["supplier"]
	})
	return findings, nil
}

func accrInMonth(d, start, end time.Time) bool {
	return !d.Before(start) && !d.After(end)
}

// accrBilledSet returns suppliers with a non-return invoice posted in the month.
func accrBilledSet(invoices []ledger.PurchaseInvoice, start, end time.Time) map[string]bool {
	out := make(map[string]bool)
	for _, inv := range invoices {
		if inv.IsReturn || !accrInMonth(inv.PostingDate, start, end) {
			continue
		}
		out[inv.Supplier] = true
	}
	return out
}

// accrJournalSet returns supplier parties on live Journal Entry GL rows in the month.
func accrJournalSet(gl []ledger.GLEntry, start, end time.Time) map[string]bool {
	out := make(map[string]bool)
	for _, e := range gl {
		if e.IsCancelled || e.IsOpening || e.VoucherType != "Journal Entry" || e.PartyType != "Supplier" || e.Party == "" {
			continue
		}
		if !accrInMonth(e.PostingDate, start, end) {
			continue
		}
		out[e.Party] = true
	}
	return out
}
