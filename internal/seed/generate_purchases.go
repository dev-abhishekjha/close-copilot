package seed

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"time"

	"github.com/abhishekjha/close-copilot/internal/money"
)

// The quarterly laptop's taxable amount, in whole rupees: always above the
// ₹50,000 capitalisation threshold.
const (
	assetMinINR = 55000
	assetMaxINR = 95000
	assetRate   = 18
)

// maxBillsPerMonth is how many bills a goods or services supplier without
// a fixed day raises in a month (1 to this; 1 when Small).
const maxBillsPerMonth = 3

// expenseAccount is the account a supplier's bills are booked to. Utilities
// whose name mentions power or electricity go to Electricity; every other
// utility (telephone, broadband, mobile) goes to Telephone and Internet.
func expenseAccount(s Supplier) string {
	switch s.Kind {
	case KindRent:
		return AccountRent
	case KindUtility:
		name := strings.ToLower(s.Name + " " + s.ID)
		if strings.Contains(name, "power") || strings.Contains(name, "electric") {
			return AccountElectricity
		}
		return AccountTelephoneInternet
	case KindSubscription:
		return AccountSoftwareSubscriptions
	case KindGoods:
		return AccountCostOfGoodsSold
	default:
		return AccountProfessionalCharges
	}
}

// drawAnnual draws every annual supplier's amount, in profile order. It is
// always the first use of a month's RNG, so a later month can recompute a
// renewal's amount from the profile alone.
func drawAnnual(r *rand.Rand, p Profile) []money.Paise {
	var out []money.Paise
	for _, s := range p.Suppliers {
		if s.IsAnnual() {
			out = append(out, drawPaise(r, deref(s.AmountINR)))
		}
	}
	return out
}

// purchases emits every active supplier's bills for the month and the
// payments that fall inside it. A supplier with a day bills once on that
// day. A goods or services supplier without one raises 1-3 bills on drawn
// days that share a monthly total drawn from monthly_inr. An annual
// supplier bills only in its renew_month, to Prepaid Expenses.
func (g *gen) purchases() {
	annual := 0
	for _, s := range g.p.Suppliers {
		if s.IsAnnual() {
			amount := g.annual[annual]
			annual++
			if activeIn(s, g.month) && int(g.start.Month()) == s.RenewMonth {
				g.annualBill(s, amount)
			}
			continue
		}
		if !activeIn(s, g.month) {
			continue
		}
		account := expenseAccount(s)
		from, to := g.start, g.end
		if s.Day.IsSet() {
			g.bill(s, g.dayOf(s.Day), drawPaise(g.rng, deref(s.MonthlyINR)), account, g.monthlyDescription(s), from, to)
			continue
		}
		n := 1
		if !g.opt.Small && (s.Kind == KindGoods || s.Kind == KindServices) {
			n = g.between(1, maxBillsPerMonth)
		}
		if s.Kind == KindGoods {
			from, to = time.Time{}, time.Time{} // goods have no service period
		}
		total := drawPaise(g.rng, deref(s.MonthlyINR))
		parts := g.split(total, n)
		days := make([]time.Time, len(parts))
		for i := range days {
			days[i] = g.randomDay()
		}
		slices.SortFunc(days, time.Time.Compare) // invoice numbers rise with the date
		for i, amount := range parts {
			g.bill(s, days[i], amount, account, g.monthlyDescription(s), from, to)
		}
	}
}

// split divides total into n positive parts with drawn weights; the
// rounding remainder goes to the last part.
func (g *gen) split(total money.Paise, n int) []money.Paise {
	if n <= 1 {
		return []money.Paise{total}
	}
	weights := make([]int64, n)
	var sum int64
	for i := range weights {
		weights[i] = int64(g.between(50, 150))
		sum += weights[i]
	}
	parts := make([]money.Paise, n)
	var used money.Paise
	for i := range n - 1 {
		parts[i] = money.Paise(int64(total) * weights[i] / sum)
		used += parts[i]
	}
	parts[n-1] = total - used
	return parts
}

