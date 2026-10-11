package checks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/abhishekjha/close-copilot/internal/ledger"
	"github.com/abhishekjha/close-copilot/internal/money"
	"github.com/abhishekjha/close-copilot/internal/store"
)

type gstrBooks struct {
	dupBooks
}

type gstrEvidence struct {
	byPeriod map[string][]store.GSTR2BEntry
	errOn    string
}

func (f *gstrEvidence) BankLines(context.Context, string, time.Time, time.Time) ([]store.BankLine, error) {
	return nil, nil
}
func (f *gstrEvidence) GSTR2BEntries(_ context.Context, _ string, period string) ([]store.GSTR2BEntry, error) {
	if period == f.errOn {
		return nil, errors.New("boom")
	}
	return f.byPeriod[period], nil
}

const (
	gM    = "2026-09"
	gPrev = "2026-08"
	gNext = "2026-10"
	gstA  = "27AABCX1111E1ZX"
	gstB  = "29AABCY2222F1ZY"
)

func gDay(m time.Month, d int) time.Time { return time.Date(2026, m, d, 0, 0, 0, 0, time.UTC) }

func gInv(name, gstin, bill string, billDate time.Time, cgst, sgst int64) ledger.PurchaseInvoice {
	return ledger.PurchaseInvoice{
		Name: name, Supplier: "S", SupplierName: "Supplier S", BillNo: bill, BillDate: billDate,
		PostingDate: billDate, SupplierGSTIN: gstin,
		Taxes: []ledger.PurchaseTaxesAndCharges{
			{AccountHead: "Input Tax CGST - T", TaxAmount: money.Paise(cgst), AddDeductTax: "Add"},
			{AccountHead: "Input Tax SGST - T", TaxAmount: money.Paise(sgst), AddDeductTax: "Add"},
		},
	}
}

func gEnt(period, gstin, no string, cgst, sgst int64, itc bool) store.GSTR2BEntry {
	return store.GSTR2BEntry{
		Period: period, SupplierGSTIN: gstin, InvoiceNo: no, InvoiceNoNorm: normForTest(no),
		CGSTPaise: money.Paise(cgst), SGSTPaise: money.Paise(sgst), ITCAvailable: itc,
	}
}

func normForTest(s string) string {
	// Test entries use already-normalised numbers except the normalisation row.
	if s == "CLD-2026-912" {
		return "CLD2026912"
	}
	return s
}

func runG(t *testing.T, invs []ledger.PurchaseInvoice, ev map[string][]store.GSTR2BEntry, tol int64) ([]Finding, error) {
	t.Helper()
	in := Inputs{
		Company: "C", Month: gM,
		Books:    &gstrBooks{dupBooks{invoices: invs}},
		Evidence: &gstrEvidence{byPeriod: ev},
	}
	in.Rules.GSTR2B.AmountTolerancePaise = tol
	return (&GSTR2BCheck{}).Run(context.Background(), in)
}

