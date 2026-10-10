package agent

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/abhishekjha/close-copilot/internal/money"
)

func TestExtractAmounts(t *testing.T) {
	tests := []struct {
		name string
		text string
		want []money.Paise
		bad  []string
	}{
		{"no amounts", "The books have no matching entry.", nil, nil},
		{"rupee sign", "The bank debited ₹590 on 15 Sep.", []money.Paise{59000}, nil},
		{"rupee sign with space and decimals", "Charge of ₹ 5.90 booked.", []money.Paise{590}, nil},
		{"one decimal", "₹1,180.5 is due.", []money.Paise{118050}, nil},
		{"Indian grouping", "A fee of ₹1,18,000 was charged.", []money.Paise{11800000}, nil},
		{"Indian grouping, large", "₹12,34,56,789.01 in total", []money.Paise{123456789*100 + 1}, nil},
		{"Rs. without space", "Rs.1,180.50 debited", []money.Paise{118050}, nil},
		{"Rs without space", "Rs590 debited", []money.Paise{59000}, nil},
		{"Rs with space", "Rs 590 debited", []money.Paise{59000}, nil},
		{"RS upper case", "RS. 17.70 debited", []money.Paise{1770}, nil},
		{"INR prefix without comma", "INR 1180 debited", []money.Paise{118000}, nil},
		{"INR without space", "INR1180", []money.Paise{118000}, nil},
		{"bare western grouping", "a total of 118,000.50 was paid", []money.Paise{11800050}, nil},
		{"bare Indian grouping", "1,18,000 left", []money.Paise{11800000}, nil},
		{"trailing period", "The bank charged ₹1,180.", []money.Paise{118000}, nil},
		{"several amounts", "₹5.90 and ₹17.70, total Rs.23.60; balance 2,44,098.23.", []money.Paise{590, 1770, 2360, 24409823}, nil},
		{"negative amounts read as absolute", "₹-590 and -1,180.00", []money.Paise{59000, 118000}, nil},
		{"three decimals unparseable", "₹1,180.505 is odd", nil, []string{"1,180.505"}},
		{"bare three decimals unparseable", "about 1,180.505", nil, []string{"1,180.505"}},
		{"bad grouping after prefix unparseable", "₹1,1800", nil, []string{"1,1800"}},
		{"bad bare grouping is not an amount", "items 1,2,3 and 12,34", nil, nil},
		{"ISO date", "posted on 2026-08-31", nil, nil},
		{"slash date", "dated 31/08/2026", nil, nil},
		{"month", "for 2026-09", nil, nil},
		{"GSTIN", "supplier 27ABCDE1234F1Z5 billed", nil, nil},
		{"plain integers", "15 Sep 2026, 3 charges, txn 590", nil, nil},
		{"date with comma is not an amount", "Sep 15, 2026", nil, nil},
		{"word before Rs is not a prefix", "hrs 590 and pairs 12", nil, nil},
		{"code with a grouped number", "ref A1,234 and B-1,234", nil, nil},
		{"minus sign after a space still read", "net -1,234 and (-2,000)", []money.Paise{123400, 200000}, nil},
		{"rupee sign then line breaks and a date", "₹\n\n31/08/2026", nil, nil},
		{"rupee sign then a line break and an amount", "total ₹\n5,000", []money.Paise{500000}, nil},
		{"INR then an ISO date", "INR 2026-08-31", nil, nil},
		{"Rs then a slash date", "Rs 31/08/2026", nil, nil},
		{"bare grouped number before a hyphen and a digit", "1,234-5", []money.Paise{123400, 500}, nil},
		{"prefixed range", "₹500-600", []money.Paise{50000, 60000}, nil},
		{"prefixed grouped range", "₹5,000-6,000", []money.Paise{500000, 600000}, nil},
		{"bare range", "5,000-6,000", []money.Paise{500000, 600000}, nil},
		{"bare range in a sentence", "charges of 5,000-6,000 each", []money.Paise{500000, 600000}, nil},
		{"Rs range to a single digit", "Rs 9,87,654-1", []money.Paise{98765400, 100}, nil},
		{"slash and a plain digit", "₹9,87,654/2", []money.Paise{98765400, 200}, nil},
		{"prefixed slash and an ungrouped number", "₹500/600", []money.Paise{50000, 60000}, nil},
		{"bare range to an ungrouped number", "5,000-6000", []money.Paise{500000, 600000}, nil},
		{"bare range to an ungrouped number in a sentence", "fees of 5,000-6000 each", []money.Paise{500000, 600000}, nil},
		{"bare slash and an ungrouped number is not read", "5,000/6000", []money.Paise{500000}, nil},
		{"prefixed range, en dash", "₹500–600", []money.Paise{50000, 60000}, nil},
		{"prefixed range, em dash", "₹500—600", []money.Paise{50000, 60000}, nil},
		{"prefixed range, hyphen", "₹500‐600", []money.Paise{50000, 60000}, nil},
		{"prefixed range, non-breaking hyphen", "₹500‑600", []money.Paise{50000, 60000}, nil},
		{"prefixed range, figure dash", "₹500‒600", []money.Paise{50000, 60000}, nil},
		{"prefixed range, minus sign", "₹500−600", []money.Paise{50000, 60000}, nil},
		{"prefixed range, spaced hyphen", "₹500 - 600", []money.Paise{50000, 60000}, nil},
		{"prefixed range, spaced en dash", "₹500 – 600 debited", []money.Paise{50000, 60000}, nil},
		{"prefixed range, no-break spaces and an em dash", "Rs.500 — 600", []money.Paise{50000, 60000}, nil},
		{"prefixed range, to", "₹500 to 600", []money.Paise{50000, 60000}, nil},
		{"prefixed range, TO without spaces", "INR 500TO600", []money.Paise{50000, 60000}, nil},
		{"prefixed range, to and a decimal", "Rs 1,180 to 1180.50", []money.Paise{118000, 118050}, nil},
		{"prefixed range, en dash and bad grouping", "₹500 – 1,1800", []money.Paise{50000}, []string{"1,1800"}},
		{"a word starting with to is not a range", "₹500 total 600", []money.Paise{50000}, nil},
		{"a line break before the dash is not a range", "₹500\n- 600", []money.Paise{50000}, nil},
		{"a date after to is not the other end", "₹590 to 2026-09-30", []money.Paise{59000}, nil},
		{"a slash date after a spaced dash is not the other end", "₹590 – 30/09/2026", []money.Paise{59000}, nil},
		{"not a date: digits after the day", "₹5000-10-2000", []money.Paise{500000, 1000, 200000}, nil},
		{"not a date: no four-digit year", "₹1-2-3", []money.Paise{100, 200, 300}, nil},
		{"not a date: day-first month 13", "Rs 12/13/2026", []money.Paise{1200, 1300, 202600}, nil},
		{"bare decimal without a comma or prefix is not read", "a fee of 1180.50 paid", nil, nil},
		{"slash and a grouped number", "₹9,87,654/1,234", []money.Paise{98765400, 123400}, nil},
		{"rupee sign and slash-dash", "₹9,87,654/-", []money.Paise{98765400}, nil},
		{"Rs. and slash-dash", "Rs. 5,000/- paid", []money.Paise{500000}, nil},
		{"bare slash-dash", "a fee of 5,000/- paid", []money.Paise{500000}, nil},
		{"Rs then a day-first hyphen date", "Rs 31-08-2026", nil, nil},
		{"rupee sign then an ISO date", "₹2026-08-31 posting", nil, nil},
		{"code range is not read", "BT-0001,0002-0003,0004", nil, nil},
		{"years list", "in 2025,2026", nil, nil},
		{"account codes list", "accounts 1110,1120", nil, nil},
		{"hyphenated code list", "BT-0001,0002", nil, nil},
		{"four-digit groups with decimals still an amount", "about 1234,5678.50", nil, []string{"1234,5678.50"}},
		{"words are not extracted", "one lakh eighteen thousand, 1.18 lakh", nil, nil},
		{"rupee sign and a no-break space", "₹\u00a05000 debited", []money.Paise{500000}, nil},
		{"rupee sign and a narrow no-break space", "₹\u202f5000 debited", []money.Paise{500000}, nil},
		{"rupee sign and two spaces", "₹  5000 debited", []money.Paise{500000}, nil},
		{"Rs. and two spaces", "Rs.  5000 debited", []money.Paise{500000}, nil},
		{"INR with a space", "INR 5000 debited", []money.Paise{500000}, nil},
		{"INR, a no-break space and a tab", "INR\u00a0\t5,000 debited", []money.Paise{500000}, nil},
		{"bad bare Indian grouping unparseable", "a total of 9,87,6543 was paid", nil, []string{"9,87,6543"}},
		{"bad bare grouping with decimals unparseable", "about 12,34.50", nil, []string{"12,34.50"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, bad := extractAmounts(tt.text)
			if !slices.Equal(got, tt.want) || !slices.Equal(bad, tt.bad) {
				t.Errorf("extractAmounts(%q) = %v, bad %q; want %v, bad %q", tt.text, got, bad, tt.want, tt.bad)
			}
		})
	}
}

