package checks

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/abhishekjha/close-copilot/internal/ledger"
	"github.com/abhishekjha/close-copilot/internal/money"
)

const (
	// varianceHistoryMonths is the history window: this month plus three prior.
	varianceHistoryMonths = 4
	// varianceTopEntries is how many of the month's largest GL entries are cited.
	varianceTopEntries = 5
	// varianceMaxInvoices caps the purchase-invoice IDs cited as evidence.
	varianceMaxInvoices = 20
	// varianceKeyRelated is the finding key LinkVariances fills.
	varianceKeyRelated = "related"
)

// VarianceCheck flags income and expense accounts whose month moved sharply
// against the average of the previous three months. Findings are
// informational (severity low) and unscored by the evals.
type VarianceCheck struct{}

var _ Check = (*VarianceCheck)(nil)

// Name returns the identifier for this check.
func (v *VarianceCheck) Name() string {
	return "variance"
}

func varianceAbs(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

// varianceFlag applies the integer tests. sum3 is the sum of the three prior
// oriented nets and cur this month's; the average is sum3/3 and is never
// divided out. It returns whether to flag and d3 = 3*cur - sum3.
func varianceFlag(cur, sum3 int64, pct, absINR int64) (bool, int64, error) {
	d3 := 3*cur - sum3
	ad := varianceAbs(d3)
	// |d3|*100 stays inside int64 while a net is below about 3e16 paise; return
	// an error rather than wrapping.
	if ad > math.MaxInt64/100 {
		return false, 0, fmt.Errorf("checks: variance amount %d paise too large", d3)
	}
	as := varianceAbs(sum3)
	if pct > 0 && as > math.MaxInt64/pct {
		return false, 0, fmt.Errorf("checks: variance average %d paise too large", sum3)
	}
	if absINR > math.MaxInt64/300 {
		return false, 0, fmt.Errorf("checks: variance abs threshold %d too large", absINR)
	}
	if ad <= 3*absINR*100 {
		return false, d3, nil
	}
	if sum3 != 0 && ad*100 <= pct*as {
		return false, d3, nil
	}
	return true, d3, nil
}

// varianceOrient makes a normal balance positive: expense is debit minus
// credit, income is credit minus debit.
func varianceOrient(rootType string, net money.Paise) int64 {
	if rootType == ledger.RootTypeIncome {
		return -int64(net)
	}
	return int64(net)
}

// Run executes the variance check.
func (v *VarianceCheck) Run(ctx context.Context, in Inputs) ([]Finding, error) {
	start, err := time.Parse("2006-01", in.Month)
	if err != nil {
		return nil, fmt.Errorf("checks: variance invalid month %q: %w", in.Month, err)
	}
	end := start.AddDate(0, 1, -1)
	from := start.AddDate(0, -(varianceHistoryMonths - 1), 0)

	tb, err := in.Books.TrialBalance(ctx, in.Company, from, end)
	if err != nil {
		return nil, fmt.Errorf("checks: variance load trial balance: %w", err)
	}
	var rows []ledger.TBRow
	for _, r := range tb.Rows {
		if (r.RootType == ledger.RootTypeIncome || r.RootType == ledger.RootTypeExpense) && r.Debit+r.Credit != 0 {
			rows = append(rows, r)
		}
	}
	slices.SortFunc(rows, func(a, b ledger.TBRow) int { return strings.Compare(a.Account, b.Account) })
	if len(rows) == 0 {
		return nil, nil
	}

	gl, err := in.Books.GLEntries(ctx, in.Company, start, end)
	if err != nil {
		return nil, fmt.Errorf("checks: variance load gl entries: %w", err)
	}
	byAccount := map[string][]ledger.GLEntry{}
	for _, e := range gl {
		if e.IsCancelled {
			continue
		}
		byAccount[e.Account] = append(byAccount[e.Account], e)
	}

	act := ActionInvestigate
	var findings []Finding
	for _, r := range rows {
		hist, err := in.Books.AccountHistory(ctx, in.Company, r.Account, in.Month, varianceHistoryMonths)
		if err != nil {
			return nil, fmt.Errorf("checks: variance load history for %q: %w", r.Account, err)
		}
		if len(hist) != varianceHistoryMonths || hist[varianceHistoryMonths-1].Month != in.Month {
			return nil, fmt.Errorf("checks: variance history for %q: want %d months ending %s, got %d rows",
				r.Account, varianceHistoryMonths, in.Month, len(hist))
		}
		cur := varianceOrient(r.RootType, hist[varianceHistoryMonths-1].Net)
		var sum3 int64
		var prior bool
		for _, h := range hist[:varianceHistoryMonths-1] {
			sum3 += varianceOrient(r.RootType, h.Net)
			prior = prior || h.Net != 0
		}
		if cur == 0 && !prior {
			continue
		}
		flag, d3, err := varianceFlag(cur, sum3, in.Rules.Variance.PctThreshold, in.Rules.Variance.AbsThresholdINR)
		if err != nil {
			return nil, fmt.Errorf("checks: variance account %q: %w", r.Account, err)
		}
		if !flag {
			continue
		}
		amt := money.Paise(varianceAbs(d3) / 3)
		dir := "up"
		if d3 < 0 {
			dir = "down"
		}
		findings = append(findings, Finding{
			Type:        TypeVariance,
			Severity:    SeverityLow,
			Title:       fmt.Sprintf("%s %s by %s against the 3-month average", r.Account, dir, amt.Format()),
			AmountPaise: &amt,
			Keys:        map[string]string{"account": r.Account, "month": in.Month},
			Evidence:    varianceEvidence(in, start, end, r.Account, byAccount[r.Account]),
			Action:      &act,
			Status:      StatusOpen,
		})
	}
	return findings, nil
}

func varianceEvidence(in Inputs, start, end time.Time, account string, entries []ledger.GLEntry) []EvidenceRef {
	sorted := slices.Clone(entries)
	size := func(e ledger.GLEntry) int64 { return varianceAbs(int64(e.Debit - e.Credit)) }
	slices.SortFunc(sorted, func(a, b ledger.GLEntry) int {
		if sa, sb := size(a), size(b); sa != sb {
			if sa > sb {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Name, b.Name)
	})
	var glIDs, invoices []string
	for i, e := range sorted {
		if i < varianceTopEntries {
			glIDs = append(glIDs, e.Name)
		}
		if e.VoucherType == "Purchase Invoice" && e.VoucherNo != "" &&
			len(invoices) < varianceMaxInvoices && !slices.Contains(invoices, e.VoucherNo) {
			invoices = append(invoices, e.VoucherNo)
		}
	}
	slices.Sort(invoices)
	ev := []EvidenceRef{
		BooksEvidence("get_account_history", map[string]any{
			"company": in.Company, "account": account, "through_month": in.Month, "months": varianceHistoryMonths,
		}),
		BooksEvidence("list_gl_entries", map[string]string{
			"company": in.Company, "from_date": start.Format("2006-01-02"), "to_date": end.Format("2006-01-02"), "account": account,
		}, glIDs...),
	}
	if len(invoices) > 0 {
		ev = append(ev, BooksEvidence("list_purchase_invoices", map[string]string{"company": in.Company, "month": in.Month}, invoices...))
	}
	return ev
}

// LinkVariances sets keys.related on each variance finding to the ID of the
// first non-variance finding on the same account and month, or whose invoice
// key is among the variance's purchase-invoice evidence. Call it after
// DedupeFindings, since related is part of DedupeKey. It returns a new slice,
// copies the maps it changes, and leaves a variance that already has related
// alone.
func LinkVariances(findings []Finding) []Finding {
	out := make([]Finding, len(findings))
	copy(out, findings)
	for i, v := range out {
		if v.Type != TypeVariance || v.Keys[varianceKeyRelated] != "" {
			continue
		}
		var invoices []string
		for _, ev := range v.Evidence {
			if ev.Tool == "list_purchase_invoices" {
				invoices = append(invoices, ev.IDs...)
			}
		}
		for _, p := range findings {
			if p.Type == TypeVariance {
				continue
			}
			byAcct := p.Keys["account"] != "" && p.Keys["account"] == v.Keys["account"] && p.Keys["month"] == v.Keys["month"]
			byInv := p.Keys["invoice"] != "" && slices.Contains(invoices, p.Keys["invoice"])
			if !byAcct && !byInv {
				continue
			}
			keys := make(map[string]string, len(v.Keys)+1)
			for k, val := range v.Keys {
				keys[k] = val
			}
			keys[varianceKeyRelated] = p.ID.String()
			out[i].Keys = keys
			break
		}
	}
	return out
}
