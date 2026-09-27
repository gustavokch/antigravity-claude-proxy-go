package ccusage

//go:generate go run gen_pricing.go

import (
	"context"
	_ "embed"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
)

// LiteLLMPricingURL is LiteLLM's model price table, the source of the embedded
// snapshot and of LiteLLMPricer.Refresh.
const LiteLLMPricingURL = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"

// maxPricingBody caps how much of the LiteLLM document Refresh reads.
const maxPricingBody = 64 << 20

// longContextThreshold is the per-bucket token count above which the
// *Above200k rates apply.
const longContextThreshold = 200_000

// dateSuffixDigits is the length of a YYYYMMDD model date suffix. Other
// numeric suffixes name distinct model versions.
const dateSuffixDigits = 8

// claudeKeyPrefixes are the LiteLLM keys the pricer keeps. gen_pricing.go
// carries the same list.
var claudeKeyPrefixes = []string{"claude-", "anthropic.claude-", "anthropic/claude-"}

//go:embed litellm_claude.json
var embeddedLiteLLM []byte

// ModelPrice holds per-token USD rates for one model.
//
// A nil *Above200k rate means the bucket is billed at its base rate however
// many tokens it holds. MaxInputTokens is zero when unknown. FastMultiplier
// scales the cost of fast-mode responses; zero is treated as 1.
type ModelPrice struct {
	Input       float64
	Output      float64
	CacheCreate float64
	CacheRead   float64

	InputAbove200k       *float64
	OutputAbove200k      *float64
	CacheCreateAbove200k *float64
	CacheReadAbove200k   *float64

	MaxInputTokens int64
	FastMultiplier float64
}

// Pricer finds the price of a model.
type Pricer interface {
	Find(model string) (ModelPrice, bool)
}

// PricerFunc adapts a function to the Pricer interface.
type PricerFunc func(model string) (ModelPrice, bool)

// Find calls f.
func (f PricerFunc) Find(model string) (ModelPrice, bool) { return f(model) }

type priceLookup struct {
	price ModelPrice
	ok    bool
}

// ChainPricer asks its pricers in order and returns the first hit. Results,
// misses included, are memoised per model name; call Reset after a pricer's
// data changes.
type ChainPricer struct {
	pricers []Pricer

	mu    sync.Mutex
	cache map[string]priceLookup
}

// NewChainPricer returns a chain over pricers, skipping nil ones.
func NewChainPricer(pricers ...Pricer) *ChainPricer {
	c := &ChainPricer{cache: map[string]priceLookup{}}
	for _, p := range pricers {
		if p != nil {
			c.pricers = append(c.pricers, p)
		}
	}
	return c
}

// Find returns the price from the first pricer that knows model.
func (c *ChainPricer) Find(model string) (ModelPrice, bool) {
	c.mu.Lock()
	hit, cached := c.cache[model]
	c.mu.Unlock()
	if cached {
		return hit.price, hit.ok
	}
	// The lookup runs unlocked so a slow pricer does not serialise callers.
	for _, p := range c.pricers {
		if price, ok := p.Find(model); ok {
			hit = priceLookup{price: price, ok: true}
			break
		}
	}
	c.mu.Lock()
	c.cache[model] = hit
	c.mu.Unlock()
	return hit.price, hit.ok
}

// Reset drops the memoised results.
func (c *ChainPricer) Reset() {
	c.mu.Lock()
	clear(c.cache)
	c.mu.Unlock()
}

// LiteLLMPricer prices models from LiteLLM data, starting from the embedded
// Claude-only snapshot. Lookups follow ccusage: an exact key first, then the
// longest key that matches the model on name boundaries.
type LiteLLMPricer struct {
	mu      sync.RWMutex
	entries map[string]ModelPrice
	// keys holds entries' keys, sorted, so the fuzzy scan is deterministic.
	keys  []string
	cache map[string]priceLookup
}

// NewLiteLLMPricer returns a pricer loaded from the embedded snapshot.
func NewLiteLLMPricer() *LiteLLMPricer {
	p := &LiteLLMPricer{entries: map[string]ModelPrice{}, cache: map[string]priceLookup{}}
	if _, err := p.load(embeddedLiteLLM); err != nil {
		// The snapshot is generated and covered by tests.
		panic(fmt.Sprintf("ccusage: embedded LiteLLM pricing: %v", err))
	}
	return p
}

// ParseLiteLLMPricing returns a pricer holding the Claude entries of a
// LiteLLM price document. It fails when no entry is usable.
func ParseLiteLLMPricing(data []byte) (*LiteLLMPricer, error) {
	p := &LiteLLMPricer{entries: map[string]ModelPrice{}, cache: map[string]priceLookup{}}
	if _, err := p.load(data); err != nil {
		return nil, err
	}
	return p, nil
}

// Len reports how many models the pricer holds.
func (p *LiteLLMPricer) Len() int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.entries)
}

