package seed

import (
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"slices"
	"sort"
	"time"

	"github.com/abhishekjha/close-copilot/internal/money"
)

// DefaultLateShareBP is the default share of registered-supplier invoices
// filed late: 500 basis points, 5%.
const DefaultLateShareBP = 500

// GSTR2BOptions tune BuildGSTR2B.
type GSTR2BOptions struct {
	// LateShareBP is the share of registered-supplier invoices the
	// supplier files late, in basis points (0 to 10000). A late invoice
	// dated in month M appears in the GSTR-2B of M+1 instead of M. Zero
	// means none; DefaultGSTR2BOptions sets DefaultLateShareBP.
	LateShareBP int
}

// DefaultGSTR2BOptions returns the options cmd/seed uses.
func DefaultGSTR2BOptions() GSTR2BOptions { return GSTR2BOptions{LateShareBP: DefaultLateShareBP} }

// IsLate reports whether the invoice of the purchase with this ExtID is
// filed late: FNV-1a 64 of the ExtID, mod 10000, below LateShareBP. It
// depends on nothing else, so an invoice is always late or always on time.
func (o GSTR2BOptions) IsLate(extID string) bool {
	h := fnv.New64a()
	_, _ = h.Write([]byte(extID))
	return h.Sum64()%10000 < uint64(max(o.LateShareBP, 0)) //nolint:gosec // G115: clamped to non-negative
}

// GSTR2B is one return period's simplified GSTR-2B (shared spec "GSTR-2B").
// Data is what gstr2b.json holds. The other fields stay outside the JSON:
// they tell ground truth (CC-306) which purchases the return covers.
type GSTR2B struct {
	Data GSTR2BData `json:"data"`
	// Included is every purchase ExtID in this return, sorted.
	Included []string `json:"-"`
	// Late is the ExtIDs in this return that are dated in the previous
	// month and were filed late, sorted.
	Late []string `json:"-"`
	// Deferred is the ExtIDs of registered-supplier purchases dated in this
	// period that were filed late, so they are absent here and appear in the
	// next period's return, sorted.
	Deferred []string `json:"-"`
}

// GSTR2BData is the "data" object.
type GSTR2BData struct {
	GSTIN   string        `json:"gstin"`
	RtnPrd  string        `json:"rtnprd"`
	DocData GSTR2BDocData `json:"docdata"`
}

// GSTR2BDocData is the "docdata" object: B2B invoices only.
type GSTR2BDocData struct {
	B2B []GSTR2BSupplier `json:"b2b"`
}

// GSTR2BSupplier is one supplier's invoices, keyed by its GSTIN (ctin).
type GSTR2BSupplier struct {
	CTIN  string          `json:"ctin"`
	TrdNm string          `json:"trdnm"`
	Inv   []GSTR2BInvoice `json:"inv"`
}

// GSTR2BInvoice is one invoice. Amounts are paise and marshal as rupee
// numbers with exactly two decimals. Date is YYYY-MM-DD and marshals as
// dt in dd-mm-yyyy. ExtID is the purchase event and is not marshalled.
type GSTR2BInvoice struct {
	Inum   string
	Date   string
	Val    money.Paise
	Txval  money.Paise
	IGST   money.Paise
	CGST   money.Paise
	SGST   money.Paise
	ItcAvl string
	ExtID  string
}

// gstr2bInvoiceJSON is the wire form of GSTR2BInvoice.
type gstr2bInvoiceJSON struct {
	Inum   string      `json:"inum"`
	Dt     string      `json:"dt"`
	Val    json.Number `json:"val"`
	Txval  json.Number `json:"txval"`
	IGST   json.Number `json:"igst"`
	CGST   json.Number `json:"cgst"`
	SGST   json.Number `json:"sgst"`
	ItcAvl string      `json:"itcavl"`
}

// MarshalJSON writes the invoice as the shared spec gives it.
func (inv GSTR2BInvoice) MarshalJSON() ([]byte, error) {
	d, err := time.Parse(dateLayout, inv.Date)
	if err != nil {
		return nil, fmt.Errorf("invoice %s: date %q is not YYYY-MM-DD", inv.Inum, inv.Date)
	}
	return json.Marshal(gstr2bInvoiceJSON{
		Inum:   inv.Inum,
		Dt:     ddmmyyyy(d),
		Val:    json.Number(inv.Val.Rupees()),
		Txval:  json.Number(inv.Txval.Rupees()),
		IGST:   json.Number(inv.IGST.Rupees()),
		CGST:   json.Number(inv.CGST.Rupees()),
		SGST:   json.Number(inv.SGST.Rupees()),
		ItcAvl: inv.ItcAvl,
	})
}

