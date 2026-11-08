// Package tokencount estimates the input tokens of an Anthropic Messages
// request for gateways that have no count_tokens endpoint (Cloud Code, Zen).
// It learns a chars-per-token ratio per model family from the real input
// counts upstreams report, and prices images per image.
package tokencount

import (
	"strings"
	"sync"
	"time"
)

const (
	// startCharsPerToken is a family's ratio before its first observation.
	startCharsPerToken = 4.0
	// ratioAlpha is the weight of one observation in the moving average.
	ratioAlpha = 0.2
	// minLearnChars keeps short requests out of the ratio: their real count
	// is mostly overhead the proxy and the upstream add (system prompt,
	// tool framing), which says little about the text's own ratio.
	minLearnChars = 1000
)

// Family names the models that share one chars-per-token ratio. Every model
// outside the Claude, Gemini and gpt-oss families learns its own.
func Family(model string) string {
	switch {
	case strings.Contains(model, "claude"):
		return "claude"
	case strings.Contains(model, "gemini"):
		return "gemini"
	case strings.Contains(model, "gpt-oss"):
		return "gpt-oss"
	}
	return model
}

// Counter learns from requests an upstream answered and estimates requests
// that are never sent. It is safe for concurrent use.
type Counter struct {
	now func() time.Time

	mu     sync.Mutex
	ratios map[string]float64 // family -> chars per token
}

// New returns an empty Counter. now is its clock; nil means time.Now.
func New(now func() time.Time) *Counter {
	if now == nil {
		now = time.Now
	}
	return &Counter{now: now, ratios: make(map[string]float64)}
}

// Observe learns from a request an upstream answered. realInput is the input
// the upstream reported for it, cache reads and writes included. Image tokens
// are taken out first: they are priced per image, not by the ratio.
func (c *Counter) Observe(session, model string, request map[string]any, realInput int) {
	measured := measureRequest(pricingFor(model), request)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.learnLocked(Family(model), measured, realInput)
}

// Estimate returns the input tokens request would count for model.
func (c *Counter) Estimate(session, model string, request map[string]any) int {
	measured := measureRequest(pricingFor(model), request)
	c.mu.Lock()
	ratio := c.ratioLocked(Family(model))
	c.mu.Unlock()
	return measured.tokens(ratio)
}

func (c *Counter) ratioLocked(family string) float64 {
	if ratio, ok := c.ratios[family]; ok {
		return ratio
	}
	return startCharsPerToken
}

func (c *Counter) learnLocked(family string, measured measurement, realInput int) {
	textTokens := realInput - measured.imageTokens
	if measured.chars < minLearnChars || textTokens <= 0 {
		return
	}
	observed := float64(measured.chars) / float64(textTokens)
	c.ratios[family] = (1-ratioAlpha)*c.ratioLocked(family) + ratioAlpha*observed
}
