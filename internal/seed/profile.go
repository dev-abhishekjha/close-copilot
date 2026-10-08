package seed

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Supplier kinds.
const (
	KindRent         = "rent"
	KindUtility      = "utility"
	KindSubscription = "subscription"
	KindGoods        = "goods"
	KindServices     = "services"
)

// Supplier billing cycles. An empty billing means monthly.
const (
	BillingMonthly = "monthly"
	BillingAnnual  = "annual"
)

// Supplier count bounds for a profile (CC-301).
const (
	MinSuppliers = 15
	MaxSuppliers = 20
)

var (
	supplierKinds = []string{KindRent, KindUtility, KindSubscription, KindGoods, KindServices}
	gstRates      = []int{0, 5, 12, 18, 28}
)

// ErrUnregistered is returned when a GSTIN is asked for a supplier that
// isn't registered for GST.
var ErrUnregistered = errors.New("supplier is not registered for GST")

// Profile is one synthetic company: config/companies/<id>.yaml (shared
// spec "Company profile"). Amounts in _inr fields are whole rupees.
type Profile struct {
	ID                    string     `yaml:"id"`
	ERPCompany            string     `yaml:"erp_company"`
	Abbr                  string     `yaml:"abbr"`
	StateCode             string     `yaml:"state_code"`
	PAN                   string     `yaml:"pan"`
	OpeningBankBalanceINR int64      `yaml:"opening_bank_balance_inr"`
	Seed                  int64      `yaml:"seed"`
	Customers             int        `yaml:"customers"`
	Sales                 Sales      `yaml:"sales"`
	Suppliers             []Supplier `yaml:"suppliers"`
	Payroll               Payroll    `yaml:"payroll"`
	Bank                  Bank       `yaml:"bank"`
}

// Sales describes the company's sales pattern.
type Sales struct {
	InvoicesPerMonth Range   `yaml:"invoices_per_month"`
	AmountINR        Range   `yaml:"amount_inr"`
	GSTRates         []int   `yaml:"gst_rates"`
	GatewayShare     Share   `yaml:"gateway_share"`   // share of sales collected through the payment gateway
	GatewayFeePct    Percent `yaml:"gateway_fee_pct"` // gateway fee, before 18% GST on the fee
}

// Supplier is one vendor the company buys from.
//
// A monthly supplier (billing empty or monthly) carries monthly_inr; an
// annual one carries amount_inr and renew_month. A registered supplier
// (the default) carries state_code and gst_rate; an unregistered one
// carries neither (gst_rate 0 is allowed).
type Supplier struct {
	ID         string `yaml:"id"`
	Name       string `yaml:"name"`
	Kind       string `yaml:"kind"`
	Billing    string `yaml:"billing"`
	MonthlyINR *Range `yaml:"monthly_inr"`
	AmountINR  *Range `yaml:"amount_inr"`
	RenewMonth int    `yaml:"renew_month"`
	Day        Day    `yaml:"day"`
	GSTRate    *int   `yaml:"gst_rate"`
	StateCode  string `yaml:"state_code"`
	Registered *bool  `yaml:"registered"`
	// StartMonth and EndMonth (YYYY-MM, optional, inclusive) bound the
	// months in which the supplier is active.
	StartMonth string `yaml:"start_month"`
	EndMonth   string `yaml:"end_month"`
}

// Payroll is the monthly salary run.
type Payroll struct {
	MonthlyINR Range `yaml:"monthly_inr"`
	Day        Day   `yaml:"day"`
}

// Bank is the company's current account.
type Bank struct {
	Name              string `yaml:"name"`
	Account           string `yaml:"account"`
	MonthlyChargesINR Range  `yaml:"monthly_charges_inr"`
}

// IsRegistered reports whether the supplier is registered for GST (the
// default when registered is absent).
func (s Supplier) IsRegistered() bool { return s.Registered == nil || *s.Registered }

