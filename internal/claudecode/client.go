package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	DefaultAnthropicVersion = "2023-06-01"
	OAuthBetaHeader         = "oauth-2025-04-20"
)

// IsOAuthToken reports whether a token string is an Anthropic / Claude Code OAuth access token.
// It matches the sk-ant-oat prefix, the unprefixed ant-oat form (defends against
// tokens persisted without the sk- prefix), and a Bearer scheme prefix of any
// case, since the RFC 7235 auth scheme is case-insensitive.
func IsOAuthToken(token string) bool {
	trimmed := strings.TrimSpace(token)
	if len(trimmed) > len("bearer ") && strings.EqualFold(trimmed[:len("bearer ")], "bearer ") {
		return true
	}
	if strings.HasPrefix(trimmed, "sk-ant-oat") || strings.HasPrefix(trimmed, "ant-oat") {
		return true
	}
	return false
}

// ApplyAuthHeaders applies appropriate headers for OAuth vs API Key authentication.
func ApplyAuthHeaders(req *http.Request, token string) {
	trimmed := strings.TrimSpace(token)
	if IsOAuthToken(trimmed) {
		cleanToken := trimmed
		if len(cleanToken) > len("bearer ") && strings.EqualFold(cleanToken[:len("bearer ")], "bearer ") {
			cleanToken = cleanToken[len("bearer "):]
		}
		cleanToken = strings.TrimSpace(cleanToken)
		req.Header.Set("Authorization", "Bearer "+cleanToken)
		req.Header.Del("x-api-key")

		// Ensure anthropic-beta includes oauth-2025-04-20
		existingBeta := req.Header.Get("anthropic-beta")
		if existingBeta == "" {
			req.Header.Set("anthropic-beta", OAuthBetaHeader)
		} else {
			parts := strings.Split(existingBeta, ",")
			found := false
			for _, p := range parts {
				if strings.TrimSpace(p) == OAuthBetaHeader {
					found = true
					break
				}
			}
			if !found {
				req.Header.Set("anthropic-beta", existingBeta+","+OAuthBetaHeader)
			}
		}
	} else {
		req.Header.Set("x-api-key", trimmed)
		req.Header.Del("Authorization")
	}
}

// DefaultClient is the package-level client targeting standard Anthropic endpoints.
var DefaultClient = NewClient("https://api.anthropic.com", nil)

// DiscoveredModel represents a model item returned by Anthropic model discovery.
type DiscoveredModel struct {
	ID           string   `json:"id"`
	DisplayName  string   `json:"display_name"`
	CreatedAt    string   `json:"created_at,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"` // e.g. ["thinking", "vision", "tools"]
	Aliases      []string `json:"aliases,omitempty"`      // recommended aliases
	Family       string   `json:"family,omitempty"`       // sonnet, haiku, opus
}

