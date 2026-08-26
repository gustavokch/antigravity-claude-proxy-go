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

// ProviderEndpoint represents a single upstream provider serving a model on OpenRouter.
type ProviderEndpoint struct {
	ProviderName         string   `json:"provider_name"`
	Tag                  string   `json:"tag,omitempty"`
	ContextLength        int      `json:"context_length,omitempty"`
	MaxCompletionTokens  int      `json:"max_completion_tokens,omitempty"`
	UptimeLast5m         float64  `json:"uptime_last_5m,omitempty"`
	UptimeLast30m        float64  `json:"uptime_last_30m,omitempty"`
	UptimeLast1d         float64  `json:"uptime_last_1d,omitempty"`
	LatencyLast30mMs     float64  `json:"latency_last_30m,omitempty"`
	ThroughputLast30mTPS float64  `json:"throughput_last_30m,omitempty"`
	Status               int      `json:"status,omitempty"`
	SupportedParameters  []string `json:"supported_parameters,omitempty"`
	Pricing              *Pricing `json:"pricing,omitempty"`
}

// BlendedUptime returns the weighted uptime signal (5m 50%, 30m 30%, 1d 20%),
// or 0 when no uptime data is reported.
func (e *ProviderEndpoint) BlendedUptime() float64 {
	if e == nil {
		return 0
	}
	return clamp01(e.UptimeLast5m*0.5 + e.UptimeLast30m*0.3 + e.UptimeLast1d*0.2)
}

// Healthy reports whether the endpoint is considered healthy (uptime blend and explicit status).
func (e *ProviderEndpoint) Healthy() bool {
	if e == nil {
		return false
	}
	if e.Status != 0 && e.Status >= 400 {
		return false
	}
	if e.UptimeLast5m == 0 && e.UptimeLast30m == 0 && e.UptimeLast1d == 0 {
		// No uptime data — treat as unknown, not unhealthy
		return true
	}
	return e.BlendedUptime() >= 0.4
}

// AvailabilityScore returns a 0..1 availability signal (0.5 when no data).
func (e *ProviderEndpoint) AvailabilityScore() float64 {
	if e == nil {
		return 0
	}
	if e.Status != 0 && e.Status >= 400 {
		return 0
	}
	if e.UptimeLast5m == 0 && e.UptimeLast30m == 0 && e.UptimeLast1d == 0 {
		return 0.5 // neutral
	}
	return e.BlendedUptime()
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// EndpointsResponse is the OpenRouter endpoints JSON envelope.
type EndpointsResponse struct {
	Data struct {
		ProviderName string             `json:"name,omitempty"`
		Endpoints    []ProviderEndpoint `json:"endpoints"`
	} `json:"data"`
}

// FlattenEndpointsResponse handles the common variant where the response is a
// bare list of endpoints (some probes returned `{"endpoints": [...]}` or
// `{"data": [...]}` directly).
func flattenEndpointsResponse(body []byte) ([]ProviderEndpoint, error) {
	body = []byte(strings.TrimSpace(string(body)))
	if len(body) == 0 {
		return nil, fmt.Errorf("empty body")
	}

	// Try the canonical `data.endpoints` shape first. An explicitly empty
	// list is a valid catalog (model temporarily unserved), not an error.
	var wrapped EndpointsResponse
	if err := json.Unmarshal(body, &wrapped); err == nil && wrapped.Data.Endpoints != nil {
		return wrapped.Data.Endpoints, nil
	}

	// Fallback: top-level array.
	var list []ProviderEndpoint
	if err := json.Unmarshal(body, &list); err == nil {
		return list, nil
	}

	// Fallback: `data` is the array.
	var alt struct {
		Data []ProviderEndpoint `json:"data"`
	}
	if err := json.Unmarshal(body, &alt); err == nil && len(alt.Data) > 0 {
		return alt.Data, nil
	}

	// Last resort: `endpoints` at the top level.
	var alt2 struct {
		Endpoints []ProviderEndpoint `json:"endpoints"`
	}
	if err := json.Unmarshal(body, &alt2); err == nil && len(alt2.Endpoints) > 0 {
		return alt2.Endpoints, nil
	}

	return nil, fmt.Errorf("decode endpoints response: unrecognized shape")
}

// EndpointsClient extends Client with provider-endpoint discovery and caching.
type EndpointsClient struct {
	httpClient *http.Client
	mu         sync.RWMutex
	cache      map[string]endpointsCacheEntry
	cacheTTL   time.Duration
	flightMu   sync.Mutex
	flightMap  map[string]*endpointsCall
}

type endpointsCacheEntry struct {
	endpoints []ProviderEndpoint
	cachedAt  time.Time
}

type endpointsCall struct {
	wg  sync.WaitGroup
	val []ProviderEndpoint
	err error
}

// NewEndpointsClient creates an endpoints discovery client with its own TTL and HTTP client.
func NewEndpointsClient(timeout time.Duration, cacheTTL time.Duration) *EndpointsClient {
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	if cacheTTL <= 0 {
		cacheTTL = 30 * time.Minute
	}
	return &EndpointsClient{
		httpClient: &http.Client{Timeout: timeout},
		cacheTTL:   cacheTTL,
		cache:      make(map[string]endpointsCacheEntry),
		flightMap:  make(map[string]*endpointsCall),
	}
}

// DefaultEndpointsClient is a shared package-level endpoints client.
// TTL matches the model catalog cache (1h).
var DefaultEndpointsClient = NewEndpointsClient(15*time.Second, time.Hour)

// ResolveModelSlug splits "author/slug" into (author, slug) accepting the
// optional "openrouter/" prefix.
func ResolveModelSlug(modelID string) (string, string, bool) {
	clean := strings.TrimSpace(modelID)
	clean = strings.TrimPrefix(clean, "openrouter/")
	parts := strings.SplitN(clean, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func endpointsURL(baseURL, author, slug string) string {
	cleanBase := NormalizeBaseURL(baseURL)
	return cleanBase + "/v1/models/" + author + "/" + slug + "/endpoints"
}

// FetchModelEndpoints queries the OpenRouter endpoints API for a model and returns its provider list.
func (e *EndpointsClient) FetchModelEndpoints(ctx context.Context, modelID, apiKey, baseURL string) ([]ProviderEndpoint, error) {
	author, slug, ok := ResolveModelSlug(modelID)
	if !ok {
		return nil, fmt.Errorf("model %q is not in author/slug form", modelID)
	}

	url := endpointsURL(baseURL, author, slug)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create endpoints request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if key := strings.TrimSpace(apiKey); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}

	resp, err := e.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch endpoints: %w", err)
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read endpoints body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("endpoints API error (status %d): %s", resp.StatusCode, string(bodyBytes))
	}

	endpoints, err := flattenEndpointsResponse(bodyBytes)
	if err != nil {
		return nil, err
	}
	return endpoints, nil
}

// cacheKey returns a stable per-model cache key.
func (e *EndpointsClient) cacheKey(modelID, baseURL string) string {
	return NormalizeBaseURL(baseURL) + "|" + modelID
}

// GetCachedEndpoints returns the cached endpoints list for a model if fresh.
func (e *EndpointsClient) GetCachedEndpoints(modelID, baseURL string) ([]ProviderEndpoint, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	entry, ok := e.cache[e.cacheKey(modelID, baseURL)]
	if !ok || len(entry.endpoints) == 0 {
		return nil, false
	}
	if time.Since(entry.cachedAt) > e.cacheTTL {
		return nil, false
	}
	out := make([]ProviderEndpoint, len(entry.endpoints))
	copy(out, entry.endpoints)
	return out, true
}

// SaveEndpoints stores the endpoints list in the cache.
func (e *EndpointsClient) SaveEndpoints(modelID, baseURL string, endpoints []ProviderEndpoint) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cache[e.cacheKey(modelID, baseURL)] = endpointsCacheEntry{
		endpoints: append([]ProviderEndpoint(nil), endpoints...),
		cachedAt:  time.Now(),
	}
}

