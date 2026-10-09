package checks

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/abhishekjha/close-copilot/internal/books"
	"github.com/abhishekjha/close-copilot/internal/frappe"
	"github.com/abhishekjha/close-copilot/internal/money"
	"github.com/abhishekjha/close-copilot/internal/store"
)

// BooksReader provides access to accounting books and ERPNext data.
// In E6 this is backed by DirectBooks; in E7 by MCP-backed readers.
type BooksReader interface {
	TrialBalance(ctx context.Context, company string, from, to time.Time) (books.TB, error)
	GLEntries(ctx context.Context, company string, from, to time.Time) ([]frappe.GLEntry, error)
	PurchaseInvoices(ctx context.Context, company string, from, to time.Time) ([]frappe.PurchaseInvoice, error)
	SalesInvoices(ctx context.Context, company string, from, to time.Time) ([]frappe.SalesInvoice, error)
	PaymentEntries(ctx context.Context, company string, from, to time.Time) ([]frappe.PaymentEntry, error)
	AccountHistory(ctx context.Context, company, account string, through string, months int) ([]books.MonthTotal, error)
	RecurringSuppliers(ctx context.Context, company string, beforeMonth string, lookbackMonths int, minOccurrences int, amountBandPct int) ([]RecurringSupplier, error)
}

// EvidenceReader provides access to external evidence data stored in Postgres.
// In E6 this is backed by DirectEvidence; in E7 by MCP-backed readers.
type EvidenceReader interface {
	BankLines(ctx context.Context, company string, from, to time.Time) ([]store.BankLine, error)
	GSTR2BEntries(ctx context.Context, company string, period string) ([]store.GSTR2BEntry, error)
}

// RecurringSupplier summarizes a supplier with regular, recurring monthly billing.
type RecurringSupplier struct {
	Supplier     string      `json:"supplier"`
	MedianAmount money.Paise `json:"median_amount"`
	TypicalDay   int         `json:"typical_day"`
	MonthsSeen   []string    `json:"months_seen"`
}

// CompanyDirectory resolves a company ID ("sharma") to its stored record,
// whose ERPCompany is the name ERPNext knows the company by.
// *store.Store implements CompanyDirectory.
type CompanyDirectory interface {
	GetCompany(ctx context.Context, id string) (store.Company, error)
}

var _ CompanyDirectory = (*store.Store)(nil)

// DirectBooks implements BooksReader directly against the ERPNext REST API
// client. Like the books MCP tools, its methods take the company ID and
// resolve it to the ERPNext company name through Companies before querying.
type DirectBooks struct {
	Client    *frappe.Client
	Companies CompanyDirectory
}

var _ BooksReader = (*DirectBooks)(nil)

