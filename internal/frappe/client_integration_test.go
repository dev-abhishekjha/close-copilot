//go:build integration

package frappe

// Integration tests against the local erp.localhost site (CC-102, CC-201).
// Run with the keys from tmp/erp-keys.env in the environment, never on argv:
//
//	sh -c 'set -a; . tmp/erp-keys.env; set +a; go test -tags=integration ./internal/frappe/... -run TestIntegration -count=1 -v'
//
// Without the variables every test here skips (see integrationConfig and
// TestIntegrationSkips). The only write is one ₹1.00 Journal Entry marked
// with itRemark, which the test submits, cancels and deletes again. A
// t.Cleanup removes this test's entries even when the test fails; it
// selects only entries of itCompany owned by the bot user, of voucher type
// Journal Entry, with total_debit 1 and itRemark, so nothing else on the
// shared site is touched.
//
// Deleting a cancelled voucher needs the site's audit trail off and Accounts
// Settings delete_linked_ledger_entries on (CC-102 follow-up); see
// itDeleteHint.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/abhishekjha/close-copilot/internal/config"
)

const (
	itCompany = "Sharma Traders Pvt Ltd"
	itRemark  = "CC-202 integration test"
	itJE      = "Journal Entry"
)

type itAccount struct {
	Name        string `json:"name"`
	AccountType string `json:"account_type"`
	RootType    string `json:"root_type"`
}

type itJournal struct {
	Name       string      `json:"name"`
	Docstatus  int         `json:"docstatus"`
	UserRemark string      `json:"user_remark"`
	TotalDebit json.Number `json:"total_debit"`
	Accounts   []struct {
		Account string      `json:"account"`
		Debit   json.Number `json:"debit_in_account_currency"`
		Credit  json.Number `json:"credit_in_account_currency"`
	} `json:"accounts"`
}

func itClient(t *testing.T, cfg config.Config, key, secret string) *Client {
	t.Helper()
	c, err := New(cfg, key, Secret(secret))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestIntegrationListAccounts(t *testing.T) {
	cfg := integrationConfig(t, os.LookupEnv)
	bot := itClient(t, cfg, cfg.ERPAPIKey, cfg.ERPAPISecret)
	accounts, err := itLeafAccounts(t.Context(), bot)
	if err != nil {
		t.Fatalf("list accounts with the bot key: %v", err)
	}
	if len(accounts) == 0 {
		t.Fatalf("no leaf Account for %s", itCompany)
	}
	t.Logf("bot key listed %d leaf accounts of %s, first %q", len(accounts), itCompany, accounts[0].Name)
}

func TestIntegrationJournalEntryRoundTrip(t *testing.T) {
	cfg := integrationConfig(t, os.LookupEnv)
	bot := itClient(t, cfg, cfg.ERPAPIKey, cfg.ERPAPISecret)
	ctx := t.Context()

	// The bot's user id scopes the cleanup to entries this test's key made.
	owner, err := Call[string](ctx, bot, http.MethodGet, "frappe.auth.get_logged_user", nil)
	if err != nil || owner == "" {
		t.Fatalf("frappe.auth.get_logged_user: %q, %v", owner, err)
	}

	// The seeder (Administrator) client is built lazily and used only to
	// delete the entry this run inserted, and only if Frappe refuses the bot
	// (see itRemove).
	seeder := func() *Client { return itClient(t, cfg, cfg.ERPSeedAPIKey, cfg.ERPSeedAPISecret) }
	var inserted string // the name this run created, once known

	// Clean up leftovers from an earlier failed run, and register the same
	// cleanup before writing anything, so a failure below never leaves a
	// draft or submitted entry behind. t.Context() is already cancelled when
	// cleanups run, so they use their own context.
	itCleanup(ctx, t, bot, seeder, owner, "")
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		itCleanup(cctx, t, bot, seeder, owner, inserted)
		left, err := itRemarked(cctx, bot)
		if err != nil {
			t.Errorf("list entries after cleanup: %v", err)
		} else if len(left) != 0 {
			t.Errorf("%d Journal Entry with remark %q remain after cleanup: %v", len(left), itRemark, left)
		}
		itLogCounts(cctx, t, bot, inserted)
	})

	debit, credit := itPickAccounts(ctx, t, bot)
	date := itPostingDate(ctx, t, bot)

	// ₹1.00 between two balance-sheet accounts, so no cost center or party
	// is needed. Amounts go out as json.Number, never float.
	doc := map[string]any{
		"voucher_type": "Journal Entry",
		"company":      itCompany,
		"posting_date": date,
		"user_remark":  itRemark,
		"accounts": []map[string]any{
			{"account": debit, "debit_in_account_currency": json.Number("1.00")},
			{"account": credit, "credit_in_account_currency": json.Number("1.00")},
		},
	}
	ins, err := Insert(ctx, bot, itJE, doc)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	name, _ := ins["name"].(string)
	if name == "" {
		t.Fatalf("insert returned no name: %v", ins)
	}
	inserted = name
	t.Logf("inserted %s as %s (Dr %s, Cr %s, %s)", name, owner, debit, credit, date)
	itExpectDocstatus(ctx, t, bot, name, 0)

	// frappe.client.submit with the full latest document works on frappe
	// 15.122.0 for the Accounts User bot.
	sub, err := Submit(ctx, bot, itJE, name)
	if err != nil {
		t.Fatalf("submit via frappe.client.submit: %v", err)
	}
	if sub["docstatus"] != json.Number("1") {
		t.Errorf("submit returned docstatus %v", sub["docstatus"])
	}
	got := itExpectDocstatus(ctx, t, bot, name, 1)
	if got.UserRemark != itRemark || len(got.Accounts) != 2 {
		t.Errorf("read back %+v, want the remark and two account rows", got)
	}
	t.Logf("submitted %s: total_debit %s (json.Number)", name, got.TotalDebit)

	can, err := Cancel(ctx, bot, itJE, name)
	if err != nil {
		t.Fatalf("cancel via frappe.client.cancel: %v", err)
	}
	if can["docstatus"] != json.Number("2") {
		t.Errorf("cancel returned docstatus %v", can["docstatus"])
	}
	itExpectDocstatus(ctx, t, bot, name, 2)

	// The bot (Accounts User) holds delete on Journal Entry (erpnext
	// journal_entry.json), so it deletes the cancelled entry itself; the
	// seeder key is not needed.
	if err := Delete(ctx, bot, itJE, name); err != nil {
		t.Fatalf("delete cancelled entry with the bot key: %v%s", err, itDeleteHint(err))
	}
	if _, err := Get[itJournal](ctx, bot, itJE, name); !IsNotFound(err) {
		t.Errorf("get after delete: err = %v, want not found", err)
	}
	t.Logf("cancelled and deleted %s", name)
}

