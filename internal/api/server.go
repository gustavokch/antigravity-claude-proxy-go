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
	"math/rand"
	"net"
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
	"antigravity-go-proxy/internal/claudecode"
	"antigravity-go-proxy/internal/cloudcode"
	"antigravity-go-proxy/internal/config"
	proxyformat "antigravity-go-proxy/internal/format"
	"antigravity-go-proxy/internal/headroom"
	"antigravity-go-proxy/internal/kimi"
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
	ClaudeCodeOAuthMgr *auth.ClaudeCodeOAuthManager
}

type Server struct {
	apiKey             string
	projectID          string
	credentials        func(context.Context) (auth.Credentials, error)
	newUpstream        func(string) Upstream
	backend            Backend
	builder            *proxyformat.Builder
	now                func() time.Time
	logger             *slog.Logger
	accountManager     *accounts.Manager
	broadcaster        *logger.Broadcaster
	webUI              http.Handler
	oauthHandler       http.Handler
	claudeCodeOAuthMgr *auth.ClaudeCodeOAuthManager
	tracker            *stats.Tracker
	headroom           *headroom.Engine

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
	if options.ClaudeCodeOAuthMgr == nil {
		options.ClaudeCodeOAuthMgr = auth.NewClaudeCodeOAuthManager()
	}
	srv := &Server{
		apiKey: options.APIKey, projectID: options.ProjectID,
		credentials: options.Credentials, newUpstream: options.NewUpstream, backend: options.Backend,
		builder: options.Builder, now: options.Now, logger: options.Logger,
		accountManager: options.AccountManager, broadcaster: options.Broadcaster,
		webUI: options.WebUI, oauthHandler: options.OAuthHandler, tracker: options.Tracker,
		claudeCodeOAuthMgr: options.ClaudeCodeOAuthMgr,
		projects:           make(map[string]string),
	}

	cfg := config.Get()
	srv.headroom = headroom.NewEngine(cfg.Headroom)
	if cfg.OpenRouter.Enabled {
		openrouter.DefaultClient.WarmupCacheAsync(cfg.OpenRouter.APIKey, cfg.OpenRouter.BaseURL)
	}

	// Router state (sticky assignments, EWMA stats) survives restarts.
	openrouter.DefaultRouter.EnablePersistence(filepath.Join(config.GetConfigDir(), "openrouter-router.json"))
	applyRouterConfig(cfg.OpenRouter)

	return srv, nil
}

// applyRouterConfig pushes the persisted routing knobs into the live router.
// Called at startup and on config save — never per request (SetConfig takes
// the router write-lock).
func applyRouterConfig(openRouterCfg config.OpenRouterConfig) {
	openrouter.DefaultRouter.SetConfig(openrouter.RoutingConfig{
		FailureThreshold: openRouterCfg.Routing.FailureThreshold,
		RankWeights:      openRouterCfg.Routing.RankWeightsToOpenRouter(),
	})
}

