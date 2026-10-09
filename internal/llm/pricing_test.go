package llm

import (
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

	// Verify key models exist
	for _, m := range []string{"haiku", "sonnet", "opus", "claude-3-5-haiku-20241022", "claude-3-5-sonnet-20241022"} {
		if _, ok := pt.Models[m]; !ok {
			t.Errorf("expected model %q in pricing table", m)
		}
	}

	// Test fallback and calculations
	haikuCost := pt.Cost("haiku", Usage{
		InputTokens:      1_000_000,
		OutputTokens:     1_000_000,
		CacheWriteTokens: 1_000_000,
		CacheReadTokens:  1_000_000,
	})
	// 1.00 + 5.00 + 1.25 + 0.10 = 7.35
	expectedHaiku := 7.35
	if haikuCost < expectedHaiku-0.001 || haikuCost > expectedHaiku+0.001 {
		t.Errorf("expected haiku cost ~%.2f, got %.4f", expectedHaiku, haikuCost)
	}

	// Test model alias resolution (uppercase, custom variant with family name)
	aliasCost := pt.Cost("CLAUDE-3-5-HAIKU-CUSTOM", Usage{
		InputTokens: 1_000_000,
	})
	if aliasCost < 0.999 || aliasCost > 1.001 {
		t.Errorf("expected alias fallback to haiku cost 1.00, got %.4f", aliasCost)
	}

	// Test unknown model returns 0
	unknownCost := pt.Cost("unknown-vendor-model", Usage{InputTokens: 100})
	if unknownCost != 0.0 {
		t.Errorf("expected unknown model cost 0, got %.4f", unknownCost)
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
}
