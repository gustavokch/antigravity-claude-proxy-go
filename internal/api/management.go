package api

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"antigravity-go-proxy/internal/accounts"
	"antigravity-go-proxy/internal/classifier"
	"antigravity-go-proxy/internal/claudecode"
	"antigravity-go-proxy/internal/config"
	"antigravity-go-proxy/internal/kimi"
	"antigravity-go-proxy/internal/logger"
	"antigravity-go-proxy/internal/openrouter"
	"antigravity-go-proxy/internal/stats"
	"antigravity-go-proxy/internal/zen"
)

// maxManagementRequestBody bounds the body of any request serveHTTP classifies
// as management traffic. Every handler the dispatch table serves decodes a small
// JSON payload: a config, a preset, one account, or an exported pool. The cap
// sits far below maxRequestBody, the /v1/messages limit, because no management
// payload is anywhere near that size; without it a single POST to the management
// API could pin a worker as hard as a generation request can.
const maxManagementRequestBody = 8 << 20

// maxImportedAccounts bounds one POST /api/accounts/import. The body cap alone
// would still admit millions of two-byte objects, and the pool has no ceiling:
// every added account is walked on later /v1/messages selections, so an
// oversized array is a permanent cost, not only a request-time one. 1000 is far
// above any real pool.
const maxImportedAccounts = 1000

func (server *Server) checkWebUIPassword(request *http.Request) bool {
	cfg := config.Get()
	password := cfg.WebUIPassword
	if password == "" {
		return true
	}
	provided := request.Header.Get("x-webui-password")
	if provided == "" {
		provided = request.URL.Query().Get("password")
	}
	// The compare authorized uses for API keys. This password is the only guard
	// on a non-loopback bind, so a plain == would be a timing oracle on it.
	return subtle.ConstantTimeCompare([]byte(provided), []byte(password)) == 1
}

// isManagementRoute reports whether path belongs to the management surface. It
// is the one list behind three rules: the Origin check and the body cap in
// serveHTTP, and the password gate in handleManagement. It is not the set
// handleManagement dispatches: /health and /api/event_logging/batch are served
// there and deliberately stay outside it, on the public CORS path. Every other
// route the dispatchers serve outside /api/ must be named here, or it is answered
// with Access-Control-Allow-Origin: *, no Origin check, no body cap and no
// password, which is how /refresh-token let a foreign page both trigger a pool
// reload and read the result.
// TestEveryNonAPIDispatchRouteIsManagementScoped fails when a route is added
// without being named here.
func isManagementRoute(path string) bool {
	if path == "/api/event_logging/batch" {
		return false // telemetry swallow: no state, no secret
	}
	switch path {
	case "/account-limits", "/refresh-token":
		return true
	}
	return strings.HasPrefix(path, "/api/")
}

// requiresWebUIPassword is default-deny over the management surface: a route
// isManagementRoute names is gated unless it is named public here. Every other
// path (/v1/*, /health, the WebUI assets) is not this gate's business, and
// handleManagement sees them too, so the scope check has to live here. GET
// /api/config stays public so the login form can read hasPassword before it has
// a credential; GET /api/auth/url starts the OAuth flow before login; GET
// /health answers with counts only, which is what a monitoring probe needs.
func requiresWebUIPassword(path, method string) bool {
	if !isManagementRoute(path) {
		return false
	}
	switch path {
	case "/api/config", "/api/auth/url":
		return method != http.MethodGet
	}
	return true
}

func (server *Server) handleManagement(writer http.ResponseWriter, request *http.Request, path string) bool {
	method := request.Method

	// Claude Code event logging swallow
	if path == "/api/event_logging/batch" && method == http.MethodPost {
		writeJSON(writer, http.StatusOK, map[string]any{"status": "ok"})
		return true
	}

	// WebUI Password protection. Default-deny over the management surface:
	// requiresWebUIPassword gates every route isManagementRoute names unless it
	// names the route public. The earlier form gated only the /api/ prefix, which
	// left the /refresh-token alias below serving a state-changing reload with no
	// credential while its own twin /api/accounts/reload asked for one. The scope
	// matters the other way too: this function runs for every request, so a gate
	// with no scope answers /v1/* and the WebUI assets with this 401.
	if requiresWebUIPassword(path, method) && !server.checkWebUIPassword(request) {
		writeJSON(writer, http.StatusUnauthorized, map[string]any{"status": "error", "error": "Unauthorized: Password required"})
		return true
	}

	// Dispatch routes
	switch {
	case path == "/health" && method == http.MethodGet:
		server.handleHealth(writer, request)
		return true
	case path == "/account-limits" && method == http.MethodGet:
		server.handleAccountLimits(writer, request)
		return true
	case path == "/api/custom-endpoints/refresh" && method == http.MethodPost:
		server.refreshEndpointCatalogs(request.Context(), true)
		writeJSON(writer, http.StatusOK, map[string]any{"status": "ok", "catalogs": server.endpointCatalogStatus(config.Get())})
		return true
	case path == "/api/accounts" && method == http.MethodGet:
		server.handleAccountsList(writer, request)
		return true
	case strings.HasPrefix(path, "/api/accounts/") && strings.HasSuffix(path, "/refresh") && method == http.MethodPost:
		email := strings.TrimSuffix(strings.TrimPrefix(path, "/api/accounts/"), "/refresh")
		server.handleAccountRefresh(writer, request, email)
		return true
	case strings.HasPrefix(path, "/api/accounts/") && strings.HasSuffix(path, "/toggle") && method == http.MethodPost:
		email := strings.TrimSuffix(strings.TrimPrefix(path, "/api/accounts/"), "/toggle")
		server.handleAccountToggle(writer, request, email)
		return true
	case strings.HasPrefix(path, "/api/accounts/") && method == http.MethodDelete:
		email := strings.TrimPrefix(path, "/api/accounts/")
		server.handleAccountDelete(writer, request, email)
		return true
	case strings.HasPrefix(path, "/api/accounts/") && method == http.MethodPatch:
		email := strings.TrimPrefix(path, "/api/accounts/")
		server.handleAccountPatch(writer, request, email)
		return true
	case (path == "/api/accounts/reload" || path == "/refresh-token") && method == http.MethodPost:
		server.handleAccountsReload(writer, request)
		return true
	case path == "/api/accounts/export" && method == http.MethodGet:
		server.handleAccountsExport(writer, request)
		return true
	case path == "/api/accounts/import" && method == http.MethodPost:
		server.handleAccountsImport(writer, request)
		return true
	case path == "/api/config" && method == http.MethodGet:
		server.handleConfigGet(writer, request)
		return true
	case path == "/api/config" && method == http.MethodPost:
		server.handleConfigSave(writer, request)
		return true
	case path == "/api/config/password" && method == http.MethodPost:
		server.handleConfigPassword(writer, request)
		return true
	case path == "/api/settings" && method == http.MethodGet:
		server.handleSettingsGet(writer, request)
		return true
	case path == "/api/claude/config" && method == http.MethodGet:
		server.handleClaudeConfigGet(writer, request)
		return true
	case path == "/api/claude/config" && method == http.MethodPost:
		server.handleClaudeConfigUpdate(writer, request)
		return true
	case path == "/api/claude/config/restore" && method == http.MethodPost:
		server.handleClaudeConfigRestore(writer, request)
		return true
	case path == "/api/claude/mode" && method == http.MethodGet:
		server.handleClaudeModeGet(writer, request)
		return true
	case path == "/api/claude/mode" && method == http.MethodPost:
		server.handleClaudeModeSet(writer, request)
		return true
	case path == "/api/claude/presets" && method == http.MethodGet:
		server.handleClaudePresetsGet(writer, request)
		return true
	case path == "/api/claude/presets" && method == http.MethodPost:
		server.handleClaudePresetsSave(writer, request)
		return true
	case strings.HasPrefix(path, "/api/claude/presets/") && method == http.MethodDelete:
		name := strings.TrimPrefix(path, "/api/claude/presets/")
		server.handleClaudePresetsDelete(writer, request, name)
		return true
	case path == "/api/server/presets" && method == http.MethodGet:
		server.handleServerPresetsGet(writer, request)
		return true
	case path == "/api/server/presets" && method == http.MethodPost:
		server.handleServerPresetsSave(writer, request)
		return true
	case strings.HasPrefix(path, "/api/server/presets/") && method == http.MethodPatch:
		name := strings.TrimPrefix(path, "/api/server/presets/")
		server.handleServerPresetsPatch(writer, request, name)
		return true
	case strings.HasPrefix(path, "/api/server/presets/") && method == http.MethodDelete:
		name := strings.TrimPrefix(path, "/api/server/presets/")
		server.handleServerPresetsDelete(writer, request, name)
		return true
	case path == "/api/models/config" && method == http.MethodPost:
		server.handleModelsConfigPost(writer, request)
		return true
	case path == "/api/strategy/health" && method == http.MethodGet:
		server.handleStrategyHealthGet(writer, request)
		return true
	case path == "/api/stats/history" && method == http.MethodGet:
		server.handleStatsHistory(writer, request)
		return true
	case path == "/api/stats/history" && method == http.MethodDelete:
		server.handleStatsHistoryClear(writer, request)
		return true
	case strings.HasPrefix(path, "/api/stats/history/") && method == http.MethodDelete:
		rest := strings.TrimPrefix(path, "/api/stats/history/")
		parts := strings.SplitN(rest, "/", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": "Expected /api/stats/history/{family}/{model}"})
			return true
		}
		server.handleStatsModelClear(writer, request, parts[0], parts[1])
		return true
	case path == "/api/headroom/stats" && method == http.MethodGet:
		server.handleHeadroomStats(writer, request)
		return true
	case path == "/api/cache-bump" && method == http.MethodGet:
		server.handleCacheBumpGet(writer, request)
		return true
	case path == "/api/mitm/status" && method == http.MethodGet:
		server.handleMitmStatus(writer, request)
		return true
	case path == "/api/mitm/ca.pem" && method == http.MethodGet:
		server.handleMitmCA(writer, request)
		return true
	case path == "/api/sessions/cloud" && method == http.MethodGet:
		server.handleCloudSessionsList(writer, request)
		return true
	case strings.HasPrefix(path, "/api/sessions/cloud/") && method == http.MethodGet:
		server.handleCloudSessionGet(writer, request, strings.TrimPrefix(path, "/api/sessions/cloud/"))
		return true
	case path == "/api/cache-bump" && method == http.MethodDelete:
		server.handleCacheBumpClear(writer, request)
		return true
	case strings.HasPrefix(path, "/api/cache-bump/") && strings.HasSuffix(path, "/stop") && method == http.MethodPost:
		server.handleCacheBumpStop(writer, request, stopSessionID(path))
		return true
	case path == "/api/logs" && method == http.MethodGet:
		server.handleLogsGet(writer, request)
		return true
	case path == "/api/logs/stream" && method == http.MethodGet:
		server.handleLogsStream(writer, request)
		return true
	case path == "/api/classifier/audit/stream" && method == http.MethodGet:
		server.handleClassifierAuditStream(writer, request)
		return true
	case path == "/api/openrouter/config" && method == http.MethodGet:
		server.handleOpenRouterConfigGet(writer, request)
		return true
	case path == "/api/openrouter/config" && method == http.MethodPost:
		server.handleOpenRouterConfigSave(writer, request)
		return true
	case path == "/api/openrouter/models/fetch" && method == http.MethodPost:
		server.handleOpenRouterModelsFetch(writer, request)
		return true
	case path == "/api/openrouter/models/cached" && method == http.MethodGet:
		server.handleOpenRouterModelsCached(writer, request)
		return true
	case path == "/api/openrouter/credits" && method == http.MethodGet:
		server.handleOpenRouterCredits(writer, request)
		return true
	case path == "/api/openrouter/providers" && method == http.MethodGet:
		server.handleOpenRouterProvidersGet(writer, request)
		return true
	case path == "/api/kimi/config" && method == http.MethodGet:
		server.handleKimiConfigGet(writer, request)
		return true
	case path == "/api/kimi/config" && method == http.MethodPost:
		server.handleKimiConfigSave(writer, request)
		return true
	case path == "/api/kimi/models/fetch" && method == http.MethodPost:
		server.handleKimiModelsFetch(writer, request)
		return true
	case path == "/api/kimi/auth/start" && method == http.MethodPost:
		server.handleKimiAuthStartPost(writer, request)
		return true
	case path == "/api/kimi/auth/status" && method == http.MethodGet:
		server.handleKimiAuthStatusGet(writer, request)
		return true
	case path == "/api/kimi/auth/cancel" && method == http.MethodPost:
		server.handleKimiAuthCancelPost(writer, request)
		return true
	case path == "/api/kimi/auth/logout" && method == http.MethodPost:
		server.handleKimiAuthLogoutPost(writer, request)
		return true
	case path == "/api/zen/config" && method == http.MethodGet:
		server.handleZenConfigGet(writer, request)
		return true
	case path == "/api/zen/config" && method == http.MethodPost:
		server.handleZenConfigSave(writer, request)
		return true
	case path == "/api/zen/models/fetch" && method == http.MethodPost:
		server.handleZenModelsFetch(writer, request)
		return true
	case path == "/api/auth/url" && method == http.MethodGet:
		server.handleAuthURLGet(writer, request)
		return true
	case path == "/api/auth/complete" && method == http.MethodPost:
		server.handleAuthCompletePost(writer, request)
		return true
	}

	if server.routeClaudeCodeManagement(writer, request, path, method) {
		return true
	}

	return false
}

