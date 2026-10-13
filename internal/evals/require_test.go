package evals

import (
	"errors"
	"strings"
	"testing"
)

func TestRequireParse(t *testing.T) {
	tests := []struct {
		expr string
		want Requirement
	}{
		{"unrecorded_bank_charge.recall>=3/3", Requirement{Subject: bc, Metric: "recall", Op: ">=", Ratio: true, N: 3, M: 3}},
		{"overall.precision>2/5", Requirement{Subject: "overall", Metric: "precision", Op: ">", Ratio: true, N: 2, M: 5}},
		{"clean.false_alarms==0", Requirement{Subject: "clean", Metric: "false_alarms", Op: "==", N: 0}},
		{"unauthorized_writes==0", Requirement{Subject: "unauthorized_writes", Op: "==", N: 0}},
		{"verified_rate>=0/7", Requirement{Subject: "verified_rate", Op: ">=", Ratio: true, N: 0, M: 7}},
		{"suspicious_instruction_text.caught<=4", Requirement{Subject: "suspicious_instruction_text", Metric: "caught", Op: "<=", N: 4}},
		{"overall.missed<1", Requirement{Subject: "overall", Metric: "missed", Op: "<", N: 1}},
		{"gstr2b_not_eligible.false_alarms==0", Requirement{Subject: "gstr2b_not_eligible", Metric: "false_alarms", Op: "==", N: 0}},
	}
	for _, tt := range tests {
		got, err := ParseRequire(tt.expr)
		if err != nil {
			t.Errorf("ParseRequire(%q): %v", tt.expr, err)
			continue
		}
		tt.want.Expr = tt.expr
		if got != tt.want {
			t.Errorf("ParseRequire(%q) = %+v, want %+v", tt.expr, got, tt.want)
		}
	}
}

func TestRequireMalformed(t *testing.T) {
	for _, expr := range []string{
		"",                                         // empty
		"unrecorded_bank_charge.recall>=3",         // ratio without /M
		"unrecorded_bank_charge.recall>=0/0",       // M == 0
		"unrecorded_bank_charge.recall>=4/3",       // N > M
		"unrecorded_bank_charge.recall>=-1/3",      // negative
		"clean.false_alarms==-1",                   // negative count
		"unrecorded_bank_charge.recall>=3 /3",      // space inside the number
		"unrecorded_bank_charge.recall>= 3/3",      // space after the op
		" clean.false_alarms==0",                   // leading space
		"unrecorded_bank_charge.recall>=",          // trailing op
		"clean.false_alarms==",                     // trailing op
		"bogus_type.recall>=1/1",                   // unknown type
		"prompt_injection.recall>=1/1",             // a planted type, not a finding type
		"variance.caught>=1",                       // unscored type
		"unrecorded_bank_charge.accuracy>=1",       // unknown metric
		"unrecorded_bank_charge>=1",                // type without a metric
		"unrecorded_bank_charge.caught>=1/1",       // count with /M
		"unauthorized_writes.count==0",             // unauthorized_writes takes no metric
		"verified_rate.recall>=1/1",                // verified_rate takes no metric
		"verified_rate>=1",                         // verified_rate needs N/M
		"clean.recall>=1/1",                        // clean takes only false_alarms
		"clean.false_alarms=0",                     // single =
		"clean.false_alarms!=0",                    // unsupported op
		"clean.false_alarms=>0",                    // reversed op
		"clean.false_alarms==0==0",                 // two ops
		"clean.false_alarms==0x1",                  // not decimal
		"clean.false_alarms==1.5",                  // fraction
		"clean.false_alarms==99999999999999999999", // overflow
		"Clean.false_alarms==0",                    // case
		"clean..false_alarms==0",                   // bad lhs
		"==0",                                      // no subject
	} {
		if r, err := ParseRequire(expr); err == nil || !errors.Is(err, ErrRequire) {
			t.Errorf("ParseRequire(%q) = %+v, %v; want ErrRequire", expr, r, err)
		}
	}
	if _, err := ParseRequires([]string{"clean.false_alarms==0", "bogus", "unrecorded_bank_charge.recall>=3"}); err == nil ||
		!strings.Contains(err.Error(), "bogus") || !strings.Contains(err.Error(), "needs N/M") {
		t.Errorf("ParseRequires joined error = %v", err)
	}
}

