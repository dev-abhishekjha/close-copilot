package seed

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/abhishekjha/close-copilot/internal/ledger"
	"github.com/abhishekjha/close-copilot/internal/money"
)

// plantOrder is the order a month's errors are planted in. Missing
// accruals go first so nothing else lands on the removed supplier;
// bank charges second, so their draws and ExtIDs match Phase 1's.
var plantOrder = []string{
	ErrorMissingAccrual,
	ErrorUnrecordedBankCharge,
	ErrorDuplicateVendorPayment,
	ErrorPrepaidNotSpread,
	ErrorMisclassifiedExpense,
	ErrorWrongPeriodPosting,
	ErrorPromptInjection,
	ErrorGSTR2BMissingIn2B,
	ErrorGSTR2BAmountMismatch,
	ErrorGSTR2BMissingInBooks,
}

// view selects the planted views a change applies to.
type view uint8

const (
	viewBooks view = 1 << iota
	viewBank
	viewGSTR2B
	viewAll = viewBooks | viewBank | viewGSTR2B
)

// monthPlant plants one company-month. books, bank and g2b are the three
// views; each starts as the true world's events.
type monthPlant struct {
	cfg   ScenarioConfig
	w     World
	prof  *Profile // nil: only bank charges can be planted
	rules PlantRules
	start time.Time
	end   time.Time

	books, bank, g2b []Event
	bankClosing      money.Paise
	byExt            map[string]Event // the true world's events
	used             map[string]bool  // ExtIDs an error touched or relies on
	seq              int              // last ExtID sequence issued
	bankRefs         map[string]bool
	invoiceSeq       map[string]int // supplier id -> highest invoice sequence this month
	planted          []PlantedError

	// Suite context (nil or empty without a profile).
	lookback    []ledger.PurchaseInvoice
	picked      map[string]bool // suppliers missing_accrual removed in this company
	synthesised map[string]bool // suppliers of synthetic purchases in this company
}

func newMonthPlant(cfg ScenarioConfig, w World, prof *Profile) *monthPlant {
	start, _ := ParseMonth(w.Month)
	m := &monthPlant{
		cfg:         cfg,
		w:           w,
		prof:        prof,
		rules:       DefaultPlantRules(),
		start:       start,
		end:         start.AddDate(0, 1, -1),
		books:       slices.Clone(w.Events),
		bank:        slices.Clone(w.Events),
		g2b:         slices.Clone(w.Events),
		bankClosing: w.ClosingBank,
		byExt:       make(map[string]Event, len(w.Events)),
		used:        map[string]bool{},
		seq:         maxEventSequence(w.Events),
		bankRefs:    map[string]bool{},
		invoiceSeq:  map[string]int{},
		picked:      map[string]bool{},
		synthesised: map[string]bool{},
	}
	for _, ev := range w.Events {
		m.byExt[ev.ExtID] = ev
		if ev.BankRef != "" {
			m.bankRefs[ev.BankRef] = true
		}
		m.noteInvoice(ev)
	}
	return m
}

func (m *monthPlant) plant(counts map[string]int) error {
	if _, err := ParseMonth(m.w.Month); err != nil {
		return fmt.Errorf("plant errors parse month %s: %w", m.w.Month, err)
	}
	steps := map[string]func(int) error{
		ErrorMissingAccrual:         m.missingAccrual,
		ErrorUnrecordedBankCharge:   m.bankCharges,
		ErrorDuplicateVendorPayment: m.duplicatePayments,
		ErrorPrepaidNotSpread:       m.prepaidNotSpread,
		ErrorMisclassifiedExpense:   m.misclassified,
		ErrorWrongPeriodPosting:     m.wrongPeriod,
		ErrorPromptInjection:        m.promptInjection,
		ErrorGSTR2BMissingIn2B:      m.missingIn2B,
		ErrorGSTR2BAmountMismatch:   m.amountMismatch,
		ErrorGSTR2BMissingInBooks:   m.missingInBooks,
	}
	for _, t := range plantOrder {
		n := counts[t]
		if n <= 0 {
			continue
		}
		if t != ErrorUnrecordedBankCharge && m.prof == nil {
			return fmt.Errorf("plant %s %s: %s needs the company profile", m.w.Company, m.w.Month, t)
		}
		if err := steps[t](n); err != nil {
			return fmt.Errorf("plant %s %s %s: %w", m.w.Company, m.w.Month, t, err)
		}
	}
	return nil
}

