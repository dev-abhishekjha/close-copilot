package seed

import (
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// CorpusHistoryMonths are the months that get a close note. The corpus is
// built from the profiles and the rules only, never from planted errors.
var CorpusHistoryMonths = []string{"2026-04", "2026-05", "2026-06"}

// corpusEffective is the contract start date for a supplier with no
// start_month in its profile.
const corpusEffective = "1 April 2025"

// CorpusDoc is one generated Markdown source file.
type CorpusDoc struct {
	Path    string // relative to the corpus root, slash-separated
	Content string
}

// CorpusDocs builds every contract and close note from the profiles and
// rules, in a stable order (company id, then supplier or month). The result
// depends only on its inputs.
func CorpusDocs(profiles []Profile, rules Rules) ([]CorpusDoc, error) {
	ps := append([]Profile(nil), profiles...)
	sort.Slice(ps, func(i, j int) bool { return ps[i].ID < ps[j].ID })
	var docs []CorpusDoc
	for _, p := range ps {
		for _, s := range p.Suppliers {
			c, err := contractMD(p, s)
			if err != nil {
				return nil, err
			}
			docs = append(docs, CorpusDoc{Path: fmt.Sprintf("%s/contracts/CON-%s.md", p.ID, s.ID), Content: c})
		}
		for _, m := range CorpusHistoryMonths {
			n, err := noteMD(p, rules, m)
			if err != nil {
				return nil, err
			}
			docs = append(docs, CorpusDoc{Path: fmt.Sprintf("%s/notes/NOTE-%s-%s.md", p.ID, p.ID, m), Content: n})
		}
	}
	return docs, nil
}

// GenerateCorpus writes the contract and close note Markdown files under
// outDir (outDir/<company>/contracts and outDir/<company>/notes).
func GenerateCorpus(profiles []Profile, rules Rules, outDir string) error {
	docs, err := CorpusDocs(profiles, rules)
	if err != nil {
		return err
	}
	for _, d := range docs {
		path := filepath.Join(outDir, filepath.FromSlash(d.Path))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return fmt.Errorf("corpus: %w", err)
		}
		if err := os.WriteFile(path, []byte(d.Content), 0o600); err != nil {
			return fmt.Errorf("corpus: %w", err)
		}
	}
	return nil
}

// inr formats whole rupees with Indian digit grouping: ₹1,50,000.
func inr(n int64) string {
	neg := n < 0
	if neg {
		n = -n
	}
	s := strconv.FormatInt(n, 10)
	if len(s) > 3 {
		head, tail := s[:len(s)-3], s[len(s)-3:]
		var parts []string
		for len(head) > 2 {
			parts = append([]string{head[len(head)-2:]}, parts...)
			head = head[:len(head)-2]
		}
		if head != "" {
			parts = append([]string{head}, parts...)
		}
		s = strings.Join(parts, ",") + "," + tail
	}
	if neg {
		s = "-" + s
	}
	return "₹" + s
}

func inrRange(lo, hi int64) string {
	if lo == hi {
		return inr(lo)
	}
	return inr(lo) + " to " + inr(hi)
}

// possessive returns the name with its possessive ending.
func possessive(name string) string {
	if strings.HasSuffix(name, "s") {
		return name + "'"
	}
	return name + "'s"
}

func ordinal(n int) string {
	suffix := "th"
	if n%100 < 11 || n%100 > 13 {
		switch n % 10 {
		case 1:
			suffix = "st"
		case 2:
			suffix = "nd"
		case 3:
			suffix = "rd"
		}
	}
	return strconv.Itoa(n) + suffix
}

func dayText(d Day) string {
	switch {
	case !d.IsSet():
		return ""
	case d.Last:
		return "the last day of the month"
	default:
		return "the " + ordinal(d.N) + " of the month"
	}
}

