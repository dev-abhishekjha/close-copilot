package books

// Trial balance and account history (CC-204), computed from GL Entries in
// exact paise rather than through ERPNext's report endpoints.
//
// Sign convention: every balance is a signed money.Paise with debit
// positive and credit negative. Debit and Credit columns are period totals
// and never negative.
//
// Opening rule (Indian fiscal year, 1 April to 31 March):
//
//   - Balance-sheet accounts (root type Asset, Liability, Equity) open with
//     every entry dated before the period.
//   - Profit-and-loss accounts (root type Income, Expense) restart at zero
//     on 1 April: they open with the entries from the start of the fiscal
//     year containing the period's first day up to the day before it.
//
// Cancelled GL Entries (is_cancelled = 1, which ERPNext sets on both the
// original and its reversal) never count.

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/abhishekjha/close-copilot/internal/frappe"
	"github.com/abhishekjha/close-copilot/internal/money"
)

const (
	dateLayout  = "2006-01-02"
	monthLayout = "2006-01"

	// MaxHistoryMonths is the most months AccountHistory returns.
	MaxHistoryMonths = 36
)

// ErrUnbalanced is wrapped by TrialBalance when the ledger doesn't balance:
// period debits differ from period credits, or the opening or closing
// balances don't sum to zero.
var ErrUnbalanced = errors.New("books: trial balance does not balance")

// Ledger is the data source for the reports. FrappeLedger reads ERPNext;
// tests use fakes.
type Ledger interface {
	// GLEntries returns the company's non-cancelled GL Entries posted from
	// from to to, both inclusive. A zero from means "from the beginning".
	GLEntries(ctx context.Context, company string, from, to time.Time) ([]frappe.GLEntry, error)
	// Accounts returns the company's accounts, group accounts included.
	Accounts(ctx context.Context, company string) ([]frappe.Account, error)
}

// FrappeLedger is a Ledger over the ERPNext REST API. Use the bot key
// (ERP_API_KEY, Accounts User), which is all it needs.
type FrappeLedger struct {
	C *frappe.Client
}

var _ Ledger = FrappeLedger{}

// GLEntries lists the company's GL Entries with is_cancelled = 0 and a
// posting date between from and to (or up to to when from is zero).
func (f FrappeLedger) GLEntries(ctx context.Context, company string, from, to time.Time) ([]frappe.GLEntry, error) {
	if f.C == nil {
		return nil, errors.New("books: FrappeLedger has no client")
	}
	if company == "" {
		return nil, errors.New("books: gl entries: empty company")
	}
	if to.IsZero() {
		return nil, errors.New("books: gl entries: zero end date")
	}
	if !from.IsZero() && from.After(to) {
		return nil, fmt.Errorf("books: gl entries: from %s is after to %s", from.Format(dateLayout), to.Format(dateLayout))
	}
	dates := []any{"posting_date", "<=", to.Format(dateLayout)}
	if !from.IsZero() {
		dates = []any{"posting_date", "between", []string{from.Format(dateLayout), to.Format(dateLayout)}}
	}
	raw, err := frappe.List[frappe.GLEntryRaw](ctx, f.C, frappe.DocTypeGLEntry, frappe.Query{
		Fields: frappe.Fields[frappe.GLEntryRaw](),
		Filters: [][]any{
			{"company", "=", company},
			dates,
			{"is_cancelled", "=", 0},
		},
		OrderBy: "name asc",
	})
	if err != nil {
		return nil, fmt.Errorf("books: gl entries for %s: %w", company, err)
	}
	out := make([]frappe.GLEntry, 0, len(raw))
	for _, r := range raw {
		g, err := r.Domain()
		if err != nil {
			return nil, fmt.Errorf("books: gl entries for %s: %w", company, err)
		}
		out = append(out, g)
	}
	return out, nil
}

// Accounts lists every Account of the company, group accounts included.
func (f FrappeLedger) Accounts(ctx context.Context, company string) ([]frappe.Account, error) {
	if f.C == nil {
		return nil, errors.New("books: FrappeLedger has no client")
	}
	if company == "" {
		return nil, errors.New("books: accounts: empty company")
	}
	raw, err := frappe.List[frappe.AccountRaw](ctx, f.C, frappe.DocTypeAccount, frappe.Query{
		Fields:  frappe.Fields[frappe.AccountRaw](),
		Filters: [][]any{{"company", "=", company}},
		OrderBy: "name asc",
	})
	if err != nil {
		return nil, fmt.Errorf("books: accounts for %s: %w", company, err)
	}
	out := make([]frappe.Account, 0, len(raw))
	for _, r := range raw {
		a, err := r.Domain()
		if err != nil {
			return nil, fmt.Errorf("books: accounts for %s: %w", company, err)
		}
		out = append(out, a)
	}
	return out, nil
}

