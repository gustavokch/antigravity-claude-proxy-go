package api

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"math/rand"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"antigravity-go-proxy/internal/accounts"
	"antigravity-go-proxy/internal/auth"
	"antigravity-go-proxy/internal/cachebump"
	"antigravity-go-proxy/internal/ccidentity"
	"antigravity-go-proxy/internal/classifier"
	"antigravity-go-proxy/internal/claudecode"
	"antigravity-go-proxy/internal/cloudcode"
	"antigravity-go-proxy/internal/config"
	proxyformat "antigravity-go-proxy/internal/format"
	"antigravity-go-proxy/internal/headroom"
	"antigravity-go-proxy/internal/headroom/stages/ccr"
	"antigravity-go-proxy/internal/headroom/stages/code"
	"antigravity-go-proxy/internal/headroom/stages/crusher"
	"antigravity-go-proxy/internal/headroom/stages/shaper"
	"antigravity-go-proxy/internal/headroom/stages/smart"
	"antigravity-go-proxy/internal/kimi"
	"antigravity-go-proxy/internal/logger"
	"antigravity-go-proxy/internal/modelcatalog"
	"antigravity-go-proxy/internal/openrouter"
	"antigravity-go-proxy/internal/stats"
	"antigravity-go-proxy/internal/zen"
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
	APIKey             string
	ProjectID          string
	Credentials        func(context.Context) (auth.Credentials, error)
	NewUpstream        func(string) Upstream
	Backend            Backend
	Builder            *proxyformat.Builder
	Now                func() time.Time
	Logger             *slog.Logger
	AccountManager     *accounts.Manager
	Broadcaster        *logger.Broadcaster
	WebUI              http.Handler
	OAuthHandler       http.Handler
	Tracker            *stats.Tracker
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
	ccrStore           *ccr.CCRStore
	cacheBumpStore     *cachebump.Store
	cacheBumpSched     *cachebump.Scheduler
	classifierMatcher  *classifier.ConfigurableMatcher
	classifierAudit    *classifier.Recorder

	mu                sync.Mutex
	cachedCredentials auth.Credentials
	upstreamToken     string
	upstream          Upstream
	projects          map[string]string

	// appSpoofActivated flips to true the first time any request crosses an
	// OpenRouter harness gate. It is process-lifetime by design: the gate is
	// uniform (attribution or no attribution), so a single intercept proves
	// the proxy must carry spoofed app headers for the rest of the run.
	appSpoofActivated bool

	// coldCatalogAttemptAt records the last cold-start catalog fetch attempt
	// on a status poll; guarded by catalogMu. It bounds how often a
	// catalog-less server pays a blocking fetch when upstream never recovers.
	coldCatalogAttemptAt time.Time
	catalogMu            sync.Mutex
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
	srv.classifierAudit = classifier.NewRecorder(200)
	srv.applyClassifierConfig(cfg.Classifier)
	srv.ccrStore = ccr.NewCCRStoreFromMB(cfg.Headroom.CCR.MaxStoreMB)
	srv.headroom = headroom.NewEngine(
		cfg.Headroom,
		srv.logger,
		ccr.NewStage(srv.ccrStore),
		crusher.NewStage(),
		smart.NewStage(),
		code.NewStage(),
		shaper.NewStage(),
	)
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
	if openRouterCfg.Routing.MinRequestIntervalMs > 0 {
		openrouter.DefaultRateLimiter.SetMinRequestInterval(time.Duration(openRouterCfg.Routing.MinRequestIntervalMs) * time.Millisecond)
	} else {
		openrouter.DefaultRateLimiter.SetMinRequestInterval(0)
	}
}

func (server *Server) applyHeadroomConfig(cfg config.HeadroomConfig) {
	if server.headroom != nil {
		server.headroom.UpdateConfig(cfg)
	}
	if server.ccrStore != nil {
		server.ccrStore.SetMaxMB(cfg.CCR.MaxStoreMB)
	}
}

// tickClaudeCodeBackgroundWorker checks for expiring OAuth tokens and proactively refreshes them.
func (server *Server) tickClaudeCodeBackgroundWorker() {
	cfg := config.Get()
	if !cfg.ClaudeCode.Enabled {
		return
	}
	pool, _ := server.getOrCreateCCPool(cfg.ClaudeCode)
	if pool == nil {
		return
	}

	// Proactively refresh tokens expiring in <= 15 minutes
	refreshed, err := pool.RefreshAllExpiringTokens(15 * time.Minute)
	if err != nil {
		if server.logger != nil {
			server.logger.Warn("background token refresh failed", "error", err)
		}
		return
	}
	if len(refreshed) > 0 {
		for _, id := range refreshed {
			if refreshedAcc, ok := pool.GetAccount(id); ok {
				server.syncRefreshedAccountToConfig(id, refreshedAcc.Token, refreshedAcc.RefreshToken, refreshedAcc.ExpiresAt)
			}
		}
		if server.logger != nil {
			server.logger.Info("background refreshed Claude Code tokens", "count", len(refreshed), "accounts", refreshed)
		}
	}
}

// StartClaudeCodeBackgroundWorker starts a background loop to refresh expiring Claude Code OAuth tokens.
func (server *Server) StartClaudeCodeBackgroundWorker(ctx context.Context) {
	go func() {
		// Run an initial refresh check immediately upon startup
		server.tickClaudeCodeBackgroundWorker()

		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				server.tickClaudeCodeBackgroundWorker()
			}
		}
	}()
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
		case path == "/v1/chat/completions" && request.Method == http.MethodPost:
			server.chatCompletions(writer, request)
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

// defaultDiscoveryContextWindow is the context window /v1/models advertises for
// an allowlist entry when neither the operator's config nor the live provider
// catalog states one.
const defaultDiscoveryContextWindow = 200000

// defaultDiscoveryMaxOutputTokens caps the max_output_tokens that /v1/models
// advertises when only the context window is known. A model's output cap is
// always far below its context window, so reporting the context window as the
// output cap invites clients to send a max_tokens the provider rejects.
const defaultDiscoveryMaxOutputTokens = 200000