// handleHealth answers a monitoring probe. It is one of the two routes served
// with no credential, so it carries counts only, and it reads them with Counts:
// GetStatus also builds the per-account list — emails, project IDs, subscription
// tier, quota windows and last-used timestamps — under the pool's write lock, and
// that list belongs on the gated /api/accounts view, not on a public probe.
func (server *Server) handleHealth(writer http.ResponseWriter, request *http.Request) {
	if server.accountManager != nil {
		counts := server.accountManager.Counts()
		writeJSON(writer, http.StatusOK, map[string]any{
			"status":    "ok",
			"timestamp": server.now().UTC().Format(time.RFC3339Nano),
			"accounts": map[string]any{
				"total":       counts.Total,
				"available":   counts.Available,
				"rateLimited": counts.RateLimited,
				"invalid":     counts.Invalid,
				"summary":     counts.Summary(),
			},
		})
		return
	}
	server.health(writer)
}

// Quota pool names for the Claude Code subscription windows.
const (
	claudeCodePool5h     = "claude-5h"
	claudeCodePoolWeekly = "claude-weekly"
)

// claudeCodePoolsForModel names the subscription pools a Claude Code model
// draws on. Both windows cap every model, so each model maps to both.
func claudeCodePoolsForModel(string) []string {
	return []string{claudeCodePool5h, claudeCodePoolWeekly}
}

// claudeCodeAccountSnapshots lists the Claude Code accounts from the live
// pool, or from the configuration when the pool has none. It is empty when
// the gateway is off and no account is configured.
func (server *Server) claudeCodeAccountSnapshots(cc claudecode.Config) []claudecode.AccountSnapshot {
	if !cc.Enabled && len(cc.Accounts) == 0 {
		return nil
	}
	var snapshots []claudecode.AccountSnapshot
	pool, _ := server.getOrCreateCCPool(cc)
	if pool != nil {
		snapshots = pool.Snapshots()
	}
	if len(snapshots) == 0 && len(cc.Accounts) > 0 {
		for _, acc := range cc.Accounts {
			status := "healthy"
			if !acc.Enabled {
				status = "disabled"
			}
			snapshots = append(snapshots, claudecode.AccountSnapshot{
				ID:        acc.ID,
				Name:      acc.Name,
				Email:     acc.Email,
				Type:      acc.Type,
				Priority:  acc.Priority,
				Enabled:   acc.Enabled,
				Source:    acc.Source,
				Status:    status,
				CreatedAt: server.now(),
			})
		}
	}
	return snapshots
}

// claudeCodeUsageLimitsByID maps account IDs to their configured usage
// limits, for the accounts that have any.
func claudeCodeUsageLimitsByID(cc claudecode.Config) map[string]*claudecode.UsageLimits {
	out := make(map[string]*claudecode.UsageLimits, len(cc.Accounts))
	for _, a := range cc.Accounts {
		if a.UsageLimits != nil {
			out[a.ID] = a.UsageLimits
		}
	}
	return out
}

// forEachClaudeCodeModel calls fn for every model ID and alias the Claude
// Code gateway serves: the configured allowlist, or the default catalogue
// when there is none. contextLen is the allowlist's context length, or 0.
func forEachClaudeCodeModel(cc claudecode.Config, fn func(id string, contextLen int)) {
	if len(cc.Allowlist) > 0 {
		for _, m := range cc.Allowlist {
			if m.ID != "" {
				fn(m.ID, m.ContextLen)
			}
			for _, alias := range m.ExpandAliases() {
				fn(alias, m.ContextLen)
			}
		}
		return
	}
	for _, m := range claudecode.DefaultClaudeCatalogue() {
		if m.ID != "" {
			fn(m.ID, 0)
		}
		for _, alias := range m.Aliases {
			if alias != "" {
				fn(alias, 0)
			}
		}
	}
}

// claudeCodeDisplayName is the name a Claude Code account is shown by: its
// name, else its email, else its ID.
func claudeCodeDisplayName(acc claudecode.AccountSnapshot) string {
	switch {
	case acc.Name != "":
		return acc.Name
	case acc.Email != "":
		return acc.Email
	}
	return acc.ID
}

// claudeCodeQuotaPools turns the unified subscription windows into quota
// pools. A window that is missing, has no utilization or has already reset
// at now is left out. lastChecked is the observation time in milliseconds,
// or nil when no unified headers were seen.
func claudeCodeQuotaPools(u *claudecode.Unified, now time.Time) (map[string]claudeCodePool, any) {
	pools := map[string]claudeCodePool{}
	if u == nil {
		return pools, nil
	}
	for name, w := range map[string]claudecode.UnifiedWindow{
		claudeCodePool5h:     u.FiveHour,
		claudeCodePoolWeekly: u.SevenDay,
	} {
		if w.Utilization == nil || !w.Reset.After(now) {
			continue
		}
		frac := math.Min(math.Max(1-*w.Utilization, 0), 1)
		pools[name] = claudeCodePool{
			ModelQuota: accounts.ModelQuota{
				RemainingFraction: &frac,
				ResetTime:         w.Reset.UTC().Format(time.RFC3339),
			},
			Source: claudeCodeSourceHeaders,
		}
	}
	var lastChecked any
	if !u.ObservedAt.IsZero() {
		lastChecked = u.ObservedAt.UnixMilli()
	}
	return pools, lastChecked
}

// minClaudeCodePool returns the pool with the lowest remaining fraction
// among names that are present in pools. On a tie the earlier name wins.
func minClaudeCodePool(pools map[string]claudeCodePool, names []string) (accounts.ModelQuota, bool) {
	var best accounts.ModelQuota
	found := false
	for _, name := range names {
		q, ok := pools[name]
		if !ok || q.RemainingFraction == nil {
			continue
		}
		if !found || *q.RemainingFraction < *best.RemainingFraction {
			best = q.ModelQuota
			found = true
		}
	}
	return best, found
}

func isClaudeModel(modelId string, allowlist []claudecode.ModelConfig) bool {
	lower := strings.ToLower(modelId)
	if strings.HasPrefix(lower, "claude-") {
		return true
	}
	for _, m := range allowlist {
		if strings.EqualFold(m.ID, modelId) {
			return true
		}
		for _, a := range m.ExpandAliases() {
			if strings.EqualFold(a, modelId) {
				return true
			}
		}
	}
	for _, m := range claudecode.DefaultClaudeCatalogue() {
		if strings.EqualFold(m.ID, modelId) {
			return true
		}
		for _, a := range m.Aliases {
			if strings.EqualFold(a, modelId) {
				return true
			}
		}
	}
	return false
}

