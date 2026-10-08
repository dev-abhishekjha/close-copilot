package seed

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const (
	companiesDir = "../../config/companies"
	rulesPath    = "../../config/rules.yaml"
)

func TestLoadShippedProfiles(t *testing.T) {
	profiles, err := LoadProfiles(companiesDir)
	if err != nil {
		t.Fatalf("LoadProfiles: %v", err)
	}
	if len(profiles) != 2 || profiles[0].ID != "mehta" || profiles[1].ID != "sharma" {
		t.Fatalf("LoadProfiles: want [mehta sharma] sorted by id, got %d profiles", len(profiles))
	}

	want := map[string]struct{ company, abbr, state string }{
		"sharma": {"Sharma Traders Pvt Ltd", "STPL", "29"},
		"mehta":  {"Mehta Tech Services Pvt Ltd", "MTSPL", "27"},
	}
	for _, p := range profiles {
		t.Run(p.ID, func(t *testing.T) {
			w := want[p.ID]
			if p.ERPCompany != w.company || p.Abbr != w.abbr || p.StateCode != w.state {
				t.Errorf("got %q %q %q, want %q %q %q", p.ERPCompany, p.Abbr, p.StateCode, w.company, w.abbr, w.state)
			}
			if got := PAN(p.Seed, p.ID, EntityCompany); p.PAN != got {
				t.Errorf("pan = %q, want the generated %q", p.PAN, got)
			}
			g, err := p.GSTIN()
			if err != nil {
				t.Fatalf("company GSTIN: %v", err)
			}
			if err := ValidGSTIN(g); err != nil {
				t.Errorf("company GSTIN: %v", err)
			}
			if n := len(p.Suppliers); n < MinSuppliers || n > MaxSuppliers {
				t.Errorf("%d suppliers, want %d-%d", n, MinSuppliers, MaxSuppliers)
			}

			var inState, outState, unregistered int
			gstins := map[string]string{g: "company"}
			for _, s := range p.Suppliers {
				if !s.IsRegistered() {
					unregistered++
					if _, err := p.SupplierGSTIN(s); !errors.Is(err, ErrUnregistered) {
						t.Errorf("%s: SupplierGSTIN = %v, want ErrUnregistered", s.ID, err)
					}
					continue
				}
				sg, err := p.SupplierGSTIN(s)
				if err != nil {
					t.Errorf("%s: %v", s.ID, err)
					continue
				}
				if err := ValidGSTIN(sg); err != nil {
					t.Errorf("%s: %v", s.ID, err)
				}
				if sg[:2] != s.StateCode {
					t.Errorf("%s: GSTIN %s has state code %s, want %s", s.ID, sg, sg[:2], s.StateCode)
				}
				if sg[2:12] != p.SupplierPAN(s) {
					t.Errorf("%s: GSTIN %s doesn't carry PAN %s", s.ID, sg, p.SupplierPAN(s))
				}
				if prev, dup := gstins[sg]; dup {
					t.Errorf("%s and %s share GSTIN %s", s.ID, prev, sg)
				}
				gstins[sg] = s.ID
				if p.InState(s) {
					inState++
				} else {
					outState++
				}
			}
			if inState == 0 || outState == 0 || unregistered == 0 {
				t.Errorf("in-state %d, out-of-state %d, unregistered %d: want each at least 1", inState, outState, unregistered)
			}
		})
	}

	sharma, mehta := profiles[1], profiles[0]
	if sharma.Seed == mehta.Seed {
		t.Errorf("both companies have seed %d", sharma.Seed)
	}
	if sharma.Sales.GatewayShare.BP != 4000 || sharma.Sales.GatewayFeePct.BP != 200 {
		t.Errorf("sharma gateway_share %d bp, gateway_fee_pct %d bp; want 4000 and 200",
			sharma.Sales.GatewayShare.BP, sharma.Sales.GatewayFeePct.BP)
	}
	if !sharma.Payroll.Day.Last || sharma.Payroll.MonthlyINR != Fixed(600000) {
		t.Errorf("sharma payroll = %+v, want 600000 on the last day", sharma.Payroll)
	}
	for _, s := range sharma.Suppliers {
		if s.ID == "city-power" && (s.MonthlyINR == nil || *s.MonthlyINR != Between(18000, 26000) || s.Day.N != 10) {
			t.Errorf("city-power = %+v, want [18000, 26000] on day 10", s)
		}
	}
}

