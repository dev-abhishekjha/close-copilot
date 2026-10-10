package fakeerp

import (
	"testing"

	"github.com/abhishekjha/close-copilot/internal/company"
)

func TestStatements(t *testing.T) {
	sk, err := SkeletonStatement()
	if err != nil {
		t.Fatal(err)
	}
	if want := 2 + CashSales + len(PlantedCharges); len(sk) != want {
		t.Errorf("skeleton statement %d lines, want %d", len(sk), want)
	}
	par, err := ParityStatement()
	if err != nil {
		t.Fatal(err)
	}
	if want := 3 + CashSales - 10 + len(PlantedCharges); len(par) != want {
		t.Errorf("parity statement %d lines, want %d", len(par), want)
	}
	seen := map[string]bool{}
	for _, l := range sk {
		if seen[l.TxnID] {
			t.Errorf("duplicate txn %s", l.TxnID)
		}
		seen[l.TxnID] = true
		if l.CompanyID != CompanyID || l.TxnDate.Format("2006-01") != Month {
			t.Errorf("line %s outside the company or month: %+v", l.TxnID, l)
		}
	}
	for _, c := range PlantedCharges {
		if !seen[c.TxnID] {
			t.Errorf("planted charge %s missing", c.TxnID)
		}
	}
	if _, err := Statement(StatementOptions{OmitLastCashSales: CashSales + 1}); err == nil {
		t.Error("omitting more cash sales than exist: want an error")
	}
}

// TestBankAccountMatchesProfile keeps the fake's bank account in step with
// the sharma profile the agent derives it from.
func TestBankAccountMatchesProfile(t *testing.T) {
	p, err := company.LoadProfile("../../../config/companies/sharma.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if got := company.ERPAccount(p.Bank.Account, p.Abbr); got != BankAccount || p.ID != CompanyID || p.ERPCompany != ERPCompany {
		t.Errorf("profile %s/%s/%s, fake %s/%s/%s", p.ID, p.ERPCompany, got, CompanyID, ERPCompany, BankAccount)
	}
}
