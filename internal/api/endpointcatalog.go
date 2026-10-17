package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"antigravity-go-proxy/internal/config"
)

// A custom endpoint with discoverModels set serves every model its own catalog
// lists, not only the model that keys its config entry. The catalog is fetched
// from the endpoint (GET <base>/v1/models), cached, and refreshed in the
// background. Routing stays byte-transparent (ADR-0001): the catalog only
// decides which endpoint receives a model ID, and the request body is never
// rewritten.
const (
	// endpointCatalogTTL is how long a fetched catalog is trusted before the
	// background worker refetches it.
	endpointCatalogTTL = 5 * time.Minute
	// endpointCatalogRetry is the minimum gap between two fetch attempts for
	// one endpoint, so a dead endpoint is not hit on every request or tick.
	endpointCatalogRetry = 30 * time.Second
	// endpointCatalogFetchTimeout bounds one catalog fetch, including every page.
	endpointCatalogFetchTimeout = 5 * time.Second
	// endpointCatalogWorkerTick is how often the worker looks for due catalogs.
	endpointCatalogWorkerTick = 30 * time.Second
	endpointCatalogMaxPages   = 10
	endpointCatalogMaxModels  = 5000
	endpointCatalogMaxBody    = 4 << 20
)

// discoveredModel is one catalog entry. Zero limits mean the endpoint did not
// state them.
type discoveredModel struct {
	ID              string
	DisplayName     string
	ContextLen      int
	MaxOutputTokens int
}

// catalogSource identifies what a cached catalog was fetched from. A config
// change that moves the endpoint invalidates the catalog: its models belong to
// the old source and must not route to the new one.
type catalogSource struct {
	url, apiKey, modelsURL string
}

func sourceOf(endpoint config.EndpointConfig) catalogSource {
	return catalogSource{url: endpoint.URL, apiKey: endpoint.APIKey, modelsURL: endpoint.ModelsURL}
}

type endpointCatalog struct {
	source      catalogSource
	models      []discoveredModel
	ids         map[string]bool
	fetchedAt   time.Time // last success; zero = never
	attemptedAt time.Time
	lastErr     string
	inflight    chan struct{} // non-nil while a fetch runs; closed when it ends
}

// due reports whether a fetch should run now.
func (c *endpointCatalog) due(now time.Time) bool {
	if !c.attemptedAt.IsZero() && now.Sub(c.attemptedAt) < endpointCatalogRetry {
		return false
	}
	return c.fetchedAt.IsZero() || now.Sub(c.fetchedAt) >= endpointCatalogTTL
}

type endpointCatalogs struct {
	mu     sync.Mutex
	byName map[string]*endpointCatalog
}

func (server *Server) endpointCatalogs() *endpointCatalogs {
	server.endpointCatalogsOnce.Do(func() {
		server.endpointCats = &endpointCatalogs{byName: make(map[string]*endpointCatalog)}
	})
	return server.endpointCats
}

