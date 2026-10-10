package direct

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/abhishekjha/close-copilot/internal/books"
	"github.com/abhishekjha/close-copilot/internal/checks"
	"github.com/abhishekjha/close-copilot/internal/frappe"
	"github.com/abhishekjha/close-copilot/internal/store"
)

// CompanyDirectory resolves a company ID ("sharma") to its stored record,
// whose ERPCompany is the name ERPNext knows the company by.
// *store.Store implements CompanyDirectory.
type CompanyDirectory interface {
	GetCompany(ctx context.Context, id string) (store.Company, error)
}

var _ CompanyDirectory = (*store.Store)(nil)

// Books implements checks.BooksReader directly against the ERPNext REST
// API client. Like the books MCP tools, its methods take the company ID and
// resolve it to the ERPNext company name through Companies before querying.
type Books struct {
	Client    *frappe.Client
	Companies CompanyDirectory
}

var _ checks.BooksReader = (*Books)(nil)

// erpCompany checks the client and directory, and returns the ERPNext
// company name for the company ID.
func (d *Books) erpCompany(ctx context.Context, company string) (string, error) {
	if d.Client == nil {
		return "", errors.New("checks: direct.Books has nil client")
	}
	if d.Companies == nil {
		return "", errors.New("checks: direct.Books has nil company directory")
	}
	if company == "" {
		return "", errors.New("checks: direct.Books: empty company ID")
	}
	c, err := d.Companies.GetCompany(ctx, company)
	if err != nil {
		return "", fmt.Errorf("checks: resolve company %q: %w", company, err)
	}
	if c.ERPCompany == "" {
		return "", fmt.Errorf("checks: resolve company %q: no ERPNext company name", company)
	}
	return c.ERPCompany, nil
}

// TrialBalance calculates the trial balance for the given period.
func (d *Books) TrialBalance(ctx context.Context, company string, from, to time.Time) (books.TB, error) {
	erp, err := d.erpCompany(ctx, company)
	if err != nil {
		return books.TB{}, err
	}
	return books.TrialBalance(ctx, books.FrappeLedger{C: d.Client}, erp, from, to)
}

// GLEntries returns non-cancelled GL entries for the given date range.
func (d *Books) GLEntries(ctx context.Context, company string, from, to time.Time) ([]frappe.GLEntry, error) {
	erp, err := d.erpCompany(ctx, company)
	if err != nil {
		return nil, err
	}
	return books.FrappeLedger{C: d.Client}.GLEntries(ctx, erp, from, to)
}

// PurchaseInvoices returns submitted (docstatus 1) purchase invoices with
// items and taxes. Drafts and cancelled invoices are not in the ledger.
func (d *Books) PurchaseInvoices(ctx context.Context, company string, from, to time.Time) ([]frappe.PurchaseInvoice, error) {
	erp, err := d.erpCompany(ctx, company)
	if err != nil {
		return nil, err
	}
	return listSubmitted(ctx, d.Client, frappe.DocTypePurchaseInvoice, erp, from, to,
		func(r frappe.PurchaseInvoiceRaw) string { return r.Name },
		frappe.PurchaseInvoiceRaw.Domain)
}

// SalesInvoices returns submitted (docstatus 1) sales invoices with items
// and taxes.
func (d *Books) SalesInvoices(ctx context.Context, company string, from, to time.Time) ([]frappe.SalesInvoice, error) {
	erp, err := d.erpCompany(ctx, company)
	if err != nil {
		return nil, err
	}
	return listSubmitted(ctx, d.Client, frappe.DocTypeSalesInvoice, erp, from, to,
		func(r frappe.SalesInvoiceRaw) string { return r.Name },
		frappe.SalesInvoiceRaw.Domain)
}

// PaymentEntries returns submitted (docstatus 1) payment entries with
// child reference rows.
func (d *Books) PaymentEntries(ctx context.Context, company string, from, to time.Time) ([]frappe.PaymentEntry, error) {
	erp, err := d.erpCompany(ctx, company)
	if err != nil {
		return nil, err
	}
	return listSubmitted(ctx, d.Client, frappe.DocTypePaymentEntry, erp, from, to,
		func(r frappe.PaymentEntryRaw) string { return r.Name },
		frappe.PaymentEntryRaw.Domain)
}