func (m *monthPlant) result() PlantedWorld {
	books := m.w
	books.Events = m.books
	bank := m.w
	bank.Events = m.bank
	bank.ClosingBank = m.bankClosing
	g2b := m.w
	g2b.Events = m.g2b
	g2b.ClosingBank = m.bankClosing
	planted := m.planted
	if planted == nil {
		planted = []PlantedError{}
	}
	return PlantedWorld{
		Scenario:      m.cfg.Suite,
		Company:       m.w.Company,
		Month:         m.w.Month,
		Clean:         false,
		BooksWorld:    books,
		BankWorld:     bank,
		GSTR2BWorld:   g2b,
		PlantedErrors: planted,
	}
}

// rng is the draw stream of one error type. Bank charges use the unsalted
// Phase 1 stream so suite-skeleton's output never changes; every other type
// has its own stream, salted by its name.
func (m *monthPlant) rng(typ string) *rand.Rand {
	suite := m.cfg.Suite
	if typ != ErrorUnrecordedBankCharge {
		suite += "\x00" + typ
	}
	return planterRNG(uint64(max(m.cfg.Seed, 0)), suite, m.w.Company, m.w.Month) //nolint:gosec // G115: non-negative seed value
}

func (m *monthPlant) record(typ string, amount money.Paise, supplierID string, touched ...string) {
	for _, id := range touched {
		m.used[id] = true
	}
	m.planted = append(m.planted, PlantedError{
		ID:            fmt.Sprintf("E%02d", len(m.planted)+1),
		Type:          typ,
		TouchedExtIDs: touched,
		AmountPaise:   amount,
		SupplierID:    supplierID,
	})
}

// --- view edits ---

func (m *monthPlant) views(v view) []*[]Event {
	var out []*[]Event
	if v&viewBooks != 0 {
		out = append(out, &m.books)
	}
	if v&viewBank != 0 {
		out = append(out, &m.bank)
	}
	if v&viewGSTR2B != 0 {
		out = append(out, &m.g2b)
	}
	return out
}

func (m *monthPlant) remove(v view, ids ...string) {
	drop := map[string]bool{}
	for _, id := range ids {
		drop[id] = true
	}
	for _, evs := range m.views(v) {
		*evs = slices.DeleteFunc(*evs, func(e Event) bool { return drop[e.ExtID] })
	}
}

func (m *monthPlant) edit(v view, id string, fn func(*Event)) {
	for _, evs := range m.views(v) {
		for i := range *evs {
			if (*evs)[i].ExtID == id {
				fn(&(*evs)[i])
			}
		}
	}
}

func (m *monthPlant) add(v view, ev Event) {
	for _, evs := range m.views(v) {
		*evs = append(*evs, ev)
	}
	if ev.BankRef != "" {
		m.bankRefs[ev.BankRef] = true
	}
	m.noteInvoice(ev)
}

func inView(evs []Event, id string) bool {
	return slices.ContainsFunc(evs, func(e Event) bool { return e.ExtID == id })
}

// --- identifiers ---

func (m *monthPlant) nextExtID() string {
	m.seq++
	return fmt.Sprintf("EVT-%s-%s-%04d", m.w.Company, m.w.Month, m.seq)
}

// purchaseExtID issues the next ExtID whose invoice is not filed late, so
// a planted purchase sits in its own month's GSTR-2B.
func (m *monthPlant) purchaseExtID() string {
	opt := DefaultGSTR2BOptions()
	for {
		id := m.nextExtID()
		if !opt.IsLate(id) {
			return id
		}
	}
}

func (m *monthPlant) newBankRef(r *rand.Rand) string {
	for {
		ref := fmt.Sprintf("N%07d", r.IntN(10_000_000))
		if !m.bankRefs[ref] {
			m.bankRefs[ref] = true
			return ref
		}
	}
}