// IsAnnual reports whether the supplier bills once a year.
func (s Supplier) IsAnnual() bool { return s.Billing == BillingAnnual }

// GSTIN returns the company's GSTIN, derived from its state code and PAN.
func (p Profile) GSTIN() (string, error) { return GSTIN(p.StateCode, p.PAN) }

// SupplierPAN returns the supplier's synthetic PAN, derived from the
// company seed and the supplier id: stable across runs, different between
// companies.
func (p Profile) SupplierPAN(s Supplier) string { return PAN(p.Seed, s.ID, EntityCompany) }

// SupplierGSTIN returns a registered supplier's GSTIN, or ErrUnregistered.
func (p Profile) SupplierGSTIN(s Supplier) (string, error) {
	if !s.IsRegistered() {
		return "", fmt.Errorf("%s: %w", s.ID, ErrUnregistered)
	}
	return GSTIN(s.StateCode, p.SupplierPAN(s))
}

// InState reports whether a registered supplier is in the company's own
// state (CGST + SGST) rather than another (IGST).
func (p Profile) InState(s Supplier) bool {
	return s.IsRegistered() && s.StateCode == p.StateCode
}

// Rules is config/rules.yaml (shared spec "Rules").
type Rules struct {
	Capitalisation CapitalisationRules `yaml:"capitalisation"`
	Prepaid        PrepaidRules        `yaml:"prepaid"`
	Variance       VarianceRules       `yaml:"variance"`
	BankMatch      BankMatchRules      `yaml:"bank_match"`
	GSTR2B         GSTR2BRules         `yaml:"gstr2b"`
	Accruals       AccrualRules        `yaml:"accruals"`
}

// CapitalisationRules flag asset purchases booked to expense accounts.
type CapitalisationRules struct {
	ThresholdINR           int64    `yaml:"threshold_inr"`
	AssetKeywords          []string `yaml:"asset_keywords"`
	WatchedExpenseAccounts []string `yaml:"watched_expense_accounts"`
}

// PrepaidRules flag multi-period costs expensed at once.
type PrepaidRules struct {
	MinAmountINR int64    `yaml:"min_amount_inr"`
	Keywords     []string `yaml:"keywords"`
}

// VarianceRules flag month-on-month swings.
type VarianceRules struct {
	PctThreshold    int64 `yaml:"pct_threshold"`
	AbsThresholdINR int64 `yaml:"abs_threshold_inr"`
}

// BankMatchRules tune the bank reconciliation matcher.
type BankMatchRules struct {
	DateWindowDays int `yaml:"date_window_days"`
}

// GSTR2BRules tune the GSTR-2B matcher.
type GSTR2BRules struct {
	AmountTolerancePaise int64 `yaml:"amount_tolerance_paise"`
}

// AccrualRules tune the missing-accrual detector.
type AccrualRules struct {
	LookbackMonths int `yaml:"lookback_months"`
	MinOccurrences int `yaml:"min_occurrences"`
	AmountBandPct  int `yaml:"amount_band_pct"`
}

// Range is an amount or count written as a scalar (150000) or a
// two-element list ([18000, 26000]). A scalar gives Min == Max.
type Range struct {
	Min, Max int64
	set      bool
	bad      string // decoding problem, reported by validation with the field name
}

// Fixed returns a range with Min == Max == v.
func Fixed(v int64) Range { return Range{Min: v, Max: v, set: true} }

// Between returns the range [lo, hi].
func Between(lo, hi int64) Range { return Range{Min: lo, Max: hi, set: true} }

