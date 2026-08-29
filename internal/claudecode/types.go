package claudecode

import (
	"strings"
	"sync"
	"time"
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

// Config is the root configuration structure for the Claude Code subsystem.
type Config struct {
	Enabled    bool            `json:"enabled"`
	BaseURL    string          `json:"baseUrl"`    // Default: "https://api.anthropic.com"
	Mode       string          `json:"mode"`       // "pool" or "single"
	AutoImport bool            `json:"autoImport"` // Default: false
	Accounts   []AccountConfig `json:"accounts,omitempty"`
	Allowlist  []ModelConfig   `json:"allowlist,omitempty"`
	Routing    RoutingConfig   `json:"routing,omitempty"`
}

// RateLimits tracks Anthropic API rate limits extracted from response headers.
type RateLimits struct {
	RequestsLimit        int64     `json:"requestsLimit"`
	RequestsRemaining    int64     `json:"requestsRemaining"`
	RequestsReset        time.Time `json:"requestsReset"`
	TokensLimit          int64     `json:"tokensLimit"`
	TokensRemaining      int64     `json:"tokensRemaining"`
	TokensReset          time.Time `json:"tokensReset"`
	InputTokensLimit     int64     `json:"inputTokensLimit,omitempty"`
	InputTokensRemaining int64     `json:"inputTokensRemaining,omitempty"`
	InputTokensReset     time.Time `json:"inputTokensReset,omitempty"`
	OutputTokensLimit    int64     `json:"outputTokensLimit,omitempty"`
	OutputTokensRemaining int64    `json:"outputTokensRemaining,omitempty"`
	OutputTokensReset    time.Time `json:"outputTokensReset,omitempty"`
	RetryAfter           int       `json:"retryAfter,omitempty"` // Seconds
	LastUpdated          time.Time `json:"lastUpdated"`
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
	} else if (a.RateLimits.RequestsRemaining == 0 && a.RateLimits.RequestsReset.After(now)) ||
		(a.RateLimits.TokensRemaining == 0 && a.RateLimits.TokensReset.After(now)) {
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