// taxText describes the GST charged on the supplier's invoices.
func taxText(p Profile, s Supplier) string {
	if !s.IsRegistered() {
		return "The supplier is not registered for GST and charges no GST. No input tax credit is claimed on its invoices."
	}
	rate := int64(0)
	if s.GSTRate != nil {
		rate = int64(*s.GSTRate)
	}
	if rate == 0 {
		return "Supplies under this contract are charged at a nil rate of GST."
	}
	if p.InState(s) {
		half := rate * 10 / 2
		return fmt.Sprintf("GST is charged at %d%%, split into CGST at %s%% and SGST at %s%%, because the supplier is in the same state as the company.", rate, tenths(half), tenths(half))
	}
	return fmt.Sprintf("GST is charged at %d%% as IGST, because the supplier is in another state (state code %s).", rate, s.StateCode)
}

// pct formats basis points as a percentage: 4000 is "40", 250 is "2.5".
func pct(bp int64) string {
	if bp%100 == 0 {
		return strconv.FormatInt(bp/100, 10)
	}
	return strings.TrimRight(fmt.Sprintf("%d.%02d", bp/100, bp%100), "0")
}

// tenths formats a value in tenths as "9" or "2.5".
func tenths(v int64) string {
	if v%10 == 0 {
		return strconv.FormatInt(v/10, 10)
	}
	return fmt.Sprintf("%d.%d", v/10, v%10)
}

var corpusMonthNames = []string{"", "January", "February", "March", "April", "May", "June", "July", "August", "September", "October", "November", "December"}

func corpusMonth(m int) string {
	if m < 1 || m > 12 {
		return ""
	}
	return corpusMonthNames[m]
}

func monthLabel(ym string) (string, error) {
	t, err := ParseMonth(ym)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s %d", corpusMonth(int(t.Month())), t.Year()), nil
}

// paymentDays is a stable choice from the profile seed and the supplier id.
func paymentDays(seed int64, id string) int {
	h := fnv.New64a()
	_, _ = fmt.Fprintf(h, "%d:%s", seed, id)
	opts := []int{15, 30, 30, 45}
	return opts[h.Sum64()%uint64(len(opts))]
}

var serviceText = map[string]string{
	"rent":         "Lease of commercial premises, including access, common area maintenance and parking, for the company's use as its place of business.",
	"utility":      "Supply of a utility service (power, connectivity or telecom) to the company's premises, charged on use for the billing month.",
	"subscription": "Licence and support for a software or cloud service used by the company, including updates and standard support.",
	"goods":        "Supply of goods for the company's business against purchase orders, delivered to the company's premises.",
	"services":     "Provision of professional or operational services to the company as described in the monthly statement of work.",
}

