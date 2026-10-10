//go:build integration

package seed

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/abhishekjha/close-copilot/internal/config"
	"github.com/abhishekjha/close-copilot/internal/frappe"
)

// These tests write to the shared ERPNext: run them only while holding
// tmp/erpnext.lock. They leave the masters in place (Bootstrap is
// idempotent) and delete every invoice they create.

// bootstrapEnv are the variables the integration tests need.
var bootstrapEnv = []string{
	config.EnvERPBaseURL,
	config.EnvERPSite,
	config.EnvERPSeedAPIKey,
	config.EnvERPSeedAPISecret,
}

// seederClient returns a client with the seeder key, or skips t when any
// of bootstrapEnv is unset. Only variable names reach the skip message.
func seederClient(t *testing.T) *frappe.Client {
	t.Helper()
	var missing []string
	for _, k := range bootstrapEnv {
		if strings.TrimSpace(os.Getenv(k)) == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		t.Skipf("ERPNext integration test skipped: %s not set (source tmp/erp-keys.env)", strings.Join(missing, ", "))
	}
	cfg, err := config.Load(os.LookupEnv, bootstrapEnv...)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	c, err := frappe.New(cfg, cfg.ERPSeedAPIKey.Reveal(), cfg.ERPSeedAPISecret)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func sharmaProfile(t *testing.T) Profile {
	t.Helper()
	p, err := LoadProfile(filepath.Join("..", "..", "config", "companies", "sharma.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestIntegrationBootstrapIdempotent(t *testing.T) {
	c := seederClient(t)
	p := sharmaProfile(t)
	ctx := context.Background()

	first, err := Bootstrap(ctx, c, p)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	t.Logf("first run: %d changes", first.Changes())
	second, err := Bootstrap(ctx, c, p)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	for dt, n := range second.Counts {
		if n.Created != 0 || n.Updated != 0 {
			t.Errorf("second run: %s created %v, updated %v", dt, n.CreatedNames, n.UpdatedNames)
		}
	}
	if second.ExtIDField != ExtIDField {
		t.Errorf("ext id field %q", second.ExtIDField)
	}
	if len(second.Customers) != p.Customers || len(second.Suppliers) != len(p.Suppliers) {
		t.Errorf("%d customers, %d suppliers", len(second.Customers), len(second.Suppliers))
	}
}

func TestIntegrationBootstrapPurchaseInvoice(t *testing.T) {
	c := seederClient(t)
	p := sharmaProfile(t)
	ctx := context.Background()

	rep, err := Bootstrap(ctx, c, p)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	var outOfState, inState *Supplier
	for i := range p.Suppliers {
		s := &p.Suppliers[i]
		switch {
		case !s.IsRegistered():
		case p.InState(*s) && inState == nil:
			inState = s
		case !p.InState(*s) && outOfState == nil:
			outOfState = s
		}
	}
	if outOfState == nil || inState == nil {
		t.Fatal("sharma needs an in-state and an out-of-state registered supplier")
	}

	run := time.Now().UTC().Format("150405")
	cases := []struct {
		name string
		s    *Supplier
		tax  []taxRow
	}{
		{"IGST", outOfState, []taxRow{{rep.InputGST.IGST, "180.00"}}},
		{"CGST+SGST", inState, []taxRow{{rep.InputGST.CGST, "90.00"}, {rep.InputGST.SGST, "90.00"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			purchaseRoundTrip(ctx, t, c, p, rep, *tc.s, tc.tax, fmt.Sprintf("CC303-IT-%s-%s", run, tc.s.ID))
		})
	}

	// The books stay empty: no invoices and no ledger entries.
	for _, dt := range []string{"Purchase Invoice", "GL Entry"} {
		rows, err := frappe.List[map[string]any](ctx, c, dt, frappe.Query{
			Fields:  []string{"name"},
			Filters: [][]any{{"company", "=", p.ERPCompany}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 0 {
			t.Errorf("%d %s rows remain for %s", len(rows), dt, p.ERPCompany)
		}
	}
}

// taxRow is one explicit GST row at 18%: the account head and the amount
// in rupees. ERPNext clears the rate of an Actual row, so India
// Compliance takes each line's rate from its Item Tax Template and checks
// the row's amount against it (within ₹1). Without a template it refuses
// the invoice: "Charge Type is set to Actual. However, this would not
// compute item taxes".
type taxRow struct {
	head   string
	amount string
}

// purchaseRoundTrip inserts a Purchase Invoice for s with explicit tax
// rows, submits it, then cancels and deletes it.
func purchaseRoundTrip(ctx context.Context, t *testing.T, c *frappe.Client, p Profile, rep BootstrapReport, s Supplier, tax []taxRow, billNo string) {
	t.Helper()
	account := rep.Accounts[expenseAccount(s)]
	if account == "" {
		t.Fatalf("no resolved account for %s", expenseAccount(s))
	}
	template := rep.ItemTaxTemplates["18"]
	if template == "" {
		t.Fatal("no Item Tax Template for 18%")
	}
	var taxes []map[string]any
	for _, tr := range tax {
		taxes = append(taxes, map[string]any{
			"charge_type":    "Actual",
			"account_head":   tr.head,
			"tax_amount":     json.Number(tr.amount),
			"description":    strings.TrimSuffix(tr.head, " - "+p.Abbr),
			"category":       "Total",
			"add_deduct_tax": "Add",
		})
	}
	extID := "TEST-" + billNo
	pi := map[string]any{
		"company":          p.ERPCompany,
		"supplier":         rep.Suppliers[s.ID],
		"posting_date":     "2026-09-15",
		"set_posting_time": 1,
		"bill_no":          billNo,
		"bill_date":        "2026-09-14",
		"supplier_address": rep.SupplierAddresses[s.ID],
		"billing_address":  rep.CompanyAddress,
		ExtIDField:         extID,
		"remarks":          "CC-303 integration test; deleted after submit",
		"items": []map[string]any{{
			"item_code":         ItemFor(EventPurchase, s.Kind, false),
			"qty":               1,
			"rate":              json.Number("1000.00"),
			"expense_account":   account,
			"item_tax_template": template,
		}},
		"taxes": taxes,
	}
	// ERPNext adds an Item Price for an item without one when an invoice is
	// saved (Stock Settings, auto_insert_price_list_rate_if_missing). The
	// test removes any it caused, so the clean state stays free of them.
	item := ItemFor(EventPurchase, s.Kind, false)
	before := itemPrices(ctx, t, c, item)
	t.Cleanup(func() {
		for _, n := range itemPrices(context.Background(), t, c, item) {
			if !slices.Contains(before, n) {
				if err := frappe.Delete(context.Background(), c, "Item Price", n); err != nil {
					t.Errorf("delete Item Price %s: %v", n, err)
				}
			}
		}
	})

	saved, err := frappe.Insert(ctx, c, "Purchase Invoice", pi)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	name, _ := saved["name"].(string)
	done := false
	t.Cleanup(func() {
		if done {
			return
		}
		cleanupInvoice(context.Background(), t, c, name)
	})

	sub, err := frappe.Submit(ctx, c, "Purchase Invoice", name)
	if err != nil {
		t.Fatalf("submit %s: %v", name, err)
	}
	if str(sub["docstatus"]) != "1" {
		t.Errorf("%s docstatus %v after submit", name, sub["docstatus"])
	}
	if got := str(sub[ExtIDField]); got != extID {
		t.Errorf("%s %s = %q, want %q", name, ExtIDField, got, extID)
	}
	if got := str(sub["grand_total"]); got != "1180" && got != "1180.0" {
		t.Errorf("%s grand_total %s, want 1180", name, got)
	}
	if _, err := frappe.Cancel(ctx, c, "Purchase Invoice", name); err != nil {
		t.Fatalf("cancel %s: %v", name, err)
	}
	if err := frappe.Delete(ctx, c, "Purchase Invoice", name); err != nil {
		t.Fatalf("delete %s: %v", name, err)
	}
	done = true
}

// itemPrices lists the Item Price records of an item.
func itemPrices(ctx context.Context, t *testing.T, c *frappe.Client, item string) []string {
	t.Helper()
	rows, err := frappe.List[map[string]any](ctx, c, "Item Price", frappe.Query{
		Fields:  []string{"name"},
		Filters: [][]any{{"item_code", "=", item}},
	})
	if err != nil {
		t.Fatalf("list Item Price: %v", err)
	}
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = str(r["name"])
	}
	return out
}

// cleanupInvoice cancels (if submitted) and deletes a test invoice,
// logging rather than failing.
func cleanupInvoice(ctx context.Context, t *testing.T, c *frappe.Client, name string) {
	d, err := frappe.Get[map[string]any](ctx, c, "Purchase Invoice", name)
	if frappe.IsNotFound(err) {
		return
	}
	if err != nil {
		t.Logf("cleanup: get %s: %v", name, err)
		return
	}
	if str(d["docstatus"]) == "1" {
		if _, err := frappe.Cancel(ctx, c, "Purchase Invoice", name); err != nil {
			t.Logf("cleanup: cancel %s: %v", name, err)
			return
		}
	}
	if err := frappe.Delete(ctx, c, "Purchase Invoice", name); err != nil {
		t.Logf("cleanup: delete %s: %v", name, err)
	}
}