// submittedFilters returns the list filters for submitted documents of
// erpCompany posted between from and to (either may be zero for open).
func submittedFilters(erpCompany string, from, to time.Time) [][]any {
	filters := [][]any{
		{"company", "=", erpCompany},
		{"docstatus", "=", 1},
	}
	switch {
	case !from.IsZero() && !to.IsZero():
		filters = append(filters, []any{"posting_date", "between", []string{from.Format(time.DateOnly), to.Format(time.DateOnly)}})
	case !to.IsZero():
		filters = append(filters, []any{"posting_date", "<=", to.Format(time.DateOnly)})
	case !from.IsZero():
		filters = append(filters, []any{"posting_date", ">=", from.Format(time.DateOnly)})
	}
	return filters
}

// listSubmitted lists the names of submitted documents of doctype, fetches
// each in full (child tables included) and converts it to its domain type.
func listSubmitted[R, D any](ctx context.Context, c *frappe.Client, doctype, erpCompany string, from, to time.Time, name func(R) string, conv func(R) (D, error)) ([]D, error) {
	rawList, err := frappe.List[R](ctx, c, doctype, frappe.Query{
		Fields:  []string{"name"},
		Filters: submittedFilters(erpCompany, from, to),
		OrderBy: "posting_date asc, name asc",
	})
	if err != nil {
		return nil, fmt.Errorf("checks: list %s: %w", doctype, err)
	}
	if len(rawList) == 0 {
		return nil, nil
	}

	names := make([]string, len(rawList))
	for i, r := range rawList {
		names[i] = name(r)
	}

	fullRaws, err := frappe.GetMany[R](ctx, c, doctype, names)
	if err != nil {
		return nil, fmt.Errorf("checks: get %s: %w", doctype, err)
	}

	out := make([]D, len(fullRaws))
	for i, r := range fullRaws {
		dom, err := conv(r)
		if err != nil {
			return nil, fmt.Errorf("checks: convert %s %s: %w", doctype, name(r), err)
		}
		out[i] = dom
	}
	return out, nil
}

// AccountHistory returns the monthly totals for an account over the requested months ending with through.
func (d *Books) AccountHistory(ctx context.Context, company, account string, through string, months int) ([]books.MonthTotal, error) {
	erp, err := d.erpCompany(ctx, company)
	if err != nil {
		return nil, err
	}
	return books.AccountHistory(ctx, books.FrappeLedger{C: d.Client}, erp, account, through, months)
}

// RecurringSuppliers identifies suppliers who billed consistently in lookbackMonths before beforeMonth.
func (d *Books) RecurringSuppliers(ctx context.Context, company string, beforeMonth string, lookbackMonths int, minOccurrences int, amountBandPct int) ([]checks.RecurringSupplier, error) {
	lastDate, err := time.Parse("2006-01", beforeMonth)
	if err != nil {
		return nil, fmt.Errorf("checks: recurring suppliers: invalid beforeMonth %q: %w", beforeMonth, err)
	}
	if lookbackMonths <= 0 {
		return nil, fmt.Errorf("checks: recurring suppliers: lookbackMonths must be positive, got %d", lookbackMonths)
	}

	startDate := lastDate.AddDate(0, -lookbackMonths, 0)
	endDate := lastDate.AddDate(0, 0, -1) // last day of month prior to beforeMonth

	invoices, err := d.PurchaseInvoices(ctx, company, startDate, endDate)
	if err != nil {
		return nil, fmt.Errorf("checks: recurring suppliers: %w", err)
	}

	return computeRecurringSuppliers(invoices, minOccurrences, amountBandPct), nil
}

// computeRecurringSuppliers processes purchase invoices and determines
// recurring suppliers. The rule lives in ledger.RecurringSuppliers (through
// books.RecurringSuppliers), shared with the list_recurring_suppliers MCP
// tool.
func computeRecurringSuppliers(invoices []frappe.PurchaseInvoice, minOccurrences, amountBandPct int) []checks.RecurringSupplier {
	return books.RecurringSuppliers(invoices, minOccurrences, amountBandPct)
}