func (server *Server) handleAccountLimits(writer http.ResponseWriter, request *http.Request) {
	cfg := config.Get()
	includeHistory := request.URL.Query().Get("includeHistory") == "true"

	var accountsList []*accounts.Account
	if server.accountManager != nil {
		accountsList = server.accountManager.GetAllAccounts()
	}

	format := request.URL.Query().Get("format")

	if format == "table" {
		var buf bytes.Buffer
		if server.accountManager != nil {
			if line := formatSharedThrottleLine(server.accountManager.SharedThrottles()); line != "" {
				fmt.Fprintln(&buf, line)
				fmt.Fprintln(&buf)
			}
		}
		w := tabwriter.NewWriter(&buf, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "EMAIL\tSTATUS\tTIER\tPROJECT ID\tRATE LIMITS")
		for _, acc := range accountsList {
			status := "enabled"
			if !acc.Enabled {
				status = "disabled"
			} else if acc.IsInvalid {
				status = "invalid"
			}
			tier := acc.Subscription.Tier
			if tier == "" {
				tier = "unknown"
			}
			projectID := acc.ProjectID
			if projectID == "" {
				projectID = "-"
			}
			rlCount := 0
			now := server.now().UnixMilli()
			for _, rl := range acc.ModelRateLimits {
				if rl != nil && rl.IsRateLimited && rl.ResetTimeMS > now {
					rlCount++
				}
			}
			rlStr := "none"
			if rlCount > 0 {
				rlStr = fmt.Sprintf("%d active", rlCount)
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", acc.Email, status, tier, projectID, rlStr)
		}
		w.Flush()
		writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
		writer.WriteHeader(http.StatusOK)
		writer.Write(buf.Bytes())
		return
	}

	ccSnapshots := server.claudeCodeAccountSnapshots(cfg.ClaudeCode)

	modelSet := make(map[string]bool)
	modelContext := make(map[string]any)
	// Prefer the cached catalog: /account-limits is a poll endpoint and must
	// never block on upstream I/O. A blocking refresh is only attempted on
	// cold start, when no fetch has ever succeeded. The non-blocking refresh
	// kick keeps the cache (and the quota data riding on it) live for idle
	// proxies.
	catalog := server.cachedModelCatalog()
	server.refreshModelCatalogIfStale()
	if catalog == nil && server.allowColdCatalogFetch() {
		if fresh, err := server.fetchModelCatalog(request.Context()); err == nil {
			catalog = fresh
		}
	}
	if catalog != nil {
		for _, m := range catalog.Selectable() {
			if m.ID == "" {
				continue
			}
			modelSet[m.ID] = true
			if m.MaxTokens > 0 {
				modelContext[m.ID] = m.MaxTokens
			}
		}
		for _, m := range catalog.PublicModels() {
			if m.ID == "" {
				continue
			}
			modelSet[m.ID] = true
			if m.MaxTokens > 0 {
				modelContext[m.ID] = m.MaxTokens
			}
		}
	}
	for _, acc := range accountsList {
		for m := range acc.Quota.Models {
			if m == "" {
				continue
			}
			modelSet[m] = true
		}
		for m := range acc.ModelRateLimits {
			if m == "" {
				continue
			}
			modelSet[m] = true
		}
	}

	if cfg.ClaudeCode.Enabled || len(ccSnapshots) > 0 {
		forEachClaudeCodeModel(cfg.ClaudeCode, func(id string, contextLen int) {
			modelSet[id] = true
			if contextLen > 0 {
				modelContext[id] = contextLen
			}
		})
	}

	if cfg.OpenRouter.Enabled {
		for _, m := range cfg.OpenRouter.Allowlist {
			if m.ID != "" {
				modelSet[m.ID] = true
				if m.ContextLen > 0 {
					modelContext[m.ID] = m.ContextLen
				}
			}
			if m.Alias != "" {
				modelSet[m.Alias] = true
				if m.ContextLen > 0 {
					modelContext[m.Alias] = m.ContextLen
				}
			}
		}
	}
	if cfg.Kimi.Enabled {
		for _, m := range cfg.Kimi.Allowlist {
			if m.ID != "" {
				modelSet[m.ID] = true
				if m.ContextLen > 0 {
					modelContext[m.ID] = m.ContextLen
				}
			}
			if m.Alias != "" {
				modelSet[m.Alias] = true
				if m.ContextLen > 0 {
					modelContext[m.Alias] = m.ContextLen
				}
			}
		}
	}
	if cfg.Zen.Enabled {
		for _, m := range cfg.Zen.Allowlist {
			if m.ID != "" {
				modelSet[m.ID] = true
				if m.ContextLen > 0 {
					modelContext[m.ID] = m.ContextLen
				}
			}
			if m.Alias != "" {
				modelSet[m.Alias] = true
				if m.ContextLen > 0 {
					modelContext[m.Alias] = m.ContextLen
				}
			}
		}
	}

	sortedModels := make([]string, 0, len(modelSet))
	for m := range modelSet {
		sortedModels = append(sortedModels, m)
	}
	sort.Strings(sortedModels)

	result := make([]map[string]any, 0, len(accountsList)+len(ccSnapshots))
	now := server.now().UnixMilli()

	// 1. Google Cloud Code accounts
	for _, acc := range accountsList {
		rateLimits := make(map[string]any)
		// activeResets is keyed canonically so alias and [1m] spellings resolve.
		activeResets := make(map[string]time.Time)
		for model, rl := range acc.ModelRateLimits {
			if rl != nil && rl.IsRateLimited && rl.ResetTimeMS > now {
				rateLimits[model] = map[string]any{
					"isRateLimited": true,
					"waitMs":        rl.ResetTimeMS - now,
					"actualResetMs": rl.ActualResetMS,
				}
				activeResets[model] = time.UnixMilli(rl.ResetTimeMS).UTC()
			}
		}

		limits := make(map[string]any, len(sortedModels))
		for _, modelId := range sortedModels {
			candidates := accounts.ModelKeyCandidates(modelId)

			var matchedReset time.Time
			var matched bool
			for _, candidate := range candidates {
				if reset, ok := activeResets[candidate]; ok {
					matchedReset = reset
					matched = true
					break
				}
			}

			if matched {
				limits[modelId] = map[string]any{
					"remaining":         "0%",
					"remainingFraction": 0.0,
					"resetTime":         matchedReset.Format(time.RFC3339),
				}
				continue
			}

			var q accounts.ModelQuota
			var exists bool
			for _, candidate := range candidates {
				if mq, ok := acc.Quota.Models[candidate]; ok {
					q = mq
					exists = true
					break
				}
			}

			if !exists {
				if catalog != nil {
					if resolved, err := catalog.Resolve(modelId); err == nil {
						for _, candidate := range accounts.ModelKeyCandidates(resolved.ID) {
							if mq, ok := acc.Quota.Models[candidate]; ok {
								q = mq
								exists = true
								break
							}
						}
						if !exists {
							for _, selectable := range catalog.Selectable() {
								if strings.EqualFold(selectable.ID, resolved.ID) {
									exists = true
									break
								}
							}
						}
					}
				}
			}

			if !exists {
				limits[modelId] = nil
				continue
			}
			remStr := "N/A"
			if q.RemainingFraction != nil {
				remStr = fmt.Sprintf("%d%%", int(*q.RemainingFraction*100))
			}
			limits[modelId] = map[string]any{
				"remaining":         remStr,
				"remainingFraction": q.RemainingFraction,
				"resetTime":         q.ResetTime,
			}
		}

		status := "ok"
		if !acc.Enabled {
			status = "disabled"
		} else if acc.IsInvalid {
			status = "invalid"
		} else if len(activeResets) > 0 && allAllowlistedModelsRateLimited(sortedModels, activeResets) {
			status = "rate_limited"
		}

		result = append(result, map[string]any{
			"id":                   acc.Email,
			"email":                acc.Email,
			"name":                 acc.Email,
			"provider":             "google",
			"status":               status,
			"error":                acc.InvalidReason,
			"source":               acc.Source,
			"enabled":              acc.Enabled,
			"projectId":            acc.ProjectID,
			"isInvalid":            acc.IsInvalid,
			"invalidReason":        acc.InvalidReason,
			"verifyUrl":            acc.VerifyURL,
			"lastUsed":             acc.LastUsedMS,
			"subscription":         acc.Subscription,
			"quota":                acc.Quota,
			"credits":              acc.Credits,
			"rateLimits":           rateLimits,
			"modelRateLimits":      acc.ModelRateLimits,
			"limits":               limits,
			"quotaThreshold":       acc.QuotaThreshold,
			"modelQuotaThresholds": acc.ModelThreshold,
		})
	}

	// 2. Claude Code accounts
	ccLimits := claudeCodeUsageLimitsByID(cfg.ClaudeCode)
	for _, ccAcc := range ccSnapshots {
		limits := make(map[string]any, len(sortedModels))
		rl := ccAcc.RateLimits

		status := "ok"
		if !ccAcc.Enabled {
			status = "disabled"
		} else if !ccAcc.CooldownUntil.IsZero() && ccAcc.CooldownUntil.After(server.now()) {
			status = "cooldown"
		} else if rl.IsRateLimited(server.now()) {
			status = "rate_limited"
		}

		computedFrac, hasLimits := rl.MinRemainingFractionAt(server.now())
		computedReset := rl.ResetTime(server.now())
		// A subscription account refused with unified status "rejected"
		// carries no classic reset, so its wait comes from the binding
		// window instead.
		if u := rl.Unified; u != nil && strings.EqualFold(u.Status, "rejected") {
			if br := u.BindingReset(); br.After(server.now()) && br.After(computedReset) {
				computedReset = br
			}
		}

		// Subscription accounts expose their windows as quota pools, from
		// the unified headers or the usage engine's fallbacks. API-key
		// accounts have no such windows and get pools only from configured
		// limits.
		ccPools, lastChecked, ccUsage := server.claudeCodePoolsAndUsage(ccAcc, ccLimits[ccAcc.ID], server.now())
		var ccQuota map[string]any
		if ccAcc.Type == "oauth" || ccAcc.Type == "setup_token" || len(ccPools) > 0 {
			ccQuota = map[string]any{
				"models":      map[string]any{},
				"pools":       ccPools,
				"lastChecked": lastChecked,
			}
		}

		for _, modelId := range sortedModels {
			if !isClaudeModel(modelId, cfg.ClaudeCode.Allowlist) {
				limits[modelId] = nil
				continue
			}

			// frac stays nil when Anthropic sent no rate-limit headers:
			// unknown quota, not full quota. Serializes as null so the
			// UI can render N/A instead of a false-full 100%.
			var frac *float64
			var resetTime any = nil

			if status == "disabled" {
				zero := 0.0
				frac = &zero
			} else if status == "cooldown" {
				zero := 0.0
				frac = &zero
				resetTime = ccAcc.CooldownUntil.UTC().Format(time.RFC3339)
			} else if status == "rate_limited" {
				zero := 0.0
				frac = &zero
				if !computedReset.IsZero() {
					resetTime = computedReset.UTC().Format(time.RFC3339)
				}
			} else if q, ok := minClaudeCodePool(ccPools, claudeCodePoolsForModel(modelId)); ok {
				frac = q.RemainingFraction
				resetTime = q.ResetTime
				// An API key's pools are the user's own budgets; its
				// classic rate limits still apply, and the tighter wins.
				if ccAcc.Type == "api_key" && hasLimits && computedFrac < *frac {
					f := computedFrac
					frac = &f
					resetTime = nil
					if !computedReset.IsZero() {
						resetTime = computedReset.UTC().Format(time.RFC3339)
					}
				}
			} else if hasLimits {
				f := computedFrac
				frac = &f
				if !computedReset.IsZero() {
					resetTime = computedReset.UTC().Format(time.RFC3339)
				}
			}

			remStr := "N/A"
			var fracAny any
			if frac != nil {
				remStr = fmt.Sprintf("%d%%", int(*frac*100))
				fracAny = *frac
			}
			limits[modelId] = map[string]any{
				"remaining":         remStr,
				"remainingFraction": fracAny,
				"resetTime":         resetTime,
			}
		}

		displayName := claudeCodeDisplayName(ccAcc)

		var lastUsedMS int64
		if !ccAcc.LastUsed.IsZero() {
			lastUsedMS = ccAcc.LastUsed.UnixMilli()
		}

		row := map[string]any{
			"id":                   ccAcc.ID,
			"email":                ccAcc.Email,
			"name":                 displayName,
			"provider":             "claudecode",
			"status":               status,
			"error":                nil,
			"source":               ccAcc.Source,
			"type":                 ccAcc.Type,
			"enabled":              ccAcc.Enabled,
			"priority":             ccAcc.Priority,
			"inFlight":             ccAcc.InFlight,
			"isInvalid":            false,
			"invalidReason":        "",
			"verifyUrl":            "",
			"lastUsed":             lastUsedMS,
			"rateLimits":           ccAcc.RateLimits,
			"limits":               limits,
			"totalRequests":        ccAcc.TotalRequests,
			"totalTokens":          ccAcc.TotalTokens,
			"totalCost":            ccAcc.TotalCost,
			"quotaThreshold":       0.0,
			"modelQuotaThresholds": map[string]any{},
		}
		if ccQuota != nil {
			row["quota"] = ccQuota
		}
		if ccUsage != nil {
			row["usage"] = ccUsage
		}
		result = append(result, row)
	}

	modelMapping := cfg.ModelMapping
	if modelMapping == nil {
		modelMapping = make(map[string]any)
	}

	publicCfg := config.GetPublicConfig()
	res := map[string]any{
		"status":                 "ok",
		"timestamp":              server.now().UTC().Format(time.RFC3339Nano),
		"totalAccounts":          len(result),
		"models":                 sortedModels,
		"modelContext":           modelContext,
		"modelConfig":            modelMapping,
		"customEndpoints":        publicCfg["customEndpoints"],
		"customEndpointCatalogs": server.endpointCatalogStatus(cfg),
		"openrouter":             publicCfg["openrouter"],
		"kimi":                   publicCfg["kimi"],
		"zen":                    publicCfg["zen"],
		"claudecode":             publicCfg["claudecode"],
		"globalQuotaThreshold":   cfg.GlobalQuotaThreshold,
		"accounts":               result,
	}
	if includeHistory && server.tracker != nil {
		res["history"] = server.tracker.GetHistory()
	}

	writeJSON(writer, http.StatusOK, res)
}

func (server *Server) handleAccountsList(writer http.ResponseWriter, request *http.Request) {
	if server.accountManager == nil {
		writeJSON(writer, http.StatusOK, map[string]any{
			"status":   "ok",
			"accounts": []any{},
			"summary": map[string]any{
				"total": 0, "available": 0, "rateLimited": 0, "invalid": 0,
			},
		})
		return
	}
	status := server.accountManager.GetStatus()
	writeJSON(writer, http.StatusOK, map[string]any{
		"status":   "ok",
		"accounts": status["accounts"],
		"summary": map[string]any{
			"total":       status["total"],
			"available":   status["available"],
			"rateLimited": status["rateLimited"],
			"invalid":     status["invalid"],
		},
	})
}

func (server *Server) handleAccountRefresh(writer http.ResponseWriter, request *http.Request, email string) {
	if server.accountManager == nil {
		writeJSON(writer, http.StatusNotFound, map[string]any{"status": "error", "error": "No account manager"})
		return
	}
	server.accountManager.ClearTokenCache(email)
	server.accountManager.ClearProjectCache(email)

	// If account had verification required URL, clear isInvalid on manual user refresh
	for _, acc := range server.accountManager.GetAllAccounts() {
		if acc.Email == email && acc.IsInvalid && acc.VerifyURL != "" {
			server.accountManager.ClearInvalid(email)
			break
		}
	}

	if refresher, ok := server.backend.(AccountRefresher); ok {
		acc, err := refresher.RefreshAccount(request.Context(), email)
		if err != nil {
			server.logger.Warn("refresh account upstream failed", "email", email, "error", err)
			writeJSON(writer, http.StatusOK, map[string]any{
				"status":  "ok",
				"message": fmt.Sprintf("Token cache cleared for %s (upstream refresh failed: %v)", email, err),
			})
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{
			"status":  "ok",
			"message": fmt.Sprintf("Account %s refreshed successfully", email),
			"account": acc,
		})
		return
	}

	writeJSON(writer, http.StatusOK, map[string]any{
		"status":  "ok",
		"message": fmt.Sprintf("Token cache cleared for %s", email),
	})
}

func (server *Server) handleAccountToggle(writer http.ResponseWriter, request *http.Request, email string) {
	if server.accountManager == nil {
		writeJSON(writer, http.StatusNotFound, map[string]any{"status": "error", "error": "No account manager"})
		return
	}
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil || body.Enabled == nil {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": "enabled must be a boolean"})
		return
	}

	if err := server.accountManager.SetAccountEnabled(email, *body.Enabled); err != nil {
		writeJSON(writer, http.StatusNotFound, map[string]any{"status": "error", "error": err.Error()})
		return
	}

	state := "disabled"
	if *body.Enabled {
		state = "enabled"
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"status":  "ok",
		"message": fmt.Sprintf("Account %s %s", email, state),
	})
}

