package seed

import (
	"bytes"
	"encoding/json"
	"os"
	"regexp"
	"testing"
)

const schemaPath = "../../evals/schema/ground_truth.schema.json"

type jsonSchemaDoc struct {
	Type                 string                    `json:"type"`
	Required             []string                  `json:"required"`
	Properties           map[string]propertySchema `json:"properties"`
	AdditionalProperties bool                      `json:"additionalProperties"`
}

type propertySchema struct {
	Type                 string                    `json:"type"`
	Pattern              string                    `json:"pattern"`
	Items                *jsonSchemaDoc            `json:"items"`
	Required             []string                  `json:"required"`
	Properties           map[string]propertySchema `json:"properties"`
	AdditionalProperties bool                      `json:"additionalProperties"`
}

func loadSchemaBytes(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatalf("read schema file %s: %v", schemaPath, err)
	}
	return data
}

func validateAgainstSchema(t *testing.T, schemaData []byte, targetJSON []byte) {
	t.Helper()
	var doc jsonSchemaDoc
	if err := json.Unmarshal(schemaData, &doc); err != nil {
		t.Fatalf("unmarshal schema doc: %v", err)
	}

	var root map[string]any
	dec := json.NewDecoder(bytes.NewReader(targetJSON))
	dec.UseNumber()
	if err := dec.Decode(&root); err != nil {
		t.Fatalf("decode target json: %v", err)
	}

	// 1. Root required fields
	for _, req := range doc.Required {
		if _, ok := root[req]; !ok {
			t.Fatalf("root missing required property %q", req)
		}
	}

	// 2. Disallow additional root properties if false
	if !doc.AdditionalProperties {
		for k := range root {
			if _, ok := doc.Properties[k]; !ok {
				t.Fatalf("root has unexpected property %q", k)
			}
		}
	}

	// 3. Month pattern
	monthProp := doc.Properties["month"]
	monthRe := regexp.MustCompile(monthProp.Pattern)
	monthVal, ok := root["month"].(string)
	if !ok || !monthRe.MatchString(monthVal) {
		t.Fatalf("property 'month' value %v does not match pattern %s", root["month"], monthProp.Pattern)
	}

	// 4. Clean boolean
	if _, ok := root["clean"].(bool); !ok {
		t.Fatalf("property 'clean' is not a boolean: %T", root["clean"])
	}

	// 5. Planted array
	plantedVal, ok := root["planted"].([]any)
	if !ok {
		t.Fatalf("property 'planted' is not an array")
	}
	plantedSchema := doc.Properties["planted"].Items
	idRe := regexp.MustCompile(plantedSchema.Properties["id"].Pattern)
	for i, item := range plantedVal {
		obj, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("planted[%d] is not an object", i)
		}
		for _, req := range plantedSchema.Required {
			if _, ok := obj[req]; !ok {
				t.Fatalf("planted[%d] missing required property %q", i, req)
			}
		}
		if !plantedSchema.AdditionalProperties {
			for k := range obj {
				if _, ok := plantedSchema.Properties[k]; !ok {
					t.Fatalf("planted[%d] has unexpected property %q", i, k)
				}
			}
		}
		idStr, ok := obj["id"].(string)
		if !ok || !idRe.MatchString(idStr) {
			t.Fatalf("planted[%d].id %v does not match pattern %s", i, obj["id"], plantedSchema.Properties["id"].Pattern)
		}
		if _, ok := obj["type"].(string); !ok {
			t.Fatalf("planted[%d].type is not string", i)
		}
		if _, ok := obj["keys"].(map[string]any); !ok {
			t.Fatalf("planted[%d].keys is not object", i)
		}
		num, ok := obj["amount_paise"].(json.Number)
		if !ok {
			t.Fatalf("planted[%d].amount_paise is not integer number: %T", i, obj["amount_paise"])
		}
		if _, err := num.Int64(); err != nil {
			t.Fatalf("planted[%d].amount_paise is not integer: %v", i, err)
		}
	}

	// 6. Expected array
	expectedVal, ok := root["expected"].([]any)
	if !ok {
		t.Fatalf("property 'expected' is not an array")
	}
	expectedSchema := doc.Properties["expected"].Items
	for i, item := range expectedVal {
		obj, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("expected[%d] is not an object", i)
		}
		for _, req := range expectedSchema.Required {
			if _, ok := obj[req]; !ok {
				t.Fatalf("expected[%d] missing required property %q", i, req)
			}
		}
		if !expectedSchema.AdditionalProperties {
			for k := range obj {
				if _, ok := expectedSchema.Properties[k]; !ok {
					t.Fatalf("expected[%d] has unexpected property %q", i, k)
				}
			}
		}
	}

	// 7. Investigations array
	invVal, ok := root["investigations"].([]any)
	if !ok {
		t.Fatalf("property 'investigations' is not an array")
	}
	invSchema := doc.Properties["investigations"].Items
	invIDRe := regexp.MustCompile(invSchema.Properties["id"].Pattern)
	for i, item := range invVal {
		obj, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("investigations[%d] is not an object", i)
		}
		for _, req := range invSchema.Required {
			if _, ok := obj[req]; !ok {
				t.Fatalf("investigations[%d] missing required property %q", i, req)
			}
		}
		if !invSchema.AdditionalProperties {
			for k := range obj {
				if _, ok := invSchema.Properties[k]; !ok {
					t.Fatalf("investigations[%d] has unexpected property %q", i, k)
				}
			}
		}
		idStr, ok := obj["id"].(string)
		if !ok || !invIDRe.MatchString(idStr) {
			t.Fatalf("investigations[%d].id %v does not match pattern %s", i, obj["id"], invSchema.Properties["id"].Pattern)
		}
	}
}