// UnmarshalYAML implements yaml.Unmarshaler.
func (r *Range) UnmarshalYAML(n *yaml.Node) error {
	*r = Range{set: true}
	switch n.Kind {
	case yaml.ScalarNode:
		v, ok := yamlInt(n)
		if !ok {
			r.bad = fmt.Sprintf("want a whole number or [min, max], got %q", n.Value)
			return nil
		}
		r.Min, r.Max = v, v
	case yaml.SequenceNode:
		if len(n.Content) != 2 {
			r.bad = fmt.Sprintf("want [min, max], got a list of %d", len(n.Content))
			return nil
		}
		lo, ok1 := yamlInt(n.Content[0])
		hi, ok2 := yamlInt(n.Content[1])
		if !ok1 || !ok2 {
			r.bad = "want [min, max] with whole numbers"
			return nil
		}
		r.Min, r.Max = lo, hi
	default:
		r.bad = "want a whole number or [min, max]"
	}
	return nil
}

func yamlInt(n *yaml.Node) (int64, bool) {
	if n.Kind != yaml.ScalarNode || n.ShortTag() != "!!int" {
		return 0, false
	}
	var v int64
	if err := n.Decode(&v); err != nil {
		return 0, false
	}
	return v, true
}

// Day is a day of the month: 1-28, or last.
type Day struct {
	N    int  // 1-28; 0 when Last
	Last bool // the last day of the month
	set  bool
	bad  string
}

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Day) UnmarshalYAML(n *yaml.Node) error {
	*d = Day{set: true}
	if n.Kind == yaml.ScalarNode && n.Value == "last" {
		d.Last = true
		return nil
	}
	v, ok := yamlInt(n)
	if !ok {
		d.bad = fmt.Sprintf("want 1-28 or last, got %q", n.Value)
		return nil
	}
	if v < 1 || v > 28 {
		d.bad = fmt.Sprintf("want 1-28 or last, got %d", v)
		return nil
	}
	d.N = int(v)
	return nil
}

// IsSet reports whether the day was given.
func (d Day) IsSet() bool { return d.set }

// String returns "last" or the day number.
func (d Day) String() string {
	if d.Last {
		return "last"
	}
	return fmt.Sprintf("%d", d.N)
}

// BasisPoints is a fixed-point ratio: 10000 basis points are 100%.
type BasisPoints int64

// Share is a fraction from 0 to 1 written as a decimal with at most four
// places (0.4) and held in basis points (4000).
type Share struct {
	BP  BasisPoints
	set bool
	bad string
}

// UnmarshalYAML implements yaml.Unmarshaler.
func (s *Share) UnmarshalYAML(n *yaml.Node) error {
	*s = Share{set: true}
	v, err := parseDecimal(n, 4)
	if err != nil {
		s.bad = err.Error()
		return nil
	}
	s.BP = BasisPoints(v)
	return nil
}

// Percent is a percentage written as a decimal (2.0) and held in basis
// points (200), so it takes at most two decimal places.
type Percent struct {
	BP  BasisPoints
	set bool
	bad string
}

// UnmarshalYAML implements yaml.Unmarshaler.
func (p *Percent) UnmarshalYAML(n *yaml.Node) error {
	*p = Percent{set: true}
	v, err := parseDecimal(n, 2)
	if err != nil {
		p.bad = err.Error()
		return nil
	}
	p.BP = BasisPoints(v)
	return nil
}

// parseDecimal reads a non-negative decimal scalar as text and returns it
// times 10^places, rejecting more than places decimal places. No floating
// point is involved.
func parseDecimal(n *yaml.Node, places int) (int64, error) {
	if n.Kind != yaml.ScalarNode {
		return 0, errors.New("want a decimal number")
	}
	s := n.Value
	whole, frac, hasDot := strings.Cut(s, ".")
	if whole == "" || (hasDot && frac == "") || len(whole) > 12 {
		return 0, fmt.Errorf("want a non-negative decimal such as 0.4, got %q", s)
	}
	if len(frac) > places {
		return 0, fmt.Errorf("%q has more than %d decimal places", s, places)
	}
	var v int64
	for _, c := range []byte(whole + frac) {
		if !isDigit(c) {
			return 0, fmt.Errorf("want a non-negative decimal such as 0.4, got %q", s)
		}
		v = v*10 + int64(c-'0')
	}
	for range places - len(frac) {
		v *= 10
	}
	return v, nil
}