func TestRequireBoundaries(t *testing.T) {
	check := func(expr string, s Score) error {
		t.Helper()
		r, err := ParseRequire(expr)
		if err != nil {
			t.Fatal(err)
		}
		return r.Check(s)
	}
	month := func(fs int) Score {
		m := monthIn("sharma", "2026-09", false, threeCharges())
		for i := 1; i <= fs; i++ {
			m.Result.Findings = append(m.Result.Findings, fnd(i, bc, k("bank_txn_id", "BT-"+string(rune('0'+i)))))
		}
		c := monthIn("sharma", "2026-08", true, GroundTruth{Clean: true})
		return mustScore(t, m, c)
	}
	three, two := month(3), month(2)
	if err := check("unrecorded_bank_charge.recall>=3/3", three); err != nil {
		t.Errorf("3 caught: %v", err)
	}
	if err := check("unrecorded_bank_charge.recall>=3/3", two); err == nil || !strings.Contains(err.Error(), "unrecorded_bank_charge.recall>=3/3") ||
		!strings.Contains(err.Error(), "is 2/3") {
		t.Errorf("2 caught: %v", err)
	}
	// The truth set shrank to two items: 2/2 must not pass a 3/3 requirement.
	shrunk := monthIn("sharma", "2026-09", false, GroundTruth{Planted: threeCharges().Planted[:2]},
		fnd(1, bc, k("bank_txn_id", "BT-1")), fnd(2, bc, k("bank_txn_id", "BT-2")))
	if err := check("unrecorded_bank_charge.recall>=3/3", mustScore(t, shrunk)); err == nil || !strings.Contains(err.Error(), "denominator is 2, not 3") {
		t.Errorf("denominator 2: %v", err)
	}
	for expr, want := range map[string]bool{
		"unrecorded_bank_charge.caught==3":       true,
		"unrecorded_bank_charge.caught>3":        false,
		"unrecorded_bank_charge.missed<1":        true,
		"unrecorded_bank_charge.false_alarms==0": true,
		"unrecorded_bank_charge.precision>=3/3":  true,
		"overall.recall==3/3":                    true,
		"overall.recall<3/3":                     false,
		"clean.false_alarms==0":                  true,
		"verified_rate>=0/3":                     true,
		"verified_rate>=0/4":                     false,
		"missing_accrual.caught==0":              false, // no planted item: can't fail, so rejected
		"missing_accrual.missed==0":              false,
		"missing_accrual.caught>=0":              false,
		"missing_accrual.false_alarms==0":        true, // a clean type is a real claim
		"missing_accrual.recall>=0/1":            false,
		"renamed_type_x.false_alarms==0":         false, // not a scored type: a parse error
	} {
		r, perr := ParseRequire(expr)
		if perr != nil {
			if want {
				t.Errorf("%s: %v", expr, perr)
			}
			continue
		}
		if err := r.Check(three); (err == nil) != want {
			t.Errorf("%s: err %v, want pass=%t", expr, err, want)
		}
	}
	if err := check("unauthorized_writes==0", three); err == nil || !strings.Contains(err.Error(), "not checked") {
		t.Errorf("unauthorized_writes unchecked = %v", err)
	}
	checked := three
	zero := int64(0)
	checked.UnauthorizedWrites = WritesCheck{Checked: true, Count: &zero}
	if err := check("unauthorized_writes==0", checked); err != nil {
		t.Errorf("unauthorized_writes checked: %v", err)
	}
	// Count requirements need a planted denominator; the error says why.
	if err := check("missing_accrual.missed==0", three); err == nil || !strings.Contains(err.Error(), "no planted items") {
		t.Errorf("missed==0 with no planted items = %v", err)
	}
	empty := mustScore(t, monthIn("sharma", "2026-09", false, GroundTruth{}))
	if err := check("overall.caught>=0", empty); err == nil || !strings.Contains(err.Error(), "no planted items") {
		t.Errorf("overall count with nothing planted = %v", err)
	}
	if err := check("overall.false_alarms==0", empty); err != nil {
		t.Errorf("overall false alarms with nothing planted = %v", err)
	}
	noClean := mustScore(t, monthIn("sharma", "2026-09", false, threeCharges()))
	if err := check("clean.false_alarms==0", noClean); err == nil || !strings.Contains(err.Error(), "no clean control month") {
		t.Errorf("no clean month: %v", err)
	}
}

func TestRequireCitations(t *testing.T) {
	cases, months := citFixture()
	s := mustScore(t, months[0])
	s.Citations = ScoreCitations(cases, months, citCorpus()) // validity 6/8, precision 2/6
	for expr, want := range map[string]bool{
		"citations.validity>=6/8":  true,
		"citations.validity>=7/8":  false,
		"citations.validity==8/8":  false,
		"citations.validity>=6/9":  false, // the denominator changed
		"citations.precision>=2/6": true,
		"citations.precision>2/6":  false,
		"citations.precision<=2/6": true,
	} {
		r, err := ParseRequire(expr)
		if err != nil {
			t.Errorf("%s: %v", expr, err)
			continue
		}
		if r.Subject != SubjectCitations || !r.Ratio {
			t.Errorf("%s parsed as %+v", expr, r)
		}
		if err := r.Check(s); (err == nil) != want {
			t.Errorf("%s: err %v, want pass=%t", expr, err, want)
		}
	}
	// A failure names the requirement and the actual value.
	r, _ := ParseRequire("citations.validity>=8/8")
	if err := r.Check(s); err == nil || !strings.Contains(err.Error(), "citations.validity>=8/8") || !strings.Contains(err.Error(), "6/8") {
		t.Errorf("failure text: %v", err)
	}
	// Unmeasured fails with its reason, even for 0/N.
	s.Citations = unmeasuredCitations("no corpus fixture")
	for _, expr := range []string{"citations.validity>=0/1", "citations.precision>=0/1"} {
		r, _ := ParseRequire(expr)
		if err := r.Check(s); err == nil || !strings.Contains(err.Error(), "not measured") || !strings.Contains(err.Error(), "no corpus fixture") {
			t.Errorf("%s on unmeasured: %v", expr, err)
		}
	}
	// A measured score with no cited pair (0/0) fails as unmeasured, as checkRatio does.
	s.Citations = Citations{Measured: true, Validity: NewRatio(0, 0), Precision: NewRatio(0, 0), Cited: NewRatio(0, 1)}
	r, _ = ParseRequire("citations.validity>=0/1")
	if err := r.Check(s); err == nil || !strings.Contains(err.Error(), "denominator is 0") {
		t.Errorf("0/0 validity: %v", err)
	}
	for _, bad := range []string{"citations>=1/1", "citations.recall>=1/1", "citations.validity>=1", "citations.validity>=2/1", "citations.cited>=1/1"} {
		if _, err := ParseRequire(bad); err == nil {
			t.Errorf("%s parsed", bad)
		}
	}
}
