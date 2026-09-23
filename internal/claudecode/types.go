package claudecode

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"antigravity-go-proxy/internal/ccidentity"
)

// DefaultBaseURL is the official Anthropic API endpoint.
const DefaultBaseURL = "https://api.anthropic.com"

// AccountConfig defines the persistent configuration for a Claude Code account.
type AccountConfig struct {
	ID               string     `json:"id"`
	Name             string     `json:"name"`
	Token            string     `json:"token"`
	RefreshToken     string     `json:"refreshToken,omitempty"`
	ExpiresAt        *time.Time `json:"expiresAt,omitempty"`
	Email            string     `json:"email,omitempty"`
	AccountUUID      string     `json:"accountUuid,omitempty"`
	OrganizationUUID string     `json:"organizationUuid,omitempty"`
	Type             string     `json:"type"`     // "oauth", "setup_token", or "api_key"
	Priority         int        `json:"priority"` // Lower number = higher priority
	Enabled          bool       `json:"enabled"`
	Source           string     `json:"source,omitempty"` // "oauth", "manual", "auto_import", "cli"
}

// ModelConfig defines a supported Claude model and its routing attributes.
type ModelConfig struct {
	ID              string   `json:"id"`
	Alias           string   `json:"alias,omitempty"`
	Aliases         []string `json:"aliases,omitempty"`
	DisplayName     string   `json:"displayName,omitempty"`
	ContextLen      int      `json:"contextLength,omitempty"`
	MaxOutputTokens int      `json:"maxOutputTokens,omitempty"`
	Thinking        bool     `json:"thinking,omitempty"`
	Enabled         bool     `json:"enabled"`
}

// ExpandAliases returns every alias configured on m: the comma-separated
// Alias field split into individual entries, followed by the explicit
// Aliases list. Entries are trimmed, empty entries dropped, and duplicates
// removed case-insensitively (first occurrence wins, original case kept).
func (m ModelConfig) ExpandAliases() []string {
	seen := make(map[string]struct{})
	var out []string
	add := func(a string) {
		a = strings.TrimSpace(a)
		if a == "" {
			return
		}
		key := strings.ToLower(a)
		if _, dup := seen[key]; dup {
			return
		}
		seen[key] = struct{}{}
		out = append(out, a)
	}
	for _, part := range strings.Split(m.Alias, ",") {
		add(part)
	}
	for _, a := range m.Aliases {
		add(a)
	}
	return out
}

// RoutingConfig holds resilience, backoff, and retry parameters.
type RoutingConfig struct {
	Retry429Max      int `json:"retry429Max,omitempty"`      // Default: 5
	BackoffBaseMs    int `json:"backoffBaseMs,omitempty"`    // Default: 1000
	BackoffCapMs     int `json:"backoffCapMs,omitempty"`     // Default: 30000
	RequestBudgetMs  int `json:"requestBudgetMs,omitempty"`  // Default: 120000
	CooldownDuration int `json:"cooldownDuration,omitempty"` // Default: 30000 ms
}

// DefaultRoutingConfig returns sensible production defaults for Claude Code gateway.
func DefaultRoutingConfig() RoutingConfig {
	return RoutingConfig{
		Retry429Max:      5,
		BackoffBaseMs:    1000,
		BackoffCapMs:     30000,
		RequestBudgetMs:  120000,
		CooldownDuration: 30000,
	}
}

// IdentityConfig controls whether outbound requests are rewritten to the
// captured Claude Code wire identity.
//
// Disabled is a disable flag rather than an enable flag on purpose: the zero
// value must mean "normalize", because the feature exists so a foreign harness
// is not gated, and a zero-value-means-off bool would ship it silently inert.
//
// The overrides exist because three captured values are environment-specific
// (X-Stainless-OS and X-Stainless-Runtime-Version came from a Linux container)
// or still unverified (Entrypoint: only sdk-cli has ever been captured).
// Empty means use the captured value.
type IdentityConfig struct {
	Disabled                bool   `json:"disabled,omitempty"`
	ClientVersion           string `json:"clientVersion,omitempty"`
	Entrypoint              string `json:"entrypoint,omitempty"`
	TurnOrigin              string `json:"turnOrigin,omitempty"`
	UserAgent               string `json:"userAgent,omitempty"`
	StainlessOS             string `json:"stainlessOs,omitempty"`
	StainlessRuntimeVersion string `json:"stainlessRuntimeVersion,omitempty"`
}

