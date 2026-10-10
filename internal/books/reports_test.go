package books

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/frappe"
	"github.com/abhishekjha/close-copilot/internal/money"
)

const testCompany = "Test Traders Pvt Ltd"

func day(s string) time.Time {
	d, err := time.Parse(dateLayout, s)
	if err != nil {
		panic(err)
	}
	return d
}

// readList decodes a testdata/reports fixture shaped like an ERPNext list
// response ({"data": [...]}) with numbers kept as json.Number.
func readList[T any](t *testing.T, name string) []T {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "reports", name))
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var resp struct {
		Data []T `json:"data"`
	}
	if err := dec.Decode(&resp); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return resp.Data
}

func fixtureAccounts(t *testing.T) []frappe.Account {
	t.Helper()
	var out []frappe.Account
	for _, r := range readList[frappe.AccountRaw](t, "accounts.json") {
		a, err := r.Domain()
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, a)
	}
	return out
}

func fixtureEntries(t *testing.T) []frappe.GLEntry {
	t.Helper()
	var out []frappe.GLEntry
	for _, r := range readList[frappe.GLEntryRaw](t, "gl_entries.json") {
		g, err := r.Domain()
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, g)
	}
	return out
}

type glCall struct{ from, to time.Time }

// fakeLedger serves fixed accounts and entries. Like FrappeLedger it
// filters by company, date and is_cancelled, unless leaky is set, in which
// case it returns every entry (cancelled ones and other dates included).
type fakeLedger struct {
	accounts   []frappe.Account
	entries    []frappe.GLEntry
	leaky      bool
	accountErr error
	entryErr   error

	mu    sync.Mutex
	calls []glCall
}

func (f *fakeLedger) GLEntries(_ context.Context, company string, from, to time.Time) ([]frappe.GLEntry, error) {
	f.mu.Lock()
	f.calls = append(f.calls, glCall{from, to})
	f.mu.Unlock()
	if f.entryErr != nil {
		return nil, f.entryErr
	}
	if f.leaky {
		return f.entries, nil
	}
	var out []frappe.GLEntry
	for _, e := range f.entries {
		if e.Company != company || e.IsCancelled || e.PostingDate.After(to) {
			continue
		}
		if !from.IsZero() && e.PostingDate.Before(from) {
			continue
		}
		out = append(out, e)
	}
	return out, nil
}

func (f *fakeLedger) Accounts(_ context.Context, company string) ([]frappe.Account, error) {
	if f.accountErr != nil {
		return nil, f.accountErr
	}
	var out []frappe.Account
	for _, a := range f.accounts {
		if a.Company == company {
			out = append(out, a)
		}
	}
	return out, nil
}

func newFake(t *testing.T) *fakeLedger {
	t.Helper()
	return &fakeLedger{accounts: fixtureAccounts(t), entries: fixtureEntries(t)}
}

func entry(name, date, account string, debit, credit money.Paise) frappe.GLEntry {
	return frappe.GLEntry{Name: name, Company: testCompany, Account: account, Debit: debit, Credit: credit, PostingDate: day(date)}
}

