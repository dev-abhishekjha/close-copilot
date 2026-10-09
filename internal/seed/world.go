package seed

import (
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"slices"
	"sort"
	"time"

	"github.com/abhishekjha/close-copilot/internal/money"
)

// Event kinds (CC-302). The comment on each says which accounts it moves;
// CC-304 posts book-side events, CC-305 writes the bank-side ones.
const (
	// EventSale: Dr Debtors, Cr Sales and output GST. Party is a customer.
	EventSale = "sale"
	// EventReceipt: Dr bank, Cr Debtors. A customer paying a sale directly.
	EventReceipt = "receipt"
	// EventGatewayReceipt: Dr Payment Gateway Clearing, Cr Debtors. A
	// customer paying a sale through the payment gateway.
	EventGatewayReceipt = "gateway_receipt"
	// EventGatewaySettlement: the gateway paying a week's collections into
	// the bank, net of its fee and GST on the fee. Bank side only: the books
	// never record it, so it becomes an investigation case.
	EventGatewaySettlement = "gateway_settlement"
	// EventPurchase: Dr Account and input GST, Cr Creditors. Party is a
	// supplier.
	EventPurchase = "purchase"
	// EventVendorPayment: Dr Creditors, Cr bank, settling Refs[0].
	EventVendorPayment = "vendor_payment"
	// EventPayroll: Dr Salaries, Cr bank.
	EventPayroll = "payroll"
	// EventBankCharge: Dr Bank Charges and input GST, Cr bank.
	EventBankCharge = "bank_charge"
	// EventInterest: Dr bank, Cr Interest Income.
	EventInterest = "interest"
	// EventPrepaidAmortisation: Dr Account (the expense), Cr Prepaid
	// Expenses, one twelfth of an annual bill.
	EventPrepaidAmortisation = "prepaid_amortisation"
)

// EventKinds lists every event kind.
var EventKinds = []string{
	EventSale, EventReceipt, EventGatewayReceipt, EventGatewaySettlement,
	EventPurchase, EventVendorPayment, EventPayroll, EventBankCharge,
	EventInterest, EventPrepaidAmortisation,
}

// Party types.
const (
	PartySupplier = "Supplier"
	PartyCustomer = "Customer"
)

// Account names without the company abbreviation. CC-303 creates them and
// CC-304 resolves each as "<name> - <abbr>". The bank account is the
// profile's bank.account and is not listed here.
const (
	AccountSales                  = "Sales"
	AccountDebtors                = "Debtors"
	AccountCreditors              = "Creditors"
	AccountPaymentGatewayClearing = "Payment Gateway Clearing"
	AccountGatewayFees            = "Gateway Fees"
	AccountBankCharges            = "Bank Charges"
	AccountRent                   = "Rent"
	AccountElectricity            = "Electricity"
	AccountTelephoneInternet      = "Telephone and Internet"
	AccountSoftwareSubscriptions  = "Software Subscriptions"
	AccountPrepaidExpenses        = "Prepaid Expenses"
	AccountRepairsMaintenance     = "Repairs and Maintenance"
	AccountOfficeEquipment        = "Office Equipment"
	AccountSalaries               = "Salaries"
	AccountInterestIncome         = "Interest Income"
	AccountCostOfGoodsSold        = "Cost of Goods Sold"
	AccountProfessionalCharges    = "Professional Charges"
)

// Accounts is the closed list of non-bank accounts the generator and the
// planter use. Sales, Debtors and Creditors already exist in ERPNext.
var Accounts = []string{
	AccountSales,
	AccountDebtors,
	AccountCreditors,
	AccountPaymentGatewayClearing,
	AccountGatewayFees,
	AccountBankCharges,
	AccountRent,
	AccountElectricity,
	AccountTelephoneInternet,
	AccountSoftwareSubscriptions,
	AccountPrepaidExpenses,
	AccountRepairsMaintenance,
	AccountOfficeEquipment,
	AccountSalaries,
	AccountInterestIncome,
	AccountCostOfGoodsSold,
	AccountProfessionalCharges,
}

// Event is one business event that really happened. Amounts are paise.
// Date is YYYY-MM-DD.
type Event struct {
	ExtID     string      `json:"ext_id"`
	Kind      string      `json:"kind"`
	Date      string      `json:"date"`
	Party     string      `json:"party,omitempty"`
	PartyType string      `json:"party_type,omitempty"`
	Account   string      `json:"account"`
	Taxable   money.Paise `json:"taxable,omitempty"`
	IGST      money.Paise `json:"igst,omitempty"`
	CGST      money.Paise `json:"cgst,omitempty"`
	SGST      money.Paise `json:"sgst,omitempty"`
	Gross     money.Paise `json:"gross"`
	InvoiceNo string      `json:"invoice_no,omitempty"`
	// Refs are the ExtIDs this event settles: a receipt's sale, a vendor
	// payment's purchase, a settlement's gateway receipts, an amortisation's
	// annual purchase (when that purchase is in the same world).
	Refs      []string `json:"refs,omitempty"`
	BankRef   string   `json:"bank_ref,omitempty"`
	Narration string   `json:"narration"`
	Meta      Meta     `json:"meta"`
}

