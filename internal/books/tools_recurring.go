package books

import (
	"slices"
	"strings"

	"github.com/abhishekjha/close-copilot/internal/frappe"
	"github.com/abhishekjha/close-copilot/internal/money"
)

// DefaultAmountBandPct is the amount band list_recurring_suppliers uses
// when the caller doesn't give one: every invoice of a recurring supplier
// is within 20% of its median.
const DefaultAmountBandPct = 20

// RecurringSupplier summarises a supplier that bills regularly: the median
// invoice total, the typical day of the month it bills on, and the months
// (YYYY-MM, ascending) it billed in.
type RecurringSupplier struct {
	Supplier     string      `json:"supplier" jsonschema:"ERPNext Supplier name (ID)"`
	MedianAmount money.Paise `json:"median_amount" jsonschema:"median invoice grand total, in paise (1 rupee = 100 paise)"`
	TypicalDay   int         `json:"typical_day" jsonschema:"typical day of the month the supplier bills on, 1 to 31"`
	MonthsSeen   []string    `json:"months_seen" jsonschema:"months the supplier billed in, YYYY-MM, oldest first"`
}

// RecurringSuppliers finds the suppliers among invoices that billed in at
// least minOccurrences distinct months with every invoice's grand total
// within amountBandPct percent of their median. The median is the upper
// middle value of the sorted totals; the typical day is the mean of each
// invoice's bill date day (posting date when the bill date is unset),
// clamped to 1..31. Invoices without a supplier are ignored. The result is
// sorted by supplier and is nil when no supplier qualifies.
//
// It moved here from internal/checks (CC-502) so the books MCP tool and
// the checks share one rule.
func RecurringSuppliers(invoices []frappe.PurchaseInvoice, minOccurrences, amountBandPct int) []RecurringSupplier {
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
			data = &supplierData{months: make(map[string]bool)}
			bySupplier[inv.Supplier] = data
		}
		data.months[inv.PostingDate.Format(monthLayout)] = true
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

		slices.Sort(data.amounts)
		median := data.amounts[len(data.amounts)/2]

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

		sumDays := 0
		for _, d := range data.days {
			sumDays += d
		}
		typicalDay := min(max(sumDays/len(data.days), 1), 31)

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

	slices.SortFunc(out, func(a, b RecurringSupplier) int { return strings.Compare(a.Supplier, b.Supplier) })
	return out
}