// Invalidate removes a model from the endpoints cache.
func (e *EndpointsClient) Invalidate(modelID, baseURL string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.cache, e.cacheKey(modelID, baseURL))
}

// ResolveModelEndpoints returns cached endpoints or fetches them on demand.
func (e *EndpointsClient) ResolveModelEndpoints(ctx context.Context, modelID, apiKey, baseURL string) ([]ProviderEndpoint, error) {
	if cached, ok := e.GetCachedEndpoints(modelID, baseURL); ok {
		return cached, nil
	}

	cleanBase := NormalizeBaseURL(baseURL)
	cacheKey := e.cacheKey(modelID, baseURL)

	e.flightMu.Lock()
	if e.flightMap == nil {
		e.flightMap = make(map[string]*endpointsCall)
	}
	flight, inFlight := e.flightMap[cacheKey]
	if !inFlight {
		flight = &endpointsCall{}
		flight.wg.Add(1)
		e.flightMap[cacheKey] = flight
		e.flightMu.Unlock()

		func() {
			defer func() {
				e.flightMu.Lock()
				delete(e.flightMap, cacheKey)
				e.flightMu.Unlock()
				flight.wg.Done()
			}()

			fetchCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			endpoints, fetchErr := e.FetchModelEndpoints(fetchCtx, modelID, apiKey, cleanBase)
			if fetchErr == nil {
				e.SaveEndpoints(modelID, cleanBase, endpoints)
			}
			flight.val = endpoints
			flight.err = fetchErr
		}()
	} else {
		e.flightMu.Unlock()
		// The leader fetches on a detached context (singleflight, see the
		// pricing client pattern), so a follower must not block past its own
		// cancellation. The helper goroutine finishes once the flight lands.
		flightDone := make(chan struct{})
		go func() {
			flight.wg.Wait()
			close(flightDone)
		}()
		select {
		case <-flightDone:
		case <-ctx.Done():
			return nil, fmt.Errorf("wait for endpoints fetch: %w", ctx.Err())
		}
	}

	if flight.err != nil {
		return nil, flight.err
	}
	return flight.val, nil
}

// WarmupEndpointsAsync fires a background fetch if cache is missing. On success
// the shared DefaultRouter ranks are refreshed so routing decisions pick up the
// new provider list without blocking the request path.
func (e *EndpointsClient) WarmupEndpointsAsync(modelID, apiKey, baseURL string) {
	if _, ok := e.GetCachedEndpoints(modelID, baseURL); ok {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		endpoints, err := e.ResolveModelEndpoints(ctx, modelID, apiKey, baseURL)
		if err == nil && len(endpoints) > 0 {
			DefaultRouter.RefreshRanks(modelID, endpoints)
		}
	}()
}