// erpCompany checks the client and directory, and returns the ERPNext
// company name for the company ID.
func (d *DirectBooks) erpCompany(ctx context.Context, company string) (string, error) {
	if d.Client == nil {
		return "", errors.New("checks: DirectBooks has nil client")
	}
	if d.Companies == nil {
		return "", errors.New("checks: DirectBooks has nil company directory")
	}
	if company == "" {
		return "", errors.New("checks: DirectBooks: empty company ID")
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
func (d *DirectBooks) TrialBalance(ctx context.Context, company string, from, to time.Time) (books.TB, error) {
	erp, err := d.erpCompany(ctx, company)
	if err != nil {
		return books.TB{}, err
	}
	return books.TrialBalance(ctx, books.FrappeLedger{C: d.Client}, erp, from, to)
}

// GLEntries returns non-cancelled GL entries for the given date range.
func (d *DirectBooks) GLEntries(ctx context.Context, company string, from, to time.Time) ([]frappe.GLEntry, error) {
	erp, err := d.erpCompany(ctx, company)
	if err != nil {
		return nil, err
	}
	return books.FrappeLedger{C: d.Client}.GLEntries(ctx, erp, from, to)
}

// PurchaseInvoices returns submitted (docstatus 1) purchase invoices with
// items and taxes. Drafts and cancelled invoices are not in the ledger.
func (d *DirectBooks) PurchaseInvoices(ctx context.Context, company string, from, to time.Time) ([]frappe.PurchaseInvoice, error) {
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
func (d *DirectBooks) SalesInvoices(ctx context.Context, company string, from, to time.Time) ([]frappe.SalesInvoice, error) {
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
func (d *DirectBooks) PaymentEntries(ctx context.Context, company string, from, to time.Time) ([]frappe.PaymentEntry, error) {
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
func (d *DirectBooks) AccountHistory(ctx context.Context, company, account string, through string, months int) ([]books.MonthTotal, error) {
	erp, err := d.erpCompany(ctx, company)
	if err != nil {
		return nil, err
	}
	return books.AccountHistory(ctx, books.FrappeLedger{C: d.Client}, erp, account, through, months)
}

// RecurringSuppliers identifies suppliers who billed consistently in lookbackMonths before beforeMonth.
func (d *DirectBooks) RecurringSuppliers(ctx context.Context, company string, beforeMonth string, lookbackMonths int, minOccurrences int, amountBandPct int) ([]RecurringSupplier, error) {
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

// computeRecurringSuppliers processes purchase invoices and determines recurring suppliers.
func computeRecurringSuppliers(invoices []frappe.PurchaseInvoice, minOccurrences, amountBandPct int) []RecurringSupplier {
	type supplierData struct {
		months  map[string]bool
		amounts []money.Paise
		days    []int
	}

	bySupplier := make(map[string]*supplierData)
	for _, inv := range invoices {
		if inv.Supplier == "" {
			continue
		}
		data := bySupplier[inv.Supplier]
		if data == nil {
			data = &supplierData{
				months: make(map[string]bool),
			}
			bySupplier[inv.Supplier] = data
		}
		monthStr := inv.PostingDate.Format("2006-01")
		data.months[monthStr] = true
		data.amounts = append(data.amounts, inv.GrandTotal)
		day := inv.PostingDate.Day()
		if !inv.BillDate.IsZero() {
			day = inv.BillDate.Day()
		}
		data.days = append(data.days, day)
	}

	var out []RecurringSupplier
	for supplier, data := range bySupplier {
		if len(data.months) < minOccurrences || len(data.amounts) == 0 {
			continue
		}

		// Sort amounts to compute median
		slices.Sort(data.amounts)
		mid := len(data.amounts) / 2
		median := data.amounts[mid]

		// Check amount band tolerance
		tolerance := (int64(median) * int64(amountBandPct)) / 100
		allWithin := true
		for _, a := range data.amounts {
			diff := int64(a - median)
			if diff < 0 {
				diff = -diff
			}
			if diff > tolerance {
				allWithin = false
				break
			}
		}
		if !allWithin {
			continue
		}

		// Calculate typical day of month
		sumDays := 0
		for _, d := range data.days {
			sumDays += d
		}
		typicalDay := sumDays / len(data.days)
		if typicalDay < 1 {
			typicalDay = 1
		} else if typicalDay > 31 {
			typicalDay = 31
		}

		monthsSeen := make([]string, 0, len(data.months))
		for m := range data.months {
			monthsSeen = append(monthsSeen, m)
		}
		slices.Sort(monthsSeen)

		out = append(out, RecurringSupplier{
			Supplier:     supplier,
			MedianAmount: median,
			TypicalDay:   typicalDay,
			MonthsSeen:   monthsSeen,
		})
	}

	slices.SortFunc(out, func(a, b RecurringSupplier) int {
		if a.Supplier < b.Supplier {
			return -1
		}
		if a.Supplier > b.Supplier {
			return 1
		}
		return 0
	})

	return out
}

// EvidenceStore defines the database methods needed by DirectEvidence.
// *store.Store implements EvidenceStore.
type EvidenceStore interface {
	ListBankLines(ctx context.Context, filter store.BankLineFilter) ([]store.BankLine, error)
	ListGSTR2BEntries(ctx context.Context, companyID, period string) ([]store.GSTR2BEntry, error)
}

// DirectEvidence implements EvidenceReader directly over the Postgres store.
type DirectEvidence struct {
	Store EvidenceStore
}

var _ EvidenceReader = (*DirectEvidence)(nil)

// BankLines returns bank statement lines within the given date range.
func (d *DirectEvidence) BankLines(ctx context.Context, company string, from, to time.Time) ([]store.BankLine, error) {
	if d.Store == nil {
		return nil, errors.New("checks: DirectEvidence has nil store")
	}
	var fromPtr, toPtr *time.Time
	if !from.IsZero() {
		fromPtr = &from
	}
	if !to.IsZero() {
		toPtr = &to
	}
	return d.Store.ListBankLines(ctx, store.BankLineFilter{
		CompanyID: company,
		FromDate:  fromPtr,
		ToDate:    toPtr,
	})
}

// GSTR2BEntries returns GSTR-2B inward supply entries for the given return period (YYYY-MM).
func (d *DirectEvidence) GSTR2BEntries(ctx context.Context, company string, period string) ([]store.GSTR2BEntry, error) {
	if d.Store == nil {
		return nil, errors.New("checks: DirectEvidence has nil store")
	}
	return d.Store.ListGSTR2BEntries(ctx, company, period)
}