// Meta carries the detail a document's description needs.
type Meta struct {
	Description string `json:"description,omitempty"`
	// ServiceFrom and ServiceTo (YYYY-MM-DD) bound the period a bill or an
	// amortisation covers.
	ServiceFrom string `json:"service_from,omitempty"`
	ServiceTo   string `json:"service_to,omitempty"`
	// GSTRate is the GST percentage; absent means no GST.
	GSTRate       int    `json:"gst_rate,omitempty"`
	SupplierID    string `json:"supplier_id,omitempty"`
	SupplierGSTIN string `json:"supplier_gstin,omitempty"`
	// Fee and FeeGST are a gateway settlement's deductions: Gross is the
	// collections minus both.
	Fee    money.Paise `json:"fee,omitempty"`
	FeeGST money.Paise `json:"fee_gst,omitempty"`
}

// World is every event of one company-month, sorted by (Date, ExtID).
type World struct {
	Company     string      `json:"company"`
	Month       string      `json:"month"`
	OpeningBank money.Paise `json:"opening_bank"`
	Events      []Event     `json:"events"`
	ClosingBank money.Paise `json:"closing_bank"`
}

// Options tune the generator.
type Options struct {
	// Small scales invoice counts down for fast runs: sales to about a
	// tenth (at least 10), and one bill per supplier.
	Small bool
}

// JSON returns the world as cmd/seed world prints it: indented JSON with a
// trailing newline.
func (w World) JSON() ([]byte, error) {
	b, err := json.MarshalIndent(w, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal world: %w", err)
	}
	return append(b, '\n'), nil
}

// BankDelta returns the event's effect on the bank balance: positive for a
// deposit, negative for a withdrawal, zero for a book-only event.
func (e Event) BankDelta() money.Paise {
	switch e.Kind {
	case EventReceipt, EventGatewaySettlement, EventInterest:
		return e.Gross
	case EventVendorPayment, EventPayroll, EventBankCharge:
		return -e.Gross
	}
	return 0
}

const dateLayout = "2006-01-02"

// maxEvents bounds a month so the 4-digit ExtID sequence never overflows.
const maxEvents = 9999

// Generate builds the true world of one company-month: a pure function of
// the profile, the month and the options. It does no I/O, and its only
// randomness is a PCG seeded from the profile seed and the company-month.
//
// Phase 1 generates each month on its own: the bank opens at the profile's
// opening balance, and receipts and payments for sales and bills raised
// before the month are not emitted. A withdrawal that would take the
// balance below zero is deferred past the month end (left out).
func Generate(p Profile, month string, opt Options) (World, error) {
	start, err := ParseMonth(month)
	if err != nil {
		return World{}, fmt.Errorf("generate: %w", err)
	}
	if err := checkGeneratable(p); err != nil {
		return World{}, fmt.Errorf("generate %s: %w", p.ID, err)
	}
	g := &gen{
		p:     p,
		month: month,
		start: start,
		end:   start.AddDate(0, 1, -1),
		opt:   opt,
		rng:   newRNG(p, month),
		used:  map[string]bool{},
	}
	g.annual = drawAnnual(g.rng, p)
	g.prefixes = invoicePrefixes(p)
	g.counters = map[string]int{}

	g.sales()
	g.purchases()
	g.assetPurchase()
	g.payroll()
	g.bankCharge()
	g.interest()
	g.settlements()
	g.amortisation()

	return g.world()
}

func checkGeneratable(p Profile) error {
	var errs []error
	if p.ID == "" {
		errs = append(errs, errors.New("profile has no id"))
	}
	if p.Customers <= 0 {
		errs = append(errs, errors.New("customers must be positive"))
	}
	if len(p.Sales.GSTRates) == 0 {
		errs = append(errs, errors.New("sales.gst_rates is empty"))
	}
	if p.Bank.Account == "" {
		errs = append(errs, errors.New("bank.account is empty"))
	}
	return errors.Join(errs...)
}

// newRNG is the generator's only source of randomness for one month.
func newRNG(p Profile, month string) *rand.Rand {
	return rand.New(rand.NewPCG(uint64(p.Seed), monthHash(p.ID, month))) //nolint:gosec // G404: deterministic synthetic data, not security
}

// monthHash is FNV-1a 64 over company + "|" + month.
func monthHash(company, month string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(company + "|" + month))
	return h.Sum64()
}

// draft is an event before sorting: refs point at other drafts by key, and
// ExtIDs are assigned once the order is final.
type draft struct {
	ev   Event
	date time.Time
	key  int
	refs []int
}