func TestGroundSet(t *testing.T) {
	finding := money.Paise(59000)
	g := newGroundSet(evAll(-11800000, -118050, 250000000, 118050), &finding)
	for _, tt := range []struct {
		c  money.Paise
		ok bool
	}{
		{11800000, true},             // exact
		{118050, true},               // exact, duplicate
		{118150, true},               // +100 paise: the tolerance edge
		{117950, true},               // -100 paise
		{118151, false},              // 101 paise off
		{117949, false},              // 101 paise off
		{59000, true},                // the finding's amount
		{59100, true},                // the finding's amount + 100
		{59101, false},               // 101 off the finding's amount
		{11800000 + 118050, true},    // a sum
		{11800000 - 118050, true},    // a difference
		{2 * 118050, true},           // the sum of two records with the same amount
		{250000000 + 11800000, true}, // a sum with the balance
		{250000000 + 11800000 + 101, false},
		{-11800000, true}, // compared by absolute value
		{1 << 62, false},  // beyond the groundable range
		{98765432, false}, // invented
	} {
		if got := g.grounded(tt.c); got != tt.ok {
			t.Errorf("grounded(%d) = %v, want %v", tt.c, got, tt.ok)
		}
	}
	// A single record's amount doesn't pair with itself.
	one := newGroundSet(evAll(590), nil)
	if one.grounded(1180) || one.grounded(0) {
		t.Error("a record paired with itself")
	}
	if !one.grounded(590) || one.grounded(691) {
		t.Error("single amount")
	}
	// Zero leaves ground nothing: not 50 paise on their own, and not as
	// one side of a pair.
	zeros := newGroundSet(evAll(0, 0, -0), nil)
	if zeros.grounded(50) || zeros.grounded(0) || zeros.grounded(100) || zeros.groundedProposal(50) {
		t.Error("a zero leaf grounded an amount")
	}
	zeroPair := newGroundSet(evAll(0, 590), nil)
	if len(zeroPair.singles) != 1 || len(zeroPair.pairable) != 1 || !zeroPair.grounded(590) || zeroPair.grounded(50) {
		t.Errorf("zero leaf kept: %+v", zeroPair)
	}
	zeroFinding := money.Paise(0)
	if newGroundSet(evAll(), &zeroFinding).grounded(50) {
		t.Error("a zero finding amount grounded 50 paise")
	}
	// Above the cap only single amounts count.
	many := make([]money.Paise, maxEvidenceAmounts+1)
	for i := range many {
		many[i] = money.Paise(1000 * (i + 1))
	}
	big := newGroundSet(evAll(many...), nil)
	if big.pairs || !big.grounded(5000) || big.grounded(5000+maxEvidenceAmounts*1000+1000+500) {
		t.Errorf("capped set: pairs %v", big.pairs)
	}
}