func TestFakeLedgerHonoursFilter(t *testing.T) {
	f := newFake(t)
	got, err := f.GLEntries(t.Context(), testCompany, day("2026-09-01"), day("2026-09-30"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("got %d entries for September, want the 4 of JV-08 and JV-09", len(got))
	}
	for _, e := range got {
		if e.IsCancelled {
			t.Errorf("fake returned cancelled %s", e.Name)
		}
	}
}

func row(account, name, root string, opening, debit, credit, closing money.Paise) TBRow {
	return TBRow{Account: account, AccountName: name, RootType: root, Opening: opening, Debit: debit, Credit: credit, Closing: closing}
}

// The fixture in rupees (testdata/reports/gl_entries.json):
//
//	2025-05-10 JV-01 Bank dr / Capital cr 1000
//	2025-06-15 JV-02 Rent dr / Sales cr 200   (previous fiscal year)
//	2026-03-20 JV-03 Rent dr / Sales cr 40    (previous fiscal year)
//	2026-04-10 JV-04 Rent dr / Bank cr 300
//	2026-05-01 JV-05 Cash dr / Bank cr 50, reversed by JV-06 on 2026-05-02
//	2026-08-05 JV-07 Bank dr / Sales cr 500
//	2026-09-14 JV-08 Bank Charges dr / Bank cr 11.80
//	2026-09-20 JV-09 Rent dr / Creditors cr 100
//	2026-09-25 JV-10 Bank Charges dr / Bank cr 9.99, cancelled (with its reversal)
//	2026-10-02 JV-11 Bank dr / Sales cr 70
//	2026-12-15 JV-12 Bank dr / Sales cr 20
//	2027-01-10 JV-13 Rent dr / Bank cr 15
func TestTrialBalance(t *testing.T) {
	september := []TBRow{
		// Asset: carries JV-01 from the previous fiscal year.
		row("Bank - TT", "Bank", "Asset", 120000, 0, 1180, 118820),
		row("Creditors - TT", "Creditors", "Liability", 0, 0, 10000, -10000),
		row("Capital - TT", "Capital", "Equity", -100000, 0, 0, -100000),
		// Income and Expense: only entries from 2026-04-01 open the period.
		row("Sales - TT", "Sales", "Income", -50000, 0, 0, -50000),
		row("Bank Charges - TT", "Bank Charges", "Expense", 0, 1180, 0, 1180),
		row("Rent - TT", "Rent", "Expense", 30000, 10000, 0, 40000),
	}
	tests := []struct {
		name     string
		leaky    bool
		from, to string
		want     []TBRow
		totals   TBTotals
	}{
		{
			name: "september: asset carries the previous year, expense restarts on 1 April",
			from: "2026-09-01", to: "2026-09-30",
			want:   september,
			totals: TBTotals{Opening: 0, Debit: 11180, Credit: 11180, Closing: 0},
		},
		{
			name:  "a ledger that returns cancelled entries gives the same balance",
			leaky: true,
			from:  "2026-09-01", to: "2026-09-30",
			want:   september,
			totals: TBTotals{Opening: 0, Debit: 11180, Credit: 11180, Closing: 0},
		},
		{
			name: "period crossing 1 April opens at the fiscal year of from",
			from: "2026-03-01", to: "2026-04-30",
			want: []TBRow{
				row("Bank - TT", "Bank", "Asset", 100000, 0, 30000, 70000),
				row("Capital - TT", "Capital", "Equity", -100000, 0, 0, -100000),
				row("Sales - TT", "Sales", "Income", -20000, 0, 4000, -24000),
				row("Rent - TT", "Rent", "Expense", 20000, 34000, 0, 54000),
			},
			totals: TBTotals{Opening: 0, Debit: 34000, Credit: 34000, Closing: 0},
		},
		{
			name: "first day of the fiscal year: income and expense open at zero",
			from: "2026-04-01", to: "2026-04-01",
			want: []TBRow{
				row("Bank - TT", "Bank", "Asset", 100000, 0, 0, 100000),
				row("Capital - TT", "Capital", "Equity", -100000, 0, 0, -100000),
			},
		},
		{
			name: "a month with offsetting entries: cash nets to zero and is omitted",
			from: "2026-06-01", to: "2026-06-30",
			want: []TBRow{
				row("Bank - TT", "Bank", "Asset", 70000, 0, 0, 70000),
				row("Capital - TT", "Capital", "Equity", -100000, 0, 0, -100000),
				row("Rent - TT", "Rent", "Expense", 30000, 0, 0, 30000),
			},
		},
		{
			name: "cash moving in and out within the period is shown",
			from: "2026-05-01", to: "2026-05-31",
			want: []TBRow{
				row("Bank - TT", "Bank", "Asset", 70000, 5000, 5000, 70000),
				row("Cash - TT", "Cash", "Asset", 0, 5000, 5000, 0),
				row("Capital - TT", "Capital", "Equity", -100000, 0, 0, -100000),
				row("Rent - TT", "Rent", "Expense", 30000, 0, 0, 30000),
			},
			totals: TBTotals{Debit: 10000, Credit: 10000},
		},
		{
			name: "before any entry the trial balance is empty",
			from: "2025-01-01", to: "2025-01-31",
			want: []TBRow{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake(t)
			from, to := day(tt.from), day(tt.to)
			if tt.leaky {
				// Cancelled entries come through; later ones are an error
				// (TestTrialBalanceErrors), so drop those.
				f.leaky = true
				f.entries = notAfter(f.entries, to)
			}
			tb, err := TrialBalance(t.Context(), f, testCompany, from, to)
			if err != nil {
				t.Fatalf("TrialBalance: %v", err)
			}
			if tb.Company != testCompany || !tb.From.Equal(from) || !tb.To.Equal(to) {
				t.Errorf("header = %q %s %s", tb.Company, tb.From, tb.To)
			}
			if !reflect.DeepEqual(tb.Rows, tt.want) {
				t.Errorf("rows:\n got %+v\nwant %+v", tb.Rows, tt.want)
			}
			if tb.Totals != tt.totals {
				t.Errorf("totals = %+v, want %+v", tb.Totals, tt.totals)
			}
			// The function's own invariant, asserted again here.
			if tb.Totals.Debit != tb.Totals.Credit || tb.Totals.Opening != 0 || tb.Totals.Closing != 0 {
				t.Errorf("totals don't balance: %+v", tb.Totals)
			}
			for _, r := range tb.Rows {
				if r.Closing != r.Opening+r.Debit-r.Credit {
					t.Errorf("%s: closing %d != opening %d + debit %d - credit %d", r.Account, r.Closing, r.Opening, r.Debit, r.Credit)
				}
				if r.Debit < 0 || r.Credit < 0 {
					t.Errorf("%s: negative period column", r.Account)
				}
			}
			// One fetch, from the beginning up to to.
			if len(f.calls) != 1 || !f.calls[0].from.IsZero() || !f.calls[0].to.Equal(to) {
				t.Errorf("GLEntries calls = %+v, want one from zero to %s", f.calls, tt.to)
			}
		})
	}
}

func notAfter(entries []frappe.GLEntry, to time.Time) []frappe.GLEntry {
	var out []frappe.GLEntry
	for _, e := range entries {
		if !e.PostingDate.After(to) {
			out = append(out, e)
		}
	}
	return out
}

func TestTrialBalanceUnbalanced(t *testing.T) {
	tests := []struct {
		name  string
		extra []frappe.GLEntry
		want  []string
	}{
		{
			name:  "a one-sided entry in the period",
			extra: []frappe.GLEntry{entry("X1", "2026-09-10", "Bank - TT", 25000, 0)},
			want:  []string{"period debits ₹361.80 and credits ₹111.80 differ by ₹250.00", "closing balances sum to ₹250.00"},
		},
		{
			name:  "a one-sided entry before the period",
			extra: []frappe.GLEntry{entry("X2", "2026-08-10", "Creditors - TT", 0, 700)},
			want:  []string{"opening balances sum to -₹7.00", "closing balances sum to -₹7.00"},
		},
		{
			name: "an earlier year's expense never closed to equity",
			extra: []frappe.GLEntry{
				entry("X3", "2026-03-20", "Bank Charges - TT", 500, 0),
				entry("X4", "2026-03-20", "Bank - TT", 0, 500),
			},
			want: []string{"opening balances sum to -₹5.00", "profit-and-loss net of ₹5.00"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake(t)
			f.entries = append(f.entries, tt.extra...)
			_, err := TrialBalance(t.Context(), f, testCompany, day("2026-09-01"), day("2026-09-30"))
			if !errors.Is(err, ErrUnbalanced) {
				t.Fatalf("err = %v, want ErrUnbalanced", err)
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q does not name %q", err, w)
				}
			}
		})
	}
}

