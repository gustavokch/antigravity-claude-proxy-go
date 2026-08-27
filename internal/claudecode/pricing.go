package claudecode

import (
	"strings"
)

// ModelPricing represents per-token USD rates for a model.
type ModelPricing struct {
	Prompt     float64 `json:"prompt"`     // USD per input token
	Completion float64 `json:"completion"` // USD per output token
	CacheWrite float64 `json:"cacheWrite"` // USD per cache creation input token
	CacheRead  float64 `json:"cacheRead"`  // USD per cache read input token
}

// Pricing table (rates per token = rate per million / 1,000,000).
var defaultPricingTable = map[string]ModelPricing{
	"claude-fable-5": {
		Prompt:     10.0 / 1e6,
		Completion: 50.0 / 1e6,
		CacheWrite: 12.5 / 1e6,
		CacheRead:  1.0 / 1e6,
	},
	"claude-opus-5": {
		Prompt:     5.0 / 1e6,
		Completion: 25.0 / 1e6,
		CacheWrite: 6.25 / 1e6,
		CacheRead:  0.50 / 1e6,
	},
	"claude-sonnet-5": {
		Prompt:     2.0 / 1e6,
		Completion: 10.0 / 1e6,
		CacheWrite: 2.50 / 1e6,
		CacheRead:  0.20 / 1e6,
	},
	"claude-haiku-4-5-20251001": {
		Prompt:     1.0 / 1e6,
		Completion: 5.0 / 1e6,
		CacheWrite: 1.25 / 1e6,
		CacheRead:  0.10 / 1e6,
	},
	"claude-3-7-sonnet-20250219": {
		Prompt:     3.0 / 1e6,
		Completion: 15.0 / 1e6,
		CacheWrite: 3.75 / 1e6,
		CacheRead:  0.30 / 1e6,
	},
	"claude-3-5-sonnet-20241022": {
		Prompt:     3.0 / 1e6,
		Completion: 15.0 / 1e6,
		CacheWrite: 3.75 / 1e6,
		CacheRead:  0.30 / 1e6,
	},
	"claude-3-5-haiku-20241022": {
		Prompt:     0.80 / 1e6,
		Completion: 4.00 / 1e6,
		CacheWrite: 1.00 / 1e6,
		CacheRead:  0.08 / 1e6,
	},
	"claude-3-opus-20240229": {
		Prompt:     15.0 / 1e6,
		Completion: 75.0 / 1e6,
		CacheWrite: 18.75 / 1e6,
		CacheRead:  1.50 / 1e6,
	},
}

// GetModelPricing looks up the pricing rates for a given model ID or alias.
func GetModelPricing(modelID string) ModelPricing {
	modelID = strings.TrimSpace(strings.ToLower(modelID))
	if p, ok := defaultPricingTable[modelID]; ok {
		return p
	}

	// Match prefixes or aliases
	switch {
	case strings.Contains(modelID, "fable-5"):
		return defaultPricingTable["claude-fable-5"]
	case strings.Contains(modelID, "opus-5"):
		return defaultPricingTable["claude-opus-5"]
	case strings.Contains(modelID, "sonnet-5"):
		return defaultPricingTable["claude-sonnet-5"]
	case strings.Contains(modelID, "haiku-4-5") || strings.Contains(modelID, "haiku-4.5"):
		return defaultPricingTable["claude-haiku-4-5-20251001"]
	case strings.Contains(modelID, "3-7-sonnet") || strings.Contains(modelID, "3.7-sonnet"):
		return defaultPricingTable["claude-3-7-sonnet-20250219"]
	case strings.Contains(modelID, "3-5-sonnet") || strings.Contains(modelID, "3.5-sonnet"):
		return defaultPricingTable["claude-3-5-sonnet-20241022"]
	case strings.Contains(modelID, "3-5-haiku") || strings.Contains(modelID, "3.5-haiku"):
		return defaultPricingTable["claude-3-5-haiku-20241022"]
	case strings.Contains(modelID, "3-opus") || strings.Contains(modelID, "3.0-opus"):
		return defaultPricingTable["claude-3-opus-20240229"]
	default:
		// Default fallback to Sonnet 3.5 pricing
		return defaultPricingTable["claude-3-5-sonnet-20241022"]
	}
}

// CalculateCost computes the total USD cost from token usage.
func CalculateCost(modelID string, promptTokens, completionTokens, cacheCreationTokens, cacheReadTokens int) float64 {
	pricing := GetModelPricing(modelID)

	// Anthropic prompt tokens in usage include uncached prompt tokens
	cost := float64(promptTokens) * pricing.Prompt
	cost += float64(completionTokens) * pricing.Completion
	cost += float64(cacheCreationTokens) * pricing.CacheWrite
	cost += float64(cacheReadTokens) * pricing.CacheRead

	return cost
}
