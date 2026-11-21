// Package budget provides deterministic token accounting, USD cost estimation,
// prompt cache hit-rate calculation, CI budget assertions, and GitHub Step Summary formatting.
package budget

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
)

// ModelPricing defines per-1M-token USD pricing for a model.
type ModelPricing struct {
	InputPerMillion      float64 `json:"input_per_million" yaml:"input_per_million"`
	OutputPerMillion     float64 `json:"output_per_million" yaml:"output_per_million"`
	CacheReadPerMillion  float64 `json:"cache_read_per_million" yaml:"cache_read_per_million"`
	CacheWritePerMillion float64 `json:"cache_write_per_million" yaml:"cache_write_per_million"`
}

// DefaultFallbackPricing is used when an interaction's model is unknown or omitted.
var DefaultFallbackPricing = ModelPricing{
	InputPerMillion:      3.00,
	OutputPerMillion:     15.00,
	CacheReadPerMillion:  0.30,
	CacheWritePerMillion: 3.75,
}

// PricingTable maps normalized model names/prefixes to per-1M-token USD rates.
type PricingTable struct {
	models   map[string]ModelPricing
	fallback ModelPricing
}

// DefaultPricingTable returns the built-in 2026 model pricing table.
func DefaultPricingTable() *PricingTable {
	return &PricingTable{
		fallback: DefaultFallbackPricing,
		models: map[string]ModelPricing{
			// Anthropic Claude models
			"claude-sonnet-4-5": {
				InputPerMillion:      3.00,
				OutputPerMillion:     15.00,
				CacheReadPerMillion:  0.30,
				CacheWritePerMillion: 3.75,
			},
			"claude-sonnet-4": {
				InputPerMillion:      3.00,
				OutputPerMillion:     15.00,
				CacheReadPerMillion:  0.30,
				CacheWritePerMillion: 3.75,
			},
			"claude-opus-4": {
				InputPerMillion:      15.00,
				OutputPerMillion:     75.00,
				CacheReadPerMillion:  1.50,
				CacheWritePerMillion: 18.75,
			},
			"claude-3-7-sonnet": {
				InputPerMillion:      3.00,
				OutputPerMillion:     15.00,
				CacheReadPerMillion:  0.30,
				CacheWritePerMillion: 3.75,
			},
			"claude-3-5-sonnet": {
				InputPerMillion:      3.00,
				OutputPerMillion:     15.00,
				CacheReadPerMillion:  0.30,
				CacheWritePerMillion: 3.75,
			},
			// OpenAI models
			"gpt-5": {
				InputPerMillion:      1.25,
				OutputPerMillion:     10.00,
				CacheReadPerMillion:  0.125,
				CacheWritePerMillion: 1.25,
			},
			"gpt-4.1": {
				InputPerMillion:      2.00,
				OutputPerMillion:     8.00,
				CacheReadPerMillion:  0.50,
				CacheWritePerMillion: 2.00,
			},
			"gpt-4o": {
				InputPerMillion:      2.50,
				OutputPerMillion:     10.00,
				CacheReadPerMillion:  1.25,
				CacheWritePerMillion: 2.50,
			},
			"o3": {
				InputPerMillion:      2.00,
				OutputPerMillion:     8.00,
				CacheReadPerMillion:  0.50,
				CacheWritePerMillion: 2.00,
			},
			// Google Gemini models
			"gemini-2.5-pro": {
				InputPerMillion:      1.25,
				OutputPerMillion:     10.00,
				CacheReadPerMillion:  0.31,
				CacheWritePerMillion: 1.25,
			},
			"gemini-2.5-flash": {
				InputPerMillion:      0.15,
				OutputPerMillion:     0.60,
				CacheReadPerMillion:  0.0375,
				CacheWritePerMillion: 0.15,
			},
		},
	}
}

// LoadPricingFile loads a JSON pricing override file and merges it onto DefaultPricingTable().
// Supports either a top-level map {"model-name": {...}} or a wrapped {"models": {"model-name": {...}}}.
func LoadPricingFile(path string) (*PricingTable, error) {
	if strings.TrimSpace(path) == "" {
		return DefaultPricingTable(), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read pricing file %q: %w", path, err)
	}

	var rawMap map[string]json.RawMessage
	if err := json.Unmarshal(data, &rawMap); err != nil {
		return nil, fmt.Errorf("failed to parse pricing file %q as JSON: %w", path, err)
	}

	overrides := make(map[string]ModelPricing)
	if modelsRaw, hasWrapped := rawMap["models"]; hasWrapped {
		if err := json.Unmarshal(modelsRaw, &overrides); err != nil {
			return nil, fmt.Errorf("failed to decode 'models' table in pricing file %q: %w", path, err)
		}
	} else {
		if err := json.Unmarshal(data, &overrides); err != nil {
			return nil, fmt.Errorf("failed to decode pricing entries in %q: %w", path, err)
		}
	}

	table := DefaultPricingTable()
	for name, rate := range overrides {
		if err := validatePricingRate(name, rate); err != nil {
			return nil, err
		}
		table.Set(name, rate)
	}
	return table, nil
}

func validatePricingRate(name string, rate ModelPricing) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("pricing model name cannot be empty")
	}
	if rate.InputPerMillion < 0 || rate.OutputPerMillion < 0 ||
		rate.CacheReadPerMillion < 0 || rate.CacheWritePerMillion < 0 {
		return fmt.Errorf("pricing rates for model %q must be >= 0", name)
	}
	return nil
}

// Set adds or overrides the pricing entry for a model name.
func (t *PricingTable) Set(model string, pricing ModelPricing) {
	if t.models == nil {
		t.models = make(map[string]ModelPricing)
	}
	t.models[normalizeModelName(model)] = pricing
}

// Models returns a sorted list of model identifiers registered in the table.
func (t *PricingTable) Models() []string {
	if t == nil {
		return nil
	}
	out := make([]string, 0, len(t.models))
	for k := range t.models {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Lookup resolves pricing for a model string (exact match first, then longest-prefix match).
// Returns the pricing, the matched table key (or "default"), and whether an explicit table match succeeded.
func (t *PricingTable) Lookup(model string) (ModelPricing, string, bool) {
	if t == nil {
		t = DefaultPricingTable()
	}
	norm := normalizeModelName(model)
	if norm == "" {
		return t.fallback, "default", false
	}
	if p, ok := t.models[norm]; ok {
		return p, norm, true
	}

	// Longest-prefix match so versioned snapshots (e.g. claude-sonnet-4-5-20250929, gpt-4o-2024-11-20)
	// resolve deterministically to their canonical model entry.
	var bestKey string
	for k := range t.models {
		if strings.HasPrefix(norm, k) {
			if len(norm) == len(k) || isModelBoundary(norm[len(k)]) {
				if len(k) > len(bestKey) {
					bestKey = k
				}
			}
		}
	}
	if bestKey != "" {
		return t.models[bestKey], bestKey, true
	}
	return t.fallback, "default", false
}

func normalizeModelName(model string) string {
	s := strings.ToLower(strings.TrimSpace(model))
	for _, prefix := range []string{"models/", "anthropic/", "openai/", "google/", "publishers/google/models/"} {
		s = strings.TrimPrefix(s, prefix)
	}
	return strings.TrimSpace(s)
}

func isModelBoundary(b byte) bool {
	return b == '-' || b == '_' || b == '@' || b == ':' || b == '.'
}
