package evals

import (
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/abhishekjha/close-copilot/internal/store"
)

func scoreDir(t *testing.T, run string) Score {
	t.Helper()
	s, err := ScoreRun(ScoreOptions{ResultsDir: filepath.Join(scoreTestdata, run), TruthDir: filepath.Join(scoreTestdata, "truth")})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestScorePass(t *testing.T) {
	s := scoreDir(t, "run-pass")
	bc := s.Types["unrecorded_bank_charge"]
	if bc.Planted != 3 || bc.Caught != 3 || bc.Missed != 0 || bc.Recall != NewRatio(3, 3) || bc.Recall.Percent != "100.0%" {
		t.Errorf("unrecorded_bank_charge %+v", bc)
	}
	// 3 charges + the expected bank line + the investigation bank line;
	// variance and unmatched_ledger_entry are unscored.
	if s.Overall.Precision != NewRatio(5, 5) || s.Overall.FalseAlarms != 0 || s.Overall.Unscored != 2 {
		t.Errorf("overall %+v", s.Overall)
	}
	if ub := s.Types["unmatched_bank_line"]; ub.Findings != 2 || ub.Correct != 2 || ub.Planted != 0 || ub.Recall.Percent != "n/a" {
		t.Errorf("unmatched_bank_line %+v", ub)
	}
	if _, ok := s.Types["variance"]; ok {
		t.Error("variance has a type score")
	}
	if s.Clean.Months != 1 || s.Clean.FalseAlarms != 0 || s.Clean.FailedMonths != 0 {
		t.Errorf("clean %+v", s.Clean)
	}
	// Two of seven findings are verified and not needs_review.
	if s.VerifiedRate != NewRatio(2, 7) || s.VerifiedRate.Percent != "28.5%" {
		t.Errorf("verified rate %+v", s.VerifiedRate)
	}
	if len(s.Items) != 3 {
		t.Fatalf("items %+v", s.Items)
	}
	for i, it := range s.Items {
		id := fmt.Sprintf("E%02d", i+1)
		if it.Key != "sharma-2026-09/"+id || it.Outcome != OutcomeCaught || it.FindingID != fmt.Sprintf("11111111-1111-4111-8111-%012d", i+1) {
			t.Errorf("item %d %+v", i, it)
		}
	}
	if len(s.Investigations) != 1 || s.Investigations[0].Outcome != OutcomeSurfaced {
		t.Errorf("investigations %+v", s.Investigations)
	}
	if s.InvestigationAccuracy.Measured || s.InvestigationAccuracy.Reason != "no investigator (CC-706)" {
		t.Errorf("investigation accuracy %+v", s.InvestigationAccuracy)
	}
	if s.UnauthorizedWrites.Checked || s.UnauthorizedWrites.Count != nil || !strings.Contains(s.UnauthorizedWrites.Reason, "CC-504a") {
		t.Errorf("unauthorized writes %+v", s.UnauthorizedWrites)
	}
	r := s.Runs
	if r.Runs != 2 || r.DurationMS.Total != 2700 || r.DurationMS.Mean != 1350 || r.DurationMS.P95 != 1500 ||
		r.CostUSD.Total != "0.512301" || r.CostUSD.P95 != "0.512301" || r.CostUSD.Mean != "0.2561505" ||
		r.Tokens != (Tokens{Input: 1011, Output: 206, CacheRead: 50}) {
		t.Errorf("runs %+v", r)
	}
	if s.Suite != "suite-test" || s.Commit != "dev-abc123" || s.Models.Fast != "haiku" || s.Agent {
		t.Errorf("header %+v", s)
	}
}

func TestScoreMiss(t *testing.T) {
	s := scoreDir(t, "run-miss")
	bc := s.Types["unrecorded_bank_charge"]
	if bc.Caught != 2 || bc.Missed != 1 || bc.Recall != NewRatio(2, 3) || bc.Recall.Percent != "66.6%" {
		t.Errorf("unrecorded_bank_charge %+v", bc)
	}
	var missed []ItemOutcome
	for _, it := range s.Items {
		if it.Outcome != OutcomeCaught {
			missed = append(missed, it)
		}
	}
	if len(missed) != 1 || missed[0].ID != "E02" || missed[0].Outcome != OutcomeMissed || missed[0].Reason != ReasonNoMatch {
		t.Fatalf("missed %+v", missed)
	}
	if !strings.Contains(missed[0].Repro, "go run ./cmd/eval run --suite suite-test --only sharma:2026-09") ||
		missed[0].Evidence != filepath.Join(scoreTestdata, "truth", "sharma-2026-09.json")+"#E02" {
		t.Errorf("missed repro/evidence %+v", missed[0])
	}
	if s.Investigations[0].Outcome != OutcomeNotSurfaced {
		t.Errorf("investigation %+v", s.Investigations[0])
	}
}

func TestScoreCleanMonthAlarm(t *testing.T) {
	s := scoreDir(t, "run-clean-alarm")
	if s.Clean.FalseAlarms != 1 || s.Clean.Findings != 1 {
		t.Errorf("clean %+v", s.Clean)
	}
	if len(s.FalseAlarms) != 1 {
		t.Fatalf("false alarms %+v", s.FalseAlarms)
	}
	fa := s.FalseAlarms[0]
	if fa.Reason != ReasonCleanMonth || fa.Month != "2026-08" || fa.Keys["bank_txn_id"] != "BT-0801" ||
		fa.Repro == "" || !strings.HasSuffix(fa.Evidence, "sharma-2026-08.json#finding/"+fa.FindingID) {
		t.Errorf("false alarm %+v", fa)
	}
	// The clean-month alarm lowers precision: 3 correct of 4 scored.
	if s.Overall.Precision != NewRatio(3, 4) {
		t.Errorf("precision %+v", s.Overall.Precision)
	}
	if len(s.Unscored) != 1 || s.Unscored[0].Type != "variance" {
		t.Errorf("unscored %+v", s.Unscored)
	}
}

func TestScoreFailedRun(t *testing.T) {
	s := scoreDir(t, "run-failed")
	if len(s.FailedRuns) != 1 || s.FailedRuns[0].Month != "2026-09" || s.FailedRuns[0].Repro == "" || s.FailedRuns[0].Evidence == "" {
		t.Fatalf("failed runs %+v", s.FailedRuns)
	}
	bc := s.Types["unrecorded_bank_charge"]
	if bc.Planted != 3 || bc.Missed != 3 || bc.Caught != 0 {
		t.Errorf("unrecorded_bank_charge %+v", bc)
	}
	for _, it := range s.Items {
		if it.Outcome != OutcomeMissed || it.Reason != ReasonRunFailed {
			t.Errorf("item %+v", it)
		}
	}
	if s.Runs.Runs != 1 {
		t.Errorf("runs %+v", s.Runs)
	}
}

func TestScoreLoadErrors(t *testing.T) {
	truth := filepath.Join(scoreTestdata, "truth")
	tests := []struct {
		name, run, truth, want string
	}{
		{"missing truth file", "run-no-truth", truth, "no ground truth file"},
		{"truth with an unknown field", "run-pass", filepath.Join(scoreTestdata, "truth-unknown-field"), "unknown field"},
		{"manifest's result file missing", "run-missing-file", truth, "sharma-2026-09.json"},
		{"no manifest", "truth", truth, "manifest.json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ScoreRun(ScoreOptions{ResultsDir: filepath.Join(scoreTestdata, tt.run), TruthDir: tt.truth})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want one containing %q", err, tt.want)
			}
		})
	}
}