func TestTrialBalanceErrors(t *testing.T) {
	ist := time.FixedZone("IST", 5*3600+1800)
	tests := []struct {
		name     string
		company  string
		from, to time.Time
		edit     func(*fakeLedger)
		want     string
	}{
		{name: "from after to", from: day("2026-09-30"), to: day("2026-09-01"), want: "is after"},
		{name: "from not midnight", from: day("2026-09-01").Add(time.Hour), to: day("2026-09-30"), want: "not at UTC midnight"},
		{name: "to not UTC", from: day("2026-09-01"), to: time.Date(2026, 9, 30, 0, 0, 0, 0, ist), want: "not at UTC midnight"},
		{name: "zero from", to: day("2026-09-30"), want: "from date is zero"},
		{name: "zero to", from: day("2026-09-01"), want: "to date is zero"},
		{name: "empty company", company: "-", from: day("2026-09-01"), to: day("2026-09-30"), want: "empty company"},
		{
			name: "entry on an unknown account", from: day("2026-09-01"), to: day("2026-09-30"),
			edit: func(f *fakeLedger) {
				f.entries = append(f.entries, entry("X1", "2026-09-02", "Ghost - TT", 100, 0), entry("X2", "2026-09-02", "Bank - TT", 0, 100))
			},
			want: `posts to "Ghost - TT", which is not an account`,
		},
		{
			name: "entry on a group account", from: day("2026-09-01"), to: day("2026-09-30"),
			edit: func(f *fakeLedger) {
				f.entries = append(f.entries, entry("X1", "2026-09-02", "Application of Funds (Assets) - TT", 100, 0))
			},
			want: "group account",
		},
		{
			name: "account with an unknown root type", from: day("2026-09-01"), to: day("2026-09-30"),
			edit: func(f *fakeLedger) {
				f.accounts = append(f.accounts, frappe.Account{Name: "Odd - TT", Company: testCompany, RootType: "Assets"})
			},
			want: `unknown root type "Assets"`,
		},
		{
			name: "entry from another company", from: day("2026-09-01"), to: day("2026-09-30"),
			edit: func(f *fakeLedger) {
				f.leaky = true
				f.entries = []frappe.GLEntry{{Name: "X1", Company: "Other Co", Account: "Bank - TT", PostingDate: day("2026-09-02")}}
			},
			want: `belongs to "Other Co"`,
		},
		{
			name: "ledger returns an entry after the period", from: day("2026-09-01"), to: day("2026-09-30"),
			edit: func(f *fakeLedger) { f.leaky = true },
			want: "after the period",
		},
		{
			name: "ledger error", from: day("2026-09-01"), to: day("2026-09-30"),
			edit: func(f *fakeLedger) { f.entryErr = errors.New("boom") },
			want: "boom",
		},
		{
			name: "accounts error", from: day("2026-09-01"), to: day("2026-09-30"),
			edit: func(f *fakeLedger) { f.accountErr = errors.New("no accounts") },
			want: "no accounts",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake(t)
			if tt.edit != nil {
				tt.edit(f)
			}
			company := testCompany
			if tt.company == "-" {
				company = ""
			}
			_, err := TrialBalance(t.Context(), f, company, tt.from, tt.to)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want one containing %q", err, tt.want)
			}
			if errors.Is(err, ErrUnbalanced) {
				t.Errorf("err %v should not be ErrUnbalanced", err)
			}
		})
	}
	if _, err := TrialBalance(t.Context(), nil, testCompany, day("2026-09-01"), day("2026-09-30")); err == nil {
		t.Error("nil ledger: no error")
	}
}

