package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"antigravity-go-proxy/internal/transport"
)

// maxModelsBody bounds the model catalog read; the real one is a few hundred
// kilobytes.
const maxModelsBody = 8 << 20

// maxErrorBody bounds the part of an error reply kept in a StatusError.
const maxErrorBody = 2048

// SendRequest is one Codex Responses request.
type SendRequest struct {
	Token, AccountID string
	SessionID        string // Session-Id header (the prompt cache key)
	Model            string // X-Codex-Routing-Hint
	ServiceTier      string
	Body             []byte
	Identity         IdentityConfig
}

// Client sends requests to the Codex backend.
type Client struct {
	baseURL string
	http    *http.Client
}

// NewClient returns a client for baseURL (normalized; empty means
// DefaultBaseURL). rt nil means transport.Codex(); every call then builds its
// own transport, so the caller keeps one Client.
func NewClient(baseURL string, rt http.RoundTripper) *Client {
	if rt == nil {
		rt = transport.Codex()
	}
	return &Client{
		baseURL: NormalizeBaseURL(baseURL),
		http: &http.Client{
			Transport: rt,
			// A redirect is returned as is: the caller gets the upstream's
			// status untouched, and the bearer token never follows a
			// Location header to another host.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// Send posts a Responses request and returns the upstream response, whatever
// its status. The caller closes the body.
func (c *Client) Send(ctx context.Context, r SendRequest) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/responses", bytes.NewReader(r.Body))
	if err != nil {
		return nil, fmt.Errorf("codex: build request: %w", err)
	}
	setIdentity(req.Header, r.Token, r.AccountID, r.Identity)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if r.SessionID != "" {
		req.Header.Set("Session-Id", r.SessionID)
	}
	if r.Model != "" {
		hint := "model=" + r.Model
		if r.ServiceTier != "" {
			hint += ";tier=" + r.ServiceTier
		}
		req.Header.Set("X-Codex-Routing-Hint", hint)
	}
	return c.http.Do(req)
}

// setIdentity sets the headers every Codex request carries: the bearer token,
// the client identity and the ChatGPT workspace the token acts for.
func setIdentity(h http.Header, token, accountID string, id IdentityConfig) {
	h.Set("Authorization", "Bearer "+token)
	h.Set("User-Agent", id.userAgent())
	h.Set("Originator", id.originator())
	if accountID != "" {
		h.Set("Chatgpt-Account-Id", accountID)
	}
}

// DiscoveredModel is one model of the account's upstream catalog.
type DiscoveredModel struct {
	ID            string   `json:"id"`
	DisplayName   string   `json:"displayName,omitempty"`
	ContextLength int      `json:"contextLength,omitempty"`
	Efforts       []string `json:"efforts,omitempty"`
	DefaultEffort string   `json:"defaultEffort,omitempty"`
}

// StatusError is a non-2xx reply to a catalog request.
type StatusError struct {
	StatusCode int
	Body       string // the start of the reply body
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("codex: upstream returned status %d: %s", e.StatusCode, e.Body)
}

// catalogModel is the part of a catalog entry the gateway uses.
type catalogModel struct {
	Slug                     string `json:"slug"`
	DisplayName              string `json:"display_name"`
	ContextWindow            int    `json:"context_window"`
	Visibility               string `json:"visibility"`
	SupportedInAPI           *bool  `json:"supported_in_api"`
	DefaultReasoningLevel    string `json:"default_reasoning_level"`
	SupportedReasoningLevels []struct {
		Effort string `json:"effort"`
	} `json:"supported_reasoning_levels"`
}

// FetchModels reads the account's model catalog. Hidden entries and entries
// not served over the API are left out, as the official client leaves them
// out of its picker. The response headers are returned with any reply, so the
// caller can read the usage windows from them.
func (c *Client) FetchModels(ctx context.Context, token, accountID string, id IdentityConfig) ([]DiscoveredModel, http.Header, error) {
	endpoint := c.baseURL + "/models?client_version=" + url.QueryEscape(DefaultClientVersion)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("codex: build models request: %w", err)
	}
	setIdentity(req.Header, token, accountID, id)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("codex: models request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxModelsBody))
	if err != nil {
		return nil, resp.Header, fmt.Errorf("codex: read models response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		if len(body) > maxErrorBody {
			body = body[:maxErrorBody]
		}
		return nil, resp.Header, &StatusError{StatusCode: resp.StatusCode, Body: string(body)}
	}
	var catalog struct {
		Models []catalogModel `json:"models"`
	}
	if err := json.Unmarshal(body, &catalog); err != nil {
		return nil, resp.Header, fmt.Errorf("codex: decode models response: %w", err)
	}
	models := make([]DiscoveredModel, 0, len(catalog.Models))
	for _, m := range catalog.Models {
		if m.Slug == "" || m.Visibility == "hide" || (m.SupportedInAPI != nil && !*m.SupportedInAPI) {
			continue
		}
		d := DiscoveredModel{
			ID:            m.Slug,
			DisplayName:   m.DisplayName,
			ContextLength: m.ContextWindow,
			DefaultEffort: m.DefaultReasoningLevel,
		}
		for _, level := range m.SupportedReasoningLevels {
			if level.Effort != "" {
				d.Efforts = append(d.Efforts, level.Effort)
			}
		}
		models = append(models, d)
	}
	return models, resp.Header, nil
}