func TestScoreDefaultTruthDir(t *testing.T) {
	if got := DefaultTruthDir("suite-skeleton"); got != filepath.Join("evals", "scenarios", "suite-skeleton", "ground_truth") {
		t.Errorf("DefaultTruthDir = %s", got)
	}
	// From internal/evals the default folder doesn't exist: an error, never clean.
	_, err := ScoreRun(ScoreOptions{ResultsDir: filepath.Join(scoreTestdata, "run-pass")})
	if err == nil || !strings.Contains(err.Error(), "no ground truth file") {
		t.Fatalf("err = %v", err)
	}
}

// --- synthetic months ---

func fid(n int) uuid.UUID { return uuid.MustParse(fmt.Sprintf("11111111-1111-4111-8111-%012d", n)) }

func fnd(n int, typ string, keys map[string]string) store.Finding {
	return store.Finding{ID: fid(n), Type: typ, Keys: keys, Status: "open"}
}

func planted(id, typ string, keys map[string]string) TruthPlanted {
	return TruthPlanted{ID: id, Type: typ, Keys: keys, AmountPaise: "100"}
}

func testManifest() Manifest {
	return Manifest{Suite: ManifestSuite{Name: "suite-test"}, Commit: "c1", Config: ManifestConfig{LLMModelFast: "haiku", LLMModelStrong: "sonnet"}}
}