func TestFiscalYearStart(t *testing.T) {
	tests := []struct{ in, want string }{
		{"2026-03-31", "2025-04-01"},
		{"2026-04-01", "2026-04-01"},
		{"2026-04-30", "2026-04-01"},
		{"2026-09-15", "2026-04-01"},
		{"2026-12-31", "2026-04-01"},
		{"2027-01-01", "2026-04-01"},
		{"2027-03-31", "2026-04-01"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got := FiscalYearStart(day(tt.in))
			if !got.Equal(day(tt.want)) || got.Location() != time.UTC {
				t.Errorf("FiscalYearStart(%s) = %s, want %s UTC", tt.in, got, tt.want)
			}
		})
	}
}

func TestAccountHistory(t *testing.T) {
	tests := []struct {
		name    string
		leaky   bool
		account string
		through string
		months  int
		want    []MonthTotal
	}{
		{
			name: "bank, three months through September", account: "Bank - TT", through: "2026-09", months: 3,
			want: []MonthTotal{
				{Month: "2026-07"},
				{Month: "2026-08", Debit: 50000, Net: 50000},
				{Month: "2026-09", Credit: 1180, Net: -1180},
			},
		},
		{
			name: "cancelled entries are excluded even when the ledger returns them", leaky: true,
			account: "Bank Charges - TT", through: "2026-09", months: 1,
			want: []MonthTotal{{Month: "2026-09", Debit: 1180, Net: 1180}},
		},
		{
			name: "across a year boundary with empty months", account: "Bank - TT", through: "2027-02", months: 4,
			want: []MonthTotal{
				{Month: "2026-11"},
				{Month: "2026-12", Debit: 2000, Net: 2000},
				{Month: "2027-01", Credit: 1500, Net: -1500},
				{Month: "2027-02"},
			},
		},
		{
			name: "debit and credit in one month", account: "Bank - TT", through: "2026-05", months: 2,
			want: []MonthTotal{
				{Month: "2026-04", Credit: 30000, Net: -30000},
				{Month: "2026-05", Debit: 5000, Credit: 5000},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake(t)
			if tt.leaky {
				// The leaky fake returns cancelled entries but, restricted
				// here to the one month asked for, nothing outside it
				// (AccountHistory reports such entries as errors).
				f.leaky = true
				f.entries = onlyMonth(f.entries, tt.through)
			}
			got, err := AccountHistory(t.Context(), f, testCompany, tt.account, tt.through, tt.months)
			if err != nil {
				t.Fatalf("AccountHistory: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %+v\nwant %+v", got, tt.want)
			}
		})
	}

	t.Run("36 months ends with through", func(t *testing.T) {
		f := newFake(t)
		got, err := AccountHistory(t.Context(), f, testCompany, "Bank - TT", "2026-09", 36)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 36 || got[0].Month != "2023-10" || got[35].Month != "2026-09" {
			t.Fatalf("got %d months %s..%s", len(got), got[0].Month, got[len(got)-1].Month)
		}
		last := f.calls[len(f.calls)-1]
		if !last.from.Equal(day("2023-10-01")) || !last.to.Equal(day("2026-09-30")) {
			t.Errorf("fetched %s..%s, want 2023-10-01..2026-09-30", last.from, last.to)
		}
	})
}