// DefaultClaudeCatalogue returns the standard offline catalogue of Claude models.
func DefaultClaudeCatalogue() []DiscoveredModel {
	return []DiscoveredModel{
		{
			ID:           "claude-fable-5",
			DisplayName:  "Claude Fable 5",
			Capabilities: []string{"thinking", "vision", "tools"},
			Aliases:      []string{"claude-fable-5", "fable", "claude-fable"},
			Family:       "fable",
		},
		{
			ID:           "claude-opus-5",
			DisplayName:  "Claude Opus 5",
			Capabilities: []string{"thinking", "vision", "tools"},
			Aliases:      []string{"claude-opus-5", "opus", "claude-5-opus"},
			Family:       "opus",
		},
		{
			ID:           "claude-sonnet-5",
			DisplayName:  "Claude Sonnet 5",
			Capabilities: []string{"thinking", "vision", "tools"},
			Aliases:      []string{"claude-sonnet-5", "sonnet", "claude-5-sonnet"},
			Family:       "sonnet",
		},
		{
			ID:           "claude-haiku-4-5-20251001",
			DisplayName:  "Claude Haiku 4.5",
			CreatedAt:    "2025-10-01T00:00:00Z",
			Capabilities: []string{"thinking", "vision", "tools"},
			Aliases:      []string{"claude-haiku-4-5", "claude-haiku-4.5", "haiku"},
			Family:       "haiku",
		},
		{
			ID:           "claude-opus-4-8",
			DisplayName:  "Claude Opus 4.8",
			Capabilities: []string{"thinking", "vision", "tools"},
			Aliases:      []string{"claude-opus-4.8", "claude-opus-4-8"},
			Family:       "opus",
		},
		{
			ID:           "claude-opus-4-7",
			DisplayName:  "Claude Opus 4.7",
			Capabilities: []string{"thinking", "vision", "tools"},
			Aliases:      []string{"claude-opus-4.7", "claude-opus-4-7"},
			Family:       "opus",
		},
		{
			ID:           "claude-sonnet-4-6",
			DisplayName:  "Claude Sonnet 4.6",
			Capabilities: []string{"thinking", "vision", "tools"},
			Aliases:      []string{"claude-sonnet-4.6", "claude-sonnet-4-6"},
			Family:       "sonnet",
		},
		{
			ID:           "claude-opus-4-6",
			DisplayName:  "Claude Opus 4.6",
			Capabilities: []string{"thinking", "vision", "tools"},
			Aliases:      []string{"claude-opus-4.6", "claude-opus-4-6"},
			Family:       "opus",
		},
		{
			ID:           "claude-3-7-sonnet-20250219",
			DisplayName:  "Claude 3.7 Sonnet",
			CreatedAt:    "2025-02-19T00:00:00Z",
			Capabilities: []string{"thinking", "vision", "tools"},
			Aliases:      []string{"claude-3.7-sonnet", "claude-3-7-sonnet", "claude-3-7-sonnet-thinking"},
			Family:       "sonnet",
		},
		{
			ID:           "claude-3-5-sonnet-20241022",
			DisplayName:  "Claude 3.5 Sonnet v2",
			CreatedAt:    "2024-10-22T00:00:00Z",
			Capabilities: []string{"vision", "tools"},
			Aliases:      []string{"claude-3.5-sonnet", "claude-3-5-sonnet"},
			Family:       "sonnet",
		},
		{
			ID:           "claude-3-5-haiku-20241022",
			DisplayName:  "Claude 3.5 Haiku",
			CreatedAt:    "2024-10-22T00:00:00Z",
			Capabilities: []string{"tools"},
			Aliases:      []string{"claude-3.5-haiku", "claude-3-5-haiku"},
			Family:       "haiku",
		},
		{
			ID:           "claude-3-opus-20240229",
			DisplayName:  "Claude 3 Opus",
			CreatedAt:    "2024-02-29T00:00:00Z",
			Capabilities: []string{"vision", "tools"},
			Aliases:      []string{"claude-3-opus"},
			Family:       "opus",
		},
		{
			ID:           "claude-3-sonnet-20240229",
			DisplayName:  "Claude 3 Sonnet",
			CreatedAt:    "2024-02-29T00:00:00Z",
			Capabilities: []string{"vision", "tools"},
			Aliases:      []string{},
			Family:       "sonnet",
		},
		{
			ID:           "claude-3-haiku-20240307",
			DisplayName:  "Claude 3 Haiku",
			CreatedAt:    "2024-03-07T00:00:00Z",
			Capabilities: []string{"vision", "tools"},
			Aliases:      []string{},
			Family:       "haiku",
		},
	}
}

// EnrichModel infers capabilities, family, and recommended aliases for a model ID.
func EnrichModel(id, displayName, createdAt string) DiscoveredModel {
	m := DiscoveredModel{
		ID:          id,
		DisplayName: displayName,
		CreatedAt:   createdAt,
	}
	if m.DisplayName == "" {
		m.DisplayName = id
	}

	lowerID := strings.ToLower(id)
	switch {
	case strings.Contains(lowerID, "fable"):
		m.Family = "fable"
	case strings.Contains(lowerID, "opus"):
		m.Family = "opus"
	case strings.Contains(lowerID, "haiku"):
		m.Family = "haiku"
	case strings.Contains(lowerID, "sonnet"):
		m.Family = "sonnet"
	default:
		m.Family = "other"
	}

	// Check if known catalogue has rich metadata for this ID
	for _, def := range DefaultClaudeCatalogue() {
		if strings.EqualFold(def.ID, id) {
			m.Capabilities = def.Capabilities
			m.Aliases = def.Aliases
			if def.DisplayName != "" {
				m.DisplayName = def.DisplayName
			}
			m.Family = def.Family
			return m
		}
	}

	// Dynamic fallback capabilities & aliases
	capabilities := []string{"tools"}
	if strings.Contains(lowerID, "thinking") || strings.Contains(lowerID, "fable") || strings.Contains(lowerID, "5") || strings.Contains(lowerID, "3-7") || strings.Contains(lowerID, "3.7") {
		capabilities = append([]string{"thinking"}, capabilities...)
	}
	if !strings.Contains(lowerID, "3-5-haiku") && !strings.Contains(lowerID, "3.5-haiku") {
		capabilities = append(capabilities, "vision")
	}
	m.Capabilities = capabilities

	// Generate standard aliases if id has date suffix like claude-3-7-sonnet-20250219
	parts := strings.Split(id, "-")
	if len(parts) >= 4 {
		baseAlias := strings.Join(parts[:len(parts)-1], "-")
		dotAlias := strings.Replace(baseAlias, "-7-", ".7-", 1)
		dotAlias = strings.Replace(dotAlias, "-5-", ".5-", 1)
		var aliases []string
		if dotAlias != id && dotAlias != baseAlias {
			aliases = append(aliases, dotAlias)
		}
		if baseAlias != id {
			aliases = append(aliases, baseAlias)
		}
		m.Aliases = aliases
	}

	return m
}

