package llm

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadPricing(t *testing.T) {
	// Test loading shipped config/pricing.yaml via discovery
	pt, err := LoadPricing("../../config/pricing.yaml")
	if err != nil {
		t.Fatalf("LoadPricing failed: %v", err)
	}

	if len(pt.Models) == 0 {
		t.Fatal("expected models in pricing table, got none")
	}

	// Verify key models exist, including the current IDs.
	for _, m := range []string{
		"haiku", "sonnet", "opus", "claude-3-5-haiku-20241022", "claude-3-5-sonnet-20241022",
		"claude-opus-5-5", "claude-sonnet-5-5", "claude-haiku-4-5-20251001", "claude-haiku-5-5", "claude-fable-5-1",
	} {
		if _, ok := pt.Models[m]; !ok {
			t.Errorf("expected model %q in pricing table", m)
		}
	}

	million := Usage{InputTokens: 1_000_000, OutputTokens: 1_000_000, CacheWriteTokens: 1_000_000, CacheReadTokens: 1_000_000}
	haikuCost, err := pt.Cost("haiku", million)
	if err != nil {
		t.Fatalf("Cost(haiku): %v", err)
	}
	// 1.00 + 5.00 + 1.25 + 0.10 = 7.35
	if haikuCost < 7.349 || haikuCost > 7.351 {
		t.Errorf("expected haiku cost ~7.35, got %.4f", haikuCost)
	}

	// Case-insensitive exact match.
	upper, err := pt.Cost("CLAUDE-HAIKU-4-5-20251001", Usage{InputTokens: 1_000_000})
	if err != nil || upper < 0.999 || upper > 1.001 {
		t.Errorf("expected case-insensitive match at 1.00, got %.4f, %v", upper, err)
	}

	// Rows without an owner-confirmed price use the Opus row.
	opus := pt.Models["opus"]
	for _, m := range []string{"claude-opus-5-5", "claude-fable-5-1", "claude-sonnet-5-5", "claude-haiku-5-5"} {
		if pt.Models[m] != opus {
			t.Errorf("%s should use the Opus row %+v, got %+v", m, opus, pt.Models[m])
		}
	}
}

func TestCostFailsClosed(t *testing.T) {
	pt := &PricingTable{Models: map[string]ModelPricing{"haiku": {Input: 1}}}

	for _, model := range []string{"unknown-vendor-model", "claude-3-5-haiku-custom", "claude-haiku-9", ""} {
		cost, err := pt.Cost(model, Usage{InputTokens: 100})
		if !errors.Is(err, ErrUnpricedModel) {
			t.Errorf("Cost(%q): expected ErrUnpricedModel, got %v", model, err)
		}
		if cost != 0 {
			t.Errorf("Cost(%q) = %g on error", model, cost)
		}
	}

	var nilTable *PricingTable
	if _, err := nilTable.Cost("haiku", Usage{}); !errors.Is(err, ErrUnpricedModel) {
		t.Errorf("nil table: expected ErrUnpricedModel, got %v", err)
	}
	if _, err := (&PricingTable{}).Cost("haiku", Usage{}); !errors.Is(err, ErrUnpricedModel) {
		t.Errorf("empty table: expected ErrUnpricedModel, got %v", err)
	}
}

func TestLoadPricingErrors(t *testing.T) {
	// Non-existent file
	if _, err := LoadPricing("non-existent-pricing-file.yaml"); err == nil {
		t.Error("expected error for non-existent file, got nil")
	}

	// Negative pricing
	tmp := filepath.Join(t.TempDir(), "invalid.yaml")
	invalidContent := `
models:
  bad-model:
    input_per_million: -1.0
    output_per_million: 2.0
`
	if err := os.WriteFile(tmp, []byte(invalidContent), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPricing(tmp); err == nil {
		t.Error("expected error for negative pricing, got nil")
	}

	// Empty models table
	emptyFile := filepath.Join(t.TempDir(), "empty.yaml")
	if err := os.WriteFile(emptyFile, []byte("models: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadPricing(emptyFile); err == nil {
		t.Error("expected error for empty pricing table, got nil")
	}

	// NaN and infinite prices
	for _, v := range []string{".nan", ".inf"} {
		f := filepath.Join(t.TempDir(), "nan.yaml")
		body := "models:\n  m:\n    input_per_million: " + v + "\n"
		if err := os.WriteFile(f, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadPricing(f); err == nil {
			t.Errorf("expected error for price %s, got nil", v)
		}
	}
}