func (server *Server) handleAccountDelete(writer http.ResponseWriter, request *http.Request, email string) {
	if server.accountManager == nil {
		writeJSON(writer, http.StatusNotFound, map[string]any{"status": "error", "error": "No account manager"})
		return
	}
	if err := server.accountManager.RemoveAccount(email); err != nil {
		writeJSON(writer, http.StatusNotFound, map[string]any{"status": "error", "error": err.Error()})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"status":  "ok",
		"message": fmt.Sprintf("Account %s removed", email),
	})
}

func (server *Server) handleAccountPatch(writer http.ResponseWriter, request *http.Request, email string) {
	if server.accountManager == nil {
		writeJSON(writer, http.StatusNotFound, map[string]any{"status": "error", "error": "No account manager"})
		return
	}
	var body struct {
		QuotaThreshold       *float64           `json:"quotaThreshold"`
		ModelQuotaThresholds map[string]float64 `json:"modelQuotaThresholds"`
	}
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": "Invalid request body"})
		return
	}

	if body.QuotaThreshold != nil && (*body.QuotaThreshold < 0 || *body.QuotaThreshold >= 1) {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": "quotaThreshold must be 0-0.99 or null"})
		return
	}
	for model, th := range body.ModelQuotaThresholds {
		if th < 0 || th >= 1 {
			writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": fmt.Sprintf("Invalid threshold for model %s: must be 0-0.99", model)})
			return
		}
	}

	if err := server.accountManager.UpdateThresholds(email, body.QuotaThreshold, body.ModelQuotaThresholds); err != nil {
		writeJSON(writer, http.StatusNotFound, map[string]any{"status": "error", "error": err.Error()})
		return
	}

	writeJSON(writer, http.StatusOK, map[string]any{
		"status":  "ok",
		"message": fmt.Sprintf("Account %s thresholds updated", email),
	})
}

func (server *Server) handleAccountsReload(writer http.ResponseWriter, request *http.Request) {
	if server.accountManager == nil {
		writeJSON(writer, http.StatusNotFound, map[string]any{"status": "error", "error": "No account manager"})
		return
	}
	if err := server.accountManager.Reload(""); err != nil {
		writeJSON(writer, http.StatusInternalServerError, map[string]any{"status": "error", "error": err.Error()})
		return
	}
	status := server.accountManager.GetStatus()
	writeJSON(writer, http.StatusOK, map[string]any{
		"status":  "ok",
		"message": "Accounts reloaded from disk",
		"summary": status["summary"],
	})
}

func (server *Server) handleAccountsExport(writer http.ResponseWriter, request *http.Request) {
	if server.accountManager == nil {
		writeJSON(writer, http.StatusOK, []any{})
		return
	}
	accountsList := server.accountManager.GetAllAccounts()
	result := make([]map[string]any, 0, len(accountsList))
	for _, acc := range accountsList {
		if acc.Source == "database" {
			continue
		}
		item := map[string]any{"email": acc.Email}
		if acc.RefreshToken != "" {
			item["refresh_token"] = acc.RefreshToken
		}
		if acc.APIKey != "" {
			item["api_key"] = acc.APIKey
		}
		result = append(result, item)
	}
	writeJSON(writer, http.StatusOK, result)
}

func (server *Server) handleAccountsImport(writer http.ResponseWriter, request *http.Request) {
	if server.accountManager == nil {
		writeJSON(writer, http.StatusNotFound, map[string]any{"status": "error", "error": "No account manager"})
		return
	}
	var rawData json.RawMessage
	if err := json.NewDecoder(request.Body).Decode(&rawData); err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": "Invalid JSON"})
		return
	}

	// The array is decoded as raw items so the length cap is paid before the
	// items are. Decoding straight into []map[string]any builds the whole map
	// expansion of an over-long body before the cap can answer, and the body is
	// already decoded twice over — once into the wrapper, once into the list.
	// The shape comes from the first non-space byte, so {"accounts": [...]}
	// still wins over a bare array.
	var rawItems []json.RawMessage
	if bytes.HasPrefix(bytes.TrimSpace(rawData), []byte("{")) {
		var wrapper struct {
			Accounts []json.RawMessage `json:"accounts"`
		}
		if err := json.Unmarshal(rawData, &wrapper); err != nil {
			writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": "accounts must be a non-empty array"})
			return
		}
		rawItems = wrapper.Accounts
	} else if err := json.Unmarshal(rawData, &rawItems); err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": "accounts must be a non-empty array"})
		return
	}
	if len(rawItems) > maxImportedAccounts {
		writeJSON(writer, http.StatusBadRequest, map[string]any{
			"status": "error",
			"error":  fmt.Sprintf("accounts array is too large: %d entries, maximum %d", len(rawItems), maxImportedAccounts),
		})
		return
	}
	if len(rawItems) == 0 {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": "accounts must be a non-empty array"})
		return
	}
	importList := make([]map[string]any, 0, len(rawItems))
	for _, raw := range rawItems {
		var item map[string]any
		if err := json.Unmarshal(raw, &item); err != nil {
			writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": "accounts must be a non-empty array"})
			return
		}
		importList = append(importList, item)
	}

	results := map[string][]any{
		"added":   {},
		"updated": {},
		"failed":  {},
	}
	response := map[string]any{
		"status":  "ok",
		"results": results,
	}

	existingMap := make(map[string]bool)
	for _, acc := range server.accountManager.GetAllAccounts() {
		existingMap[acc.Email] = true
	}

	applied := 0
	for _, item := range importList {
		email, _ := item["email"].(string)
		if email == "" {
			results["failed"] = append(results["failed"], map[string]string{"email": "unknown", "reason": "Missing email"})
			continue
		}
		refreshToken, _ := item["refresh_token"].(string)
		if refreshToken == "" {
			refreshToken, _ = item["refreshToken"].(string)
		}
		apiKey, _ := item["api_key"].(string)
		if apiKey == "" {
			apiKey, _ = item["apiKey"].(string)
		}

		if refreshToken == "" && apiKey == "" {
			results["failed"] = append(results["failed"], map[string]string{"email": email, "reason": "Missing refresh_token or api_key"})
			continue
		}

		source := "oauth"
		if apiKey != "" {
			source = "manual"
		}
		acc := &accounts.Account{
			Email:        email,
			Source:       source,
			RefreshToken: refreshToken,
			APIKey:       apiKey,
			Enabled:      true,
		}
		exists := existingMap[email]
		if err := server.accountManager.AddOrUpdateAccountNoSave(acc); err != nil {
			results["failed"] = append(results["failed"], map[string]string{"email": email, "reason": err.Error()})
		} else {
			applied++
			if exists {
				results["updated"] = append(results["updated"], email)
			} else {
				results["added"] = append(results["added"], email)
			}
		}
	}

	// One write for the whole import. AddOrUpdateAccount would serialize the
	// entire pool once per item.
	if applied > 0 {
		if err := server.accountManager.SaveToDisk(); err != nil {
			// The pool is updated in memory; only its persistence failed. The
			// WebUI shows status, results and error and nothing else, so a 200
			// with a side-channel field reads as a clean import and the accounts
			// are gone on the next restart. Answer as an error, and keep the
			// results so a client can still see what the pool took.
			if server.logger != nil {
				server.logger.Warn("accounts import: save to disk failed", "error", err)
			}
			response["status"] = "error"
			response["error"] = "Import applied in memory but could not be saved: " + err.Error()
			writeJSON(writer, http.StatusInternalServerError, response)
			return
		}
	}
	response["message"] = fmt.Sprintf("Imported %d accounts", len(results["added"])+len(results["updated"]))
	writeJSON(writer, http.StatusOK, response)
}

