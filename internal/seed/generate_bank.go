package seed

import (
	"fmt"
	"time"

	"github.com/abhishekjha/close-copilot/internal/money"
)

// Bank charges carry 18% GST, charged by the bank's branch in the company's
// own state (CGST and SGST).
const bankChargeGSTRate = 18

// Quarterly interest is 0.75% of the opening balance (3% a year), scaled by
// a drawn factor of 90-110%.
const (
	interestQuarterBP = 75
	interestJitterMin = 9000 // basis points of the base amount
	interestJitterMax = 11000
)

// payroll pays the month's salaries from the bank on the payroll day: the
// last working day (Monday to Friday) for day: last, otherwise that date
// or the working day before it.
func (g *gen) payroll() {
	date := g.dayOf(g.p.Payroll.Day)
	for date.Weekday() == time.Saturday || date.Weekday() == time.Sunday {
		date = date.AddDate(0, 0, -1)
	}
	amount := drawPaise(g.rng, g.p.Payroll.MonthlyINR)
	g.add(Event{
		Kind:      EventPayroll,
		Account:   AccountSalaries,
		Gross:     amount,
		BankRef:   g.bankRef("SAL", 6),
		Narration: "Salaries for " + monthName(g.start),
		Meta:      Meta{Description: "Net salaries paid by bank transfer for " + monthName(g.start)},
	}, date)
}

// bankCharge is the bank's monthly account charge plus GST, debited on a
// drawn day.
func (g *gen) bankCharge() {
	date := g.randomDay()
	taxable := drawPaise(g.rng, g.p.Bank.MonthlyChargesINR)
	t := computeGST(taxable, bankChargeGSTRate, true, true)
	g.add(Event{
		Kind:      EventBankCharge,
		Account:   AccountBankCharges,
		Taxable:   taxable,
		CGST:      t.CGST,
		SGST:      t.SGST,
		Gross:     taxable + t.total(),
		Narration: "Bank charges for " + monthName(g.start),
		Meta: Meta{
			Description: fmt.Sprintf("%s account charges for %s, including GST", g.p.Bank.Name, monthName(g.start)),
			GSTRate:     bankChargeGSTRate,
		},
	}, date)
}

// interest credits the quarter's interest on the last day of the quarter's
// last month (March, June, September, December).
func (g *gen) interest() {
	if int(g.start.Month())%3 != 0 {
		return
	}
	factor := int64(g.between(interestJitterMin, interestJitterMax))
	amount := money.Paise(roundDiv(g.p.OpeningBankBalanceINR*100*interestQuarterBP*factor, 10000*10000))
	if amount <= 0 {
		return
	}
	g.add(Event{
		Kind:      EventInterest,
		Account:   AccountInterestIncome,
		Gross:     amount,
		Narration: "Interest credit for the quarter ending " + monthName(g.start),
		Meta:      Meta{Description: "Quarterly interest on the current account balance"},
	}, g.end)
}

// fail records the first generation error; world returns it.
func (g *gen) fail(err error) {
	if g.err == nil {
		g.err = err
	}
}
