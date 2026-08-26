package api

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"antigravity-go-proxy/internal/accounts"
	"antigravity-go-proxy/internal/auth"
	"antigravity-go-proxy/internal/cloudcode"
	"antigravity-go-proxy/internal/config"
	proxyformat "antigravity-go-proxy/internal/format"
	"antigravity-go-proxy/internal/logger"
	"antigravity-go-proxy/internal/modelcatalog"
	"antigravity-go-proxy/internal/openrouter"
	"antigravity-go-proxy/internal/stats"
)

const (
	maxRequestBody = 50 << 20
	maxMappingHops = 5
)

var jsonBufferPool = sync.Pool{
	New: func() any {
		return new(bytes.Buffer)
	},
}

type Upstream interface {
	LoadCodeAssist(context.Context, string) (cloudcode.Response, error)
	FetchAvailableModels(context.Context, string) (cloudcode.Response, error)
	StreamGenerateContent(context.Context, any, cloudcode.RequestOptions, func(cloudcode.SSEEvent) error) (cloudcode.Response, error)
}

type Backend interface {
	FetchAvailableModels(context.Context) (cloudcode.Response, error)
	StreamGenerateContent(context.Context, map[string]any, func(cloudcode.SSEEvent) error) (cloudcode.Response, error)
}

type AccountRefresher interface {
	RefreshAccount(context.Context, string) (*accounts.Account, error)
}

type ConfigUpdater interface {
	UpdateConfig(cfg config.Config)
}

type Options struct {
	APIKey         string
	ProjectID      string
	Credentials    func(context.Context) (auth.Credentials, error)
	NewUpstream    func(string) Upstream
	Backend        Backend
	Builder        *proxyformat.Builder
	Now            func() time.Time
	Logger         *slog.Logger
	AccountManager *accounts.Manager
	Broadcaster    *logger.Broadcaster
	WebUI          http.Handler
	OAuthHandler   http.Handler
	Tracker        *stats.Tracker
}

type Server struct {
	apiKey         string
	projectID      string
	credentials    func(context.Context) (auth.Credentials, error)
	newUpstream    func(string) Upstream
	backend        Backend
	builder        *proxyformat.Builder
	now            func() time.Time
	logger         *slog.Logger
	accountManager *accounts.Manager
	broadcaster    *logger.Broadcaster
	webUI          http.Handler
	oauthHandler   http.Handler
	tracker        *stats.Tracker

	mu                sync.Mutex
	cachedCredentials auth.Credentials
	upstreamToken     string
	upstream          Upstream
	projects          map[string]string
}