func TestLoadProfileChecksFileName(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(companiesDir, "sharma.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "other.yaml")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = LoadProfile(path)
	if err == nil || !strings.Contains(err.Error(), `id: "sharma" doesn't match the file name (want "other")`) {
		t.Errorf("LoadProfile(other.yaml) = %v, want an id mismatch", err)
	}
}

func TestLoadRules(t *testing.T) {
	got, err := LoadRules(rulesPath)
	if err != nil {
		t.Fatalf("LoadRules: %v", err)
	}
	want := Rules{
		Capitalisation: CapitalisationRules{
			ThresholdINR:           50000,
			AssetKeywords:          []string{"laptop", "printer", "server", "furniture", "air conditioner"},
			WatchedExpenseAccounts: []string{"Repairs and Maintenance"},
		},
		Prepaid: PrepaidRules{
			MinAmountINR: 50000,
			Keywords:     []string{"annual", "yearly", "12 months", "renewal"},
		},
		Variance:  VarianceRules{PctThreshold: 25, AbsThresholdINR: 50000},
		BankMatch: BankMatchRules{DateWindowDays: 3},
		GSTR2B:    GSTR2BRules{AmountTolerancePaise: 100},
		Accruals:  AccrualRules{LookbackMonths: 4, MinOccurrences: 3, AmountBandPct: 20},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("LoadRules =\n%+v\nwant\n%+v", got, want)
	}
}

func TestBrokenRulesReportsAll(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rules.yaml")
	writeFile(t, path, `
capitalisation:
  threshold_inr: 0
  asset_keywords: []
  watched_expense_accounts: ["Repairs and Maintenance"]
prepaid:
  min_amount_inr: 50000
  keywords: [annual, " "]
variance:
  pct_threshold: 25
  abs_threshold_inr: -1
bank_match:
  date_window_days: 16
gstr2b:
  amount_tolerance_paise: -5
accruals:
  lookback_months: 2
  min_occurrences: 3
  amount_band_pct: 20
  extra: 1
`)
	_, err := LoadRules(path)
	if err == nil {
		t.Fatal("LoadRules: want an error")
	}
	assertProblems(t, path, err, []string{
		"capitalisation.threshold_inr: must be positive",
		"capitalisation.asset_keywords: must not be empty",
		"prepaid.keywords[1]: must not be blank",
		"variance.abs_threshold_inr: must be positive",
		"bank_match.date_window_days: want 0-15, got 16",
		"gstr2b.amount_tolerance_paise: must not be negative",
		"accruals.min_occurrences: 3 is more than lookback_months 2",
		"field extra not found",
	})
}

func TestBrokenProfileReportsAll(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broken.yaml")
	writeFile(t, path, `
id: brokenx
erp_company: Broken Co
abbr: BRK
state_code: "40"
pan: ABC12
opening_bank_balance_inr: 100
seed: 5
customers: 3
colour: blue
sales:
  invoices_per_month: [10, 5]
  amount_inr: [100, 200]
  gst_rates: [5, 7]
  gateway_share: 0.12345
  gateway_fee_pct: 2.0
suppliers:
  - {id: a, name: A, kind: rent, monthly_inr: 100, day: 31, gst_rate: 18, state_code: "29"}
  - {id: a, name: A2, kind: furniture, monthly_inr: 100, gst_rate: 18, state_code: "29"}
  - {id: c, name: C, kind: subscription, billing: annual, amount_inr: 100, renew_month: 13, gst_rate: 18, state_code: "27", start_month: "2026-13"}
  - {id: d, name: D, kind: utility, monthly_inr: 100, registered: false, state_code: "29", gst_rate: 18}
  - {id: e, name: E, kind: services, billing: weekly, monthly_inr: [1, 2, 3], gst_rate: 18, state_code: "99"}
payroll: {monthly_inr: 0, day: last}
bank: {name: X, account: Y, monthly_charges_inr: 10}
`)
	_, err := LoadProfile(path)
	if err == nil {
		t.Fatal("LoadProfile: want an error")
	}
	assertProblems(t, path, err, []string{
		`id: "brokenx" doesn't match the file name (want "broken")`,
		`state_code: "40" is not a GST state code`,
		`pan: PAN "ABC12": want 10 characters`,
		"field colour not found",
		"sales.invoices_per_month: min 10 is greater than max 5",
		"sales.gst_rates[1]: 7 is not a GST rate",
		`sales.gateway_share: "0.12345" has more than 4 decimal places`,
		"suppliers: want 15-20 suppliers, got 5",
		"suppliers[0] (a).day: want 1-28 or last, got 31",
		`suppliers[1] (a).id: "a" is also suppliers[0]`,
		`suppliers[1] (a).kind: "furniture" is not one of`,
		"suppliers[2] (c).renew_month: want 1-12, got 13",
		`suppliers[2] (c).start_month: "2026-13" is not a YYYY-MM month`,
		"suppliers[3] (d).state_code: an unregistered supplier has no state_code",
		"suppliers[3] (d).gst_rate: an unregistered supplier charges no GST",
		`suppliers[4] (e).billing: "weekly" is not one of`,
		`suppliers[4] (e).state_code: "99" is not a GST state code`,
		"payroll.monthly_inr: amounts must be positive",
		"suppliers: need at least one supplier of kind goods",
		"suppliers: need at least one registered supplier in the company's state",
	})
}

func TestBrokenProfileFatal(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.yaml")
	writeFile(t, empty, "")
	if _, err := LoadProfile(empty); err == nil || !strings.Contains(err.Error(), "file is empty") {
		t.Errorf("empty file: got %v", err)
	}
	syntax := filepath.Join(dir, "syntax.yaml")
	writeFile(t, syntax, "id: [unclosed\n")
	if _, err := LoadProfile(syntax); err == nil || !strings.HasPrefix(err.Error(), syntax+": ") {
		t.Errorf("syntax error: got %v", err)
	}
	if _, err := LoadProfiles(dir); err == nil {
		t.Error("LoadProfiles over broken files: want an error")
	}
	if _, err := LoadProfiles(t.TempDir()); err == nil || !strings.Contains(err.Error(), "no *.yaml profiles") {
		t.Errorf("LoadProfiles over an empty dir: got %v", err)
	}
}

func TestLoadProfilesRejectsSharedSeed(t *testing.T) {
	dir := t.TempDir()
	for _, id := range []string{"sharma", "mehta"} {
		data, err := os.ReadFile(filepath.Join(companiesDir, id+".yaml"))
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, id+".yaml"), string(data))
	}
	// Give mehta Sharma's seed; its PAN no longer matches the generator,
	// but validation only checks the PAN's format, so the seed check is
	// the one that fires.
	path := filepath.Join(dir, "mehta.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, strings.Replace(string(data), "seed: 1729", "seed: 42", 1))
	_, err = LoadProfiles(dir)
	if err == nil || !strings.Contains(err.Error(), "seed: 42 is also the seed of") {
		t.Errorf("LoadProfiles with a shared seed: got %v", err)
	}
}

