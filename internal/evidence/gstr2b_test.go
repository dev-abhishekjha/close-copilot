package evidence_test

import (
	"strings"
	"testing"
	"time"

	"github.com/abhishekjha/close-copilot/internal/evidence"
	"github.com/abhishekjha/close-copilot/internal/money"
)

func gstr2bJSON(rtnprd, suppliers string) string {
	return `{"data":{"gstin":"29AAACS1234A1ZX","rtnprd":"` + rtnprd + `","docdata":{"b2b":[` + suppliers + `]}}}`
}

func inv(inum, dt, txval, itc string) string {
	return `{"inum":"` + inum + `","dt":"` + dt + `","val":0,"txval":` + txval + `,"igst":1.10,"cgst":0.05,"sgst":0.05,"itcavl":"` + itc + `"}`
}

func supplier(ctin string, invs ...string) string {
	return `{"ctin":"` + ctin + `","trdnm":"Cloudly SaaS","inv":[` + strings.Join(invs, ",") + `]}`
}

func TestParseGSTR2B(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantN   int
		wantErr string
	}{
		{"valid", gstr2bJSON("092026", supplier("27AABCC5678D1ZX", inv("CLD/0912", "12-09-2026", "120000.10", "Y"))), 1, ""},
		{"two suppliers", gstr2bJSON("092026",
			supplier("27AABCC5678D1ZX", inv("A/1", "01-09-2026", "10", "Y"), inv("A/2", "02-09-2026", "10", "N"))+","+
				supplier("29AABCC5678D1ZX", inv("A/1", "01-09-2026", "10", "Y"))), 3, ""},
		{"late filed prior month", gstr2bJSON("092026", supplier("27AABCC5678D1ZX", inv("L/1", "28-08-2026", "10", "Y"))), 1, ""},
		{"malformed date", gstr2bJSON("092026", supplier("27AABCC5678D1ZX", inv("A/1", "2026-09-01", "10", "Y"))), 0, "not dd-mm-yyyy"},
		{"bad itcavl", gstr2bJSON("092026", supplier("27AABCC5678D1ZX", inv("A/1", "01-09-2026", "10", "X"))), 0, "not Y or N"},
		{"wrong rtnprd", gstr2bJSON("102026", supplier("27AABCC5678D1ZX", inv("A/1", "01-09-2026", "10", "Y"))), 0, "want 2026-09"},
		{"bad rtnprd", gstr2bJSON("2026-09", supplier("27AABCC5678D1ZX", inv("A/1", "01-09-2026", "10", "Y"))), 0, "not MMYYYY"},
		{"missing ctin", gstr2bJSON("092026", supplier("", inv("A/1", "01-09-2026", "10", "Y"))), 0, "ctin is empty"},
		{"missing inum", gstr2bJSON("092026", supplier("27AABCC5678D1ZX", inv("", "01-09-2026", "10", "Y"))), 0, "inum is empty"},
		{"duplicate normalised", gstr2bJSON("092026", supplier("27AABCC5678D1ZX", inv("abc/101", "01-09-2026", "10", "Y"), inv("ABC-0101", "01-09-2026", "10", "Y"))), 0, "duplicate normalised"},
		{"bad amount", gstr2bJSON("092026", supplier("27AABCC5678D1ZX", inv("A/1", "01-09-2026", "1e400", "Y"))), 0, "txval"},
		{"unknown shape", `{"foo":1}`, 0, "unknown field"},
		{"missing docdata", `{"data":{"gstin":"x","rtnprd":"092026"}}`, 0, "missing data.docdata"},
		{"not json", `nope`, 0, "decode json"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := evidence.ParseGSTR2B(strings.NewReader(tc.in), "sharma", "2026-09")
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != tc.wantN {
				t.Fatalf("got %d entries, want %d", len(got), tc.wantN)
			}
		})
	}
}

func TestParseGSTR2BMapping(t *testing.T) {
	in := gstr2bJSON("092026", supplier("27AABCC5678D1ZX",
		inv("CLD/2026/0912", "12-09-2026", "120000.10", "N"),
		inv("L/1", "28-08-2026", "5", "Y")))
	got, err := evidence.ParseGSTR2B(strings.NewReader(in), "sharma", "2026-09")
	if err != nil {
		t.Fatal(err)
	}
	e := got[0]
	if e.CompanyID != "sharma" || e.Period != "2026-09" || e.SupplierGSTIN != "27AABCC5678D1ZX" ||
		e.InvoiceNo != "CLD/2026/0912" || e.InvoiceNoNorm != "CLD2026912" || e.ITCAvailable {
		t.Errorf("entry = %+v", e)
	}
	if e.SupplierName == nil || *e.SupplierName != "Cloudly SaaS" {
		t.Errorf("supplier name = %v", e.SupplierName)
	}
	if want := time.Date(2026, 9, 12, 0, 0, 0, 0, time.UTC); !e.InvoiceDate.Equal(want) {
		t.Errorf("date = %v, want %v", e.InvoiceDate, want)
	}
	if e.TaxablePaise != money.Paise(12000010) || e.IGSTPaise != 110 || e.CGSTPaise != 5 || e.SGSTPaise != 5 {
		t.Errorf("amounts = %d %d %d %d", e.TaxablePaise, e.IGSTPaise, e.CGSTPaise, e.SGSTPaise)
	}
	// Late-filed invoice dated in August keeps the return's period.
	if got[1].Period != "2026-09" || got[1].InvoiceDate.Month() != time.August || !got[1].ITCAvailable {
		t.Errorf("late entry = %+v", got[1])
	}
}