func New(options Options) (*Server, error) {
	if options.Backend == nil && options.Credentials == nil {
		return nil, errors.New("credential provider is required")
	}
	if options.Backend == nil && options.NewUpstream == nil {
		return nil, errors.New("Cloud Code client factory is required")
	}
	if options.Builder == nil {
		options.Builder = proxyformat.NewBuilder()
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Logger == nil {
		options.Logger = slog.Default()
	}
	srv := &Server{
		apiKey: options.APIKey, projectID: options.ProjectID,
		credentials: options.Credentials, newUpstream: options.NewUpstream, backend: options.Backend,
		builder: options.Builder, now: options.Now, logger: options.Logger,
		accountManager: options.AccountManager, broadcaster: options.Broadcaster,
		webUI: options.WebUI, oauthHandler: options.OAuthHandler, tracker: options.Tracker,
		projects: make(map[string]string),
	}

	cfg := config.Get()
	if cfg.OpenRouter.Enabled {
		openrouter.DefaultClient.WarmupCacheAsync(cfg.OpenRouter.APIKey, cfg.OpenRouter.BaseURL)
	}

	// Router state (sticky assignments, EWMA stats) survives restarts.
	openrouter.DefaultRouter.EnablePersistence(filepath.Join(config.GetConfigDir(), "openrouter-router.json"))

	return srv, nil
}

type responseWriterRecorder struct {
	http.ResponseWriter
	statusCode   int
	bytesWritten int64
}

func (r *responseWriterRecorder) WriteHeader(statusCode int) {
	r.statusCode = statusCode
	r.ResponseWriter.WriteHeader(statusCode)
}

func (r *responseWriterRecorder) Write(b []byte) (int, error) {
	if r.statusCode == 0 {
		r.statusCode = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytesWritten += int64(n)
	return n, err
}

func (r *responseWriterRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func shouldSkipLogging(path string) bool {
	if path == "/api/event_logging/batch" || path == "/v1/messages/count_tokens" {
		return true
	}
	if strings.HasPrefix(path, "/.well-known/") {
		return true
	}
	return false
}

func loggingMiddleware(next http.Handler, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if shouldSkipLogging(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		start := time.Now()
		rec := &responseWriterRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)

		status := rec.statusCode
		if status == 0 {
			status = http.StatusOK
		}
		duration := time.Since(start).Truncate(time.Millisecond)
		logMsg := fmt.Sprintf("[%s] %s %d (%s)", r.Method, r.URL.Path, status, duration)

		if log == nil {
			log = slog.Default()
		}

		switch {
		case status >= 500:
			log.Error(logMsg)
		case status >= 400:
			log.Warn(logMsg)
		default:
			log.Info(logMsg)
		}
	})
}

func (server *Server) Handler() http.Handler {
	return loggingMiddleware(http.HandlerFunc(server.serveHTTP), server.logger)
}

func (server *Server) serveHTTP(writer http.ResponseWriter, request *http.Request) {
	path := request.URL.Path
	if path == "/anthropic" {
		path = "/"
	} else if strings.HasPrefix(path, "/anthropic/") {
		path = strings.TrimPrefix(path, "/anthropic")
	}
	setCORS(writer)
	if request.Method == http.MethodOptions {
		writer.WriteHeader(http.StatusNoContent)
		return
	}

	// First try management handlers (/health, /account-limits, /api/*)
	if server.handleManagement(writer, request, path) {
		return
	}

	if path == "/" && request.Method == http.MethodPost {
		writeJSON(writer, http.StatusOK, map[string]any{"status": "ok"})
		return
	}

	if strings.HasPrefix(path, "/v1/") {
		if !server.authorized(request) {
			writeAPIError(writer, http.StatusUnauthorized, "authentication_error", "Invalid or missing API key")
			return
		}
		switch {
		case path == "/v1/models" && request.Method == http.MethodGet:
			server.models(writer, request)
		case path == "/v1/usage" && request.Method == http.MethodGet:
			server.usage(writer, request)
		case path == "/v1/messages" && request.Method == http.MethodPost:
			server.messages(writer, request)
		case path == "/v1/messages/count_tokens" && request.Method == http.MethodPost:
			writeAPIError(writer, http.StatusNotImplemented, "not_implemented", "Token counting is not implemented. Use /v1/messages with max_tokens or configure your client to skip token counting.")
		default:
			writeAPIError(writer, http.StatusNotFound, "not_found_error", fmt.Sprintf("Endpoint %s %s not found", request.Method, request.URL.Path))
		}
		return
	}

	// Web UI static assets fallback
	if server.webUI != nil && (request.Method == http.MethodGet || request.Method == http.MethodHead) {
		server.webUI.ServeHTTP(writer, request)
		return
	}

	writeAPIError(writer, http.StatusNotFound, "not_found_error", fmt.Sprintf("Endpoint %s %s not found", request.Method, request.URL.Path))
}

func (server *Server) authorized(request *http.Request) bool {
	if server.apiKey == "" {
		return true
	}
	provided := request.Header.Get("x-api-key")
	if provided == "" {
		if authorization := request.Header.Get("Authorization"); strings.HasPrefix(authorization, "Bearer ") {
			provided = strings.TrimPrefix(authorization, "Bearer ")
		}
	}
	return subtle.ConstantTimeCompare([]byte(provided), []byte(server.apiKey)) == 1
}

func (server *Server) health(writer http.ResponseWriter) {
	writeJSON(writer, http.StatusOK, map[string]any{
		"status": "ok", "timestamp": server.now().UTC().Format(time.RFC3339Nano),
	})
}

func (server *Server) models(writer http.ResponseWriter, request *http.Request) {
	catalog, err := server.fetchModelCatalog(request.Context())
	if err != nil {
		server.writeError(writer, err)
		return
	}
	selectable := catalog.PublicModels()
	models := make([]any, 0, len(selectable))
	for _, details := range selectable {
		description := details.DisplayName
		if description == "" {
			description = details.ID
		}
		ownedBy := "google"
		switch proxyformat.GetModelFamily(details.ID) {
		case proxyformat.FamilyClaude:
			ownedBy = "anthropic"
		case proxyformat.FamilyOpenAI:
			ownedBy = "openai"
		}
		models = append(models, map[string]any{
			"id": details.ID, "object": "model", "created": server.now().Unix(),
			"owned_by": ownedBy, "description": description,
			"context_window": details.MaxTokens, "max_output_tokens": details.MaxOutputTokens,
			"supports_thinking": details.SupportsThinking,
		})
	}
	cfg := config.Get()
	if cfg.OpenRouter.Enabled {
		for _, item := range cfg.OpenRouter.Allowlist {
			if !item.Enabled {
				continue
			}
			desc := item.DisplayName
			if desc == "" {
				desc = item.ID
			}
			contextLen := item.ContextLen
			if contextLen <= 0 {
				contextLen = 200000
			}
			maxOutput := item.MaxOutputTokens
			if maxOutput <= 0 {
				maxOutput = contextLen
			}
			models = append(models, map[string]any{
				"id":                item.ID,
				"object":            "model",
				"created":           server.now().Unix(),
				"owned_by":          "openrouter",
				"description":       desc,
				"context_window":    contextLen,
				"max_output_tokens": maxOutput,
				"supports_thinking": true,
			})
			if item.Alias != "" && item.Alias != item.ID {
				models = append(models, map[string]any{
					"id":                item.Alias,
					"object":            "model",
					"created":           server.now().Unix(),
					"owned_by":          "openrouter",
					"description":       desc + " (Alias)",
					"context_window":    contextLen,
					"max_output_tokens": maxOutput,
					"supports_thinking": true,
				})
			}
		}
	}
	writeJSON(writer, http.StatusOK, map[string]any{"object": "list", "data": models})
}

func (server *Server) usage(writer http.ResponseWriter, request *http.Request) {
	catalog, err := server.fetchModelCatalog(request.Context())
	if err != nil {
		server.writeError(writer, err)
		return
	}
	selectable := catalog.Selectable()
	models := make([]any, 0, len(selectable))
	for _, details := range selectable {
		if details.QuotaRemainingFraction == nil {
			continue
		}
		remaining := min(1, max(0, *details.QuotaRemainingFraction))
		models = append(models, map[string]any{
			"id":                 details.ID,
			"display_name":       details.DisplayName,
			"remaining_fraction": remaining,
			"used_percent":       (1 - remaining) * 100,
			"reset_at":           details.QuotaResetTime,
		})
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"object":     "account_usage",
		"provider":   "antigravity-proxy",
		"source":     "cloudcode.fetchAvailableModels",
		"fetched_at": server.now().UTC().Format(time.RFC3339Nano),
		"windows":    groupQuotaWindows(selectable),
		"models":     models,
	})
}

