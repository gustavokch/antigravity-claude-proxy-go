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