type gen struct {
	p          Profile
	month      string
	start, end time.Time
	opt        Options
	rng        *rand.Rand
	drafts     []*draft
	used       map[string]bool // bank references already issued
	annual     []money.Paise   // annual suppliers' amounts, in profile order
	prefixes   map[string]string
	counters   map[string]int // next invoice sequence per supplier
	err        error
}

func (g *gen) add(ev Event, date time.Time, refs ...*draft) *draft {
	d := &draft{ev: ev, date: date, key: len(g.drafts)}
	for _, r := range refs {
		d.refs = append(d.refs, r.key)
	}
	g.drafts = append(g.drafts, d)
	return d
}

func (g *gen) inMonth(t time.Time) bool { return !t.Before(g.start) && !t.After(g.end) }

// day returns the n-th day of the month (1-based).
func (g *gen) day(n int) time.Time { return g.start.AddDate(0, 0, n-1) }

// randomDay returns a uniformly drawn day of the month.
func (g *gen) randomDay() time.Time { return g.day(1 + g.rng.IntN(g.end.Day())) }

// dayOf resolves a profile Day in this month.
func (g *gen) dayOf(d Day) time.Time {
	if d.Last {
		return g.end
	}
	return g.day(d.N)
}

// between draws an int in [lo, hi].
func (g *gen) between(lo, hi int) int { return lo + g.rng.IntN(hi-lo+1) }

// drawPaise draws an amount in paise uniformly from a range of whole rupees.
func drawPaise(r *rand.Rand, rg Range) money.Paise {
	lo, hi := rg.Min*100, rg.Max*100
	if hi <= lo {
		return money.Paise(lo)
	}
	return money.Paise(lo + r.Int64N(hi-lo+1))
}

// bankRef draws a reference not yet issued in this world: prefix and a
// zero-padded number of the given width.
func (g *gen) bankRef(prefix string, digits int) string {
	limit := 1
	for range digits {
		limit *= 10
	}
	for {
		ref := fmt.Sprintf("%s%0*d", prefix, digits, g.rng.IntN(limit))
		if !g.used[ref] {
			g.used[ref] = true
			return ref
		}
	}
}

// kindRank orders events within a day: book-only events first, then
// deposits, then withdrawals, so a day's money arrives before it leaves.
func kindRank(kind string) int {
	switch kind {
	case EventReceipt, EventGatewaySettlement, EventInterest:
		return 1
	case EventVendorPayment, EventPayroll, EventBankCharge:
		return 2
	}
	return 0
}

// world sorts the drafts, defers withdrawals the balance can't cover,
// assigns ExtIDs and resolves Refs.
func (g *gen) world() (World, error) {
	if g.err != nil {
		return World{}, fmt.Errorf("generate %s %s: %w", g.p.ID, g.month, g.err)
	}
	ds := g.drafts
	sort.SliceStable(ds, func(i, j int) bool {
		a, b := ds[i], ds[j]
		if !a.date.Equal(b.date) {
			return a.date.Before(b.date)
		}
		if ra, rb := kindRank(a.ev.Kind), kindRank(b.ev.Kind); ra != rb {
			return ra < rb
		}
		return a.key < b.key
	})

	opening := money.Paise(g.p.OpeningBankBalanceINR * 100)
	balance := opening
	kept := ds[:0:0]
	for _, d := range ds {
		delta := d.ev.BankDelta()
		if delta < 0 && balance+delta < 0 {
			continue // deferred past the month end
		}
		balance += delta
		kept = append(kept, d)
	}
	if len(kept) > maxEvents {
		return World{}, fmt.Errorf("generate %s %s: %d events is more than %d", g.p.ID, g.month, len(kept), maxEvents)
	}

	ext := make(map[int]string, len(kept))
	for i, d := range kept {
		ext[d.key] = fmt.Sprintf("EVT-%s-%s-%04d", g.p.ID, g.month, i+1)
	}
	events := make([]Event, len(kept))
	for i, d := range kept {
		ev := d.ev
		ev.ExtID = ext[d.key]
		ev.Date = d.date.Format(dateLayout)
		ev.Refs = nil
		for _, k := range d.refs {
			if id, ok := ext[k]; ok {
				ev.Refs = append(ev.Refs, id)
			}
		}
		slices.Sort(ev.Refs)
		events[i] = ev
	}
	return World{
		Company:     g.p.ID,
		Month:       g.month,
		OpeningBank: opening,
		Events:      events,
		ClosingBank: balance,
	}, nil
}

// monthName is "September 2026".
func monthName(t time.Time) string { return t.Format("January 2006") }

// ddmmyyyy is the Indian date format used in descriptions.
func ddmmyyyy(t time.Time) string { return t.Format("02-01-2006") }

// activeIn reports whether a supplier is active in a YYYY-MM month.
func activeIn(s Supplier, month string) bool {
	return (s.StartMonth == "" || month >= s.StartMonth) && (s.EndMonth == "" || month <= s.EndMonth)
}