func groupQuotaWindows(models []modelcatalog.Model) []any {
	type group struct {
		remaining float64
		resetAt   string
		modelIDs  []string
		families  []string
	}
	groups := make([]group, 0)
	byQuota := make(map[string]int)
	for _, model := range models {
		if model.QuotaRemainingFraction == nil {
			continue
		}
		remaining := min(1, max(0, *model.QuotaRemainingFraction))
		key := strconv.FormatFloat(remaining, 'g', -1, 64) + "\x00" + model.QuotaResetTime
		index, exists := byQuota[key]
		if !exists {
			index = len(groups)
			byQuota[key] = index
			groups = append(groups, group{remaining: remaining, resetAt: model.QuotaResetTime})
		}
		groups[index].modelIDs = append(groups[index].modelIDs, model.ID)
		family := quotaFamily(model.ID)
		if family != "" && !containsString(groups[index].families, family) {
			groups[index].families = append(groups[index].families, family)
		}
	}
	windows := make([]any, 0, len(groups))
	for _, group := range groups {
		label := strings.Join(group.families, " / ")
		if label == "" {
			label = "Model"
		}
		windows = append(windows, map[string]any{
			"label":              label + " quota",
			"remaining_fraction": group.remaining,
			"used_percent":       (1 - group.remaining) * 100,
			"reset_at":           group.resetAt,
			"model_ids":          group.modelIDs,
		})
	}
	return windows
}