func TestMoneyLeaves(t *testing.T) {
	var v any
	dec := json.NewDecoder(strings.NewReader(`{"txn_id":"C1","amount_paise":-590,"balance":100,"count":7,"day":15,
		"nested":{"Debit":200,"months":3},"items":[{"Amount":300}],"amounts_paise":[1,2],"note":"₹999","rate":"18"}`))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}
	var got evidenceAmounts
	moneyLeaves(v, &got)
	slices.Sort(got.singles)
	slices.Sort(got.pairable)
	if want := []money.Paise{-590, 1, 2, 100, 200, 300}; !slices.Equal(got.singles, want) {
		t.Errorf("money leaves %v, want %v", got.singles, want)
	}
	// balance is balance-type: single only.
	if want := []money.Paise{-590, 1, 2, 200, 300}; !slices.Equal(got.pairable, want) {
		t.Errorf("pairable leaves %v, want %v", got.pairable, want)
	}
}

// evAll is evidence whose every amount is pairable (no balance keys).
func evAll(ps ...money.Paise) evidenceAmounts {
	return evidenceAmounts{singles: ps, pairable: slices.Clone(ps)}
}

func TestIsBalanceKey(t *testing.T) {
	for k, want := range map[string]bool{
		"balance": true, "balance_paise": true, "opening": true, "closing": true, "closing_balance_paise": true,
		"OpeningBalance": true, "running_total_paise": true, "cumulative_paise": true,
		"outstanding_amount": true, "OutstandingAmount": true, "outstanding_paise": true,
		"total_debit": true, "total_credit": true, "TotalDebit": true, "TotalCredit": true,
		"totaldebit": true, "TOTAL_CREDIT": true, "total_debit_paise": true,
		"amount": false, "amount_paise": false, "debit": false, "credit": false, "net": false, "igst": false,
		"grand_total": false, "median_amount": false,
	} {
		if got := isBalanceKey(k); got != want {
			t.Errorf("isBalanceKey(%q) = %v, want %v", k, got, want)
		}
	}
}