// Validate reports the first override that cannot be sent as an HTTP header
// value.
//
// All six overrides end up in header values or in the system-block marker. Go's
// transport refuses a value containing a control character with "invalid header
// field value", so an unchecked CR or LF here would break every request to this
// gateway with an error that names neither the field nor where it was set.
// Refusing at save time puts the message where the operator is.
func (c IdentityConfig) Validate() error {
	for _, field := range []struct {
		name  string
		value string
	}{
		{"clientVersion", c.ClientVersion},
		{"entrypoint", c.Entrypoint},
		{"turnOrigin", c.TurnOrigin},
		{"userAgent", c.UserAgent},
		{"stainlessOs", c.StainlessOS},
		{"stainlessRuntimeVersion", c.StainlessRuntimeVersion},
	} {
		for _, r := range field.value {
			if r < 0x20 || r == 0x7f {
				return fmt.Errorf(
					"identity.%s contains a control character (%q); it is sent as an HTTP header value",
					field.name, r)
			}
		}
	}
	return nil
}

// Identity builds the wire identity from this configuration.
//
// It returns ok=false when normalization is disabled. accountUUID may be empty:
// the capture recorded metadata.user_id carrying an EMPTY account_uuid in every
// request, so an endpoint with no pooled account reproduces that faithfully
// rather than inventing one.
func (c IdentityConfig) Identity(accountUUID, sessionKey string) (ccidentity.Identity, bool) {
	if c.Disabled {
		return ccidentity.Identity{}, false
	}
	return ccidentity.Identity{
		AccountUUID:             accountUUID,
		SessionKey:              sessionKey,
		ClientVersion:           c.ClientVersion,
		Entrypoint:              c.Entrypoint,
		TurnOrigin:              c.TurnOrigin,
		UserAgent:               c.UserAgent,
		StainlessOS:             c.StainlessOS,
		StainlessRuntimeVersion: c.StainlessRuntimeVersion,
	}, true
}

// SpoofIdentity returns the wire identity to send for one request of the pooled
// Claude Code gateway, and whether normalization is enabled at all.
//
// accountUUID and sessionKey are what the request already knows: the account
// chosen by the pool, and the session key used for stickiness. Reusing the
// session key keeps the spoofed session UUID consistent with that routing rather
// than inventing a second, unrelated notion of session.
func (c Config) SpoofIdentity(accountUUID, sessionKey string) (ccidentity.Identity, bool) {
	return c.Identity.Identity(accountUUID, sessionKey)
}

// Config is the root configuration structure for the Claude Code subsystem.
type Config struct {
	Enabled    bool            `json:"enabled"`
	BaseURL    string          `json:"baseUrl"`    // Default: "https://api.anthropic.com"
	Mode       string          `json:"mode"`       // "pool" or "single"
	AutoImport bool            `json:"autoImport"` // Default: false
	Accounts   []AccountConfig `json:"accounts,omitempty"`
	Allowlist  []ModelConfig   `json:"allowlist,omitempty"`
	Routing    RoutingConfig   `json:"routing,omitempty"`
	Identity   IdentityConfig  `json:"identity,omitempty"`
}

// RateLimits tracks Anthropic API rate limits extracted from response headers.
type RateLimits struct {
	RequestsLimit         int64     `json:"requestsLimit"`
	RequestsRemaining     int64     `json:"requestsRemaining"`
	RequestsReset         time.Time `json:"requestsReset"`
	TokensLimit           int64     `json:"tokensLimit"`
	TokensRemaining       int64     `json:"tokensRemaining"`
	TokensReset           time.Time `json:"tokensReset"`
	InputTokensLimit      int64     `json:"inputTokensLimit,omitempty"`
	InputTokensRemaining  int64     `json:"inputTokensRemaining,omitempty"`
	InputTokensReset      time.Time `json:"inputTokensReset,omitempty"`
	OutputTokensLimit     int64     `json:"outputTokensLimit,omitempty"`
	OutputTokensRemaining int64     `json:"outputTokensRemaining,omitempty"`
	OutputTokensReset     time.Time `json:"outputTokensReset,omitempty"`
	RetryAfter            int       `json:"retryAfter,omitempty"` // Seconds
	LastUpdated           time.Time `json:"lastUpdated"`
}