// ParseMonth parses a YYYY-MM month to UTC midnight on its first day.
func ParseMonth(s string) (time.Time, error) {
	if len(s) != 7 {
		return time.Time{}, fmt.Errorf("%q is not a YYYY-MM month", s)
	}
	t, err := time.Parse("2006-01", s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is not a YYYY-MM month", s)
	}
	return t, nil
}

// LoadProfile reads and validates one profile. Every problem is reported
// at once, each naming the file and the field.
func LoadProfile(path string) (Profile, error) {
	var p Profile
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return Profile{}, fmt.Errorf("load profile: %w", err)
	}
	errs, fatal := decodeStrict(path, data, &p)
	if !fatal {
		wantID := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		errs = append(errs, validateProfile(path, wantID, &p)...)
	}
	if len(errs) > 0 {
		return Profile{}, errors.Join(errs...)
	}
	return p, nil
}

// LoadProfiles loads every *.yaml profile in dir, sorted by id, and checks
// that the companies differ in seed, ERPNext name and abbreviation.
func LoadProfiles(dir string) ([]Profile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("load profiles: %w", err)
	}
	var (
		out   []Profile
		files []string
		errs  []error
	)
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		p, err := LoadProfile(path)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		out = append(out, p)
		files = append(files, path)
	}
	if len(out) == 0 && len(errs) == 0 {
		return nil, fmt.Errorf("load profiles: no *.yaml profiles in %s", dir)
	}
	seeds := map[int64]string{}
	names := map[string]string{}
	abbrs := map[string]string{}
	for i, p := range out {
		if prev, ok := seeds[p.Seed]; ok {
			errs = append(errs, fmt.Errorf("%s: seed: %d is also the seed of %s", files[i], p.Seed, prev))
		}
		seeds[p.Seed] = files[i]
		if prev, ok := names[p.ERPCompany]; ok {
			errs = append(errs, fmt.Errorf("%s: erp_company: %q is also in %s", files[i], p.ERPCompany, prev))
		}
		names[p.ERPCompany] = files[i]
		if prev, ok := abbrs[p.Abbr]; ok {
			errs = append(errs, fmt.Errorf("%s: abbr: %q is also in %s", files[i], p.Abbr, prev))
		}
		abbrs[p.Abbr] = files[i]
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// LoadRules reads and validates config/rules.yaml, reporting every problem
// at once.
func LoadRules(path string) (Rules, error) {
	var r Rules
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return Rules{}, fmt.Errorf("load rules: %w", err)
	}
	errs, fatal := decodeStrict(path, data, &r)
	if !fatal {
		errs = append(errs, validateRules(path, &r)...)
	}
	if len(errs) > 0 {
		return Rules{}, errors.Join(errs...)
	}
	return r, nil
}

// decodeStrict decodes one YAML document, rejecting unknown keys. Type
// errors are returned one per problem and don't stop validation; a syntax
// error or an empty file is fatal.
func decodeStrict(file string, data []byte, out any) (errs []error, fatal bool) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	err := dec.Decode(out)
	if err == nil {
		return nil, false
	}
	if errors.Is(err, io.EOF) {
		return []error{fmt.Errorf("%s: file is empty", file)}, true
	}
	var te *yaml.TypeError
	if errors.As(err, &te) {
		for _, msg := range te.Errors {
			errs = append(errs, fmt.Errorf("%s: %s", file, msg))
		}
		return errs, false
	}
	return []error{fmt.Errorf("%s: %w", file, err)}, true
}

// problems collects validation errors, each prefixed with the file name.
type problems struct {
	file string
	errs []error
}

func (p *problems) add(field, format string, args ...any) {
	p.errs = append(p.errs, fmt.Errorf("%s: %s: %s", p.file, field, fmt.Sprintf(format, args...)))
}