func (g *gen) monthlyDescription(s Supplier) string {
	period := servicePeriod(g.start, g.end)
	switch s.Kind {
	case KindRent:
		return fmt.Sprintf("Rent for %s. %s", monthName(g.start), period)
	case KindUtility:
		return fmt.Sprintf("%s charges for %s. %s", s.Name, monthName(g.start), period)
	case KindSubscription:
		return fmt.Sprintf("Monthly subscription for %s. %s", monthName(g.start), period)
	case KindGoods:
		return "Goods for resale"
	default:
		return fmt.Sprintf("Services for %s. %s", monthName(g.start), period)
	}
}

func servicePeriod(from, to time.Time) string {
	return fmt.Sprintf("Service period: %s to %s", ddmmyyyy(from), ddmmyyyy(to))
}

// annualBill books a 12-month renewal to Prepaid Expenses, with the
// service period from the first day of the renewal month.
func (g *gen) annualBill(s Supplier, amount money.Paise) {
	date := g.randomDay()
	if s.Day.IsSet() {
		date = g.dayOf(s.Day)
	}
	from := g.start
	to := from.AddDate(1, 0, -1)
	desc := fmt.Sprintf("Annual %s renewal, 12 months. %s", kindNoun(s.Kind), servicePeriod(from, to))
	g.bill(s, date, amount, AccountPrepaidExpenses, desc, from, to)
}

func kindNoun(kind string) string {
	if kind == KindSubscription {
		return "subscription"
	}
	return kind
}

// assetPurchase books one laptop in each quarter's first month (January,
// April, July, October) to Office Equipment, from the first goods supplier
// charging 18% GST (or else the first goods supplier).
func (g *gen) assetPurchase() {
	if (int(g.start.Month())-1)%3 != 0 {
		return
	}
	var pick *Supplier
	for i := range g.p.Suppliers {
		s := &g.p.Suppliers[i]
		if s.Kind != KindGoods || s.IsAnnual() || !activeIn(*s, g.month) {
			continue
		}
		if s.GSTRate != nil && *s.GSTRate == assetRate {
			pick = s
			break
		}
		if pick == nil {
			pick = s
		}
	}
	if pick == nil {
		return
	}
	taxable := money.Paise(int64(g.between(assetMinINR, assetMaxINR)) * 100)
	g.bill(*pick, g.randomDay(), taxable, AccountOfficeEquipment,
		"Laptop, 14-inch business notebook for office use (capital asset)", time.Time{}, time.Time{})
}

// bill emits a purchase and, if it falls inside the month, its payment
// 5-20 days later.
func (g *gen) bill(s Supplier, date time.Time, taxable money.Paise, account, desc string, from, to time.Time) {
	rate := 0
	if s.GSTRate != nil {
		rate = *s.GSTRate
	}
	gstin, err := g.p.SupplierGSTIN(s)
	if err != nil && !errors.Is(err, ErrUnregistered) {
		g.fail(fmt.Errorf("supplier %s: %w", s.ID, err))
	}
	t := computeGST(taxable, rate, g.p.InState(s), s.IsRegistered())
	if t.total() == 0 {
		rate = 0
	}
	inv := g.invoiceNo(s)
	meta := Meta{
		Description:   desc,
		GSTRate:       rate,
		SupplierID:    s.ID,
		SupplierGSTIN: gstin,
	}
	if !from.IsZero() {
		meta.ServiceFrom, meta.ServiceTo = from.Format(dateLayout), to.Format(dateLayout)
	}
	gross := taxable + t.total()
	purchase := g.add(Event{
		Kind:      EventPurchase,
		Party:     s.Name,
		PartyType: PartySupplier,
		Account:   account,
		Taxable:   taxable,
		IGST:      t.IGST,
		CGST:      t.CGST,
		SGST:      t.SGST,
		Gross:     gross,
		InvoiceNo: inv,
		Narration: fmt.Sprintf("Bill %s from %s", inv, s.Name),
		Meta:      meta,
	}, date)

	paid := date.AddDate(0, 0, g.between(5, 20))
	if !g.inMonth(paid) {
		return
	}
	g.add(Event{
		Kind:      EventVendorPayment,
		Party:     s.Name,
		PartyType: PartySupplier,
		Account:   g.p.Bank.Account,
		Gross:     gross,
		InvoiceNo: inv,
		BankRef:   g.bankRef("N", 7),
		Narration: fmt.Sprintf("Payment for %s to %s", inv, s.Name),
		Meta:      Meta{Description: "NEFT to supplier", SupplierID: s.ID},
	}, paid, purchase)
}