// BuildGSTR2B builds the company's GSTR-2B for period (YYYY-MM) from the
// true worlds. The period's own world must be among worlds. The return
// holds the purchases dated in the period from registered suppliers that
// are not filed late, plus the late ones dated in the previous month when
// that month's world is among worlds. Unregistered suppliers (no GSTIN) and
// sales never appear.
//
// Known simplification: the GST the bank charges on its own charges is
// eligible input tax and would appear in a real GSTR-2B under the bank's
// GSTIN, but the bank is not a profile supplier, so it is left out.
func BuildGSTR2B(p Profile, period string, worlds []World, opt GSTR2BOptions) (GSTR2B, error) {
	start, err := ParseMonth(period)
	if err != nil {
		return GSTR2B{}, fmt.Errorf("gstr2b: %w", err)
	}
	if opt.LateShareBP < 0 || opt.LateShareBP > 10000 {
		return GSTR2B{}, fmt.Errorf("gstr2b: late share %d bp is outside 0 to 10000", opt.LateShareBP)
	}
	gstin, err := p.GSTIN()
	if err != nil {
		return GSTR2B{}, fmt.Errorf("gstr2b %s: company GSTIN: %w", p.ID, err)
	}
	prev := start.AddDate(0, -1, 0).Format("2006-01")

	var cur, before *World
	seen := map[string]bool{}
	for i := range worlds {
		w := &worlds[i]
		if w.Company != p.ID {
			return GSTR2B{}, fmt.Errorf("gstr2b %s: world of company %q", p.ID, w.Company)
		}
		if seen[w.Month] {
			return GSTR2B{}, fmt.Errorf("gstr2b %s: two worlds for %s", p.ID, w.Month)
		}
		seen[w.Month] = true
		switch w.Month {
		case period:
			cur = w
		case prev:
			before = w
		}
	}
	if cur == nil {
		return GSTR2B{}, fmt.Errorf("gstr2b %s %s: the period's world is not among the %d worlds", p.ID, period, len(worlds))
	}

	g := GSTR2B{Data: GSTR2BData{GSTIN: gstin, RtnPrd: start.Format("012006")}}
	var invs []invEntry
	var errs []error
	collect := func(w *World, late bool) {
		for _, ev := range w.Events {
			if !inGSTR2B(ev, w.Month) {
				continue
			}
			if opt.IsLate(ev.ExtID) != late {
				if !late {
					g.Deferred = append(g.Deferred, ev.ExtID)
				}
				continue
			}
			if err := ValidGSTIN(ev.Meta.SupplierGSTIN); err != nil {
				errs = append(errs, fmt.Errorf("purchase %s: supplier GSTIN: %w", ev.ExtID, err))
				continue
			}
			if ev.InvoiceNo == "" {
				errs = append(errs, fmt.Errorf("purchase %s has no invoice number", ev.ExtID))
				continue
			}
			invs = append(invs, invEntry{ctin: ev.Meta.SupplierGSTIN, name: ev.Party, inv: GSTR2BInvoice{
				Inum:   ev.InvoiceNo,
				Date:   ev.Date,
				Val:    ev.Gross,
				Txval:  ev.Taxable,
				IGST:   ev.IGST,
				CGST:   ev.CGST,
				SGST:   ev.SGST,
				ItcAvl: "Y",
				ExtID:  ev.ExtID,
			}})
			g.Included = append(g.Included, ev.ExtID)
			if late {
				g.Late = append(g.Late, ev.ExtID)
			}
		}
	}
	if before != nil {
		collect(before, true)
	}
	collect(cur, false)
	if err := errors.Join(errs...); err != nil {
		return GSTR2B{}, fmt.Errorf("gstr2b %s %s: %w", p.ID, period, err)
	}

	g.Data.DocData.B2B = groupB2B(invs)
	slices.Sort(g.Included)
	slices.Sort(g.Late)
	slices.Sort(g.Deferred)
	return g, nil
}

// inGSTR2B reports whether an event of the month's world belongs in some
// GSTR-2B: a purchase from a registered supplier (one with a GSTIN), dated
// in that month.
func inGSTR2B(ev Event, month string) bool {
	return ev.Kind == EventPurchase && ev.Meta.SupplierGSTIN != "" &&
		len(ev.Date) >= len(month) && ev.Date[:len(month)] == month
}

// invEntry is an invoice before grouping by supplier.
type invEntry struct {
	ctin, name string
	inv        GSTR2BInvoice
}

// groupB2B groups invoices by ctin (sorted), each supplier's invoices
// sorted by (date, inum). The trade name is the supplier's name on its
// first invoice in that order. Never nil, so an empty return is "b2b": [].
func groupB2B(invs []invEntry) []GSTR2BSupplier {
	sort.SliceStable(invs, func(i, j int) bool {
		a, b := invs[i], invs[j]
		if a.ctin != b.ctin {
			return a.ctin < b.ctin
		}
		if a.inv.Date != b.inv.Date {
			return a.inv.Date < b.inv.Date
		}
		if a.inv.Inum != b.inv.Inum {
			return a.inv.Inum < b.inv.Inum
		}
		return a.inv.ExtID < b.inv.ExtID
	})
	out := []GSTR2BSupplier{}
	for _, e := range invs {
		if n := len(out); n == 0 || out[n-1].CTIN != e.ctin {
			out = append(out, GSTR2BSupplier{CTIN: e.ctin, TrdNm: e.name})
		}
		last := &out[len(out)-1]
		last.Inv = append(last.Inv, e.inv)
	}
	return out
}

// JSON returns the return as WriteGSTR2B writes it: 2-space indent and a
// trailing newline.
func (g GSTR2B) JSON() ([]byte, error) {
	if g.Data.DocData.B2B == nil {
		g.Data.DocData.B2B = []GSTR2BSupplier{}
	}
	b, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal gstr2b: %w", err)
	}
	return append(b, '\n'), nil
}

// WriteGSTR2B writes g to path as gstr2b.json, creating the parent
// directory, atomically (a temporary file, then a rename).
func WriteGSTR2B(path string, g GSTR2B) error {
	b, err := g.JSON()
	if err != nil {
		return fmt.Errorf("write gstr2b: %w", err)
	}
	if err := writeFileAtomic(path, b); err != nil {
		return fmt.Errorf("write gstr2b: %w", err)
	}
	return nil
}
