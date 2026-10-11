package checks

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/abhishekjha/close-copilot/internal/ledger"
	"github.com/abhishekjha/close-copilot/internal/money"
	"github.com/abhishekjha/close-copilot/internal/store"
)

const (
	// duplicateWindowDays is the fixed pairing window for Rule A, either side
	// of the suspect's posting date.
	duplicateWindowDays = 30
	// duplicateInvoiceLookbackMonths is how far back purchase invoices are read
	// so the original invoice of a duplicate can be found.
	duplicateInvoiceLookbackMonths = 6
	minPartyWordLen                = 4
)

// invoiceRefRegex matches invoice-number-like tokens in payment remarks:
// letters, digits, '/' and '-', at least four characters.
var invoiceRefRegex = regexp.MustCompile(`[A-Za-z0-9][A-Za-z0-9/\-]{3,}`)

// normInvoiceRef normalises an invoice number: upper-case, letters and digits
// only. It is to be replaced by the shared NormInvoiceNo once CC-403 lands.
func normInvoiceRef(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// DuplicatePaymentCheck flags supplier invoices that were paid more than once.
// ERPNext will not allocate beyond the outstanding amount, so a duplicate shows
// up as an unallocated advance to the same supplier.
type DuplicatePaymentCheck struct{}

var _ Check = (*DuplicatePaymentCheck)(nil)

// Name returns the identifier for this check.
func (d *DuplicatePaymentCheck) Name() string {
	return "duplicates"
}

type duplicateHit struct {
	invoice    string
	suspect    ledger.PaymentEntry
	originalPE string
	bankIDs    []string
}

// Run executes the duplicate vendor payment check.
func (d *DuplicatePaymentCheck) Run(ctx context.Context, in Inputs) ([]Finding, error) {
	startMonth, err := time.Parse("2006-01", in.Month)
	if err != nil {
		return nil, fmt.Errorf("checks: duplicates invalid month %q: %w", in.Month, err)
	}
	endMonth := startMonth.AddDate(0, 1, -1)
	payFrom := startMonth.AddDate(0, 0, -duplicateWindowDays)

	payments, err := in.Books.PaymentEntries(ctx, in.Company, payFrom, endMonth)
	if err != nil {
		return nil, fmt.Errorf("checks: duplicates load payment entries: %w", err)
	}
	invoices, err := in.Books.PurchaseInvoices(ctx, in.Company, startMonth.AddDate(0, -duplicateInvoiceLookbackMonths, 0), endMonth)
	if err != nil {
		return nil, fmt.Errorf("checks: duplicates load purchase invoices: %w", err)
	}
	bankLines, err := in.Evidence.BankLines(ctx, in.Company, payFrom, endMonth)
	if err != nil {
		return nil, fmt.Errorf("checks: duplicates load bank lines: %w", err)
	}

	var suspects []ledger.PaymentEntry
	for _, p := range payments {
		if isSupplierPay(p) && p.UnallocatedAmount > 0 &&
			!p.PostingDate.Before(startMonth) && !p.PostingDate.After(endMonth) {
			suspects = append(suspects, p)
		}
	}
	slices.SortFunc(suspects, func(a, b ledger.PaymentEntry) int {
		if c := a.PostingDate.Compare(b.PostingDate); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})

	var hits []duplicateHit
	for _, s := range suspects {
		invoice, originalPE := ruleA(s, payments)
		if name := ruleB(s, invoices); name != "" {
			invoice = name
		}
		if invoice == "" {
			continue
		}
		hits = append(hits, duplicateHit{
			invoice:    invoice,
			suspect:    s,
			originalPE: originalPE,
			bankIDs:    bankCorroboration(s, bankLines),
		})
	}

	byInvoice := map[string][]duplicateHit{}
	var order []string
	for _, h := range hits {
		if _, ok := byInvoice[h.invoice]; !ok {
			order = append(order, h.invoice)
		}
		byInvoice[h.invoice] = append(byInvoice[h.invoice], h)
	}
	slices.Sort(order)

	act := ActionFollowUpSupplier
	var findings []Finding
	for _, inv := range order {
		group := byInvoice[inv]
		first := group[0].suspect
		amt := first.PaidAmount
		severity := SeverityMedium
		var payIDs, bankIDs []string
		for _, h := range group {
			payIDs = append(payIDs, h.suspect.Name)
			if h.originalPE != "" && !slices.Contains(payIDs, h.originalPE) {
				payIDs = append(payIDs, h.originalPE)
			}
			if len(h.bankIDs) > 0 {
				severity = SeverityHigh
				for _, id := range h.bankIDs {
					if !slices.Contains(bankIDs, id) {
						bankIDs = append(bankIDs, id)
					}
				}
			}
		}
		slices.Sort(bankIDs)
		evidence := []EvidenceRef{
			BooksEvidence("list_payments", map[string]string{"company": in.Company, "month": in.Month}, payIDs...),
			BooksEvidence("list_purchase_invoices", map[string]string{"company": in.Company, "month": in.Month}, inv),
		}
		if len(bankIDs) > 0 {
			evidence = append(evidence, EvidenceEvidence("list_bank_lines", map[string]string{"company": in.Company, "month": in.Month}, bankIDs...))
		}
		findings = append(findings, Finding{
			Type:        TypeDuplicateVendorPayment,
			Severity:    severity,
			Title:       fmt.Sprintf("Duplicate payment to %s for invoice %s (%s)", first.Party, inv, amt.Format()),
			AmountPaise: &amt,
			Keys:        map[string]string{"invoice": inv},
			Evidence:    evidence,
			Action:      &act,
			Status:      StatusOpen,
		})
	}
	return findings, nil
}

func isSupplierPay(p ledger.PaymentEntry) bool {
	return p.PaymentType == "Pay" && p.PartyType == "Supplier"
}

// ruleA finds an allocated Pay entry to the same supplier with the same amount
// within the window, and returns its Purchase Invoice and its own name.
func ruleA(s ledger.PaymentEntry, payments []ledger.PaymentEntry) (invoice, paymentName string) {
	window := time.Duration(duplicateWindowDays) * 24 * time.Hour
	var best *ledger.PaymentEntry
	var bestInv string
	var bestGap time.Duration
	for i := range payments {
		p := &payments[i]
		if p.Name == s.Name || !isSupplierPay(*p) || p.Party != s.Party || p.PaidAmount != s.PaidAmount {
			continue
		}
		gap := p.PostingDate.Sub(s.PostingDate)
		if gap < 0 {
			gap = -gap
		}
		if gap > window {
			continue
		}
		inv := ""
		for _, r := range p.References {
			if r.ReferenceDoctype == "Purchase Invoice" && r.ReferenceName != "" {
				inv = r.ReferenceName
				break
			}
		}
		if inv == "" {
			continue
		}
		if best == nil || gap < bestGap || (gap == bestGap && p.Name < best.Name) {
			best, bestInv, bestGap = p, inv, gap
		}
	}
	if best == nil {
		return "", ""
	}
	return bestInv, best.Name
}

// ruleB finds a fully paid purchase invoice of the same supplier whose bill
// number is named in the suspect's remarks.
func ruleB(s ledger.PaymentEntry, invoices []ledger.PurchaseInvoice) string {
	tokens := map[string]bool{}
	for _, tok := range invoiceRefRegex.FindAllString(s.Remarks, -1) {
		if n := normInvoiceRef(tok); n != "" {
			tokens[n] = true
		}
	}
	if len(tokens) == 0 {
		return ""
	}
	best := ""
	for _, inv := range invoices {
		if inv.Supplier != s.Party || inv.OutstandingAmount != 0 {
			continue
		}
		n := normInvoiceRef(inv.BillNo)
		if n == "" || !tokens[n] {
			continue
		}
		if best == "" || inv.Name < best {
			best = inv.Name
		}
	}
	return best
}

// bankCorroboration returns the IDs of the withdrawals of the paid amount whose
// narrations name the supplier, when there are at least two of them.
func bankCorroboration(s ledger.PaymentEntry, lines []store.BankLine) []string {
	want := -money.Paise(s.PaidAmount)
	names := []string{s.Party, s.PartyName}
	var ids []string
	for _, l := range lines {
		if l.AmountPaise != want || !narrationNamesParty(l.Narration, names) {
			continue
		}
		ids = append(ids, l.TxnID)
	}
	if len(ids) < 2 {
		return nil
	}
	slices.Sort(ids)
	return ids
}

func narrationNamesParty(narration string, names []string) bool {
	n := strings.ToUpper(narration)
	for _, name := range names {
		name = strings.ToUpper(strings.TrimSpace(name))
		if name == "" {
			continue
		}
		if strings.Contains(n, name) {
			return true
		}
		for _, w := range strings.FieldsFunc(name, func(r rune) bool {
			return (r < 'A' || r > 'Z') && (r < '0' || r > '9')
		}) {
			if len(w) >= minPartyWordLen && strings.Contains(n, w) {
				return true
			}
		}
	}
	return false
}