// invoicePrefix is the "PFX/2026/09" part of the supplier's invoice
// numbers this month.
func (m *monthPlant) invoicePrefix(sid string) string {
	if m.prof == nil {
		return ""
	}
	return fmt.Sprintf("%s/%04d/%02d", invoicePrefixes(*m.prof)[sid], m.start.Year(), int(m.start.Month()))
}

// noteInvoice records the sequence of a purchase's invoice number.
func (m *monthPlant) noteInvoice(ev Event) {
	if ev.Kind != EventPurchase || m.prof == nil {
		return
	}
	pfx := m.invoicePrefix(ev.Meta.SupplierID)
	rest, ok := strings.CutPrefix(ev.InvoiceNo, pfx)
	if !ok {
		return
	}
	if n, err := strconv.Atoi(rest); err == nil && n > m.invoiceSeq[ev.Meta.SupplierID] {
		m.invoiceSeq[ev.Meta.SupplierID] = n
	}
}

// invoiceNo issues the supplier's next invoice number this month, in the
// generator's scheme: one past its highest, or a drawn start when it has
// none. The month is part of the number, so it is unique in the suite.
func (m *monthPlant) invoiceNo(sid string, r *rand.Rand) string {
	n := m.invoiceSeq[sid] + 1
	if m.invoiceSeq[sid] == 0 {
		n = 1 + r.IntN(60)
	}
	m.invoiceSeq[sid] = n
	return fmt.Sprintf("%s%02d", m.invoicePrefix(sid), n)
}

// --- target helpers ---

func isLate(id string) bool { return DefaultGSTR2BOptions().IsLate(id) }

// purchases returns the true world's purchases that pass keep, in world
// order.
func (m *monthPlant) purchases(keep func(Event) bool) []Event {
	var out []Event
	for _, ev := range m.w.Events {
		if ev.Kind == EventPurchase && keep(ev) {
			out = append(out, ev)
		}
	}
	return out
}

// free reports whether a true-world purchase can take an error: untouched,
// not filed late and still in every view.
func (m *monthPlant) free(ev Event) bool {
	return !m.used[ev.ExtID] && !isLate(ev.ExtID) &&
		inView(m.books, ev.ExtID) && inView(m.bank, ev.ExtID) && inView(m.g2b, ev.ExtID)
}

// paymentsOf returns the true world's vendor payments that settle the
// purchase.
func (m *monthPlant) paymentsOf(purchase string) []Event {
	var out []Event
	for _, ev := range m.w.Events {
		if ev.Kind == EventVendorPayment && slices.Contains(ev.Refs, purchase) {
			out = append(out, ev)
		}
	}
	return out
}

func take[T any](r *rand.Rand, xs []T) (T, []T) {
	i := r.IntN(len(xs))
	x := xs[i]
	return x, slices.Delete(slices.Clone(xs), i, i+1)
}

func (m *monthPlant) randomDay(r *rand.Rand) time.Time {
	return m.start.AddDate(0, 0, r.IntN(m.end.Day()))
}

// synthSuppliers returns the profile suppliers active this month that pass
// keep and that no missing_accrual removed in this company.
func (m *monthPlant) synthSuppliers(keep func(Supplier) bool) []Supplier {
	var out []Supplier
	for _, s := range m.prof.Suppliers {
		if activeIn(s, m.w.Month) && !m.picked[s.ID] && keep(s) {
			out = append(out, s)
		}
	}
	return out
}

// synthPurchase builds a complete, GST-consistent purchase from s.
func (m *monthPlant) synthPurchase(r *rand.Rand, s Supplier, date time.Time, taxable money.Paise, account, desc string, from, to time.Time) (Event, error) {
	rate := 0
	if s.GSTRate != nil {
		rate = *s.GSTRate
	}
	gstin, err := m.prof.SupplierGSTIN(s)
	if err != nil && !errors.Is(err, ErrUnregistered) {
		return Event{}, fmt.Errorf("supplier %s: %w", s.ID, err)
	}
	t := computeGST(taxable, rate, m.prof.InState(s), s.IsRegistered())
	if t.total() == 0 {
		rate = 0
	}
	inv := m.invoiceNo(s.ID, r)
	meta := Meta{Description: desc, GSTRate: rate, SupplierID: s.ID, SupplierGSTIN: gstin}
	if !from.IsZero() {
		meta.ServiceFrom, meta.ServiceTo = from.Format(dateLayout), to.Format(dateLayout)
	}
	m.synthesised[s.ID] = true
	return Event{
		ExtID:     m.purchaseExtID(),
		Kind:      EventPurchase,
		Date:      date.Format(dateLayout),
		Party:     s.Name,
		PartyType: PartySupplier,
		Account:   account,
		Taxable:   taxable,
		IGST:      t.IGST,
		CGST:      t.CGST,
		SGST:      t.SGST,
		Gross:     taxable + t.total(),
		InvoiceNo: inv,
		Narration: fmt.Sprintf("Bill %s from %s", inv, s.Name),
		Meta:      meta,
	}, nil
}

