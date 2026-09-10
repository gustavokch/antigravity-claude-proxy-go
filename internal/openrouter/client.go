package openrouter

import (
	"context"
	"encoding/json"
	"errors"
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

// ErrManagementKeyRequired indicates the API key lacks the management
// permissions OpenRouter requires for the credits endpoint.
var ErrManagementKeyRequired = errors.New("openrouter management API key required to query credits")

// CreditsData represents the payload of OpenRouter's GET /v1/credits response.
type CreditsData struct {
	TotalCredits float64 `json:"total_credits"`
	TotalUsage   float64 `json:"total_usage"`
}

// CreditsResponse represents the response format of OpenRouter's GET /v1/credits.
type CreditsResponse struct {
	Data CreditsData `json:"data"`
}

// CreditsInfo holds resolved credit totals with a derived balance and fetch time.
type CreditsInfo struct {
	TotalCredits float64   `json:"total_credits"`
	TotalUsage   float64   `json:"total_usage"`
	Balance      float64   `json:"balance"`
	FetchedAt    time.Time `json:"fetched_at"`
}

// creditsCall tracks an in-flight singleflight request for credits.
type creditsCall struct {
	wg  sync.WaitGroup
	val *CreditsInfo
	err error
}

// creditsCacheEntry stores cached credits with the timestamp it was fetched.
type creditsCacheEntry struct {
	info     *CreditsInfo
	cachedAt time.Time
}

func creditsKey(cleanBase, apiKey string) string {
	return cleanBase + "\x00" + strings.TrimSpace(apiKey)
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

	creditsMu       sync.RWMutex
	creditsCache    map[string]*creditsCacheEntry
	creditsCacheTTL time.Duration

	creditsFlightMap map[string]*creditsCall
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
		httpClient:       &http.Client{Timeout: timeout},
		cacheTTL:         cacheTTL,
		flightMap:        make(map[string]*call),
		creditsCache:     make(map[string]*creditsCacheEntry),
		creditsCacheTTL:  60 * time.Second,
		creditsFlightMap: make(map[string]*creditsCall),
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

// GetModelLimits retrieves the cached context length and max output tokens
// for a model ID, using the same tolerant matching as GetModelPricing
// (case-insensitive, "openrouter/" prefix optional on either side). ok is
// false only when no cache entry matches modelID at all; a matched entry
// with unknown limits reports 0 with ok=true.
//
// Like GetModelPricing, this reads the cache directly and does not enforce
// cacheTTL: an expired entry is still returned with ok=true, and a cold cache
// is never filled as a side effect. Callers own freshness — pair it with
// WarmupCacheAsync or ResolveModelPricing.
func (c *Client) GetModelLimits(modelID string) (contextLength, maxOutputTokens int, ok bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for i := range c.cache {
		if matchModel(c.cache[i], modelID) {
			return c.cache[i].ContextLength, c.cache[i].GetMaxOutputTokens(), true
		}
	}
	return 0, 0, false
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

		func() {
			defer func() {
				c.flightMu.Lock()
				delete(c.flightMap, cleanBase)
				c.flightMu.Unlock()
				gCall.wg.Done()
			}()

			// Decouple from caller context so single client abort does not cancel shared catalog fetch
			fetchCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			_, _ = c.FetchAvailableModels(fetchCtx, apiKey, baseURL)
		}()
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

// FetchCredits queries GET <baseURL>/v1/credits with the given API key. A 403
// response maps to ErrManagementKeyRequired because OpenRouter reserves the
// credits endpoint for management keys.
func (c *Client) FetchCredits(ctx context.Context, apiKey, baseURL string) (*CreditsInfo, error) {
	cleanBase := NormalizeBaseURL(baseURL)
	url := cleanBase + "/v1/credits"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create credits request: %w", err)
	}

	req.Header.Set("Accept", "application/json")
	if strings.TrimSpace(apiKey) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(apiKey))
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch credits from openrouter: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}

	if resp.StatusCode == http.StatusForbidden {
		return nil, ErrManagementKeyRequired
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("openrouter API error (status %d): %s", resp.StatusCode, string(bodyBytes))
	}

	var parsed CreditsResponse
	if err := json.Unmarshal(bodyBytes, &parsed); err != nil {
		return nil, fmt.Errorf("decode openrouter credits json: %w", err)
	}

	info := &CreditsInfo{
		TotalCredits: parsed.Data.TotalCredits,
		TotalUsage:   parsed.Data.TotalUsage,
		Balance:      parsed.Data.TotalCredits - parsed.Data.TotalUsage,
		FetchedAt:    time.Now(),
	}

	key := creditsKey(cleanBase, apiKey)
	c.creditsMu.Lock()
	if c.creditsCache == nil {
		c.creditsCache = make(map[string]*creditsCacheEntry)
	}
	c.creditsCache[key] = &creditsCacheEntry{
		info:     info,
		cachedAt: info.FetchedAt,
	}
	c.creditsMu.Unlock()

	return info, nil
}

// getCachedCredits returns a copy of cached credits for the given apiKey and
// baseURL while the TTL holds, and nil otherwise.
func (c *Client) getCachedCredits(apiKey, baseURL string) *CreditsInfo {
	cleanBase := NormalizeBaseURL(baseURL)
	key := creditsKey(cleanBase, apiKey)
	c.creditsMu.RLock()
	defer c.creditsMu.RUnlock()
	entry := c.creditsCache[key]
	if entry == nil || entry.cachedAt.IsZero() || time.Since(entry.cachedAt) >= c.creditsCacheTTL {
		return nil
	}
	info := *entry.info
	return &info
}

// ResolveCredits returns cached credits while fresh, otherwise fetches fresh
// credits with singleflight deduplication. force bypasses the cache.
func (c *Client) ResolveCredits(ctx context.Context, apiKey, baseURL string, force bool) (*CreditsInfo, error) {
	cleanBase := NormalizeBaseURL(baseURL)
	key := creditsKey(cleanBase, apiKey)
	if !force {
		if cached := c.getCachedCredits(apiKey, cleanBase); cached != nil {
			return cached, nil
		}
	}

	c.flightMu.Lock()
	if c.creditsFlightMap == nil {
		c.creditsFlightMap = make(map[string]*creditsCall)
	}
	cCall, inFlight := c.creditsFlightMap[key]
	if !inFlight {
		cCall = &creditsCall{}
		cCall.wg.Add(1)
		c.creditsFlightMap[key] = cCall
		c.flightMu.Unlock()

		func() {
			defer func() {
				c.flightMu.Lock()
				delete(c.creditsFlightMap, key)
				c.flightMu.Unlock()
				cCall.wg.Done()
			}()

			// Decouple from caller context so a single client abort does not cancel the shared credits fetch.
			fetchCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			info, err := c.FetchCredits(fetchCtx, apiKey, baseURL)
			cCall.val = info
			cCall.err = err
		}()
	} else {
		c.flightMu.Unlock()
	}
	cCall.wg.Wait()

	if cCall.err != nil {
		return nil, cCall.err
	}
	return cCall.val, nil
}