func TestGroundTruthSchema(t *testing.T) {
	schemaBytes := loadSchemaBytes(t)

	p := loadTestProfile(t, "sharma")
	cfg := DefaultSkeletonConfig()

	// 1. Evaluated month: 2026-09
	w09, err := Generate(p, "2026-09", Options{Small: true})
	if err != nil {
		t.Fatalf("Generate 2026-09: %v", err)
	}
	pw09, err := PlantErrors(w09, cfg)
	if err != nil {
		t.Fatalf("PlantErrors 2026-09: %v", err)
	}
	lines09, err := BankLines(pw09.BankWorld)
	if err != nil {
		t.Fatalf("BankLines 2026-09: %v", err)
	}

	gt09, err := BuildGroundTruth(GroundTruthOptions{
		PlantedWorld: pw09,
		BankLines:    lines09,
	})
	if err != nil {
		t.Fatalf("BuildGroundTruth 2026-09: %v", err)
	}

	json09, err := gt09.JSON()
	if err != nil {
		t.Fatalf("gt09.JSON: %v", err)
	}
	validateAgainstSchema(t, schemaBytes, json09)

	// Validate field constraints
	if gt09.Clean {
		t.Errorf("gt09.Clean: want false, got true")
	}
	if len(gt09.Planted) != 3 {
		t.Errorf("gt09.Planted count: want 3, got %d", len(gt09.Planted))
	}

	// Verify all bank_txn_ids in planted and investigations exist in bankLines
	bankTxnIDs := make(map[string]bool)
	for _, l := range lines09 {
		bankTxnIDs[l.TxnID] = true
	}

	for _, pf := range gt09.Planted {
		txnID, ok := pf.Keys["bank_txn_id"]
		if !ok || txnID == "" {
			t.Errorf("planted finding %s missing bank_txn_id key", pf.ID)
		} else if !bankTxnIDs[txnID] {
			t.Errorf("planted finding %s bank_txn_id %s not in bank lines", pf.ID, txnID)
		}
	}

	for _, inv := range gt09.Investigations {
		txnID, ok := inv.Keys["bank_txn_id"]
		if !ok || txnID == "" {
			t.Errorf("investigation %s missing bank_txn_id key", inv.ID)
		} else if !bankTxnIDs[txnID] {
			t.Errorf("investigation %s bank_txn_id %s not in bank lines", inv.ID, txnID)
		}
		if inv.ExpectedResolution != "gateway_settlement" {
			t.Errorf("investigation %s expected_resolution = %q, want gateway_settlement", inv.ID, inv.ExpectedResolution)
		}
	}

	// 2. Clean control month: 2026-08
	w08, err := Generate(p, "2026-08", Options{Small: true})
	if err != nil {
		t.Fatalf("Generate 2026-08: %v", err)
	}
	pw08, err := PlantErrors(w08, cfg)
	if err != nil {
		t.Fatalf("PlantErrors 2026-08: %v", err)
	}
	gt08, err := BuildGroundTruth(GroundTruthOptions{PlantedWorld: pw08})
	if err != nil {
		t.Fatalf("BuildGroundTruth 2026-08: %v", err)
	}

	json08, err := gt08.JSON()
	if err != nil {
		t.Fatalf("gt08.JSON: %v", err)
	}
	validateAgainstSchema(t, schemaBytes, json08)

	if !gt08.Clean {
		t.Errorf("gt08.Clean: want true, got false")
	}
	if len(gt08.Planted) != 0 {
		t.Errorf("gt08.Planted: want 0, got %d", len(gt08.Planted))
	}
}