func (server *Server) handleConfigGet(writer http.ResponseWriter, request *http.Request) {
	publicCfg := config.GetPublicConfig()
	writeJSON(writer, http.StatusOK, map[string]any{
		"status":  "ok",
		"config":  publicCfg,
		"version": "1.0.0",
		"note":    "Edit ~/.config/antigravity-proxy/config.json or use env vars to change these values",
	})
}

// normalizeGatewayOrderUpdate validates and normalises an incoming
// "gatewayOrder" section. It returns a map holding only the keys the client
// sent, deliberately not a typed struct: Save's generic merge is per-key
// present, and replacing the whole section here would make a partial
// {"order":[...]} POST silently discard every per-model override. Pointer fields
// distinguish "key absent" from "key empty".
func normalizeGatewayOrderUpdate(raw any) (map[string]any, error) {
	rawMap, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("gatewayOrder must be an object")
	}
	out := make(map[string]any)
	if orderRaw, present := rawMap["order"]; present {
		ids, err := normalizeGatewayIDList(orderRaw, "gatewayOrder order")
		if err != nil {
			return nil, err
		}
		out["order"] = ids
	}
	if byModelRaw, present := rawMap["byModel"]; present {
		byModelMap, ok := byModelRaw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("gatewayOrder byModel must be an object")
		}
		keys := make([]string, 0, len(byModelMap))
		for k := range byModelMap {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		merged := make(map[string][]string)
		for _, k := range keys {
			norm := config.NormalizeModelKey(k)
			if norm == "" {
				return nil, fmt.Errorf("gatewayOrder byModel key %q must not be empty", k)
			}
			ids, err := normalizeGatewayIDList(byModelMap[k], fmt.Sprintf("gatewayOrder byModel entry for %q", k))
			if err != nil {
				return nil, err
			}
			// Two raw keys can normalise to one model (case, "[1m]",
			// whitespace): merge first-wins in sorted raw-key order so
			// nothing the operator wrote is silently dropped.
			seen := make(map[string]bool, len(merged[norm])+len(ids))
			combined := append([]string(nil), merged[norm]...)
			for _, id := range combined {
				seen[id] = true
			}
			for _, id := range ids {
				if !seen[id] {
					seen[id] = true
					combined = append(combined, id)
				}
			}
			merged[norm] = combined
		}
		out["byModel"] = merged
	}
	return out, nil
}

// normalizeGatewayIDList validates one provider-ID list: every entry must be
// a known provider string (trimmed and lowercased), with no duplicates after
// normalisation. An empty list is accepted and equals unset.
func normalizeGatewayIDList(raw any, what string) ([]string, error) {
	list, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an array of provider IDs", what)
	}
	ids := make([]string, 0, len(list))
	seen := make(map[string]bool, len(list))
	for _, item := range list {
		s, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("%s must be an array of provider IDs", what)
		}
		norm := strings.ToLower(strings.TrimSpace(s))
		if !config.IsKnownGatewayID(config.GatewayID(norm)) {
			return nil, fmt.Errorf("unknown provider %q: must be one of %s", s, strings.Join(gatewayIDStrings(config.KnownGatewayIDs()), ", "))
		}
		if seen[norm] {
			return nil, fmt.Errorf("duplicate provider %q in %s", s, what)
		}
		seen[norm] = true
		ids = append(ids, norm)
	}
	return ids, nil
}

func gatewayIDStrings(ids []config.GatewayID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, string(id))
	}
	return out
}

func (server *Server) handleConfigSave(writer http.ResponseWriter, request *http.Request) {
	var updates map[string]any
	if err := json.NewDecoder(request.Body).Decode(&updates); err != nil || len(updates) == 0 {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": "No valid configuration updates provided"})
		return
	}
	if rawKeys, ok := updates["apiKeys"]; ok && rawKeys != nil {
		// Reject here rather than in Save: Save persists first and ignores a
		// post-write decode error, so a wrongly typed list would land on disk
		// without ever being applied.
		var entries []config.APIKeyEntry
		keysBytes, err := json.Marshal(rawKeys)
		if err == nil {
			err = json.Unmarshal(keysBytes, &entries)
		}
		if err != nil {
			writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": "Invalid apiKeys configuration format"})
			return
		}
		if err := config.ValidateAPIKeys(entries); err != nil {
			writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": err.Error()})
			return
		}
	}

	if rawMitm, ok := updates["mitm"]; ok && rawMitm != nil {
		// Validate the patch over the defaults, the base Load uses. The in-memory
		// config is not safe: Save refreshes it from a file that may have no mitm
		// block, which zeroes Listen and the registry limits.
		merged := config.DefaultMitmConfig()
		mitmBytes, err := json.Marshal(rawMitm)
		if err == nil {
			err = json.Unmarshal(mitmBytes, &merged)
		}
		if err != nil {
			writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": "Invalid mitm configuration format"})
			return
		}
		if err := merged.Validate(); err != nil {
			writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": err.Error()})
			return
		}
	}

	if rawClassifier, ok := updates["classifier"]; ok && rawClassifier != nil {
		classifierBytes, err := json.Marshal(rawClassifier)
		if err != nil {
			writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": "Invalid classifier configuration format"})
			return
		}

		var classifierReq config.ClassifierConfig
		if err := json.Unmarshal(classifierBytes, &classifierReq); err != nil {
			writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": "Invalid classifier configuration: " + err.Error()})
			return
		}

		switch classifierReq.Action {
		case "", config.ActionAlwaysStub, config.ActionFallbackOnExhaustion, config.ActionRerouteOnly, config.ActionPassthrough:
			// valid
		default:
			writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": fmt.Sprintf("Invalid classifier action %q: must be empty or one of always_stub, fallback_on_exhaustion, reroute_only, passthrough", classifierReq.Action)})
			return
		}

		if classifierReq.DefaultMaxTokens < 0 {
			writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": "classifier defaultMaxTokens must be non-negative"})
			return
		}

		if classifierReq.DefaultTemp != nil && (*classifierReq.DefaultTemp < 0.0 || *classifierReq.DefaultTemp > 2.0) {
			writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": "classifier defaultTemperature must be between 0.0 and 2.0"})
			return
		}

		for variantKey, variant := range classifierReq.Variants {
			if variant.MaxTokens < 0 {
				writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": fmt.Sprintf("classifier variant %q maxTokens must be non-negative", variantKey)})
				return
			}
			if variant.Temperature != nil && (*variant.Temperature < 0.0 || *variant.Temperature > 2.0) {
				writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": fmt.Sprintf("classifier variant %q temperature must be between 0.0 and 2.0", variantKey)})
				return
			}
		}

		capture := classifierReq.Capture
		if capture.ContextEntries < -1 || capture.ContextEntries > 20 {
			writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": "classifier capture contextEntries must be between -1 and 20"})
			return
		}
		if capture.MaxFiles < -1 || capture.MaxFiles > 365 {
			writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": "classifier capture maxFiles must be between -1 and 365 (-1 keeps every day file)"})
			return
		}
		if capture.MaxFileBytes < 0 || capture.MaxFileBytes > int64(4)<<30 {
			writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": "classifier capture maxFileBytes must be between 0 and 4294967296"})
			return
		}
		if capture.Dir != "" && !filepath.IsAbs(capture.Dir) {
			writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": "classifier capture dir must be an absolute path"})
			return
		}

		for backendKey, backend := range classifierReq.Backends {
			if err := backend.ValidateLaya(); err != nil {
				writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": fmt.Sprintf("backend %q: %v", backendKey, err)})
				return
			}
		}

		for key, backend := range classifierReq.Backends {
			if strings.TrimSpace(backend.URL) == "" {
				writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": fmt.Sprintf("classifier backend %q must set a url", key)})
				return
			}
			parsed, err := url.Parse(backend.URL)
			if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
				writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": fmt.Sprintf("classifier backend %q url must be an http or https URL", key)})
				return
			}
			switch backend.Format {
			case "", config.BackendFormatAnthropic, config.BackendFormatOpenAI, config.BackendFormatLaya, config.BackendFormatJev:
				// valid
			default:
				writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": fmt.Sprintf("classifier backend %q format must be anthropic, openai, laya or jev", key)})
				return
			}
			if backend.TimeoutMs < 0 || backend.MaxTokens < 0 {
				writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": fmt.Sprintf("classifier backend %q timeoutMs and maxTokens must be non-negative", key)})
				return
			}
		}

		for index, rule := range classifierReq.Rules {
			if strings.TrimSpace(rule.ID) == "" {
				writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": fmt.Sprintf("classifier rule %d must set an id", index)})
				return
			}
			switch rule.Action {
			case config.RuleActionReroute, config.RuleActionStub, config.RuleActionPassthrough:
				// valid
			default:
				writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": fmt.Sprintf("classifier rule %q action must be reroute, stub, or passthrough", rule.ID)})
				return
			}
			if rule.Action == config.RuleActionReroute {
				if _, exists := classifierReq.Backends[rule.TargetBackend]; !exists {
					writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": fmt.Sprintf("classifier rule %q reroutes to unknown backend %q", rule.ID, rule.TargetBackend)})
					return
				}
			}
			if rule.Action == config.RuleActionStub && strings.TrimSpace(rule.VerdictTemplate) == "" {
				writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": fmt.Sprintf("classifier rule %q must set a verdictTemplate to stub with", rule.ID)})
				return
			}

			conditions := rule.Conditions
			// A rule with nothing populated matches every request the proxy
			// forwards, which would silently divert normal chat traffic.
			if len(conditions.SystemPromptPatterns) == 0 && len(conditions.FooterPatterns) == 0 &&
				len(conditions.Models) == 0 && conditions.MaxTokensMin == 0 && conditions.MaxTokensMax == 0 {
				writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": fmt.Sprintf("classifier rule %q must declare at least one condition", rule.ID)})
				return
			}
			if conditions.MaxTokensMin < 0 || conditions.MaxTokensMax < 0 {
				writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": fmt.Sprintf("classifier rule %q maxTokens bounds must be non-negative", rule.ID)})
				return
			}
			if conditions.MaxTokensMax > 0 && conditions.MaxTokensMin > conditions.MaxTokensMax {
				writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": fmt.Sprintf("classifier rule %q maxTokensMin exceeds maxTokensMax", rule.ID)})
				return
			}

			patterns := append(append([]config.MatchPattern{}, conditions.SystemPromptPatterns...), conditions.FooterPatterns...)
			for _, pattern := range patterns {
				if pattern.Type != config.PatternRegex {
					continue
				}
				if _, err := regexp.Compile(pattern.Pattern); err != nil {
					writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": fmt.Sprintf("classifier rule %q has an invalid regex %q: %v", rule.ID, pattern.Pattern, err)})
					return
				}
			}
		}

		// The WebUI reads config from the redacted public view, so it cannot
		// send back a backend key it never saw. An empty incoming key means
		// "unchanged", not "clear it".
		existing := config.Get().Classifier
		for key, incoming := range classifierReq.Backends {
			if incoming.APIKey != "" {
				continue
			}
			if previous, ok := existing.Backends[key]; ok && previous.APIKey != "" {
				incoming.APIKey = previous.APIKey
				classifierReq.Backends[key] = incoming
			}
		}
		updates["classifier"] = classifierReq
	}

	if rawGatewayOrder, ok := updates["gatewayOrder"]; ok && rawGatewayOrder != nil {
		normalized, err := normalizeGatewayOrderUpdate(rawGatewayOrder)
		if err != nil {
			writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": err.Error()})
			return
		}
		updates["gatewayOrder"] = normalized
	}

	// The identity overrides on both config surfaces become outbound header
	// values, so a control character in one breaks every request through that
	// route at the transport layer. Refuse it here, where the operator can see
	// which field it was.
	if err := validateIdentityOverrides(updates); err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": err.Error()})
		return
	}

	// The advertised limits on custom endpoints flow straight into /v1/models,
	// so the same save must refuse an output cap the provider would reject.
	if err := validateCustomEndpointLimits(updates); err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": err.Error()})
		return
	}

	// No applyXConfig push for gateway order: config.Get() is read once per
	// request (server.go), so a saved order takes effect on the next request
	// with no restart. Ordering is hot by construction.
	updated, err := config.Save(updates)
	if err != nil {
		writeJSON(writer, http.StatusInternalServerError, map[string]any{"status": "error", "error": err.Error()})
		return
	}

	if server.accountManager != nil {
		server.accountManager.SetSelectionConfig(updated.AccountSelection, updated.GlobalQuotaThreshold)
	}
	if updater, ok := server.backend.(ConfigUpdater); ok {
		updater.UpdateConfig(updated)
	}
	// Any config save may have fixed or broken the Zen key: re-arm the
	// one-shot keyless warning (see resetZenKeylessWarning).
	resetZenKeylessWarning()
	server.applyHeadroomConfig(updated.Headroom)
	server.applyClassifierConfig(updated.Classifier)
	// Same hook as startup: pushes the OpenRouter routing knobs and the Zen
	// harness disguise into their live package state.
	applyRouterConfig(updated)

	writeJSON(writer, http.StatusOK, map[string]any{
		"status":  "ok",
		"message": "Configuration saved",
		"updates": updates,
		"config":  config.GetPublicConfig(),
	})
}