func quotaFamily(model string) string {
	switch proxyformat.GetModelFamily(model) {
	case proxyformat.FamilyGemini:
		return "Gemini"
	case proxyformat.FamilyClaude:
		return "Anthropic"
	case proxyformat.FamilyOpenAI:
		return "GPT-OSS"
	default:
		return ""
	}
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func (server *Server) fetchModelCatalog(ctx context.Context) (*modelcatalog.Catalog, error) {
	var response cloudcode.Response
	var err error
	accountLabel := "account pool"
	if server.backend != nil {
		response, err = server.backend.FetchAvailableModels(ctx)
	} else {
		var credentials auth.Credentials
		var upstream Upstream
		credentials, upstream, err = server.client(ctx)
		if err == nil {
			accountLabel = credentials.Email
			response, err = upstream.FetchAvailableModels(ctx, "")
		}
	}
	if err != nil {
		return nil, err
	}
	catalog, err := modelcatalog.Parse(response.Body)
	if err != nil {
		return nil, fmt.Errorf("decode Cloud Code models for %s: %w", accountLabel, err)
	}
	return catalog, nil
}

func (server *Server) messages(writer http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(writer, request.Body, maxRequestBody)
	decoder := json.NewDecoder(request.Body)
	var anthropicRequest map[string]any
	if err := decoder.Decode(&anthropicRequest); err != nil {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request_error", "Invalid JSON request body: "+err.Error())
		return
	}
	messages, ok := anthropicRequest["messages"].([]any)
	if !ok {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request_error", "messages is required and must be an array")
		return
	}
	if model, _ := anthropicRequest["model"].(string); model == "" {
		anthropicRequest["model"] = "gemini-3.5-flash-low"
	}
	cfg := config.Get()
	if cfg.ModelMapping != nil {
		reqModel := stringFrom(anthropicRequest["model"])
		current := reqModel
		visited := make(map[string]bool)
		for i := 0; i < maxMappingHops; i++ {
			if visited[current] {
				break
			}
			visited[current] = true
			mappingVal, exists := cfg.ModelMapping[current]
			if !exists {
				break
			}
			var mappedModel string
			switch v := mappingVal.(type) {
			case string:
				mappedModel = v
			case map[string]any:
				mappedModel, _ = v["mapping"].(string)
			}
			if mappedModel == "" || mappedModel == current {
				break
			}
			current = mappedModel
		}
		if current != reqModel {
			slog.Info(fmt.Sprintf("[Server] Mapping model %s -> %s", reqModel, current))
			anthropicRequest["model"] = current
		}
	}
	if _, exists := anthropicRequest["max_tokens"]; !exists {
		anthropicRequest["max_tokens"] = 4096
	}
	if len(messages) == 1 && messages[0] != nil {
		if message, ok := messages[0].(map[string]any); ok && message["content"] == "count" {
			writeJSON(writer, http.StatusOK, map[string]any{})
			return
		}
	}

	model := stringFrom(anthropicRequest["model"])
	if cfg.OpenRouter.Enabled {
		for _, item := range cfg.OpenRouter.Allowlist {
			if !item.Enabled {
				continue
			}
			if item.ID == model || (item.Alias != "" && item.Alias == model) {
				anthropicRequest["model"] = item.ID
				reqBody, err := json.Marshal(anthropicRequest)
				if err != nil {
					writeAPIError(writer, http.StatusBadRequest, "invalid_request_error", "Failed to marshal request: "+err.Error())
					return
				}
				server.forwardToOpenRouter(writer, request, cfg.OpenRouter, reqBody, anthropicRequest)
				return
			}
		}
	}

	if endpoint, exists := cfg.CustomEndpoints[model]; exists && endpoint.URL != "" {
		reqBody, err := json.Marshal(anthropicRequest)
		if err != nil {
			writeAPIError(writer, http.StatusBadRequest, "invalid_request_error", "Failed to marshal request: "+err.Error())
			return
		}
		server.forwardToCustomEndpoint(writer, request, endpoint, reqBody)
		return
	}

	var send streamSender
	if server.backend != nil {
		send = func(ctx context.Context, consume func(cloudcode.SSEEvent) error) (cloudcode.Response, error) {
			return server.backend.StreamGenerateContent(ctx, anthropicRequest, consume)
		}
	} else {
		credentials, upstream, err := server.client(request.Context())
		if err != nil {
			server.writeError(writer, err)
			return
		}
		projectID, err := server.resolveProject(request.Context(), credentials, upstream)
		if err != nil {
			server.writeError(writer, err)
			return
		}
		payload := server.builder.BuildCloudCodeRequest(anthropicRequest, projectID, credentials.Email)
		innerRequest, _ := payload["request"].(map[string]any)
		options := cloudcode.RequestOptions{SessionID: stringFrom(innerRequest["sessionId"])}
		if proxyformat.GetModelFamily(model) == proxyformat.FamilyClaude && proxyformat.IsThinkingModel(model) {
			options.Headers = make(http.Header)
			options.Headers.Set("anthropic-beta", "interleaved-thinking-2025-05-14")
		}
		send = func(ctx context.Context, consume func(cloudcode.SSEEvent) error) (cloudcode.Response, error) {
			return upstream.StreamGenerateContent(ctx, payload, options, consume)
		}
	}

	if stream, _ := anthropicRequest["stream"].(bool); stream {
		server.streamMessage(writer, request, send, model)
		return
	}
	server.unaryMessage(writer, request, send, model)
}

func (server *Server) forwardToCustomEndpoint(writer http.ResponseWriter, request *http.Request, endpoint config.EndpointConfig, reqBody []byte) {
	targetURL, err := url.Parse(endpoint.URL)
	if err != nil {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request_error", "Invalid custom endpoint URL: "+err.Error())
		return
	}
	if !strings.HasSuffix(targetURL.Path, "/v1/messages") {
		targetURL.Path = strings.TrimSuffix(targetURL.Path, "/") + "/v1/messages"
	}

	proxy := &httputil.ReverseProxy{
		FlushInterval: -1,
		Director: func(req *http.Request) {
			req.URL.Scheme = targetURL.Scheme
			req.URL.Host = targetURL.Host
			req.URL.Path = targetURL.Path
			req.URL.RawQuery = targetURL.RawQuery
			req.Host = targetURL.Host

			req.Body = io.NopCloser(bytes.NewReader(reqBody))
			req.ContentLength = int64(len(reqBody))

			if endpoint.APIKey != "" {
				req.Header.Set("x-api-key", endpoint.APIKey)
			}
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, proxyErr error) {
			server.logger.Error("custom endpoint proxy error", "error", proxyErr, "url", targetURL.String())
			writeAPIError(w, http.StatusBadGateway, "api_error", "Custom endpoint forwarding error: "+proxyErr.Error())
		},
	}

	proxy.ServeHTTP(writer, request)
}

func (server *Server) forwardToOpenRouter(writer http.ResponseWriter, request *http.Request, openRouterCfg config.OpenRouterConfig, reqBody []byte, anthropicRequest map[string]any) {
	baseURL := openrouter.NormalizeBaseURL(openRouterCfg.BaseURL)
	targetURL, err := url.Parse(baseURL + "/v1/messages")
	if err != nil {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request_error", "Invalid OpenRouter target URL: "+err.Error())
		return
	}

	model := stringFrom(anthropicRequest["model"])
	sessionID := openrouter.ExtractSessionID(request, anthropicRequest)
	pricing, _ := openrouter.DefaultClient.ResolveModelPricing(request.Context(), model, openRouterCfg.APIKey, openRouterCfg.BaseURL)
	startTime := server.now()
	deadline := startTime.Add(2 * time.Minute)
	if openRouterCfg.Routing.RequestBudgetMs > 0 {
		deadline = startTime.Add(time.Duration(openRouterCfg.Routing.RequestBudgetMs) * time.Millisecond)
	}

	// Resolve per-model provider order from the allowlist item. Missing entry = auto.
	var perModel config.OpenRouterModelConfig
	for _, item := range openRouterCfg.Allowlist {
		if item.ID == model {
			perModel = item
			break
		}
	}
	order := openrouter.ProviderOrder{
		Mode:  stringDefault(perModel.ProviderMode, "auto"),
		Pin:   perModel.PinnedProvider,
		Order: perModel.ProviderOrder,
	}
	// Sync router config if the cfg has it.
	openrouter.DefaultRouter.SetConfig(openrouter.RoutingConfig{
		FailureThreshold: openRouterCfg.Routing.FailureThreshold,
		RankWeights:      openRouterCfg.Routing.RankWeightsToOpenRouter(),
	})

	// Build the ordered failover chain: a single provider for "pinned", the
	// configured order for "custom", sticky-then-ranked for "auto".
	candidates := openrouter.DefaultRouter.SelectChain(sessionID, model, order)

	// Ensure endpoints are ranked. Cache hit refreshes ranks if missing; miss
	// fires an async warmup (which refreshes ranks on success) and this request
	// proceeds unpinned.
	if endpoints, ok := openrouter.DefaultEndpointsClient.GetCachedEndpoints(model, baseURL); ok {
		if ranks := openrouter.DefaultRouter.GetRanks(model); len(ranks) == 0 {
			openrouter.DefaultRouter.RefreshRanks(model, endpoints)
		}
	} else {
		openrouter.DefaultEndpointsClient.WarmupEndpointsAsync(model, openRouterCfg.APIKey, baseURL)
	}

	// Per-attempt classification: what should we do next on this provider?
	const (
		nextRetrySame    = iota // retry same provider
		nextNextProvider        // advance to next provider
		nextGiveUp              // return last error
	)

	classify := func(status int, networkErr error) (action int, backoff time.Duration) {
		if networkErr != nil {
			return nextNextProvider, 200 * time.Millisecond
		}
		switch {
		case status == http.StatusTooManyRequests:
			return nextRetrySame, 0 // backoff computed by caller using 429 settings
		case status >= 500, status == http.StatusBadGateway, status == http.StatusServiceUnavailable:
			return nextNextProvider, 200 * time.Millisecond
		case status >= 400:
			return nextNextProvider, 0 // immediate
		default:
			return nextGiveUp, 0
		}
	}

	httpClient := openRouterUpstreamClient()

	var (
		lastStatus  int
		lastBody    []byte
		providerIdx = 0
		consec429   int
		tried       = make(map[string]bool)
	)

	// No ranked/pinned/custom provider available — single unpinned attempt
	// (equivalent to the pre-routing passthrough behavior).
	if len(candidates) == 0 {
		candidates = []string{""}
	}

	for {
		if time.Now().After(deadline) {
			break
		}
		if request.Context().Err() != nil {
			// Client disconnected — abort retry loop.
			return
		}
		if providerIdx >= len(candidates) {
			break
		}
		provider := candidates[providerIdx]
		if tried[provider] {
			providerIdx++
			continue
		}
		tried[provider] = true

		// Build body with provider injection.
		body := injectProvider(reqBody, provider, order.Mode)
		attemptStart := server.now()

		// Per-endpoint pricing wins over model-level pricing when known.
		attemptPricing := pricing
		if provider != "" {
			if ep := endpointPricing(model, provider); ep != nil {
				attemptPricing = *ep
			}
		}

		upReq, err := http.NewRequestWithContext(request.Context(), http.MethodPost, targetURL.String(), bytes.NewReader(body))
		if err != nil {
			writeAPIError(writer, http.StatusInternalServerError, "api_error", "Failed to build request: "+err.Error())
			return
		}
		upReq.Header.Set("Content-Type", "application/json")
		upReq.Header.Set("Accept", "application/json")
		if openRouterCfg.APIKey != "" {
			apiKey := strings.TrimSpace(openRouterCfg.APIKey)
			upReq.Header.Set("Authorization", "Bearer "+apiKey)
			upReq.Header.Set("x-api-key", apiKey)
		}
		if av := request.Header.Get("anthropic-version"); av != "" {
			upReq.Header.Set("anthropic-version", av)
		}
		if ab := request.Header.Get("anthropic-beta"); ab != "" {
			upReq.Header.Set("anthropic-beta", ab)
		}

		resp, err := httpClient.Do(upReq)
		if err != nil {
			if provider != "" {
				openrouter.DefaultRouter.RecordResult(model, provider, false, server.now().Sub(attemptStart), 0)
			}
			_, backoff := classify(0, err)
			if backoff > 0 {
				time.Sleep(backoff)
			}
			providerIdx++
			lastStatus = 0
			continue
		}

		// 2xx — handle success
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			isStream := strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")
			if isStream {
				server.proxyStreamResponse(writer, resp, model, sessionID, attemptPricing, startTime, attemptStart, provider)
				return
			}
			// Buffer full body before writing — failover impossible after first byte.
			bodyBytes, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if readErr != nil {
				if provider != "" {
					openrouter.DefaultRouter.RecordResult(model, provider, false, server.now().Sub(attemptStart), 0)
				}
				providerIdx++
				continue
			}
			// Capture served provider from response if present
			servedProvider := extractServedProviderJSON(bodyBytes)
			if servedProvider != "" {
				provider = servedProvider
			}
			// Write headers + status
			for k, vs := range resp.Header {
				if strings.EqualFold(k, "Content-Length") || strings.EqualFold(k, "Transfer-Encoding") {
					continue
				}
				for _, v := range vs {
					writer.Header().Add(k, v)
				}
			}
			writer.WriteHeader(resp.StatusCode)
			_, _ = writer.Write(bodyBytes)

			// Observability + record result
			in, out, cr, cw := openrouter.ParseUsageFromJSON(bodyBytes)
			if provider != "" {
				openrouter.DefaultRouter.RecordResult(model, provider, true, server.now().Sub(attemptStart), in+out)
			}
			server.recordOpenRouterMetrics(model, sessionID, attemptPricing, startTime, in, out, cr, cw, provider)
			return
		}

		// Non-2xx: buffer body, classify, decide next.
		bodyBytes, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		lastStatus = resp.StatusCode
		lastBody = bodyBytes
		action, backoff := classify(resp.StatusCode, nil)
		if provider != "" {
			openrouter.DefaultRouter.RecordResult(model, provider, false, server.now().Sub(attemptStart), 0)
		}

		switch action {
		case nextRetrySame:
			consec429++
			max429 := openRouterCfg.Routing.Retry429Max
			if max429 <= 0 {
				max429 = 10
			}
			if consec429 > max429 {
				providerIdx++
				consec429 = 0
				continue
			}
			base := openRouterCfg.Routing.BackoffBaseMs
			if base <= 0 {
				base = 500
			}
			cap := openRouterCfg.Routing.BackoffCapMs
			if cap <= 0 {
				cap = 120000
			}
			d := computeBackoff(consec429, time.Duration(base)*time.Millisecond, time.Duration(cap)*time.Millisecond)
			if time.Now().Add(d).After(deadline) {
				break
			}
			time.Sleep(d)
			// Don't advance providerIdx; re-enter the loop with same provider.
			tried[provider] = false
			continue
		case nextNextProvider:
			consec429 = 0
			if backoff > 0 {
				time.Sleep(backoff)
			}
			providerIdx++
			continue
		default:
			// nextGiveUp
		}
		break
	}

	// Out of candidates or budget exhausted — return last error.
	status := lastStatus
	if status == 0 {
		status = http.StatusBadGateway
	}
	server.logger.Warn("OpenRouter forward exhausted",
		"model", model, "status", status, "tried", len(tried))
	writeAPIError(writer, status, "api_error", fmt.Sprintf("OpenRouter upstream failed after %d attempt(s): %s", len(tried), truncate(string(lastBody), 256)))
}

