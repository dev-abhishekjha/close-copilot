package books

import (
	"github.com/abhishekjha/close-copilot/internal/frappe"
	"github.com/abhishekjha/close-copilot/internal/ledger"
)

// DefaultAmountBandPct is the amount band list_recurring_suppliers uses
// when the caller doesn't give one: every invoice of a recurring supplier
// is within 20% of its median.
const DefaultAmountBandPct = 20

// RecurringSupplier summarises a supplier that bills regularly. It is
// ledger.RecurringSupplier (CC-601a).
type RecurringSupplier = ledger.RecurringSupplier

// RecurringSuppliers finds the recurring suppliers among invoices. It
// forwards to ledger.RecurringSuppliers, which documents the rule; the
// books MCP tool and the checks share it.
func RecurringSuppliers(invoices []frappe.PurchaseInvoice, minOccurrences, amountBandPct int) []RecurringSupplier {
	return ledger.RecurringSuppliers(invoices, minOccurrences, amountBandPct)
}