func (server *Server) handleConfigPassword(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		OldPassword string `json:"oldPassword"`
		NewPassword string `json:"newPassword"`
	}
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil || body.NewPassword == "" {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": "New password is required"})
		return
	}

	current := config.Get()
	if current.WebUIPassword != "" && current.WebUIPassword != body.OldPassword {
		writeJSON(writer, http.StatusForbidden, map[string]any{"status": "error", "error": "Invalid current password"})
		return
	}

	if _, err := config.Save(map[string]any{"webuiPassword": body.NewPassword}); err != nil {
		writeJSON(writer, http.StatusInternalServerError, map[string]any{"status": "error", "error": "Failed to save password"})
		return
	}

	writeJSON(writer, http.StatusOK, map[string]any{
		"status":  "ok",
		"message": "Password changed successfully",
	})
}

func (server *Server) handleSettingsGet(writer http.ResponseWriter, request *http.Request) {
	settings := make(map[string]any)
	if server.accountManager != nil {
		settings = server.accountManager.GetSettings()
	}
	settings["port"] = 8080
	writeJSON(writer, http.StatusOK, map[string]any{
		"status":   "ok",
		"settings": settings,
	})
}

func (server *Server) handleClaudeConfigGet(writer http.ResponseWriter, request *http.Request) {
	claudeCfg, err := config.ReadClaudeConfig()
	if err != nil {
		writeJSON(writer, http.StatusInternalServerError, map[string]any{"status": "error", "error": err.Error()})
		return
	}
	path, _ := config.ClaudeConfigPath()
	writeJSON(writer, http.StatusOK, map[string]any{
		"status": "ok",
		"config": claudeCfg,
		"path":   path,
	})
}

func (server *Server) handleClaudeConfigUpdate(writer http.ResponseWriter, request *http.Request) {
	var updates map[string]any
	if err := json.NewDecoder(request.Body).Decode(&updates); err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": "Invalid config updates"})
		return
	}
	newCfg, err := config.UpdateClaudeConfig(updates)
	if err != nil {
		writeJSON(writer, http.StatusInternalServerError, map[string]any{"status": "error", "error": err.Error()})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"status":  "ok",
		"config":  newCfg,
		"message": "Claude configuration updated",
	})
}

func (server *Server) handleClaudeConfigRestore(writer http.ResponseWriter, request *http.Request) {
	newCfg, err := config.RestoreClaudeConfig()
	if err != nil {
		writeJSON(writer, http.StatusInternalServerError, map[string]any{"status": "error", "error": err.Error()})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"status":  "ok",
		"config":  newCfg,
		"message": "Claude CLI configuration restored to defaults",
	})
}

func (server *Server) handleClaudeModeGet(writer http.ResponseWriter, request *http.Request) {
	claudeCfg, _ := config.ReadClaudeConfig()
	baseUrl := ""
	if env, ok := claudeCfg["env"].(map[string]any); ok {
		baseUrl, _ = env["ANTHROPIC_BASE_URL"].(string)
	}
	isProxy := baseUrl != "" && (strings.Contains(baseUrl, "localhost") || strings.Contains(baseUrl, "127.0.0.1") || strings.Contains(baseUrl, "0.0.0.0"))
	mode := "paid"
	if isProxy {
		mode = "proxy"
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"status": "ok",
		"mode":   mode,
	})
}

func (server *Server) handleClaudeModeSet(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil || (body.Mode != "proxy" && body.Mode != "paid") {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": `mode must be "proxy" or "paid"`})
		return
	}

	claudeCfg, _ := config.ReadClaudeConfig()
	if body.Mode == "proxy" {
		claudeCfg["env"] = map[string]any{
			"ANTHROPIC_AUTH_TOKEN": "test",
			"ANTHROPIC_BASE_URL":   "http://localhost:8080",
			"ANTHROPIC_MODEL":      "claude-opus-4-6-thinking",
		}
	} else {
		delete(claudeCfg, "env")
	}

	if err := config.ReplaceClaudeConfig(claudeCfg); err != nil {
		writeJSON(writer, http.StatusInternalServerError, map[string]any{"status": "error", "error": err.Error()})
		return
	}

	writeJSON(writer, http.StatusOK, map[string]any{
		"status":  "ok",
		"mode":    body.Mode,
		"config":  claudeCfg,
		"message": fmt.Sprintf("Switched to %s mode", body.Mode),
	})
}

func (server *Server) handleClaudePresetsGet(writer http.ResponseWriter, request *http.Request) {
	presets, err := config.ReadClaudePresets()
	if err != nil {
		writeJSON(writer, http.StatusInternalServerError, map[string]any{"status": "error", "error": err.Error()})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"status": "ok", "presets": presets})
}

func (server *Server) handleClaudePresetsSave(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		Name   string         `json:"name"`
		Config map[string]any `json:"config"`
	}
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil || body.Name == "" || body.Config == nil {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": "Preset name and config required"})
		return
	}
	presets, err := config.SaveClaudePreset(body.Name, body.Config)
	if err != nil {
		writeJSON(writer, http.StatusInternalServerError, map[string]any{"status": "error", "error": err.Error()})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"status": "ok", "presets": presets, "message": fmt.Sprintf("Preset %q saved", body.Name)})
}

func (server *Server) handleClaudePresetsDelete(writer http.ResponseWriter, request *http.Request, name string) {
	presets, err := config.DeleteClaudePreset(name)
	if err != nil {
		writeJSON(writer, http.StatusInternalServerError, map[string]any{"status": "error", "error": err.Error()})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"status": "ok", "presets": presets, "message": fmt.Sprintf("Preset %q deleted", name)})
}

func (server *Server) handleServerPresetsGet(writer http.ResponseWriter, request *http.Request) {
	presets, err := config.ReadServerPresets()
	if err != nil {
		writeJSON(writer, http.StatusInternalServerError, map[string]any{"status": "error", "error": err.Error()})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"status": "ok", "presets": presets})
}

func (server *Server) handleServerPresetsSave(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Config      map[string]any `json:"config"`
	}
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil || body.Name == "" || body.Config == nil {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": "Preset name and config required"})
		return
	}
	presets, err := config.SaveServerPreset(body.Name, body.Config, body.Description)
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": err.Error()})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"status": "ok", "presets": presets, "message": fmt.Sprintf("Server preset %q saved", body.Name)})
}

func (server *Server) handleServerPresetsPatch(writer http.ResponseWriter, request *http.Request, name string) {
	var body struct {
		Description string         `json:"description"`
		Config      map[string]any `json:"config"`
	}
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": "Invalid request body"})
		return
	}
	presets, err := config.SaveServerPreset(name, body.Config, body.Description)
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": err.Error()})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"status": "ok", "presets": presets, "message": fmt.Sprintf("Server preset %q updated", name)})
}

func (server *Server) handleServerPresetsDelete(writer http.ResponseWriter, request *http.Request, name string) {
	presets, err := config.DeleteServerPreset(name)
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": err.Error()})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"status": "ok", "presets": presets, "message": fmt.Sprintf("Server preset %q deleted", name)})
}