// Client handles HTTP transport to official Anthropic API endpoints.
type Client struct {
	baseURL    string
	httpClient *http.Client
}

// NewClient creates a new Anthropic Claude Code client.
func NewClient(baseURL string, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{
			Timeout: 5 * time.Minute, // Allow long streaming sessions
		}
	}
	return &Client{
		baseURL:    NormalizeBaseURL(baseURL),
		httpClient: httpClient,
	}
}

// SendMessage forwards a /v1/messages request to Anthropic with appropriate auth and beta headers.
func (c *Client) SendMessage(ctx context.Context, token string, reqBody []byte, clientHeaders http.Header) (*http.Response, error) {
	url := fmt.Sprintf("%s/v1/messages", c.baseURL)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create upstream request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")

	// Set or forward anthropic-version
	version := DefaultAnthropicVersion
	if clientHeaders != nil {
		if v := clientHeaders.Get("anthropic-version"); v != "" {
			version = v
		}
	}
	httpReq.Header.Set("anthropic-version", version)

	// Forward beta headers if present
	if clientHeaders != nil {
		if betas := clientHeaders.Values("anthropic-beta"); len(betas) > 0 {
			for _, b := range betas {
				httpReq.Header.Add("anthropic-beta", b)
			}
		} else if beta := clientHeaders.Get("anthropic-beta"); beta != "" {
			httpReq.Header.Set("anthropic-beta", beta)
		}

		// Forward Accept header
		if accept := clientHeaders.Get("Accept"); accept != "" {
			httpReq.Header.Set("Accept", accept)
		}
	}

	// Apply authentication headers (x-api-key vs Authorization: Bearer + anthropic-beta)
	ApplyAuthHeaders(httpReq, token)

	return c.httpClient.Do(httpReq)
}

// ValidateAccount tests an account's token against the Anthropic /v1/models endpoint.
func (c *Client) ValidateAccount(ctx context.Context, token string) error {
	url := fmt.Sprintf("%s/v1/models", c.baseURL)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("failed to create validation request: %w", err)
	}

	req.Header.Set("anthropic-version", DefaultAnthropicVersion)
	ApplyAuthHeaders(req, token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("network error during account validation: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var errResp struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &errResp); err == nil && errResp.Error.Message != "" {
		return fmt.Errorf("anthropic api error (%d %s): %s", resp.StatusCode, errResp.Error.Type, errResp.Error.Message)
	}

	return fmt.Errorf("anthropic api returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
}

// FetchModels queries the Anthropic /v1/models endpoint using token and optional baseURL.
// If token is missing, or if the upstream call fails, it falls back to the default catalogue.
func (c *Client) FetchModels(ctx context.Context, token string, baseURL string) ([]DiscoveredModel, error) {
	cleanToken := strings.TrimSpace(token)
	if cleanToken == "" {
		return DefaultClaudeCatalogue(), nil
	}

	targetBase := c.baseURL
	if baseURL != "" {
		targetBase = NormalizeBaseURL(baseURL)
	}

	url := fmt.Sprintf("%s/v1/models", targetBase)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return DefaultClaudeCatalogue(), fmt.Errorf("failed to create models request: %w", err)
	}

	req.Header.Set("anthropic-version", DefaultAnthropicVersion)
	req.Header.Set("User-Agent", "Claude-Code/2.1.246")
	ApplyAuthHeaders(req, cleanToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return DefaultClaudeCatalogue(), fmt.Errorf("upstream models request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return DefaultClaudeCatalogue(), fmt.Errorf("upstream returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var upstreamResp struct {
		Data []struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
			CreatedAt   string `json:"created_at"`
			Type        string `json:"type"`
		} `json:"data"`
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return DefaultClaudeCatalogue(), fmt.Errorf("failed to read response body: %w", err)
	}

	if err := json.Unmarshal(body, &upstreamResp); err != nil {
		return DefaultClaudeCatalogue(), fmt.Errorf("failed to decode models response: %w", err)
	}

	if len(upstreamResp.Data) == 0 {
		return DefaultClaudeCatalogue(), nil
	}

	models := make([]DiscoveredModel, 0, len(upstreamResp.Data))
	for _, item := range upstreamResp.Data {
		if strings.TrimSpace(item.ID) == "" {
			continue
		}
		enriched := EnrichModel(item.ID, item.DisplayName, item.CreatedAt)
		models = append(models, enriched)
	}

	if len(models) == 0 {
		return DefaultClaudeCatalogue(), nil
	}

	return models, nil
}
