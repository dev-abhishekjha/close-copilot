package seed

import (
	"bytes"
	"flag"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/abhishekjha/close-copilot/internal/money"
)

var update = flag.Bool("update", false, "rewrite testdata golden files")

const goldenWorld = "testdata/world-sharma-2026-09.json"

func loadCompany(t *testing.T, id string) Profile {
	t.Helper()
	p, err := LoadProfile(filepath.Join("..", "..", "config", "companies", id+".yaml"))
	if err != nil {
		t.Fatalf("load %s: %v", id, err)
	}
	return p
}

func mustGenerate(t *testing.T, p Profile, month string, opt Options) World {
	t.Helper()
	w, err := Generate(p, month, opt)
	if err != nil {
		t.Fatalf("Generate(%s, %s, %+v): %v", p.ID, month, opt, err)
	}
	return w
}

func TestWorldGolden(t *testing.T) {
	w := mustGenerate(t, loadCompany(t, "sharma"), "2026-09", Options{})
	got, err := w.JSON()
	if err != nil {
		t.Fatal(err)
	}
	if *update {
		if err := os.MkdirAll(filepath.Dir(goldenWorld), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenWorld, got, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(goldenWorld)
	if err != nil {
		t.Fatalf("read golden (run with -update to create it): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("world differs from %s; run go test ./internal/seed -run TestWorldGolden -update and review the diff", goldenWorld)
	}
}

func TestWorldDeterministic(t *testing.T) {
	p := loadCompany(t, "sharma")
	for _, opt := range []Options{{}, {Small: true}} {
		a := mustGenerate(t, p, "2026-09", opt)
		b := mustGenerate(t, p, "2026-09", opt)
		if !reflect.DeepEqual(a, b) {
			t.Errorf("%+v: two calls gave different worlds", opt)
		}
		c := mustGenerate(t, p, "2026-08", opt)
		if reflect.DeepEqual(a.Events, c.Events) {
			t.Errorf("%+v: 2026-08 and 2026-09 gave the same events", opt)
		}
	}
	other := p
	other.Seed++
	if reflect.DeepEqual(mustGenerate(t, p, "2026-09", Options{}).Events, mustGenerate(t, other, "2026-09", Options{}).Events) {
		t.Error("a different seed gave the same events")
	}
}

func TestGenerateRejectsBadMonth(t *testing.T) {
	p := loadCompany(t, "sharma")
	for _, m := range []string{"", "2026-9", "2026-13", "09-2026", "2026-09-01"} {
		if _, err := Generate(p, m, Options{}); err == nil {
			t.Errorf("Generate(%q): want an error", m)
		}
	}
}

var invariantMonths = []string{
	"2026-04", "2026-05", "2026-06", "2026-07", "2026-08",
	"2026-09", "2026-10", "2026-11", "2026-12",
}

func TestWorldInvariants(t *testing.T) {
	for _, id := range []string{"sharma", "mehta"} {
		p := loadCompany(t, id)
		for _, month := range invariantMonths {
			for _, opt := range []Options{{}, {Small: true}} {
				t.Run(fmt.Sprintf("%s/%s/small=%v", id, month, opt.Small), func(t *testing.T) {
					checkInvariants(t, p, month, mustGenerate(t, p, month, opt))
				})
			}
		}
	}
}

// TestWorldInvariantsLowBalance starts the month with an empty bank, so
// withdrawals must be deferred to keep the balance from going negative.
func TestWorldInvariantsLowBalance(t *testing.T) {
	p := loadCompany(t, "sharma")
	p.OpeningBankBalanceINR = 0
	full := mustGenerate(t, loadCompany(t, "sharma"), "2026-09", Options{})
	for _, month := range invariantMonths {
		w := mustGenerate(t, p, month, Options{})
		checkInvariants(t, p, month, w)
		if month == "2026-09" && len(w.Events) >= len(full.Events) {
			t.Errorf("an empty opening balance deferred nothing (%d events, %d with the full balance)", len(w.Events), len(full.Events))
		}
	}
}

func checkInvariants(t *testing.T, p Profile, month string, w World) {
	t.Helper()
	start, err := ParseMonth(month)
	if err != nil {
		t.Fatal(err)
	}
	end := start.AddDate(0, 1, -1)
	if w.Company != p.ID || w.Month != month {
		t.Errorf("world is %s %s, want %s %s", w.Company, w.Month, p.ID, month)
	}
	if w.OpeningBank != money.Paise(p.OpeningBankBalanceINR*100) {
		t.Errorf("opening bank %d, want %d", w.OpeningBank, p.OpeningBankBalanceINR*100)
	}
	if len(w.Events) == 0 {
		t.Fatal("no events")
	}

	accounts := map[string]bool{p.Bank.Account: true}
	for _, a := range Accounts {
		accounts[a] = true
	}
	byID := map[string]Event{}
	balance := w.OpeningBank
	for i, e := range w.Events {
		want := fmt.Sprintf("EVT-%s-%s-%04d", p.ID, month, i+1)
		if e.ExtID != want {
			t.Errorf("event %d: ExtID %s, want %s", i, e.ExtID, want)
		}
		if _, dup := byID[e.ExtID]; dup {
			t.Errorf("duplicate ExtID %s", e.ExtID)
		}
		byID[e.ExtID] = e
		if i > 0 {
			prev := w.Events[i-1]
			if e.Date < prev.Date || (e.Date == prev.Date && e.ExtID <= prev.ExtID) {
				t.Errorf("%s: not sorted by (date, ext_id) after %s", e.ExtID, prev.ExtID)
			}
		}
		if !slices.Contains(EventKinds, e.Kind) {
			t.Errorf("%s: unknown kind %q", e.ExtID, e.Kind)
		}
		d, err := time.Parse(dateLayout, e.Date)
		if err != nil || d.Before(start) || d.After(end) {
			t.Errorf("%s: date %q is not inside %s", e.ExtID, e.Date, month)
		}
		if !accounts[e.Account] {
			t.Errorf("%s: account %q is neither in Accounts nor the bank", e.ExtID, e.Account)
		}
		if e.Gross <= 0 {
			t.Errorf("%s: gross %d is not positive", e.ExtID, e.Gross)
		}
		if e.Taxable != 0 || e.IGST != 0 || e.CGST != 0 || e.SGST != 0 {
			if sum := e.Taxable + e.IGST + e.CGST + e.SGST; e.Gross != sum {
				t.Errorf("%s: gross %d != taxable + taxes %d", e.ExtID, e.Gross, sum)
			}
		}
		if e.IGST != 0 && (e.CGST != 0 || e.SGST != 0) {
			t.Errorf("%s: both IGST and CGST/SGST set", e.ExtID)
		}
		if e.CGST != e.SGST {
			t.Errorf("%s: CGST %d != SGST %d", e.ExtID, e.CGST, e.SGST)
		}
		for _, r := range e.Refs {
			if !strings.HasPrefix(r, "EVT-"+p.ID+"-"+month+"-") {
				t.Errorf("%s: ref %s is not in this world", e.ExtID, r)
			}
		}
		balance += e.BankDelta()
		if balance < 0 {
			t.Errorf("%s: bank balance %d is negative", e.ExtID, balance)
		}
	}
	if balance != w.ClosingBank {
		t.Errorf("closing bank %d, want %d from the events", w.ClosingBank, balance)
	}

	// References resolve to earlier events of the right kind, and payments
	// never exceed what they settle.
	settles := map[string]string{
		EventReceipt:             EventSale,
		EventGatewayReceipt:      EventSale,
		EventVendorPayment:       EventPurchase,
		EventGatewaySettlement:   EventGatewayReceipt,
		EventPrepaidAmortisation: EventPurchase,
	}
	paid := map[string]money.Paise{}
	for _, e := range w.Events {
		wantKind, ok := settles[e.Kind]
		if !ok {
			if len(e.Refs) > 0 {
				t.Errorf("%s (%s): unexpected refs %v", e.ExtID, e.Kind, e.Refs)
			}
			continue
		}
		var refTotal money.Paise
		for _, r := range e.Refs {
			ref, ok := byID[r]
			switch {
			case !ok:
				t.Errorf("%s: ref %s is not in the world", e.ExtID, r)
			case ref.Kind != wantKind:
				t.Errorf("%s (%s): ref %s is a %s, want a %s", e.ExtID, e.Kind, r, ref.Kind, wantKind)
			case ref.ExtID >= e.ExtID:
				t.Errorf("%s: ref %s is not earlier", e.ExtID, r)
			}
			refTotal += ref.Gross
		}
		switch e.Kind {
		case EventReceipt, EventGatewayReceipt, EventVendorPayment:
			if len(e.Refs) != 1 {
				t.Fatalf("%s (%s): want one ref, got %v", e.ExtID, e.Kind, e.Refs)
			}
			paid[e.Refs[0]] += e.Gross
			if paid[e.Refs[0]] > refTotal {
				t.Errorf("%s: payments %d exceed invoice %s gross %d", e.ExtID, paid[e.Refs[0]], e.Refs[0], refTotal)
			}
			if e.Kind == EventVendorPayment && e.InvoiceNo != byID[e.Refs[0]].InvoiceNo {
				t.Errorf("%s: invoice no %q, want %q", e.ExtID, e.InvoiceNo, byID[e.Refs[0]].InvoiceNo)
			}
		case EventGatewaySettlement:
			if len(e.Refs) == 0 {
				t.Errorf("%s: settlement settles nothing", e.ExtID)
			}
			if e.Gross+e.Meta.Fee+e.Meta.FeeGST != refTotal {
				t.Errorf("%s: net %d + fee %d + fee GST %d != collections %d", e.ExtID, e.Gross, e.Meta.Fee, e.Meta.FeeGST, refTotal)
			}
			if e.BankRef == "" {
				t.Errorf("%s: settlement has no bank ref", e.ExtID)
			}
		}
	}

	// Each settled receipt is settled once; bank refs and supplier invoice
	// numbers are unique.
	settled := map[string]bool{}
	bankRefs := map[string]bool{}
	invoices := map[string]bool{}
	for _, e := range w.Events {
		if e.Kind == EventGatewaySettlement {
			for _, r := range e.Refs {
				if settled[r] {
					t.Errorf("%s: receipt %s settled twice", e.ExtID, r)
				}
				settled[r] = true
			}
		}
		if e.BankRef != "" {
			if bankRefs[e.BankRef] {
				t.Errorf("%s: bank ref %s used twice", e.ExtID, e.BankRef)
			}
			bankRefs[e.BankRef] = true
		}
		if e.BankDelta() != 0 && e.BankRef == "" && e.Kind != EventBankCharge && e.Kind != EventInterest {
			t.Errorf("%s (%s): bank-side event without a bank ref", e.ExtID, e.Kind)
		}
		if e.Kind == EventPurchase {
			if e.InvoiceNo == "" || e.PartyType != PartySupplier || e.Meta.SupplierID == "" {
				t.Errorf("%s: purchase without invoice no, supplier or supplier id", e.ExtID)
			}
			key := e.Meta.SupplierID + "|" + e.InvoiceNo
			if invoices[key] {
				t.Errorf("%s: invoice %s repeated for %s", e.ExtID, e.InvoiceNo, e.Meta.SupplierID)
			}
			invoices[key] = true
		}
	}
}

func TestWorldInvoiceNumbersAcrossMonths(t *testing.T) {
	p := loadCompany(t, "sharma")
	seen := map[string]string{}
	for _, month := range invariantMonths {
		for _, e := range mustGenerate(t, p, month, Options{}).Events {
			if e.Kind != EventPurchase {
				continue
			}
			key := e.Meta.SupplierID + "|" + e.InvoiceNo
			if prev, dup := seen[key]; dup {
				t.Errorf("%s: invoice %s already used by %s", e.ExtID, e.InvoiceNo, prev)
			}
			seen[key] = e.ExtID
		}
	}
}

func TestGST(t *testing.T) {
	cases := []struct {
		name             string
		taxable          money.Paise
		rate             int
		inState, reg     bool
		igst, cgst, sgst money.Paise
	}{
		{"in-state 18%", 10000000, 18, true, true, 0, 900000, 900000},
		{"out-of-state 18%", 10000000, 18, false, true, 1800000, 0, 0},
		// 1000.01 at 9% = 90.0009 -> 90.00; at 18% = 180.0018 -> 180.00.
		{"in-state 18% odd paisa", 100001, 18, true, true, 0, 9000, 9000},
		{"out-of-state 18% odd paisa", 100001, 18, false, true, 18000, 0, 0},
		// 0.03 at 9% = 0.0027 -> 0.00; at 18% = 0.0054 -> 0.01.
		{"in-state 18% tiny", 3, 18, true, true, 0, 0, 0},
		{"out-of-state 18% tiny", 3, 18, false, true, 1, 0, 0},
		// 1234.57 at 2.5% = 30.86425 -> 30.86; at 5% = 61.7285 -> 61.73.
		{"in-state 5% odd paisa", 123457, 5, true, true, 0, 3086, 3086},
		{"out-of-state 5% odd paisa", 123457, 5, false, true, 6173, 0, 0},
		// 0.10 at 2.5% = 0.0025 -> 0.00; 0.20 at 2.5% = 0.005 -> 0.01 (half away from zero).
		{"in-state 5% below half", 10, 5, true, true, 0, 0, 0},
		{"in-state 5% exact half", 20, 5, true, true, 0, 1, 1},
		// 0.50 at 9% = 0.045 -> 0.05 each; IGST 0.09.
		{"in-state 18% half up", 50, 18, true, true, 0, 5, 5},
		{"out-of-state 18% half", 50, 18, false, true, 9, 0, 0},
		{"unregistered", 2228946, 18, false, false, 0, 0, 0},
		{"unregistered in-state flag", 2228946, 18, true, false, 0, 0, 0},
		{"rate 0", 100000, 0, true, true, 0, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := computeGST(tc.taxable, tc.rate, tc.inState, tc.reg)
			want := tax{IGST: tc.igst, CGST: tc.cgst, SGST: tc.sgst}
			if got != want {
				t.Errorf("computeGST(%d, %d, %v, %v) = %+v, want %+v", tc.taxable, tc.rate, tc.inState, tc.reg, got, want)
			}
		})
	}
}

func TestRoundDiv(t *testing.T) {
	cases := []struct{ n, d, want int64 }{
		{0, 7, 0}, {5, 10, 1}, {4, 10, 0}, {15, 10, 2}, {-5, 10, -1}, {-4, 10, 0}, {-15, 10, -2},
		{100000100, 12, 8333342}, {7, 2, 4}, {-7, 2, -4},
	}
	for _, tc := range cases {
		if got := roundDiv(tc.n, tc.d); got != tc.want {
			t.Errorf("roundDiv(%d, %d) = %d, want %d", tc.n, tc.d, got, tc.want)
		}
	}
}

func eventsOf(w World, kind string) []Event {
	var out []Event
	for _, e := range w.Events {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func TestWorldCoverage(t *testing.T) {
	p := loadCompany(t, "sharma")

	t.Run("every kind in a quarter-end month", func(t *testing.T) {
		w := mustGenerate(t, p, "2026-09", Options{})
		for _, k := range EventKinds {
			if len(eventsOf(w, k)) == 0 {
				t.Errorf("2026-09 has no %s", k)
			}
		}
	})
	t.Run("every kind but interest in other months", func(t *testing.T) {
		for _, month := range []string{"2026-07", "2026-08", "2026-10", "2026-11"} {
			w := mustGenerate(t, p, month, Options{})
			for _, k := range EventKinds {
				n := len(eventsOf(w, k))
				switch {
				case k == EventInterest && n != 0:
					t.Errorf("%s: interest outside a quarter-end month", month)
				case k != EventInterest && n == 0:
					t.Errorf("%s has no %s", month, k)
				}
			}
		}
	})
	t.Run("interest in quarter-end months", func(t *testing.T) {
		for _, month := range []string{"2026-06", "2026-09", "2026-12"} {
			if n := len(eventsOf(mustGenerate(t, p, month, Options{}), EventInterest)); n != 1 {
				t.Errorf("%s: %d interest events, want 1", month, n)
			}
		}
	})
	t.Run("renewal month has the prepaid purchase and its amortisation", func(t *testing.T) {
		w := mustGenerate(t, p, "2026-09", Options{})
		var bill *Event
		for _, e := range eventsOf(w, EventPurchase) {
			if e.Account == AccountPrepaidExpenses {
				if bill != nil {
					t.Fatalf("two prepaid purchases: %s and %s", bill.ExtID, e.ExtID)
				}
				bill = &e
			}
		}
		if bill == nil {
			t.Fatal("no purchase booked to Prepaid Expenses")
		}
		if bill.Meta.SupplierID != "cloudly" || bill.Meta.ServiceFrom != "2026-09-01" || bill.Meta.ServiceTo != "2027-08-31" {
			t.Errorf("prepaid purchase %s: supplier %s, service %s to %s", bill.ExtID, bill.Meta.SupplierID, bill.Meta.ServiceFrom, bill.Meta.ServiceTo)
		}
		if bill.Taxable != 12000000 || bill.IGST != 2160000 || bill.Gross != 14160000 {
			t.Errorf("prepaid purchase amounts %d + %d = %d, want 120000.00 + 21600.00 IGST", bill.Taxable, bill.IGST, bill.Gross)
		}
		if !strings.Contains(strings.ToLower(bill.Meta.Description), "annual") {
			t.Errorf("description %q doesn't say annual", bill.Meta.Description)
		}
		am := eventsOf(w, EventPrepaidAmortisation)
		if len(am) != 1 || am[0].Gross != 1000000 || am[0].Account != AccountSoftwareSubscriptions || !slices.Equal(am[0].Refs, []string{bill.ExtID}) {
			t.Errorf("amortisation %+v, want one of 10000.00 to Software Subscriptions referencing %s", am, bill.ExtID)
		}
	})
	t.Run("quarter-start months have a laptop above the threshold", func(t *testing.T) {
		for _, month := range []string{"2026-04", "2026-07", "2026-10", "2027-01"} {
			for _, opt := range []Options{{}, {Small: true}} {
				var laptops []Event
				for _, e := range eventsOf(mustGenerate(t, p, month, opt), EventPurchase) {
					if e.Account == AccountOfficeEquipment {
						laptops = append(laptops, e)
					}
				}
				if len(laptops) != 1 {
					t.Fatalf("%s %+v: %d laptops, want 1", month, opt, len(laptops))
				}
				l := laptops[0]
				if l.Taxable <= 5000000 || !strings.Contains(strings.ToLower(l.Meta.Description), "laptop") {
					t.Errorf("%s: laptop %s taxable %d, description %q", month, l.ExtID, l.Taxable, l.Meta.Description)
				}
			}
		}
		for _, month := range []string{"2026-05", "2026-08", "2026-09", "2026-12"} {
			for _, e := range eventsOf(mustGenerate(t, p, month, Options{}), EventPurchase) {
				if e.Account == AccountOfficeEquipment {
					t.Errorf("%s: laptop %s outside a quarter's first month", month, e.ExtID)
				}
			}
		}
	})
}

func TestAmortisationSpreadsOverTwelveMonths(t *testing.T) {
	p := loadCompany(t, "sharma")
	// An amount that doesn't divide by 12: the remainder lands in month 12.
	for i := range p.Suppliers {
		if p.Suppliers[i].ID == "cloudly" {
			r := Between(100001, 100001)
			p.Suppliers[i].AmountINR = &r
		}
	}
	var total money.Paise
	var last Event
	months := []string{
		"2026-09", "2026-10", "2026-11", "2026-12", "2027-01", "2027-02",
		"2027-03", "2027-04", "2027-05", "2027-06", "2027-07", "2027-08",
	}
	for i, month := range months {
		am := eventsOf(mustGenerate(t, p, month, Options{}), EventPrepaidAmortisation)
		if len(am) != 1 {
			t.Fatalf("%s: %d amortisations, want 1", month, len(am))
		}
		if am[0].Meta.ServiceFrom != "2026-09-01" || !strings.Contains(am[0].Narration, fmt.Sprintf("month %d of 12", i+1)) {
			t.Errorf("%s: amortisation %q, service from %s", month, am[0].Narration, am[0].Meta.ServiceFrom)
		}
		if i < 11 && am[0].Gross != 833342 {
			t.Errorf("%s: amortisation %d, want 833342", month, am[0].Gross)
		}
		total += am[0].Gross
		last = am[0]
	}
	if total != 10000100 {
		t.Errorf("12 amortisations total %d, want the annual 10000100", total)
	}
	if last.Gross != 10000100-11*833342 {
		t.Errorf("last amortisation %d, want the remainder", last.Gross)
	}
	// The month before the renewal belongs to the previous year's renewal.
	am := eventsOf(mustGenerate(t, p, "2026-08", Options{}), EventPrepaidAmortisation)
	if len(am) != 1 || am[0].Meta.ServiceFrom != "2025-09-01" {
		t.Errorf("2026-08 amortisation %+v, want the 2025-09 renewal", am)
	}
}

func TestAmortisationSkipsInactiveRenewal(t *testing.T) {
	p := loadCompany(t, "sharma")
	for i := range p.Suppliers {
		if p.Suppliers[i].ID == "cloudly" {
			p.Suppliers[i].StartMonth = "2026-10"
		}
	}
	for _, month := range []string{"2026-09", "2026-10"} {
		w := mustGenerate(t, p, month, Options{})
		if n := len(eventsOf(w, EventPrepaidAmortisation)); n != 0 {
			t.Errorf("%s: %d amortisations for a renewal before the supplier started", month, n)
		}
		for _, e := range eventsOf(w, EventPurchase) {
			if e.Meta.SupplierID == "cloudly" {
				t.Errorf("%s: purchase %s from an inactive supplier", month, e.ExtID)
			}
		}
	}
}

func TestPayrollOnLastWorkingDay(t *testing.T) {
	p := loadCompany(t, "sharma")
	cases := map[string]string{
		"2026-09": "2026-09-30", // Wednesday
		"2026-10": "2026-10-30", // the 31st is a Saturday
		"2026-05": "2026-05-29", // the 31st is a Sunday
	}
	for _, month := range slices.Sorted(maps.Keys(cases)) {
		pay := eventsOf(mustGenerate(t, p, month, Options{}), EventPayroll)
		if len(pay) != 1 || pay[0].Date != cases[month] {
			t.Errorf("%s: payroll %+v, want one on %s", month, pay, cases[month])
		}
	}
	// A fixed day on a weekend moves to the Friday before.
	p.Payroll.Day = DayOfMonth(3) // 2026-10-03 is a Saturday
	pay := eventsOf(mustGenerate(t, p, "2026-10", Options{}), EventPayroll)
	if len(pay) != 1 || pay[0].Date != "2026-10-02" {
		t.Errorf("payroll %+v, want one on 2026-10-02", pay)
	}
}

func TestSmallScalesSalesDown(t *testing.T) {
	p := loadCompany(t, "sharma")
	for _, month := range invariantMonths {
		full := len(eventsOf(mustGenerate(t, p, month, Options{}), EventSale))
		small := len(eventsOf(mustGenerate(t, p, month, Options{Small: true}), EventSale))
		if full < int(p.Sales.InvoicesPerMonth.Min) || full > int(p.Sales.InvoicesPerMonth.Max) {
			t.Errorf("%s: %d sales, want %d-%d", month, full, p.Sales.InvoicesPerMonth.Min, p.Sales.InvoicesPerMonth.Max)
		}
		if small < minSmallSales || small > (full+9)/10+1 {
			t.Errorf("%s: small has %d sales, full %d", month, small, full)
		}
	}
}

func TestInvoicePrefixes(t *testing.T) {
	for id, want := range map[string]string{"cloudly": "CLD", "techkart": "TCH", "omkar-estates": "OMK", "aeiou": "AEI", "ab": "ABX", "123": "SUP"} {
		if got := prefixOf(id); got != want {
			t.Errorf("prefixOf(%q) = %q, want %q", id, got, want)
		}
	}
	p := Profile{Suppliers: []Supplier{{ID: "cloudly"}, {ID: "cldx"}, {ID: "cloud"}}}
	got := invoicePrefixes(p)
	want := map[string]string{"cloudly": "CLD", "cldx": "CLD2", "cloud": "CLD3"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("invoicePrefixes = %v, want %v", got, want)
	}
	seen := map[string]string{}
	for _, s := range loadCompany(t, "sharma").Suppliers {
		pfx := invoicePrefixes(loadCompany(t, "sharma"))[s.ID]
		if other, dup := seen[pfx]; dup {
			t.Errorf("%s and %s share prefix %s", s.ID, other, pfx)
		}
		seen[pfx] = s.ID
	}
}

func TestMonthHash(t *testing.T) {
	// FNV-1a 64 over company + "|" + month, written out by hand.
	fnv1a := func(s string) uint64 {
		h := uint64(0xcbf29ce484222325)
		for i := range len(s) {
			h ^= uint64(s[i])
			h *= 0x100000001b3
		}
		return h
	}
	for _, m := range []string{"2026-08", "2026-09"} {
		if got, want := monthHash("sharma", m), fnv1a("sharma|"+m); got != want {
			t.Errorf("monthHash(sharma, %s) = %#x, want %#x", m, got, want)
		}
	}
	if monthHash("sharma", "2026-09") == monthHash("sharma", "2026-08") {
		t.Error("months hash the same")
	}
}
