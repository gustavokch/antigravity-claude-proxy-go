// Package codex serves Anthropic Messages requests from ChatGPT subscription
// accounts through the Codex backend (chatgpt.com/backend-api/codex). It holds
// the gateway's configuration types, the account pool, the Anthropic→Codex
// request builder, the quota parsers and the HTTP client. The response half of
// the translation is shared with Zen and lives in internal/responses.
//
// The request builder and the wire identity are ported from CLIProxyAPI
// (router-for-me/CLIProxyAPI, MIT) at 681ce41; ADR-0005 records why the Codex
// path copies that client's identity.
package codex

import (
	"fmt"
	"strings"
	"time"
)

const (
	// DefaultBaseURL is the Codex backend the official CLI talks to when signed
	// in with a ChatGPT account.
	DefaultBaseURL = "https://chatgpt.com/backend-api/codex"
	// DefaultUserAgent and DefaultOriginator are the identity CLIProxyAPI sends
	// on the OAuth path (its codexUserAgent and codexOriginator). They are not
	// pinned against a capture of our own; codex.identity overrides them.
	DefaultUserAgent  = "codex-tui/0.154.0 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.154.0)"
	DefaultOriginator = "codex-tui"
	// DefaultClientVersion is the client_version query parameter of the model
	// catalog request; the backend filters the catalog by it.
	DefaultClientVersion = "0.154.0"
	// RefreshLead is how long before expiry an access token is refreshed.
	// Refresh tokens are single-use, so a refresh that fails transiently must
	// leave enough of the old token's life to retry before requests start to
	// fail.
	RefreshLead = 24 * time.Hour
)

// AccountConfig is one ChatGPT account as stored in config.json under
// codex.accounts. The tokens rotate on every refresh, and the pool's persister
// writes the new set back here before the new access token is used.
type AccountConfig struct {
	ID           string `json:"id"`
	Name         string `json:"name,omitempty"`
	Token        string `json:"token"` // OAuth access token
	RefreshToken string `json:"refreshToken"`
	IDToken      string `json:"idToken,omitempty"`
	ExpiresAt    string `json:"expiresAt,omitempty"` // RFC3339
	Email        string `json:"email,omitempty"`
	AccountID    string `json:"accountId"` // chatgpt_account_id claim
	PlanType     string `json:"planType,omitempty"`
	Priority     int    `json:"priority"`
	Enabled      bool   `json:"enabled"`
	Source       string `json:"source,omitempty"` // "oauth"
}

// ModelConfig is one allowlisted Codex model.
type ModelConfig struct {
	ID              string   `json:"id"`
	Alias           string   `json:"alias,omitempty"` // comma list
	Aliases         []string `json:"aliases,omitempty"`
	DisplayName     string   `json:"displayName,omitempty"`
	ContextLength   int      `json:"contextLength,omitempty"`
	MaxOutputTokens int      `json:"maxOutputTokens,omitempty"`
	Efforts         []string `json:"efforts,omitempty"` // supported reasoning levels, ascending
	DefaultEffort   string   `json:"defaultEffort,omitempty"`
	Enabled         bool     `json:"enabled"`
}

// ExpandAliases returns every alias configured on m: the comma-separated Alias
// field split into entries, then the Aliases list. Entries are trimmed, empty
// ones dropped, and duplicates removed case-insensitively (first one wins).
func (m ModelConfig) ExpandAliases() []string {
	seen := make(map[string]bool)
	var out []string
	add := func(a string) {
		a = strings.TrimSpace(a)
		if a == "" || seen[strings.ToLower(a)] {
			return
		}
		seen[strings.ToLower(a)] = true
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

// IdentityConfig overrides the wire identity headers. Empty means the default.
type IdentityConfig struct {
	UserAgent  string `json:"userAgent,omitempty"`
	Originator string `json:"originator,omitempty"`
}

// Validate reports the first override that cannot be sent as an HTTP header
// value. Go's transport refuses such a value with an error that names neither
// the field nor where it was set, so the check runs when the config is saved.
func (c IdentityConfig) Validate() error {
	for _, field := range []struct{ name, value string }{
		{"userAgent", c.UserAgent},
		{"originator", c.Originator},
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

func (c IdentityConfig) userAgent() string {
	if ua := strings.TrimSpace(c.UserAgent); ua != "" {
		return ua
	}
	return DefaultUserAgent
}

func (c IdentityConfig) originator() string {
	if o := strings.TrimSpace(c.Originator); o != "" {
		return o
	}
	return DefaultOriginator
}

// Config is the codex section of config.json.
type Config struct {
	Enabled   bool            `json:"enabled"`
	BaseURL   string          `json:"baseUrl"`
	Accounts  []AccountConfig `json:"accounts"`
	Allowlist []ModelConfig   `json:"allowlist"`
	Identity  IdentityConfig  `json:"identity,omitzero"`
}

// NormalizeBaseURL trims a configured base URL to the form requests are built
// on: no trailing slash and no /responses endpoint pasted onto it. Empty means
// DefaultBaseURL.
func NormalizeBaseURL(raw string) string {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	raw = strings.TrimRight(strings.TrimSuffix(raw, "/responses"), "/")
	if raw == "" {
		return DefaultBaseURL
	}
	return raw
}

// Account is a copy of one pooled account's credentials and settings, taken
// under the pool's lock. Holding one does not pin anything: the pool may
// refresh the account's tokens after the copy was made.
type Account struct {
	ID           string
	Name         string
	Email        string
	AccountID    string // Chatgpt-Account-Id header value
	PlanType     string
	Source       string
	Token        string
	RefreshToken string
	IDToken      string
	ExpiresAt    time.Time // zero when unknown
	Priority     int
	Enabled      bool
}

// AccountSnapshot is the management API's view of one account.
type AccountSnapshot struct {
	ID                  string    `json:"id"`
	Name                string    `json:"name"`
	Email               string    `json:"email,omitempty"`
	AccountID           string    `json:"accountId"`
	PlanType            string    `json:"planType,omitempty"`
	Enabled             bool      `json:"enabled"`
	Priority            int       `json:"priority"`
	Source              string    `json:"source,omitempty"`
	Status              string    `json:"status"` // "healthy", "cooldown", "needs_login", "disabled"
	CooldownUntil       time.Time `json:"cooldownUntil,omitzero"`
	CooldownReason      string    `json:"cooldownReason,omitempty"`
	ExpiresAt           time.Time `json:"expiresAt,omitzero"`
	InFlight            int64     `json:"inFlight"`
	TotalRequests       int64     `json:"totalRequests"`
	ConsecutiveFailures int       `json:"consecutiveFailures"`
	LastError           string    `json:"lastError,omitempty"`
	Quota               *Quota    `json:"quota,omitempty"`
}