func itLeafAccounts(ctx context.Context, c *Client) ([]itAccount, error) {
	return List[itAccount](ctx, c, "Account", Query{
		Fields:  []string{"name", "account_type", "root_type"},
		Filters: [][]any{{"company", "=", itCompany}, {"is_group", "=", 0}},
		OrderBy: "name asc",
	})
}

// itPickAccounts returns the company's cash account and an Asset account
// with no account type (no party, cost center or bank reference needed).
func itPickAccounts(ctx context.Context, t *testing.T, c *Client) (debit, credit string) {
	t.Helper()
	accounts, err := itLeafAccounts(ctx, c)
	if err != nil {
		t.Fatalf("list accounts: %v", err)
	}
	for _, a := range accounts {
		switch {
		case a.AccountType == "Cash" && credit == "":
			credit = a.Name
		case a.RootType == "Asset" && a.AccountType == "" && debit == "":
			debit = a.Name
		}
	}
	if debit == "" || credit == "" {
		t.Fatalf("no Cash account or untyped Asset account among %d accounts of %s", len(accounts), itCompany)
	}
	return debit, credit
}

// itPostingDate is today when a Fiscal Year covers it, else the start of
// the first Fiscal Year.
func itPostingDate(ctx context.Context, t *testing.T, c *Client) string {
	t.Helper()
	type fy struct {
		Start string `json:"year_start_date"`
		End   string `json:"year_end_date"`
	}
	years, err := List[fy](ctx, c, "Fiscal Year", Query{Fields: []string{"name", "year_start_date", "year_end_date"}, OrderBy: "year_start_date asc"})
	if err != nil || len(years) == 0 {
		t.Fatalf("fiscal years: %v (%d found)", err, len(years))
	}
	today := time.Now().In(time.FixedZone("IST", 5*3600+1800)).Format(time.DateOnly)
	for _, y := range years {
		if y.Start <= today && today <= y.End {
			return today
		}
	}
	return years[0].Start
}

func itExpectDocstatus(ctx context.Context, t *testing.T, c *Client, name string, want int) itJournal {
	t.Helper()
	got, err := Get[itJournal](ctx, c, itJE, name)
	if err != nil {
		t.Fatalf("read back %s: %v", name, err)
	}
	if got.Docstatus != want {
		t.Fatalf("%s docstatus = %d, want %d", name, got.Docstatus, want)
	}
	return got
}

