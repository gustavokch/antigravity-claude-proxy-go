package openrouter

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// TopProvider represents provider-specific limits in OpenRouter responses.
type TopProvider struct {
	MaxCompletionTokens *int `json:"max_completion_tokens,omitempty"`
}

// ModelItem represents a single OpenRouter model in the catalog.
type ModelItem struct {
	ID                  string       `json:"id"`
	CanonicalSlug       string       `json:"canonical_slug,omitempty"`
	Name                string       `json:"name"`
	Description         string       `json:"description"`
	ContextLength       int          `json:"context_length"`
	TopProvider         *TopProvider `json:"top_provider,omitempty"`
	MaxCompletionTokens int          `json:"max_completion_tokens,omitempty"`
	Pricing             *Pricing     `json:"pricing,omitempty"`
}

// GetMaxOutputTokens returns max completion tokens if available.
func (m *ModelItem) GetMaxOutputTokens() int {
	if m.MaxCompletionTokens > 0 {
		return m.MaxCompletionTokens
	}
	if m.TopProvider != nil && m.TopProvider.MaxCompletionTokens != nil && *m.TopProvider.MaxCompletionTokens > 0 {
		return *m.TopProvider.MaxCompletionTokens
	}
	return 0
}

// ModelsResponse represents the response format of OpenRouter's GET /v1/models.
type ModelsResponse struct {
	Data []ModelItem `json:"data"`
}

// call tracks an in-flight singleflight request for model catalog discovery.
type call struct {
	wg  sync.WaitGroup
	val []ModelItem
	err error
}

// Client manages OpenRouter catalog discovery and caching.
type Client struct {
	httpClient *http.Client
	mu         sync.RWMutex
	cache      []ModelItem
	cachedAt   time.Time
	cacheTTL   time.Duration

	flightMu  sync.Mutex
	flightMap map[string]*call
}

// DefaultClient is a shared package-level client instance.
var DefaultClient = NewClient(15*time.Second, 1*time.Hour)

// NewClient initializes a new OpenRouter client with configurable timeout and cache TTL.
func NewClient(timeout time.Duration, cacheTTL time.Duration) *Client {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	if cacheTTL <= 0 {
		cacheTTL = 1 * time.Hour
	}
	return &Client{
		httpClient: &http.Client{Timeout: timeout},
		cacheTTL:   cacheTTL,
		flightMap:  make(map[string]*call),
	}
}

// NormalizeBaseURL strips trailing slashes and version segments to obtain base API URL.
func NormalizeBaseURL(baseURL string) string {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		baseURL = "https://openrouter.ai/api"
	}
	baseURL = strings.TrimRight(baseURL, "/")
	baseURL = strings.TrimSuffix(baseURL, "/v1")
	return strings.TrimRight(baseURL, "/")
}

// FetchAvailableModels queries GET <baseURL>/v1/models with the given API key.
func (c *Client) FetchAvailableModels(ctx context.Context, apiKey, baseURL string) ([]ModelItem, error) {
	cleanBase := NormalizeBaseURL(baseURL)
	url := cleanBase + "/v1/models"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create models request: %w", err)
	}

	req.Header.Set("Accept", "application/json")
	if strings.TrimSpace(apiKey) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(apiKey))
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch models from openrouter: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openrouter API error (status %d): %s", resp.StatusCode, string(bodyBytes))
	}

	var parsed ModelsResponse
	if err := json.Unmarshal(bodyBytes, &parsed); err != nil {
		return nil, fmt.Errorf("decode openrouter models json: %w", err)
	}

	c.SaveCache(parsed.Data)
	return parsed.Data, nil
}

// SaveCache updates the in-memory models cache.
func (c *Client) SaveCache(models []ModelItem) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cache = make([]ModelItem, len(models))
	copy(c.cache, models)
	c.cachedAt = time.Now()
}

// GetCachedModels returns a copy of currently cached models.
func (c *Client) GetCachedModels() []ModelItem {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.cache) == 0 {
		return nil
	}
	result := make([]ModelItem, len(c.cache))
	copy(result, c.cache)
	return result
}

// IsCacheValid reports whether the in-memory cache has valid non-expired models.
func (c *Client) IsCacheValid() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.cache) > 0 && !c.cachedAt.IsZero() && time.Since(c.cachedAt) < c.cacheTTL
}

func matchModel(m ModelItem, target string) bool {
	cleanTarget := strings.TrimSpace(target)
	if cleanTarget == "" {
		return false
	}
	// Direct ID match
	if strings.EqualFold(m.ID, cleanTarget) {
		return true
	}
	// Slug match
	if m.CanonicalSlug != "" && strings.EqualFold(m.CanonicalSlug, cleanTarget) {
		return true
	}
	// Match stripped prefix "openrouter/"
	strippedTarget := strings.TrimPrefix(strings.ToLower(cleanTarget), "openrouter/")
	strippedID := strings.TrimPrefix(strings.ToLower(m.ID), "openrouter/")
	if strippedTarget == strippedID {
		return true
	}
	if m.CanonicalSlug != "" && strippedTarget == strings.TrimPrefix(strings.ToLower(m.CanonicalSlug), "openrouter/") {
		return true
	}
	return false
}

// GetModelPricing retrieves pricing for a specific model ID from cache.
func (c *Client) GetModelPricing(modelID string) (Pricing, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, m := range c.cache {
		if matchModel(m, modelID) && m.Pricing != nil {
			return *m.Pricing, true
		}
	}
	return Pricing{}, false
}

// ResolveModelPricing returns cached pricing if valid, or fetches fresh models from OpenRouter on-demand.
func (c *Client) ResolveModelPricing(ctx context.Context, modelID string, apiKey, baseURL string) (Pricing, bool) {
	// 1. Fast path: check valid cache
	if c.IsCacheValid() {
		if p, ok := c.GetModelPricing(modelID); ok {
			return p, true
		}
	}

	// 2. Fetch fresh catalog with singleflight deduplication
	cleanBase := NormalizeBaseURL(baseURL)
	c.flightMu.Lock()
	if c.flightMap == nil {
		c.flightMap = make(map[string]*call)
	}
	gCall, inFlight := c.flightMap[cleanBase]
	if !inFlight {
		gCall = &call{}
		gCall.wg.Add(1)
		c.flightMap[cleanBase] = gCall
		c.flightMu.Unlock()

		_, _ = c.FetchAvailableModels(ctx, apiKey, baseURL)
		gCall.wg.Done()

		c.flightMu.Lock()
		delete(c.flightMap, cleanBase)
		c.flightMu.Unlock()
	} else {
		c.flightMu.Unlock()
		gCall.wg.Wait()
	}

	// 3. Check cache after fetch
	if p, ok := c.GetModelPricing(modelID); ok {
		return p, true
	}

	return Pricing{}, false
}

// WarmupCacheAsync triggers a background fetch to populate the models cache if empty or expired.
func (c *Client) WarmupCacheAsync(apiKey, baseURL string) {
	if c.IsCacheValid() {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, _ = c.ResolveModelPricing(ctx, "", apiKey, baseURL)
	}()
}