func (p *problems) required(field, value string) {
	if strings.TrimSpace(value) == "" {
		p.add(field, "is required")
	}
}

func (p *problems) rng(field string, r Range, required bool) {
	switch {
	case r.bad != "":
		p.add(field, "%s", r.bad)
	case !r.set:
		if required {
			p.add(field, "is required")
		}
	case r.Min <= 0 || r.Max <= 0:
		p.add(field, "amounts must be positive, got [%d, %d]", r.Min, r.Max)
	case r.Min > r.Max:
		p.add(field, "min %d is greater than max %d", r.Min, r.Max)
	}
}

func (p *problems) day(field string, d Day, required bool) {
	switch {
	case d.bad != "":
		p.add(field, "%s", d.bad)
	case !d.set && required:
		p.add(field, "is required (1-28 or last)")
	}
}

func (p *problems) month(field, value string) {
	if value == "" {
		return
	}
	if _, err := ParseMonth(value); err != nil {
		p.add(field, "%v", err)
	}
}

func (p *problems) gstRate(field string, rate int) {
	if !slices.Contains(gstRates, rate) {
		p.add(field, "%d is not a GST rate (0, 5, 12, 18, 28)", rate)
	}
}

func validSlug(s string) bool {
	if s == "" || s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if (c < 'a' || c > 'z') && !isDigit(c) && c != '-' {
			return false
		}
	}
	return true
}

func validateProfile(file, wantID string, prof *Profile) []error {
	p := &problems{file: file}

	switch {
	case prof.ID == "":
		p.add("id", "is required")
	case prof.ID != wantID:
		p.add("id", "%q doesn't match the file name (want %q)", prof.ID, wantID)
	case !validSlug(prof.ID):
		p.add("id", "%q must be lower-case letters, digits and hyphens", prof.ID)
	}
	p.required("erp_company", prof.ERPCompany)
	p.required("abbr", prof.Abbr)
	if !ValidStateCode(prof.StateCode) {
		p.add("state_code", "%q is not a GST state code (01-38 or 97)", prof.StateCode)
	}
	p.companyPAN(prof)
	if prof.OpeningBankBalanceINR < 0 {
		p.add("opening_bank_balance_inr", "must not be negative, got %d", prof.OpeningBankBalanceINR)
	}
	if prof.Seed == 0 {
		p.add("seed", "is required and must not be 0")
	}
	if prof.Customers <= 0 {
		p.add("customers", "must be positive, got %d", prof.Customers)
	}

	p.sales(&prof.Sales)
	p.suppliers(prof)

	p.rng("payroll.monthly_inr", prof.Payroll.MonthlyINR, true)
	p.day("payroll.day", prof.Payroll.Day, true)

	p.required("bank.name", prof.Bank.Name)
	p.required("bank.account", prof.Bank.Account)
	p.rng("bank.monthly_charges_inr", prof.Bank.MonthlyChargesINR, true)

	return p.errs
}

func (p *problems) companyPAN(prof *Profile) {
	if err := ValidPAN(prof.PAN); err != nil {
		p.add("pan", "%v", err)
		return
	}
	if prof.PAN[3] != EntityCompany {
		p.add("pan", "%q: 4th letter must be C for a company", prof.PAN)
	}
	if prof.ERPCompany != "" && prof.PAN[4] != toUpper(prof.ERPCompany[0]) {
		p.add("pan", "%q: 5th letter must be %q, the first letter of %q", prof.PAN, toUpper(prof.ERPCompany[0]), prof.ERPCompany)
	}
}

