package api

import (
	"strings"

	"antigravity-go-proxy/internal/claudecode"
	"antigravity-go-proxy/internal/config"
	"antigravity-go-proxy/internal/openrouter"
	"antigravity-go-proxy/internal/zen"
)

// gatewayOwnedModelIDs returns the IDs a dispatch-active gateway would accept.
// The Cloud Code catalog must not advertise an ID that a gateway wins, or the
// advertised owner disagrees with the dispatcher.
//
// The conditions mirror the dispatch handlers: each gateway's enabled flag,
// its allowlist entries' enabled flags, and (for Zen) the wire-subset gate.
// Matchers stay asymmetric on purpose — the same spellings each handler
// accepts (case variants, [1m] and opencode/ spellings) are owned here too —
// but key resolution is not: a keyless Zen config still owns its IDs in
// discovery, matching what the Zen appender below advertises.
func gatewayOwnedModelIDs(cfg config.Config) map[string]bool {
	owned := make(map[string]bool)
	if cfg.Kimi.Enabled {
		for _, item := range cfg.Kimi.Allowlist {
			if !item.Enabled {
				continue
			}
			for _, spelling := range []string{item.ID, item.Alias} {
				if strings.TrimSpace(spelling) == "" {
					continue
				}
				owned[spelling] = true
				owned[stripKimi1mSuffix(spelling)] = true
				owned[strings.ToLower(spelling)] = true
				owned[strings.ToLower(stripKimi1mSuffix(spelling))] = true
			}
		}
	}
	if cfg.Zen.Enabled {
		for _, item := range cfg.Zen.Allowlist {
			if !item.Enabled {
				continue
			}
			if !zen.IsForwardable(item.ID) {
				continue
			}
			for _, spelling := range []string{item.ID, item.Alias} {
				if strings.TrimSpace(spelling) == "" {
					continue
				}
				owned[spelling] = true
				owned[zen.StripOpencodePrefix(spelling)] = true
				owned[strings.ToLower(spelling)] = true
				owned[strings.ToLower(zen.StripOpencodePrefix(spelling))] = true
			}
		}
	}
	if cfg.ClaudeCode.Enabled {
		allowlist := cfg.ClaudeCode.Allowlist
		if len(allowlist) == 0 {
			allowlist = claudecode.DefaultAllowlist()
		}
		for _, item := range allowlist {
			if !item.Enabled {
				continue
			}
			owned[item.ID] = true
			owned[strings.ToLower(item.ID)] = true
			for _, alias := range item.ExpandAliases() {
				owned[alias] = true
				owned[strings.ToLower(alias)] = true
			}
		}
	}
	if cfg.OpenRouter.Enabled {
		for _, item := range cfg.OpenRouter.Allowlist {
			if !item.Enabled {
				continue
			}
			// The OpenRouter branch compares literally: only exact
			// spellings are owned.
			if item.ID != "" {
				owned[item.ID] = true
			}
			if item.Alias != "" {
				owned[item.Alias] = true
			}
		}
	}
	for model, endpoint := range cfg.CustomEndpoints {
		if endpoint.URL != "" {
			owned[model] = true
		}
	}
	return owned
}

// gatewayModelAppenders appends one gateway's models to the /v1/models list.
// Coverage is pinned by TestGatewayModelAppenders_CoverEveryKnownGateway:
// every known gateway has an entry, so a future gateway cannot merge without
// deciding what it advertises. custom contributes no entries (no discovery
// helper for custom endpoints exists, and this change does not add one) and
// cloudcode is the terminal path the catalog loop already covers; both are
// no-ops.
var gatewayModelAppenders = map[config.GatewayID]func(*Server, config.Config, *[]any, map[string]bool){
	config.GatewayKimi:       appendKimiDiscovery,
	config.GatewayZen:        appendZenDiscovery,
	config.GatewayClaudeCode: appendClaudeCodeDiscovery,
	config.GatewayOpenRouter: appendOpenRouterDiscovery,
	config.GatewayCustom:     func(*Server, config.Config, *[]any, map[string]bool) {},
	config.GatewayCloudCode:  func(*Server, config.Config, *[]any, map[string]bool) {},
}

// zenSeenKey collapses the two advertised spellings of one Zen model (the
// opencode/-prefixed spelling and the bare wire ID) onto one discovery key.
func zenSeenKey(id string) string {
	return strings.ToLower(zen.StripOpencodePrefix(id))
}

