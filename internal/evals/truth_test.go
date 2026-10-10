package evals

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const scoreTestdata = "testdata/score"

func TestTruthLoad(t *testing.T) {
	gt, err := LoadGroundTruth(filepath.Join(scoreTestdata, "truth", "sharma-2026-09.json"))
	if err != nil {
		t.Fatal(err)
	}
	if gt.Company != "sharma" || gt.Month != "2026-09" || gt.Clean || len(gt.Planted) != 3 || len(gt.Expected) != 1 || len(gt.Investigations) != 1 {
		t.Fatalf("truth %+v", gt)
	}
	if p := gt.Planted[1]; p.ID != "E02" || p.Type != "unrecorded_bank_charge" || p.Keys["bank_txn_id"] != "BT-0902" || p.AmountPaise.String() != "59000" {
		t.Errorf("planted[1] %+v", p)
	}
	clean, err := LoadGroundTruth(filepath.Join(scoreTestdata, "truth", "sharma-2026-08.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !clean.Clean || len(clean.Planted) != 0 || clean.Planted == nil {
		t.Errorf("clean truth %+v", clean)
	}
}

func TestTruthUnknownField(t *testing.T) {
	_, err := LoadGroundTruth(filepath.Join(scoreTestdata, "truth-unknown-field", "sharma-2026-09.json"))
	if !errors.Is(err, ErrTruth) || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("err = %v, want an unknown-field ErrTruth", err)
	}
}

func TestTruthMissingFile(t *testing.T) {
	_, err := LoadGroundTruth(filepath.Join(t.TempDir(), "sharma-2026-09.json"))
	if !errors.Is(err, ErrTruth) || !strings.Contains(err.Error(), "no ground truth file") {
		t.Fatalf("err = %v", err)
	}
}

