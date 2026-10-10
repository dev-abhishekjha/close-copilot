package seed

import (
	"testing"

	"github.com/abhishekjha/close-copilot/internal/company"
)

// The PAN and GSTIN tests live in internal/company with the code
// (CC-601a). This checks that the seeder's forwarders reach it.

func TestGSTINForwarders(t *testing.T) {
	pan := PAN(42, "acme", EntityCompany)
	if want := company.PAN(42, "acme", company.EntityCompany); pan != want {
		t.Fatalf("PAN = %q, company says %q", pan, want)
	}
	if err := ValidPAN(pan); err != nil {
		t.Fatalf("ValidPAN(%q): %v", pan, err)
	}
	g, err := GSTIN("29", pan)
	if err != nil {
		t.Fatalf("GSTIN: %v", err)
	}
	if want, _ := company.GSTIN("29", pan); g != want {
		t.Errorf("GSTIN = %q, company says %q", g, want)
	}
	if err := ValidGSTIN(g); err != nil {
		t.Errorf("ValidGSTIN(%q): %v", g, err)
	}
	if c, err := CheckDigit(g[:14]); err != nil || c != g[14] {
		t.Errorf("CheckDigit(%q) = %q, %v; want %q", g[:14], c, err, g[14])
	}
	if !ValidStateCode("29") || ValidStateCode("39") {
		t.Error("ValidStateCode disagrees with company")
	}
	if !isDigit('7') || isDigit('x') {
		t.Error("isDigit")
	}
}