func contractMD(p Profile, s Supplier) (string, error) {
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }

	annual := s.IsAnnual()
	var lo, hi int64
	switch {
	case annual && s.AmountINR != nil:
		lo, hi = s.AmountINR.Min, s.AmountINR.Max
	case s.MonthlyINR != nil:
		lo, hi = s.MonthlyINR.Min, s.MonthlyINR.Max
	default:
		return "", fmt.Errorf("corpus: supplier %s/%s has no amount", p.ID, s.ID)
	}
	rate := int64(0)
	if s.GSTRate != nil {
		rate = int64(*s.GSTRate)
	}
	billing := "monthly"
	if annual {
		billing = "annual"
	}
	start, end := s.StartMonth, s.EndMonth
	startText, endText := corpusEffective, "until ended by either party with 60 days' written notice"
	if start != "" {
		l, err := monthLabel(start)
		if err != nil {
			return "", fmt.Errorf("corpus: %s/%s start_month: %w", p.ID, s.ID, err)
		}
		startText = "the start of " + l
	}
	if end != "" {
		l, err := monthLabel(end)
		if err != nil {
			return "", fmt.Errorf("corpus: %s/%s end_month: %w", p.ID, s.ID, err)
		}
		endText = "the end of " + l
	}
	orNone := func(v string) string {
		if v == "" {
			return "none"
		}
		return v
	}

	w("# Supply Agreement: %s", s.Name)
	w("")
	w("<!-- contract:supplier=%s company=%s billing=%s amount_min_inr=%d amount_max_inr=%d gst_rate=%d registered=%t service_start=%s service_end=%s renew_month=%d -->",
		s.ID, p.ID, billing, lo, hi, rate, s.IsRegistered(), orNone(start), orNone(end), s.RenewMonth)
	w("")
	w("Contract reference CON-%s. This agreement is between the company and the supplier named below. All names and figures are synthetic.", s.ID)
	w("")
	w("## 1. Parties")
	w("")
	w("| Party | Name | Registration |")
	w("| --- | --- | --- |")
	cg, err := p.GSTIN()
	if err != nil {
		return "", fmt.Errorf("corpus: %s gstin: %w", p.ID, err)
	}
	w("| Customer | %s | GSTIN %s |", p.ERPCompany, cg)
	reg := "Not registered for GST"
	if s.IsRegistered() && !s.Individual {
		sg, err := p.SupplierGSTIN(s)
		if err != nil {
			return "", fmt.Errorf("corpus: %s/%s gstin: %w", p.ID, s.ID, err)
		}
		reg = "GSTIN " + sg
	}
	w("| Supplier | %s | %s |", s.Name, reg)
	w("")
	w("## 2. Service")
	w("")
	w("%s", serviceText[s.Kind])
	w("")
	w("## 3. Billing and amount")
	w("")
	if annual {
		w("The supplier bills **annually, upfront**, once a year in %s, for the twelve months that follow. The annual fee is %s before GST.", corpusMonth(s.RenewMonth), inrRange(lo, hi))
		w("")
		w("Because the fee covers twelve months of service, the company books it as a prepaid expense and spreads it monthly over the service period.")
	} else {
		w("The supplier bills **monthly**. The amount is %s a month before GST.", monthlyAmountText(lo, hi))
		if d := dayText(s.Day); d != "" {
			w("")
			w("Invoices are raised on %s.", d)
		}
	}
	w("")
	w("## 4. GST")
	w("")
	w("%s", taxText(p, s))
	w("")
	w("## 5. Service period")
	w("")
	if annual {
		w("The agreement is effective from %s and renews each year in %s, %s.", startText, corpusMonth(s.RenewMonth), endText)
	} else {
		w("The agreement is effective from %s and continues %s.", startText, endText)
	}
	w("")
	w("## 6. Payment terms")
	w("")
	w("The company pays each undisputed invoice within %d days of the invoice date, by bank transfer. A disputed amount is raised in writing within seven days of receipt, and the undisputed part is paid on time.", paymentDays(p.Seed, s.ID))
	w("")
	w("## 7. Invoices")
	w("")
	w("Each invoice quotes this contract reference, the service period it covers, the amount before GST and the tax. The supplier files its GST return in time so that the invoice appears in the company's GSTR-2B. A duplicate invoice for the same period is not payable.")
	return b.String(), nil
}

func monthlyAmountText(lo, hi int64) string {
	if lo == hi {
		return "fixed at " + inr(lo)
	}
	return "variable, between " + inr(lo) + " and " + inr(hi) + " depending on use or supply"
}

// activeIn reports whether the supplier is active in the month (YYYY-MM).
func corpusActive(s Supplier, ym string) bool {
	if s.StartMonth != "" && ym < s.StartMonth {
		return false
	}
	if s.EndMonth != "" && ym > s.EndMonth {
		return false
	}
	return true
}

func weekdays(t time.Time) bool { return t.Weekday() != time.Saturday && t.Weekday() != time.Sunday }