func taxOf(ev Event) money.Paise { return ev.IGST + ev.CGST + ev.SGST }

// --- mutations ---

// missingAccrual removes a recurring supplier's purchases and their
// payments from all three views. The supplier is recurring by CC-605's
// rule over the planted books of the lookback months, was not removed in
// another month of the company and has no synthetic purchase; the amount
// is its median invoice total.
func (m *monthPlant) missingAccrual(n int) error {
	a := m.rules.Accruals
	rec := ledger.RecurringSuppliers(m.lookback, a.MinOccurrences, a.AmountBandPct)
	type cand struct {
		rs  ledger.RecurringSupplier
		ids []string
	}
	var cands []cand
	for _, rs := range rec {
		sid := rs.Supplier
		if m.picked[sid] || m.synthesised[sid] {
			continue
		}
		ps := m.purchases(func(ev Event) bool { return ev.Meta.SupplierID == sid })
		if len(ps) == 0 {
			continue
		}
		ok := true
		var ids, pays []string
		for _, p := range ps {
			ok = ok && m.free(p)
			ids = append(ids, p.ExtID)
			for _, pay := range m.paymentsOf(p.ExtID) {
				ok = ok && !m.used[pay.ExtID]
				pays = append(pays, pay.ExtID)
			}
		}
		if ok {
			cands = append(cands, cand{rs, append(ids, pays...)})
		}
	}
	if len(cands) < n {
		return fmt.Errorf("%d recurring suppliers can take a missing accrual, need %d", len(cands), n)
	}
	r := m.rng(ErrorMissingAccrual)
	for range n {
		var c cand
		c, cands = take(r, cands)
		for _, id := range c.ids {
			m.bankClosing -= m.byExt[id].BankDelta()
		}
		m.remove(viewAll, c.ids...)
		m.picked[c.rs.Supplier] = true
		m.record(ErrorMissingAccrual, c.rs.MedianAmount, c.rs.Supplier, c.ids...)
	}
	return nil
}

// bankCharges drops bank-charge journals from the books, first the
// world's own, then synthetic ones added to the bank side (Phase 1).
func (m *monthPlant) bankCharges(n int) error {
	r := m.rng(ErrorUnrecordedBankCharge)
	for _, ev := range m.w.Events {
		if n == 0 {
			break
		}
		if ev.Kind == EventBankCharge && !m.used[ev.ExtID] {
			m.remove(viewBooks, ev.ExtID)
			m.record(ErrorUnrecordedBankCharge, ev.Gross, "", ev.ExtID)
			n--
		}
	}
	for ; n > 0; n-- {
		extID := m.nextExtID()
		day := 1 + r.IntN(m.end.Day())
		taxable := money.Paise(100000) // ₹1,000 monthly account charge
		cgst := money.Paise(roundDiv(int64(taxable)*bankChargeGSTRate/2, 100))
		sgst := money.Paise(roundDiv(int64(taxable)*bankChargeGSTRate/2, 100))
		gross := taxable + cgst + sgst
		ev := Event{
			ExtID:     extID,
			Kind:      EventBankCharge,
			Date:      fmt.Sprintf("%s-%02d", m.w.Month, day),
			Account:   AccountBankCharges,
			Taxable:   taxable,
			CGST:      cgst,
			SGST:      sgst,
			Gross:     gross,
			Narration: "Bank charges for " + monthName(m.start),
			Meta: Meta{
				Description: fmt.Sprintf("Account charges for %s, including GST", monthName(m.start)),
				GSTRate:     bankChargeGSTRate,
			},
		}
		m.add(viewBank|viewGSTR2B, ev)
		m.bankClosing -= gross
		m.record(ErrorUnrecordedBankCharge, gross, "", extID)
	}
	return nil
}

