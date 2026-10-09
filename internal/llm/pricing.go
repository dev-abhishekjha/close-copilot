package llm

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"
)

// ModelPricing defines the token cost per million tokens in USD for a model.
type ModelPricing struct {
	Input      float64 `yaml:"input_per_million"`
	Output     float64 `yaml:"output_per_million"`
	CacheWrite float64 `yaml:"cache_write_per_million"`
	CacheRead  float64 `yaml:"cache_read_per_million"`
}

// PricingTable holds pricing definitions for all known models.
type PricingTable struct {
	Models map[string]ModelPricing `yaml:"models"`
}

// Cost computes the USD cost for the given token usage on a model.
// It resolves exact names, lowercased names, and model family aliases
// (haiku, sonnet, opus).
func (pt *PricingTable) Cost(model string, u Usage) float64 {
	if pt == nil || len(pt.Models) == 0 {
		return 0.0
	}

	p, ok := pt.lookup(model)
	if !ok {
		return 0.0
	}

	return (float64(u.InputTokens)*p.Input +
		float64(u.OutputTokens)*p.Output +
		float64(u.CacheWriteTokens)*p.CacheWrite +
		float64(u.CacheReadTokens)*p.CacheRead) / 1_000_000.0
}

func (pt *PricingTable) lookup(model string) (ModelPricing, bool) {
	if p, ok := pt.Models[model]; ok {
		return p, true
	}
	lower := strings.ToLower(strings.TrimSpace(model))
	if p, ok := pt.Models[lower]; ok {
		return p, true
	}

	// Family fallbacks
	switch {
	case strings.Contains(lower, "haiku"):
		if p, ok := pt.Models["haiku"]; ok {
			return p, true
		}
	case strings.Contains(lower, "sonnet"):
		if p, ok := pt.Models["sonnet"]; ok {
			return p, true
		}
	case strings.Contains(lower, "opus"):
		if p, ok := pt.Models["opus"]; ok {
			return p, true
		}
	}

	return ModelPricing{}, false
}

// LoadPricing reads and validates a pricing table YAML file.
// If path is empty, it attempts to discover config/pricing.yaml from the working
// directory or parent directories.
func LoadPricing(path string) (*PricingTable, error) {
	resolvedPath, err := resolvePricingPath(path)
	if err != nil {
		return nil, fmt.Errorf("llm: load pricing: %w", err)
	}

	data, err := os.ReadFile(filepath.Clean(resolvedPath))
	if err != nil {
		return nil, fmt.Errorf("llm: read pricing %s: %w", resolvedPath, err)
	}

	var pt PricingTable
	if err := yaml.Unmarshal(data, &pt); err != nil {
		return nil, fmt.Errorf("llm: parse pricing %s: %w", resolvedPath, err)
	}

	if len(pt.Models) == 0 {
		return nil, fmt.Errorf("llm: pricing table at %s has no models configured", resolvedPath)
	}

	var errs []error
	for name, p := range pt.Models {
		if p.Input < 0 || p.Output < 0 || p.CacheWrite < 0 || p.CacheRead < 0 {
			errs = append(errs, fmt.Errorf("model %q has negative pricing: %+v", name, p))
		}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}

	return &pt, nil
}

func resolvePricingPath(path string) (string, error) {
	if path != "" {
		if _, err := os.Stat(path); err != nil {
			return "", err
		}
		return path, nil
	}

	candidates := []string{
		"config/pricing.yaml",
		"../config/pricing.yaml",
		"../../config/pricing.yaml",
	}

	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}

	return "", errors.New("config/pricing.yaml not found in current or parent directories")
}