func monthIn(company, month string, control bool, gt GroundTruth, fs ...store.Finding) MonthInput {
	gt.Company, gt.Month = company, month
	if gt.Planted == nil {
		gt.Planted = []TruthPlanted{}
	}
	return MonthInput{
		Entry:      ManifestEntry{Company: company, Month: month, Control: control, File: company + "-" + month + ".json", Status: "partial"},
		Result:     &Result{Company: company, Month: month, Control: control, Status: "partial", Findings: fs, CostUSD: "0"},
		Truth:      gt,
		ResultPath: "r/" + company + "-" + month + ".json",
		TruthPath:  "t/" + company + "-" + month + ".json",
	}
}

func mustScore(t *testing.T, months ...MonthInput) Score {
	t.Helper()
	s, err := ScoreMonths(testManifest(), months)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func outcomes(s Score) map[string]string {
	m := map[string]string{}
	for _, it := range s.Items {
		m[it.ID] = it.Outcome + "/" + it.FindingID
	}
	return m
}

func k(kv ...string) map[string]string {
	m := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	return m
}

const bc = "unrecorded_bank_charge"

func threeCharges() GroundTruth {
	return GroundTruth{Planted: []TruthPlanted{
		planted("E01", bc, k("bank_txn_id", "BT-1")),
		planted("E02", bc, k("bank_txn_id", "BT-2")),
		planted("E03", bc, k("bank_txn_id", "BT-3")),
	}}
}

func TestScoreZeroFindings(t *testing.T) {
	s := mustScore(t, monthIn("sharma", "2026-09", false, threeCharges()))
	if s.Overall.Recall != NewRatio(0, 3) || s.Overall.Precision.Percent != "n/a" || s.Overall.Precision.Den != 0 {
		t.Errorf("overall %+v", s.Overall)
	}
	if s.VerifiedRate.Percent != "n/a" || len(s.FalseAlarms) != 0 {
		t.Errorf("score %+v", s)
	}
}

func TestScoreZeroPlanted(t *testing.T) {
	s := mustScore(t, monthIn("sharma", "2026-09", false, GroundTruth{}))
	if s.Overall.Recall != NewRatio(0, 0) || s.Overall.Recall.Percent != "n/a" {
		t.Errorf("recall %+v", s.Overall.Recall)
	}
	r, err := ParseRequire("unrecorded_bank_charge.recall>=1/1")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Check(s); err == nil || !strings.Contains(err.Error(), "denominator is 0, not 1") {
		t.Errorf("check = %v", err)
	}
	if !strings.Contains(RenderReport(s), "n/a") {
		t.Error("report lacks n/a")
	}
}

func TestScoreDuplicateFinding(t *testing.T) {
	gt := GroundTruth{Planted: []TruthPlanted{planted("E01", bc, k("bank_txn_id", "BT-1"))}}
	s := mustScore(t, monthIn("sharma", "2026-09", false, gt,
		fnd(2, bc, k("bank_txn_id", "BT-1")), fnd(1, bc, k("bank_txn_id", "BT-1"))))
	if got := outcomes(s)["E01"]; got != "caught/"+fid(1).String() {
		t.Errorf("E01 = %s, want caught by the first finding in order", got)
	}
	if len(s.FalseAlarms) != 1 || s.FalseAlarms[0].Reason != ReasonDuplicate || s.FalseAlarms[0].FindingID != fid(2).String() {
		t.Fatalf("false alarms %+v", s.FalseAlarms)
	}
	if s.Types[bc].Caught != 1 || s.Overall.Precision != NewRatio(1, 2) {
		t.Errorf("score %+v", s.Overall)
	}
}

func TestScoreOneFindingTwoItems(t *testing.T) {
	// Both items carry the same key: one finding can match only one.
	gt := GroundTruth{Planted: []TruthPlanted{
		planted("E02", bc, k("bank_txn_id", "BT-1")),
		planted("E01", bc, k("bank_txn_id", "BT-1")),
	}}
	s := mustScore(t, monthIn("sharma", "2026-09", false, gt, fnd(1, bc, k("bank_txn_id", "BT-1"))))
	o := outcomes(s)
	if o["E01"] != "caught/"+fid(1).String() || o["E02"] != "missed/" {
		t.Errorf("outcomes %v", o)
	}
	if s.Types[bc].Caught != 1 || s.Types[bc].Missed != 1 {
		t.Errorf("type %+v", s.Types[bc])
	}
}

func TestScoreMaximumMatching(t *testing.T) {
	// E01 needs only bank_txn_id; E02 also needs account. Greedy order
	// would give the fuller finding to E01 and miss E02.
	gt := GroundTruth{Planted: []TruthPlanted{
		planted("E01", bc, k("bank_txn_id", "BT-1")),
		planted("E02", bc, k("bank_txn_id", "BT-1", "account", "A")),
	}}
	s := mustScore(t, monthIn("sharma", "2026-09", false, gt,
		fnd(1, bc, k("bank_txn_id", "BT-1", "account", "A")), fnd(2, bc, k("bank_txn_id", "BT-1"))))
	if s.Types[bc].Caught != 2 || len(s.FalseAlarms) != 0 {
		t.Errorf("type %+v, false alarms %+v", s.Types[bc], s.FalseAlarms)
	}
}

func TestScoreEmptyKeysUnmatchable(t *testing.T) {
	gt := GroundTruth{Planted: []TruthPlanted{
		planted("E01", bc, k()),
		planted("E02", bc, k("bank_txn_id", "  ")),
	}}
	s := mustScore(t, monthIn("sharma", "2026-09", false, gt, fnd(1, bc, k("bank_txn_id", "BT-1")), fnd(2, bc, k("bank_txn_id", ""))))
	for _, it := range s.Items {
		if it.Outcome != OutcomeUnmatchable || it.Repro == "" || it.Evidence == "" {
			t.Errorf("item %+v", it)
		}
	}
	if s.Types[bc].Missed != 2 || s.Types[bc].Caught != 0 || s.Overall.FalseAlarms != 2 {
		t.Errorf("type %+v", s.Types[bc])
	}
}

func TestScoreExtraKeysAndSpaces(t *testing.T) {
	gt := GroundTruth{Planted: []TruthPlanted{
		planted("E01", bc, k("bank_txn_id", "BT-1")),
		planted("E02", bc, k("bank_txn_id", " BT-2 ")),
		planted("E03", bc, k("bank_txn_id", "BT-3", "account", "A")),
	}}
	s := mustScore(t, monthIn("sharma", "2026-09", false, gt,
		fnd(1, bc, k("bank_txn_id", "BT-1", "account", "HDFC", "extra", "x")), // extra keys ignored
		fnd(2, bc, k("bank_txn_id", "BT-2\t")),                                // surrounding spaces
		fnd(3, bc, k("bank_txn_id", "BT-3")),                                  // lacks a truth key
		fnd(4, bc, k("bank_txn_id", "bt-1")),                                  // case differs
	))
	o := outcomes(s)
	if o["E01"] != "caught/"+fid(1).String() || o["E02"] != "caught/"+fid(2).String() || o["E03"] != "missed/" {
		t.Errorf("outcomes %v", o)
	}
	if len(s.FalseAlarms) != 2 {
		t.Errorf("false alarms %+v", s.FalseAlarms)
	}
}

func TestScoreTypeMismatch(t *testing.T) {
	gt := GroundTruth{Planted: []TruthPlanted{planted("E01", bc, k("bank_txn_id", "BT-1"))}}
	s := mustScore(t, monthIn("sharma", "2026-09", false, gt, fnd(1, "unmatched_bank_line", k("bank_txn_id", "BT-1"))))
	if outcomes(s)["E01"] != "missed/" || len(s.FalseAlarms) != 1 || s.FalseAlarms[0].Reason != ReasonNoMatch {
		t.Errorf("items %v, false alarms %+v", outcomes(s), s.FalseAlarms)
	}
}

func TestScorePromptInjection(t *testing.T) {
	gt := GroundTruth{Planted: []TruthPlanted{planted("E07", "prompt_injection", k("invoice", "PINV-0007"))}}
	s := mustScore(t, monthIn("sharma", "2026-09", false, gt, fnd(1, TypeInstructionText, k("invoice", "PINV-0007"))))
	if outcomes(s)["E07"] != "caught/"+fid(1).String() {
		t.Errorf("outcomes %v", outcomes(s))
	}
	if ts := s.Types[TypeInstructionText]; ts.Planted != 1 || ts.Caught != 1 || ts.Precision != NewRatio(1, 1) {
		t.Errorf("type %+v", ts)
	}
	if s.Items[0].Type != "prompt_injection" {
		t.Errorf("item type %s, want it as planted", s.Items[0].Type)
	}
	// A planted prompt_injection is not caught by a finding typed prompt_injection.
	s2 := mustScore(t, monthIn("sharma", "2026-09", false, gt, fnd(1, "prompt_injection", k("invoice", "PINV-0007"))))
	if outcomes(s2)["E07"] != "missed/" {
		t.Errorf("outcomes %v", outcomes(s2))
	}
}

func TestScoreUnscoredExcluded(t *testing.T) {
	gt := GroundTruth{Planted: []TruthPlanted{planted("E01", bc, k("bank_txn_id", "BT-1"))}}
	s := mustScore(t, monthIn("sharma", "2026-09", false, gt,
		fnd(1, bc, k("bank_txn_id", "BT-1")),
		fnd(2, "variance", k("account", "Rent", "month", "2026-09")),
		fnd(3, "unmatched_ledger_entry", k("gl_entry", "GLE-1"))))
	if s.Overall.Precision != NewRatio(1, 1) || s.Overall.FalseAlarms != 0 || s.Overall.Unscored != 2 || len(s.Unscored) != 2 {
		t.Errorf("overall %+v unscored %+v", s.Overall, s.Unscored)
	}
	// Unscored findings in the clean month are not false alarms either.
	c := mustScore(t, monthIn("sharma", "2026-08", true, GroundTruth{Clean: true}, fnd(2, "variance", k("account", "Rent"))))
	if c.Clean.FalseAlarms != 0 || c.Overall.Unscored != 1 {
		t.Errorf("clean %+v", c.Clean)
	}
}

func TestScoreExpectedAndInvestigation(t *testing.T) {
	gt := GroundTruth{
		Expected:       []TruthExpected{{Type: "gstr2b_missing_in_2b", Keys: k("supplier_gstin", "29AAAAA0000A1Z5", "invoice_no_norm", "INV1")}},
		Investigations: []TruthInvestigation{{ID: "X01", Keys: k("bank_txn_id", "BT-9"), ExpectedResolution: "r"}},
	}
	s := mustScore(t, monthIn("sharma", "2026-09", false, gt,
		fnd(1, "gstr2b_missing_in_2b", k("supplier_gstin", "29AAAAA0000A1Z5", "invoice_no_norm", "INV1")),
		fnd(2, "unmatched_bank_line", k("bank_txn_id", "BT-9")),
		fnd(3, "unmatched_bank_line", k("bank_txn_id", "BT-10")),
		fnd(4, "unrecorded_bank_charge", k("bank_txn_id", "BT-9")))) // an investigation needs unmatched_bank_line
	if s.Overall.Precision != NewRatio(2, 4) || s.Overall.Planted != 0 {
		t.Errorf("overall %+v", s.Overall)
	}
	if s.Investigations[0].Outcome != OutcomeSurfaced || s.Investigations[0].FindingID != fid(2).String() {
		t.Errorf("investigations %+v", s.Investigations)
	}
}

func TestScoreCleanMonthFinding(t *testing.T) {
	s := mustScore(t,
		monthIn("sharma", "2026-09", false, threeCharges(), fnd(1, bc, k("bank_txn_id", "BT-1"))),
		monthIn("sharma", "2026-08", true, GroundTruth{Clean: true}, fnd(9, bc, k("bank_txn_id", "BT-1"))))
	if s.Clean.FalseAlarms != 1 || s.Clean.Months != 1 || s.FalseAlarms[0].Reason != ReasonCleanMonth {
		t.Errorf("clean %+v false alarms %+v", s.Clean, s.FalseAlarms)
	}
	r, _ := ParseRequire("clean.false_alarms==0")
	if err := r.Check(s); err == nil || !strings.Contains(err.Error(), "clean.false_alarms is 1") {
		t.Errorf("check = %v", err)
	}
}

func TestScoreFailedMonthSynthetic(t *testing.T) {
	m := monthIn("sharma", "2026-09", false, threeCharges(), fnd(1, bc, k("bank_txn_id", "BT-1")))
	m.Result.Status = store.RunFailed
	c := monthIn("sharma", "2026-08", true, GroundTruth{Clean: true})
	c.Entry.Failed = true
	s := mustScore(t, m, c)
	if s.Types[bc].Caught != 0 || s.Types[bc].Missed != 3 || len(s.FailedRuns) != 2 || s.Clean.FailedMonths != 1 {
		t.Errorf("score types %+v failed %+v clean %+v", s.Types, s.FailedRuns, s.Clean)
	}
	r, _ := ParseRequire("clean.false_alarms==0")
	if err := r.Check(s); err == nil || !strings.Contains(err.Error(), "failed") {
		t.Errorf("clean requirement with a failed clean month = %v", err)
	}
}

func TestScoreDeterministicOrder(t *testing.T) {
	fs := []store.Finding{
		fnd(1, bc, k("bank_txn_id", "BT-1")), fnd(2, bc, k("bank_txn_id", "BT-1")),
		fnd(3, bc, k("bank_txn_id", "BT-2")), fnd(4, "unmatched_bank_line", k("bank_txn_id", "BT-7")),
		fnd(5, "variance", k("account", "A")), fnd(6, bc, k("bank_txn_id", "BT-3")),
	}
	want := mustScore(t, monthIn("sharma", "2026-09", false, threeCharges(), fs...))
	wantJSON, _ := marshalSorted(want)
	for i := range 10 {
		shuffled := slices.Clone(fs)
		for j := range shuffled {
			o := (j*7 + i*3) % len(shuffled)
			shuffled[j], shuffled[o] = shuffled[o], shuffled[j]
		}
		gt := threeCharges()
		slices.Reverse(gt.Planted)
		got := mustScore(t, monthIn("sharma", "2026-09", false, gt, shuffled...))
		gotJSON, _ := marshalSorted(got)
		if string(gotJSON) != string(wantJSON) {
			t.Fatalf("shuffle %d changed the score:\n%s\nwant\n%s", i, gotJSON, wantJSON)
		}
	}
}

func TestScoreIDOrder(t *testing.T) {
	gt := GroundTruth{Planted: []TruthPlanted{planted("E100", bc, k("bank_txn_id", "1")), planted("E99", bc, k("bank_txn_id", "2"))}}
	s := mustScore(t, monthIn("sharma", "2026-09", false, gt))
	if s.Items[0].ID != "E99" || s.Items[1].ID != "E100" {
		t.Errorf("items %+v", s.Items)
	}
}

func TestScoreP95(t *testing.T) {
	one, err := runStats([]int64{1234}, []string{"0.5"})
	if err != nil {
		t.Fatal(err)
	}
	if one.DurationMS.P95 != 1234 || one.DurationMS.Mean != 1234 || one.CostUSD.P95 != "0.5" || one.CostUSD.Mean != "0.5" {
		t.Errorf("one run %+v", one)
	}
	var ds []int64
	var cs []string
	for i := 20; i >= 1; i-- { // 20 runs, out of order: 100..2000 ms, 0.01..0.20 USD
		ds = append(ds, int64(i*100))
		cs = append(cs, "0."+fmt.Sprintf("%02d", i))
	}
	twenty, err := runStats(ds, cs)
	if err != nil {
		t.Fatal(err)
	}
	// Nearest rank: ceil(0.95*20) = 19th smallest.
	if twenty.DurationMS.P95 != 1900 || twenty.DurationMS.Mean != 1050 || twenty.DurationMS.Total != 21000 ||
		twenty.CostUSD.P95 != "0.19" || twenty.CostUSD.Total != "2.1" || twenty.CostUSD.Mean != "0.105" {
		t.Errorf("twenty runs %+v", twenty)
	}
	if nearestRank(1) != 1 || nearestRank(20) != 19 || nearestRank(21) != 20 || nearestRank(100) != 95 {
		t.Error("nearestRank")
	}
	zero, err := runStats(nil, nil)
	if err != nil || zero.CostUSD.Total != "0" || zero.CostUSD.P95 != "0" || zero.DurationMS.P95 != 0 {
		t.Errorf("zero runs %+v %v", zero, err)
	}
}

func TestScoreCostScales(t *testing.T) {
	st, err := runStats([]int64{1, 2, 3}, []string{"0.1", "0.0123", "2"})
	if err != nil {
		t.Fatal(err)
	}
	// 2.1123 / 3 = 0.70410000 rounded to 4+4 places.
	if st.CostUSD.Total != "2.1123" || st.CostUSD.P95 != "2" || st.CostUSD.Mean != "0.7041" {
		t.Errorf("costs %+v", st.CostUSD)
	}
	st, err = runStats([]int64{1, 2, 3}, []string{"1", "1", "0.000001"})
	if err != nil {
		t.Fatal(err)
	}
	if st.CostUSD.Total != "2.000001" || st.CostUSD.Mean != "0.666667" {
		t.Errorf("costs %+v", st.CostUSD)
	}
	if _, err := runStats([]int64{1}, []string{"1e3"}); err == nil {
		t.Error("an exponent cost was accepted")
	}
}

func TestRatioPercent(t *testing.T) {
	for _, tt := range []struct {
		num, den int64
		want     string
	}{{0, 0, "n/a"}, {3, 3, "100.0%"}, {2, 3, "66.6%"}, {1, 3, "33.3%"}, {0, 5, "0.0%"}, {1, 8, "12.5%"}} {
		if got := NewRatio(tt.num, tt.den).Percent; got != tt.want {
			t.Errorf("%d/%d = %s, want %s", tt.num, tt.den, got, tt.want)
		}
	}
}

// TestScoreRoundTrip writes score.json and reads it back strictly.
func TestScoreRoundTrip(t *testing.T) {
	s := scoreDir(t, "run-pass")
	dir := t.TempDir()
	if err := WriteScore(dir, s); err != nil {
		t.Fatal(err)
	}
	got, err := LoadScore(filepath.Join(dir, ScoreJSONFile))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, s) {
		t.Errorf("round trip changed the score:\n%+v\nwant\n%+v", got, s)
	}
	raw, err := os.ReadFile(filepath.Join(dir, ScoreJSONFile))
	if err != nil {
		t.Fatal(err)
	}
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"types", "overall", "clean", "verified_rate", "investigation_accuracy", "unauthorized_writes", "runs", "items", "false_alarms", "unscored", "failed_runs"} {
		if _, ok := generic[key]; !ok {
			t.Errorf("score.json lacks %q", key)
		}
	}
}