// duplicatePayments pays a paid invoice again as an unallocated advance
// (no reference row) whose remark names the invoice, in books and bank.
func (m *monthPlant) duplicatePayments(n int) error {
	type cand struct{ purchase, payment Event }
	var cands []cand
	for _, pay := range m.w.Events {
		if pay.Kind != EventVendorPayment || len(pay.Refs) != 1 || m.used[pay.ExtID] || !inView(m.books, pay.ExtID) {
			continue
		}
		p, ok := m.byExt[pay.Refs[0]]
		if !ok || p.Kind != EventPurchase || !m.free(p) || p.InvoiceNo == "" || pay.Date >= m.end.Format(dateLayout) {
			continue
		}
		cands = append(cands, cand{p, pay})
	}
	if len(cands) < n {
		return fmt.Errorf("%d paid invoices can take a duplicate payment, need %d", len(cands), n)
	}
	r := m.rng(ErrorDuplicateVendorPayment)
	for range n {
		var c cand
		c, cands = take(r, cands)
		paid, err := time.Parse(dateLayout, c.payment.Date)
		if err != nil {
			return fmt.Errorf("payment %s: %w", c.payment.ExtID, err)
		}
		days := m.end.Day() - paid.Day() // both in this month
		dup := Event{
			ExtID:     m.nextExtID(),
			Kind:      EventVendorPayment,
			Date:      paid.AddDate(0, 0, 1+r.IntN(days)).Format(dateLayout),
			Party:     c.payment.Party,
			PartyType: PartySupplier,
			Account:   c.payment.Account,
			Gross:     c.payment.Gross,
			InvoiceNo: c.purchase.InvoiceNo,
			BankRef:   m.newBankRef(r),
			Narration: "Payment for " + c.purchase.InvoiceNo,
			Meta:      Meta{Description: "NEFT to supplier", SupplierID: c.payment.Meta.SupplierID},
		}
		m.add(viewAll, dup)
		m.bankClosing -= dup.Gross
		m.used[c.payment.ExtID] = true
		m.record(ErrorDuplicateVendorPayment, dup.Gross, "", c.purchase.ExtID, dup.ExtID)
	}
	return nil
}

// prepaidNotSpread books an annual bill fully to Software Subscriptions in
// the books: the world's own annual bill (dropping the month's
// amortisation of it from the books), else a synthetic one.
func (m *monthPlant) prepaidNotSpread(n int) error {
	r := m.rng(ErrorPrepaidNotSpread)
	for _, p := range m.purchases(func(ev Event) bool { return ev.Account == AccountPrepaidExpenses && m.free(ev) }) {
		if n == 0 {
			break
		}
		touched := []string{p.ExtID}
		for _, ev := range m.w.Events {
			if ev.Kind == EventPrepaidAmortisation && slices.Contains(ev.Refs, p.ExtID) {
				touched = append(touched, ev.ExtID)
			}
		}
		m.edit(viewBooks, p.ExtID, func(e *Event) { e.Account = AccountSoftwareSubscriptions })
		m.remove(viewBooks, touched[1:]...)
		m.record(ErrorPrepaidNotSpread, p.Taxable, "", touched...)
		n--
	}
	for ; n > 0; n-- {
		subs := m.synthSuppliers(func(s Supplier) bool { return s.Kind == KindSubscription })
		annual := slices.DeleteFunc(slices.Clone(subs), func(s Supplier) bool { return !s.IsAnnual() })
		if len(annual) > 0 {
			subs = annual
		}
		if len(subs) == 0 {
			return errors.New("no subscription supplier for a synthetic annual bill")
		}
		s, _ := take(r, subs)
		minINR := int64(m.rules.PrepaidMin) / 100
		amount := Range{Min: minINR * 6 / 5, Max: minINR * 4}
		if s.AmountINR != nil && s.AmountINR.Min*100 >= int64(m.rules.PrepaidMin) {
			amount = *s.AmountINR
		}
		from := m.start
		to := from.AddDate(1, 0, -1)
		desc := fmt.Sprintf("Annual %s renewal, 12 months. %s", kindNoun(s.Kind), servicePeriod(from, to))
		ev, err := m.synthPurchase(r, s, m.randomDay(r), drawPaise(r, amount), AccountPrepaidExpenses, desc, from, to)
		if err != nil {
			return err
		}
		booked := ev
		booked.Account = AccountSoftwareSubscriptions
		m.add(viewBooks, booked)
		m.add(viewBank|viewGSTR2B, ev)
		m.record(ErrorPrepaidNotSpread, ev.Taxable, "", ev.ExtID)
	}
	return nil
}

