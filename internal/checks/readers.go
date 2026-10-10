package checks

import (
	"context"
	"errors"
	"time"

	"github.com/abhishekjha/close-copilot/internal/ledger"
	"github.com/abhishekjha/close-copilot/internal/store"
)

// BooksReader provides access to accounting books and ERPNext data, in the
// ERPNext-free types of internal/ledger. In E6 this is backed by
// direct.Books (internal/checks/direct); in E7 by MCP-backed readers.
type BooksReader interface {
	TrialBalance(ctx context.Context, company string, from, to time.Time) (ledger.TB, error)
	GLEntries(ctx context.Context, company string, from, to time.Time) ([]ledger.GLEntry, error)
	PurchaseInvoices(ctx context.Context, company string, from, to time.Time) ([]ledger.PurchaseInvoice, error)
	SalesInvoices(ctx context.Context, company string, from, to time.Time) ([]ledger.SalesInvoice, error)
	PaymentEntries(ctx context.Context, company string, from, to time.Time) ([]ledger.PaymentEntry, error)
	AccountHistory(ctx context.Context, company, account string, through string, months int) ([]ledger.MonthTotal, error)
	RecurringSuppliers(ctx context.Context, company string, beforeMonth string, lookbackMonths int, minOccurrences int, amountBandPct int) ([]RecurringSupplier, error)
}

// EvidenceReader provides access to external evidence data stored in Postgres.
// In E6 this is backed by DirectEvidence; in E7 by MCP-backed readers.
type EvidenceReader interface {
	BankLines(ctx context.Context, company string, from, to time.Time) ([]store.BankLine, error)
	GSTR2BEntries(ctx context.Context, company string, period string) ([]store.GSTR2BEntry, error)
}

// RecurringSupplier summarizes a supplier with regular, recurring monthly
// billing. It is ledger.RecurringSupplier, which the books MCP tool
// list_recurring_suppliers returns too (CC-502).
type RecurringSupplier = ledger.RecurringSupplier

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
