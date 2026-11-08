package codex

import "strings"

// defaultMaxOutputTokens is the output budget every default entry advertises.
const defaultMaxOutputTokens = 128000

// DefaultAllowlist returns the models the Codex CLI lists in its picker: the
// visibility "list" entries of CLIProxyAPI's codex_client_models.json, with
// their context windows, reasoning levels and default level. Hidden entries
// (gpt-6-chat-thinking, gpt-reserve, codex-auto-review) are left out because
// the official client never offers them.
func DefaultAllowlist() []ModelConfig {
	withUltra := []string{"low", "medium", "high", "xhigh", "max", "ultra"}
	toMax := []string{"low", "medium", "high", "xhigh", "max"}
	entry := func(id, name string, ctx int, efforts []string, def string) ModelConfig {
		return ModelConfig{
			ID:              id,
			DisplayName:     name,
			ContextLength:   ctx,
			MaxOutputTokens: defaultMaxOutputTokens,
			Efforts:         append([]string(nil), efforts...),
			DefaultEffort:   def,
			Enabled:         true,
		}
	}
	return []ModelConfig{
		entry("gpt-6.1-sol", "GPT-6.1-Sol", 500000, withUltra, "low"),
		entry("gpt-6.1-terra", "GPT-6.1 Terra", 500000, withUltra, "low"),
		entry("gpt-6-astra", "GPT-6-Astra", 272000, withUltra, "medium"),
		entry("gpt-6-sol", "GPT-6-Sol", 272000, withUltra, "medium"),
		entry("gpt-6-luna", "GPT-6-Luna", 272000, toMax, "medium"),
		entry("gpt-5.6-sol", "GPT-5.6-Sol", 272000, withUltra, "low"),
		entry("gpt-5.6-terra", "GPT-5.6-Terra", 272000, withUltra, "medium"),
		entry("gpt-5.6-luna", "GPT-5.6-Luna", 272000, toMax, "medium"),
		// The catalog's gpt-5.5 entry is a clone of gpt-6.1-sol and still
		// carries that display name; the id is what the backend routes on.
		entry("gpt-5.5", "GPT-5.5", 500000, withUltra, "low"),
	}
}

// ResolveModel finds the enabled allowlist entry for a requested model: an exact
// id, an exact alias, an id differing only in case, then an alias differing
// only in case.
func ResolveModel(allowlist []ModelConfig, requested string) (ModelConfig, bool) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return ModelConfig{}, false
	}
	for _, same := range []func(a, b string) bool{
		func(a, b string) bool { return a == b },
		strings.EqualFold,
	} {
		for _, m := range allowlist {
			if m.Enabled && same(strings.TrimSpace(m.ID), requested) {
				return m, true
			}
		}
		for _, m := range allowlist {
			if !m.Enabled {
				continue
			}
			for _, a := range m.ExpandAliases() {
				if same(a, requested) {
					return m, true
				}
			}
		}
	}
	return ModelConfig{}, false
}

// effortRank orders the reasoning levels the clamp can land on. "none" is
// absent on purpose: a model that offers it runs it only when asked for it by
// name, never because a low request was rounded down to it.
var effortRank = map[string]int{
	"minimal": 0,
	"low":     1,
	"medium":  2,
	"high":    3,
	"xhigh":   4,
	"max":     5,
	"ultra":   6,
}

// clampEffort maps a requested level onto the levels a model publishes: the
// highest published level at or below the request (CONTEXT.md, Effort Level),
// else the lowest published level when the request is below all of them. A
// model with no published levels gets the request as is, except minimal, which
// only some models offer and becomes low.
func clampEffort(level string, efforts []string) string {
	rank, known := effortRank[level]
	best, bestRank := "", -1
	lowest, lowestRank := "", len(effortRank)
	for _, e := range efforts {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == level {
			return e
		}
		r, ok := effortRank[e]
		if !ok {
			continue
		}
		if known && r <= rank && r > bestRank {
			best, bestRank = e, r
		}
		if r < lowestRank {
			lowest, lowestRank = e, r
		}
	}
	switch {
	case best != "":
		return best
	case known && lowest != "":
		return lowest
	case level == "minimal":
		return "low"
	}
	return level
}