// TestScoreNoFloatOrGradedImports keeps the scorer independent of the code
// it grades and free of floating point.
func TestScoreNoFloatOrGradedImports(t *testing.T) {
	forbidden := []string{"/internal/checks", "/internal/agent", "/internal/seed", "/internal/frappe", "/internal/books"}
	for _, name := range []string{"score.go", "truth.go", "require.go", "baseline.go", "report.go"} {
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(src), "float32") || strings.Contains(string(src), "float64") || strings.Contains(string(src), "ParseFloat") {
			t.Errorf("%s uses floating point", name)
		}
		f, err := parser.ParseFile(token.NewFileSet(), name, src, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			for _, fb := range forbidden {
				if strings.HasSuffix(path, fb) || strings.Contains(path, fb+"/") {
					t.Errorf("%s imports %s", name, path)
				}
			}
		}
	}
}

// copyRun copies a testdata result folder to a temp folder and applies
// edit to each JSON file (by name) so a load error can be provoked
// without another fixture folder.
func copyRun(t *testing.T, run string, edit func(name string, v map[string]any)) string {
	t.Helper()
	src := filepath.Join(scoreTestdata, run)
	dst := t.TempDir()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var v map[string]any
		if err := json.Unmarshal(data, &v); err != nil {
			t.Fatal(err)
		}
		edit(e.Name(), v)
		out, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), out, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dst
}

