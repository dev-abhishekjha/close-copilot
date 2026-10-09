//go:build integration

package books

// Read-only integration test for the CC-204 reports against the local
// erp.localhost site, which holds Sharma Traders' books for 2026-08 and
// 2026-09 (CC-304). It never inserts, submits or cancels anything. Run with
// the keys from tmp/erp-keys.env in the environment, never on argv:
//
//	sh -c 'set -a; . tmp/erp-keys.env; set +a; go test -tags=integration ./internal/books/... -run TestIntegrationReports -count=1 -v'

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/frappe"
	"github.com/abhishekjha/close-copilot/internal/money"
)

const (
	stplCompany = "Sharma Traders Pvt Ltd"
	stplBank    = "HDFC Current 0001 - STPL"
)

// reportsEnv are the variables this test needs: the bot key, the one the
// books MCP server uses. The seeder key is optional (see erpReport).
var reportsEnv = []string{
	config.EnvERPBaseURL,
	config.EnvERPSite,
	config.EnvERPAPIKey,
	config.EnvERPAPISecret,
}

// integrationConfig loads the ERPNext settings or skips t when any of
// reportsEnv is unset. Only variable names reach the skip message.
func integrationConfig(t *testing.T) config.Config {
	t.Helper()
	var missing []string
	for _, k := range reportsEnv {
		if v, ok := os.LookupEnv(k); !ok || strings.TrimSpace(v) == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		t.Skipf("ERPNext integration test skipped: %s not set (source tmp/erp-keys.env)", strings.Join(missing, ", "))
	}
	cfg, err := config.Load(os.LookupEnv, reportsEnv...)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	return cfg
}

func botLedger(t *testing.T, cfg config.Config) FrappeLedger {
	t.Helper()
	bot, err := frappe.New(cfg, cfg.ERPAPIKey.Reveal(), frappe.Secret(cfg.ERPAPISecret.Reveal()))
	if err != nil {
		t.Fatalf("frappe.New (bot): %v", err)
	}
	return FrappeLedger{C: bot}
}

func septemberTB(t *testing.T, l FrappeLedger) TB {
	t.Helper()
	tb, err := TrialBalance(t.Context(), l, stplCompany, day("2026-09-01"), day("2026-09-30"))
	if err != nil {
		t.Fatalf("TrialBalance: %v", err)
	}
	if tb.Totals.Debit != tb.Totals.Credit || tb.Totals.Opening != 0 || tb.Totals.Closing != 0 {
		t.Fatalf("totals don't balance: %+v", tb.Totals)
	}
	if tb.Totals.Debit == 0 {
		t.Fatal("September has no entries; are the CC-304 books loaded?")
	}
	return tb
}

// reportRow is one row of ERPNext's Trial Balance report, as amounts.
type reportRow struct {
	account                                                  string
	openingDebit, openingCredit, closingDebit, closingCredit money.Paise
}

// erpReport runs ERPNext's own Trial Balance report through
// frappe.desk.query_report.run (GET: the report only reads).
//
// The bot key (Accounts User) should be allowed to run it. If ERPNext
// refuses the bot, the seeder key is used for this report call only; the
// ledger under test is always read with the bot key.
func erpReport(t *testing.T, cfg config.Config, bot *frappe.Client, fiscalYear string) []reportRow {
	t.Helper()
	params := map[string]any{
		"report_name": "Trial Balance",
		"filters": map[string]any{
			"company":          stplCompany,
			"fiscal_year":      fiscalYear,
			"from_date":        "2026-09-01",
			"to_date":          "2026-09-30",
			"show_zero_values": 0,
		},
	}
	type message struct {
		Result []json.RawMessage `json:"result"`
	}
	msg, err := frappe.Call[message](t.Context(), bot, http.MethodGet, "frappe.desk.query_report.run", params)
	if frappe.IsPermission(err) {
		key, secret := os.Getenv(config.EnvERPSeedAPIKey), os.Getenv(config.EnvERPSeedAPISecret)
		if key == "" || secret == "" {
			t.Fatalf("the bot may not run the Trial Balance report and no seeder key is set: %v", err)
		}
		t.Logf("the bot may not run the Trial Balance report; using the seeder key for the report call only")
		seeder, nerr := frappe.New(cfg, key, frappe.Secret(secret))
		if nerr != nil {
			t.Fatalf("frappe.New (seeder): %v", nerr)
		}
		msg, err = frappe.Call[message](t.Context(), seeder, http.MethodGet, "frappe.desk.query_report.run", params)
	}
	if err != nil {
		t.Fatalf("Trial Balance report: %v", err)
	}

	var rows []reportRow
	for i, raw := range msg.Result {
		var m map[string]any
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&m); err != nil {
			// A row that isn't an object (such as a blank list) carries no account.
			continue
		}
		account, _ := m["account"].(string)
		if account == "" {
			continue
		}
		r := reportRow{account: account}
		for _, f := range []struct {
			key string
			dst *money.Paise
		}{
			{"opening_debit", &r.openingDebit},
			{"opening_credit", &r.openingCredit},
			{"closing_debit", &r.closingDebit},
			{"closing_credit", &r.closingCredit},
		} {
			n, ok := m[f.key].(json.Number)
			if !ok {
				t.Fatalf("report row %d (%s): %s is %T, not a number", i, account, f.key, m[f.key])
			}
			p, err := money.FromJSONNumber(n)
			if err != nil {
				t.Fatalf("report row %d (%s): %s: %v", i, account, f.key, err)
			}
			*f.dst = p
		}
		rows = append(rows, r)
	}
	return rows
}