func appendClaudeCodeDiscovery(server *Server, cfg config.Config, models *[]any, seen map[string]bool) {
	// Unlike the pre-order discovery section, this appender gates on the
	// enabled flag alone: the dispatcher never routes to Claude Code when
	// disabled, so a disabled gateway with accounts present must not own
	// any advertised ID.
	if !cfg.ClaudeCode.Enabled {
		return
	}
	allowlist := cfg.ClaudeCode.Allowlist
	if len(allowlist) == 0 {
		allowlist = claudecode.DefaultAllowlist()
	}
	for _, item := range allowlist {
		if !item.Enabled {
			continue
		}
		desc := item.DisplayName
		if desc == "" {
			desc = item.ID
		}
		contextLen := item.ContextLen
		if contextLen <= 0 {
			contextLen = defaultDiscoveryContextWindow
		}
		maxOutput := item.MaxOutputTokens
		if maxOutput <= 0 {
			maxOutput = 8192
		}
		aliases := item.ExpandAliases()

		if !seen[item.ID] {
			entry := map[string]any{
				"id":                item.ID,
				"object":            "model",
				"created":           server.now().Unix(),
				"owned_by":          "anthropic",
				"description":       desc,
				"display_name":      desc,
				"context_window":    contextLen,
				"max_output_tokens": maxOutput,
				"supports_thinking": item.Thinking,
			}
			if len(aliases) > 0 {
				entry["aliases"] = aliases
			}
			*models = append(*models, entry)
			seen[item.ID] = true
		}

		for _, alias := range aliases {
			if alias != "" && alias != item.ID && !seen[alias] {
				*models = append(*models, map[string]any{
					"id":                alias,
					"object":            "model",
					"created":           server.now().Unix(),
					"owned_by":          "anthropic",
					"description":       desc + " (Alias)",
					"display_name":      desc + " (Alias)",
					"context_window":    contextLen,
					"max_output_tokens": maxOutput,
					"supports_thinking": item.Thinking,
				})
				seen[alias] = true
			}
		}
	}
}

func appendOpenRouterDiscovery(server *Server, cfg config.Config, models *[]any, seen map[string]bool) {
	if !cfg.OpenRouter.Enabled {
		return
	}
	// The catalog lookups below read the cache without refreshing it. The
	// startup warmup is asynchronous and silent on failure, so repair a
	// cold or expired cache here: discovery is often the first request a
	// client makes, and a miss otherwise pins every advertised limit to
	// the conservative defaults. Returns immediately when the cache is
	// valid.
	openrouter.DefaultClient.WarmupCacheAsync(cfg.OpenRouter.APIKey, cfg.OpenRouter.BaseURL)
	for _, item := range cfg.OpenRouter.Allowlist {
		if !item.Enabled {
			continue
		}
		desc := item.DisplayName
		if desc == "" {
			desc = item.ID
		}
		// Prefer the operator's manual override, then the live OpenRouter
		// catalog (so context_window/max_output_tokens reflect a model's
		// real capability, e.g. 1M context, automatically), then a
		// conservative fallback when neither is known.
		catalogContext, catalogMaxOutput, haveCatalog := openrouter.DefaultClient.GetModelLimits(item.ID)
		contextLen := item.ContextLen
		if contextLen <= 0 && haveCatalog {
			contextLen = catalogContext
		}
		if contextLen <= 0 {
			contextLen = defaultDiscoveryContextWindow
		}
		maxOutput := item.MaxOutputTokens
		if maxOutput <= 0 && haveCatalog {
			maxOutput = catalogMaxOutput
		}
		if maxOutput <= 0 {
			// Nothing states the output cap. Fall back to the context
			// window, but never above the conservative default: a large
			// context says nothing about how much a model may emit.
			maxOutput = contextLen
			if maxOutput > defaultDiscoveryMaxOutputTokens {
				maxOutput = defaultDiscoveryMaxOutputTokens
			}
		}
		if !seen[item.ID] {
			*models = append(*models, map[string]any{
				"id":                item.ID,
				"object":            "model",
				"created":           server.now().Unix(),
				"owned_by":          "openrouter",
				"description":       desc,
				"display_name":      desc,
				"context_window":    contextLen,
				"max_output_tokens": maxOutput,
				"supports_thinking": true,
			})
			seen[item.ID] = true
		}
		if item.Alias != "" && item.Alias != item.ID && !seen[item.Alias] {
			*models = append(*models, map[string]any{
				"id":                item.Alias,
				"object":            "model",
				"created":           server.now().Unix(),
				"owned_by":          "openrouter",
				"description":       desc + " (Alias)",
				"display_name":      desc + " (Alias)",
				"context_window":    contextLen,
				"max_output_tokens": maxOutput,
				"supports_thinking": true,
			})
			seen[item.Alias] = true
		}
	}
}

