package ccusage

import "fmt"

// CostMode selects where an entry's cost comes from.
type CostMode string

const (
	// CostModeAuto uses the logged costUSD when present and otherwise
	// calculates it from tokens.
	CostModeAuto CostMode = "auto"
	// CostModeCalculate always calculates the cost from tokens.
	CostModeCalculate CostMode = "calculate"
	// CostModeDisplay uses only the logged costUSD, counting 0 without one.
	CostModeDisplay CostMode = "display"
)

// ParseCostMode parses a cost mode name. The empty string means auto.
func ParseCostMode(s string) (CostMode, error) {
	switch CostMode(s) {
	case "", CostModeAuto:
		return CostModeAuto, nil
	case CostModeCalculate, CostModeDisplay:
		return CostMode(s), nil
	}
	return "", fmt.Errorf("ccusage: unknown cost mode %q", s)
}

// cacheCreate1hInputMultiplier prices a 1-hour cache write at twice the input
// rate.
const cacheCreate1hInputMultiplier = 2.0

// TokenUsage is the token counts of one response. Every cache write is either
// a 5-minute or a 1-hour write; without a breakdown, count it as 5-minute.
type TokenUsage struct {
	Input         int64
	Output        int64
	CacheCreate5m int64
	CacheCreate1h int64
	CacheRead     int64
	// Fast marks a fast-mode response, billed at the model's FastMultiplier.
	Fast bool
}

// Usage returns the entry's token counts.
func (e Entry) Usage() TokenUsage {
	return TokenUsage{
		Input:         e.Input,
		Output:        e.Output,
		CacheCreate5m: e.CacheCreate5m,
		CacheCreate1h: e.CacheCreate1h,
		CacheRead:     e.CacheRead,
		Fast:          e.Speed == "fast",
	}
}

// EntryCost returns the entry's cost in USD under mode, pricing its logged
// model through p. A model p does not know costs 0.
func EntryCost(e Entry, mode CostMode, p Pricer) float64 {
	switch mode {
	case CostModeDisplay:
		if e.CostUSD != nil {
			return *e.CostUSD
		}
		return 0
	case CostModeCalculate:
		return TokenCost(e.Model, e.Usage(), p)
	default:
		if e.CostUSD != nil {
			return *e.CostUSD
		}
		return TokenCost(e.Model, e.Usage(), p)
	}
}

// TokenCost prices u for model through p, applying the fast multiplier to a
// fast-mode response. An empty model, a nil pricer or an unknown model costs 0.
func TokenCost(model string, u TokenUsage, p Pricer) float64 {
	if model == "" || p == nil {
		return 0
	}
	price, ok := p.Find(model)
	if !ok {
		return 0
	}
	cost := PriceTokens(u, price)
	if u.Fast && price.FastMultiplier > 0 {
		cost *= price.FastMultiplier
	}
	return cost
}

// PriceTokens prices u at price without the fast multiplier. Each bucket is
// tiered on its own: tokens above 200k are billed at the bucket's *Above200k
// rate when there is one. A 1-hour cache write costs twice the input rate.
func PriceTokens(u TokenUsage, price ModelPrice) float64 {
	cache1h := price.Input * cacheCreate1hInputMultiplier
	var cache1hAbove *float64
	if price.InputAbove200k != nil {
		v := *price.InputAbove200k * cacheCreate1hInputMultiplier
		cache1hAbove = &v
	}
	return tieredCost(u.Input, price.Input, price.InputAbove200k) +
		tieredCost(u.Output, price.Output, price.OutputAbove200k) +
		tieredCost(u.CacheCreate5m, price.CacheCreate, price.CacheCreateAbove200k) +
		tieredCost(u.CacheCreate1h, cache1h, cache1hAbove) +
		tieredCost(u.CacheRead, price.CacheRead, price.CacheReadAbove200k)
}

// tieredCost bills the first 200k tokens at base and the rest at above, or
// all of them at base when above is nil.
func tieredCost(tokens int64, base float64, above *float64) float64 {
	if tokens <= 0 {
		return 0
	}
	if above != nil && tokens > longContextThreshold {
		return longContextThreshold*base + float64(tokens-longContextThreshold)*(*above)
	}
	return float64(tokens) * base
}