// invoiceNo returns the supplier's next invoice number, such as
// CLD/2026/0912: a prefix from the supplier id, the year, the month and a
// per-month sequence that starts at a drawn number. Unique per supplier.
func (g *gen) invoiceNo(s Supplier) string {
	n, ok := g.counters[s.ID]
	if !ok {
		n = 1 + g.rng.IntN(60)
	}
	g.counters[s.ID] = n + 1
	return fmt.Sprintf("%s/%04d/%02d%02d", g.prefixes[s.ID], g.start.Year(), int(g.start.Month()), n)
}

// invoicePrefixes gives each supplier a unique 3-letter prefix: the first
// letter of its id, then the next consonants (cloudly -> CLD). A clash
// takes a digit suffix.
func invoicePrefixes(p Profile) map[string]string {
	out := make(map[string]string, len(p.Suppliers))
	taken := map[string]bool{}
	for _, s := range p.Suppliers {
		base := prefixOf(s.ID)
		pfx := base
		for i := 2; taken[pfx]; i++ {
			pfx = fmt.Sprintf("%s%d", base, i)
		}
		taken[pfx] = true
		out[s.ID] = pfx
	}
	return out
}

func prefixOf(id string) string {
	var letters []byte
	for i := range len(id) {
		if c := id[i]; c >= 'a' && c <= 'z' {
			letters = append(letters, c-'a'+'A')
		}
	}
	if len(letters) == 0 {
		return "SUP"
	}
	out := []byte{letters[0]}
	var rest []byte
	for _, c := range letters[1:] {
		if len(out) == 3 {
			break
		}
		if strings.IndexByte("AEIOU", c) < 0 {
			out = append(out, c)
		} else {
			rest = append(rest, c)
		}
	}
	for _, c := range rest {
		if len(out) == 3 {
			break
		}
		out = append(out, c)
	}
	for len(out) < 3 {
		out = append(out, 'X')
	}
	return string(out)
}

// amortisation moves one twelfth of each annual bill from Prepaid Expenses
// to the supplier's expense account, on the last day of every month of the
// service period. Phase 1 takes the most recent renewal on or before the
// month from the profile and recomputes its amount from that month's RNG;
// the rounding remainder goes in the twelfth month. Refs points at the
// annual purchase only when it is in this world (the renewal month).
func (g *gen) amortisation() {
	annual := 0
	for _, s := range g.p.Suppliers {
		if !s.IsAnnual() {
			continue
		}
		idx := annual
		annual++
		year := g.start.Year()
		if int(g.start.Month()) < s.RenewMonth {
			year--
		}
		renewal := time.Date(year, time.Month(s.RenewMonth), 1, 0, 0, 0, 0, time.UTC)
		renewalMonth := renewal.Format("2006-01")
		if !activeIn(s, renewalMonth) {
			continue
		}
		k := (g.start.Year()-renewal.Year())*12 + int(g.start.Month()) - int(renewal.Month())
		amount := g.annual[idx]
		if renewalMonth != g.month {
			amount = drawAnnual(newRNG(g.p, renewalMonth), g.p)[idx]
		}
		share := money.Paise(roundDiv(int64(amount), 12))
		if k == 11 {
			share = amount - 11*share
		}
		to := renewal.AddDate(1, 0, -1)
		var refs []*draft
		for _, d := range g.drafts {
			if d.ev.Kind == EventPurchase && d.ev.Meta.SupplierID == s.ID && d.ev.Account == AccountPrepaidExpenses {
				refs = append(refs, d)
			}
		}
		g.add(Event{
			Kind:      EventPrepaidAmortisation,
			Account:   expenseAccount(s),
			Gross:     share,
			Narration: fmt.Sprintf("Amortisation of %s annual %s, month %d of 12", s.Name, kindNoun(s.Kind), k+1),
			Meta: Meta{
				Description: fmt.Sprintf("One twelfth of the annual %s from Prepaid Expenses. %s",
					kindNoun(s.Kind), servicePeriod(renewal, to)),
				ServiceFrom: renewal.Format(dateLayout),
				ServiceTo:   to.Format(dateLayout),
				SupplierID:  s.ID,
			},
		}, g.end, refs...)
	}
}