// FiscalYearStart returns 1 April of the Indian fiscal year (1 April to 31
// March) containing d, at UTC midnight: 2026-03-31 gives 2025-04-01 and
// 2026-04-01 gives 2026-04-01.
func FiscalYearStart(d time.Time) time.Time {
	y := d.Year()
	if d.Month() < time.April {
		y--
	}
	return time.Date(y, time.April, 1, 0, 0, 0, 0, time.UTC)
}

// TB is a trial balance for one company and period. Rows hold leaf
// accounts with any non-zero value, ordered by root type (Asset,
// Liability, Equity, Income, Expense) and then account name.
type TB struct {
	Company string    `json:"company"`
	From    time.Time `json:"from_date"`
	To      time.Time `json:"to_date"`
	Rows    []TBRow   `json:"rows"`
	Totals  TBTotals  `json:"totals"`
}

// TBRow is one account's line. Opening and Closing are signed balances
// (debit positive); Debit and Credit are the period's totals (never
// negative); Closing = Opening + Debit - Credit.
type TBRow struct {
	Account     string      `json:"account"`
	AccountName string      `json:"account_name"`
	RootType    string      `json:"root_type"`
	Opening     money.Paise `json:"opening"`
	Debit       money.Paise `json:"debit"`
	Credit      money.Paise `json:"credit"`
	Closing     money.Paise `json:"closing"`
}

// TBTotals sums the rows' columns. In a balanced ledger Debit equals Credit
// and Opening and Closing are zero.
type TBTotals struct {
	Opening money.Paise `json:"opening"`
	Debit   money.Paise `json:"debit"`
	Credit  money.Paise `json:"credit"`
	Closing money.Paise `json:"closing"`
}

// rootOrder is the row order of the root types; it also lists the valid ones.
var rootOrder = map[string]int{
	frappe.RootTypeAsset:     0,
	frappe.RootTypeLiability: 1,
	frappe.RootTypeEquity:    2,
	frappe.RootTypeIncome:    3,
	frappe.RootTypeExpense:   4,
}

func isProfitAndLoss(rootType string) bool {
	return rootType == frappe.RootTypeIncome || rootType == frappe.RootTypeExpense
}

