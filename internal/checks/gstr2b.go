package checks

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/abhishekjha/close-copilot/internal/evidence"
	"github.com/abhishekjha/close-copilot/internal/ledger"
	"github.com/abhishekjha/close-copilot/internal/money"
	"github.com/abhishekjha/close-copilot/internal/store"
)

// GSTR2BCheck compares the month's purchase invoices with GSTR-2B and
// reports every difference as input tax credit (ITC) at risk.
type GSTR2BCheck struct{}

var _ Check = (*GSTR2BCheck)(nil)

// Name returns the identifier for this check.
func (g *GSTR2BCheck) Name() string { return "gstr2b" }

type gstrKey struct{ gstin, norm string }

// gstrBook is one purchase invoice (or the first of several sharing a key).
type gstrBook struct {
	names          []string
	billNo         string
	supplier       string
	igst, cgst, sg money.Paise
}

func (b gstrBook) total() money.Paise { return b.igst + b.cgst + b.sg }

func gstrTotal(e store.GSTR2BEntry) money.Paise { return e.IGSTPaise + e.CGSTPaise + e.SGSTPaise }

// gstrKind is the GST head of a tax row: its gst_tax_type when that is
// igst, cgst or sgst, or, when empty, the IGST, CGST or SGST word in the
// account head. Anything else (cess, RCM, TDS, freight) is "".
func gstrKind(gstTaxType, accountHead string) string {
	switch t := strings.ToLower(gstTaxType); t {
	case "igst", "cgst", "sgst":
		return t
	case "":
		for _, k := range []string{"IGST", "CGST", "SGST"} {
			if strings.Contains(accountHead, k) {
				return strings.ToLower(k)
			}
		}
	}
	return ""
}

// gstrSplit sums the IGST, CGST and SGST rows; a Deduct row counts negative.
func gstrSplit(taxes []ledger.PurchaseTaxesAndCharges) (igst, cgst, sgst money.Paise) {
	for _, t := range taxes {
		amt := t.TaxAmount
		if t.AddDeductTax == "Deduct" {
			amt = -amt
		}
		switch gstrKind(t.GSTTaxType, t.AccountHead) {
		case "igst":
			igst += amt
		case "cgst":
			cgst += amt
		case "sgst":
			sgst += amt
		}
	}
	return igst, cgst, sgst
}

// gstrBooksByMonth groups registered, non-return invoices whose bill date
// (posting date when unset) is in month, keyed by GSTIN and normalised bill
// number. Of invoices sharing a key the first by name is kept.
func gstrBooksByMonth(invs []ledger.PurchaseInvoice, month string) map[gstrKey]gstrBook {
	sorted := slices.Clone(invs)
	slices.SortStableFunc(sorted, func(a, b ledger.PurchaseInvoice) int { return strings.Compare(a.Name, b.Name) })
	out := map[gstrKey]gstrBook{}
	for _, inv := range sorted {
		if inv.IsReturn || inv.SupplierGSTIN == "" {
			continue
		}
		d := inv.BillDate
		if d.IsZero() {
			d = inv.PostingDate
		}
		if d.Format("2006-01") != month {
			continue
		}
		k := gstrKey{inv.SupplierGSTIN, evidence.NormInvoiceNo(inv.BillNo)}
		if b, ok := out[k]; ok {
			b.names = append(b.names, inv.Name)
			out[k] = b
			continue
		}
		i, c, s := gstrSplit(inv.Taxes)
		name := inv.SupplierName
		if name == "" {
			name = inv.Supplier
		}
		out[k] = gstrBook{names: []string{inv.Name}, billNo: inv.BillNo, supplier: name, igst: i, cgst: c, sg: s}
	}
	return out
}

func gstrEntries(es []store.GSTR2BEntry) map[gstrKey]store.GSTR2BEntry {
	m := make(map[gstrKey]store.GSTR2BEntry, len(es))
	for _, e := range es {
		m[gstrKey{e.SupplierGSTIN, e.InvoiceNoNorm}] = e
	}
	return m
}

func gstrAbs(p money.Paise) money.Paise {
	if p < 0 {
		return -p
	}
	return p
}

