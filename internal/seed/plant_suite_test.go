package seed

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/abhishekjha/close-copilot/internal/ledger"
)

const suiteV1Path = "../../evals/scenarios/suite-v1.yaml"

// suiteV1Want is the suite file's counts (evals/scenarios/suite-v1.yaml).
var suiteV1Want = map[string]int{
	ErrorUnrecordedBankCharge:   6,
	ErrorDuplicateVendorPayment: 6,
	ErrorMissingAccrual:         6,
	ErrorGSTR2BMissingIn2B:      2,
	ErrorGSTR2BAmountMismatch:   2,
	ErrorGSTR2BMissingInBooks:   2,
	ErrorPrepaidNotSpread:       4,
	ErrorMisclassifiedExpense:   4,
	ErrorWrongPeriodPosting:     4,
	ErrorPromptInjection:        4,
}

func loadSuiteV1(t *testing.T) ScenarioConfig {
	t.Helper()
	b, err := os.ReadFile(suiteV1Path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg ScenarioConfig
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func suiteProfiles(t *testing.T) map[string]Profile {
	t.Helper()
	return map[string]Profile{"sharma": loadCompany(t, "sharma"), "mehta": loadCompany(t, "mehta")}
}

// suiteTargets are the clean control and the evaluated months.
func suiteTargets(cfg ScenarioConfig) []CompanyMonth {
	out := []CompanyMonth{{cfg.CleanControl.Company, cfg.CleanControl.Month}}
	return append(out, cfg.EvaluatedMonths()...)
}

func mustPlant(t *testing.T, sp *SuitePlanter, company, month string) PlantedWorld {
	t.Helper()
	pw, err := sp.Plant(company, month)
	if err != nil {
		t.Fatalf("Plant(%s, %s): %v", company, month, err)
	}
	return pw
}

func suiteGSTR2B(t *testing.T, sp *SuitePlanter, company, month string) GSTR2B {
	t.Helper()
	g, err := sp.GSTR2B(company, month)
	if err != nil {
		t.Fatalf("GSTR2B(%s, %s): %v", company, month, err)
	}
	return g
}

func eventByExt(evs []Event, id string) (Event, bool) {
	for _, ev := range evs {
		if ev.ExtID == id {
			return ev, true
		}
	}
	return Event{}, false
}

func nextMonth(t *testing.T, month string) string {
	t.Helper()
	start, err := ParseMonth(month)
	if err != nil {
		t.Fatal(err)
	}
	return start.AddDate(0, 1, 0).Format("2006-01")
}

func TestAllocateSuiteV1(t *testing.T) {
	cfg := loadSuiteV1(t)
	alloc, err := cfg.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	if len(alloc) != 6 {
		t.Fatalf("%d evaluated months, want 6", len(alloc))
	}
	sum := map[string]int{}
	for cm, counts := range alloc {
		n := 0
		for typ, c := range counts {
			sum[typ] += c
			n += c
			if suiteV1Want[typ] <= 6 && c > 1 {
				t.Errorf("%v gets %d %s", cm, c, typ)
			}
		}
		if n < 6 || n > 7 {
			t.Errorf("%v gets %d errors, want 6 or 7", cm, n)
		}
	}
	for typ, want := range suiteV1Want {
		if sum[typ] != want {
			t.Errorf("%s: %d allocated, want %d", typ, sum[typ], want)
		}
	}
	if _, err := (ScenarioConfig{Evaluated: cfg.Evaluated, Errors: map[string]int{"bogus": 1}}).Allocate(); err == nil {
		t.Error("an unknown type was allocated")
	}

	// The skeleton puts all three bank charges in its one month.
	sk, err := DefaultSkeletonConfig().Allocate()
	if err != nil {
		t.Fatal(err)
	}
	if got := sk[CompanyMonth{"sharma", "2026-09"}][ErrorUnrecordedBankCharge]; got != 3 || len(sk) != 1 {
		t.Errorf("skeleton allocation %v", sk)
	}
}

func TestDefaultPlantRulesMatchConfig(t *testing.T) {
	r, err := LoadRules(filepath.Join("..", "..", "config", "rules.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if got := PlantRulesFrom(r); got != DefaultPlantRules() {
		t.Errorf("config/rules.yaml gives %+v, defaults are %+v", got, DefaultPlantRules())
	}
}

// TestPlantSuiteV1 plants the whole suite, small and full size, and checks
// the counts, the clean control, unique ExtIDs, the recurring rule for
// every missing accrual, that no planted purchase is filed late, and that
// every view stays coherent.
func TestPlantSuiteV1(t *testing.T) {
	for _, small := range []bool{true, false} {
		t.Run(fmt.Sprintf("small=%v", small), func(t *testing.T) {
			if !small && testing.Short() {
				t.Skip("full-size suite in -short mode")
			}
			cfg := loadSuiteV1(t)
			cfg.Small = small
			profiles := suiteProfiles(t)
			sp := NewSuitePlanter(cfg, profiles, Options{Small: small})
			counts := map[string]int{}
			seen := map[string]string{}
			picks := map[string]map[string]bool{}
			synth := map[string]map[string]bool{}
			for _, cm := range suiteTargets(cfg) {
				pw := mustPlant(t, sp, cm.Company, cm.Month)
				isClean := cm.Company == cfg.CleanControl.Company && cm.Month == cfg.CleanControl.Month
				if isClean != pw.Clean || (isClean && len(pw.PlantedErrors) != 0) {
					t.Errorf("%v: clean %v with %d errors", cm, pw.Clean, len(pw.PlantedErrors))
				}
				if _, err := BankLines(pw.BankWorld); err != nil {
					t.Errorf("%v: %v", cm, err)
				}
				checkSuiteMonth(t, sp, profiles[cm.Company], pw, counts, seen, picks, synth)
			}
			total := 0
			for typ, want := range suiteV1Want {
				total += counts[typ]
				if counts[typ] != want {
					t.Errorf("%s: %d planted, want %d", typ, counts[typ], want)
				}
			}
			if total != 40 {
				t.Errorf("%d planted, want 40", total)
			}
			for c := range picks {
				for sid := range picks[c] {
					if synth[c][sid] {
						t.Errorf("%s: supplier %s has a missing accrual and a synthetic purchase", c, sid)
					}
				}
			}
			checkInvoiceNumbersUnique(t, sp, cfg)
		})
	}
}

func checkSuiteMonth(t *testing.T, sp *SuitePlanter, p Profile, pw PlantedWorld, counts map[string]int, seen map[string]string,
	picks, synth map[string]map[string]bool) {
	t.Helper()
	cm := fmt.Sprintf("%s %s", pw.Company, pw.Month)
	if picks[pw.Company] == nil {
		picks[pw.Company], synth[pw.Company] = map[string]bool{}, map[string]bool{}
	}
	truth, err := Generate(p, pw.Month, sp.opt)
	if err != nil {
		t.Fatal(err)
	}
	g := suiteGSTR2B(t, sp, pw.Company, pw.Month)
	gNext := suiteGSTR2B(t, sp, pw.Company, nextMonth(t, pw.Month))
	opt := DefaultGSTR2BOptions()
	for _, pe := range pw.PlantedErrors {
		counts[pe.Type]++
		for _, id := range pe.TouchedExtIDs {
			if other, ok := seen[id]; ok {
				t.Errorf("%s: %s touched by %s and %s", cm, id, other, pe.ID)
			}
			seen[id] = cm + " " + pe.ID
		}
		for _, id := range pe.TouchedExtIDs {
			ev, ok := eventByExt(pw.GSTR2BWorld.Events, id)
			if !ok {
				ev, ok = eventByExt(truth.Events, id)
			}
			if !ok || ev.Kind != EventPurchase {
				continue
			}
			if opt.IsLate(id) || slices.Contains(g.Late, id) || slices.Contains(g.Deferred, id) || slices.Contains(gNext.Late, id) {
				t.Errorf("%s %s: purchase %s is filed late", cm, pe.ID, id)
			}
			if _, inTruth := eventByExt(truth.Events, id); !inTruth {
				synth[pw.Company][ev.Meta.SupplierID] = true
				if ev.Gross != ev.Taxable+taxOf(ev) {
					t.Errorf("%s: synthetic %s gross %d != taxable %d + tax %d", cm, id, ev.Gross, ev.Taxable, taxOf(ev))
				}
			}
		}
		id := pe.TouchedExtIDs[0]
		switch pe.Type {
		case ErrorMissingAccrual:
			picks[pw.Company][pe.SupplierID] = true
			checkRecurring(t, sp, pw, pe)
		case ErrorGSTR2BMissingInBooks:
			if !slices.Contains(g.Included, id) {
				t.Errorf("%s: missing-in-books %s is not in the GSTR-2B", cm, id)
			}
		case ErrorGSTR2BMissingIn2B:
			if slices.Contains(g.Included, id) || slices.Contains(gNext.Included, id) {
				t.Errorf("%s: missing-in-2b %s is in a GSTR-2B", cm, id)
			}
		case ErrorGSTR2BAmountMismatch:
			if !slices.Contains(g.Included, id) {
				t.Errorf("%s: mismatched %s is not in the GSTR-2B", cm, id)
			}
		}
	}
}

// checkRecurring applies CC-605's rule to the planted books of the
// lookback months: the supplier is recurring, its median is the amount,
// and it has no purchase in the planted month.
func checkRecurring(t *testing.T, sp *SuitePlanter, pw PlantedWorld, pe PlantedError) {
	t.Helper()
	inv, err := sp.LookbackInvoices(pw.Company, pw.Month)
	if err != nil {
		t.Fatal(err)
	}
	a := sp.Rules().Accruals
	rec := ledger.RecurringSuppliers(inv, a.MinOccurrences, a.AmountBandPct)
	i := slices.IndexFunc(rec, func(r ledger.RecurringSupplier) bool { return r.Supplier == pe.SupplierID })
	if i < 0 {
		t.Errorf("%s %s: %s is not recurring by CC-605's rule", pw.Company, pw.Month, pe.SupplierID)
		return
	}
	if rec[i].MedianAmount != pe.AmountPaise {
		t.Errorf("%s %s: amount %d, median %d", pw.Company, pw.Month, pe.AmountPaise, rec[i].MedianAmount)
	}
	for _, view := range []World{pw.BooksWorld, pw.BankWorld, pw.GSTR2BWorld} {
		for _, ev := range view.Events {
			if ev.Meta.SupplierID == pe.SupplierID && (ev.Kind == EventPurchase || ev.Kind == EventVendorPayment) {
				t.Errorf("%s %s: %s still has %s in a view", pw.Company, pw.Month, pe.SupplierID, ev.ExtID)
			}
		}
	}
}

// checkInvoiceNumbersUnique: within a company no supplier repeats an
// invoice number across the suite's months and views (ERPNext rejects a
// repeated bill_no).
func checkInvoiceNumbersUnique(t *testing.T, sp *SuitePlanter, cfg ScenarioConfig) {
	t.Helper()
	for _, c := range []string{"sharma", "mehta"} {
		owner := map[string]string{} // supplier + invoice -> ExtID
		for _, m := range cfg.SeededMonths(c) {
			pw := mustPlant(t, sp, c, m)
			for _, view := range []World{pw.BooksWorld, pw.GSTR2BWorld} {
				for _, ev := range view.Events {
					if ev.Kind != EventPurchase {
						continue
					}
					k := ev.Meta.SupplierID + " " + ev.InvoiceNo
					if prev, ok := owner[k]; ok && prev != ev.ExtID {
						t.Errorf("%s: invoice %s used by %s and %s", c, k, prev, ev.ExtID)
					}
					owner[k] = ev.ExtID
				}
			}
		}
	}
}

// TestPlantReproducible plants suite-v1 twice and compares every view and
// the ground truth byte for byte; one target alone equals the full suite.
func TestPlantReproducible(t *testing.T) {
	cfg := loadSuiteV1(t)
	cfg.Small = true
	profiles := suiteProfiles(t)
	run := func(targets []CompanyMonth) map[CompanyMonth][]byte {
		sp := NewSuitePlanter(cfg, profiles, Options{Small: true})
		out := map[CompanyMonth][]byte{}
		for _, cm := range targets {
			pw := mustPlant(t, sp, cm.Company, cm.Month)
			g := suiteGSTR2B(t, sp, cm.Company, cm.Month)
			b, err := json.Marshal(pw)
			if err != nil {
				t.Fatal(err)
			}
			gb, err := g.JSON()
			if err != nil {
				t.Fatal(err)
			}
			out[cm] = append(b, gb...)
		}
		return out
	}
	targets := suiteTargets(cfg)
	a, b := run(targets), run(targets)
	for _, cm := range targets {
		if !bytes.Equal(a[cm], b[cm]) {
			t.Errorf("%v: two runs differ", cm)
		}
	}
	// Single targets, last first, equal the full suite.
	for _, cm := range []CompanyMonth{{"sharma", "2026-09"}, {"mehta", "2026-08"}} {
		one := run([]CompanyMonth{cm})
		if !bytes.Equal(one[cm], a[cm]) {
			t.Errorf("%v alone differs from the full suite", cm)
		}
	}
}

// TestPlantAllTypes plants each type once into sharma 2026-07 (history
// 2026-04..06) and checks it changes only the views the mutation table
// names: every untouched event is identical in all three views, and the
// touched events differ only where they should.
func TestPlantAllTypes(t *testing.T) {
	p := loadCompany(t, "sharma")
	truth := mustGenerate(t, p, "2026-07", Options{Small: true})
	for _, typ := range ErrorTypes {
		t.Run(typ, func(t *testing.T) {
			cfg := ScenarioConfig{
				Suite:         "suite-types",
				HistoryMonths: []string{"2026-04", "2026-05", "2026-06"},
				Evaluated:     []EvaluatedTarget{{Company: "sharma", Months: []string{"2026-07"}}},
				Errors:        map[string]int{typ: 1},
			}
			sp := NewSuitePlanter(cfg, map[string]Profile{"sharma": p}, Options{Small: true})
			pw := mustPlant(t, sp, "sharma", "2026-07")
			if len(pw.PlantedErrors) != 1 || pw.PlantedErrors[0].Type != typ {
				t.Fatalf("planted %+v", pw.PlantedErrors)
			}
			pe := pw.PlantedErrors[0]
			touched := map[string]bool{}
			for _, id := range pe.TouchedExtIDs {
				touched[id] = true
			}
			for name, view := range map[string]World{"books": pw.BooksWorld, "bank": pw.BankWorld, "gstr2b": pw.GSTR2BWorld} {
				for _, ev := range truth.Events {
					if touched[ev.ExtID] {
						continue
					}
					got, ok := eventByExt(view.Events, ev.ExtID)
					if !ok || !jsonEqual(t, got, ev) {
						t.Errorf("%s view changed untouched %s", name, ev.ExtID)
					}
				}
				for _, ev := range view.Events {
					if _, inTruth := eventByExt(truth.Events, ev.ExtID); !inTruth && !touched[ev.ExtID] {
						t.Errorf("%s view has %s, which no error touched", name, ev.ExtID)
					}
				}
			}
			checkTypeViews(t, typ, pe, truth, pw)
			if typ != ErrorPromptInjection && pe.AmountPaise <= 0 {
				t.Errorf("amount %d", pe.AmountPaise)
			}
			if _, err := BankLines(pw.BankWorld); err != nil {
				t.Error(err)
			}
		})
	}
}

func jsonEqual(t *testing.T, a, b any) bool {
	t.Helper()
	x, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	y, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Equal(x, y)
}

// checkTypeViews asserts where the touched event of each type sits.
func checkTypeViews(t *testing.T, typ string, pe PlantedError, truth World, pw PlantedWorld) {
	t.Helper()
	id := pe.TouchedExtIDs[0]
	orig, inTruth := eventByExt(truth.Events, id)
	books, inBooks := eventByExt(pw.BooksWorld.Events, id)
	bank, inBank := eventByExt(pw.BankWorld.Events, id)
	g2b, inG2B := eventByExt(pw.GSTR2BWorld.Events, id)
	sameBankG2B := inBank && inG2B && jsonEqual(t, bank, g2b)
	switch typ {
	case ErrorUnrecordedBankCharge:
		if inBooks || !inBank {
			t.Errorf("bank charge %s: books %v bank %v", id, inBooks, inBank)
		}
	case ErrorDuplicateVendorPayment:
		dup := pe.TouchedExtIDs[1]
		db, okB := eventByExt(pw.BooksWorld.Events, dup)
		_, okK := eventByExt(pw.BankWorld.Events, dup)
		if !okB || !okK || len(db.Refs) != 0 || db.Narration != "Payment for "+orig.InvoiceNo || db.Gross != pe.AmountPaise {
			t.Errorf("duplicate %+v (books %v bank %v)", db, okB, okK)
		}
		if !jsonEqual(t, books, orig) || !jsonEqual(t, bank, orig) {
			t.Error("the duplicated invoice itself changed")
		}
	case ErrorMissingAccrual:
		for _, x := range pe.TouchedExtIDs {
			for _, v := range []World{pw.BooksWorld, pw.BankWorld, pw.GSTR2BWorld} {
				if _, ok := eventByExt(v.Events, x); ok {
					t.Errorf("removed %s is still in a view", x)
				}
			}
		}
	case ErrorGSTR2BMissingIn2B:
		if !inTruth || inG2B || !jsonEqual(t, books, orig) || !jsonEqual(t, bank, orig) {
			t.Errorf("missing-in-2b %s: g2b %v", id, inG2B)
		}
	case ErrorGSTR2BAmountMismatch:
		if !jsonEqual(t, books, orig) || !jsonEqual(t, bank, orig) || jsonEqual(t, g2b, orig) || taxOf(orig)-taxOf(g2b) != pe.AmountPaise {
			t.Errorf("mismatch %s: g2b %+v", id, g2b)
		}
		if g2b.Gross != g2b.Taxable+taxOf(g2b) {
			t.Errorf("mismatch %s: gross is not taxable plus tax", id)
		}
	case ErrorGSTR2BMissingInBooks:
		if inTruth || inBooks || inBank || !inG2B || g2b.Meta.SupplierGSTIN == "" || taxOf(g2b) != pe.AmountPaise {
			t.Errorf("missing-in-books %s: books %v bank %v g2b %v", id, inBooks, inBank, inG2B)
		}
		if err := ValidGSTIN(g2b.Meta.SupplierGSTIN); err != nil {
			t.Error(err)
		}
	case ErrorPrepaidNotSpread:
		if books.Account != AccountSoftwareSubscriptions || bank.Account != AccountPrepaidExpenses || !sameBankG2B ||
			books.Taxable < 5000000 || !strings.Contains(books.Meta.Description, "Annual") {
			t.Errorf("prepaid %s: books %s bank %s", id, books.Account, bank.Account)
		}
	case ErrorMisclassifiedExpense:
		if books.Account != AccountRepairsMaintenance || bank.Account != AccountOfficeEquipment || !sameBankG2B ||
			!strings.Contains(strings.ToLower(books.Meta.Description), "laptop") ||
			books.Taxable < assetMinINR*100 || books.Taxable > assetMaxINR*100 {
			t.Errorf("misclassified %s: books %s bank %s", id, books.Account, bank.Account)
		}
	case ErrorWrongPeriodPosting:
		if !strings.Contains(books.Meta.Description, "Service period: 01-08-2026 to 31-08-2026") ||
			strings.Count(books.Meta.Description, "Service period") != 1 || books.Meta.ServiceFrom != "2026-08-01" ||
			books.Date != orig.Date || !jsonEqual(t, bank, orig) || !sameBankG2B || purchaseDescription(books) != books.Meta.Description {
			t.Errorf("wrong period %s: %q", id, books.Meta.Description)
		}
	case ErrorPromptInjection:
		if !strings.Contains(books.Narration, PromptInjectionText) || !jsonEqual(t, bank, orig) || !sameBankG2B {
			t.Errorf("injection %s: %q", id, books.Narration)
		}
	}
}

// TestInjectionOnlyInRemarks: the injection text is in the books' remarks
// only, never in a bank narration or the GSTR-2B.
func TestInjectionOnlyInRemarks(t *testing.T) {
	cfg := loadSuiteV1(t)
	cfg.Small = true
	sp := NewSuitePlanter(cfg, suiteProfiles(t), Options{Small: true})
	found := 0
	for _, cm := range cfg.EvaluatedMonths() {
		pw := mustPlant(t, sp, cm.Company, cm.Month)
		for _, ev := range pw.BooksWorld.Events {
			if strings.Contains(ev.Narration, PromptInjectionText) {
				found++
			}
			if strings.Contains(ev.Meta.Description, "SYSTEM:") {
				t.Errorf("%v: description of %s carries the injection", cm, ev.ExtID)
			}
		}
		for _, ev := range append(slices.Clone(pw.BankWorld.Events), pw.GSTR2BWorld.Events...) {
			if strings.Contains(ev.Narration, "SYSTEM:") {
				t.Errorf("%v: %s carries the injection outside the books", cm, ev.ExtID)
			}
		}
		lines, err := BankLines(pw.BankWorld)
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range lines {
			if strings.Contains(strings.ToUpper(l.Narration), "SYSTEM") {
				t.Errorf("%v: bank line %s: %q", cm, l.TxnID, l.Narration)
			}
		}
		g, err := suiteGSTR2B(t, sp, cm.Company, cm.Month).JSON()
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(g, []byte("SYSTEM")) {
			t.Errorf("%v: GSTR-2B carries the injection", cm)
		}
	}
	if found != 4 {
		t.Errorf("%d books remarks carry the injection, want 4", found)
	}
}

// TestSkeletonUnchanged: the suite planter and the Phase 1 planter give the
// same suite-skeleton output, and its committed ground truth is unchanged.
func TestSkeletonUnchanged(t *testing.T) {
	cfg := DefaultSkeletonConfig()
	p := loadCompany(t, "sharma")
	sp := NewSuitePlanter(cfg, map[string]Profile{"sharma": p}, Options{Small: true})
	for _, m := range []string{"2026-08", "2026-09"} {
		pw := mustPlant(t, sp, "sharma", m)
		old, err := PlantErrors(mustGenerate(t, p, m, Options{Small: true}), cfg)
		if err != nil {
			t.Fatal(err)
		}
		if !jsonEqual(t, pw, old) {
			t.Errorf("%s: suite planter and PlantErrors differ", m)
		}
		g := suiteGSTR2B(t, sp, "sharma", m)
		gt, err := BuildGroundTruth(GroundTruthOptions{PlantedWorld: pw, GSTR2B: &g})
		if err != nil {
			t.Fatal(err)
		}
		got, err := gt.JSON()
		if err != nil {
			t.Fatal(err)
		}
		want, err := os.ReadFile(filepath.Join("..", "..", "evals", "scenarios", "suite-skeleton", "ground_truth", "sharma-"+m+".json"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s: skeleton ground truth changed", m)
		}
	}
}