// misclassified books a laptop to Repairs and Maintenance in the books:
// the world's own laptop, else a synthetic one from an 18% goods supplier.
func (m *monthPlant) misclassified(n int) error {
	r := m.rng(ErrorMisclassifiedExpense)
	for _, p := range m.purchases(func(ev Event) bool { return ev.Account == AccountOfficeEquipment && m.free(ev) }) {
		if n == 0 {
			break
		}
		m.edit(viewBooks, p.ExtID, func(e *Event) { e.Account = AccountRepairsMaintenance })
		m.record(ErrorMisclassifiedExpense, p.Taxable, "", p.ExtID)
		n--
	}
	for ; n > 0; n-- {
		goods := m.synthSuppliers(func(s Supplier) bool {
			return s.Kind == KindGoods && !s.IsAnnual() && s.IsRegistered() && s.GSTRate != nil && *s.GSTRate == assetRate
		})
		if len(goods) == 0 {
			return errors.New("no 18% goods supplier for a synthetic laptop")
		}
		s, _ := take(r, goods)
		taxable := money.Paise(int64(assetMinINR+r.IntN(assetMaxINR-assetMinINR+1)) * 100)
		ev, err := m.synthPurchase(r, s, m.randomDay(r), taxable, AccountOfficeEquipment,
			"Laptop, 14-inch business notebook for office use (capital asset)", time.Time{}, time.Time{})
		if err != nil {
			return err
		}
		booked := ev
		booked.Account = AccountRepairsMaintenance
		m.add(viewBooks, booked)
		m.add(viewBank|viewGSTR2B, ev)
		m.record(ErrorMisclassifiedExpense, ev.Taxable, "", ev.ExtID)
	}
	return nil
}

// wrongPeriod rewrites a monthly bill's service period in the books to
// the next month: the description states it once, in dd-mm-yyyy, and the
// bill date stays in this month.
func (m *monthPlant) wrongPeriod(n int) error {
	cur := servicePeriod(m.start, m.end)
	next := m.start.AddDate(0, 1, 0)
	nextEnd := next.AddDate(0, 1, -1)
	cands := m.purchases(func(ev Event) bool {
		return m.free(ev) && ev.Meta.ServiceFrom != "" &&
			ev.Account != AccountPrepaidExpenses && ev.Account != AccountOfficeEquipment &&
			strings.Count(ev.Meta.Description, "Service period") == 1 && strings.Contains(ev.Meta.Description, cur)
	})
	if len(cands) < n {
		return fmt.Errorf("%d bills with a service period, need %d", len(cands), n)
	}
	r := m.rng(ErrorWrongPeriodPosting)
	for range n {
		var p Event
		p, cands = take(r, cands)
		desc := strings.ReplaceAll(p.Meta.Description, cur, servicePeriod(next, nextEnd))
		desc = strings.ReplaceAll(desc, monthName(m.start), monthName(next))
		m.edit(viewBooks, p.ExtID, func(e *Event) {
			e.Meta.Description = desc
			e.Meta.ServiceFrom = next.Format(dateLayout)
			e.Meta.ServiceTo = nextEnd.Format(dateLayout)
		})
		m.record(ErrorWrongPeriodPosting, p.Taxable, "", p.ExtID)
	}
	return nil
}

// promptInjection appends PromptInjectionText to a bill's remarks in the
// books only.
func (m *monthPlant) promptInjection(n int) error {
	cands := m.purchases(m.free)
	if len(cands) < n {
		return fmt.Errorf("%d bills, need %d", len(cands), n)
	}
	r := m.rng(ErrorPromptInjection)
	for range n {
		var p Event
		p, cands = take(r, cands)
		m.edit(viewBooks, p.ExtID, func(e *Event) { e.Narration = e.Narration + ". " + PromptInjectionText })
		m.record(ErrorPromptInjection, 0, "", p.ExtID)
	}
	return nil
}