// TestGroundSetBalances: balance-type amounts ground on their own but never
// in a sum or difference.
func TestGroundSetBalances(t *testing.T) {
	var ev evidenceAmounts
	var v any
	dec := json.NewDecoder(strings.NewReader(`[
		{"txn_id":"C1","amount_paise":-590,"balance_paise":10000000},
		{"txn_id":"C2","amount_paise":-1770,"balance_paise":9500000}]`))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}
	moneyLeaves(v, &ev)
	g := newGroundSet(ev, nil)
	for _, tt := range []struct {
		c  money.Paise
		ok bool
	}{
		{10000000, true},        // a balance on its own
		{9500000, true},         // a balance on its own
		{500000, false},         // the difference of two balances
		{19500000, false},       // the sum of two balances
		{10000000 + 590, false}, // a balance plus an amount
		{590 + 1770, true},      // two amounts
		{1770 - 590, true},      // two amounts
	} {
		if got := g.grounded(tt.c); got != tt.ok {
			t.Errorf("grounded(%d) = %v, want %v", tt.c, got, tt.ok)
		}
	}
	if g.groundedSingle(590+1770) || !g.groundedSingle(1770) || !g.groundedSingle(9500000) {
		t.Error("groundedSingle accepted a pair or rejected a single")
	}
	// A proposal never posts a balance, alone or in a pair.
	if g.groundedProposalSingle(9500000) || g.groundedProposal(10000000) || g.groundedProposal(500000) {
		t.Error("a balance grounded a proposal amount")
	}
	if !g.groundedProposalSingle(1770) || !g.groundedProposal(590+1770) || g.groundedProposalSingle(590+1770) {
		t.Error("proposal grounding of plain amounts")
	}
}