func TestGSTR2BBuckets(t *testing.T) {
	d := gDay(time.September, 10)
	tests := []struct {
		name     string
		invs     []ledger.PurchaseInvoice
		ev       map[string][]store.GSTR2BEntry
		wantType string // "" means no finding
		wantNorm string
		wantAmt  int64
	}{
		{"matched", []ledger.PurchaseInvoice{gInv("PI1", gstA, "A1", d, 900, 900)},
			map[string][]store.GSTR2BEntry{gM: {gEnt(gM, gstA, "A1", 900, 900, true)}}, "", "", 0},
		{"mismatch beyond tolerance", []ledger.PurchaseInvoice{gInv("PI1", gstA, "A1", d, 1100, 1100)},
			map[string][]store.GSTR2BEntry{gM: {gEnt(gM, gstA, "A1", 900, 900, true)}}, TypeGSTR2BAmountMismatch, "A1", 400},
		{"exactly at tolerance", []ledger.PurchaseInvoice{gInv("PI1", gstA, "A1", d, 1000, 1000)},
			map[string][]store.GSTR2BEntry{gM: {gEnt(gM, gstA, "A1", 900, 1000, true)}}, "", "", 0},
		{"one paisa above tolerance", []ledger.PurchaseInvoice{gInv("PI1", gstA, "A1", d, 1001, 1000)},
			map[string][]store.GSTR2BEntry{gM: {gEnt(gM, gstA, "A1", 900, 1000, true)}}, TypeGSTR2BAmountMismatch, "A1", 101},
		{"missing in 2b", []ledger.PurchaseInvoice{gInv("PI1", gstA, "A1", d, 900, 900)},
			nil, TypeGSTR2BMissingIn2B, "A1", 1800},
		{"wrong period via next", []ledger.PurchaseInvoice{gInv("PI1", gstA, "A1", d, 900, 900)},
			map[string][]store.GSTR2BEntry{gNext: {gEnt(gNext, gstA, "A1", 900, 900, true)}}, TypeGSTR2BWrongPeriod, "A1", 1800},
		{"wrong period via prev", []ledger.PurchaseInvoice{gInv("PI1", gstA, "A1", d, 900, 900)},
			map[string][]store.GSTR2BEntry{gPrev: {gEnt(gPrev, gstA, "A1", 900, 900, true)}}, TypeGSTR2BWrongPeriod, "A1", 1800},
		{"wrong period not eligible there", []ledger.PurchaseInvoice{gInv("PI1", gstA, "A1", d, 900, 900)},
			map[string][]store.GSTR2BEntry{gNext: {gEnt(gNext, gstA, "A1", 900, 900, false)}}, TypeGSTR2BWrongPeriod, "A1", 1800},
		{"missing in books", nil,
			map[string][]store.GSTR2BEntry{gM: {gEnt(gM, gstA, "B9", 500, 500, true)}}, TypeGSTR2BMissingInBooks, "B9", 1000},
		{"2b only but in prior month books", []ledger.PurchaseInvoice{gInv("PI1", gstA, "B9", gDay(time.August, 30), 500, 500)},
			map[string][]store.GSTR2BEntry{gM: {gEnt(gM, gstA, "B9", 500, 500, true)}}, "", "", 0},
		{"2b only but in next month books", []ledger.PurchaseInvoice{gInv("PI1", gstA, "B9", gDay(time.October, 2), 500, 500)},
			map[string][]store.GSTR2BEntry{gM: {gEnt(gM, gstA, "B9", 500, 500, true)}}, "", "", 0},
		{"2b only not eligible", nil,
			map[string][]store.GSTR2BEntry{gM: {gEnt(gM, gstA, "B9", 500, 500, false)}}, "", "", 0},
		{"not eligible", []ledger.PurchaseInvoice{gInv("PI1", gstA, "A1", d, 900, 900)},
			map[string][]store.GSTR2BEntry{gM: {gEnt(gM, gstA, "A1", 900, 900, false)}}, TypeGSTR2BNotEligible, "A1", 1800},
		{"unregistered supplier skipped", []ledger.PurchaseInvoice{gInv("PI1", "", "A1", d, 900, 900)}, nil, "", "", 0},
		{"return skipped", []ledger.PurchaseInvoice{func() ledger.PurchaseInvoice {
			i := gInv("PI1", gstA, "A1", d, 900, 900)
			i.IsReturn = true
			return i
		}()}, nil, "", "", 0},
		{"bill date outside month skipped", []ledger.PurchaseInvoice{gInv("PI1", gstA, "A1", gDay(time.July, 31), 900, 900)}, nil, "", "", 0},
		{"bill date in month, later posting date", []ledger.PurchaseInvoice{func() ledger.PurchaseInvoice {
			i := gInv("PI1", gstA, "A1", gDay(time.September, 29), 900, 900)
			i.PostingDate = gDay(time.October, 3)
			return i
		}()}, nil, TypeGSTR2BMissingIn2B, "A1", 1800},
		{"zero bill date falls back to posting date", []ledger.PurchaseInvoice{func() ledger.PurchaseInvoice {
			i := gInv("PI1", gstA, "A1", time.Time{}, 900, 900)
			i.PostingDate = d
			return i
		}()}, nil, TypeGSTR2BMissingIn2B, "A1", 1800},
		{"normalisation matched", []ledger.PurchaseInvoice{gInv("PI1", gstA, "CLD/2026/0912", d, 900, 900)},
			map[string][]store.GSTR2BEntry{gM: {gEnt(gM, gstA, "CLD-2026-912", 900, 900, true)}}, "", "", 0},
		{"normalised key in finding", []ledger.PurchaseInvoice{gInv("PI1", gstA, "CLD/2026/0912", d, 900, 900)},
			nil, TypeGSTR2BMissingIn2B, "CLD2026912", 1800},
		{"clean month", nil, nil, "", "", 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := runG(t, tc.invs, tc.ev, 100)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantType == "" {
				if len(got) != 0 {
					t.Fatalf("want no findings, got %+v", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("want 1 finding, got %d: %+v", len(got), got)
			}
			f := got[0]
			if f.Type != tc.wantType || f.Keys["invoice_no_norm"] != tc.wantNorm || f.Keys["supplier_gstin"] != gstA {
				t.Errorf("got type %s keys %v", f.Type, f.Keys)
			}
			if f.AmountPaise == nil || int64(*f.AmountPaise) != tc.wantAmt {
				t.Errorf("amount = %v, want %d", f.AmountPaise, tc.wantAmt)
			}
			if f.Status != StatusOpen || f.Action == nil {
				t.Errorf("status/action not set: %+v", f)
			}
		})
	}
}

func TestGSTR2BTaxFromHeads(t *testing.T) {
	inv := ledger.PurchaseInvoice{
		Name: "PI1", BillNo: "A1", BillDate: gDay(time.September, 3), SupplierGSTIN: gstA,
		Taxes: []ledger.PurchaseTaxesAndCharges{
			{AccountHead: "Anything", GSTTaxType: "IGST", TaxAmount: 1000, AddDeductTax: "Add"},
			{AccountHead: "Input Tax CGST - T", TaxAmount: 300, AddDeductTax: "Add"},
			{AccountHead: "Input Tax SGST - T", TaxAmount: 50, AddDeductTax: "Deduct"},
			{AccountHead: "Input Cess - T", TaxAmount: 777, AddDeductTax: "Add"},
		},
	}
	got, err := runG(t, []ledger.PurchaseInvoice{inv}, nil, 100)
	if err != nil || len(got) != 1 {
		t.Fatalf("got %v, %v", got, err)
	}
	if *got[0].AmountPaise != 1250 {
		t.Errorf("amount = %d, want 1250", *got[0].AmountPaise)
	}
	raw := got[0].Evidence[0].Args
	var any map[string]json.RawMessage
	if err := json.Unmarshal(raw, &any); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{"books_tax_paise": "1250", "igst_paise": "1000", "cgst_paise": "300", "sgst_paise": "-50"} {
		if string(any[k]) != want {
			t.Errorf("evidence %s = %s, want %s", k, any[k], want)
		}
	}
}

func TestGSTR2BSplitMismatch(t *testing.T) {
	inv := gInv("PI1", gstA, "A1", gDay(time.September, 3), 900, 900)
	e := gEnt(gM, gstA, "A1", 0, 0, true)
	e.IGSTPaise = 1800
	got, err := runG(t, []ledger.PurchaseInvoice{inv}, map[string][]store.GSTR2BEntry{gM: {e}}, 100)
	if err != nil || len(got) != 1 || got[0].Type != TypeGSTR2BAmountMismatch {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestGSTR2BDuplicateKey(t *testing.T) {
	d := gDay(time.September, 3)
	invs := []ledger.PurchaseInvoice{gInv("PI2", gstA, "A-1", d, 100, 100), gInv("PI1", gstA, "A1", d, 900, 900)}
	got, err := runG(t, invs, nil, 100)
	if err != nil || len(got) != 1 {
		t.Fatalf("got %v, %v", got, err)
	}
	if *got[0].AmountPaise != 1800 {
		t.Errorf("amount = %d, want first-by-name PI1 (1800)", *got[0].AmountPaise)
	}
	if ids := got[0].Evidence[0].IDs; len(ids) != 2 || ids[0] != "PI1" || ids[1] != "PI2" {
		t.Errorf("ids = %v", ids)
	}
}

func TestGSTR2BEvidenceBothSides(t *testing.T) {
	inv := gInv("PI1", gstA, "A1", gDay(time.September, 3), 1100, 1100)
	got, err := runG(t, []ledger.PurchaseInvoice{inv}, map[string][]store.GSTR2BEntry{gM: {gEnt(gM, gstA, "A1", 900, 900, true)}}, 100)
	if err != nil || len(got) != 1 || len(got[0].Evidence) != 2 {
		t.Fatalf("got %v, %v", got, err)
	}
	var b, e map[string]any
	if json.Unmarshal(got[0].Evidence[0].Args, &b) != nil || json.Unmarshal(got[0].Evidence[1].Args, &e) != nil {
		t.Fatal("bad args")
	}
	if b["books_tax_paise"].(float64) != 2200 || e["gstr2b_tax_paise"].(float64) != 1800 {
		t.Errorf("books %v, 2b %v", b, e)
	}
	if got[0].Evidence[0].Tool != "list_purchase_invoices" || got[0].Evidence[1].Tool != "list_gstr2b_entries" {
		t.Errorf("tools: %s %s", got[0].Evidence[0].Tool, got[0].Evidence[1].Tool)
	}
}

func TestGSTR2BReaderErrors(t *testing.T) {
	d := gDay(time.September, 3)
	for _, tc := range []string{"books", gPrev, gM, gNext} {
		t.Run(tc, func(t *testing.T) {
			in := Inputs{Company: "C", Month: gM}
			sentinel := errors.New("boom")
			if tc == "books" {
				in.Books = &gstrBooks{dupBooks{invoicesErr: sentinel}}
				in.Evidence = &gstrEvidence{}
			} else {
				in.Books = &gstrBooks{dupBooks{invoices: []ledger.PurchaseInvoice{gInv("PI1", gstA, "A1", d, 1, 1)}}}
				in.Evidence = &gstrEvidence{errOn: tc}
			}
			_, err := (&GSTR2BCheck{}).Run(context.Background(), in)
			if err == nil || err.Error() == "" {
				t.Fatal("want error")
			}
			if tc == "books" && !errors.Is(err, sentinel) {
				t.Errorf("books error not wrapped: %v", err)
			}
		})
	}
	_, err := (&GSTR2BCheck{}).Run(context.Background(), Inputs{Month: "bad"})
	if err == nil {
		t.Error("want month error")
	}
}

func TestGSTR2BSixPlanted(t *testing.T) {
	d := gDay(time.September, 12)
	var invs []ledger.PurchaseInvoice
	var in2b []store.GSTR2BEntry
	for i := 0; i < 10; i++ {
		no := fmt.Sprintf("OK%d", i)
		invs = append(invs, gInv("PIOK"+no, gstB, no, d, 1000, 1000))
		in2b = append(in2b, gEnt(gM, gstB, no, 1000, 1000, true))
	}
	want := map[string]int64{}
	for i := 0; i < 2; i++ { // dropped from 2B
		no := fmt.Sprintf("DROP%d", i)
		invs = append(invs, gInv("PID"+no, gstA, no, d, 5000, 5000))
		want[TypeGSTR2BMissingIn2B+"/"+no] = 10000
	}
	for i := 0; i < 2; i++ { // tax cut by 10% in 2B
		no := fmt.Sprintf("CUT%d", i)
		invs = append(invs, gInv("PIC"+no, gstA, no, d, 5000, 5000))
		in2b = append(in2b, gEnt(gM, gstA, no, 4500, 4500, true))
		want[TypeGSTR2BAmountMismatch+"/"+no] = 1000
	}
	for i := 0; i < 2; i++ { // 2B only
		no := fmt.Sprintf("GHOST%d", i)
		in2b = append(in2b, gEnt(gM, gstA, no, 2000, 2000, true))
		want[TypeGSTR2BMissingInBooks+"/"+no] = 4000
	}
	invs = append(invs, gInv("PILATE", gstB, "LATE1", d, 700, 700))
	want[TypeGSTR2BWrongPeriod+"/LATE1"] = 1400
	ev := map[string][]store.GSTR2BEntry{gM: in2b, gNext: {gEnt(gNext, gstB, "LATE1", 700, 700, true)}}

	got, err := runG(t, invs, ev, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d findings, want %d: %+v", len(got), len(want), got)
	}
	for i, f := range got {
		k := f.Type + "/" + f.Keys["invoice_no_norm"]
		amt, ok := want[k]
		if !ok || int64(*f.AmountPaise) != amt {
			t.Errorf("unexpected finding %s amount %d", k, *f.AmountPaise)
		}
		if i > 0 && got[i-1].Type > f.Type {
			t.Errorf("not sorted by type at %d", i)
		}
	}
	// Following month: the late filer must not reappear as missing in books.
	in := Inputs{
		Company: "C", Month: gNext,
		Books:    &gstrBooks{dupBooks{invoices: invs}},
		Evidence: &gstrEvidence{byPeriod: ev},
	}
	nextFindings, err := (&GSTR2BCheck{}).Run(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range nextFindings {
		if f.Keys["invoice_no_norm"] == "LATE1" {
			t.Errorf("late filer reported again: %+v", f)
		}
	}
}