func TestNormInvoiceNo(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"abc/101", "ABC101"},
		{"ABC-0101", "ABC101"},
		{"ABC 101", "ABC101"},
		{"INV/2026/007", "INV20267"},
		{"CLD/2026/0912", "CLD2026912"},
		{"CLD-2026-912", "CLD2026912"},
		{"TK/2026/0931", "TK2026931"},
		{"ACC-PINV-2026-00044", "ACCPINV202644"},
		{"xyz_001.002", "XYZ12"},
		{"000", "0"},
		{"0", "0"},
	}

	for _, tc := range tests {
		got := NormInvoiceNo(tc.input)
		if got != tc.want {
			t.Errorf("NormInvoiceNo(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestWriteAndLoadGroundTruth(t *testing.T) {
	p := loadTestProfile(t, "sharma")
	w, err := Generate(p, "2026-09", Options{Small: true})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	cfg := DefaultSkeletonConfig()
	pw, err := PlantErrors(w, cfg)
	if err != nil {
		t.Fatalf("PlantErrors: %v", err)
	}

	gt, err := BuildGroundTruth(GroundTruthOptions{PlantedWorld: pw})
	if err != nil {
		t.Fatalf("BuildGroundTruth: %v", err)
	}

	tmpDir := t.TempDir()
	outPath, err := WriteGroundTruth(tmpDir, gt)
	if err != nil {
		t.Fatalf("WriteGroundTruth: %v", err)
	}

	loaded, err := LoadGroundTruth(outPath)
	if err != nil {
		t.Fatalf("LoadGroundTruth: %v", err)
	}

	if loaded.Scenario != gt.Scenario || loaded.Company != gt.Company || loaded.Month != gt.Month {
		t.Errorf("loaded header mismatch: got %+v, want %+v", loaded, gt)
	}
	if len(loaded.Planted) != len(gt.Planted) {
		t.Errorf("loaded planted length: got %d, want %d", len(loaded.Planted), len(gt.Planted))
	}
}

func TestGroundTruthWithGSTR2B(t *testing.T) {
	schemaBytes := loadSchemaBytes(t)

	p := loadTestProfile(t, "sharma")
	cfg := DefaultSkeletonConfig()

	prev, err := Generate(p, "2026-08", Options{Small: true})
	if err != nil {
		t.Fatalf("Generate prev: %v", err)
	}
	curr, err := Generate(p, "2026-09", Options{Small: true})
	if err != nil {
		t.Fatalf("Generate curr: %v", err)
	}

	g2b, err := BuildGSTR2B(p, "2026-09", []World{prev, curr}, DefaultGSTR2BOptions())
	if err != nil {
		t.Fatalf("BuildGSTR2B: %v", err)
	}

	pw, err := PlantErrors(curr, cfg)
	if err != nil {
		t.Fatalf("PlantErrors: %v", err)
	}

	gt, err := BuildGroundTruth(GroundTruthOptions{
		PlantedWorld: pw,
		GSTR2B:       &g2b,
	})
	if err != nil {
		t.Fatalf("BuildGroundTruth: %v", err)
	}

	b, err := gt.JSON()
	if err != nil {
		t.Fatalf("gt.JSON: %v", err)
	}
	validateAgainstSchema(t, schemaBytes, b)

	if len(g2b.Late) > 0 {
		if len(gt.Expected) != len(g2b.Late) {
			t.Errorf("gt.Expected count %d != g2b.Late count %d", len(gt.Expected), len(g2b.Late))
		}
		for _, exp := range gt.Expected {
			if exp.Type != "gstr2b_wrong_period" {
				t.Errorf("expected finding type %q, want gstr2b_wrong_period", exp.Type)
			}
			if exp.Keys["supplier_gstin"] == "" {
				t.Errorf("expected finding missing supplier_gstin key")
			}
			if exp.Keys["invoice_no_norm"] == "" {
				t.Errorf("expected finding missing invoice_no_norm key")
			}
		}
	}
}
