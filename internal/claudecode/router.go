package claudecode

import (
	"strings"
	"sync"
)

// DefaultAllowlist returns the standard built-in model allowlist for Claude Code gateway.
func DefaultAllowlist() []ModelConfig {
	return []ModelConfig{
		{
			ID:              "claude-fable-5",
			Alias:           "claude-fable-5",
			DisplayName:     "Claude Fable 5",
			ContextLen:      200000,
			MaxOutputTokens: 8192,
			Enabled:         true,
		},
		{
			ID:              "claude-opus-5",
			Alias:           "claude-opus-5",
			DisplayName:     "Claude Opus 5",
			ContextLen:      200000,
			MaxOutputTokens: 8192,
			Enabled:         true,
		},
		{
			ID:              "claude-sonnet-5",
			Alias:           "claude-sonnet-5",
			DisplayName:     "Claude Sonnet 5",
			ContextLen:      200000,
			MaxOutputTokens: 8192,
			Enabled:         true,
		},
		{
			ID:              "claude-haiku-4-5-20251001",
			Alias:           "claude-haiku-4-5",
			DisplayName:     "Claude Haiku 4.5",
			ContextLen:      200000,
			MaxOutputTokens: 8192,
			Enabled:         true,
		},
		{
			ID:              "claude-3-7-sonnet-20250219",
			Alias:           "claude-3-7-sonnet",
			DisplayName:     "Claude 3.7 Sonnet",
			ContextLen:      200000,
			MaxOutputTokens: 8192,
			Enabled:         true,
		},
		{
			ID:              "claude-3-5-sonnet-20241022",
			Alias:           "claude-3-5-sonnet",
			DisplayName:     "Claude 3.5 Sonnet",
			ContextLen:      200000,
			MaxOutputTokens: 8192,
			Enabled:         true,
		},
		{
			ID:              "claude-3-5-haiku-20241022",
			Alias:           "claude-3-5-haiku",
			DisplayName:     "Claude 3.5 Haiku",
			ContextLen:      200000,
			MaxOutputTokens: 8192,
			Enabled:         true,
		},
		{
			ID:              "claude-3-opus-20240229",
			Alias:           "claude-3-opus",
			DisplayName:     "Claude 3 Opus",
			ContextLen:      200000,
			MaxOutputTokens: 4096,
			Enabled:         true,
		},
	}
}

// Router provides thread-safe model matching, alias resolution, and allowlist checks.
type Router struct {
	mu        sync.RWMutex
	allowlist map[string]ModelConfig
	aliases   map[string]string
}

// NewRouter initializes a Router with the provided allowlist or defaults.
func NewRouter(models []ModelConfig) *Router {
	r := &Router{
		allowlist: make(map[string]ModelConfig),
		aliases:   make(map[string]string),
	}
	if len(models) == 0 {
		models = DefaultAllowlist()
	}
	r.UpdateAllowlist(models)
	return r
}

// UpdateAllowlist updates the active allowlist and re-indexes aliases.
func (r *Router) UpdateAllowlist(models []ModelConfig) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.allowlist = make(map[string]ModelConfig)
	r.aliases = make(map[string]string)

	for _, m := range models {
		if !m.Enabled {
			continue
		}
		id := strings.ToLower(strings.TrimSpace(m.ID))
		r.allowlist[id] = m

		if m.Alias != "" {
			alias := strings.ToLower(strings.TrimSpace(m.Alias))
			r.aliases[alias] = id
		}
	}
}

// IsModelAllowed checks if the requested model name (or alias) is enabled in the allowlist.
func (r *Router) IsModelAllowed(requested string) bool {
	_, found := r.ResolveModel(requested)
	return found
}

// ResolveModel maps a requested model ID or alias to the canonical model ID.
func (r *Router) ResolveModel(requested string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	req := strings.ToLower(strings.TrimSpace(requested))
	if req == "" {
		return "", false
	}

	// 1. Direct ID match
	if _, ok := r.allowlist[req]; ok {
		return req, true
	}

	// 2. Alias match
	if canonical, ok := r.aliases[req]; ok {
		return canonical, true
	}

	// 3. Prefix matching against known canonical IDs or aliases (e.g. model with suffix)
	for id := range r.allowlist {
		if strings.HasPrefix(req, id) {
			return id, true
		}
	}
	for alias, id := range r.aliases {
		if strings.HasPrefix(req, alias) {
			return id, true
		}
	}

	return "", false
}

// GetAllowedModels returns a slice of currently enabled models.
func (r *Router) GetAllowedModels() []ModelConfig {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]ModelConfig, 0, len(r.allowlist))
	for _, m := range r.allowlist {
		out = append(out, m)
	}
	return out
}