// Find returns the price of model.
func (p *LiteLLMPricer) Find(model string) (ModelPrice, bool) {
	p.mu.RLock()
	hit, cached := p.cache[model]
	p.mu.RUnlock()
	if cached {
		return hit.price, hit.ok
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	hit = p.lookup(model)
	p.cache[model] = hit
	return hit.price, hit.ok
}

func (p *LiteLLMPricer) lookup(model string) priceLookup {
	if model == "" {
		return priceLookup{}
	}
	if price, ok := p.entries[model]; ok {
		return priceLookup{price: price, ok: true}
	}
	normalized := normalizedPricingKey(model)
	best := ""
	for _, key := range p.keys {
		if !pricingKeyMatches(key, model, normalized) {
			continue
		}
		// Longest key wins; keys are sorted, so on a tie the first stays.
		if len(key) > len(best) {
			best = key
		}
	}
	if best == "" {
		return priceLookup{}
	}
	return priceLookup{price: p.entries[best], ok: true}
}

// Refresh merges the current LiteLLM price table into the pricer, using
// client for the download. Entries the download lacks keep their embedded
// rates. On failure the pricer is unchanged. It is never called implicitly.
func (p *LiteLLMPricer) Refresh(ctx context.Context, client *http.Client) error {
	if client == nil {
		return errors.New("ccusage: pricing refresh needs an HTTP client")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, LiteLLMPricingURL, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ccusage: fetch LiteLLM pricing: %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxPricingBody+1))
	if err != nil {
		return err
	}
	if len(data) > maxPricingBody {
		return errors.New("ccusage: LiteLLM pricing document too large")
	}
	fresh, err := parseLiteLLMEntries(data)
	if err != nil {
		return err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	p.merge(fresh)
	return nil
}

func (p *LiteLLMPricer) load(data []byte) (int, error) {
	fresh, err := parseLiteLLMEntries(data)
	if err != nil {
		return 0, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.merge(fresh)
	return len(fresh), nil
}

// merge adds or replaces entries and drops the lookup cache. The caller holds
// p.mu.
func (p *LiteLLMPricer) merge(fresh map[string]ModelPrice) {
	for k, v := range fresh {
		p.entries[k] = v
	}
	p.keys = p.keys[:0]
	for k := range p.entries {
		p.keys = append(p.keys, k)
	}
	slices.Sort(p.keys)
	clear(p.cache)
}

// liteLLMEntry is the part of a LiteLLM model entry the pricer reads.
type liteLLMEntry struct {
	Input                *float64 `json:"input_cost_per_token"`
	Output               *float64 `json:"output_cost_per_token"`
	CacheCreate          *float64 `json:"cache_creation_input_token_cost"`
	CacheRead            *float64 `json:"cache_read_input_token_cost"`
	InputAbove200k       *float64 `json:"input_cost_per_token_above_200k_tokens"`
	OutputAbove200k      *float64 `json:"output_cost_per_token_above_200k_tokens"`
	CacheCreateAbove200k *float64 `json:"cache_creation_input_token_cost_above_200k_tokens"`
	CacheReadAbove200k   *float64 `json:"cache_read_input_token_cost_above_200k_tokens"`
	MaxInputTokens       *int64   `json:"max_input_tokens"`
	ProviderSpecific     *struct {
		Fast *float64 `json:"fast"`
	} `json:"provider_specific_entry"`
}

// parseLiteLLMEntries decodes the Claude entries of a LiteLLM document. As in
// ccusage, an entry that fails to decode or lacks an input or output rate is
// skipped, a missing cache-write rate defaults to input*1.25, a missing
// cache-read rate to input*0.1, and a missing fast multiplier comes from the
// built-in overrides, else 1.
func parseLiteLLMEntries(data []byte) (map[string]ModelPrice, error) {
	var raw map[string]jsontext.Value
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("ccusage: decode LiteLLM pricing: %w", err)
	}
	out := map[string]ModelPrice{}
	for model, value := range raw {
		if !isClaudeKey(model) {
			continue
		}
		var e liteLLMEntry
		if json.Unmarshal(value, &e) != nil || e.Input == nil || e.Output == nil {
			continue
		}
		price := ModelPrice{
			Input:                *e.Input,
			Output:               *e.Output,
			CacheCreate:          *e.Input * 1.25,
			CacheRead:            *e.Input * 0.1,
			InputAbove200k:       e.InputAbove200k,
			OutputAbove200k:      e.OutputAbove200k,
			CacheCreateAbove200k: e.CacheCreateAbove200k,
			CacheReadAbove200k:   e.CacheReadAbove200k,
			FastMultiplier:       1,
		}
		if e.CacheCreate != nil {
			price.CacheCreate = *e.CacheCreate
		}
		if e.CacheRead != nil {
			price.CacheRead = *e.CacheRead
		}
		if e.MaxInputTokens != nil {
			price.MaxInputTokens = *e.MaxInputTokens
		}
		if e.ProviderSpecific != nil && e.ProviderSpecific.Fast != nil {
			price.FastMultiplier = *e.ProviderSpecific.Fast
		} else if m, ok := fastMultiplierOverride(model); ok {
			price.FastMultiplier = m
		}
		out[model] = price
	}
	if len(out) == 0 {
		return nil, errors.New("ccusage: LiteLLM pricing has no usable Claude entries")
	}
	return out, nil
}

func isClaudeKey(key string) bool {
	for _, prefix := range claudeKeyPrefixes {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

// Fast-mode multipliers for models whose LiteLLM entry does not publish one,
// from ccusage's fast-multiplier-overrides.json. Exact names are checked
// first; a normalised prefix matches a model name ending in it or in it plus
// a "-" suffix.
var (
	fastMultiplierExact = map[string]float64{
		"gpt-5.6-sol":   2.0,
		"gpt-5.6-terra": 2.0,
		"gpt-5.6-luna":  2.0,
		"gpt-5.5":       2.5,
		"gpt-5.4":       2.0,
		"gpt-5.3-codex": 2.0,
		"gpt-6-astra":   2.0,
	}
	fastMultiplierPrefixes = []struct {
		base       string
		multiplier float64
	}{
		{"claude-opus-4-6", 6.0},
		{"claude-opus-4-7", 6.0},
		{"claude-opus-4-8", 2.0},
	}
)

func fastMultiplierOverride(model string) (float64, bool) {
	if m, ok := fastMultiplierExact[model]; ok {
		return m, true
	}
	for part := range strings.FieldsFuncSeq(model, func(r rune) bool { return r == '/' || r == ':' }) {
		if m, ok := fastMultiplierExact[part]; ok {
			return m, true
		}
		normalized := normalizedPricingKey(part)
		for _, o := range fastMultiplierPrefixes {
			if matchesModelSuffix(normalized, o.base) {
				return o.multiplier, true
			}
		}
	}
	return 0, false
}

// matchesModelSuffix reports whether part's last occurrence of base ends the
// string or is followed by "-".
func matchesModelSuffix(part, base string) bool {
	i := strings.LastIndex(part, base)
	if i < 0 {
		return false
	}
	rest := part[i+len(base):]
	return rest == "" || rest[0] == '-'
}

// pricingKeyMatches reports whether a pricing key names model, in either
// direction, on raw or separator-normalised spellings.
func pricingKeyMatches(candidate, model, normalizedModel string) bool {
	if containsPricingKey(model, candidate) || containsPricingKey(candidate, model) {
		return true
	}
	normalizedCandidate := normalizedPricingKey(candidate)
	return containsPricingKey(normalizedModel, normalizedCandidate) ||
		containsPricingKey(normalizedCandidate, normalizedModel)
}

// containsPricingKey finds key in value only between non-alphanumeric
// boundaries, and not when a numeric key is followed by a further version
// number. Occurrences are scanned without overlap, as Rust's match_indices
// does.
func containsPricingKey(value, key string) bool {
	if key == "" {
		return false
	}
	for offset := 0; offset <= len(value)-len(key); {
		i := strings.Index(value[offset:], key)
		if i < 0 {
			return false
		}
		i += offset
		if (i == 0 || isPricingKeyBoundary(value[i-1])) && suffixAllowsPricingKeyMatch(key, value[i+len(key):]) {
			return true
		}
		offset = i + len(key)
	}
	return false
}

func isPricingKeyBoundary(b byte) bool {
	return !isASCIIAlnum(b)
}

func isASCIIAlnum(b byte) bool {
	return b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

func isASCIIDigit(b byte) bool { return b >= '0' && b <= '9' }

func suffixAllowsPricingKeyMatch(key, suffix string) bool {
	if suffix == "" {
		return true
	}
	if !isPricingKeyBoundary(suffix[0]) {
		return false
	}
	return !suffixStartsWithNumericModelVersion(key, suffix)
}

// suffixStartsWithNumericModelVersion reports whether a key ending in a digit
// is followed by "-<digits>" or ".<digits>" other than an 8-digit date.
func suffixStartsWithNumericModelVersion(key, suffix string) bool {
	if key == "" || !isASCIIDigit(key[len(key)-1]) {
		return false
	}
	if suffix[0] != '-' && suffix[0] != '.' {
		return false
	}
	rest := suffix[1:]
	n := 0
	for n < len(rest) && isASCIIDigit(rest[n]) {
		n++
	}
	if n == 0 {
		return false
	}
	dateLike := n == dateSuffixDigits && (n == len(rest) || isPricingKeyBoundary(rest[n]))
	return !dateLike
}

// normalizedPricingKey maps the "." and "@" separator variants to "-".
func normalizedPricingKey(s string) string {
	if !strings.ContainsAny(s, ".@") {
		return s
	}
	return strings.Map(func(r rune) rune {
		if r == '.' || r == '@' {
			return '-'
		}
		return r
	}, s)
}