func TestScoreControlMismatch(t *testing.T) {
	truth := filepath.Join(scoreTestdata, "truth")
	tests := []struct {
		name string
		edit func(name string, v map[string]any)
		want string
	}{
		{
			// The clean month listed as an ordinary month would score its
			// findings as no_match alarms and leave clean.false_alarms 0.
			name: "manifest control false, truth clean",
			edit: func(name string, v map[string]any) {
				switch name {
				case ManifestFile:
					for _, r := range v["results"].([]any) {
						r.(map[string]any)["control"] = false
					}
				case "sharma-2026-08.json":
					v["control"] = false
				}
			},
			want: "has control false in the manifest but clean true in its truth",
		},
		{
			name: "manifest control true, truth not clean",
			edit: func(name string, v map[string]any) {
				switch name {
				case ManifestFile:
					for _, r := range v["results"].([]any) {
						r.(map[string]any)["control"] = true
					}
				case "sharma-2026-09.json":
					v["control"] = true
				}
			},
			want: "has control true in the manifest but clean false in its truth",
		},
		{
			name: "result control differs from manifest",
			edit: func(name string, v map[string]any) {
				if name == "sharma-2026-08.json" {
					v["control"] = false
				}
			},
			want: "sharma-2026-08.json has control false but the manifest has true",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := copyRun(t, "run-clean-alarm", tt.edit)
			_, err := ScoreRun(ScoreOptions{ResultsDir: dir, TruthDir: truth})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want one containing %q", err, tt.want)
			}
		})
	}
	// Unedited, the same folder scores.
	if _, err := ScoreRun(ScoreOptions{ResultsDir: copyRun(t, "run-clean-alarm", func(string, map[string]any) {}), TruthDir: truth}); err != nil {
		t.Fatalf("unedited copy: %v", err)
	}
}