func (p *problems) sales(s *Sales) {
	p.rng("sales.invoices_per_month", s.InvoicesPerMonth, true)
	p.rng("sales.amount_inr", s.AmountINR, true)
	if len(s.GSTRates) == 0 {
		p.add("sales.gst_rates", "is required")
	}
	for i, r := range s.GSTRates {
		p.gstRate(fmt.Sprintf("sales.gst_rates[%d]", i), r)
		if slices.Contains(s.GSTRates[:i], r) {
			p.add(fmt.Sprintf("sales.gst_rates[%d]", i), "%d is listed twice", r)
		}
	}
	switch {
	case s.GatewayShare.bad != "":
		p.add("sales.gateway_share", "%s", s.GatewayShare.bad)
	case !s.GatewayShare.set:
		p.add("sales.gateway_share", "is required")
	case s.GatewayShare.BP > 10000:
		p.add("sales.gateway_share", "must be between 0 and 1")
	}
	switch {
	case s.GatewayFeePct.bad != "":
		p.add("sales.gateway_fee_pct", "%s", s.GatewayFeePct.bad)
	case !s.GatewayFeePct.set:
		p.add("sales.gateway_fee_pct", "is required")
	case s.GatewayFeePct.BP > 10000:
		p.add("sales.gateway_fee_pct", "must be between 0 and 100")
	}
}

func (p *problems) suppliers(prof *Profile) {
	n := len(prof.Suppliers)
	if n < MinSuppliers || n > MaxSuppliers {
		p.add("suppliers", "want %d-%d suppliers, got %d", MinSuppliers, MaxSuppliers, n)
	}
	seen := map[string]int{}
	kinds := map[string]bool{}
	var annual, unregistered, inState, outState bool
	companyGSTIN, _ := prof.GSTIN()
	gstins := map[string]string{}
	if companyGSTIN != "" {
		gstins[companyGSTIN] = "the company"
	}

	for i, s := range prof.Suppliers {
		f := fmt.Sprintf("suppliers[%d]", i)
		if s.ID != "" {
			f = fmt.Sprintf("suppliers[%d] (%s)", i, s.ID)
		}
		switch {
		case s.ID == "":
			p.add(f+".id", "is required")
		case !validSlug(s.ID):
			p.add(f+".id", "%q must be lower-case letters, digits and hyphens", s.ID)
		case s.ID == prof.ID:
			p.add(f+".id", "%q is the company id", s.ID)
		}
		if j, dup := seen[s.ID]; dup && s.ID != "" {
			p.add(f+".id", "%q is also suppliers[%d]", s.ID, j)
		} else {
			seen[s.ID] = i
		}
		p.required(f+".name", s.Name)

		if slices.Contains(supplierKinds, s.Kind) {
			kinds[s.Kind] = true
		} else {
			p.add(f+".kind", "%q is not one of %s", s.Kind, strings.Join(supplierKinds, ", "))
		}

		switch s.Billing {
		case "", BillingMonthly:
			p.rng(f+".monthly_inr", deref(s.MonthlyINR), true)
			if s.AmountINR != nil {
				p.add(f+".amount_inr", "is only for billing: annual; use monthly_inr")
			}
			if s.RenewMonth != 0 {
				p.add(f+".renew_month", "is only for billing: annual")
			}
		case BillingAnnual:
			p.rng(f+".amount_inr", deref(s.AmountINR), true)
			if s.MonthlyINR != nil {
				p.add(f+".monthly_inr", "is not for billing: annual; use amount_inr")
			}
			if s.RenewMonth == 0 {
				p.add(f+".renew_month", "is required for billing: annual (1-12)")
			}
			if s.Kind == KindSubscription {
				annual = true
			}
		default:
			p.add(f+".billing", "%q is not one of monthly, annual", s.Billing)
		}
		if s.RenewMonth != 0 && (s.RenewMonth < 1 || s.RenewMonth > 12) {
			p.add(f+".renew_month", "want 1-12, got %d", s.RenewMonth)
		}
		p.day(f+".day", s.Day, false)
		p.month(f+".start_month", s.StartMonth)
		p.month(f+".end_month", s.EndMonth)
		if s.StartMonth != "" && s.EndMonth != "" && s.EndMonth < s.StartMonth {
			p.add(f+".end_month", "%s is before start_month %s", s.EndMonth, s.StartMonth)
		}

		if !s.IsRegistered() {
			unregistered = true
			if s.StateCode != "" {
				p.add(f+".state_code", "an unregistered supplier has no state_code")
			}
			if s.GSTRate != nil && *s.GSTRate != 0 {
				p.add(f+".gst_rate", "an unregistered supplier charges no GST, got %d", *s.GSTRate)
			}
			continue
		}
		if s.GSTRate == nil {
			p.add(f+".gst_rate", "is required for a registered supplier")
		} else {
			p.gstRate(f+".gst_rate", *s.GSTRate)
		}
		if !ValidStateCode(s.StateCode) {
			p.add(f+".state_code", "%q is not a GST state code (01-38 or 97)", s.StateCode)
			continue
		}
		if s.StateCode == prof.StateCode {
			inState = true
		} else {
			outState = true
		}
		if g, err := prof.SupplierGSTIN(s); err == nil && s.ID != "" {
			if other, dup := gstins[g]; dup {
				p.add(f, "derived GSTIN %s collides with %s; rename the supplier id", g, other)
			}
			gstins[g] = s.ID
		}
	}

	for _, k := range supplierKinds {
		if !kinds[k] {
			p.add("suppliers", "need at least one supplier of kind %s", k)
		}
	}
	if !annual {
		p.add("suppliers", "need at least one subscription with billing: annual")
	}
	if !unregistered {
		p.add("suppliers", "need at least one supplier with registered: false")
	}
	if !inState {
		p.add("suppliers", "need at least one registered supplier in the company's state (CGST + SGST)")
	}
	if !outState {
		p.add("suppliers", "need at least one registered supplier in another state (IGST)")
	}
}

