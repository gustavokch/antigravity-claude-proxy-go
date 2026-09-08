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
// request 404. Providers with no known endpoint, and models with no ranks, pass
// through untouched.
func (r *ProviderRouter) FilterCapable(model string, candidates []string, need ToolRequirements) []string {
	if need.Empty() || len(candidates) == 0 {
		return candidates
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	ranks := r.ranks[model]
	if len(ranks) == 0 {
		return candidates
	}
	known := make(map[string]ProviderEndpoint, len(ranks))
	for _, rk := range ranks {
		known[rk.endpoint.ProviderName] = rk.endpoint
	}

	capable := func(p string) bool {
		ep, ok := known[p]
		if !ok {
			return true // unknown provider: no basis to exclude it
		}
		return ep.SupportsRequirements(need)
	}

	out := make([]string, 0, len(candidates))
	for _, p := range candidates {
		if p == "" || capable(p) {
			out = append(out, p)
		}
	}
	if len(out) > 0 {
		return out
	}

	// Every candidate is incapable: fall back to ranked capable providers.
	seen := map[string]bool{}
	for _, rk := range ranks {
		p := rk.endpoint.ProviderName
		if p == "" || seen[p] || !capable(p) {
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