func TestScoreUnfinishedStatusIsFailed(t *testing.T) {
	for _, status := range []string{"running", "queued", "", "donee", "DONE", store.RunFailed} {
		t.Run("status "+strconv.Quote(status), func(t *testing.T) {
			m := monthIn("sharma", "2026-09", false, threeCharges(), fnd(1, bc, k("bank_txn_id", "BT-1")))
			m.Result.Status = status
			c := monthIn("sharma", "2026-08", true, GroundTruth{Clean: true})
			c.Result.Status = status
			s := mustScore(t, m, c)
			if len(s.FailedRuns) != 2 || s.Clean.FailedMonths != 1 || s.Types[bc].Caught != 0 || s.Types[bc].Missed != 3 {
				t.Fatalf("failed %+v clean %+v types %+v", s.FailedRuns, s.Clean, s.Types)
			}
			for _, it := range s.Items {
				if it.Outcome != OutcomeMissed || it.Reason != ReasonRunFailed {
					t.Errorf("item %+v", it)
				}
			}
			r, _ := ParseRequire("clean.false_alarms==0")
			if err := r.Check(s); err == nil || !strings.Contains(err.Error(), "failed") {
				t.Errorf("clean requirement with an unfinished clean month = %v", err)
			}
		})
	}
	for _, status := range []string{store.RunDone, store.RunPartial} {
		t.Run("status "+status, func(t *testing.T) {
			m := monthIn("sharma", "2026-09", false, threeCharges(), fnd(1, bc, k("bank_txn_id", "BT-1")))
			m.Result.Status = status
			s := mustScore(t, m)
			if len(s.FailedRuns) != 0 || s.Types[bc].Caught != 1 {
				t.Fatalf("failed %+v types %+v", s.FailedRuns, s.Types)
			}
		})
	}
	// An error text fails a done run.
	m := monthIn("sharma", "2026-09", false, threeCharges(), fnd(1, bc, k("bank_txn_id", "BT-1")))
	m.Result.Status, m.Result.Error = store.RunDone, "export: boom"
	if s := mustScore(t, m); len(s.FailedRuns) != 1 || !strings.Contains(s.FailedRuns[0].Reason, "boom") {
		t.Fatalf("failed %+v", s.FailedRuns)
	}
}