// itRemarked lists every Journal Entry of itCompany with itRemark,
// whoever made it. It is only read, for the final assertion.
func itRemarked(ctx context.Context, c *Client) ([]itJournal, error) {
	return List[itJournal](ctx, c, itJE, Query{
		Fields:  []string{"name", "docstatus", "user_remark"},
		Filters: [][]any{{"company", "=", itCompany}, {"user_remark", "=", itRemark}},
		OrderBy: "name asc",
	})
}

// itOwnEntries lists the entries this test may remove: itCompany, made by
// owner (the bot user), voucher type Journal Entry, total_debit 1 and
// itRemark.
func itOwnEntries(ctx context.Context, c *Client, owner string) ([]itJournal, error) {
	return List[itJournal](ctx, c, itJE, Query{
		Fields: []string{"name", "docstatus", "user_remark"},
		Filters: [][]any{
			{"company", "=", itCompany},
			{"owner", "=", owner},
			{"voucher_type", "=", "Journal Entry"},
			{"total_debit", "=", 1},
			{"user_remark", "=", itRemark},
		},
		OrderBy: "name asc",
	})
}

// itCleanup removes this test's entries (itOwnEntries): a draft is
// deleted, a submitted one is cancelled and then deleted, a cancelled one is
// deleted. inserted is the name this run created ("" before the insert).
func itCleanup(ctx context.Context, t *testing.T, bot *Client, seeder func() *Client, owner, inserted string) {
	t.Helper()
	own, err := itOwnEntries(ctx, bot, owner)
	if err != nil {
		t.Errorf("cleanup: list entries: %v", err)
		return
	}
	for _, je := range own {
		if je.Docstatus == 1 {
			if _, err := Cancel(ctx, bot, itJE, je.Name); err != nil {
				t.Errorf("cleanup: cancel %s: %v", je.Name, err)
				continue
			}
		}
		if itRemove(ctx, t, bot, seeder, je.Name, je.Name == inserted) {
			t.Logf("cleanup: removed leftover %s (docstatus %d)", je.Name, je.Docstatus)
		}
	}
}

// itRemove deletes name with the bot key and reports whether it is gone.
// Only for the entry this run inserted (mayEscalate), and only if Frappe
// refuses the bot with a PermissionError, does it fall back to the seeder
// (Administrator) key, and it says so in the test log.
func itRemove(ctx context.Context, t *testing.T, bot *Client, seeder func() *Client, name string, mayEscalate bool) bool {
	t.Helper()
	err := Delete(ctx, bot, itJE, name)
	if IsPermission(err) && mayEscalate {
		t.Logf("cleanup: the bot may not delete %s; using the seeder key", name)
		err = Delete(ctx, seeder(), itJE, name)
	}
	if err != nil && !IsNotFound(err) {
		t.Errorf("cleanup: delete %s: %v%s", name, err, itDeleteHint(err))
		return false
	}
	return true
}

// itDeleteHint explains the LinkExistsError ERPNext raises when deleting a
// cancelled voucher: its GL Entries (is_cancelled = 1, docstatus 1) still
// link to it, and AccountsController.on_trash removes them only when
// Accounts Settings > delete_linked_ledger_entries is on. No key, not even
// Administrator, gets past that check.
func itDeleteHint(err error) string {
	var ae *APIError
	if errors.As(err, &ae) && ae.ExcType == "LinkExistsError" {
		return " (enable Accounts Settings > \"Delete Accounting and Stock Ledger Entries on deletion of Transaction\" (delete_linked_ledger_entries) on the site)"
	}
	return ""
}

// itLogCounts logs the site's Journal Entry and GL Entry totals and any GL
// Entry still pointing at inserted, and fails if one does.
func itLogCounts(ctx context.Context, t *testing.T, c *Client, inserted string) {
	t.Helper()
	jes, err := List[itJournal](ctx, c, itJE, Query{Fields: []string{"name"}})
	if err != nil {
		t.Errorf("count Journal Entries: %v", err)
		return
	}
	type gle struct {
		Name string `json:"name"`
	}
	gles, err := List[gle](ctx, c, "GL Entry", Query{Fields: []string{"name"}})
	if err != nil {
		t.Errorf("count GL Entries: %v", err)
		return
	}
	var linked []gle
	if inserted != "" {
		linked, err = List[gle](ctx, c, "GL Entry", Query{
			Fields:  []string{"name"},
			Filters: [][]any{{"voucher_type", "=", itJE}, {"voucher_no", "=", inserted}},
		})
		if err != nil {
			t.Errorf("GL Entries of %s: %v", inserted, err)
			return
		}
		if len(linked) != 0 {
			t.Errorf("%d GL Entry still reference %s", len(linked), inserted)
		}
	}
	t.Logf("after cleanup: %d Journal Entry and %d GL Entry on the site; %d GL Entry reference %q",
		len(jes), len(gles), len(linked), inserted)
}