func (server *Server) models(writer http.ResponseWriter, request *http.Request) {
	catalog, err := server.fetchModelCatalog(request.Context())
	if err != nil {
		server.writeError(writer, err)
		return
	}
	selectable := catalog.PublicModels()
	models := make([]any, 0, len(selectable))
	seen := make(map[string]bool)
	cfg := config.Get()
	owned := gatewayOwnedModelIDs(cfg)

	for _, details := range selectable {
		// A colliding ID is advertised by the gateway that wins it, never
		// by the catalog: the dispatcher routes the ID to the gateway, so
		// a catalog entry here would advertise an owner that disagrees
		// with the dispatcher.
		if owned[details.ID] {
			continue
		}
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
			"display_name":   details.DisplayName,
			"context_window": details.MaxTokens, "max_output_tokens": details.MaxOutputTokens,
			"supports_thinking": details.SupportsThinking,
		})
		seen[details.ID] = true
	}

	// Each gateway appends its models in configured precedence order. Entry
	// order is cosmetic — the catalog stays first in the array — but
	// ownership is functional: the shared seen guard gives the win to the
	// first gateway in order, the same gateway the dispatcher picks.
	for _, id := range cfg.GatewayOrder.Effective("") {
		if appender, ok := gatewayModelAppenders[id]; ok {
			appender(server, cfg, &models, seen)
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

// cachedCatalogBackend is implemented by backends that retain the last
// fetched model catalog. Read-only status endpoints prefer it over a
// blocking refresh so a poll can never stall on upstream I/O.
type cachedCatalogBackend interface {
	CachedCatalog() *modelcatalog.Catalog
	RefreshCatalogIfStale() bool
}

// cachedModelCatalog returns the backend's last fetched catalog without any
// upstream I/O. It returns nil when the backend retains none (or is not a
// retaining backend); callers fall back to fetchModelCatalog then.
func (server *Server) cachedModelCatalog() *modelcatalog.Catalog {
	if backend, ok := server.backend.(cachedCatalogBackend); ok {
		return backend.CachedCatalog()
	}
	return nil
}

// refreshModelCatalogIfStale asks a retaining backend to refresh a stale
// catalog in the background. It never blocks and is a no-op for backends that
// do not retain a catalog.
func (server *Server) refreshModelCatalogIfStale() {
	if backend, ok := server.backend.(cachedCatalogBackend); ok {
		backend.RefreshCatalogIfStale()
	}
}

// coldCatalogCooldown bounds how often a catalog-less server pays a blocking
// catalog fetch on a status poll. Without it, an upstream that never recovers
// charges every poll the full fetchModelsTimeout.
const coldCatalogCooldown = 60 * time.Second

// allowColdCatalogFetch reports whether a catalog-less status poll may pay one
// blocking catalog fetch, recording the attempt when it does. It is false
// inside coldCatalogCooldown of the last attempt.
func (server *Server) allowColdCatalogFetch() bool {
	server.catalogMu.Lock()
	defer server.catalogMu.Unlock()
	if !server.coldCatalogAttemptAt.IsZero() && server.now().Sub(server.coldCatalogAttemptAt) < coldCatalogCooldown {
		return false
	}
	server.coldCatalogAttemptAt = server.now()
	return true
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
	request = server.consumeCacheBumpHeader(request)
	request.Body = http.MaxBytesReader(writer, request.Body, maxRequestBody)
	// Keep the raw bytes: when nothing rewrites the request, custom-endpoint
	// forwarding passes them through byte-for-byte instead of re-marshaling
	// the decoded map (which reorders keys and reformats numbers).
	rawBody, err := io.ReadAll(request.Body)
	if err != nil {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request_error", "Failed to read request body: "+err.Error())
		return
	}
	var anthropicRequest map[string]any
	if err := json.Unmarshal(rawBody, &anthropicRequest); err != nil {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request_error", "Invalid JSON request body: "+err.Error())
		return
	}
	bodyMutated := false
	messages, ok := anthropicRequest["messages"].([]any)
	if !ok {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request_error", "messages is required and must be an array")
		return
	}
	if model, _ := anthropicRequest["model"].(string); model == "" {
		anthropicRequest["model"] = "gemini-3.5-flash-low"
		bodyMutated = true
	}
	reqModel := stringFrom(anthropicRequest["model"])
	if current := server.resolveModelMapping(reqModel); current != reqModel {
		slog.Info(fmt.Sprintf("[Server] Mapping model %s -> %s", reqModel, current))
		anthropicRequest["model"] = current
		bodyMutated = true
	}
	cfg := config.Get()
	// max_tokens is no longer injected here. It is sent upstream only when
	// the client supplied it or a per-model limit is known; see
	// applyMaxTokensPolicy.
	if len(messages) == 1 && messages[0] != nil {
		if message, ok := messages[0].(map[string]any); ok && message["content"] == "count" {
			writeJSON(writer, http.StatusOK, map[string]any{})
			return
		}
	}

	if server.headroom != nil {
		if hrCtx, err := server.headroom.Process(request.Context(), anthropicRequest); err != nil {
			server.logger.Warn("headroom pipeline failed; forwarding request as decoded", "error", err)
			// The pipeline mutates in place and may have half-applied before
			// failing, so the map can no longer be proven identical to the
			// raw client bytes.
			bodyMutated = true
		} else {
			if hrCtx.BytesBefore > 0 || hrCtx.EffortClamped {
				if server.tracker != nil {
					server.tracker.RecordHeadroom(stats.HeadroomSample{
						BytesBefore:           hrCtx.BytesBefore,
						BytesAfter:            hrCtx.BytesAfter,
						ThinkingTokensClamped: hrCtx.OriginalThinking - hrCtx.ClampedThinking,
					})
				}
			}
			if hrCtx.BytesBefore > 0 || hrCtx.EffortClamped || hrCtx.RewritesCount > 0 || hrCtx.ChunksStored > 0 {
				bodyMutated = true
			}
		}
	}

	model := stringFrom(anthropicRequest["model"])

	var (
		isClassifierFallback           bool
		classifierFallbackKind         classifier.Kind
		classifierFallbackVerdictTmpl  string
		classifierFallbackThinkingTmpl string
	)

	streamRequested, _ := anthropicRequest["stream"].(bool)

	// Operator rules are consulted first. Anything they decline to handle —
	// including a reroute whose backend failed — falls through to the
	// built-in Detect path below, so today's behavior is the default.
	skipClassifierDetect := false
	if cfg.Classifier.Enabled && len(cfg.Classifier.Rules) > 0 && server.classifierMatcher != nil {
		if rule, backend, matched := server.classifierMatcher.Match(rawBody); matched {
			responded, skipDetect := server.applyClassifierRule(
				writer,
				request,
				classifierRequest{
					rule:            rule,
					backend:         backend,
					rawBody:         rawBody,
					model:           model,
					streamRequested: streamRequested,
				},
			)
			if responded {
				return
			}
			skipClassifierDetect = skipDetect
		}
	}

	if !skipClassifierDetect && (cfg.Classifier.Enabled || config.ClassifierFallbackEnabled()) && !streamRequested {
		if kind, detected := classifier.Detect(rawBody); detected {
			effectiveAction := cfg.Classifier.Action
			if effectiveAction == "" {
				effectiveAction = config.ActionFallbackOnExhaustion
			}
			targetModel := cfg.Classifier.DefaultModel
			maxTokens := cfg.Classifier.DefaultMaxTokens
			temp := cfg.Classifier.DefaultTemp
			compact := cfg.Classifier.CompactTranscript
			verdictTmpl := ""
			if kind != classifier.KindBlockPrefilter {
				verdictTmpl = cfg.Classifier.DefaultVerdict
			}
			thinkingTmpl := cfg.Classifier.DefaultThinking

			if variant, exists := cfg.Classifier.Variants[kind.String()]; exists {
				if variant.TargetModel != "" {
					targetModel = variant.TargetModel
				}
				if variant.MaxTokens > 0 {
					maxTokens = variant.MaxTokens
				}
				if variant.Temperature != nil {
					temp = variant.Temperature
				}
				if variant.CompactTranscript != nil {
					compact = *variant.CompactTranscript
				}
				if variant.CannedVerdict != "" {
					verdictTmpl = variant.CannedVerdict
				}
				if variant.ThinkingText != "" {
					thinkingTmpl = variant.ThinkingText
				}
			}

			if effectiveAction == config.ActionAlwaysStub {
				logger := server.logger
				if logger == nil {
					logger = slog.Default()
				}
				stubModel := model
				if targetModel != "" {
					stubModel = targetModel
				}
				if stub, stubErr := classifier.BuildStub(kind, stubModel, verdictTmpl, thinkingTmpl); stubErr == nil {
					logger.Warn("[Server] classifier interception: answering a security-monitor call with a canned allow verdict; its real injection/scope-creep check is skipped",
						"kind", kind, "model", stubModel)
					writer.Header().Set("Content-Type", "application/json")
					writer.WriteHeader(http.StatusOK)
					_, _ = writer.Write(stub)
					return
				} else if errors.Is(stubErr, classifier.ErrUnsupportedKind) && targetModel != "" {
					// Kinds without a captured verdict format (block-prefilter)
					// cannot be stubbed. Failing fast here breaks every
					// auto-mode permission check in the client, so reroute to
					// the variant's target model and forward instead.
					logger.Warn("[Server] classifier interception: no canned verdict for this variant; rerouting to the variant target model instead of failing fast",
						"kind", kind, "model", targetModel)
					effectiveAction = config.ActionRerouteOnly
				} else {
					logger.Warn("[Server] classifier interception: no canned verdict for this variant; failing fast instead of retrying",
						"kind", kind, "model", stubModel)
					writeAPIError(writer, http.StatusBadRequest, "invalid_request_error", "No canned verdict for this classifier variant, so the request fails fast instead of retrying.")
					return
				}
			}

			if effectiveAction == config.ActionRerouteOnly || effectiveAction == config.ActionFallbackOnExhaustion {
				if targetModel != "" {
					anthropicRequest["model"] = targetModel
					model = targetModel
					bodyMutated = true
				}
				if maxTokens > 0 {
					anthropicRequest["max_tokens"] = maxTokens
					bodyMutated = true
				}
				if temp != nil {
					anthropicRequest["temperature"] = *temp
					bodyMutated = true
				}
				if compact {
					if msgs, ok := anthropicRequest["messages"].([]any); ok {
						for i, msg := range msgs {
							if m, ok := msg.(map[string]any); ok {
								if content, ok := m["content"]; ok {
									rawContent, err := json.Marshal(content)
									if err == nil {
										if compacted, changed := classifier.CompactTranscript(rawContent); changed {
											var newContent any
											if err := json.Unmarshal(compacted, &newContent); err == nil {
												m["content"] = newContent
												msgs[i] = m
												bodyMutated = true
											}
										}
									}
								}
							}
						}
					}
				}
				if bodyMutated {
					newBody, err := json.Marshal(anthropicRequest)
					if err == nil {
						rawBody = newBody
					}
				}
				if effectiveAction == config.ActionFallbackOnExhaustion {
					isClassifierFallback = true
					classifierFallbackKind = kind
					classifierFallbackVerdictTmpl = verdictTmpl
					classifierFallbackThinkingTmpl = thinkingTmpl
				}
			}
		}
	}
	if server.dispatchAlternateBackend(&gatewayRequest{
		writer: writer, request: request, cfg: cfg,
		body: anthropicRequest, rawBody: rawBody, mutated: bodyMutated, model: model,
	}) {
		return
	}

	// Classifier fallback on exhaustion is evaluated here, after every
	// alternate-backend route has had its chance to return: Kimi, Zen,
	// Claude Code, OpenRouter and custom endpoints carry their own credentials and never
	// consume account capacity, so account exhaustion says nothing about
	// whether those requests would stall. Only the account-backed dispatch path
	// below is gated.
	if isClassifierFallback {
		noCapacity := server.accountManager != nil && server.accountManager.Available(model) == 0
		if noCapacity {
			logger := server.logger
			if logger == nil {
				logger = slog.Default()
			}
			if stub, stubErr := classifier.BuildStub(classifierFallbackKind, model, classifierFallbackVerdictTmpl, classifierFallbackThinkingTmpl); stubErr == nil {
				logger.Warn("[Server] classifier fallback: answering a security-monitor call with a canned allow verdict; its real injection/scope-creep check is skipped",
					"kind", classifierFallbackKind, "model", model)
				writer.Header().Set("Content-Type", "application/json")
				writer.WriteHeader(http.StatusOK)
				_, _ = writer.Write(stub)
				return
			}
			// 400, not 429: a 429 invites the caller's own retry/backoff,
			// which is exactly the stall this fallback exists to remove.
			logger.Warn("[Server] classifier fallback: no canned verdict for this variant; failing fast instead of retrying",
				"kind", classifierFallbackKind, "model", model)
			writeAPIError(writer, http.StatusBadRequest, "invalid_request_error", "No account capacity for model "+model+"; classifier fallback active and this classifier variant has no canned verdict, so the request fails fast instead of retrying.")
			return
		}
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
		options := cloudcode.RequestOptions{}
		if proxyformat.GetModelFamily(model) == proxyformat.FamilyClaude && proxyformat.IsThinkingModel(model) {
			options.Headers = make(http.Header)
			options.Headers.Set("anthropic-beta", "interleaved-thinking-2025-05-14")
		}
		send = func(ctx context.Context, req map[string]any, consume func(cloudcode.SSEEvent) error) (cloudcode.Response, error) {
			cloudcode.SetExecutionMetadata(ctx, credentials.Email, projectID)
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

func (server *Server) resolveModelMapping(model string) string {
	cfg := config.Get()
	if cfg.ModelMapping == nil || model == "" {
		return model
	}
	current := model
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
	return current
}

func isAnthropicEndpoint(endpointURL string) bool {
	parsed, err := url.Parse(endpointURL)
	if err != nil {
		return false
	}
	clean := strings.TrimRight(parsed.Path, "/")
	if clean == "/messages" || strings.HasSuffix(clean, "/messages") {
		return true
	}
	host := strings.ToLower(parsed.Hostname())
	return host == "api.anthropic.com" || host == "anthropic.com" || strings.HasSuffix(host, ".anthropic.com")
}

func resolveCustomEndpointURL(endpointURL string, requestPath string) (*url.URL, error) {
	targetURL, err := url.Parse(endpointURL)
	if err != nil {
		return nil, err
	}
	cleanPath := strings.TrimRight(targetURL.Path, "/")
	if cleanPath == "" {
		targetURL.Path = requestPath
		return targetURL, nil
	}
	if cleanPath == "/v1" || strings.HasSuffix(cleanPath, "/v1") {
		if strings.HasPrefix(requestPath, "/v1/") {
			targetURL.Path = cleanPath + strings.TrimPrefix(requestPath, "/v1")
		} else {
			targetURL.Path = cleanPath + requestPath
		}
		return targetURL, nil
	}
	return targetURL, nil
}

// ccDefaultProfile is the captured profile, built once.
//
// ccidentity.DefaultProfile allocates a 28-name omit list, the 13-entry beta
// list and two closures on every call, and this path calls it two or three times
// per request. The value is a constant, so it is built here instead.
var ccDefaultProfile = ccidentity.DefaultProfile()

// withCapturedBetaQuery adds the beta=true the capture records on
// POST /v1/messages.
//
// internal/ccidentity/defaults.go states a request without it does not match the
// captured traffic, and the pooled gateway path sends it because it takes its
// path from Profile.Path. This path takes its URL from the endpoint's own
// configuration, so the query has to be added rather than inherited. An existing
// beta parameter is left alone.
func withCapturedBetaQuery(rawQuery string) string {
	values, err := url.ParseQuery(rawQuery)
	if err == nil && values.Get("beta") != "" {
		return rawQuery
	}
	if rawQuery == "" {
		return "beta=true"
	}
	return rawQuery + "&beta=true"
}

// customEndpointIdentity builds the wire identity for one custom-endpoint
// request, and reports whether normalization applies.
//
// Gated on isAnthropicEndpoint: the rewrite claims to be the Claude Code client
// speaking the Anthropic wire, so an endpoint that is not Anthropic-shaped keeps
// its own headers rather than being told a lie about its request format.
//
// accountUUID is empty because a custom endpoint has no pooled Claude Code
// account. That is what the capture recorded anyway: every observed request
// carried an empty account_uuid inside metadata.user_id.
func customEndpointIdentity(endpoint config.EndpointConfig, sessionKey string) (ccidentity.Identity, bool) {
	if !isAnthropicEndpoint(endpoint.URL) {
		return ccidentity.Identity{}, false
	}
	return endpoint.Identity.Identity("", sessionKey)
}

func (server *Server) forwardToCustomEndpoint(writer http.ResponseWriter, request *http.Request, endpoint config.EndpointConfig, model string, reqBody []byte) {
	isMessagesRequest := request.URL.Path == "/v1/messages" || strings.HasSuffix(request.URL.Path, "/messages")

	targetURL, err := resolveCustomEndpointURL(endpoint.URL, request.URL.Path)
	if err != nil {
		if !isMessagesRequest {
			writeOpenAIError(writer, http.StatusBadRequest, "invalid_request_error", "Invalid custom endpoint URL: "+err.Error())
			return
		}
		writeAPIError(writer, http.StatusBadRequest, "invalid_request_error", "Invalid custom endpoint URL: "+err.Error())
		return
	}

	if strings.HasSuffix(targetURL.Path, "/messages") {
		isMessagesRequest = true
	}

	if server.isCCREnabled() && isMessagesRequest {
		var reqMap map[string]any
		if err := json.Unmarshal(reqBody, &reqMap); err == nil {
			customSessionKey := ccExtractSessionID(request, ccParseBodyMap(reqBody))
			identity, normalize := customEndpointIdentity(endpoint, customSessionKey)
			sender := func(ctx context.Context, bodyBytes []byte) (*http.Response, error) {
				if normalize {
					normalized, err := ccidentity.ApplyBody(bodyBytes, ccDefaultProfile, identity, ccidentity.Turn{})
					if err != nil {
						return nil, fmt.Errorf("normalize request body: %w", err)
					}
					bodyBytes = normalized
				}

				httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL.String(), bytes.NewReader(bodyBytes))
				if err != nil {
					return nil, err
				}
				httpReq.Header.Set("Content-Type", "application/json")
				if endpoint.APIKey != "" {
					httpReq.Header.Set("Authorization", "Bearer "+endpoint.APIKey)
					httpReq.Header.Set("x-api-key", endpoint.APIKey)
				}
				if normalize {
					ccidentity.ApplyHeaders(httpReq.Header, ccDefaultProfile, identity, ccidentity.Turn{})
					httpReq.URL.RawQuery = withCapturedBetaQuery(httpReq.URL.RawQuery)
				} else {
					if v := request.Header.Get("anthropic-version"); v != "" {
						httpReq.Header.Set("anthropic-version", v)
					} else {
						httpReq.Header.Set("anthropic-version", "2023-06-01")
					}
					if b := request.Header.Get("anthropic-beta"); b != "" {
						httpReq.Header.Set("anthropic-beta", b)
					}
				}
				httpReq.ContentLength = int64(len(bodyBytes))
				resp, err := http.DefaultClient.Do(httpReq)
				if err == nil && resp.StatusCode < 400 {
					server.maybeRecordCacheBump(cachebump.RouteCustom, request, bodyBytes, customSessionKey, model, "", model, minMaxTokensFloor)
				}
				return resp, err
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

	customSessionKey := ccExtractSessionID(request, ccParseBodyMap(reqBody))

	// Normalization happens HERE, not inside Rewrite. ApplyBody is fallible and
	// Rewrite has no error return, so a failure there could only be logged — and
	// the request would go upstream carrying the client's own body and headers,
	// which is the fingerprint normalization exists to remove.
	// ccidentity.ErrNotAnObject's doc comment names that outcome as worse than
	// refusing, and the SendMessage path already refuses. Doing the fallible work
	// before the proxy exists is what lets this path refuse too.
	outBody := reqBody
	identity, normalize := customEndpointIdentity(endpoint, customSessionKey)
	if normalize {
		normalized, err := ccidentity.ApplyBody(reqBody, ccDefaultProfile, identity, ccidentity.Turn{})
		if err != nil {
			if server.logger != nil {
				server.logger.Error("custom endpoint identity normalization failed; refusing to forward",
					"error", err, "url", targetURL.String())
			}
			message := "Custom endpoint identity normalization failed: " + err.Error()
			if !isMessagesRequest {
				writeOpenAIError(writer, http.StatusBadGateway, "api_error", message)
				return
			}
			writeAPIError(writer, http.StatusBadGateway, "api_error", message)
			return
		}
		outBody = normalized
	}

	// Rewrite (not Director): the stdlib strips Forwarded/X-Forwarded-* before
	// the hook and does not re-add them, so the custom endpoint never sees
	// proxy or client forwarding headers. It also closes the Director
	// hop-by-hop header hole. Header names still pass through net/http
	// canonicalization (X-Stainless-OS -> X-Stainless-Os); irrelevant over
	// HTTP/2, which lowercases everything on the wire.
	proxy := &httputil.ReverseProxy{
		FlushInterval: -1,
		ModifyResponse: func(resp *http.Response) error {
			if resp.StatusCode < 400 && isMessagesRequest {
				server.maybeRecordCacheBump(cachebump.RouteCustom, request, reqBody, customSessionKey, model, "", model, minMaxTokensFloor)
			}
			return nil
		},
		Rewrite: func(pr *httputil.ProxyRequest) {
			out := pr.Out
			out.URL.Scheme = targetURL.Scheme
			out.URL.Host = targetURL.Host
			out.URL.Path = targetURL.Path
			targetQuery := targetURL.RawQuery
			if targetQuery == "" || out.URL.RawQuery == "" {
				out.URL.RawQuery = targetQuery + out.URL.RawQuery
			} else {
				out.URL.RawQuery = targetQuery + "&" + out.URL.RawQuery
			}
			out.Host = targetURL.Host

			out.Body = io.NopCloser(bytes.NewReader(outBody))
			out.ContentLength = int64(len(outBody))

			if endpoint.APIKey != "" {
				out.Header.Set("Authorization", "Bearer "+endpoint.APIKey)
				out.Header.Set("x-api-key", endpoint.APIKey)
			} else {
				out.Header.Del("Authorization")
				out.Header.Del("x-api-key")
			}

			// Normalization runs LAST and only for Anthropic-shaped endpoints, so
			// the omit list can remove the x-api-key the auth block just set: the
			// captured OAuth request carries Authorization alone.
			if normalize {
				ccidentity.ApplyHeaders(out.Header, ccDefaultProfile, identity, ccidentity.Turn{})
				if isMessagesRequest {
					out.URL.RawQuery = withCapturedBetaQuery(out.URL.RawQuery)
				}
				return
			}

			if v := request.Header.Get("anthropic-version"); v != "" {
				out.Header.Set("anthropic-version", v)
			} else if isMessagesRequest {
				out.Header.Set("anthropic-version", "2023-06-01")
			}
			if b := request.Header.Get("anthropic-beta"); b != "" {
				out.Header.Set("anthropic-beta", b)
			}
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, proxyErr error) {
			server.logger.Error("custom endpoint proxy error", "error", proxyErr, "url", targetURL.String())
			if !isMessagesRequest {
				writeOpenAIError(w, http.StatusBadGateway, "api_error", "Custom endpoint forwarding error: "+proxyErr.Error())
				return
			}
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

	startTime := server.nowTime()
	sessionKey := ccExtractSessionID(request, ccParseBodyMap(body))

	if !server.isCCREnabled() {
		modify := func(resp *http.Response) error {
			if resp.StatusCode < 400 {
				server.maybeRecordCacheBump(cachebump.RouteKimi, request, body, sessionKey, model, "", "", minMaxTokensFloor)
				server.kimiInstrumentResponse(resp, model, sessionKey, startTime)
			}
			return nil
		}
		kimi.ForwardMessagesWithModify(writer, request, kimiCfg.BaseURL, kimiCfg.APIKey, body, modify)
		return
	}

	var reqMap map[string]any
	if err := json.Unmarshal(body, &reqMap); err != nil {
		modify := func(resp *http.Response) error {
			if resp.StatusCode < 400 {
				server.kimiInstrumentResponse(resp, model, sessionKey, startTime)
			}
			return nil
		}
		kimi.ForwardMessagesWithModify(writer, request, kimiCfg.BaseURL, kimiCfg.APIKey, body, modify)
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
		resp, err := http.DefaultClient.Do(httpReq)
		if err == nil && resp.StatusCode < 400 {
			server.maybeRecordCacheBump(cachebump.RouteKimi, request, reqBytes, sessionKey, model, "", "", minMaxTokensFloor)
		}
		return resp, err
	}

	opts := server.defaultCCROptions(sender)
	opts.OnUsage = func(in, out, cr, cw int) {
		latency := server.nowTime().Sub(startTime)
		metrics := kimi.RequestMetrics{
			Model:               model,
			SessionID:           sessionKey,
			InputTokens:         in,
			OutputTokens:        out,
			CacheReadTokens:     cr,
			CacheCreationTokens: cw,
			Latency:             latency,
		}
		metrics.ComputeFinalMetrics()
		kimi.LogObservability(server.logger, metrics)
		if server.tracker != nil {
			server.tracker.TrackRequest(model, latency, in, out, cr)
		}
	}

	isStreaming, _ := reqMap["stream"].(bool)
	if isStreaming {
		_ = ProxyAnthropicStreamWithCCR(request.Context(), writer, reqMap, opts)
	} else {
		_ = ProxyAnthropicJSONWithCCR(request.Context(), writer, reqMap, opts)
	}
}

func (server *Server) kimiInstrumentResponse(resp *http.Response, model, sessionID string, startTime time.Time) {
	onComplete := func(in, out, cr, cw int) {
		latency := server.nowTime().Sub(startTime)
		metrics := kimi.RequestMetrics{
			Model:               model,
			SessionID:           sessionID,
			InputTokens:         in,
			OutputTokens:        out,
			CacheReadTokens:     cr,
			CacheCreationTokens: cw,
			Latency:             latency,
		}
		metrics.ComputeFinalMetrics()
		kimi.LogObservability(server.logger, metrics)
		if server.tracker != nil {
			server.tracker.TrackRequest(model, latency, in, out, cr)
		}
	}
	if ccIsSSEResponse(resp.Header) {
		resp.Body = openrouter.NewSSEInterceptor(resp.Body, onComplete)
		return
	}
	respBytes, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		resp.Body = io.NopCloser(bytes.NewReader(respBytes))
		return
	}
	payloadBytes := respBytes
	if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		if gz, err := gzip.NewReader(bytes.NewReader(respBytes)); err == nil {
			if decompressed, err := io.ReadAll(gz); err == nil {
				payloadBytes = decompressed
			}
			_ = gz.Close()
		}
	}
	in, out, cr, cw := openrouter.ParseUsageFromJSON(payloadBytes)
	onComplete(in, out, cr, cw)
	resp.Body = io.NopCloser(bytes.NewReader(respBytes))
	resp.ContentLength = int64(len(respBytes))
	resp.Header.Set("Content-Length", strconv.Itoa(len(respBytes)))
}

// zenAPIKey resolves the Zen gateway key: config first, then the
// OPENCODE_API_KEY env fallback. Empty means no key from either source.
// The two call sites (forward path, cache-bump replay) share this helper so
// they cannot drift.
func zenAPIKey(cfg config.ZenConfig) string {
	if cfg.APIKey != "" {
		return cfg.APIKey
	}
	return os.Getenv("OPENCODE_API_KEY")
}

// forwardToZen transparently forwards an /v1/messages request to the OpenCode
// Zen gateway. The Zen Anthropic-wire endpoint needs no translation: we
// rewrite Authorization, preserve the Anthropic version/beta headers, and
// stream the response back. When CCR is enabled, it hydrates headroom_retrieve calls.
func (server *Server) forwardToZen(writer http.ResponseWriter, request *http.Request, zenCfg config.ZenConfig, body []byte, anthropicRequest map[string]any, model string, zenEntry config.ZenModelConfig) {
	key := zenAPIKey(zenCfg)
	if key == "" {
		// Defence-in-depth: matchZenModelEntry must reject keyless configs
		// before this point, so reaching here is a programming error.
		writeAPIError(writer, http.StatusInternalServerError, "api_error", "Zen route claimed without a resolved API key (zen.apiKey or OPENCODE_API_KEY)")
		return
	}
	if !zen.IsAnthropicWire(model) {
		// Defence-in-depth: matchZenModelEntry must reject non-wire entries
		// before this point, so reaching here is a programming error.
		writeAPIError(writer, http.StatusInternalServerError, "api_error",
			"Model "+model+" claimed the Zen route but is not in the Anthropic-wire subset")
		return
	}
	// max_tokens fill, not clamp: the Zen catalog carries no output limit, so
	// an omitted client value falls back to the allowlist entry, then to the
	// package default. The default must not be passed as applyMaxTokensPolicy's
	// derivedLimit — that would silently cut an explicit client value down.
	if _, present := anthropicRequest["max_tokens"]; !present {
		fill := zenEntry.MaxOutputTokens
		if fill <= 0 {
			fill = zen.DefaultMaxOutputTokens
		}
		next := maps.Clone(anthropicRequest)
		next["max_tokens"] = fill
		remarshaled, err := json.Marshal(next)
		if err != nil {
			writeAPIError(writer, http.StatusBadRequest, "invalid_request_error",
				"Failed to marshal Zen request: "+err.Error())
			return
		}
		anthropicRequest = next
		body = remarshaled
	}
	body = applyMaxTokensPolicy(body, anthropicRequest, zenEntry.MaxOutputTokens, 0)

	if server.logger != nil {
		server.logger.Info("zen forward", "model", model)
	}

	startTime := server.nowTime()
	sessionKey := ccExtractSessionID(request, ccParseBodyMap(body))

	if !server.isCCREnabled() {
		modify := func(resp *http.Response) error {
			if resp.StatusCode < 400 {
				server.maybeRecordCacheBump(cachebump.RouteZen, request, body, sessionKey, model, "", "", minMaxTokensFloor)
				server.zenInstrumentResponse(resp, model, sessionKey, startTime)
			}
			return nil
		}
		zen.ForwardMessagesWithModify(writer, request, zenCfg.BaseURL, key, body, modify)
		return
	}

	var reqMap map[string]any
	if err := json.Unmarshal(body, &reqMap); err != nil {
		modify := func(resp *http.Response) error {
			if resp.StatusCode < 400 {
				server.zenInstrumentResponse(resp, model, sessionKey, startTime)
			}
			return nil
		}
		zen.ForwardMessagesWithModify(writer, request, zenCfg.BaseURL, key, body, modify)
		return
	}

	targetURL := zen.NormalizeBaseURL(zenCfg.BaseURL) + "/v1/messages"
	sender := func(ctx context.Context, reqBytes []byte) (*http.Response, error) {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(reqBytes))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Authorization", "Bearer "+key)
		if v := request.Header.Get("anthropic-version"); v != "" {
			httpReq.Header.Set("anthropic-version", v)
		} else {
			httpReq.Header.Set("anthropic-version", "2023-06-01")
		}
		if b := request.Header.Get("anthropic-beta"); b != "" {
			httpReq.Header.Set("anthropic-beta", b)
		}
		resp, err := http.DefaultClient.Do(httpReq)
		if err == nil && resp.StatusCode < 400 {
			server.maybeRecordCacheBump(cachebump.RouteZen, request, reqBytes, sessionKey, model, "", "", minMaxTokensFloor)
		}
		return resp, err
	}

	opts := server.defaultCCROptions(sender)
	opts.OnUsage = func(in, out, cr, cw int) {
		latency := server.nowTime().Sub(startTime)
		metrics := zen.RequestMetrics{
			Model:               model,
			SessionID:           sessionKey,
			InputTokens:         in,
			OutputTokens:        out,
			CacheReadTokens:     cr,
			CacheCreationTokens: cw,
			Latency:             latency,
		}
		metrics.ComputeFinalMetrics()
		zen.LogObservability(server.logger, metrics)
		if server.tracker != nil {
			server.tracker.TrackRequest(model, latency, in, out, cr)
		}
	}

	isStreaming, _ := reqMap["stream"].(bool)
	if isStreaming {
		_ = ProxyAnthropicStreamWithCCR(request.Context(), writer, reqMap, opts)
	} else {
		_ = ProxyAnthropicJSONWithCCR(request.Context(), writer, reqMap, opts)
	}
}

func (server *Server) zenInstrumentResponse(resp *http.Response, model, sessionID string, startTime time.Time) {
	onComplete := func(in, out, cr, cw int) {
		latency := server.nowTime().Sub(startTime)
		metrics := zen.RequestMetrics{
			Model:               model,
			SessionID:           sessionID,
			InputTokens:         in,
			OutputTokens:        out,
			CacheReadTokens:     cr,
			CacheCreationTokens: cw,
			Latency:             latency,
		}
		metrics.ComputeFinalMetrics()
		zen.LogObservability(server.logger, metrics)
		if server.tracker != nil {
			server.tracker.TrackRequest(model, latency, in, out, cr)
		}
	}
	if ccIsSSEResponse(resp.Header) {
		resp.Body = openrouter.NewSSEInterceptor(resp.Body, onComplete)
		return
	}
	respBytes, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		resp.Body = io.NopCloser(bytes.NewReader(respBytes))
		return
	}
	payloadBytes := respBytes
	if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		if gz, err := gzip.NewReader(bytes.NewReader(respBytes)); err == nil {
			if decompressed, err := io.ReadAll(gz); err == nil {
				payloadBytes = decompressed
			}
			_ = gz.Close()
		}
	}
	in, out, cr, cw := openrouter.ParseUsageFromJSON(payloadBytes)
	onComplete(in, out, cr, cw)
	resp.Body = io.NopCloser(bytes.NewReader(respBytes))
	resp.ContentLength = int64(len(respBytes))
	resp.Header.Set("Content-Length", strconv.Itoa(len(respBytes)))
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

// minMaxTokensFloor guards against providers that reject tiny max_tokens
// values (muse-spark 1.3 returns 400 "max_output_tokens The number must be
// >= 16"). Applied only to values we actually send.
const minMaxTokensFloor = 16

// applyMaxTokensPolicy decides the max_tokens sent upstream:
//   - client value present: kept, clamped down to the effective limit when
//     the limit is known and the value exceeds it. Never raised above the
//     client value except by the provider floor (see minMaxTokensFloor).
//   - absent with a manual webUI override (allowlist MaxOutputTokens > 0):
//     set to the override, raised to the floor when below it.
//   - absent with no override but a derived model limit: set to the limit,
//     raised to the floor when below it.
//   - absent with nothing known: the field is omitted entirely.
//
// Returns the re-marshaled body when the value changed, the original body
// otherwise, so both the raw passthrough path and the provider-injected path
// see the same value. The caller's map is never mutated.
func applyMaxTokensPolicy(reqBody []byte, req map[string]any, manualOverride, derivedLimit int) []byte {
	limit := manualOverride
	if limit <= 0 {
		limit = derivedLimit
	}
	// The provider floor never exceeds a known limit: an admin-set cap below
	// the floor still wins.
	floor := minMaxTokensFloor
	if limit > 0 && limit < floor {
		floor = limit
	}
	value := 0
	raw, present := req["max_tokens"]
	if !present {
		if limit <= 0 {
			return reqBody
		}
		value = limit
		if value < floor {
			value = floor
		}
	} else {
		var current int
		switch v := raw.(type) {
		case float64:
			current = int(v)
		case int:
			current = v
		case int64:
			current = int(v)
		default:
			slog.Warn("max_tokens policy: non-numeric client value, forwarding unchanged",
				"type", fmt.Sprintf("%T", raw))
			return reqBody
		}
		value = current
		if limit > 0 && value > limit {
			value = limit
		}
		if value < floor {
			value = floor
		}
		if value == current {
			return reqBody
		}
	}
	next := maps.Clone(req)
	next["max_tokens"] = value
	out, err := json.Marshal(next)
	if err != nil {
		slog.Warn("max_tokens policy: failed to re-marshal request body, forwarding unchanged", "error", err)
		return reqBody
	}
	return out
}

// deriveOpenRouterMaxOutput returns the model's advertised max output from
// the cached OpenRouter model catalog, or 0 when unknown. Matching is
// case-insensitive and tolerant of an "openrouter/" prefix (GetModelLimits
// uses the same matching as GetModelPricing) because allowlist entries are
// operator-typed and commonly differ from the catalog's raw ID in case or
// prefix — an exact-string match here silently disables automatic
// max_tokens derivation for any such entry.
func deriveOpenRouterMaxOutput(model string) int {
	_, maxOutput, ok := openrouter.DefaultClient.GetModelLimits(model)
	if !ok {
		return 0
	}
	return maxOutput
}

// matchKimiModel returns the Kimi model ID if `model` matches an enabled
// allowlist entry by either ID or alias. Returns "" if no match.
func matchKimiModel(cfg config.KimiConfig, model string) string {
	item, ok := matchKimiModelEntry(cfg, model)
	if !ok {
		return ""
	}
	return stripKimi1mSuffix(item.ID)
}

// matchKimiModelEntry returns the enabled allowlist entry matching `model` by
// either ID or alias. Returns ok=false if no match.
// Suffixes such as "[1m]" (used by Claude Code for 1M context models) are normalized
// during matching.
func matchKimiModelEntry(cfg config.KimiConfig, model string) (config.KimiModelConfig, bool) {
	if strings.TrimSpace(model) == "" {
		return config.KimiModelConfig{}, false
	}
	cleanModel := stripKimi1mSuffix(model)
	if cleanModel == "" {
		return config.KimiModelConfig{}, false
	}
	for _, item := range cfg.Allowlist {
		if !item.Enabled {
			continue
		}
		itemIDClean := stripKimi1mSuffix(item.ID)
		itemAliasClean := stripKimi1mSuffix(item.Alias)
		if (item.ID != "" && (strings.EqualFold(item.ID, model) || strings.EqualFold(item.ID, cleanModel) || strings.EqualFold(itemIDClean, cleanModel))) ||
			(item.Alias != "" && (strings.EqualFold(item.Alias, model) || strings.EqualFold(item.Alias, cleanModel) || strings.EqualFold(itemAliasClean, cleanModel))) {
			return item, true
		}
	}
	return config.KimiModelConfig{}, false
}

func stripKimi1mSuffix(s string) string {
	trimmed := strings.TrimSpace(s)
	lower := strings.ToLower(trimmed)
	if len(trimmed) > 4 && strings.HasSuffix(lower, "[1m]") {
		return strings.TrimSpace(trimmed[:len(trimmed)-4])
	}
	return trimmed
}

var zenKeylessWarned atomic.Bool

// resetZenKeylessWarning re-arms the one-shot keyless warning. Called from the
// config-update path so a later misconfiguration warns again.
func resetZenKeylessWarning() { zenKeylessWarned.Store(false) }

// matchZenModelEntry returns the enabled allowlist entry matching `model` by
// either ID or alias. Returns ok=false if no match. A leading "opencode/"
// prefix (case-insensitive) is stripped from both sides before compare, so
// `opencode/claude-sonnet-4-6` matches allowlist id `claude-sonnet-4-6`.
//
// Only entries the route can actually serve claim it: the entry ID must be in
// the Anthropic-wire subset and a key must resolve. Anything else falls
// through to Claude Code / OpenRouter / CloudCode. A keyless config with
// enabled entries emits a one-shot slog.Warn (re-armed on config change)
// instead of failing the request.
func matchZenModelEntry(cfg config.ZenConfig, model string) (config.ZenModelConfig, bool) {
	if strings.TrimSpace(model) == "" {
		return config.ZenModelConfig{}, false
	}
	// Key check hoisted out of the loop: a keyless Zen config is
	// indistinguishable from an unused gateway, so it must not claim the
	// route. Diagnosability comes from the one-shot warn below.
	if zenAPIKey(cfg) == "" {
		enabled := 0
		for _, item := range cfg.Allowlist {
			if item.Enabled {
				enabled++
			}
		}
		if enabled > 0 && zenKeylessWarned.CompareAndSwap(false, true) {
			slog.Warn("zen gateway enabled with allowlisted models but no API key resolved; these models will fall through to other backends",
				"models", enabled, "hint", "set zen.apiKey or OPENCODE_API_KEY")
		}
		return config.ZenModelConfig{}, false
	}
	cleanModel := zen.StripOpencodePrefix(model)
	if cleanModel == "" {
		return config.ZenModelConfig{}, false
	}
	for _, item := range cfg.Allowlist {
		if !item.Enabled {
			continue
		}
		// Wire gate on the entry ID — the value actually forwarded upstream.
		// A non-wire entry never claims the route, so it cannot shadow a
		// backend that can serve the model.
		if !zen.IsAnthropicWire(zen.StripOpencodePrefix(item.ID)) {
			continue
		}
		if item.ID != "" && strings.EqualFold(zen.StripOpencodePrefix(item.ID), cleanModel) {
			return item, true
		}
		if item.Alias != "" && (strings.EqualFold(strings.TrimSpace(item.Alias), strings.TrimSpace(model)) ||
			strings.EqualFold(zen.StripOpencodePrefix(item.Alias), cleanModel)) {
			return item, true
		}
	}
	return config.ZenModelConfig{}, false
}

// zenTargetModel returns the canonical catalog spelling of the Zen model ID —
// the value actually sent upstream. Zen's catalog ids are lowercase; without
// this an allowlist entry spelled `Claude-Sonnet-4-6` would be forwarded
// verbatim after passing the case-insensitive wire guard.
func zenTargetModel(entry config.ZenModelConfig) string {
	if canonical, ok := zen.CanonicalAnthropicWireID(entry.ID); ok {
		return canonical
	}
	return zen.StripOpencodePrefix(entry.ID)
}

// claudeCodeEntryMaxOutput returns the allowlist entry's MaxOutputTokens for
// a canonical Claude Code model ID, falling back to the built-in defaults
// when the configured allowlist has no entry. 0 when unknown.
func claudeCodeEntryMaxOutput(cfg claudecode.Config, canonicalID string) int {
	allowlist := cfg.Allowlist
	if len(allowlist) == 0 {
		allowlist = claudecode.DefaultAllowlist()
	}
	for _, item := range allowlist {
		if item.ID == canonicalID {
			if item.MaxOutputTokens > 0 {
				return item.MaxOutputTokens
			}
			// Zero-limit entry (e.g. WebUI-imported row): inherit the
			// built-in default for the same ID so the max_tokens policy
			// still applies.
			for _, d := range claudecode.DefaultAllowlist() {
				if d.ID == canonicalID {
					return d.MaxOutputTokens
				}
			}
			return 0
		}
	}
	return 0
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
	// max_tokens policy: pass the client value through (clamped down to the
	// known model max), fill from the webUI override when set, otherwise
	// derive from the cached OpenRouter catalog. Never raised above the
	// client value except by the provider floor.
	// The upstream /v1/messages schema requires max_tokens. When the client
	// omitted it and nothing is known (cold catalog cache, no override),
	// forwarding is guaranteed to 400 — reject early with a clear error.
	derivedMax := 0
	if perModel.MaxOutputTokens <= 0 {
		derivedMax = deriveOpenRouterMaxOutput(model)
	}
	if _, present := anthropicRequest["max_tokens"]; !present && perModel.MaxOutputTokens <= 0 && derivedMax <= 0 {
		writeAPIError(writer, http.StatusBadRequest, "invalid_request_error",
			"max_tokens is required: client omitted it and no limit is known for model "+model)
		return
	}
	reqBody = applyMaxTokensPolicy(reqBody, anthropicRequest, perModel.MaxOutputTokens, derivedMax)
	order := openrouter.ProviderOrder{
		Mode:  stringDefault(perModel.ProviderMode, "auto"),
		Pin:   perModel.PinnedProvider,
		Order: perModel.ProviderOrder,
	}
	// Ensure endpoints are ranked before selection: both the auto chain and the
	// capability filter below read the ranked endpoint metadata. Cache hit
	// refreshes ranks if missing; miss fires an async warmup (which refreshes
	// ranks on success) and this request proceeds unpinned.
	// Read the cached endpoints and their fill time in one acquisition: a
	// concurrent refill must not pair endpoints from one fetch with the
	// timestamp of another.
	endpoints, cachedAt, haveCached := openrouter.DefaultEndpointsClient.GetCachedEndpointsWithTime(model, baseURL)
	if haveCached {
		// Re-rank when there are no ranks at all, and also when the cache has
		// been refilled since the last refresh: a later fetch can carry changed
		// capability metadata (supported_parameters, supports_tool_choice) that
		// the capability filter below reads, and ranks are never re-derived
		// otherwise for the process lifetime.
		rankedAt := openrouter.DefaultRouter.RankedAt(model)
		if rankedAt.IsZero() || rankedAt.Before(cachedAt) {
			openrouter.DefaultRouter.RefreshRanks(model, endpoints)
		}
	} else {
		openrouter.DefaultEndpointsClient.WarmupEndpointsAsync(model, openRouterCfg.APIKey, baseURL)
	}

	// Build the ordered failover chain: a single provider for "pinned", the
	// configured order for "custom", sticky-then-ranked for "auto".
	candidates := openrouter.DefaultRouter.SelectChain(sessionID, model, order)

	// Drop providers that cannot serve the request's tool requirements. The
	// chain is injected as provider.order with allow_fallbacks:false, so a
	// tool-incapable provider makes OpenRouter return 404 "No endpoints found"
	// instead of routing elsewhere — a pinned provider without tool support
	// fails every attempt for tool-carrying requests while plain completions
	// keep working.
	need := openrouter.ToolRequirementsFromAnthropic(anthropicRequest)
	if !need.Empty() {
		filtered := openrouter.DefaultRouter.FilterCapable(model, candidates, need, order)
		if !sameProviderChain(filtered, candidates) {
			server.logger.Info("provider chain narrowed to tool-capable endpoints",
				"model", model, "toolChoice", need.ToolChoice,
				"before", candidates, "after", filtered)
		}
		candidates = filtered
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

	maxRetries := openRouterCfg.Routing.MaxRetries
	// maxRetries bounds retry cycles over the full candidate chain; the first
	// walk of every provider is not counted against it.
	if maxRetries <= 0 {
		maxRetries = 3
	}
	base := openRouterCfg.Routing.BackoffBaseMs
	if base <= 0 {
		base = 500
	}
	cap := openRouterCfg.Routing.BackoffCapMs
	if cap <= 0 {
		cap = 120000
	}

	var (
		lastStatus         int
		lastBody           []byte
		providerIdx        = 0
		consec429          int
		tried              = make(map[string]bool)
		attempts           int
		retryCycle         int
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
		if err := openrouter.DefaultRateLimiter.Wait(request.Context(), model); err != nil {
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
		//
		// require_parameters is added only when the request forces a tool call.
		// The proxy's capability filter deliberately fails open on endpoints
		// with no advertised metadata, so a forced tool_choice can still land
		// on a provider that silently ignores it — the model answers in prose
		// and the tool is bypassed with no error anywhere. require_parameters
		// hands that decision to OpenRouter's live catalog, which drops
		// endpoints that cannot honour the parameters in the body; a request
		// left with no endpoint 404s, which the failover loop classifies and
		// retries. That is a visible failure instead of a silent one.
		//
		// Requests that merely carry tools with "auto" (the common agentic
		// case) are left alone: an endpoint that ignores tool_choice there
		// still behaves as asked, so narrowing them would only expose them to
		// 404s from an incomplete catalog.
		body := reqBody
		if bodyParsed && (provider != "" || need.ForcesTool()) {
			providerBlock := map[string]any{}
			if existing, ok := payload["provider"].(map[string]any); ok {
				for k, v := range existing {
					providerBlock[k] = v
				}
			}
			if provider != "" {
				providerBlock["order"] = []string{provider}
				providerBlock["allow_fallbacks"] = false
			}
			if need.ForcesTool() {
				providerBlock["require_parameters"] = true
			}
			payload["provider"] = providerBlock
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
		server.mu.Lock()
		appSpoofed := server.appSpoofActivated
		server.mu.Unlock()
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
			if providerIdx >= len(candidates) && retryCycle < maxRetries && server.now().Before(deadline) {
				retryCycle++
				providerIdx = 0
				tried = make(map[string]bool)
				d := computeBackoff(retryCycle, time.Duration(base)*time.Millisecond, time.Duration(cap)*time.Millisecond)
				if server.now().Add(d).After(deadline) {
					break
				}
				if !sleepOrDone(request.Context(), d) {
					return
				}
			}
			lastStatus = 0
			continue
		}

		// 2xx — handle success
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			rl := openrouter.ExtractRateLimits(resp.Header)
			openrouter.DefaultRateLimiter.RecordSuccess(model, rl)
			cacheInfo := openrouter.ExtractResponseCacheHeaders(resp.Header)
			if headerProvider := openrouter.ExtractProviderFromHeader(resp.Header); headerProvider != "" && provider == "" {
				provider = canonicalServedProvider(model, headerProvider)
			}
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
					if providerIdx >= len(candidates) && retryCycle < maxRetries && server.now().Before(deadline) {
						retryCycle++
						providerIdx = 0
						tried = make(map[string]bool)
						d := computeBackoff(retryCycle, time.Duration(base)*time.Millisecond, time.Duration(cap)*time.Millisecond)
						if server.now().Add(d).After(deadline) {
							break
						}
						if !sleepOrDone(request.Context(), d) {
							return
						}
					}
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
					case ":comment":
						if !streamStarted {
							copyUpstreamHeaders(writer.Header(), resp.Header)
							writer.WriteHeader(resp.StatusCode)
							streamStarted = true
						}
						if _, err := bw.WriteString(string(rawData) + "\n\n"); err != nil {
							return err
						}
						if err := bw.Flush(); err != nil {
							return err
						}
						if hasFlusher && flusher != nil {
							flusher.Flush()
						}
						return nil

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
						case "thinking_delta":
							if text, ok := delta["thinking"].(string); ok {
								state.AppendThinking(idx, text)
							}
						case "signature_delta":
							if sig, ok := delta["signature"].(string); ok {
								state.AppendSignature(idx, sig)
							}
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
						if providerIdx >= len(candidates) && retryCycle < maxRetries && server.now().Before(deadline) {
							retryCycle++
							providerIdx = 0
							tried = make(map[string]bool)
							d := computeBackoff(retryCycle, time.Duration(base)*time.Millisecond, time.Duration(cap)*time.Millisecond)
							if server.now().Add(d).After(deadline) {
								break
							}
							if !sleepOrDone(request.Context(), d) {
								return
							}
						}
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
				if providerIdx >= len(candidates) && retryCycle < maxRetries && server.now().Before(deadline) {
					retryCycle++
					providerIdx = 0
					tried = make(map[string]bool)
					d := computeBackoff(retryCycle, time.Duration(base)*time.Millisecond, time.Duration(cap)*time.Millisecond)
					if server.now().Add(d).After(deadline) {
						break
					}
					if !sleepOrDone(request.Context(), d) {
						return
					}
				}
				continue
			}
			// Capture served provider from response if present
			servedProvider := extractServedProviderJSON(bodyBytes)
			if servedProvider != "" {
				provider = canonicalServedProvider(model, servedProvider)
			} else if headerProvider := openrouter.ExtractProviderFromHeader(resp.Header); headerProvider != "" && provider == "" {
				provider = canonicalServedProvider(model, headerProvider)
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

		rl := openrouter.ExtractRateLimits(resp.Header)

		// Harness-gated model: attribution-level rejection, not a provider
		// failure. Retry once with spoofed app headers; if the gate persists,
		// further providers fail identically — surface the upstream error.
		// The retry decision is keyed on appSpoofed, the value read when this
		// attempt's own headers were built — not a fresh read of
		// server.appSpoofActivated, which another concurrent request could
		// have flipped after this attempt was already sent unspoofed.
		if resp.StatusCode == http.StatusForbidden && openrouter.IsHarnessGateError(bodyBytes) {
			if !appSpoofed {
				server.mu.Lock()
				server.appSpoofActivated = true
				server.mu.Unlock()
				tried[provider] = false
				server.logger.Info("OpenRouter harness gate intercepted; retrying with spoofed attribution headers",
					"model", model, "provider", provider)
				continue
			}
			break
		}

		isTransient := openrouter.IsOpenRouterTransientError(resp.StatusCode, bodyBytes)
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
				if providerIdx >= len(candidates) && retryCycle < maxRetries && server.now().Before(deadline) {
					retryCycle++
					providerIdx = 0
					tried = make(map[string]bool)
					// Record the cooldown instead of sleeping here: the
					// loop-top RateLimiter.Wait enforces it, which also
					// paces concurrent requests to the same model.
					d := computeBackoff(retryCycle, time.Duration(base)*time.Millisecond, time.Duration(cap)*time.Millisecond)
					openrouter.DefaultRateLimiter.RecordRateLimit(model, rl, d)
				}
				continue
			}
			d := computeBackoff(consec429, time.Duration(base)*time.Millisecond, time.Duration(cap)*time.Millisecond)
			openrouter.DefaultRateLimiter.RecordRateLimit(model, rl, d)
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
			if providerIdx >= len(candidates) && isTransient && retryCycle < maxRetries && server.now().Before(deadline) {
				retryCycle++
				providerIdx = 0
				tried = make(map[string]bool)
				d := computeBackoff(retryCycle, time.Duration(base)*time.Millisecond, time.Duration(cap)*time.Millisecond)
				if server.now().Add(d).After(deadline) {
					break
				}
				if !sleepOrDone(request.Context(), d) {
					return
				}
			}
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
	writeAPIError(writer, status, "api_error", fmt.Sprintf("OpenRouter upstream failed after %d attempt(s): %s", attempts, truncate(string(lastBody), upstreamErrorBodyLimit)))
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

// sameProviderChain reports whether two candidate chains are identical.
func sameProviderChain(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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
	d += time.Duration(rand.Int63n(int64(d)/2+1)) - d/4
	if d > cap {
		d = cap
	}
	return d
}

// upstreamErrorBodyLimit bounds the upstream error body echoed to the client.
// OpenRouter appends a routing_funnel to routing failures that names the filter
// step which emptied the endpoint list; 256 bytes cut it off and made a pinned
// provider's capability rejection look like a model-wide outage.
const upstreamErrorBodyLimit = 2048

// truncate caps a string at n bytes. It backs off to the previous rune
// boundary so a cut never lands mid-rune and puts invalid UTF-8 into a
// client-facing error message.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
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
		// Prefer the provider reported by the stream over the header/requested one.
		served := provider
		if headerProvider := openrouter.ExtractProviderFromHeader(resp.Header); headerProvider != "" && served == "" {
			served = canonicalServedProvider(model, headerProvider)
		}
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
			} else if strings.HasPrefix(lineStr, ":") {
				if err := handleEvent(":comment", nil, []byte(lineStr)); err != nil {
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

func (server *Server) nowTime() time.Time {
	if server != nil && server.now != nil {
		return server.now()
	}
	return time.Now()
}

func (server *Server) isCCREnabled() bool {
	if server.headroom == nil {
		return false
	}
	cfg := server.headroom.GetConfig()
	return cfg.Enabled && cfg.CCR.Enabled && server.ccrStore != nil
}

func (server *Server) getCCRChunkPayload(chunkID string) (string, bool) {
	if server.ccrStore == nil {
		return fmt.Sprintf("Error: CCR store unavailable (chunk %s)", chunkID), true
	}
	payload, found := server.ccrStore.Get(chunkID)
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
	startTime := server.nowTime()
	reqCtx, meta := cloudcode.WithExecutionMetadata(request.Context())
	totalCCRRetrievals := 0
	var totalInput, totalOutput, totalCacheRead, totalThinking int

	for iter := 0; iter <= maxCCRHydrations; iter++ {
		accumulator := proxyformat.NewThinkingAccumulator()
		_, err := send(reqCtx, anthropicRequest, func(event cloudcode.SSEEvent) error {
			return accumulator.Consume(event.Data)
		})
		if err != nil {
			server.writeError(writer, err)
			return
		}

		totalInput += accumulator.InputTokens()
		totalOutput += accumulator.OutputTokens()
		totalCacheRead += accumulator.CacheReadTokens()
		totalThinking += accumulator.ThinkingTokens()

		response := accumulator.Response(model, server.builder.Cache, "")
		retrieveCalls := findRetrieveToolUsesFromResponse(response)

		if len(retrieveCalls) == 0 || iter == maxCCRHydrations || !server.isCCREnabled() {
			stripRetrieveBlocks(response)
			if usage, ok := response["usage"].(map[string]any); ok {
				usage["input_tokens"] = totalInput
				usage["output_tokens"] = totalOutput
				usage["cache_read_input_tokens"] = totalCacheRead
			}
			latency := server.nowTime().Sub(startTime)
			if server.tracker != nil {
				server.tracker.TrackRequest(model, latency, totalInput, totalOutput, totalCacheRead)
				if totalCCRRetrievals > 0 {
					server.tracker.RecordHeadroom(stats.HeadroomSample{CCRRetrievals: totalCCRRetrievals})
				}
			}
			sessionID := ccExtractSessionID(request, anthropicRequest)
			metrics := cloudcode.RequestMetrics{
				Model:           model,
				Account:         meta.Account,
				ProjectID:       meta.ProjectID,
				SessionID:       sessionID,
				InputTokens:     totalInput,
				OutputTokens:    totalOutput,
				CacheReadTokens: totalCacheRead,
				ThinkingTokens:  totalThinking,
				CCRRetrievals:   totalCCRRetrievals,
				Latency:         latency,
			}
			metrics.ComputeFinalMetrics(cloudcode.DefaultSessionTracker, server.nowTime())
			cloudcode.LogObservability(server.logger, metrics)

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
	startTime := server.nowTime()
	reqCtx, meta := cloudcode.WithExecutionMetadata(request.Context())
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
	var totalInput, totalOutput, totalCacheRead, totalThinking int

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
				case "thinking_delta":
					if text, ok := delta["thinking"].(string); ok {
						state.AppendThinking(idx, text)
					}
				case "signature_delta":
					if sig, ok := delta["signature"].(string); ok {
						state.AppendSignature(idx, sig)
					}
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

		_, err := send(reqCtx, anthropicRequest, func(event cloudcode.SSEEvent) error {
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
		totalThinking += converter.ThinkingTokens()

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

			latency := server.nowTime().Sub(startTime)
			if server.tracker != nil {
				server.tracker.TrackRequest(model, latency, totalInput, totalOutput, totalCacheRead)
				if totalCCRRetrievals > 0 {
					server.tracker.RecordHeadroom(stats.HeadroomSample{CCRRetrievals: totalCCRRetrievals})
				}
			}
			sessionID := ccExtractSessionID(request, anthropicRequest)
			metrics := cloudcode.RequestMetrics{
				Model:           model,
				Account:         meta.Account,
				ProjectID:       meta.ProjectID,
				SessionID:       sessionID,
				InputTokens:     totalInput,
				OutputTokens:    totalOutput,
				CacheReadTokens: totalCacheRead,
				ThinkingTokens:  totalThinking,
				CCRRetrievals:   totalCCRRetrievals,
				Latency:         latency,
			}
			metrics.ComputeFinalMetrics(cloudcode.DefaultSessionTracker, server.nowTime())
			cloudcode.LogObservability(server.logger, metrics)
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

// statusClientClosedRequest is nginx's convention (499) for a client that
// disconnected before the response was written.
const statusClientClosedRequest = 499

func (server *Server) writeError(writer http.ResponseWriter, err error) {
	if isContextError(err) {
		// Cancellation and deadlines originate at the client or at a
		// bounded internal deadline, not at this server; keep them out of
		// the error log.
		server.logger.Warn("API request aborted", "error", err)
	} else {
		server.logger.Error("API request failed", "error", err)
	}
	status, kind, message := classifyError(err)
	if seconds := retryAfterSeconds(err); seconds > 0 {
		writer.Header().Set("Retry-After", strconv.Itoa(seconds))
	}
	writeAPIError(writer, status, kind, message)
}

// isContextError reports whether err is a context cancellation or deadline
// error, possibly wrapped by intermediate layers.
func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func classifyError(err error) (int, string, string) {
	if errors.Is(err, context.Canceled) {
		return statusClientClosedRequest, "api_error", "Request canceled by the client."
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout, "api_error", "Request deadline exceeded while contacting the upstream."
	}
	var rateLimitError *accounts.RateLimitError
	if errors.As(err, &rateLimitError) {
		return http.StatusTooManyRequests, "rate_limit_error", rateLimitError.Error()
	}
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
			return http.StatusTooManyRequests, "rate_limit_error", "RESOURCE_EXHAUSTED: the upstream throttled this model. Retry after the interval in Retry-After."
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

func retryAfterSeconds(err error) int {
	var rateLimitError *accounts.RateLimitError
	if errors.As(err, &rateLimitError) && rateLimitError.RetryAfter > 0 {
		return ceilSeconds(rateLimitError.RetryAfter)
	}
	var upstreamError *cloudcode.HTTPError
	if errors.As(err, &upstreamError) && upstreamError.StatusCode == http.StatusTooManyRequests {
		if wait := accounts.ParseResetTime(upstreamError.Header, upstreamError.Body, time.Now()); wait > 0 {
			return ceilSeconds(wait)
		}
	}
	return 0
}

func ceilSeconds(value time.Duration) int {
	return int((value + time.Second - 1) / time.Second)
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
