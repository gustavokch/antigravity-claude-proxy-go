package kimi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// ModelItem is one entry from Kimi's /v1/models response, trimmed to the
// fields the proxy actually needs.
type ModelItem struct {
	ID              string `json:"id"`
	DisplayName     string `json:"display_name,omitempty"`
	ContextLen      int    `json:"context_length,omitempty"`
	MaxOutputTokens int    `json:"max_tokens,omitempty"`
}

// Client fetches and caches the Kimi model catalog. Safe for concurrent use.
type Client struct {
	mu      sync.RWMutex
	cached  []ModelItem
	fetched time.Time
	ttl     time.Duration
}

const defaultCatalogTTL = 5 * time.Minute

// DefaultClient is the package-level client used by the proxy.
var DefaultClient = &Client{ttl: defaultCatalogTTL}

type kimiModelsResponse struct {
	Data []ModelItem `json:"data"`
}

// FetchModels GETs /v1/models from Kimi, returns the parsed list, and caches
// it. The cache is consulted by GetCachedModels.
func (c *Client) FetchModels(ctx context.Context, apiKey, baseURL string) ([]ModelItem, error) {
	url := NormalizeBaseURL(baseURL) + "/v1/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build Kimi models request: %w", err)
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call Kimi /v1/models: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("Kimi /v1/models returned %d", resp.StatusCode)
	}
	var body kimiModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode Kimi /v1/models: %w", err)
	}
	c.mu.Lock()
	c.cached = body.Data
	c.fetched = time.Now()
	c.mu.Unlock()
	return body.Data, nil
}

// GetCachedModels returns the last FetchModels result, or an empty slice if
// nothing has been fetched yet.
func (c *Client) GetCachedModels() []ModelItem {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.cached == nil {
		return []ModelItem{}
	}
	out := make([]ModelItem, len(c.cached))
	copy(out, c.cached)
	return out
}