func (server *Server) handleModelsConfigPost(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		ModelID string         `json:"modelId"`
		Config  map[string]any `json:"config"`
	}
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil || body.ModelID == "" || body.Config == nil {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": "Invalid parameters"})
		return
	}

	cfg := config.Get()
	if cfg.ModelMapping == nil {
		cfg.ModelMapping = make(map[string]any)
	}

	if del, ok := body.Config["delete"].(bool); ok && del {
		delete(cfg.ModelMapping, body.ModelID)
		if _, err := config.Save(map[string]any{"modelMapping": cfg.ModelMapping}); err != nil {
			writeJSON(writer, http.StatusInternalServerError, map[string]any{"status": "error", "error": "Failed to save configuration"})
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"status": "ok", "deleted": true, "modelId": body.ModelID})
		return
	}

	existing, _ := cfg.ModelMapping[body.ModelID].(map[string]any)
	if existing == nil {
		existing = make(map[string]any)
	}
	for k, v := range body.Config {
		existing[k] = v
	}
	cfg.ModelMapping[body.ModelID] = existing

	if _, err := config.Save(map[string]any{"modelMapping": cfg.ModelMapping}); err != nil {
		writeJSON(writer, http.StatusInternalServerError, map[string]any{"status": "error", "error": "Failed to save configuration"})
		return
	}

	writeJSON(writer, http.StatusOK, map[string]any{"status": "ok", "modelConfig": existing})
}

func (server *Server) handleStrategyHealthGet(writer http.ResponseWriter, request *http.Request) {
	if server.accountManager == nil {
		writeJSON(writer, http.StatusOK, map[string]any{"status": "ok", "trackers": nil})
		return
	}
	healthData := server.accountManager.GetStrategyHealthData()
	writeJSON(writer, http.StatusOK, map[string]any{
		"status":   "ok",
		"strategy": healthData["strategy"],
		"trackers": healthData["trackers"],
	})
}

func (server *Server) handleLogsGet(writer http.ResponseWriter, request *http.Request) {
	if server.broadcaster == nil {
		writeJSON(writer, http.StatusOK, map[string]any{"status": "ok", "logs": []any{}})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"status": "ok", "logs": server.broadcaster.GetHistory()})
}

func (server *Server) handleLogsStream(writer http.ResponseWriter, request *http.Request) {
	if server.broadcaster == nil {
		writeJSON(writer, http.StatusOK, map[string]any{"status": "ok", "logs": []any{}})
		return
	}

	streamSSE(
		writer,
		request,
		server.broadcaster.GetHistory,
		func() (<-chan logger.LogEntry, func()) { return server.broadcaster.Subscribe(100) },
		func(e logger.LogEntry) uint64 { return e.Seq },
	)
}

// handleClassifierAuditStream streams interception decisions as SSE. Like
// handleLogsStream, it subscribes before replaying ?history=true and dedupes
// the overlap by the recorder's monotonic Seq.
func (server *Server) handleClassifierAuditStream(writer http.ResponseWriter, request *http.Request) {
	if server.classifierAudit == nil {
		writeJSON(writer, http.StatusOK, map[string]any{"status": "ok", "events": []any{}})
		return
	}

	streamSSE(
		writer,
		request,
		server.classifierAudit.History,
		func() (<-chan classifier.Event, func()) { return server.classifierAudit.Subscribe(100) },
		func(e classifier.Event) uint64 { return e.Seq },
	)
}

func (server *Server) handleAuthURLGet(writer http.ResponseWriter, request *http.Request) {
	if server.oauthHandler != nil {
		server.oauthHandler.ServeHTTP(writer, request)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"status": "ok",
		"url":    "",
		"state":  "",
		"note":   "OAuth server handler not configured",
	})
}

func (server *Server) handleAuthCompletePost(writer http.ResponseWriter, request *http.Request) {
	if server.oauthHandler != nil {
		server.oauthHandler.ServeHTTP(writer, request)
		return
	}
	writeJSON(writer, http.StatusBadRequest, map[string]any{
		"status": "error",
		"error":  "OAuth callback handler not configured",
	})
}

func (server *Server) handleStatsHistory(writer http.ResponseWriter, request *http.Request) {
	if server.tracker == nil {
		writeJSON(writer, http.StatusOK, map[string]any{
			"status":  "ok",
			"history": map[string]any{},
		})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"status":  "ok",
		"history": server.tracker.GetHistory(),
	})
}

func (server *Server) handleStatsHistoryClear(writer http.ResponseWriter, request *http.Request) {
	if server.tracker == nil {
		writeJSON(writer, http.StatusOK, map[string]any{"status": "ok", "cleared": map[string]any{"models": 0, "buckets": 0}})
		return
	}
	includeHeadroom := request.URL.Query().Get("headroom") == "true"
	requests, buckets, err := server.tracker.ResetAll(includeHeadroom)
	if err != nil {
		writeJSON(writer, http.StatusInternalServerError, map[string]any{"status": "error", "error": err.Error()})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"status":  "ok",
		"cleared": map[string]any{"models": requests, "buckets": buckets},
	})
}

func (server *Server) handleStatsModelClear(writer http.ResponseWriter, request *http.Request, family, model string) {
	if server.tracker == nil {
		writeJSON(writer, http.StatusOK, map[string]any{"status": "ok", "cleared": map[string]any{"models": 0, "buckets": 0}})
		return
	}
	requests, buckets, err := server.tracker.ResetModel(family, model)
	if err != nil {
		writeJSON(writer, http.StatusInternalServerError, map[string]any{"status": "error", "error": err.Error()})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"status":  "ok",
		"cleared": map[string]any{"models": requests, "buckets": buckets},
	})
}

func (server *Server) handleHeadroomStats(writer http.ResponseWriter, request *http.Request) {
	if server.tracker == nil {
		writeJSON(writer, http.StatusOK, stats.HeadroomStats{})
		return
	}
	writeJSON(writer, http.StatusOK, server.tracker.GetHeadroomStats())
}

func (server *Server) handleOpenRouterConfigGet(writer http.ResponseWriter, request *http.Request) {
	pub := config.GetPublicConfig()
	orMap, _ := pub["openrouter"].(map[string]any)
	if orMap == nil {
		orMap = map[string]any{
			"enabled":   false,
			"baseUrl":   "https://openrouter.ai/api",
			"hasApiKey": false,
			"allowlist": []any{},
		}
	}
	activeCount := 0
	cfg := config.Get()
	if cfg.OpenRouter.Enabled {
		for _, m := range cfg.OpenRouter.Allowlist {
			if m.Enabled {
				activeCount++
			}
		}
	}
	orMap["activeModelCount"] = activeCount
	writeJSON(writer, http.StatusOK, map[string]any{
		"status": "ok",
		"config": orMap,
	})
}

func (server *Server) handleOpenRouterConfigSave(writer http.ResponseWriter, request *http.Request) {
	var body map[string]any
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": "Invalid JSON: " + err.Error()})
		return
	}
	saved, err := config.Save(map[string]any{"openrouter": body})
	if err != nil {
		writeJSON(writer, http.StatusInternalServerError, map[string]any{"status": "error", "error": "Failed to save config: " + err.Error()})
		return
	}
	if updater, ok := server.backend.(ConfigUpdater); ok {
		updater.UpdateConfig(saved)
	}
	if saved.OpenRouter.Enabled {
		openrouter.DefaultClient.WarmupCacheAsync(saved.OpenRouter.APIKey, saved.OpenRouter.BaseURL)
		for _, item := range saved.OpenRouter.Allowlist {
			if item.Enabled {
				openrouter.DefaultEndpointsClient.WarmupEndpointsAsync(item.ID, saved.OpenRouter.APIKey, saved.OpenRouter.BaseURL)
			}
		}
	}
	applyRouterConfig(saved)
	pub := config.GetPublicConfig()
	writeJSON(writer, http.StatusOK, map[string]any{
		"status": "ok",
		"config": pub["openrouter"],
	})
}

func (server *Server) handleOpenRouterModelsFetch(writer http.ResponseWriter, request *http.Request) {
	var req struct {
		APIKey  string `json:"apiKey,omitempty"`
		BaseURL string `json:"baseUrl,omitempty"`
	}
	_ = json.NewDecoder(request.Body).Decode(&req)
	cfg := config.Get()
	apiKey := req.APIKey
	if apiKey == "" {
		apiKey = cfg.OpenRouter.APIKey
	}
	baseURL := req.BaseURL
	if baseURL == "" {
		baseURL = cfg.OpenRouter.BaseURL
	}
	models, err := openrouter.DefaultClient.FetchAvailableModels(request.Context(), apiKey, baseURL)
	if err != nil {
		writeJSON(writer, http.StatusBadGateway, map[string]any{
			"status": "error",
			"error":  err.Error(),
		})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"status": "ok",
		"models": models,
		"total":  len(models),
	})
}

func (server *Server) handleOpenRouterModelsCached(writer http.ResponseWriter, request *http.Request) {
	models := openrouter.DefaultClient.GetCachedModels()
	if models == nil {
		models = []openrouter.ModelItem{}
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"status": "ok",
		"models": models,
		"total":  len(models),
	})
}

// handleOpenRouterCredits reports OpenRouter prepaid credit totals, usage, and
// the derived balance for the dashboard balance card. A 403 from OpenRouter
// maps to requiresManagementKey instead of a generic error: standard API keys
// lack the management permissions the credits endpoint requires.
func (server *Server) handleOpenRouterCredits(writer http.ResponseWriter, request *http.Request) {
	cfg := config.Get()
	if !cfg.OpenRouter.Enabled {
		writeJSON(writer, http.StatusOK, map[string]any{
			"status":  "ok",
			"enabled": false,
		})
		return
	}
	if strings.TrimSpace(cfg.OpenRouter.APIKey) == "" {
		writeJSON(writer, http.StatusOK, map[string]any{
			"status":    "ok",
			"enabled":   true,
			"hasApiKey": false,
		})
		return
	}

	force := request.URL.Query().Get("force") == "true"
	credits, err := openrouter.DefaultClient.ResolveCredits(request.Context(), cfg.OpenRouter.APIKey, cfg.OpenRouter.BaseURL, force)
	if err != nil {
		if errors.Is(err, openrouter.ErrManagementKeyRequired) {
			writeJSON(writer, http.StatusOK, map[string]any{
				"status":                "ok",
				"enabled":               true,
				"hasApiKey":             true,
				"requiresManagementKey": true,
				"error":                 "OpenRouter management API key required to view credits",
			})
			return
		}
		writeJSON(writer, http.StatusBadGateway, map[string]any{
			"status": "error",
			"error":  err.Error(),
		})
		return
	}

	writeJSON(writer, http.StatusOK, map[string]any{
		"status":    "ok",
		"enabled":   true,
		"hasApiKey": true,
		"credits":   credits,
	})
}