// Run executes the GSTR-2B match for in.Month.
func (g *GSTR2BCheck) Run(ctx context.Context, in Inputs) ([]Finding, error) {
	start, err := time.Parse("2006-01", in.Month)
	if err != nil {
		return nil, fmt.Errorf("checks: gstr2b invalid month %q: %w", in.Month, err)
	}
	end := start.AddDate(0, 1, -1)
	prev := start.AddDate(0, -1, 0).Format("2006-01")
	next := start.AddDate(0, 1, 0).Format("2006-01")
	tol := money.Paise(in.Rules.GSTR2B.AmountTolerancePaise)

	invs, err := in.Books.PurchaseInvoices(ctx, in.Company, start.AddDate(0, -1, 0), start.AddDate(0, 2, -1))
	if err != nil {
		return nil, fmt.Errorf("checks: gstr2b purchase invoices: %w", err)
	}
	periods := map[string]map[gstrKey]store.GSTR2BEntry{}
	for _, p := range []string{prev, in.Month, next} {
		es, err := in.Evidence.GSTR2BEntries(ctx, in.Company, p)
		if err != nil {
			return nil, fmt.Errorf("checks: gstr2b entries for %s: %w", p, err)
		}
		periods[p] = gstrEntries(es)
	}
	booksM := gstrBooksByMonth(invs, in.Month)
	booksPrev := gstrBooksByMonth(invs, prev)
	booksNext := gstrBooksByMonth(invs, next)
	twoB := periods[in.Month]

	booksArgs := func(b gstrBook) map[string]any {
		return map[string]any{
			"company": in.Company, "from": start.Format("2006-01-02"), "to": end.Format("2006-01-02"),
			"books_tax_paise": int64(b.total()), "igst_paise": int64(b.igst),
			"cgst_paise": int64(b.cgst), "sgst_paise": int64(b.sg),
		}
	}
	twoBArgs := func(period string, e store.GSTR2BEntry) map[string]any {
		return map[string]any{
			"company": in.Company, "period": period, "supplier_gstin": e.SupplierGSTIN,
			"gstr2b_tax_paise": int64(gstrTotal(e)), "igst_paise": int64(e.IGSTPaise),
			"cgst_paise": int64(e.CGSTPaise), "sgst_paise": int64(e.SGSTPaise),
			"itc_available": e.ITCAvailable,
		}
	}

	var findings []Finding
	add := func(typ string, k gstrKey, billNo, supplier string, amt money.Paise, refs []EvidenceRef) {
		sev, act := SeverityHigh, ActionFollowUpSupplier
		switch typ {
		case TypeGSTR2BWrongPeriod:
			sev = SeverityMedium
		case TypeGSTR2BMissingInBooks, TypeGSTR2BNotEligible:
			act = ActionInvestigate
		}
		var what string
		switch typ {
		case TypeGSTR2BAmountMismatch:
			what = "GSTR-2B tax differs from books"
		case TypeGSTR2BMissingIn2B:
			what = "invoice missing in GSTR-2B"
		case TypeGSTR2BWrongPeriod:
			what = "invoice appears in another GSTR-2B period"
		case TypeGSTR2BMissingInBooks:
			what = "GSTR-2B invoice not in books"
		case TypeGSTR2BNotEligible:
			what = "ITC not available in GSTR-2B"
		}
		a := amt
		findings = append(findings, Finding{
			Type:        typ,
			Severity:    sev,
			Title:       fmt.Sprintf("%s: %s, invoice %s, ITC %s", what, supplier, billNo, amt.Format()),
			AmountPaise: &a,
			Keys:        map[string]string{"supplier_gstin": k.gstin, "invoice_no_norm": k.norm},
			Evidence:    refs,
			Action:      &act,
			Status:      StatusOpen,
		})
	}

	for k, b := range booksM {
		booksRef := BooksEvidence("list_purchase_invoices", booksArgs(b), b.names...)
		if e, ok := twoB[k]; ok {
			ref2b := EvidenceEvidence("list_gstr2b_entries", twoBArgs(in.Month, e), e.InvoiceNoNorm)
			refs := []EvidenceRef{booksRef, ref2b}
			if !e.ITCAvailable {
				add(TypeGSTR2BNotEligible, k, b.billNo, b.supplier, b.total(), refs)
				continue
			}
			diff := func(x, y money.Paise) bool { return gstrAbs(x-y) > tol }
			if diff(b.igst, e.IGSTPaise) || diff(b.cgst, e.CGSTPaise) || diff(b.sg, e.SGSTPaise) {
				add(TypeGSTR2BAmountMismatch, k, b.billNo, b.supplier, gstrAbs(b.total()-gstrTotal(e)), refs)
			}
			continue
		}
		found := false
		for _, p := range []string{prev, next} {
			if e, ok := periods[p][k]; ok {
				add(TypeGSTR2BWrongPeriod, k, b.billNo, b.supplier, b.total(), []EvidenceRef{
					booksRef, EvidenceEvidence("list_gstr2b_entries", twoBArgs(p, e), e.InvoiceNoNorm),
				})
				found = true
				break
			}
		}
		if !found {
			add(TypeGSTR2BMissingIn2B, k, b.billNo, b.supplier, b.total(), []EvidenceRef{booksRef})
		}
	}

	for k, e := range twoB {
		if _, ok := booksM[k]; ok || !e.ITCAvailable {
			continue
		}
		if _, ok := booksPrev[k]; ok {
			continue
		}
		if _, ok := booksNext[k]; ok {
			continue
		}
		name := k.gstin
		if e.SupplierName != nil && *e.SupplierName != "" {
			name = *e.SupplierName
		}
		add(TypeGSTR2BMissingInBooks, k, e.InvoiceNo, name, gstrTotal(e), []EvidenceRef{
			EvidenceEvidence("list_gstr2b_entries", twoBArgs(in.Month, e), e.InvoiceNoNorm),
		})
	}

	slices.SortFunc(findings, func(a, b Finding) int {
		if c := strings.Compare(a.Type, b.Type); c != 0 {
			return c
		}
		if c := strings.Compare(a.Keys["supplier_gstin"], b.Keys["supplier_gstin"]); c != 0 {
			return c
		}
		return strings.Compare(a.Keys["invoice_no_norm"], b.Keys["invoice_no_norm"])
	})
	return findings, nil
}