// TrialBalance returns the company's trial balance from from to to (both
// inclusive, UTC midnight, from not after to), following the opening rule
// and sign convention in the file comment. For a period that crosses 1
// April, a profit-and-loss account opens at the fiscal year of from and
// its period columns include both sides of 1 April.
//
// It reads every GL Entry up to to in one call, since balance-sheet
// openings need all earlier entries.
//
// TrialBalance checks its own result: if period debits differ from period
// credits, or the openings or closings don't sum to zero, it returns an
// error wrapping ErrUnbalanced that names the gap. An entry on an account
// missing from the ledger's accounts, on a group account or with an unknown
// root type is also an error.
func TrialBalance(ctx context.Context, l Ledger, company string, from, to time.Time) (TB, error) {
	if l == nil {
		return TB{}, errors.New("books: trial balance: nil ledger")
	}
	if company == "" {
		return TB{}, errors.New("books: trial balance: empty company")
	}
	if err := checkDate("from", from); err != nil {
		return TB{}, fmt.Errorf("books: trial balance: %w", err)
	}
	if err := checkDate("to", to); err != nil {
		return TB{}, fmt.Errorf("books: trial balance: %w", err)
	}
	if from.After(to) {
		return TB{}, fmt.Errorf("books: trial balance: from %s is after to %s", from.Format(dateLayout), to.Format(dateLayout))
	}
	period := fmt.Sprintf("%s %s..%s", company, from.Format(dateLayout), to.Format(dateLayout))

	accounts, err := accountIndex(ctx, l, company)
	if err != nil {
		return TB{}, fmt.Errorf("books: trial balance for %s: %w", period, err)
	}
	entries, err := l.GLEntries(ctx, company, time.Time{}, to)
	if err != nil {
		return TB{}, fmt.Errorf("books: trial balance for %s: %w", period, err)
	}

	fyStart := FiscalYearStart(from)
	rows := map[string]*TBRow{}
	var earlierPL money.Paise // P&L net before fyStart: carried nowhere
	for _, e := range entries {
		if e.IsCancelled {
			continue
		}
		acc, err := checkEntry(e, company, accounts)
		if err != nil {
			return TB{}, fmt.Errorf("books: trial balance for %s: %w", period, err)
		}
		if e.PostingDate.After(to) {
			return TB{}, fmt.Errorf("books: trial balance for %s: GL Entry %s is dated %s, after the period", period, e.Name, e.PostingDate.Format(dateLayout))
		}
		pl := isProfitAndLoss(acc.RootType)
		if e.PostingDate.Before(from) && pl && e.PostingDate.Before(fyStart) {
			earlierPL += e.Debit - e.Credit
			continue
		}
		r := rows[acc.Name]
		if r == nil {
			r = &TBRow{Account: acc.Name, AccountName: acc.AccountName, RootType: acc.RootType}
			rows[acc.Name] = r
		}
		if e.PostingDate.Before(from) {
			r.Opening += e.Debit - e.Credit
		} else {
			r.Debit += e.Debit
			r.Credit += e.Credit
		}
	}

	tb := TB{Company: company, From: from, To: to, Rows: []TBRow{}}
	for _, r := range rows {
		r.Closing = r.Opening + r.Debit - r.Credit
		if r.Opening == 0 && r.Debit == 0 && r.Credit == 0 && r.Closing == 0 {
			continue
		}
		tb.Rows = append(tb.Rows, *r)
		tb.Totals.Opening += r.Opening
		tb.Totals.Debit += r.Debit
		tb.Totals.Credit += r.Credit
		tb.Totals.Closing += r.Closing
	}
	slices.SortFunc(tb.Rows, func(a, b TBRow) int {
		return cmp.Or(
			cmp.Compare(rootOrder[a.RootType], rootOrder[b.RootType]),
			cmp.Compare(a.AccountName, b.AccountName),
			cmp.Compare(a.Account, b.Account),
		)
	})

	if err := checkBalanced(tb.Totals, earlierPL); err != nil {
		return TB{}, fmt.Errorf("books: trial balance for %s: %w", period, err)
	}
	return tb, nil
}

// checkBalanced returns an error wrapping ErrUnbalanced naming every gap.
// earlierPL is the profit-and-loss net before the fiscal year; when it is
// non-zero it is the likely cause of an opening gap (a year not closed to
// equity), and the error says so.
func checkBalanced(t TBTotals, earlierPL money.Paise) error {
	var gaps []string
	if t.Debit != t.Credit {
		gaps = append(gaps, fmt.Sprintf("period debits %s and credits %s differ by %s",
			t.Debit.Format(), t.Credit.Format(), (t.Debit-t.Credit).Format()))
	}
	if t.Opening != 0 {
		gaps = append(gaps, fmt.Sprintf("opening balances sum to %s, not zero", t.Opening.Format()))
	}
	if t.Closing != 0 {
		gaps = append(gaps, fmt.Sprintf("closing balances sum to %s, not zero", t.Closing.Format()))
	}
	if len(gaps) == 0 {
		return nil
	}
	if earlierPL != 0 {
		gaps = append(gaps, fmt.Sprintf("earlier fiscal years leave a profit-and-loss net of %s that no entry closes to equity", earlierPL.Format()))
	}
	return fmt.Errorf("%w: %s", ErrUnbalanced, strings.Join(gaps, "; "))
}

// MonthTotal is one calendar month of an account's entries. Net = Debit -
// Credit (debit positive).
type MonthTotal struct {
	Month  string      `json:"month"` // YYYY-MM
	Debit  money.Paise `json:"debit"`
	Credit money.Paise `json:"credit"`
	Net    money.Paise `json:"net"`
}

