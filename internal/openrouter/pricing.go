package openrouter

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Pricing contains token and request pricing for a model in USD.
type Pricing struct {
	Prompt          float64 `json:"prompt"`
	Completion      float64 `json:"completion"`
	Request         float64 `json:"request"`
	Image           float64 `json:"image"`
	InputCacheRead  float64 `json:"input_cache_read"`
	InputCacheWrite float64 `json:"input_cache_write"`
}

// UnmarshalJSON handles both string and float representations of pricing numbers from OpenRouter.
func (p *Pricing) UnmarshalJSON(data []byte) error {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	p.Prompt = parsePriceField(raw["prompt"])
	p.Completion = parsePriceField(raw["completion"])
	p.Request = parsePriceField(raw["request"])
	p.Image = parsePriceField(raw["image"])

	// Check both snake_case and camelCase or alternate OpenRouter field names
	if v, ok := raw["input_cache_read"]; ok {
		p.InputCacheRead = parsePriceField(v)
	} else if v, ok := raw["cache_read"]; ok {
		p.InputCacheRead = parsePriceField(v)
	}

	if v, ok := raw["input_cache_write"]; ok {
		p.InputCacheWrite = parsePriceField(v)
	} else if v, ok := raw["cache_write"]; ok {
		p.InputCacheWrite = parsePriceField(v)
	}

	return nil
}

func parsePriceField(v any) float64 {
	if v == nil {
		return 0
	}
	switch val := v.(type) {
	case float64:
		return val
	case float32:
		return float64(val)
	case int:
		return float64(val)
	case int64:
		return float64(val)
	case string:
		clean := strings.TrimSpace(val)
		if clean == "" {
			return 0
		}
		f, err := strconv.ParseFloat(clean, 64)
		if err != nil {
			return 0
		}
		return f
	default:
		clean := fmt.Sprint(v)
		f, err := strconv.ParseFloat(clean, 64)
		if err != nil {
			return 0
		}
		return f
	}
}

// CalculateCost computes total API call cost in USD.
func CalculateCost(pricing Pricing, inputTokens, outputTokens, cacheReadTokens, cacheWriteTokens int) float64 {
	uncachedInput := inputTokens
	if cacheReadTokens > 0 {
		if uncachedInput >= cacheReadTokens {
			uncachedInput -= cacheReadTokens
		}
	}

	cost := float64(uncachedInput) * pricing.Prompt
	cost += float64(outputTokens) * pricing.Completion

	if cacheReadTokens > 0 {
		readPrice := pricing.InputCacheRead
		if readPrice <= 0 && pricing.Prompt > 0 {
			// Fallback: 10% prompt price for cache read if not explicitly stated
			readPrice = pricing.Prompt * 0.1
		}
		cost += float64(cacheReadTokens) * readPrice
	}

	if cacheWriteTokens > 0 {
		writePrice := pricing.InputCacheWrite
		if writePrice <= 0 && pricing.Prompt > 0 {
			// Fallback: 125% prompt price for cache creation if not explicitly stated
			writePrice = pricing.Prompt * 1.25
		}
		cost += float64(cacheWriteTokens) * writePrice
	}

	cost += pricing.Request
	return cost
}