// handleOpenRouterProvidersGet returns the ranked provider list for a model with
// live EWMA stats from the router, plus the model's current routing config.
func (server *Server) handleOpenRouterProvidersGet(writer http.ResponseWriter, request *http.Request) {
	model := strings.TrimSpace(request.URL.Query().Get("model"))
	if model == "" {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": "model query parameter is required"})
		return
	}

	cfg := config.Get()
	baseURL := cfg.OpenRouter.BaseURL

	// Resolve endpoints: cache first, fetch on miss, refresh ranks.
	endpoints, ok := openrouter.DefaultEndpointsClient.GetCachedEndpoints(model, baseURL)
	if !ok {
		fetched, err := openrouter.DefaultEndpointsClient.ResolveModelEndpoints(request.Context(), model, cfg.OpenRouter.APIKey, baseURL)
		if err != nil {
			// Do not log the raw error: it embeds the upstream response body.
			server.logger.Warn("failed to fetch OpenRouter endpoints", "model", model)
			writeJSON(writer, http.StatusBadGateway, map[string]any{"status": "error", "error": "Failed to fetch provider endpoints from OpenRouter"})
			return
		}
		endpoints = fetched
	}
	if len(endpoints) > 0 {
		openrouter.DefaultRouter.RefreshRanks(model, endpoints)
	}

	ranks := openrouter.DefaultRouter.GetRanks(model)
	stats := openrouter.DefaultRouter.Stats(model)

	type providerEntry struct {
		Provider   string                           `json:"provider"`
		Tag        string                           `json:"tag,omitempty"`
		ContextLen int                              `json:"contextLength,omitempty"`
		Uptime     float64                          `json:"uptime"`
		Score      float64                          `json:"score"`
		Endpoint   openrouter.ProviderEndpoint      `json:"endpoint"`
		Stats      openrouter.ProviderStatsSnapshot `json:"stats"`
	}
	providers := make([]providerEntry, 0, len(ranks))
	for _, rk := range ranks {
		entry := providerEntry{
			Provider:   rk.Provider,
			Tag:        rk.Tag,
			ContextLen: rk.ContextLen,
			Uptime:     rk.Endpoint.BlendedUptime(),
			Score:      rk.Score,
			Endpoint:   rk.Endpoint,
		}
		if s, ok := stats[rk.Provider]; ok {
			entry.Stats = s
		}
		providers = append(providers, entry)
	}

	// Per-model routing config from the allowlist item.
	mode := "auto"
	var pinnedProvider string
	var order []string
	for _, item := range cfg.OpenRouter.Allowlist {
		if item.ID == model {
			if item.ProviderMode != "" {
				mode = item.ProviderMode
			}
			pinnedProvider = item.PinnedProvider
			order = item.ProviderOrder
			break
		}
	}

	writeJSON(writer, http.StatusOK, map[string]any{
		"status":         "ok",
		"model":          model,
		"mode":           mode,
		"pinnedProvider": pinnedProvider,
		"providerOrder":  order,
		"providers":      providers,
	})
}

func (server *Server) handleKimiConfigGet(writer http.ResponseWriter, request *http.Request) {
	pub := config.GetPublicConfig()
	kimiMap, _ := pub["kimi"].(map[string]any)
	if kimiMap == nil {
		kimiMap = map[string]any{
			"enabled":   false,
			"baseUrl":   "https://api.moonshot.ai/anthropic",
			"hasApiKey": false,
			"allowlist": []any{},
		}
	}
	activeCount := 0
	cfg := config.Get()
	if cfg.Kimi.Enabled {
		for _, m := range cfg.Kimi.Allowlist {
			if m.Enabled {
				activeCount++
			}
		}
	}
	kimiMap["activeModelCount"] = activeCount
	writeJSON(writer, http.StatusOK, map[string]any{
		"status": "ok",
		"config": kimiMap,
	})
}

func (server *Server) handleKimiConfigSave(writer http.ResponseWriter, request *http.Request) {
	var body map[string]any
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": "Invalid JSON: " + err.Error()})
		return
	}
	saved, err := config.Save(map[string]any{"kimi": body})
	if err != nil {
		writeJSON(writer, http.StatusInternalServerError, map[string]any{"status": "error", "error": "Failed to save config: " + err.Error()})
		return
	}
	if updater, ok := server.backend.(ConfigUpdater); ok {
		updater.UpdateConfig(saved)
	}
	pub := config.GetPublicConfig()
	writeJSON(writer, http.StatusOK, map[string]any{
		"status": "ok",
		"config": pub["kimi"],
	})
}

func (server *Server) handleKimiModelsFetch(writer http.ResponseWriter, request *http.Request) {
	var req struct {
		APIKey  string `json:"apiKey,omitempty"`
		BaseURL string `json:"baseUrl,omitempty"`
	}
	_ = json.NewDecoder(request.Body).Decode(&req)
	cfg := config.Get()

	var apiKey, baseURL string
	var headers http.Header
	if req.APIKey != "" {
		// Explicit key from the picker: the current behaviour.
		apiKey = req.APIKey
		baseURL = req.BaseURL
		if baseURL == "" {
			baseURL = cfg.Kimi.BaseURL
		}
		if baseURL == "" {
			baseURL = "https://api.moonshot.ai/anthropic"
		}
	} else {
		cred, err := server.resolveKimiCredential(request.Context(), cfg.Kimi)
		if err != nil {
			status, _ := kimiCredentialErrorStatus(err)
			writeJSON(writer, status, map[string]any{
				"status": "error",
				"error":  err.Error(),
			})
			return
		}
		apiKey = cred.token
		if cred.oauth {
			// The OAuth token is valid only on api.kimi.ai; ignore req.BaseURL.
			baseURL = cred.baseURL
			headers = server.kimiIdentityHeaders()
		} else {
			baseURL = req.BaseURL
			if baseURL == "" {
				baseURL = cred.baseURL
			}
			if baseURL == "" {
				baseURL = "https://api.moonshot.ai/anthropic"
			}
		}
	}

	models, err := kimi.DefaultClient.FetchModels(request.Context(), apiKey, baseURL, headers)
	if err != nil {
		writeJSON(writer, http.StatusBadGateway, map[string]any{
			"status": "error",
			"error":  err.Error(),
		})
		return
	}
	if models == nil {
		models = []kimi.ModelItem{}
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"status": "ok",
		"models": models,
		"total":  len(models),
	})
}

// zenKeySource reports where the Zen gateway key comes from: "config" when
// zen.apiKey is set, "env" when only OPENCODE_API_KEY is set, "none" when
// neither is. The env value itself is never echoed.
func zenKeySource(cfg config.ZenConfig) string {
	if cfg.APIKey != "" {
		return "config"
	}
	if zenAPIKey(cfg) != "" {
		return "env"
	}
	return "none"
}

func (server *Server) handleZenConfigGet(writer http.ResponseWriter, request *http.Request) {
	pub := config.GetPublicConfig()
	zenMap, _ := pub["zen"].(map[string]any)
	if zenMap == nil {
		zenMap = map[string]any{
			"enabled":   false,
			"baseUrl":   zen.DefaultBaseURL,
			"hasApiKey": false,
			"allowlist": []any{},
		}
	}
	cfg := config.Get()
	source := zenKeySource(cfg.Zen)
	zenMap["keySource"] = source
	if source != "none" {
		zenMap["hasApiKey"] = true
	}
	activeCount := 0
	if cfg.Zen.Enabled {
		for _, m := range cfg.Zen.Allowlist {
			if m.Enabled {
				activeCount++
			}
		}
	}
	zenMap["activeModelCount"] = activeCount
	writeJSON(writer, http.StatusOK, map[string]any{
		"status": "ok",
		"config": zenMap,
	})
}

func (server *Server) handleZenConfigSave(writer http.ResponseWriter, request *http.Request) {
	var body map[string]any
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": "Invalid JSON: " + err.Error()})
		return
	}
	saved, err := config.Save(map[string]any{"zen": body})
	if err != nil {
		writeJSON(writer, http.StatusInternalServerError, map[string]any{"status": "error", "error": "Failed to save config: " + err.Error()})
		return
	}
	if updater, ok := server.backend.(ConfigUpdater); ok {
		updater.UpdateConfig(saved)
	}
	pub := config.GetPublicConfig()
	zenMap, _ := pub["zen"].(map[string]any)
	if zenMap != nil {
		source := zenKeySource(saved.Zen)
		zenMap["keySource"] = source
		if source != "none" {
			zenMap["hasApiKey"] = true
		}
	}
	// Re-arm the one-shot keyless warning so a later misconfiguration warns
	// again instead of staying silent for the process lifetime.
	resetZenKeylessWarning()
	writeJSON(writer, http.StatusOK, map[string]any{
		"status": "ok",
		"config": zenMap,
	})
}

func (server *Server) handleZenModelsFetch(writer http.ResponseWriter, request *http.Request) {
	var req struct {
		APIKey  string `json:"apiKey,omitempty"`
		BaseURL string `json:"baseUrl,omitempty"`
	}
	_ = json.NewDecoder(request.Body).Decode(&req)
	cfg := config.Get()
	apiKey := req.APIKey
	if apiKey == "" {
		apiKey = cfg.Zen.APIKey
	}
	baseURL := req.BaseURL
	if baseURL == "" {
		baseURL = cfg.Zen.BaseURL
	}
	if baseURL == "" {
		baseURL = zen.DefaultBaseURL
	}
	// An empty key is not an error: the catalog endpoint answers
	// unauthenticated, so the picker populates before a key is pasted.
	models, err := zen.DefaultClient.FetchModels(request.Context(), apiKey, baseURL)
	if err != nil {
		writeJSON(writer, http.StatusBadGateway, map[string]any{
			"status": "error",
			"error":  err.Error(),
		})
		return
	}
	if models == nil {
		models = []zen.ModelItem{}
	}
	usable := make([]zen.ModelItem, 0, len(models))
	other := make([]zen.ModelItem, 0)
	anthropicCount := 0
	chatCount := 0
	responsesCount := 0
	for _, m := range models {
		_, wire := zen.WireFor(m.ID)
		switch wire {
		case zen.WireAnthropic:
			anthropicCount++
			usable = append(usable, m)
		case zen.WireChat:
			chatCount++
			usable = append(usable, m)
		case zen.WireResponses:
			responsesCount++
			usable = append(usable, m)
		default:
			other = append(other, m)
		}
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"status":    "ok",
		"models":    usable,
		"other":     other,
		"total":     len(models),
		"anthropic": anthropicCount,
		"chat":      chatCount,
		"responses": responsesCount,
	})
}

// formatSharedThrottleLine renders the pool-wide throttles above the
// per-account table. Without it a shared throttle is indistinguishable from
// two independent per-account rate limits.
func formatSharedThrottleLine(throttles map[string]time.Duration) string {
	if len(throttles) == 0 {
		return ""
	}
	models := make([]string, 0, len(throttles))
	for model := range throttles {
		models = append(models, model)
	}
	sort.Strings(models)
	parts := make([]string, 0, len(models))
	for _, model := range models {
		parts = append(parts, fmt.Sprintf("%s (%s left)", model, throttles[model].Round(time.Second)))
	}
	return "SHARED THROTTLE: " + strings.Join(parts, ", ")
}

func allAllowlistedModelsRateLimited(models []string, activeResets map[string]time.Time) bool {
	if len(models) == 0 {
		return false
	}
	for _, m := range models {
		candidates := accounts.ModelKeyCandidates(m)
		matched := false
		for _, c := range candidates {
			if _, ok := activeResets[c]; ok {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}