func TestTruthRejects(t *testing.T) {
	const okPlanted = `{"id":"E01","type":"unrecorded_bank_charge","keys":{"bank_txn_id":"BT-1"},"amount_paise":100}`
	doc := func(company, month, clean, planted, investigations string) string {
		return `{"scenario":"s","company":"` + company + `","month":"` + month + `","clean":` + clean +
			`,"planted":[` + planted + `],"expected":[],"investigations":[` + investigations + `]}`
	}
	tests := []struct {
		name, file, body, want string
	}{
		{"company differs from the file name", "sharma-2026-09.json", doc("mehta", "2026-09", "false", okPlanted, ""), "its name should be"},
		{"month differs from the file name", "sharma-2026-09.json", doc("sharma", "2026-08", "false", okPlanted, ""), "its name should be"},
		{"planted ID not E\\d{2,}", "sharma-2026-09.json", doc("sharma", "2026-09", "false", strings.Replace(okPlanted, "E01", "E1", 1), ""), "is not E"},
		{"planted ID lowercase", "sharma-2026-09.json", doc("sharma", "2026-09", "false", strings.Replace(okPlanted, "E01", "e01", 1), ""), "is not E"},
		{"duplicate planted ID", "sharma-2026-09.json", doc("sharma", "2026-09", "false", okPlanted+","+okPlanted, ""), "listed twice"},
		{"investigation ID not X\\d{2,}", "sharma-2026-09.json", doc("sharma", "2026-09", "false", okPlanted,
			`{"id":"Y01","keys":{"bank_txn_id":"BT-9"},"expected_resolution":"r"}`), "is not X"},
		{"duplicate investigation ID", "sharma-2026-09.json", doc("sharma", "2026-09", "false", okPlanted,
			`{"id":"X01","keys":{},"expected_resolution":"r"},{"id":"X01","keys":{},"expected_resolution":"r"}`), "listed twice"},
		{"non-integer amount", "sharma-2026-09.json", doc("sharma", "2026-09", "false", strings.Replace(okPlanted, "100", "100.5", 1), ""), "not an integer"},
		{"string amount", "sharma-2026-09.json", doc("sharma", "2026-09", "false", strings.Replace(okPlanted, "100", `"100"`, 1), ""), "not an integer"},
		{"missing amount", "sharma-2026-09.json", doc("sharma", "2026-09", "false", strings.Replace(okPlanted, `,"amount_paise":100`, "", 1), ""), "amount_paise is missing"},
		{"non-string key value", "sharma-2026-09.json", doc("sharma", "2026-09", "false", strings.Replace(okPlanted, `"BT-1"`, "7", 1), ""), "decode"},
		{"clean with planted errors", "sharma-2026-09.json", doc("sharma", "2026-09", "true", okPlanted, ""), "clean is true"},
		{"bad month", "sharma-2026-13.json", doc("sharma", "2026-13", "false", okPlanted, ""), "not YYYY-MM"},
		{"missing planted", "sharma-2026-09.json", `{"scenario":"s","company":"sharma","month":"2026-09","clean":false,"expected":[],"investigations":[]}`, "planted is missing"},
		{"null investigations", "sharma-2026-09.json", `{"scenario":"s","company":"sharma","month":"2026-09","clean":false,"planted":[],"expected":[],"investigations":null}`, "investigations is missing"},
		{"missing clean", "sharma-2026-09.json", `{"scenario":"s","company":"sharma","month":"2026-09","planted":[],"expected":[],"investigations":[]}`, "clean is missing"},
		{"trailing data", "sharma-2026-09.json", doc("sharma", "2026-09", "false", okPlanted, "") + `{}`, "trailing data"},
		{"planted and expected with the same keys", "sharma-2026-09.json", strings.Replace(doc("sharma", "2026-09", "false", okPlanted, ""),
			`"expected":[]`, `"expected":[{"type":"unrecorded_bank_charge","keys":{"bank_txn_id":" BT-1 "}}]`, 1), "overlapping keys"},
		{"two expected with the same keys", "sharma-2026-09.json", strings.Replace(doc("sharma", "2026-09", "false", okPlanted, ""),
			`"expected":[]`, `"expected":[{"type":"unmatched_bank_line","keys":{"bank_txn_id":"BT-5"}},{"type":"unmatched_bank_line","keys":{"bank_txn_id":"BT-5"}}]`, 1), "overlapping keys"},
		{"prompt_injection and expected suspicious_instruction_text", "sharma-2026-09.json", strings.Replace(doc("sharma", "2026-09", "false",
			`{"id":"E01","type":"prompt_injection","keys":{"invoice":"PI-1"},"amount_paise":0}`, ""),
			`"expected":[]`, `"expected":[{"type":"suspicious_instruction_text","keys":{"invoice":"PI-1"}}]`, 1), "overlapping keys"},
		{"planted keys a subset of another's", "sharma-2026-09.json", doc("sharma", "2026-09", "false",
			okPlanted+`,{"id":"E02","type":"unrecorded_bank_charge","keys":{"bank_txn_id":"BT-1","account":"A"},"amount_paise":100}`, ""), "overlapping keys"},
		{"expected and investigation with the same keys", "sharma-2026-09.json", strings.Replace(doc("sharma", "2026-09", "false", okPlanted,
			`{"id":"X01","keys":{"bank_txn_id":"BT-9"},"expected_resolution":"r"}`),
			`"expected":[]`, `"expected":[{"type":"unmatched_bank_line","keys":{"bank_txn_id":"BT-9"}}]`, 1), "overlapping keys"},
		{"unknown top-level field", "sharma-2026-09.json", strings.Replace(doc("sharma", "2026-09", "false", okPlanted, ""), `"scenario"`, `"extra":1,"scenario"`, 1), "unknown field"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), tt.file)
			if err := os.WriteFile(path, []byte(tt.body), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadGroundTruth(path)
			if !errors.Is(err, ErrTruth) || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want ErrTruth containing %q", err, tt.want)
			}
		})
	}
}

// TestTruthOverlapAllowed: the same keys on different types, and items that
// can never match, are not overlaps.
func TestTruthOverlapAllowed(t *testing.T) {
	body := `{"scenario":"s","company":"sharma","month":"2026-09","clean":false,
"planted":[{"id":"E01","type":"unrecorded_bank_charge","keys":{"bank_txn_id":"BT-1"},"amount_paise":100},
{"id":"E02","type":"unrecorded_bank_charge","keys":{},"amount_paise":100}],
"expected":[{"type":"unmatched_bank_line","keys":{"bank_txn_id":"BT-1"}},{"type":"unrecorded_bank_charge","keys":{}}],
"investigations":[{"id":"X01","keys":{"bank_txn_id":"BT-2"},"expected_resolution":"r"}]}`
	path := filepath.Join(t.TempDir(), "sharma-2026-09.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadGroundTruth(path); err != nil {
		t.Fatalf("err = %v", err)
	}
}