// TestGroundSetOutstanding: outstanding amounts and total debits or
// credits are balance-type: they ground text on their own, never pair and
// never ground a proposal.
func TestGroundSetOutstanding(t *testing.T) {
	var ev evidenceAmounts
	var v any
	dec := json.NewDecoder(strings.NewReader(`[
		{"name":"PINV-1","grand_total":118000,"outstanding_amount":50000},
		{"name":"PINV-2","grand_total":59000,"outstanding_amount":20000,"total_debit":700000,"TotalCredit":800000}]`))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}
	moneyLeaves(v, &ev)
	g := newGroundSet(ev, nil)
	for _, tt := range []struct {
		c  money.Paise
		ok bool
	}{
		{50000, true},          // outstanding on its own, in text
		{700000, true},         // total_debit on its own, in text
		{50000 + 20000, false}, // two outstanding amounts don't pair
		{50000 - 20000, false},
		{118000 - 50000, false}, // grand total minus outstanding doesn't pair
		{800000 - 700000, false},
		{118000 + 59000, true}, // two grand totals pair
	} {
		if got := g.grounded(tt.c); got != tt.ok {
			t.Errorf("grounded(%d) = %v, want %v", tt.c, got, tt.ok)
		}
	}
	if g.groundedProposal(50000) || g.groundedProposal(700000) || g.groundedProposalSingle(800000) {
		t.Error("a balance-type amount grounded a proposal amount")
	}
}

// TestMoneyKeysCoverPaiseFields fails when a money.Paise field of a record
// type a snapshot can hold (internal/ledger, internal/evidence,
// internal/store) has a JSON name the verifier would not read as money.
func TestMoneyKeysCoverPaiseFields(t *testing.T) {
	checked := 0
	for _, dir := range []string{"../ledger", "../evidence", "../store", "../checks"} {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range files {
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(src, []byte("money.Paise")) {
				continue
			}
			f, err := parser.ParseFile(token.NewFileSet(), path, src, 0)
			if err != nil {
				t.Fatal(err)
			}
			ast.Inspect(f, func(n ast.Node) bool {
				st, ok := n.(*ast.StructType)
				if !ok {
					return true
				}
				for _, field := range st.Fields.List {
					if !isPaiseType(field.Type) {
						continue
					}
					for _, name := range field.Names {
						if !name.IsExported() {
							continue
						}
						key := jsonName(name.Name, field.Tag)
						if key == "-" {
							continue
						}
						checked++
						if !isMoneyKey(key) {
							t.Errorf("%s: money.Paise field %s has JSON name %q, missing from moneyKeys", path, name.Name, key)
						}
					}
				}
				return true
			})
		}
	}
	if checked < 30 {
		t.Fatalf("checked only %d money.Paise fields; the scan is broken", checked)
	}
}

// isPaiseType reports whether a field type is money.Paise, a pointer to it
// or a slice of it.
func isPaiseType(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.StarExpr:
		return isPaiseType(x.X)
	case *ast.ArrayType:
		return isPaiseType(x.Elt)
	case *ast.SelectorExpr:
		id, ok := x.X.(*ast.Ident)
		return ok && id.Name == "money" && x.Sel.Name == "Paise"
	}
	return false
}

// jsonName is the field's JSON key: the json tag's name, or the Go name.
func jsonName(goName string, tag *ast.BasicLit) string {
	if tag == nil {
		return goName
	}
	raw, err := strconv.Unquote(tag.Value)
	if err != nil {
		return goName
	}
	name, _, _ := strings.Cut(reflect.StructTag(raw).Get("json"), ",")
	if name == "" {
		return goName
	}
	return name
}