// fiscalYear finds the Fiscal Year covering September 2026.
func fiscalYear(t *testing.T, c *frappe.Client) string {
	t.Helper()
	type fy struct {
		Name string `json:"name"`
	}
	years, err := frappe.List[fy](t.Context(), c, "Fiscal Year", frappe.Query{
		Fields: []string{"name"},
		Filters: [][]any{
			{"year_start_date", "<=", "2026-09-01"},
			{"year_end_date", ">=", "2026-09-30"},
		},
		OrderBy: "name asc",
	})
	if err != nil {
		t.Fatalf("list Fiscal Year: %v", err)
	}
	if len(years) != 1 {
		t.Fatalf("%d Fiscal Years cover September 2026, want 1: %+v", len(years), years)
	}
	return years[0].Name
}

func TestIntegrationReportsTrialBalance(t *testing.T) {
	cfg := integrationConfig(t)
	l := botLedger(t, cfg)
	tb := septemberTB(t, l)

	accounts, err := l.Accounts(t.Context(), stplCompany)
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	leaf := map[string]bool{}
	for _, a := range accounts {
		if !a.IsGroup {
			leaf[a.Name] = true
		}
	}
	ours := map[string]TBRow{}
	for _, r := range tb.Rows {
		ours[r.Account] = r
	}

	fy := fiscalYear(t, l.C)
	report := erpReport(t, cfg, l.C, fy)

	compared := 0
	seen := map[string]bool{}
	for _, rr := range report {
		if !leaf[rr.account] {
			continue // group accounts and the total row
		}
		seen[rr.account] = true
		compared++
		got := ours[rr.account] // zero when we have no row
		if want := rr.openingDebit - rr.openingCredit; got.Opening != want {
			t.Errorf("%s: opening %s, ERPNext %s", rr.account, got.Opening.Format(), want.Format())
		}
		if want := rr.closingDebit - rr.closingCredit; got.Closing != want {
			t.Errorf("%s: closing %s, ERPNext %s", rr.account, got.Closing.Format(), want.Format())
		}
	}
	for _, r := range tb.Rows {
		if !seen[r.Account] {
			t.Errorf("%s: in our trial balance (opening %s, closing %s) but not in ERPNext's report",
				r.Account, r.Opening.Format(), r.Closing.Format())
		}
	}
	if compared == 0 {
		t.Fatal("ERPNext's report has no leaf account rows")
	}
	t.Logf("fiscal year %s: compared %d leaf accounts with ERPNext's Trial Balance (%d rows ours); debits %s = credits %s",
		fy, compared, len(tb.Rows), tb.Totals.Debit.Format(), tb.Totals.Credit.Format())
}

func TestIntegrationReportsAccountHistory(t *testing.T) {
	cfg := integrationConfig(t)
	l := botLedger(t, cfg)

	hist, err := AccountHistory(t.Context(), l, stplCompany, stplBank, "2026-09", 3)
	if err != nil {
		t.Fatalf("AccountHistory: %v", err)
	}
	var months []string
	for _, m := range hist {
		months = append(months, m.Month)
	}
	if got := strings.Join(months, " "); got != "2026-07 2026-08 2026-09" {
		t.Fatalf("months = %+v", hist)
	}
	if jul := hist[0]; jul.Debit != 0 || jul.Credit != 0 || jul.Net != 0 {
		t.Errorf("July = %+v, want zeros", jul)
	}
	for _, m := range hist[1:] {
		if m.Debit == 0 && m.Credit == 0 {
			t.Errorf("%s has no entries on %s", m.Month, stplBank)
		}
		if m.Net != m.Debit-m.Credit {
			t.Errorf("%s: net %d != debit %d - credit %d", m.Month, m.Net, m.Debit, m.Credit)
		}
	}

	tb := septemberTB(t, l)
	var bank *TBRow
	for i := range tb.Rows {
		if tb.Rows[i].Account == stplBank {
			bank = &tb.Rows[i]
		}
	}
	if bank == nil {
		t.Fatalf("%s is not in September's trial balance", stplBank)
	}
	if sep := hist[2]; sep.Net != bank.Debit-bank.Credit || sep.Debit != bank.Debit || sep.Credit != bank.Credit {
		t.Errorf("September %+v, trial balance debit %s credit %s", sep, bank.Debit.Format(), bank.Credit.Format())
	}
	t.Logf("%s: %+v", stplBank, hist)
}