func noteMD(p Profile, rules Rules, ym string) (string, error) {
	t, err := ParseMonth(ym)
	if err != nil {
		return "", fmt.Errorf("corpus: %w", err)
	}
	label, _ := monthLabel(ym)
	last := t.AddDate(0, 1, -1)
	days := last.Day()
	// Working day 6 of the next month, counting weekdays only.
	wd, lockDay := 0, 0
	for d := last.AddDate(0, 0, 1); wd < 6; d = d.AddDate(0, 0, 1) {
		lockDay++
		if weekdays(d) {
			wd++
		}
	}

	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format+"\n", a...) }
	w("# Close Note: %s, %s", p.ERPCompany, label)
	w("")
	w("Written by the Controller for the close of %s. All names and figures are synthetic.", label)
	w("")
	w("## Calendar")
	w("")
	w("%s has %d days and ends on a %s. Working day 6 of the close falls %d days after month end, counting weekends.", corpusMonth(int(t.Month())), days, last.Weekday(), lockDay)
	w("")
	w("## Recurring suppliers and accruals")
	w("")
	w("Under POL-001 section 5.1, accrue a recurring supplier that has not billed at its usual amount, and reverse the accrual when the real bill arrives.")
	w("")
	count := 0
	for _, s := range p.Suppliers {
		if !corpusActive(s, ym) || s.IsAnnual() || s.MonthlyINR == nil || !s.Day.IsSet() {
			continue
		}
		count++
		usual := "at the usual amount of " + inr(s.MonthlyINR.Min)
		if s.MonthlyINR.Min != s.MonthlyINR.Max {
			usual = "at the usual amount, which runs between " + inr(s.MonthlyINR.Min) + " and " + inr(s.MonthlyINR.Max)
		}
		switch {
		case s.Kind == "utility" && !s.Day.Last:
			w("- %s bill usually arrives around the %s of the next month; accrue %s.", possessive(s.Name), ordinal(s.Day.N), usual)
		case s.Day.Last:
			w("- %s bills on the last day of the month; if the bill is missing, accrue %s.", s.Name, usual)
		default:
			w("- %s bills on the %s of the month; if the bill is missing, accrue %s.", s.Name, ordinal(s.Day.N), usual)
		}
	}
	if count == 0 {
		w("- No recurring supplier with a fixed billing day is active this month.")
	}
	w("")
	w("Suppliers without a fixed billing day invoice against deliveries. They are not accrued automatically; check the goods receipts at cut-off.")
	w("")
	w("## Annual costs")
	w("")
	m := int(t.Month())
	n := 0
	for _, s := range p.Suppliers {
		if !corpusActive(s, ym) || !s.IsAnnual() || s.RenewMonth != m {
			continue
		}
		n++
		amt := inr(s.AmountINR.Min)
		if s.AmountINR.Min != s.AmountINR.Max {
			amt = inr(s.AmountINR.Min) + " to " + inr(s.AmountINR.Max)
		}
		if s.AmountINR.Max >= rules.Prepaid.MinAmountINR {
			w("- %s renews this month at %s a year. It is above %s, so book it to prepaid and spread it monthly (POL-001 section 4.2).", s.Name, amt, inr(rules.Prepaid.MinAmountINR))
		} else {
			w("- %s renews this month at %s a year. It is below %s, so it may be expensed in the month (POL-001 section 4.3).", s.Name, amt, inr(rules.Prepaid.MinAmountINR))
		}
	}
	if n == 0 {
		w("- No annual contract renewed this month. Keep releasing the prepaid balances already on the books.")
	}
	w("")
	w("## Bank and gateway")
	w("")
	w("%s debits its current account charges every month, usually %s. Book them in the month they are debited (POL-001 section 7.1).", p.Bank.Name, inrRange(p.Bank.MonthlyChargesINR.Min, p.Bank.MonthlyChargesINR.Max))
	w("")
	w("About %s%% of sales are collected through the payment gateway, which keeps a fee of about %s%% plus 18%% GST on the fee. Book each settlement with its fee and GST (POL-001 section 7.2). Match bank lines to the ledger within %d days.", pct(int64(p.Sales.GatewayShare.BP)), pct(int64(p.Sales.GatewayFeePct.BP)), rules.BankMatch.DateWindowDays)
	w("")
	w("## Payroll")
	w("")
	pd := "on the " + ordinal(p.Payroll.Day.N)
	if p.Payroll.Day.Last {
		pd = "on the last day of the month"
	}
	w("Payroll is paid %s, about %s in total. Salaries belong to the month worked (POL-001 section 8.4).", pd, inrRange(p.Payroll.MonthlyINR.Min, p.Payroll.MonthlyINR.Max))
	w("")
	w("## Review")
	w("")
	w("Investigate a change of more than %d%% that is also more than %s against the previous month (POL-001 section 10.1). Claim input tax credit only for invoices that appear in GSTR-2B (POL-001 section 6.1).", rules.Variance.PctThreshold, inr(rules.Variance.AbsThresholdINR))
	return b.String(), nil
}