func (server *Server) applyHeadroomConfig(cfg config.HeadroomConfig) {
	if server.headroom != nil {
		server.headroom.UpdateConfig(cfg)
	}
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
	seen := make(map[string]bool)

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
			"display_name": details.DisplayName,
			"context_window": details.MaxTokens, "max_output_tokens": details.MaxOutputTokens,
			"supports_thinking": details.SupportsThinking,
		})
		seen[details.ID] = true
	}
	cfg := config.Get()

	// Claude Code allowlist models and aliases
	if cfg.ClaudeCode.Enabled || len(cfg.ClaudeCode.Accounts) > 0 {
		allowlist := cfg.ClaudeCode.Allowlist
		if len(allowlist) == 0 {
			allowlist = claudecode.DefaultAllowlist()
		}
		for _, item := range allowlist {
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
				maxOutput = 8192
			}
			aliases := item.Aliases
			if item.Alias != "" {
				hasAlias := false
				for _, a := range aliases {
					if a == item.Alias {
						hasAlias = true
						break
					}
				}
				if !hasAlias {
					aliases = append(aliases, item.Alias)
				}
			}

			if !seen[item.ID] {
				entry := map[string]any{
					"id":                item.ID,
					"object":            "model",
					"created":           server.now().Unix(),
					"owned_by":          "anthropic",
					"description":       desc,
					"display_name":      desc,
					"context_window":    contextLen,
					"max_output_tokens": maxOutput,
					"supports_thinking": item.Thinking,
				}
				if len(aliases) > 0 {
					entry["aliases"] = aliases
				}
				models = append(models, entry)
				seen[item.ID] = true
			}

			for _, alias := range aliases {
				if alias != "" && alias != item.ID && !seen[alias] {
					models = append(models, map[string]any{
						"id":                alias,
						"object":            "model",
						"created":           server.now().Unix(),
						"owned_by":          "anthropic",
						"description":       desc + " (Alias)",
						"display_name":      desc + " (Alias)",
						"context_window":    contextLen,
						"max_output_tokens": maxOutput,
						"supports_thinking": item.Thinking,
					})
					seen[alias] = true
				}
			}
		}
	}

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
				"display_name":      desc,
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
					"display_name":      desc + " (Alias)",
					"context_window":    contextLen,
					"max_output_tokens": maxOutput,
					"supports_thinking": true,
				})
			}
		}
	}
	if cfg.Kimi.Enabled {
		for _, item := range cfg.Kimi.Allowlist {
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
				"owned_by":          "kimi",
				"description":       desc,
				"display_name":      desc,
				"context_window":    contextLen,
				"max_output_tokens": maxOutput,
				"supports_thinking": true,
			})
			if item.Alias != "" && item.Alias != item.ID {
				models = append(models, map[string]any{
					"id":                item.Alias,
					"object":            "model",
					"created":           server.now().Unix(),
					"owned_by":          "kimi",
					"description":       desc + " (Alias)",
					"display_name":      desc + " (Alias)",
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

	if server.headroom != nil {
		if hrCtx, err := server.headroom.Process(request.Context(), anthropicRequest); err != nil {
			server.logger.Warn("headroom pipeline failed; forwarding request unmodified", "error", err)
		} else if hrCtx.BytesBefore > 0 || hrCtx.EffortClamped {
			server.logger.Debug("headroom compressed request",
				"bytesBefore", hrCtx.BytesBefore, "bytesAfter", hrCtx.BytesAfter,
				"effortClamped", hrCtx.EffortClamped,
				"continuation", hrCtx.ContinuationKind,
				"verbatimSkipped", hrCtx.VerbatimSkipped)
			if server.tracker != nil {
				server.tracker.RecordHeadroom(stats.HeadroomSample{
					BytesBefore:           hrCtx.BytesBefore,
					BytesAfter:            hrCtx.BytesAfter,
					ThinkingTokensClamped: hrCtx.OriginalThinking - hrCtx.ClampedThinking,
				})
			}
		}
	}

	model := stringFrom(anthropicRequest["model"])
	if cfg.Kimi.Enabled {
		if kimiMatch := matchKimiModel(cfg.Kimi, model); kimiMatch != "" {
			anthropicRequest["model"] = kimiMatch
			reqBody, err := json.Marshal(anthropicRequest)
			if err != nil {
				writeAPIError(writer, http.StatusBadRequest, "invalid_request_error", "Failed to marshal Kimi request: "+err.Error())
				return
			}
			server.forwardToKimi(writer, request, cfg.Kimi, reqBody, kimiMatch)
			return
		}
	}
	if cfg.ClaudeCode.Enabled {
		if ccMatch := matchClaudeCodeModel(cfg.ClaudeCode, model); ccMatch != "" {
			anthropicRequest["model"] = ccMatch
			ccBody, err := json.Marshal(anthropicRequest)
			if err != nil {
				writeAPIError(writer, http.StatusBadRequest, "invalid_request_error", "Failed to marshal ClaudeCode request: "+err.Error())
				return
			}
			server.forwardToClaudeCode(writer, request, cfg.ClaudeCode, ccBody, ccMatch)
			return
		}
	}
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
		send = func(ctx context.Context, req map[string]any, consume func(cloudcode.SSEEvent) error) (cloudcode.Response, error) {
			return server.backend.StreamGenerateContent(ctx, req, consume)
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
		send = func(ctx context.Context, req map[string]any, consume func(cloudcode.SSEEvent) error) (cloudcode.Response, error) {
			dynPayload := server.builder.BuildCloudCodeRequest(req, projectID, credentials.Email)
			return upstream.StreamGenerateContent(ctx, dynPayload, options, consume)
		}
	}

	if stream, _ := anthropicRequest["stream"].(bool); stream {
		server.streamMessage(writer, request, send, anthropicRequest, model)
		return
	}
	server.unaryMessage(writer, request, send, anthropicRequest, model)
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

	if server.isCCREnabled() {
		var reqMap map[string]any
		if err := json.Unmarshal(reqBody, &reqMap); err == nil {
			sender := func(ctx context.Context, bodyBytes []byte) (*http.Response, error) {
				httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL.String(), bytes.NewReader(bodyBytes))
				if err != nil {
					return nil, err
				}
				httpReq.Header.Set("Content-Type", "application/json")
				if endpoint.APIKey != "" {
					httpReq.Header.Set("x-api-key", endpoint.APIKey)
				}
				if v := request.Header.Get("anthropic-version"); v != "" {
					httpReq.Header.Set("anthropic-version", v)
				} else {
					httpReq.Header.Set("anthropic-version", "2023-06-01")
				}
				if b := request.Header.Get("anthropic-beta"); b != "" {
					httpReq.Header.Set("anthropic-beta", b)
				}
				return http.DefaultClient.Do(httpReq)
			}

			opts := server.defaultCCROptions(sender)
			isStreaming, _ := reqMap["stream"].(bool)
			if isStreaming {
				_ = ProxyAnthropicStreamWithCCR(request.Context(), writer, reqMap, opts)
			} else {
				_ = ProxyAnthropicJSONWithCCR(request.Context(), writer, reqMap, opts)
			}
			return
		}
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

// forwardToKimi transparently forwards an /v1/messages request to the Kimi
// Code gateway. The Kimi endpoint is Anthropic-compatible, so no translation
// is needed: we rewrite Authorization, preserve the Anthropic version/beta
// headers, and stream the response back. When CCR is enabled, it hydrates headroom_retrieve calls.
func (server *Server) forwardToKimi(writer http.ResponseWriter, request *http.Request, kimiCfg config.KimiConfig, body []byte, model string) {
	if kimiCfg.APIKey == "" {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request_error", "Kimi gateway enabled but no API key configured")
		return
	}
	if server.logger != nil {
		server.logger.Info("kimi forward", "model", model)
	}

	if !server.isCCREnabled() {
		kimi.ForwardMessages(writer, request, kimiCfg.BaseURL, kimiCfg.APIKey, body)
		return
	}

	var reqMap map[string]any
	if err := json.Unmarshal(body, &reqMap); err != nil {
		kimi.ForwardMessages(writer, request, kimiCfg.BaseURL, kimiCfg.APIKey, body)
		return
	}

	targetURL := kimi.NormalizeBaseURL(kimiCfg.BaseURL) + "/v1/messages"
	sender := func(ctx context.Context, reqBytes []byte) (*http.Response, error) {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(reqBytes))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Authorization", "Bearer "+kimiCfg.APIKey)
		if v := request.Header.Get("anthropic-version"); v != "" {
			httpReq.Header.Set("anthropic-version", v)
		} else {
			httpReq.Header.Set("anthropic-version", "2023-06-01")
		}
		if b := request.Header.Get("anthropic-beta"); b != "" {
			httpReq.Header.Set("anthropic-beta", b)
		}
		return http.DefaultClient.Do(httpReq)
	}

	opts := server.defaultCCROptions(sender)
	isStreaming, _ := reqMap["stream"].(bool)
	if isStreaming {
		_ = ProxyAnthropicStreamWithCCR(request.Context(), writer, reqMap, opts)
	} else {
		_ = ProxyAnthropicJSONWithCCR(request.Context(), writer, reqMap, opts)
	}
}

func (server *Server) defaultCCROptions(sender CCRSender) CCRProxyOptions {
	return CCRProxyOptions{
		IsCCREnabled: func() bool {
			return server.isCCREnabled()
		},
		GetChunk: func(chunkID string) (string, bool) {
			return server.getCCRChunkPayload(chunkID)
		},
		RecordHeadroom: func(count int) {
			if server.tracker != nil {
				server.tracker.RecordHeadroom(stats.HeadroomSample{
					CCRRetrievals: count,
				})
			}
		},
		Sender:        sender,
		MaxHydrations: maxCCRHydrations,
	}
}

// matchKimiModel returns the Kimi model ID if `model` matches an enabled
// allowlist entry by either ID or alias. Returns "" if no match.
func matchKimiModel(cfg config.KimiConfig, model string) string {
	if model == "" {
		return ""
	}
	for _, item := range cfg.Allowlist {
		if !item.Enabled {
			continue
		}
		if (item.ID != "" && item.ID == model) || (item.Alias != "" && item.Alias == model) {
			return item.ID
		}
	}
	return ""
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
		case status >= 500:
			return nextNextProvider, 200 * time.Millisecond
		case status >= 400:
			return nextNextProvider, 0 // immediate
		default:
			return nextGiveUp, 0
		}
	}

	httpClient := openRouterUpstreamClient()

	// App identity for harness-gated models: OpenRouter 403s models restricted
	// to agentic harnesses when the request carries no app attribution. On that
	// error the attempt is retried once with spoofed attribution headers.
	spoofTitle := strings.TrimSpace(openRouterCfg.AppSpoof.Title)
	if spoofTitle == "" {
		spoofTitle = openrouter.DefaultSpoofAppTitle
	}
	spoofCategories := strings.TrimSpace(openRouterCfg.AppSpoof.Categories)
	if spoofCategories == "" {
		spoofCategories = openrouter.DefaultSpoofAppCategories
	}
	spoofReferer := strings.TrimSpace(openRouterCfg.AppSpoof.Referer)
	if spoofReferer == "" {
		spoofReferer = openrouter.DefaultSpoofAppReferer
	}
	appSpoofed := false

	var (
		lastStatus         int
		lastBody           []byte
		providerIdx        = 0
		consec429          int
		tried              = make(map[string]bool)
		attempts           int
		ccrHydrations      int
		totalCCRRetrievals int
		streamStarted      bool
		baseBlockIndex     int
		totalInput         int
		totalOutput        int
		totalCacheRead     int
	)

	bw := bufio.NewWriterSize(writer, 4096)
	flusher, hasFlusher := writer.(http.Flusher)

	// No ranked/pinned/custom provider available — single unpinned attempt
	// (equivalent to the pre-routing passthrough behavior).
	if len(candidates) == 0 {
		candidates = []string{""}
	}

	// Parse the request body once; provider injection only re-marshals with
	// the routing key set. MB-scale request bodies make per-attempt parsing
	// wasteful, and failover walks several attempts per request.
	var payload map[string]any
	bodyParsed := json.Unmarshal(reqBody, &payload) == nil

	for {
		if server.now().After(deadline) {
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

		// Build body with provider injection (raw passthrough when the body
		// is unpinned or unparseable).
		body := reqBody
		if bodyParsed && provider != "" {
			payload["provider"] = map[string]any{
				"order":           []string{provider},
				"allow_fallbacks": false,
			}
			if out, err := json.Marshal(payload); err == nil {
				body = out
			}
		}
		attemptStart := server.now()

		// Derive per-attempt context. The budget bounds time-to-first-byte
		// for streams and the whole body for unary responses; an active
		// stream is exempt once headers arrive, so long generations are never
		// truncated mid-flight (see TestOpenRouterRouting_BudgetExemptsActiveStream).
		remaining := time.Until(deadline)
		if remaining <= 0 {
			remaining = 1 * time.Millisecond
		}
		attemptCtx, cancel := context.WithCancel(request.Context())
		headersCutoff := time.AfterFunc(remaining, cancel)
		upReq, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, targetURL.String(), bytes.NewReader(body))
		if err != nil {
			headersCutoff.Stop()
			cancel()
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
		for _, h := range []string{
			openrouter.SpoofAppRefererHeader,
			openrouter.SpoofAppRefererLegacyHeader,
			openrouter.SpoofAppTitleHeader,
			openrouter.SpoofAppTitleLegacyHeader,
			openrouter.SpoofAppCategoriesHeader,
		} {
			if v := request.Header.Get(h); v != "" {
				upReq.Header.Set(h, v)
			}
		}
		if appSpoofed {
			openrouter.ApplySpoofHeaders(upReq, spoofTitle, spoofCategories, spoofReferer)
		}
		cacheCfg := openrouter.ResolveResponseCacheConfig(openRouterCfg.ResponseCache, perModel.ResponseCache)
		openrouter.ApplyResponseCacheHeaders(upReq, request.Header, cacheCfg)

		attempts++
		resp, err := httpClient.Do(upReq)
		if err != nil {
			headersCutoff.Stop()
			cancel()
			if provider != "" {
				openrouter.DefaultRouter.RecordResult(model, provider, false, server.now().Sub(attemptStart), 0)
			}
			_, backoff := classify(0, err)
			// Skip the backoff when the budget is already spent — the loop-top
			// deadline check will break anyway, and sleeping only delays the
			// client's error response.
			if backoff > 0 && server.now().Before(deadline) && !sleepOrDone(request.Context(), backoff) {
				return
			}
			providerIdx++
			lastStatus = 0
			continue
		}

		// 2xx — handle success
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			cacheInfo := openrouter.ExtractResponseCacheHeaders(resp.Header)
			isStream := strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")
			if isStream {
				headersCutoff.Stop()
				// The cutoff can fire in the window between header arrival and
				// Stop(): the stream then holds a dead context and dies on the
				// first read. Treat it like a failed attempt and fail over.
				if attemptCtx.Err() != nil {
					_ = resp.Body.Close()
					cancel()
					if provider != "" {
						openrouter.DefaultRouter.RecordResult(model, provider, false, server.now().Sub(attemptStart), 0)
					}
					providerIdx++
					continue
				}

				if !server.isCCREnabled() {
					server.proxyStreamResponse(writer, resp, model, sessionID, pricing, startTime, attemptStart, provider, cancel)
					return
				}

				// Stream with CCR interception and potential re-entry.
				// ccrStreamState owns the upstream-to-downstream index mapping
				// and the headroom_retrieve suppression, shared with the
				// CloudCode and Kimi paths.
				state := newCCRStreamState(baseBlockIndex)
				var pendingTerminalEvents []map[string]any
				var attemptIn, attemptOut, attemptCr, attemptCw int

				parseErr := parseSSEStream(resp.Body, func(eventType string, dataObj map[string]any, rawData []byte) error {
					openrouter.ParseUsageFromSSELine(string(rawData), &attemptIn, &attemptOut, &attemptCr, &attemptCw)
					if p := openrouter.ExtractProviderFromSSELine(string(rawData)); p != "" {
						provider = canonicalServedProvider(model, p)
					}

					switch eventType {
					case "message_start":
						if ccrHydrations == 0 {
							if !streamStarted {
								copyUpstreamHeaders(writer.Header(), resp.Header)
								writer.WriteHeader(resp.StatusCode)
								streamStarted = true
							}
							return writeSSEEvent(bw, eventType, dataObj, rawData, hasFlusher, flusher)
						}
						return nil

					case "content_block_start":
						idx := intValue(dataObj["index"], 0)
						downstream, emit := state.StartBlock(idx, mapOrEmpty(dataObj["content_block"]))
						if !emit {
							return nil
						}
						if !streamStarted {
							copyUpstreamHeaders(writer.Header(), resp.Header)
							writer.WriteHeader(resp.StatusCode)
							streamStarted = true
						}
						dataObj["index"] = downstream
						// rawData still carries the upstream index; re-marshal.
						return writeSSEEvent(bw, eventType, dataObj, nil, hasFlusher, flusher)

					case "content_block_delta":
						idx := intValue(dataObj["index"], 0)
						delta := mapOrEmpty(dataObj["delta"])
						switch deltaType, _ := delta["type"].(string); deltaType {
						case "input_json_delta":
							partial, _ := delta["partial_json"].(string)
							state.AppendJSON(idx, partial)
						case "text_delta":
							text, _ := delta["text"].(string)
							state.AppendText(idx, text)
						}
						downstream, emit := state.MapIndex(idx)
						if !emit {
							return nil
						}
						dataObj["index"] = downstream
						return writeSSEEvent(bw, eventType, dataObj, nil, hasFlusher, flusher)

					case "content_block_stop":
						idx := intValue(dataObj["index"], 0)
						downstream, emit := state.MapIndex(idx)
						if !emit {
							return nil
						}
						dataObj["index"] = downstream
						return writeSSEEvent(bw, eventType, dataObj, nil, hasFlusher, flusher)

					case "message_delta", "message_stop":
						pendingTerminalEvents = append(pendingTerminalEvents, dataObj)
						return nil

					default:
						if !streamStarted {
							copyUpstreamHeaders(writer.Header(), resp.Header)
							writer.WriteHeader(resp.StatusCode)
							streamStarted = true
						}
						return writeSSEEvent(bw, eventType, dataObj, rawData, hasFlusher, flusher)
					}
				})
				_ = resp.Body.Close()
				cancel()

				if parseErr != nil {
					if !streamStarted {
						if provider != "" {
							openrouter.DefaultRouter.RecordResult(model, provider, false, server.now().Sub(attemptStart), 0)
						}
						providerIdx++
						continue
					}
					return
				}

				totalInput += attemptIn
				totalOutput += attemptOut
				totalCacheRead += attemptCr

				// Check for headroom_retrieve calls
				retrieveCalls := state.Finalize()

				if len(retrieveCalls) > 0 && ccrHydrations < maxCCRHydrations {
					ccrHydrations++
					totalCCRRetrievals += len(retrieveCalls)
					// Suppressed blocks consumed no downstream index, so
					// advancing by VisibleCount keeps the sequence gapless.
					baseBlockIndex += state.VisibleCount()

					assistantMsg := map[string]any{
						"role":    "assistant",
						"content": state.AssistantBlocks(),
					}
					var toolResults []any
					for _, call := range retrieveCalls {
						toolID, _ := call["id"].(string)
						inputMap, _ := call["input"].(map[string]any)
						chunkID, _ := inputMap["chunk_id"].(string)
						payload, isErr := server.getCCRChunkPayload(chunkID)
						toolResults = append(toolResults, map[string]any{
							"type":        "tool_result",
							"tool_use_id": toolID,
							"content":     payload,
							"is_error":    isErr,
						})
					}
					userMsg := map[string]any{
						"role":    "user",
						"content": toolResults,
					}
					existingMsgs, _ := anthropicRequest["messages"].([]any)
					anthropicRequest["messages"] = append(existingMsgs, assistantMsg, userMsg)
					reqBody, _ = json.Marshal(anthropicRequest)
					bodyParsed = json.Unmarshal(reqBody, &payload) == nil
					tried[provider] = false
					continue
				}

				// Terminal events flush
				for _, ev := range pendingTerminalEvents {
					if ev["type"] == "message_delta" {
						reconcileStopReasonEvent(ev, state.HasVisibleToolUse())
						usage, ok := ev["usage"].(map[string]any)
						if !ok || usage == nil {
							usage = make(map[string]any)
							ev["usage"] = usage
						}
						usage["output_tokens"] = totalOutput
						usage["cache_read_input_tokens"] = totalCacheRead
					}
					_ = writeSSEEvent(bw, stringFrom(ev["type"]), ev, nil, hasFlusher, flusher)
				}

				attemptPricing := effectiveAttemptPricing(pricing, model, provider)
				if provider != "" {
					openrouter.DefaultRouter.RecordResult(model, provider, true, server.now().Sub(attemptStart), totalInput+totalOutput)
					openrouter.DefaultRouter.SetSticky(sessionID, model, provider)
				}
				server.recordOpenRouterMetrics(model, sessionID, attemptPricing, startTime, totalInput, totalOutput, totalCacheRead, attemptCw, provider, cacheInfo)
				if totalCCRRetrievals > 0 && server.tracker != nil {
					server.tracker.RecordHeadroom(stats.HeadroomSample{CCRRetrievals: totalCCRRetrievals})
				}
				return
			}
			// Buffer full body before writing — failover impossible after first byte.
			bodyBytes, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			headersCutoff.Stop()
			cancel()
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
				provider = canonicalServedProvider(model, servedProvider)
			}
			// Cost follows the served endpoint, resolved after the override.
			attemptPricing := effectiveAttemptPricing(pricing, model, provider)

			// CCR Hydration for OpenRouter Unary
			if server.isCCREnabled() && ccrHydrations < maxCCRHydrations {
				var respObj map[string]any
				if json.Unmarshal(bodyBytes, &respObj) == nil {
					retrieveCalls := findRetrieveToolUsesFromResponse(respObj)
					if len(retrieveCalls) > 0 {
						ccrHydrations++
						totalCCRRetrievals += len(retrieveCalls)
						assistantMsg := map[string]any{
							"role":    "assistant",
							"content": respObj["content"],
						}
						var toolResults []any
						for _, call := range retrieveCalls {
							toolID, _ := call["id"].(string)
							inputMap, _ := call["input"].(map[string]any)
							chunkID, _ := inputMap["chunk_id"].(string)
							payload, isErr := server.getCCRChunkPayload(chunkID)
							toolResults = append(toolResults, map[string]any{
								"type":        "tool_result",
								"tool_use_id": toolID,
								"content":     payload,
								"is_error":    isErr,
							})
						}
						userMsg := map[string]any{
							"role":    "user",
							"content": toolResults,
						}
						existingMsgs, _ := anthropicRequest["messages"].([]any)
						anthropicRequest["messages"] = append(existingMsgs, assistantMsg, userMsg)
						reqBody, _ = json.Marshal(anthropicRequest)
						bodyParsed = json.Unmarshal(reqBody, &payload) == nil
						tried[provider] = false
						continue
					}
				}
			}

			if server.isCCREnabled() {
				bodyBytes = stripRetrieveBlocksJSON(bodyBytes)
			}

			// Write headers + status
			copyUpstreamHeaders(writer.Header(), resp.Header)
			writer.WriteHeader(resp.StatusCode)
			_, _ = writer.Write(bodyBytes)

			// Observability + record result
			in, out, cr, cw := openrouter.ParseUsageFromJSON(bodyBytes)
			if provider != "" {
				openrouter.DefaultRouter.RecordResult(model, provider, true, server.now().Sub(attemptStart), in+out)
				// Move stickiness to the provider that actually served: after a
				// failover, later requests must not retry the dead provider first.
				openrouter.DefaultRouter.SetSticky(sessionID, model, provider)
			}
			server.recordOpenRouterMetrics(model, sessionID, attemptPricing, startTime, in, out, cr, cw, provider, cacheInfo)
			if totalCCRRetrievals > 0 && server.tracker != nil {
				server.tracker.RecordHeadroom(stats.HeadroomSample{CCRRetrievals: totalCCRRetrievals})
			}
			return
		}

		// Non-2xx: buffer body, classify, decide next.
		bodyBytes, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		headersCutoff.Stop()
		cancel()
		lastStatus = resp.StatusCode
		lastBody = bodyBytes

		// Harness-gated model: attribution-level rejection, not a provider
		// failure. Retry once with spoofed app headers; if the gate persists,
		// further providers fail identically — surface the upstream error.
		if resp.StatusCode == http.StatusForbidden && openrouter.IsHarnessGateError(bodyBytes) {
			if !appSpoofed {
				appSpoofed = true
				tried[provider] = false
				server.logger.Info("OpenRouter harness gate intercepted; retrying with spoofed attribution headers",
					"model", model, "provider", provider)
				continue
			}
			break
		}

		action, backoff := classify(resp.StatusCode, nil)
		// 429 is a transient rate limit, not provider death: recording it as a
		// failure would let a rate-limit storm trip the breaker on a healthy
		// provider. All other non-2xx responses count toward the threshold.
		if provider != "" && resp.StatusCode != http.StatusTooManyRequests {
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
			if server.now().Add(d).After(deadline) {
				break
			}
			if !sleepOrDone(request.Context(), d) {
				return
			}
			// Don't advance providerIdx; re-enter the loop with same provider.
			tried[provider] = false
			continue
		case nextNextProvider:
			consec429 = 0
			if backoff > 0 && !sleepOrDone(request.Context(), backoff) {
				return
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
		"model", model, "status", status, "attempts", attempts, "tried", len(tried))
	writeAPIError(writer, status, "api_error", fmt.Sprintf("OpenRouter upstream failed after %d attempt(s): %s", attempts, truncate(string(lastBody), 256)))
}

// openRouterUpstreamClient returns the HTTP client for OpenRouter upstream
// calls. It intentionally has no total Timeout: a total timeout covers the
// full body read and would kill long-running SSE streams mid-generation.
// Cancellation comes from the inbound request context and the retry budget.
func openRouterUpstreamClient() *http.Client {
	return openRouterSharedClient
}

// openRouterSharedClient is the package-level transport for upstream calls;
// http.Client is safe for concurrent use and pools connections internally.
// The transport is tuned for high concurrency against a single upstream host:
// http.DefaultTransport caps idle connections per host at 2, which churns
// connections under parallel streaming load.
var openRouterSharedClient = &http.Client{
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	},
}

