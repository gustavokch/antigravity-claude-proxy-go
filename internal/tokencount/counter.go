// Package tokencount estimates the input tokens of an Anthropic Messages
// request for gateways that have no count_tokens endpoint (Cloud Code, Zen).
// It learns a chars-per-token ratio per model family from the real input
// counts upstreams report, and prices images per image.
package tokencount

import (
	"container/list"
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
	// minObservedRatio and maxObservedRatio bound what one request may claim
	// the ratio is: a value outside them says more about tokens the measure
	// mispriced than about the text, and one such request must not move the
	// ratio far.
	minObservedRatio = 0.5
	maxObservedRatio = 8.0
)

// Family names the models that share one chars-per-token ratio. Every model
// outside the Claude, Gemini and gpt-oss families learns its own. Claude is
// two families: Anthropic documents the tokenizer of Claude 4.7 as producing
// 1.0–1.35× the tokens of the earlier one, so 4.7 and every later Claude
// (claude-4.7) learn apart from the models before it (claude).
func Family(model string) string {
	switch {
	case strings.Contains(model, "claude"):
		if claude47OrLater(model) {
			return "claude-4.7"
		}
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

	mu            sync.Mutex
	ratios        map[string]float64       // family -> chars per token
	conversations map[string]*list.Element // memoryKey -> element of order
	order         *list.List               // conversation values, latest observation first
}

// New returns an empty Counter. now is its clock; nil means time.Now.
func New(now func() time.Time) *Counter {
	if now == nil {
		now = time.Now
	}
	return &Counter{
		now:           now,
		ratios:        make(map[string]float64),
		conversations: make(map[string]*list.Element),
		order:         list.New(),
	}
}

// Observe learns from a request an upstream answered. realInput is the input
// the upstream reported for it, cache reads and writes included. Image tokens
// are taken out first: they are priced per image, not by the ratio. With a
// session, the request also becomes that conversation's memory for Estimate.
func (c *Counter) Observe(session, model string, request map[string]any, realInput int) {
	family := Family(model)
	measured := measureRequest(pricingFor(model), request)
	remember := session != "" && realInput > 0
	var memory conversation
	if remember {
		memory = newConversation(memoryKey(session, family), request, realInput)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.learnLocked(family, measured, realInput)
	if remember {
		c.rememberLocked(memory, c.now())
	}
}

// Estimate returns the input tokens request would count for model. When it
// continues the last request observed in its session, with the same system
// and tools, the estimate is that request's real count plus an estimate of
// the messages after it; otherwise the whole request is estimated. Estimate
// does not keep a conversation alive: only Observe does.
func (c *Counter) Estimate(session, model string, request map[string]any) int {
	family := Family(model)
	pricing := pricingFor(model)
	c.mu.Lock()
	ratio := c.ratioLocked(family)
	var memory conversation
	found := false
	if session != "" {
		memory, found = c.recallLocked(memoryKey(session, family), c.now())
	}
	c.mu.Unlock()
	messages, _ := request["messages"].([]any)
	if found && memory.continuedBy(request, messages) {
		return memory.realInput + measureMessages(pricing, messages[memory.messages:]).tokens(ratio)
	}
	return measureRequest(pricing, request).tokens(ratio)
}

func (c *Counter) ratioLocked(family string) float64 {
	if ratio, ok := c.ratios[family]; ok {
		return ratio
	}
	return startCharsPerToken
}

// learnLocked moves family's ratio towards the one measured observed. A
// request teaches nothing when it holds content the measure does not price,
// when it is short, or when images are half of realInput or more: an error in
// their price would then be most of what is left for the text.
func (c *Counter) learnLocked(family string, measured measurement, realInput int) {
	if measured.unpriced || measured.chars < minLearnChars || measured.imageTokens*2 >= realInput {
		return
	}
	textTokens := realInput - measured.imageTokens
	observed := float64(measured.chars) / float64(textTokens)
	observed = min(max(observed, minObservedRatio), maxObservedRatio)
	c.ratios[family] = (1-ratioAlpha)*c.ratioLocked(family) + ratioAlpha*observed
}