// AccountHistory returns the debit, credit and net of one account for each
// of the months calendar months ending with through (YYYY-MM), oldest
// first. months is 1 to MaxHistoryMonths; months without entries are
// zeros. The account must be a leaf account of the company.
func AccountHistory(ctx context.Context, l Ledger, company, account string, through string, months int) ([]MonthTotal, error) {
	if l == nil {
		return nil, errors.New("books: account history: nil ledger")
	}
	if company == "" {
		return nil, errors.New("books: account history: empty company")
	}
	if months < 1 || months > MaxHistoryMonths {
		return nil, fmt.Errorf("books: account history: months %d is outside 1 to %d", months, MaxHistoryMonths)
	}
	last, err := time.Parse(monthLayout, through)
	if err != nil || last.Format(monthLayout) != through {
		return nil, fmt.Errorf("books: account history: through %q is not a YYYY-MM month", through)
	}
	first := last.AddDate(0, -(months - 1), 0)
	end := last.AddDate(0, 1, -1)

	accounts, err := accountIndex(ctx, l, company)
	if err != nil {
		return nil, fmt.Errorf("books: account history of %s: %w", account, err)
	}
	acc, ok := accounts[account]
	if !ok {
		return nil, fmt.Errorf("books: account history: no account %q in %s", account, company)
	}
	if acc.IsGroup {
		return nil, fmt.Errorf("books: account history: %q is a group account; GL Entries post only to leaf accounts", account)
	}

	out := make([]MonthTotal, months)
	index := make(map[string]int, months)
	for i := range out {
		m := first.AddDate(0, i, 0).Format(monthLayout)
		out[i].Month = m
		index[m] = i
	}

	entries, err := l.GLEntries(ctx, company, first, end)
	if err != nil {
		return nil, fmt.Errorf("books: account history of %s: %w", account, err)
	}
	for _, e := range entries {
		if e.IsCancelled || e.Account != account {
			continue
		}
		if e.Company != company {
			return nil, fmt.Errorf("books: account history of %s: GL Entry %s belongs to %q, not %q", account, e.Name, e.Company, company)
		}
		i, ok := index[e.PostingDate.Format(monthLayout)]
		if !ok || e.PostingDate.Before(first) || e.PostingDate.After(end) {
			return nil, fmt.Errorf("books: account history of %s: GL Entry %s is dated %s, outside %s..%s",
				account, e.Name, e.PostingDate.Format(dateLayout), first.Format(dateLayout), end.Format(dateLayout))
		}
		out[i].Debit += e.Debit
		out[i].Credit += e.Credit
	}
	for i := range out {
		out[i].Net = out[i].Debit - out[i].Credit
	}
	return out, nil
}

// accountIndex loads the company's accounts by name, checking each one's
// company and root type.
func accountIndex(ctx context.Context, l Ledger, company string) (map[string]frappe.Account, error) {
	list, err := l.Accounts(ctx, company)
	if err != nil {
		return nil, err
	}
	out := make(map[string]frappe.Account, len(list))
	for _, a := range list {
		if a.Company != company {
			return nil, fmt.Errorf("account %q belongs to %q, not %q", a.Name, a.Company, company)
		}
		if _, ok := rootOrder[a.RootType]; !ok {
			return nil, fmt.Errorf("account %q has unknown root type %q", a.Name, a.RootType)
		}
		out[a.Name] = a
	}
	return out, nil
}

// checkEntry returns the account of a GL Entry, or an error if the entry
// belongs to another company or posts to an unknown or group account.
func checkEntry(e frappe.GLEntry, company string, accounts map[string]frappe.Account) (frappe.Account, error) {
	if e.Company != company {
		return frappe.Account{}, fmt.Errorf("GL Entry %s belongs to %q, not %q", e.Name, e.Company, company)
	}
	acc, ok := accounts[e.Account]
	if !ok {
		return frappe.Account{}, fmt.Errorf("GL Entry %s posts to %q, which is not an account of %s", e.Name, e.Account, company)
	}
	if acc.IsGroup {
		return frappe.Account{}, fmt.Errorf("GL Entry %s posts to group account %q", e.Name, e.Account)
	}
	return acc, nil
}

// checkDate requires a non-zero date at UTC midnight.
func checkDate(name string, d time.Time) error {
	if d.IsZero() {
		return fmt.Errorf("%s date is zero", name)
	}
	if d.Location() != time.UTC || d.Hour() != 0 || d.Minute() != 0 || d.Second() != 0 || d.Nanosecond() != 0 {
		return fmt.Errorf("%s date %s is not at UTC midnight", name, d.Format(time.RFC3339Nano))
	}
	return nil
}