// Account represents an active, runtime-managed Claude Code credential.
type Account struct {
	mu sync.RWMutex

	ID               string     `json:"id"`
	Name             string     `json:"name"`
	Token            string     `json:"token"`
	RefreshToken     string     `json:"refreshToken,omitempty"`
	ExpiresAt        *time.Time `json:"expiresAt,omitempty"`
	Email            string     `json:"email,omitempty"`
	AccountUUID      string     `json:"accountUuid,omitempty"`
	OrganizationUUID string     `json:"organizationUuid,omitempty"`
	Type             string     `json:"type"`
	Priority         int        `json:"priority"`
	Enabled          bool       `json:"enabled"`
	Source           string     `json:"source"`

	RateLimits RateLimits `json:"rateLimits"`

	InFlight            int64     `json:"inFlight"`
	CooldownUntil       time.Time `json:"cooldownUntil"`
	ConsecutiveFailures int       `json:"consecutiveFailures"`

	TotalRequests int64     `json:"totalRequests"`
	TotalErrors   int64     `json:"totalErrors"`
	TotalTokens   int64     `json:"totalTokens"`
	TotalCost     float64   `json:"totalCost"`
	LastUsed      time.Time `json:"lastUsed"`
	CreatedAt     time.Time `json:"createdAt"`
}

// AccountSnapshot is an immutable view of an Account for UI/API consumption.
type AccountSnapshot struct {
	ID                  string     `json:"id"`
	Name                string     `json:"name"`
	Email               string     `json:"email,omitempty"`
	AccountUUID         string     `json:"accountUuid,omitempty"`
	OrganizationUUID    string     `json:"organizationUuid,omitempty"`
	Type                string     `json:"type"`
	Priority            int        `json:"priority"`
	Enabled             bool       `json:"enabled"`
	Source              string     `json:"source"`
	Status              string     `json:"status"` // "healthy", "cooldown", "rate_limited", "disabled"
	RateLimits          RateLimits `json:"rateLimits"`
	InFlight            int64      `json:"inFlight"`
	CooldownUntil       time.Time  `json:"cooldownUntil,omitempty"`
	ConsecutiveFailures int        `json:"consecutiveFailures"`
	TotalRequests       int64      `json:"totalRequests"`
	TotalErrors         int64      `json:"totalErrors"`
	TotalTokens         int64      `json:"totalTokens"`
	TotalCost           float64    `json:"totalCost"`
	LastUsed            time.Time  `json:"lastUsed,omitempty"`
	CreatedAt           time.Time  `json:"createdAt"`
	ExpiresAt           *time.Time `json:"expiresAt,omitempty"`
}

// Snapshot returns a thread-safe snapshot of the account.
func (a *Account) Snapshot() AccountSnapshot {
	a.mu.RLock()
	defer a.mu.RUnlock()

	status := "healthy"
	now := time.Now()
	if !a.Enabled {
		status = "disabled"
	} else if a.CooldownUntil.After(now) {
		status = "cooldown"
	} else if a.RateLimits.IsRateLimited(now) {
		status = "rate_limited"
	}

	return AccountSnapshot{
		ID:                  a.ID,
		Name:                a.Name,
		Email:               a.Email,
		AccountUUID:         a.AccountUUID,
		OrganizationUUID:    a.OrganizationUUID,
		Type:                a.Type,
		Priority:            a.Priority,
		Enabled:             a.Enabled,
		Source:              a.Source,
		Status:              status,
		RateLimits:          a.RateLimits,
		InFlight:            a.InFlight,
		CooldownUntil:       a.CooldownUntil,
		ConsecutiveFailures: a.ConsecutiveFailures,
		TotalRequests:       a.TotalRequests,
		TotalErrors:         a.TotalErrors,
		TotalTokens:         a.TotalTokens,
		TotalCost:           a.TotalCost,
		LastUsed:            a.LastUsed,
		CreatedAt:           a.CreatedAt,
		ExpiresAt:           a.ExpiresAt,
	}
}

// NormalizeBaseURL cleans and returns a sanitized base URL for Claude Code.
func NormalizeBaseURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return DefaultBaseURL
	}
	raw = strings.TrimRight(raw, "/")
	raw = strings.TrimSuffix(raw, "/v1")
	return strings.TrimRight(raw, "/")
}