// discoveringEndpoints returns the names of endpoints that opted in, sorted.
// Sorted order is the tie-break when two catalogs list the same model.
func discoveringEndpoints(cfg config.Config) []string {
	names := make([]string, 0, len(cfg.CustomEndpoints))
	for name, endpoint := range cfg.CustomEndpoints {
		if endpoint.DiscoverModels && endpoint.URL != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// catalogModels returns the cached models of one endpoint, or nil when none
// were fetched for its current source.
func (server *Server) catalogModels(name string, endpoint config.EndpointConfig) []discoveredModel {
	cats := server.endpointCatalogs()
	cats.mu.Lock()
	defer cats.mu.Unlock()
	entry := cats.byName[name]
	if entry == nil || entry.source != sourceOf(endpoint) {
		return nil
	}
	return entry.models
}

func (server *Server) catalogHas(name string, endpoint config.EndpointConfig, model string) bool {
	cats := server.endpointCatalogs()
	cats.mu.Lock()
	defer cats.mu.Unlock()
	entry := cats.byName[name]
	return entry != nil && entry.source == sourceOf(endpoint) && entry.ids[model]
}

// lookupCustomEndpoint resolves a model to its custom endpoint from config and
// the cached catalogs only; it never touches the network. An entry keyed by the
// model wins over any catalog, then catalogs are consulted in name order.
func (server *Server) lookupCustomEndpoint(cfg config.Config, model string) (config.EndpointConfig, bool) {
	if endpoint, ok := cfg.CustomEndpoints[model]; ok && endpoint.URL != "" {
		return endpoint, true
	}
	if model == "" {
		return config.EndpointConfig{}, false
	}
	for _, name := range discoveringEndpoints(cfg) {
		endpoint := cfg.CustomEndpoints[name]
		if server.catalogHas(name, endpoint, model) {
			return endpoint, true
		}
	}
	return config.EndpointConfig{}, false
}

// resolveCustomEndpoint is lookupCustomEndpoint plus a cold-start fetch: when
// the model is unknown and some opted-in endpoint has never returned a catalog,
// that fetch runs first so the first request after startup or after adding an
// endpoint still routes. A stale catalog is never refetched here; the worker
// owns that, so a request for an unrelated model never waits on a slow endpoint.
func (server *Server) resolveCustomEndpoint(ctx context.Context, cfg config.Config, model string) (config.EndpointConfig, bool) {
	if endpoint, ok := server.lookupCustomEndpoint(cfg, model); ok {
		return endpoint, true
	}
	if model == "" || !server.warmEndpointCatalogs(ctx, cfg) {
		return config.EndpointConfig{}, false
	}
	return server.lookupCustomEndpoint(cfg, model)
}

// warmEndpointCatalogs fetches, concurrently, every opted-in catalog that has
// never succeeded and is due. It reports whether any fetch ran.
func (server *Server) warmEndpointCatalogs(ctx context.Context, cfg config.Config) bool {
	var wg sync.WaitGroup
	var ran bool
	var ranMu sync.Mutex
	for _, name := range discoveringEndpoints(cfg) {
		endpoint := cfg.CustomEndpoints[name]
		if !server.catalogCold(name, endpoint) {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if server.refreshEndpointCatalog(ctx, name, endpoint, false) {
				ranMu.Lock()
				ran = true
				ranMu.Unlock()
			}
		}()
	}
	wg.Wait()
	return ran
}

// catalogCold reports whether the endpoint has no successful catalog for its
// current source.
func (server *Server) catalogCold(name string, endpoint config.EndpointConfig) bool {
	cats := server.endpointCatalogs()
	cats.mu.Lock()
	defer cats.mu.Unlock()
	entry := cats.byName[name]
	return entry == nil || entry.source != sourceOf(endpoint) || entry.fetchedAt.IsZero()
}

// refreshEndpointCatalog fetches one endpoint's catalog and stores it. Without
// force it skips a catalog that is not due. Concurrent calls share one fetch:
// a caller that finds one in flight waits for it. It reports whether this call
// or the fetch it waited on completed a fetch attempt.
//
// The fetch runs detached from ctx so one client hanging up does not fail the
// fetch every other waiter is blocked on; ctx only bounds this caller's wait.
func (server *Server) refreshEndpointCatalog(ctx context.Context, name string, endpoint config.EndpointConfig, force bool) bool {
	cats := server.endpointCatalogs()
	source := sourceOf(endpoint)
	now := server.nowTime()

	cats.mu.Lock()
	entry := cats.byName[name]
	if entry == nil || entry.source != source {
		entry = &endpointCatalog{source: source}
		cats.byName[name] = entry
	}
	if entry.inflight != nil {
		wait := entry.inflight
		cats.mu.Unlock()
		select {
		case <-wait:
			return true
		case <-ctx.Done():
			return false
		}
	}
	if !force && !entry.due(now) {
		cats.mu.Unlock()
		return false
	}
	done := make(chan struct{})
	entry.inflight = done
	entry.attemptedAt = now
	cats.mu.Unlock()

	fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), endpointCatalogFetchTimeout)
	models, err := fetchEndpointModels(fetchCtx, endpoint)
	cancel()

	cats.mu.Lock()
	if err != nil {
		// Keep the previous models: a flapping endpoint must not unroute them.
		entry.lastErr = err.Error()
		if server.logger != nil {
			server.logger.Warn("custom endpoint model catalog fetch failed", "endpoint", name, "error", err)
		}
	} else {
		ids := make(map[string]bool, len(models))
		for _, model := range models {
			ids[model.ID] = true
		}
		entry.models, entry.ids = models, ids
		entry.fetchedAt = now
		entry.lastErr = ""
	}
	entry.inflight = nil
	close(done)
	cats.mu.Unlock()
	return true
}