func onlyMonth(entries []frappe.GLEntry, month string) []frappe.GLEntry {
	var out []frappe.GLEntry
	for _, e := range entries {
		if e.PostingDate.Format(monthLayout) == month {
			out = append(out, e)
		}
	}
	return out
}

func TestAccountHistoryErrors(t *testing.T) {
	tests := []struct {
		name    string
		company string
		account string
		through string
		months  int
		edit    func(*fakeLedger)
		want    string
	}{
		{name: "zero months", account: "Bank - TT", through: "2026-09", months: 0, want: "outside 1 to 36"},
		{name: "37 months", account: "Bank - TT", through: "2026-09", months: 37, want: "outside 1 to 36"},
		{name: "negative months", account: "Bank - TT", through: "2026-09", months: -1, want: "outside 1 to 36"},
		{name: "single-digit month", account: "Bank - TT", through: "2026-9", months: 3, want: "not a YYYY-MM month"},
		{name: "month 13", account: "Bank - TT", through: "2026-13", months: 3, want: "not a YYYY-MM month"},
		{name: "full date", account: "Bank - TT", through: "2026-09-01", months: 3, want: "not a YYYY-MM month"},
		{name: "empty through", account: "Bank - TT", through: "", months: 3, want: "not a YYYY-MM month"},
		{name: "unknown account", account: "Ghost - TT", through: "2026-09", months: 3, want: `no account "Ghost - TT"`},
		{name: "group account", account: "Application of Funds (Assets) - TT", through: "2026-09", months: 3, want: "group account"},
		{name: "empty company", company: "-", account: "Bank - TT", through: "2026-09", months: 3, want: "empty company"},
		{name: "ledger error", account: "Bank - TT", through: "2026-09", months: 3,
			edit: func(f *fakeLedger) { f.entryErr = errors.New("boom") }, want: "boom"},
		{name: "ledger returns entries outside the window", account: "Bank - TT", through: "2026-09", months: 3,
			edit: func(f *fakeLedger) { f.leaky = true }, want: "outside 2026-07-01..2026-09-30"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake(t)
			if tt.edit != nil {
				tt.edit(f)
			}
			company := testCompany
			if tt.company == "-" {
				company = ""
			}
			_, err := AccountHistory(t.Context(), f, company, tt.account, tt.through, tt.months)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want one containing %q", err, tt.want)
			}
		})
	}
}

