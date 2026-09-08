package openrouter

import "strings"

// Tool-choice modes a request can ask for, normalized across the Anthropic
// (`{"type":"any"}`) and OpenAI (`"required"`) request shapes.
const (
	ToolChoiceAuto     = "auto"
	ToolChoiceRequired = "required"
	ToolChoiceNone     = "none"
	ToolChoiceFunction = "function"
)

// ToolChoiceSupport mirrors OpenRouter's per-endpoint `supports_tool_choice`
// object. An endpoint may list the `tools` parameter yet reject forced choices.
type ToolChoiceSupport struct {
	None     bool `json:"none"`
	Auto     bool `json:"auto"`
	Required bool `json:"required"`
	Function bool `json:"function"`
}

// ToolRequirements is what a request needs from an endpoint. OpenRouter routes
// a request only to endpoints that can serve every capability the body uses;
// pinning a provider that cannot (with allow_fallbacks:false) empties the
// endpoint list and returns 404 "No endpoints found for <model>".
type ToolRequirements struct {
	Tools      bool
	ToolChoice string
}

// Empty reports whether the request constrains provider choice at all.
func (r ToolRequirements) Empty() bool { return !r.Tools && r.ToolChoice == "" }

// ToolRequirementsFromAnthropic derives requirements from a parsed request
// body. It accepts the Anthropic tool_choice object and the OpenAI string form.
func ToolRequirementsFromAnthropic(req map[string]any) ToolRequirements {
	var need ToolRequirements
	if tools, ok := req["tools"].([]any); ok && len(tools) > 0 {
		need.Tools = true
	}
	if !need.Tools {
		return need
	}

	switch tc := req["tool_choice"].(type) {
	case string:
		need.ToolChoice = normalizeToolChoice(tc)
	case map[string]any:
		if s, ok := tc["type"].(string); ok {
			need.ToolChoice = normalizeToolChoice(s)
		}
	}
	return need
}

// normalizeToolChoice maps both request dialects onto the OpenRouter flags.
func normalizeToolChoice(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "auto":
		return ToolChoiceAuto
	case "any", "required":
		return ToolChoiceRequired
	case "none":
		return ToolChoiceNone
	case "tool", "function":
		return ToolChoiceFunction
	default:
		return ""
	}
}

// SupportsParameter reports whether the endpoint advertises a request parameter.
func (e *ProviderEndpoint) SupportsParameter(name string) bool {
	if e == nil {
		return false
	}
	for _, p := range e.SupportedParameters {
		if strings.EqualFold(p, name) {
			return true
		}
	}
	return false
}

// SupportsRequirements reports whether the endpoint can serve a request with
// the given tool requirements. Missing metadata fails open: an endpoint that
// advertises the parameters but omits `supports_tool_choice` is assumed able,
// so an incomplete catalog never strands a request.
func (e *ProviderEndpoint) SupportsRequirements(need ToolRequirements) bool {
	if need.Empty() {
		return true
	}
	if e == nil {
		return false
	}
	// An endpoint with no advertised parameters at all tells us nothing.
	if len(e.SupportedParameters) == 0 {
		return true
	}
	if need.Tools && !e.SupportsParameter("tools") {
		return false
	}
	if need.ToolChoice == "" {
		return true
	}
	if e.SupportsToolChoice == nil {
		return e.SupportsParameter("tool_choice")
	}
	switch need.ToolChoice {
	case ToolChoiceAuto:
		return e.SupportsToolChoice.Auto
	case ToolChoiceRequired:
		return e.SupportsToolChoice.Required
	case ToolChoiceNone:
		return e.SupportsToolChoice.None
	case ToolChoiceFunction:
		return e.SupportsToolChoice.Function
	}
	return true
}

// FilterCapable narrows a failover chain to providers whose endpoint metadata
// can serve the request. When every candidate is incapable — the pinned-provider
// case — it substitutes the ranked capable providers instead of letting the
// request 404. In "custom" mode the substitution stays inside the configured
// order: that list is an operator allowlist, and routing outside it would send
// traffic to a provider they deliberately excluded. Providers with no known
// endpoint, and models with no ranks, pass through untouched.
func (r *ProviderRouter) FilterCapable(model string, candidates []string, need ToolRequirements, order ProviderOrder) []string {
	if need.Empty() || len(candidates) == 0 {
		return candidates
	}

	// Read-only: the filter never mutates router state, and
	// providerHealthyUnderThresholdLocked only reads ranks, stats and cfg.
	// A write lock here would serialize every tool-carrying request against
	// SelectChain and RecordResult.
	r.mu.RLock()
	defer r.mu.RUnlock()

	ranks := r.ranks[model]
	if len(ranks) == 0 {
		return candidates
	}
	// A provider can appear once per endpoint variant (quantization, tag), and
	// provider.order names only the provider — OpenRouter then picks a variant
	// within it. So a provider qualifies when ANY of its endpoints can serve the
	// request; collapsing to a single endpoint would make the verdict depend on
	// rank order and drop providers whose top variant is capable.
	capable := make(map[string]bool, len(ranks))
	for _, rk := range ranks {
		name := rk.endpoint.ProviderName
		if !capable[name] {
			capable[name] = rk.endpoint.SupportsRequirements(need)
		}
	}

	isCapable := func(p string) bool {
		if c, ok := capable[p]; ok {
			return c
		}
		return true // unknown provider: no basis to exclude it
	}

	out := make([]string, 0, len(candidates))
	for _, p := range candidates {
		if p == "" || isCapable(p) {
			out = append(out, p)
		}
	}
	if len(out) > 0 {
		return out
	}

	// Every candidate is incapable: fall back to ranked capable providers,
	// bounded by the operator allowlist when one was configured. In "custom"
	// mode walk the allowlist itself — the fallback must hand back the
	// operator's precedence, not the rank order.
	source := make([]string, 0, len(ranks))
	for _, rk := range ranks {
		source = append(source, rk.endpoint.ProviderName)
	}
	if order.Mode == "custom" && len(order.Order) > 0 {
		ordered := make([]string, 0, len(order.Order))
		seenOrder := map[string]bool{}
		for _, p := range order.Order {
			if p != "" && !seenOrder[p] {
				seenOrder[p] = true
				ordered = append(ordered, p)
			}
		}
		source = ordered
	}
	seen := map[string]bool{}
	for _, p := range source {
		if p == "" || seen[p] || !isCapable(p) {
			continue
		}
		if !r.providerHealthyUnderThresholdLocked(model, p) {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}