// openRouterUpstreamClient returns the HTTP client for OpenRouter upstream
// calls. It intentionally has no total Timeout: a total timeout covers the
// full body read and would kill long-running SSE streams mid-generation.
// Cancellation comes from the inbound request context and the retry budget.
func openRouterUpstreamClient() *http.Client {
	return &http.Client{}
}

// injectProvider adds the OpenRouter "provider" routing key to the request body.
// `mode` controls single vs full list:
//   - "pinned" / "auto": single-entry order with allow_fallbacks=false
//   - "custom": full order with allow_fallbacks=false
func injectProvider(body []byte, provider string, mode string) []byte {
	if provider == "" {
		return body
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body
	}
	switch mode {
	case "custom":
		// Failover walks the configured order one provider per attempt, so each
		// request body pins the current candidate only.
		payload["provider"] = map[string]any{
			"order":           []string{provider},
			"allow_fallbacks": false,
		}
	default:
		payload["provider"] = map[string]any{
			"order":           []string{provider},
			"allow_fallbacks": false,
		}
	}
	out, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return out
}

// extractServedProviderJSON returns the top-level "provider" field if present.
func extractServedProviderJSON(body []byte) string {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return ""
	}
	if s, ok := raw["provider"].(string); ok {
		return s
	}
	return ""
}

func stringDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func computeBackoff(attempt int, base, cap time.Duration) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := base
	for i := 1; i < attempt; i++ {
		d *= 2
		if d > cap {
			return cap
		}
	}
	if d > cap {
		return cap
	}
	return d
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// proxyStreamResponse streams a successful response to the client while
// capturing usage and the served provider via SSE.
func (server *Server) proxyStreamResponse(writer http.ResponseWriter, resp *http.Response, model, sessionID string, pricing openrouter.Pricing, startTime, attemptStart time.Time, provider string) {
	for k, vs := range resp.Header {
		if strings.EqualFold(k, "Content-Length") || strings.EqualFold(k, "Transfer-Encoding") {
			continue
		}
		for _, v := range vs {
			writer.Header().Add(k, v)
		}
	}
	writer.WriteHeader(resp.StatusCode)
	flusher, hasFlusher := writer.(http.Flusher)

	finalPricing := resolveEffectivePricing(pricing, model)
	var interceptor *openrouter.SSEInterceptor
	interceptor = openrouter.NewSSEInterceptor(resp.Body, func(in, out, cr, cw int) {
		// Prefer the provider reported by the stream over the requested one.
		served := provider
		if p := interceptor.Provider(); p != "" {
			served = p
		}
		if served != "" {
			openrouter.DefaultRouter.RecordResult(model, served, true, server.now().Sub(attemptStart), in+out)
		}
		server.recordOpenRouterMetrics(model, sessionID, finalPricing, startTime, in, out, cr, cw, served)
	})
	defer interceptor.Close()

	buf := make([]byte, 4096)
	for {
		n, err := interceptor.Read(buf)
		if n > 0 {
			_, _ = writer.Write(buf[:n])
			if hasFlusher {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

// recordOpenRouterMetrics is shared between stream and unary paths.
func (server *Server) recordOpenRouterMetrics(model, sessionID string, pricing openrouter.Pricing, startTime time.Time, in, out, cr, cw int, provider string) {
	latency := server.now().Sub(startTime)
	metrics := openrouter.RequestMetrics{
		Model:               model,
		SessionID:           sessionID,
		Provider:            provider,
		InputTokens:         in,
		OutputTokens:        out,
		CacheReadTokens:     cr,
		CacheCreationTokens: cw,
		Latency:             latency,
	}
	metrics.ComputeFinalMetrics(pricing, openrouter.DefaultSessionTracker)
	openrouter.LogObservability(server.logger, metrics)
	if server.tracker != nil {
		server.tracker.TrackRequest(model, latency, in, out, cr)
	}
}

func resolveEffectivePricing(initial openrouter.Pricing, model string) openrouter.Pricing {
	if initial.Prompt == 0 && initial.Completion == 0 {
		if p, ok := openrouter.DefaultClient.GetModelPricing(model); ok {
			return p
		}
	}
	return initial
}

// endpointPricing returns the per-endpoint pricing for a provider from the
// router's current rank list, or nil when unknown.
func endpointPricing(model, provider string) *openrouter.Pricing {
	for _, r := range openrouter.DefaultRouter.GetRanks(model) {
		if r.Provider == provider && r.Endpoint.Pricing != nil {
			return r.Endpoint.Pricing
		}
	}
	return nil
}

type streamSender func(context.Context, func(cloudcode.SSEEvent) error) (cloudcode.Response, error)

func (server *Server) unaryMessage(writer http.ResponseWriter, request *http.Request, send streamSender, model string) {
	startTime := server.now()
	accumulator := proxyformat.NewThinkingAccumulator()
	_, err := send(request.Context(), func(event cloudcode.SSEEvent) error {
		return accumulator.Consume(event.Data)
	})
	if err != nil {
		server.writeError(writer, err)
		return
	}
	latency := server.now().Sub(startTime)
	if server.tracker != nil {
		server.tracker.TrackRequest(model, latency, accumulator.InputTokens(), accumulator.OutputTokens(), accumulator.CacheReadTokens())
	}
	response := accumulator.Response(model, server.builder.Cache, "")
	writeJSON(writer, http.StatusOK, response)
}

func (server *Server) streamMessage(writer http.ResponseWriter, request *http.Request, send streamSender, model string) {
	startTime := server.now()
	converter := proxyformat.NewStreamConverter(model, server.builder.Cache, "")
	started := false
	flusher, hasFlusher := writer.(http.Flusher)
	bw := bufio.NewWriterSize(writer, 4096)
	writeEvents := func(events []map[string]any) error {
		if len(events) == 0 {
			return nil
		}
		if !started {
			writer.Header().Set("Content-Type", "text/event-stream")
			writer.Header().Set("Cache-Control", "no-cache")
			writer.Header().Set("Connection", "keep-alive")
			writer.Header().Set("X-Accel-Buffering", "no")
			writer.WriteHeader(http.StatusOK)
			started = true
		}
		for _, event := range events {
			buf := jsonBufferPool.Get().(*bytes.Buffer)
			buf.Reset()
			err := json.NewEncoder(buf).Encode(event)
			if err == nil {
				eventType, _ := event["type"].(string)
				bw.WriteString("event: ")
				bw.WriteString(eventType)
				bw.WriteString("\ndata: ")
				// json.Encoder adds a trailing newline, so we only need one more
				bw.Write(buf.Bytes())
				bw.WriteString("\n")
			}
			jsonBufferPool.Put(buf)
			if err != nil {
				return err
			}
		}
		bw.Flush()
		if hasFlusher {
			flusher.Flush()
		}
		return nil
	}
	_, err := send(request.Context(), func(event cloudcode.SSEEvent) error {
		events, err := converter.Consume(event.Data)
		if err != nil {
			return err
		}
		return writeEvents(events)
	})
	if err == nil {
		var events []map[string]any
		events, err = converter.Finish()
		if err == nil {
			err = writeEvents(events)
		}
	}
	if err == nil {
		if server.tracker != nil {
			latency := server.now().Sub(startTime)
			server.tracker.TrackRequest(model, latency, converter.InputTokens(), converter.OutputTokens(), converter.CacheReadTokens())
		}
		return
	}
	if !started {
		server.writeError(writer, err)
		return
	}
	errorEvent := map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": err.Error()}}
	_ = writeEvents([]map[string]any{errorEvent})
}

func (server *Server) client(ctx context.Context) (auth.Credentials, Upstream, error) {
	server.mu.Lock()
	credentials := server.cachedCredentials
	if credentials.AccessToken != "" && credentials.Expiry.Sub(server.now()) > time.Minute {
		upstream := server.upstream
		server.mu.Unlock()
		return credentials, upstream, nil
	}
	server.mu.Unlock()

	credentials, err := server.credentials(ctx)
	if err != nil {
		return auth.Credentials{}, nil, err
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	server.cachedCredentials = credentials
	if server.upstream == nil || server.upstreamToken != credentials.AccessToken {
		if closer, ok := server.upstream.(interface{ CloseIdleConnections() }); ok {
			closer.CloseIdleConnections()
		}
		server.upstream = server.newUpstream(credentials.AccessToken)
		server.upstreamToken = credentials.AccessToken
	}
	return credentials, server.upstream, nil
}

func (server *Server) resolveProject(ctx context.Context, credentials auth.Credentials, upstream Upstream) (string, error) {
	if server.projectID != "" {
		return server.projectID, nil
	}
	server.mu.Lock()
	projectID := server.projects[credentials.Email]
	server.mu.Unlock()
	if projectID != "" {
		return projectID, nil
	}
	response, err := upstream.LoadCodeAssist(ctx, "")
	if err != nil {
		return "", fmt.Errorf("discover managed Cloud Code project: %w", err)
	}
	var document map[string]any
	if err := json.Unmarshal(response.Body, &document); err != nil {
		return "", fmt.Errorf("decode loadCodeAssist response: %w", err)
	}
	projectID = stringFrom(document["cloudaicompanionProject"])
	if project := objectFrom(document["cloudaicompanionProject"]); projectID == "" {
		projectID = stringFrom(project["id"])
	}
	if projectID == "" {
		return "", errors.New("loadCodeAssist response did not include a Cloud Code project")
	}
	server.mu.Lock()
	server.projects[credentials.Email] = projectID
	server.mu.Unlock()
	return projectID, nil
}

func (server *Server) writeError(writer http.ResponseWriter, err error) {
	server.logger.Error("API request failed", "error", err)
	status, kind, message := classifyError(err)
	writeAPIError(writer, status, kind, message)
}

func classifyError(err error) (int, string, string) {
	var selectionError *modelcatalog.SelectionError
	if errors.As(err, &selectionError) {
		return http.StatusBadRequest, "invalid_request_error", selectionError.Error()
	}
	var upstreamError *cloudcode.HTTPError
	if errors.As(err, &upstreamError) {
		switch upstreamError.StatusCode {
		case http.StatusUnauthorized:
			return http.StatusUnauthorized, "authentication_error", "Authentication failed. Make sure Antigravity has a valid token."
		case http.StatusForbidden:
			return http.StatusForbidden, "permission_error", upstreamError.Error()
		case http.StatusTooManyRequests:
			return http.StatusBadRequest, "invalid_request_error", "RESOURCE_EXHAUSTED: capacity is exhausted for this model. Please wait for quota to reset."
		case http.StatusBadRequest, http.StatusNotFound:
			return http.StatusBadRequest, "invalid_request_error", upstreamError.Error()
		default:
			return http.StatusServiceUnavailable, "api_error", upstreamError.Error()
		}
	}
	if errors.Is(err, proxyformat.ErrEmptyResponse) {
		return http.StatusBadGateway, "api_error", err.Error()
	}
	return http.StatusInternalServerError, "api_error", err.Error()
}

func writeAPIError(writer http.ResponseWriter, status int, kind, message string) {
	writeJSON(writer, status, map[string]any{"type": "error", "error": map[string]any{"type": kind, "message": message}})
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func setCORS(writer http.ResponseWriter) {
	writer.Header().Set("Access-Control-Allow-Origin", "*")
	writer.Header().Set("Access-Control-Allow-Headers", "authorization, content-type, x-api-key, anthropic-version, anthropic-beta")
	writer.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
}

func stringFrom(value any) string {
	text, _ := value.(string)
	return text
}

func objectFrom(value any) map[string]any {
	object, _ := value.(map[string]any)
	return object
}
