package faults

import (
	"context"
	"fmt"
	"time"

	"github.com/abhishekjha/close-copilot/internal/checks"
	"github.com/abhishekjha/close-copilot/internal/ledger"
)

// ErrUnavailable is wrapped by the error a faulted reader returns for an
// error fault. Its text reads "ERPNext unavailable", as the real client's
// breaker error does.
var ErrUnavailable = fmt.Errorf("ERPNext unavailable: %w", ErrInjected)

// apply plays the decision for one reader call: it sleeps for the delay
// and returns an error for an error fault.
func apply(ctx context.Context, inj *Injector, sleep SleepFunc, call string) error {
	d := inj.Next()
	if d.Delay > 0 {
		if sleep == nil {
			sleep = RealSleep
		}
		if err := sleep(ctx, d.Delay); err != nil {
			return err
		}
	}
	if d.Status != 0 {
		return fmt.Errorf("%s: HTTP %d: %w", call, d.Status, ErrUnavailable)
	}
	return nil
}

// Books wraps a books reader (a replay, which has no ERPNext to fail) so
// each call may be delayed or fail as the erp faults say. Evidence is read
// from Postgres, not ERPNext, so it is never faulted.
type Books struct {
	Reader checks.BooksReader
	Inj    *Injector
	Sleep  SleepFunc
}

var _ checks.BooksReader = (*Books)(nil)

func (b *Books) TrialBalance(ctx context.Context, company string, from, to time.Time) (ledger.TB, error) {
	if err := apply(ctx, b.Inj, b.Sleep, "trial_balance"); err != nil {
		return ledger.TB{}, err
	}
	return b.Reader.TrialBalance(ctx, company, from, to)
}

func (b *Books) GLEntries(ctx context.Context, company string, from, to time.Time) ([]ledger.GLEntry, error) {
	if err := apply(ctx, b.Inj, b.Sleep, "gl_entries"); err != nil {
		return nil, err
	}
	return b.Reader.GLEntries(ctx, company, from, to)
}

func (b *Books) PurchaseInvoices(ctx context.Context, company string, from, to time.Time) ([]ledger.PurchaseInvoice, error) {
	if err := apply(ctx, b.Inj, b.Sleep, "purchase_invoices"); err != nil {
		return nil, err
	}
	return b.Reader.PurchaseInvoices(ctx, company, from, to)
}

func (b *Books) SalesInvoices(ctx context.Context, company string, from, to time.Time) ([]ledger.SalesInvoice, error) {
	if err := apply(ctx, b.Inj, b.Sleep, "sales_invoices"); err != nil {
		return nil, err
	}
	return b.Reader.SalesInvoices(ctx, company, from, to)
}

func (b *Books) PaymentEntries(ctx context.Context, company string, from, to time.Time) ([]ledger.PaymentEntry, error) {
	if err := apply(ctx, b.Inj, b.Sleep, "payment_entries"); err != nil {
		return nil, err
	}
	return b.Reader.PaymentEntries(ctx, company, from, to)
}

func (b *Books) AccountHistory(ctx context.Context, company, account string, through string, months int) ([]ledger.MonthTotal, error) {
	if err := apply(ctx, b.Inj, b.Sleep, "account_history"); err != nil {
		return nil, err
	}
	return b.Reader.AccountHistory(ctx, company, account, through, months)
}

func (b *Books) RecurringSuppliers(ctx context.Context, company string, beforeMonth string, lookbackMonths int, minOccurrences int, amountBandPct int) ([]checks.RecurringSupplier, error) {
	if err := apply(ctx, b.Inj, b.Sleep, "recurring_suppliers"); err != nil {
		return nil, err
	}
	return b.Reader.RecurringSuppliers(ctx, company, beforeMonth, lookbackMonths, minOccurrences, amountBandPct)
}