// refreshEndpointCatalogs refreshes every opted-in catalog that is due (all of
// them with force) and drops cached catalogs of endpoints that no longer opt in.
func (server *Server) refreshEndpointCatalogs(ctx context.Context, force bool) {
	cfg := config.Get()
	names := discoveringEndpoints(cfg)
	active := make(map[string]bool, len(names))
	var wg sync.WaitGroup
	for _, name := range names {
		active[name] = true
		endpoint := cfg.CustomEndpoints[name]
		wg.Add(1)
		go func() {
			defer wg.Done()
			server.refreshEndpointCatalog(ctx, name, endpoint, force)
		}()
	}
	wg.Wait()

	cats := server.endpointCatalogs()
	cats.mu.Lock()
	for name, entry := range cats.byName {
		if !active[name] && entry.inflight == nil {
			delete(cats.byName, name)
		}
	}
	cats.mu.Unlock()
}

// StartEndpointCatalogWorker fetches opted-in endpoint catalogs at startup and
// keeps them fresh until ctx ends. It returns immediately.
func (server *Server) StartEndpointCatalogWorker(ctx context.Context) {
	go func() {
		server.refreshEndpointCatalogs(ctx, false)
		ticker := time.NewTicker(endpointCatalogWorkerTick)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				server.refreshEndpointCatalogs(ctx, false)
			}
		}
	}()
}

// endpointCatalogStatus describes every opted-in endpoint's catalog for the
// management UI.
func (server *Server) endpointCatalogStatus(cfg config.Config) map[string]any {
	status := make(map[string]any)
	cats := server.endpointCatalogs()
	cats.mu.Lock()
	defer cats.mu.Unlock()
	for _, name := range discoveringEndpoints(cfg) {
		row := map[string]any{"models": []string{}, "fetchedAt": nil, "error": ""}
		if entry := cats.byName[name]; entry != nil && entry.source == sourceOf(cfg.CustomEndpoints[name]) {
			ids := make([]string, len(entry.models))
			for i, model := range entry.models {
				ids[i] = model.ID
			}
			row["models"] = ids
			if !entry.fetchedAt.IsZero() {
				row["fetchedAt"] = entry.fetchedAt.UTC().Format(time.RFC3339)
			}
			row["error"] = entry.lastErr
		}
		status[name] = row
	}
	return status
}

// endpointModelsURL is where an endpoint publishes its catalog: ModelsURL when
// set, else <base>/v1/models with the base derived by stripping the request
// path the endpoint URL ends in. Query parameters of the endpoint URL (an
// api-version, say) are kept.
func endpointModelsURL(endpoint config.EndpointConfig) (*url.URL, error) {
	if endpoint.ModelsURL != "" {
		parsed, err := url.Parse(endpoint.ModelsURL)
		if err != nil {
			return nil, err
		}
		if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
			return nil, errors.New("modelsUrl must be an absolute http(s) URL")
		}
		return parsed, nil
	}
	parsed, err := url.Parse(endpoint.URL)
	if err != nil {
		return nil, err
	}
	path := strings.TrimRight(parsed.Path, "/")
	for _, suffix := range []string{"/v1/messages", "/v1/chat/completions", "/chat/completions", "/messages", "/v1"} {
		if strings.HasSuffix(path, suffix) {
			path = strings.TrimSuffix(path, suffix)
			break
		}
	}
	parsed.Path = path + "/v1/models"
	parsed.RawPath = ""
	parsed.Fragment = ""
	return parsed, nil
}