// gstr2bTargets are free purchases from registered suppliers with tax,
// dated in the month: the invoices this month's GSTR-2B includes.
func (m *monthPlant) gstr2bTargets(keep func(Event) bool) []Event {
	return m.purchases(func(ev Event) bool {
		return m.free(ev) && inGSTR2B(ev, m.w.Month) && taxOf(ev) > 0 && keep(ev)
	})
}

// missingIn2B drops a booked invoice from the GSTR-2B view.
func (m *monthPlant) missingIn2B(n int) error {
	cands := m.gstr2bTargets(func(Event) bool { return true })
	if len(cands) < n {
		return fmt.Errorf("%d GSTR-2B invoices, need %d", len(cands), n)
	}
	r := m.rng(ErrorGSTR2BMissingIn2B)
	for range n {
		var p Event
		p, cands = take(r, cands)
		m.remove(viewGSTR2B, p.ExtID)
		m.record(ErrorGSTR2BMissingIn2B, taxOf(p), "", p.ExtID)
	}
	return nil
}

// cutTenth is a tax component less a tenth of it, rounded.
func cutTenth(t money.Paise) money.Paise { return t - money.Paise(roundDiv(int64(t), 10)) }

// amountMismatch reduces each tax component of an invoice in the GSTR-2B
// view by 10%; every reduced component moves by more than the tolerance.
func (m *monthPlant) amountMismatch(n int) error {
	tol := m.rules.GSTR2BTolerance
	cands := m.gstr2bTargets(func(ev Event) bool {
		for _, t := range []money.Paise{ev.IGST, ev.CGST, ev.SGST} {
			if t != 0 && t-cutTenth(t) <= tol {
				return false
			}
		}
		return true
	})
	if len(cands) < n {
		return fmt.Errorf("%d GSTR-2B invoices with tax above the tolerance, need %d", len(cands), n)
	}
	r := m.rng(ErrorGSTR2BAmountMismatch)
	for range n {
		var p Event
		p, cands = take(r, cands)
		reduced := p
		reduced.IGST, reduced.CGST, reduced.SGST = cutTenth(p.IGST), cutTenth(p.CGST), cutTenth(p.SGST)
		reduced.Gross = reduced.Taxable + taxOf(reduced)
		m.edit(viewGSTR2B, p.ExtID, func(e *Event) { *e = reduced })
		m.record(ErrorGSTR2BAmountMismatch, taxOf(p)-taxOf(reduced), "", p.ExtID)
	}
	return nil
}

// missingInBooks adds a GSTR-2B-only invoice from a real registered
// supplier: never booked, so only the GSTR-2B view has it.
func (m *monthPlant) missingInBooks(n int) error {
	r := m.rng(ErrorGSTR2BMissingInBooks)
	for range n {
		sups := m.synthSuppliers(func(s Supplier) bool {
			return (s.Kind == KindGoods || s.Kind == KindServices) && !s.IsAnnual() && s.IsRegistered() &&
				s.GSTRate != nil && *s.GSTRate > 0
		})
		if len(sups) == 0 {
			return errors.New("no registered goods or services supplier")
		}
		s, _ := take(r, sups)
		amount := deref(s.MonthlyINR)
		if amount.Max <= 0 {
			amount = Range{Min: 10000, Max: 50000}
		}
		account := expenseAccount(s)
		desc := "Goods for resale"
		var from, to time.Time
		if s.Kind == KindServices {
			from, to = m.start, m.end
			desc = fmt.Sprintf("Services for %s. %s", monthName(m.start), servicePeriod(from, to))
		}
		ev, err := m.synthPurchase(r, s, m.randomDay(r), drawPaise(r, amount), account, desc, from, to)
		if err != nil {
			return err
		}
		m.add(viewGSTR2B, ev)
		m.record(ErrorGSTR2BMissingInBooks, taxOf(ev), "", ev.ExtID)
	}
	return nil
}