// ---- FrappeLedger against httptest ----

type listRequest struct {
	path    string
	fields  []string
	filters string
	orderBy string
}

// listServer serves body for the first page of each list request and an
// empty page after it, recording what was asked.
func listServer(t *testing.T, body []byte) (FrappeLedger, *[]listRequest) {
	t.Helper()
	var (
		mu   sync.Mutex
		reqs []listRequest
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		var fields []string
		if err := json.Unmarshal([]byte(q.Get("fields")), &fields); err != nil {
			t.Errorf("fields %q: %v", q.Get("fields"), err)
		}
		mu.Lock()
		reqs = append(reqs, listRequest{path: r.URL.Path, fields: fields, filters: q.Get("filters"), orderBy: q.Get("order_by")})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if q.Get("limit_start") != "0" {
			_, _ = w.Write([]byte(`{"data":[]}`))
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	c, err := frappe.New(config.Config{ERPBaseURL: srv.URL}, "botkey", config.NewSecret("botsecret"))
	if err != nil {
		t.Fatal(err)
	}
	return FrappeLedger{C: c}, &reqs
}

// sameJSON reports whether two JSON texts decode to equal values (Go's
// encoder escapes "<" as \u003c, which Frappe decodes the same).
func sameJSON(t *testing.T, a, b string) bool {
	t.Helper()
	var va, vb any
	if err := json.Unmarshal([]byte(a), &va); err != nil {
		t.Errorf("decode %q: %v", a, err)
		return false
	}
	if err := json.Unmarshal([]byte(b), &vb); err != nil {
		t.Errorf("decode %q: %v", b, err)
		return false
	}
	return reflect.DeepEqual(va, vb)
}

func TestFrappeLedgerGLEntries(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("testdata", "reports", "gl_entries.json"))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name        string
		from, to    time.Time
		wantFilters string
	}{
		{
			name: "bounded period", from: day("2026-09-01"), to: day("2026-09-30"),
			wantFilters: `[["company","=","Test Traders Pvt Ltd"],["posting_date","between",["2026-09-01","2026-09-30"]],["is_cancelled","=",0]]`,
		},
		{
			name: "from the beginning", to: day("2026-09-30"),
			wantFilters: `[["company","=","Test Traders Pvt Ltd"],["posting_date","<=","2026-09-30"],["is_cancelled","=",0]]`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l, reqs := listServer(t, body)
			got, err := l.GLEntries(t.Context(), testCompany, tt.from, tt.to)
			if err != nil {
				t.Fatalf("GLEntries: %v", err)
			}
			if len(*reqs) == 0 {
				t.Fatal("no request")
			}
			r := (*reqs)[0]
			if r.path != "/api/resource/GL Entry" {
				t.Errorf("path = %q", r.path)
			}
			if !sameJSON(t, r.filters, tt.wantFilters) {
				t.Errorf("filters:\n got %s\nwant %s", r.filters, tt.wantFilters)
			}
			if !reflect.DeepEqual(r.fields, frappe.Fields[frappe.GLEntryRaw]()) {
				t.Errorf("fields = %v", r.fields)
			}
			if r.orderBy == "" {
				t.Error("no order_by: paging a live ledger needs a stable order")
			}
			// The server here ignores filters, so every fixture row comes
			// back, converted.
			if len(got) != 28 {
				t.Fatalf("got %d entries, want 28", len(got))
			}
			if got[0].Debit != 100000 || got[0].Account != "Bank - TT" || !got[0].PostingDate.Equal(day("2025-05-10")) {
				t.Errorf("first entry = %+v", got[0])
			}
		})
	}
}