// fetchEndpointModels reads an endpoint's catalog. Both the OpenAI and the
// Anthropic list shape put entries under "data"; Anthropic additionally pages
// with has_more/last_id and defaults to 20 entries, so those endpoints are
// asked for the maximum page and followed to the end.
func fetchEndpointModels(ctx context.Context, endpoint config.EndpointConfig) ([]discoveredModel, error) {
	target, err := endpointModelsURL(endpoint)
	if err != nil {
		return nil, err
	}
	anthropic := isAnthropicEndpoint(endpoint.URL)
	client := &http.Client{
		Transport: customEndpointTransport,
		// Never follow a redirect: net/http strips Authorization on a
		// cross-host hop but forwards x-api-key, so following one hands the
		// endpoint key to whatever host the endpoint names. The forward path
		// (ReverseProxy) does not follow redirects either; a 3xx fails the
		// fetch through the status check below.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}

	var models []discoveredModel
	seen := make(map[string]bool)
	afterID := ""
	for range endpointCatalogMaxPages {
		pageURL := *target
		if anthropic {
			query := pageURL.Query()
			if query.Get("limit") == "" {
				query.Set("limit", "1000")
			}
			if afterID != "" {
				query.Set("after_id", afterID)
			}
			pageURL.RawQuery = query.Encode()
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL.String(), nil)
		if err != nil {
			return nil, err
		}
		request.Header.Set("Accept", "application/json")
		if endpoint.APIKey != "" {
			request.Header.Set("Authorization", "Bearer "+endpoint.APIKey)
			request.Header.Set("x-api-key", endpoint.APIKey)
		}
		if anthropic {
			request.Header.Set("anthropic-version", "2023-06-01")
		}
		response, err := client.Do(request)
		if err != nil {
			return nil, err
		}
		var payload struct {
			Data    []map[string]any `json:"data"`
			HasMore bool             `json:"has_more"`
			LastID  string           `json:"last_id"`
		}
		decodeErr := error(nil)
		if response.StatusCode != http.StatusOK {
			decodeErr = fmt.Errorf("catalog request returned HTTP %d", response.StatusCode)
		} else if err := json.NewDecoder(io.LimitReader(response.Body, endpointCatalogMaxBody)).Decode(&payload); err != nil {
			decodeErr = fmt.Errorf("catalog response is not a model list: %w", err)
		}
		response.Body.Close()
		if decodeErr != nil {
			return nil, decodeErr
		}

		for _, raw := range payload.Data {
			id, _ := raw["id"].(string)
			id = strings.TrimSpace(id)
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			displayName, _ := raw["display_name"].(string)
			models = append(models, discoveredModel{
				ID:              id,
				DisplayName:     strings.TrimSpace(displayName),
				ContextLen:      positiveIntField(raw, "max_input_tokens", "context_length", "context_window"),
				MaxOutputTokens: positiveIntField(raw, "max_tokens", "max_output_tokens"),
			})
			if len(models) >= endpointCatalogMaxModels {
				return models, nil
			}
		}
		if !anthropic || !payload.HasMore || payload.LastID == "" {
			break
		}
		afterID = payload.LastID
	}
	if len(models) == 0 {
		return nil, errors.New("catalog lists no models")
	}
	return models, nil
}

// positiveIntField returns the first of names that holds a whole positive
// number in a decoded JSON object, else 0.
func positiveIntField(raw map[string]any, names ...string) int {
	for _, name := range names {
		if v, ok := raw[name].(float64); ok && v >= 1 && v == math.Trunc(v) && v <= math.MaxInt32 {
			return int(v)
		}
	}
	return 0
}
