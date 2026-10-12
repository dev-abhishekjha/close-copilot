package seed

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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

// TestGroundTruthSuiteV1 builds suite-v1's ground truth (--small) from the
// maps-v1 fixtures, checks it against testdata/ground_truth-v1 and the
// schema, and resolves every key to a real record: an ERP map name, a bank
// line, a GSTR-2B invoice or a bootstrap supplier. It also validates the
// committed evals/scenarios/suite-v1/ground_truth files when they exist.
func TestGroundTruthSuiteV1(t *testing.T) {
	schemaBytes := loadSchemaBytes(t)
	cfg := loadSuiteV1(t)
	cfg.Small = true
	sp := NewSuitePlanter(cfg, suiteProfiles(t), Options{Small: true})
	counts := map[string]int{}
	for _, cm := range suiteTargets(cfg) {
		v := buildV1Truth(t, sp, cfg, cm)
		b, err := v.gt.JSON()
		if err != nil {
			t.Fatal(err)
		}
		validateAgainstSchema(t, schemaBytes, b)
		checkV1Golden(t, filepath.Join(truthV1Dir, cm.Company+"-"+cm.Month+".json"), b)
		for _, pf := range v.gt.Planted {
			counts[pf.Type]++
			checkKeysResolve(t, cm, pf, v)
		}
	}
	for typ, want := range suiteV1Want {
		if counts[typ] != want {
			t.Errorf("%s: %d in ground truth, want %d", typ, counts[typ], want)
		}
	}

	committed, err := filepath.Glob(filepath.Join("..", "..", "evals", "scenarios", "suite-v1", "ground_truth", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, path := range committed {
		b, err := os.ReadFile(path) //nolint:gosec // G304: repo file
		if err != nil {
			t.Fatal(err)
		}
		validateAgainstSchema(t, schemaBytes, b)
		gt, err := LoadGroundTruth(path)
		if err != nil {
			t.Fatal(err)
		}
		total += len(gt.Planted)
		if gt.Clean != (gt.Company == cfg.CleanControl.Company && gt.Month == cfg.CleanControl.Month) {
			t.Errorf("%s: clean %v", path, gt.Clean)
		}
	}
	if len(committed) > 0 && total != 40 {
		t.Errorf("committed suite-v1 ground truth plants %d errors, want 40", total)
	}
}

// wantKeys are the key names of each finding type (shared spec).
var wantKeys = map[string][]string{
	ErrorUnrecordedBankCharge:   {"bank_txn_id"},
	ErrorDuplicateVendorPayment: {"invoice"},
	ErrorMissingAccrual:         {"month", "supplier"},
	ErrorGSTR2BMissingIn2B:      {"invoice_no_norm", "supplier_gstin"},
	ErrorGSTR2BAmountMismatch:   {"invoice_no_norm", "supplier_gstin"},
	ErrorGSTR2BMissingInBooks:   {"invoice_no_norm", "supplier_gstin"},
	ErrorPrepaidNotSpread:       {"invoice"},
	ErrorMisclassifiedExpense:   {"invoice"},
	ErrorWrongPeriodPosting:     {"invoice"},
	ErrorPromptInjection:        {"invoice"},
}

func checkKeysResolve(t *testing.T, cm CompanyMonth, pf PlantedFinding, v v1Truth) {
	t.Helper()
	var names []string
	for k := range pf.Keys {
		names = append(names, k)
	}
	slices.Sort(names)
	if !slices.Equal(names, wantKeys[pf.Type]) {
		t.Errorf("%v %s %s: keys %v, want %v", cm, pf.ID, pf.Type, names, wantKeys[pf.Type])
	}
	if pf.Type == ErrorPromptInjection && pf.AmountPaise != 0 {
		t.Errorf("%v %s: injection amount %d", cm, pf.ID, pf.AmountPaise)
	}
	inMap := func(name string) bool {
		for _, ref := range v.erp {
			if ref.Name == name && ref.DocType == DocPurchaseInvoice {
				return true
			}
		}
		return false
	}
	in2B := func(ctin, norm string) bool {
		for _, s := range v.g2b.Data.DocData.B2B {
			for _, inv := range s.Inv {
				if s.CTIN == ctin && NormInvoiceNo(inv.Inum) == norm {
					return true
				}
			}
		}
		return false
	}
	inBooks := func(ctin, norm string) bool {
		for ext, ref := range v.erp {
			ev, ok := eventByExt(v.pw.BooksWorld.Events, ext)
			if ok && ref.DocType == DocPurchaseInvoice && ev.Meta.SupplierGSTIN == ctin && NormInvoiceNo(ev.InvoiceNo) == norm {
				return true
			}
		}
		return false
	}
	k := pf.Keys
	var ok bool
	switch pf.Type {
	case ErrorUnrecordedBankCharge:
		ok = slices.ContainsFunc(v.lines, func(l BankLine) bool { return l.TxnID == k["bank_txn_id"] })
	case ErrorMissingAccrual:
		ok = k["month"] == cm.Month && slices.Contains(mapValues(v.names), k["supplier"])
	case ErrorGSTR2BMissingIn2B:
		ok = inBooks(k["supplier_gstin"], k["invoice_no_norm"]) && !in2B(k["supplier_gstin"], k["invoice_no_norm"])
	case ErrorGSTR2BAmountMismatch:
		ok = inBooks(k["supplier_gstin"], k["invoice_no_norm"]) && in2B(k["supplier_gstin"], k["invoice_no_norm"])
	case ErrorGSTR2BMissingInBooks:
		ok = !inBooks(k["supplier_gstin"], k["invoice_no_norm"]) && in2B(k["supplier_gstin"], k["invoice_no_norm"])
	default:
		ok = inMap(k["invoice"])
	}
	if !ok {
		t.Errorf("%v %s %s: keys %v don't resolve", cm, pf.ID, pf.Type, k)
	}
}

// TestGroundTruthKeysNeverDropped: a key that can't be resolved is an
// error, never a silent fallback.
func TestGroundTruthKeysNeverDropped(t *testing.T) {
	cfg := loadSuiteV1(t)
	cfg.Small = true
	sp := NewSuitePlanter(cfg, suiteProfiles(t), Options{Small: true})
	v := buildV1Truth(t, sp, cfg, CompanyMonth{"sharma", "2026-09"})
	g := v.g2b
	cases := map[string]GroundTruthOptions{
		"no erp map":   {PlantedWorld: v.pw, ERPMap: nil, GSTR2B: &g, Suppliers: v.names},
		"no gstr2b":    {PlantedWorld: v.pw, ERPMap: v.erp, Suppliers: v.names},
		"no suppliers": {PlantedWorld: v.pw, ERPMap: v.erp, GSTR2B: &g},
	}
	for name, opt := range cases {
		if _, err := BuildGroundTruth(opt); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	bad := v.pw
	bad.PlantedErrors = []PlantedError{{ID: "E01", Type: "bogus", TouchedExtIDs: []string{"x"}}}
	if _, err := BuildGroundTruth(GroundTruthOptions{PlantedWorld: bad}); err == nil {
		t.Error("unknown type: no error")
	}
}
