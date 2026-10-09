//go:build integration

package seed

import (
	"context"
	"errors"
	"testing"

	"github.com/abhishekjha/close-copilot/internal/frappe"
	"github.com/abhishekjha/close-copilot/internal/money"
)

// These tests post sharma's 2026-08 --small books to the shared ERPNext:
// run them only while holding tmp/erpnext.lock. They leave August posted
// on purpose (CC-204 tests the trial balance against these books, and
// CC-307's reset restores the clean backup).

const booksITMonth = "2026-08"

// booksITWorld bootstraps sharma and generates its clean --small month.
func booksITWorld(ctx context.Context, t *testing.T, c *frappe.Client) (Profile, BootstrapReport, World) {
	t.Helper()
	p := sharmaProfile(t)
	rep, err := Bootstrap(ctx, c, p)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if n := rep.Changes(); n > 0 {
		t.Logf("bootstrap made %d changes", n)
	}
	w, err := Generate(p, booksITMonth, Options{Small: true})
	if err != nil {
		t.Fatal(err)
	}
	return p, rep, w
}

// monthDocs lists the ExtIDs of the company's documents of doctype posted
// in the month, with their docstatus.
func monthDocs(ctx context.Context, t *testing.T, c *frappe.Client, company, doctype string) []doc {
	t.Helper()
	rows, err := frappe.List[doc](ctx, c, doctype, frappe.Query{
		Fields: []string{"name", ExtIDField, "docstatus"},
		Filters: [][]any{
			{"company", "=", company},
			{"posting_date", "between", []string{"2026-08-01", "2026-08-31"}},
			{ExtIDField, "is", "set"},
			{"docstatus", "in", []int{0, 1, 2}},
		},
		OrderBy: "name asc",
	})
	if err != nil {
		t.Fatalf("list %s: %v", doctype, err)
	}
	return rows
}

func TestIntegrationBooksResume(t *testing.T) {
	c := seederClient(t)
	ctx := context.Background()
	p, rep, w := booksITWorld(ctx, t, c)
	opt := BooksOptions{Suppliers: p.Suppliers, StopAfter: 15}

	first, err := WriteBooks(ctx, c, rep, w, opt)
	switch {
	case errors.Is(err, ErrStopped):
		t.Logf("first run stopped after %d new documents", first.Changes())
	case err != nil:
		t.Fatalf("first run: %v", err)
	default:
		t.Logf("first run finished without stopping (%d changes); the month was already partly posted", first.Changes())
	}

	opt.StopAfter = 0
	second, err := WriteBooks(ctx, c, rep, w, opt)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	t.Logf("second run: %+v", countsOf(second))

	want := map[string]int{}
	for _, ev := range w.Events {
		if dt := BookDocType(ev.Kind); dt != "" {
			want[dt]++
		}
	}
	seen := map[string]string{}
	for _, dt := range []string{DocSalesInvoice, DocPurchaseInvoice, DocPaymentEntry, DocJournalEntry} {
		rows := monthDocs(ctx, t, c, rep.Company, dt)
		submitted := 0
		for _, r := range rows {
			ext, name := str(r[ExtIDField]), str(r["name"])
			if prev, dup := seen[ext]; dup {
				t.Errorf("ExtID %s is on %s and %s %s", ext, prev, dt, name)
			}
			seen[ext] = dt + " " + name
			if str(r["docstatus"]) == "1" {
				submitted++
			} else {
				t.Errorf("%s %s (%s) has docstatus %v", dt, name, ext, r["docstatus"])
			}
		}
		if submitted != want[dt] {
			t.Errorf("%s: %d submitted with an ExtID, the world has %d", dt, submitted, want[dt])
		}
	}
	if len(second.Map) != len(seen) {
		t.Errorf("the map has %d entries, ERPNext %d documents", len(second.Map), len(seen))
	}
	for ext, ref := range second.Map {
		if got := seen[ext]; got != ref.DocType+" "+ref.Name {
			t.Errorf("map %s = %s %s, ERPNext has %q", ext, ref.DocType, ref.Name, got)
		}
	}

	third, err := WriteBooks(ctx, c, rep, w, opt)
	if err != nil {
		t.Fatalf("third run: %v", err)
	}
	if n := third.Changes(); n != 0 {
		t.Errorf("third run made %d changes: %+v", n, countsOf(third))
	}
	if !mapsEqual(third.Map, second.Map) {
		t.Error("the third run's map differs from the second's")
	}
}

func TestIntegrationBooksTrialBalance(t *testing.T) {
	c := seederClient(t)
	ctx := context.Background()
	p, rep, w := booksITWorld(ctx, t, c)

	// Make sure the month is posted (a no-op after TestIntegrationBooksResume).
	if _, err := WriteBooks(ctx, c, rep, w, BooksOptions{Suppliers: p.Suppliers}); err != nil {
		t.Fatalf("write books: %v", err)
	}

	type glRow struct {
		Name    string `json:"name"`
		Account string `json:"account"`
		Debit   any    `json:"debit"`
		Credit  any    `json:"credit"`
	}
	rows, err := frappe.List[glRow](ctx, c, "GL Entry", frappe.Query{
		Fields: []string{"name", "account", "debit", "credit"},
		Filters: [][]any{
			{"company", "=", rep.Company},
			{"posting_date", "between", []string{"2026-08-01", "2026-08-31"}},
			{"is_cancelled", "=", 0},
		},
		OrderBy: "name asc",
	})
	if err != nil {
		t.Fatalf("list GL Entry: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("no GL entries for the month")
	}
	var dr, cr, bank money.Paise
	for _, r := range rows {
		d, err := paiseField(r.Debit)
		if err != nil {
			t.Fatalf("GL Entry %s debit: %v", r.Name, err)
		}
		k, err := paiseField(r.Credit)
		if err != nil {
			t.Fatalf("GL Entry %s credit: %v", r.Name, err)
		}
		dr += d
		cr += k
		if r.Account == rep.BankAccount {
			bank += d - k
		}
	}
	if dr != cr {
		t.Errorf("GL for %s: debits %s, credits %s", booksITMonth, dr.Format(), cr.Format())
	}
	var want money.Paise
	for _, ev := range w.Events {
		if BookDocType(ev.Kind) != "" {
			want += ev.BankDelta()
		}
	}
	if bank != want {
		t.Errorf("bank %s moved %s in the GL, the world's book-side events %s", rep.BankAccount, bank.Format(), want.Format())
	}
	t.Logf("%d GL entries, debits = credits = %s, bank movement %s", len(rows), dr.Format(), bank.Format())
}