func appendKimiDiscovery(server *Server, cfg config.Config, models *[]any, seen map[string]bool) {
	if !cfg.Kimi.Enabled {
		return
	}
	for _, item := range cfg.Kimi.Allowlist {
		if !item.Enabled {
			continue
		}
		desc := item.DisplayName
		if desc == "" {
			desc = item.ID
		}
		contextLen := item.ContextLen
		if contextLen <= 0 {
			contextLen = defaultDiscoveryContextWindow
		}
		maxOutput := item.MaxOutputTokens
		if maxOutput <= 0 {
			// Nothing states the output cap. Fall back to the context
			// window, but never above the conservative default: a large
			// context says nothing about how much a model may emit.
			maxOutput = contextLen
			if maxOutput > defaultDiscoveryMaxOutputTokens {
				maxOutput = defaultDiscoveryMaxOutputTokens
			}
		}
		if !seen[item.ID] {
			*models = append(*models, map[string]any{
				"id":                item.ID,
				"object":            "model",
				"created":           server.now().Unix(),
				"owned_by":          "kimi",
				"description":       desc,
				"display_name":      desc,
				"context_window":    contextLen,
				"max_output_tokens": maxOutput,
				"supports_thinking": true,
			})
			seen[item.ID] = true
		}
		if item.Alias != "" && item.Alias != item.ID && !seen[item.Alias] {
			*models = append(*models, map[string]any{
				"id":                item.Alias,
				"object":            "model",
				"created":           server.now().Unix(),
				"owned_by":          "kimi",
				"description":       desc + " (Alias)",
				"display_name":      desc + " (Alias)",
				"context_window":    contextLen,
				"max_output_tokens": maxOutput,
				"supports_thinking": true,
			})
			seen[item.Alias] = true
		}
	}
}

func appendZenDiscovery(server *Server, cfg config.Config, models *[]any, seen map[string]bool) {
	if !cfg.Zen.Enabled {
		return
	}
	for _, item := range cfg.Zen.Allowlist {
		if !item.Enabled {
			continue
		}
		if !zen.IsForwardable(item.ID) {
			continue
		}
		desc := item.DisplayName
		if desc == "" {
			desc = item.ID
		}
		contextLen := item.ContextLen
		if contextLen <= 0 {
			contextLen = defaultDiscoveryContextWindow
		}
		maxOutput := item.MaxOutputTokens
		if maxOutput <= 0 {
			// Zen's catalog states no output cap, but the forward path fills
			// max_tokens from this same constant — advertise what we will send.
			maxOutput = zen.DefaultMaxOutputTokens
		}
		// Both spellings of one Zen model (the opencode/-prefixed
		// spelling and the bare wire ID) share one seen key, or the pair
		// would advertise as two entries.
		if key := zenSeenKey(item.ID); !seen[key] {
			*models = append(*models, map[string]any{
				"id":                item.ID,
				"object":            "model",
				"created":           server.now().Unix(),
				"owned_by":          "zen",
				"description":       desc,
				"display_name":      desc,
				"context_window":    contextLen,
				"max_output_tokens": maxOutput,
				"supports_thinking": true,
			})
			seen[key] = true
		}
		if item.Alias != "" && item.Alias != item.ID {
			if key := zenSeenKey(item.Alias); !seen[key] {
				*models = append(*models, map[string]any{
					"id":                item.Alias,
					"object":            "model",
					"created":           server.now().Unix(),
					"owned_by":          "zen",
					"description":       desc + " (Alias)",
					"display_name":      desc + " (Alias)",
					"context_window":    contextLen,
					"max_output_tokens": maxOutput,
					"supports_thinking": true,
				})
				seen[key] = true
			}
		}
	}
}