func deref(r *Range) Range {
	if r == nil {
		return Range{}
	}
	return *r
}

func validateRules(file string, r *Rules) []error {
	p := &problems{file: file}
	positive := func(field string, v int64) {
		if v <= 0 {
			p.add(field, "must be positive, got %d", v)
		}
	}
	keywords := func(field string, list []string) {
		if len(list) == 0 {
			p.add(field, "must not be empty")
		}
		for i, k := range list {
			if strings.TrimSpace(k) == "" {
				p.add(fmt.Sprintf("%s[%d]", field, i), "must not be blank")
			}
		}
	}

	positive("capitalisation.threshold_inr", r.Capitalisation.ThresholdINR)
	keywords("capitalisation.asset_keywords", r.Capitalisation.AssetKeywords)
	keywords("capitalisation.watched_expense_accounts", r.Capitalisation.WatchedExpenseAccounts)
	positive("prepaid.min_amount_inr", r.Prepaid.MinAmountINR)
	keywords("prepaid.keywords", r.Prepaid.Keywords)
	positive("variance.pct_threshold", r.Variance.PctThreshold)
	positive("variance.abs_threshold_inr", r.Variance.AbsThresholdINR)
	if d := r.BankMatch.DateWindowDays; d < 0 || d > 15 {
		p.add("bank_match.date_window_days", "want 0-15, got %d", d)
	}
	if t := r.GSTR2B.AmountTolerancePaise; t < 0 {
		p.add("gstr2b.amount_tolerance_paise", "must not be negative, got %d", t)
	}
	positive("accruals.lookback_months", int64(r.Accruals.LookbackMonths))
	positive("accruals.min_occurrences", int64(r.Accruals.MinOccurrences))
	positive("accruals.amount_band_pct", int64(r.Accruals.AmountBandPct))
	if r.Accruals.MinOccurrences > r.Accruals.LookbackMonths {
		p.add("accruals.min_occurrences", "%d is more than lookback_months %d", r.Accruals.MinOccurrences, r.Accruals.LookbackMonths)
	}
	return p.errs
}
