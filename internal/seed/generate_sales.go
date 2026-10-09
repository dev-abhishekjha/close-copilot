package seed

import (
	"fmt"
	"slices"
	"time"

	"github.com/abhishekjha/close-copilot/internal/money"
)

// Gateway fee GST is 18% of the fee.
const gatewayFeeGSTRate = 18

// minSmallSales is the floor on a Small month's sales count.
const minSmallSales = 10

// sales emits the month's sales invoices and the receipts that fall inside
// the month. Every customer is in the company's own state, so sales carry
// CGST and SGST. A gateway_share of sales is paid through the payment
// gateway (gateway_receipt into Payment Gateway Clearing); the rest is paid
// directly to the bank (receipt). A sale is paid in full 0-15 days after it
// is raised; payments that fall after the month end are not emitted.
func (g *gen) sales() {
	s := g.p.Sales
	n := g.between(int(s.InvoicesPerMonth.Min), int(s.InvoicesPerMonth.Max))
	if g.opt.Small {
		n = max(minSmallSales, int(roundDiv(int64(n), 10)))
	}
	for range n {
		date := g.randomDay()
		cust := fmt.Sprintf("CUST-%03d", 1+g.rng.IntN(g.p.Customers))
		taxable := drawPaise(g.rng, s.AmountINR)
		rate := s.GSTRates[g.rng.IntN(len(s.GSTRates))]
		viaGateway := BasisPoints(g.rng.IntN(10000)) < s.GatewayShare.BP
		lag := g.rng.IntN(16)

		t := computeGST(taxable, rate, true, true)
		gross := taxable + t.total()
		sale := g.add(Event{
			Kind:      EventSale,
			Party:     cust,
			PartyType: PartyCustomer,
			Account:   AccountSales,
			Taxable:   taxable,
			IGST:      t.IGST,
			CGST:      t.CGST,
			SGST:      t.SGST,
			Gross:     gross,
			Narration: "Sale to " + cust,
			Meta: Meta{
				Description: fmt.Sprintf("Goods sold to %s at %d%% GST", cust, rate),
				GSTRate:     rate,
			},
		}, date)

		paid := date.AddDate(0, 0, lag)
		if !g.inMonth(paid) {
			continue
		}
		if viaGateway {
			g.add(Event{
				Kind:      EventGatewayReceipt,
				Party:     cust,
				PartyType: PartyCustomer,
				Account:   AccountPaymentGatewayClearing,
				Gross:     gross,
				Narration: "Gateway collection from " + cust,
				Meta:      Meta{Description: "Collected through the payment gateway"},
			}, paid, sale)
			continue
		}
		g.add(Event{
			Kind:      EventReceipt,
			Party:     cust,
			PartyType: PartyCustomer,
			Account:   g.p.Bank.Account,
			Gross:     gross,
			BankRef:   g.bankRef("UTR", 9),
			Narration: "Receipt from " + cust,
			Meta:      Meta{Description: "Paid directly to the bank"},
		}, paid, sale)
	}
}

// settlements emits the gateway's weekly payouts. Collections from Monday
// to Sunday settle into the bank on the following Tuesday, net of the
// gateway fee (gateway_fee_pct of the collections) and 18% GST on the fee.
// Only settlements dated inside the month are emitted; later ones stay in
// Payment Gateway Clearing.
func (g *gen) settlements() {
	type batch struct {
		week     time.Time
		receipts []*draft
		total    money.Paise
	}
	var batches []*batch          // first-seen order, then sorted by week
	byWeek := map[string]*batch{} // lookup only; never ranged over
	for _, d := range g.drafts {
		if d.ev.Kind != EventGatewayReceipt {
			continue
		}
		week := d.date.AddDate(0, 0, -((int(d.date.Weekday()) + 6) % 7))
		b, ok := byWeek[week.Format(dateLayout)]
		if !ok {
			b = &batch{week: week}
			byWeek[week.Format(dateLayout)] = b
			batches = append(batches, b)
		}
		b.receipts = append(b.receipts, d)
		b.total += d.ev.Gross
	}
	slices.SortStableFunc(batches, func(a, b *batch) int { return a.week.Compare(b.week) })

	feeBP := int64(g.p.Sales.GatewayFeePct.BP)
	for _, b := range batches {
		date := b.week.AddDate(0, 0, 8)
		if !g.inMonth(date) {
			continue
		}
		fee := money.Paise(roundDiv(int64(b.total)*feeBP, 10000))
		feeGST := money.Paise(roundDiv(int64(fee)*gatewayFeeGSTRate, 100))
		net := b.total - fee - feeGST
		ref := g.bankRef("PGS", 4)
		last := b.week.AddDate(0, 0, 6)
		g.add(Event{
			Kind:      EventGatewaySettlement,
			Account:   g.p.Bank.Account,
			Gross:     net,
			BankRef:   ref,
			Narration: fmt.Sprintf("PG SETTL %s BATCH %s", date.Format("0102"), ref[3:]),
			Meta: Meta{
				Description: fmt.Sprintf("Gateway settlement of collections %s to %s: %s collected, %s fee, %s GST on the fee",
					ddmmyyyy(b.week), ddmmyyyy(last), b.total.Format(), fee.Format(), feeGST.Format()),
				Fee:    fee,
				FeeGST: feeGST,
			},
		}, date, b.receipts...)
	}
}