func TestParseDecimal(t *testing.T) {
	tests := []struct {
		yaml    string
		share   BasisPoints
		percent BasisPoints
		bad     bool
	}{
		{yaml: "0.4", share: 4000, percent: 40},
		{yaml: "2.0", share: 20000, percent: 200},
		{yaml: "1", share: 10000, percent: 100},
		{yaml: "0.1234", share: 1234, bad: false, percent: -1},
		{yaml: "0.12345", bad: true},
		{yaml: "-0.4", bad: true},
		{yaml: ".5", bad: true},
		{yaml: "5.", bad: true},
		{yaml: "abc", bad: true},
		{yaml: "[1]", bad: true},
	}
	for _, tt := range tests {
		var v struct {
			S Share   `yaml:"s"`
			P Percent `yaml:"p"`
		}
		if err := decodeYAML("s: "+tt.yaml+"\np: "+tt.yaml+"\n", &v); err != nil {
			t.Fatalf("%s: %v", tt.yaml, err)
		}
		if tt.bad {
			if v.S.bad == "" || v.P.bad == "" {
				t.Errorf("%s: want a problem, got share %+v percent %+v", tt.yaml, v.S, v.P)
			}
			continue
		}
		if v.S.bad != "" || v.S.BP != tt.share {
			t.Errorf("%s as share = %+v, want %d bp", tt.yaml, v.S, tt.share)
		}
		if tt.percent < 0 {
			if v.P.bad == "" {
				t.Errorf("%s as percent: want more than 2 decimal places rejected", tt.yaml)
			}
		} else if v.P.bad != "" || v.P.BP != tt.percent {
			t.Errorf("%s as percent = %+v, want %d bp", tt.yaml, v.P, tt.percent)
		}
	}
}

func TestParseMonth(t *testing.T) {
	for _, ok := range []string{"2026-09", "2026-12", "2027-01"} {
		if _, err := ParseMonth(ok); err != nil {
			t.Errorf("ParseMonth(%q): %v", ok, err)
		}
	}
	for _, bad := range []string{"", "2026-13", "2026-00", "2026-9", "26-09", "2026-09-01", "2026/09"} {
		if _, err := ParseMonth(bad); err == nil {
			t.Errorf("ParseMonth(%q): want an error", bad)
		}
	}
}

func decodeYAML(src string, out any) error {
	errs, _ := decodeStrict("inline.yaml", []byte(src), out)
	return errors.Join(errs...)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// assertProblems checks that err names every wanted problem and that each
// line of it starts with the file name.
func assertProblems(t *testing.T, path string, err error, want []string) {
	t.Helper()
	msg := err.Error()
	lines := strings.Split(msg, "\n")
	for _, w := range want {
		found := false
		for _, line := range lines {
			// Validation problems read "<file>: <field>: ...", decoding
			// problems "<file>: line N: field <key> ...".
			if strings.HasPrefix(line, path+": "+w) ||
				(strings.HasPrefix(line, path+": line ") && strings.Contains(line, w)) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("error doesn't report %q", w)
		}
	}
	for _, line := range lines {
		if !strings.HasPrefix(line, path+": ") {
			t.Errorf("problem doesn't name the file: %q", line)
		}
	}
	if t.Failed() {
		t.Logf("full error:\n%s", msg)
	}
}