// hopByHopHeaders are connection-scoped and must not be forwarded from an
// upstream response to the proxy client.
var hopByHopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive",
	"Proxy-Authenticate", "Proxy-Authorization",
	"Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// copyUpstreamHeaders copies src into dst, skipping Content-Length and
// hop-by-hop headers (including any tokens named in a Connection header).
func copyUpstreamHeaders(dst, src http.Header) {
	drop := append([]string{"Content-Length"}, hopByHopHeaders...)
	for _, tok := range strings.Split(src.Get("Connection"), ",") {
		if tok = strings.TrimSpace(tok); tok != "" {
			drop = append(drop, tok)
		}
	}
	for k, vs := range src {
		skip := false
		for _, d := range drop {
			if strings.EqualFold(k, d) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// sleepOrDone sleeps for d or until ctx is cancelled. Returns false when the
// context finished first (client disconnect), true after a full sleep.
func sleepOrDone(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
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

// canonicalServedProvider maps a served-provider label (SSE/JSON "provider"
// field) onto the canonical provider_name from the rank list, matching
// case-insensitively. Unknown labels pass through unchanged.
func canonicalServedProvider(model, served string) string {
	if served == "" {
		return ""
	}
	for _, r := range openrouter.DefaultRouter.GetRanks(model) {
		if strings.EqualFold(r.Provider, served) {
			return r.Provider
		}
	}
	return served
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
			d = cap
			break
		}
	}
	if d > cap {
		d = cap
	}
	// ±25% jitter so concurrent clients do not retry a throttled provider in
	// lockstep. Stays within [0.75d, 1.25d]; never negative.
	d += time.Duration(rand.Int63n(int64(d)/2 + 1)) - d/4
	if d > cap {
		d = cap
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
// capturing usage and the served provider via SSE. It owns cancel: the attempt
// context lives until the stream ends (headers already arrived within budget).
func (server *Server) proxyStreamResponse(writer http.ResponseWriter, resp *http.Response, model, sessionID string, pricing openrouter.Pricing, startTime, attemptStart time.Time, provider string, cancel context.CancelFunc) {
	defer cancel()
	cacheInfo := openrouter.ExtractResponseCacheHeaders(resp.Header)
	copyUpstreamHeaders(writer.Header(), resp.Header)
	writer.WriteHeader(resp.StatusCode)
	flusher, hasFlusher := writer.(http.Flusher)

	var interceptor *openrouter.SSEInterceptor
	interceptor = openrouter.NewSSEInterceptor(resp.Body, func(in, out, cr, cw int) {
		// Prefer the provider reported by the stream over the requested one.
		served := provider
		if p := interceptor.Provider(); p != "" {
			served = canonicalServedProvider(model, p)
		}
		if served != "" {
			success := interceptor.TerminalErr() == nil
			openrouter.DefaultRouter.RecordResult(model, served, success, server.now().Sub(attemptStart), in+out)
			if success {
				// Move stickiness to the provider that actually served.
				openrouter.DefaultRouter.SetSticky(sessionID, model, served)
			}
		}
		// Cost follows the served endpoint (pricing is the model-level base here).
		server.recordOpenRouterMetrics(model, sessionID, effectiveAttemptPricing(pricing, model, served), startTime, in, out, cr, cw, served, cacheInfo)
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

func parseSSEStream(reader io.Reader, handleEvent func(eventType string, dataObj map[string]any, rawData []byte) error) error {
	br := bufio.NewReaderSize(reader, 64*1024)
	var currentEvent string
	var currentData bytes.Buffer

	dispatch := func() error {
		if currentData.Len() == 0 && currentEvent == "" {
			return nil
		}
		raw := currentData.Bytes()
		var dataObj map[string]any
		_ = json.Unmarshal(raw, &dataObj)
		evType := currentEvent
		if evType == "" && dataObj != nil {
			if t, ok := dataObj["type"].(string); ok {
				evType = t
			}
		}
		err := handleEvent(evType, dataObj, raw)
		currentEvent = ""
		currentData.Reset()
		return err
	}

	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			lineStr := strings.TrimRight(string(line), "\r\n")
			if lineStr == "" {
				if err := dispatch(); err != nil {
					return err
				}
			} else if strings.HasPrefix(lineStr, "event: ") {
				currentEvent = strings.TrimPrefix(lineStr, "event: ")
			} else if strings.HasPrefix(lineStr, "data: ") {
				if currentData.Len() > 0 {
					currentData.WriteByte('\n')
				}
				currentData.WriteString(strings.TrimPrefix(lineStr, "data: "))
			}
		}
		if err != nil {
			if err == io.EOF {
				return dispatch()
			}
			return err
		}
	}
}

func writeSSEEvent(bw *bufio.Writer, eventType string, dataObj map[string]any, rawData []byte, hasFlusher bool, flusher http.Flusher) error {
	var payload []byte
	if dataObj != nil {
		var err error
		payload, err = json.Marshal(dataObj)
		if err != nil {
			payload = rawData
		}
	} else {
		payload = rawData
	}
	if eventType != "" {
		if _, err := bw.WriteString("event: " + eventType + "\n"); err != nil {
			return err
		}
	}
	if _, err := bw.WriteString("data: "); err != nil {
		return err
	}
	if _, err := bw.Write(payload); err != nil {
		return err
	}
	if _, err := bw.WriteString("\n\n"); err != nil {
		return err
	}
	if err := bw.Flush(); err != nil {
		return err
	}
	if hasFlusher && flusher != nil {
		flusher.Flush()
	}
	return nil
}

// recordOpenRouterMetrics is shared between stream and unary paths. Pricing is
// resolved here so both paths apply the model-catalog fallback uniformly.
func (server *Server) recordOpenRouterMetrics(model, sessionID string, pricing openrouter.Pricing, startTime time.Time, in, out, cr, cw int, provider string, cacheInfo openrouter.ResponseCacheInfo) openrouter.RequestMetrics {
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
		CacheStatus:         cacheInfo.Status,
		CacheAge:            cacheInfo.Age,
		CacheTTL:            cacheInfo.TTL,
		CacheSourceID:       cacheInfo.SourceID,
	}
	metrics.ComputeFinalMetrics(resolveEffectivePricing(pricing, model), openrouter.DefaultSessionTracker)
	openrouter.LogObservability(server.logger, metrics)
	if server.tracker != nil {
		server.tracker.TrackRequest(model, latency, in, out, cr)
	}
	return metrics
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

// effectiveAttemptPricing prefers the served provider's endpoint pricing over
// the requested provider's or model-catalog price. OpenRouter may serve a
// different endpoint than ordered, so cost must follow what actually served.
func effectiveAttemptPricing(base openrouter.Pricing, model, servedProvider string) openrouter.Pricing {
	if servedProvider != "" {
		if ep := endpointPricing(model, servedProvider); ep != nil {
			return *ep
		}
	}
	return base
}

type streamSender func(context.Context, map[string]any, func(cloudcode.SSEEvent) error) (cloudcode.Response, error)

const maxCCRHydrations = 3

func (server *Server) isCCREnabled() bool {
	if server.headroom == nil {
		return false
	}
	cfg := server.headroom.GetConfig()
	return cfg.Enabled && cfg.CCR.Enabled && server.headroom.CCRStore() != nil
}

func (server *Server) getCCRChunkPayload(chunkID string) (string, bool) {
	if server.headroom == nil || server.headroom.CCRStore() == nil {
		return fmt.Sprintf("Error: CCR store unavailable (chunk %s)", chunkID), true
	}
	payload, found := server.headroom.CCRStore().Get(chunkID)
	if !found {
		return fmt.Sprintf("Error: Chunk %s not found or evicted from CCR store", chunkID), true
	}
	return payload, false
}

func findRetrieveToolUsesFromResponse(resp map[string]any) []map[string]any {
	content, ok := resp["content"].([]any)
	if !ok {
		return nil
	}
	var calls []map[string]any
	for _, raw := range content {
		block, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if block["type"] == "tool_use" && block["name"] == "headroom_retrieve" {
			calls = append(calls, block)
		}
	}
	return calls
}

func intValue(v any, defaultVal int) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case float32:
		return int(n)
	}
	return defaultVal
}

func mapOrEmpty(v any) map[string]any {
	if m, ok := v.(map[string]any); ok && m != nil {
		return m
	}
	return make(map[string]any)
}

func (server *Server) unaryMessage(writer http.ResponseWriter, request *http.Request, send streamSender, anthropicRequest map[string]any, model string) {
	startTime := server.now()
	totalCCRRetrievals := 0
	var totalInput, totalOutput, totalCacheRead int

	for iter := 0; iter <= maxCCRHydrations; iter++ {
		accumulator := proxyformat.NewThinkingAccumulator()
		_, err := send(request.Context(), anthropicRequest, func(event cloudcode.SSEEvent) error {
			return accumulator.Consume(event.Data)
		})
		if err != nil {
			server.writeError(writer, err)
			return
		}

		totalInput += accumulator.InputTokens()
		totalOutput += accumulator.OutputTokens()
		totalCacheRead += accumulator.CacheReadTokens()

		response := accumulator.Response(model, server.builder.Cache, "")
		retrieveCalls := findRetrieveToolUsesFromResponse(response)

		if len(retrieveCalls) == 0 || iter == maxCCRHydrations || !server.isCCREnabled() {
			stripRetrieveBlocks(response)
			if usage, ok := response["usage"].(map[string]any); ok {
				usage["input_tokens"] = totalInput
				usage["output_tokens"] = totalOutput
				usage["cache_read_input_tokens"] = totalCacheRead
			}
			latency := server.now().Sub(startTime)
			if server.tracker != nil {
				server.tracker.TrackRequest(model, latency, totalInput, totalOutput, totalCacheRead)
				if totalCCRRetrievals > 0 {
					server.tracker.RecordHeadroom(stats.HeadroomSample{CCRRetrievals: totalCCRRetrievals})
				}
			}
			writeJSON(writer, http.StatusOK, response)
			return
		}

		totalCCRRetrievals += len(retrieveCalls)
		assistantMsg := map[string]any{
			"role":    "assistant",
			"content": response["content"],
		}
		var toolResults []any
		for _, call := range retrieveCalls {
			toolID, _ := call["id"].(string)
			inputMap, _ := call["input"].(map[string]any)
			chunkID, _ := inputMap["chunk_id"].(string)
			payload, isErr := server.getCCRChunkPayload(chunkID)
			toolResults = append(toolResults, map[string]any{
				"type":        "tool_result",
				"tool_use_id": toolID,
				"content":     payload,
				"is_error":    isErr,
			})
		}
		userMsg := map[string]any{
			"role":    "user",
			"content": toolResults,
		}
		existingMsgs, _ := anthropicRequest["messages"].([]any)
		anthropicRequest["messages"] = append(existingMsgs, assistantMsg, userMsg)
	}
}

func (server *Server) streamMessage(writer http.ResponseWriter, request *http.Request, send streamSender, anthropicRequest map[string]any, model string) {
	startTime := server.now()
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

	baseBlockIndex := 0
	totalCCRRetrievals := 0
	var totalInput, totalOutput, totalCacheRead int

	for iter := 0; iter <= maxCCRHydrations; iter++ {
		converter := proxyformat.NewStreamConverter(model, server.builder.Cache, "")
		state := newCCRStreamState(baseBlockIndex)
		var pendingTerminalEvents []map[string]any

		handleEvent := func(event map[string]any) error {
			eventType, _ := event["type"].(string)
			switch eventType {
			case "message_start":
				if iter == 0 {
					return writeEvents([]map[string]any{event})
				}
				return nil

			case "content_block_start":
				idx := intValue(event["index"], 0)
				downstream, emit := state.StartBlock(idx, mapOrEmpty(event["content_block"]))
				if !emit {
					return nil
				}
				event["index"] = downstream
				return writeEvents([]map[string]any{event})

			case "content_block_delta":
				idx := intValue(event["index"], 0)
				delta := mapOrEmpty(event["delta"])
				switch deltaType, _ := delta["type"].(string); deltaType {
				case "input_json_delta":
					partial, _ := delta["partial_json"].(string)
					state.AppendJSON(idx, partial)
				case "text_delta":
					text, _ := delta["text"].(string)
					state.AppendText(idx, text)
				}
				downstream, emit := state.MapIndex(idx)
				if !emit {
					return nil
				}
				event["index"] = downstream
				return writeEvents([]map[string]any{event})

			case "content_block_stop":
				idx := intValue(event["index"], 0)
				downstream, emit := state.MapIndex(idx)
				if !emit {
					return nil
				}
				event["index"] = downstream
				return writeEvents([]map[string]any{event})

			case "message_delta", "message_stop":
				pendingTerminalEvents = append(pendingTerminalEvents, event)
				return nil

			default:
				return writeEvents([]map[string]any{event})
			}
		}

		_, err := send(request.Context(), anthropicRequest, func(event cloudcode.SSEEvent) error {
			events, err := converter.Consume(event.Data)
			if err != nil {
				return err
			}
			for _, ev := range events {
				if err := handleEvent(ev); err != nil {
					return err
				}
			}
			return nil
		})
		if err == nil {
			finishEvents, fErr := converter.Finish()
			if fErr == nil {
				for _, ev := range finishEvents {
					if err := handleEvent(ev); err != nil {
						break
					}
				}
			}
		}
		if err != nil {
			if !started {
				server.writeError(writer, err)
				return
			}
			errorEvent := map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": err.Error()}}
			_ = writeEvents([]map[string]any{errorEvent})
			return
		}

		totalInput += converter.InputTokens()
		totalOutput += converter.OutputTokens()
		totalCacheRead += converter.CacheReadTokens()

		retrieveCalls := state.Finalize()

		needsHydration := len(retrieveCalls) > 0 && iter < maxCCRHydrations && server.isCCREnabled()

		if !needsHydration {
			for _, ev := range pendingTerminalEvents {
				if ev["type"] == "message_delta" {
					reconcileStopReasonEvent(ev, state.HasVisibleToolUse())
					usage, ok := ev["usage"].(map[string]any)
					if !ok || usage == nil {
						usage = make(map[string]any)
						ev["usage"] = usage
					}
					usage["output_tokens"] = totalOutput
					usage["cache_read_input_tokens"] = totalCacheRead
				}
			}
			_ = writeEvents(pendingTerminalEvents)

			if server.tracker != nil {
				latency := server.now().Sub(startTime)
				server.tracker.TrackRequest(model, latency, totalInput, totalOutput, totalCacheRead)
				if totalCCRRetrievals > 0 {
					server.tracker.RecordHeadroom(stats.HeadroomSample{CCRRetrievals: totalCCRRetrievals})
				}
			}
			return
		}

		totalCCRRetrievals += len(retrieveCalls)
		baseBlockIndex += state.VisibleCount()

		assistantMsg := map[string]any{
			"role":    "assistant",
			"content": state.AssistantBlocks(),
		}

		var toolResults []any
		for _, call := range retrieveCalls {
			toolID, _ := call["id"].(string)
			inputMap, _ := call["input"].(map[string]any)
			chunkID, _ := inputMap["chunk_id"].(string)
			payload, isErr := server.getCCRChunkPayload(chunkID)
			toolResults = append(toolResults, map[string]any{
				"type":        "tool_result",
				"tool_use_id": toolID,
				"content":     payload,
				"is_error":    isErr,
			})
		}
		userMsg := map[string]any{
			"role":    "user",
			"content": toolResults,
		}
		existingMsgs, _ := anthropicRequest["messages"].([]any)
		anthropicRequest["messages"] = append(existingMsgs, assistantMsg, userMsg)
	}
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
