package llm

import (
	"errors"
	"fmt"
	"math"
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

// ErrUnpricedModel reports a model with no row in the pricing table. Costing
// fails closed: an unknown model is an error, never a silent zero, so a call on
// an unpriced model cannot slip under the daily budget. Add a row to
// config/pricing.yaml to price a new model.
var ErrUnpricedModel = errors.New("llm: model has no pricing row")

// PricingTable holds pricing definitions for all known models.
type PricingTable struct {
	Models map[string]ModelPricing `yaml:"models"`
}

// Cost computes the USD cost for the given token usage on a model.
//
// The model must match a row exactly or case-insensitively; there is no family
// fallback, so "claude-haiku-9" does not borrow the "haiku" row. A nil or empty
// table, or a model with no row, returns an error wrapping ErrUnpricedModel.
func (pt *PricingTable) Cost(model string, u Usage) (float64, error) {
	if pt == nil || len(pt.Models) == 0 {
		return 0, fmt.Errorf("%w: no pricing table loaded (model %q)", ErrUnpricedModel, model)
	}

	p, ok := pt.lookup(model)
	if !ok {
		return 0, fmt.Errorf("%w: %q", ErrUnpricedModel, model)
	}

	return (float64(u.InputTokens)*p.Input +
		float64(u.OutputTokens)*p.Output +
		float64(u.CacheWriteTokens)*p.CacheWrite +
		float64(u.CacheReadTokens)*p.CacheRead) / 1_000_000.0, nil
}

func (pt *PricingTable) lookup(model string) (ModelPricing, bool) {
	if p, ok := pt.Models[model]; ok {
		return p, true
	}
	lower := strings.ToLower(strings.TrimSpace(model))
	if lower == "" {
		return ModelPricing{}, false
	}
	p, ok := pt.Models[lower]
	return p, ok
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
		for _, v := range []float64{p.Input, p.Output, p.CacheWrite, p.CacheRead} {
			if v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
				errs = append(errs, fmt.Errorf("model %q has invalid pricing: %+v", name, p))
				break
			}
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