func TestFrappeLedgerAccounts(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("testdata", "reports", "accounts.json"))
	if err != nil {
		t.Fatal(err)
	}
	l, reqs := listServer(t, body)
	got, err := l.Accounts(t.Context(), testCompany)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 9 || !got[0].IsGroup || got[1].Name != "Bank - TT" || got[1].RootType != frappe.RootTypeAsset {
		t.Errorf("accounts = %+v", got)
	}
	r := (*reqs)[0]
	if r.path != "/api/resource/Account" || !sameJSON(t, r.filters, `[["company","=","Test Traders Pvt Ltd"]]`) {
		t.Errorf("request = %+v", r)
	}
	if !reflect.DeepEqual(r.fields, frappe.Fields[frappe.AccountRaw]()) {
		t.Errorf("fields = %v", r.fields)
	}
}

func TestFrappeLedgerErrors(t *testing.T) {
	badGL := []byte(`{"data":[{"name":"GLE-BAD","company":"Test Traders Pvt Ltd","account":"Bank - TT","debit":1,"credit":0,"posting_date":"2026-09-01","is_cancelled":0,"is_opening":"Maybe"}]}`)
	badAcc := []byte(`{"data":[{"name":"Odd - TT","company":"Test Traders Pvt Ltd","is_group":0,"root_type":"Assets"}]}`)

	l, _ := listServer(t, badGL)
	if _, err := l.GLEntries(t.Context(), testCompany, day("2026-09-01"), day("2026-09-30")); err == nil || !strings.Contains(err.Error(), "GLE-BAD") {
		t.Errorf("bad is_opening: err = %v", err)
	}
	if _, err := l.GLEntries(t.Context(), testCompany, day("2026-09-30"), day("2026-09-01")); err == nil {
		t.Error("from after to: no error")
	}
	if _, err := l.GLEntries(t.Context(), "", day("2026-09-01"), day("2026-09-30")); err == nil {
		t.Error("empty company: no error")
	}
	if _, err := l.GLEntries(t.Context(), testCompany, time.Time{}, time.Time{}); err == nil {
		t.Error("zero to: no error")
	}
	la, _ := listServer(t, badAcc)
	if _, err := la.Accounts(t.Context(), testCompany); err == nil || !strings.Contains(err.Error(), "Assets") {
		t.Errorf("bad root type: err = %v", err)
	}
	if _, err := (FrappeLedger{}).GLEntries(t.Context(), testCompany, time.Time{}, day("2026-09-30")); err == nil {
		t.Error("nil client: no error")
	}
	if _, err := (FrappeLedger{}).Accounts(t.Context(), testCompany); err == nil {
		t.Error("nil client: no error")
	}
}
