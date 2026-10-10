package seed

import (
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/abhishekjha/close-copilot/internal/company"
)

// The profile and rules tests live in internal/company with the code
// (CC-601a). These check that the seeder's aliases and forwarders reach it.

func TestProfileForwarders(t *testing.T) {
	dir := filepath.Join("..", "..", "config", "companies")
	got, err := LoadProfiles(dir)
	if err != nil {
		t.Fatalf("LoadProfiles: %v", err)
	}
	want, err := company.LoadProfiles(dir)
	if err != nil {
		t.Fatalf("company.LoadProfiles: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Error("seed.LoadProfiles differs from company.LoadProfiles")
	}
	one, err := LoadProfile(filepath.Join(dir, "sharma.yaml"))
	if err != nil || one.ID != "sharma" {
		t.Errorf("LoadProfile(sharma) = %q, %v", one.ID, err)
	}

	rulesPath := filepath.Join("..", "..", "config", "rules.yaml")
	r, err := LoadRules(rulesPath)
	if err != nil {
		t.Fatalf("LoadRules: %v", err)
	}
	cr, err := company.LoadRules(rulesPath)
	if err != nil || !reflect.DeepEqual(r, cr) {
		t.Errorf("seed.LoadRules differs from company.LoadRules (err %v)", err)
	}

	if DayOfMonth(3) != company.DayOfMonth(3) {
		t.Error("DayOfMonth differs from company's")
	}
	if Fixed(5) != company.Fixed(5) || Between(1, 9) != company.Between(1, 9) {
		t.Error("Fixed or Between differs from company's")
	}
	if m, err := ParseMonth("2026-09"); err != nil || m.Format("2006-01-02") != "2026-09-01" {
		t.Errorf("ParseMonth(2026-09) = %v, %v", m, err)
	}
	if !errors.Is(ErrUnregistered, company.ErrUnregistered) {
		t.Error("ErrUnregistered is not company.ErrUnregistered")
	}
}

func TestDerefNil(t *testing.T) {
	if deref(nil) != (Range{}) {
		t.Error("deref(nil) is not the zero Range")
	}
	r := Between(3, 4)
	if deref(&r) != r {
		t.Error("deref(&r) != r")
	}
}

// supplierKinds lists every supplier kind, in the order company validates
// them; the bootstrap tests range over it.
var supplierKinds = []string{KindRent, KindUtility, KindSubscription, KindGoods, KindServices}

func TestSupplierKindsAreCompanyKinds(t *testing.T) {
	want := []string{company.KindRent, company.KindUtility, company.KindSubscription, company.KindGoods, company.KindServices}
	if !slices.Equal(supplierKinds, want) {
		t.Errorf("supplierKinds = %v, want %v", supplierKinds, want)
	}
}

// ERPAccount (bootstrap.go) and company.ERPAccount must name accounts the
// same way: the checks find the bank account with the company one.
func TestERPAccountMatchesCompany(t *testing.T) {
	for _, tc := range [][2]string{{"HDFC Current 0001", "STPL"}, {"Creditors", "MTSPL"}} {
		if got, want := ERPAccount(tc[0], tc[1]), company.ERPAccount(tc[0], tc[1]); got != want {
			t.Errorf("ERPAccount(%q, %q) = %q, company says %q", tc[0], tc[1], got, want)
		}
	}
}
