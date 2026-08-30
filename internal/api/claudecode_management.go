package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"antigravity-go-proxy/internal/claudecode"
	"antigravity-go-proxy/internal/config"
)

// handleClaudeCodeConfigGet returns the redacted ClaudeCode gateway config.
func (server *Server) handleClaudeCodeConfigGet(writer http.ResponseWriter, _ *http.Request) {
	pub := config.GetPublicConfig()
	cc, _ := pub["claudecode"]
	writeJSON(writer, http.StatusOK, map[string]any{
		"status": "ok",
		"config": cc,
	})
}

// ccAccountToMap converts an account to its full persisted form. Every config
// rewrite must use this so surviving accounts keep their refresh token,
// expiry, email, UUIDs and source; dropping any of these breaks token refresh
// after a restart.
func ccAccountToMap(a claudecode.AccountConfig) map[string]any {
	return map[string]any{
		"id":               a.ID,
		"name":             a.Name,
		"token":            a.Token,
		"refreshToken":     a.RefreshToken,
		"expiresAt":        a.ExpiresAt,
		"email":            a.Email,
		"accountUuid":      a.AccountUUID,
		"organizationUuid": a.OrganizationUUID,
		"type":             a.Type,
		"priority":         a.Priority,
		"enabled":          a.Enabled,
		"source":           a.Source,
	}
}

// handleClaudeCodeConfigPost saves ClaudeCode gateway config.
func (server *Server) handleClaudeCodeConfigPost(writer http.ResponseWriter, request *http.Request) {
	var body map[string]any
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": "Invalid JSON"})
		return
	}

	// The UI round-trips a stale, token-redacted accounts snapshot in this
	// payload. Settings saves must never touch accounts — account mutations
	// go through the dedicated /api/claudecode/accounts endpoints.
	delete(body, "accounts")

	_, err := config.Save(map[string]any{"claudecode": body})
	if err != nil {
		writeJSON(writer, http.StatusInternalServerError, map[string]any{"status": "error", "error": err.Error()})
		return
	}

	// Invalidate pool on config change.
	ccPoolMu.Lock()
	ccPoolInst = nil
	ccHTTPClient = nil
	ccPoolMu.Unlock()

	pub := config.GetPublicConfig()
	cc, _ := pub["claudecode"]
	writeJSON(writer, http.StatusOK, map[string]any{
		"status": "ok",
		"config": cc,
	})
}

// handleClaudeCodeAccountsList returns account snapshots with live stats.
func (server *Server) handleClaudeCodeAccountsList(writer http.ResponseWriter, _ *http.Request) {
	cfg := config.Get()
	pool, _ := getOrCreateCCPool(cfg.ClaudeCode)
	snapshots := pool.Snapshots()
	if snapshots == nil {
		snapshots = make([]claudecode.AccountSnapshot, 0)
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"status":   "ok",
		"accounts": snapshots,
	})
}

// handleClaudeCodeAccountsPost adds or updates an account in config.
func (server *Server) handleClaudeCodeAccountsPost(writer http.ResponseWriter, request *http.Request) {
	var body map[string]any
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": "Invalid JSON"})
		return
	}

	cfg := config.Get()
	// Merge account into config via Save — config.Save handles token preservation.
	accounts := make([]any, 0, len(cfg.ClaudeCode.Accounts)+1)
	for _, a := range cfg.ClaudeCode.Accounts {
		accounts = append(accounts, ccAccountToMap(a))
	}
	// Add or update by ID.
	id, _ := body["id"].(string)
	found := false
	for i, a := range accounts {
		if aMap, ok := a.(map[string]any); ok {
			if aMap["id"] == id {
				for k, v := range body {
					aMap[k] = v
				}
				accounts[i] = aMap
				found = true
				break
			}
		}
	}
	if !found {
		accounts = append(accounts, body)
	}

	ccCfg := map[string]any{
		"enabled":    cfg.ClaudeCode.Enabled,
		"baseUrl":    cfg.ClaudeCode.BaseURL,
		"mode":       cfg.ClaudeCode.Mode,
		"autoImport": cfg.ClaudeCode.AutoImport,
		"accounts":   accounts,
	}
	_, err := config.Save(map[string]any{"claudecode": ccCfg})
	if err != nil {
		writeJSON(writer, http.StatusInternalServerError, map[string]any{"status": "error", "error": err.Error()})
		return
	}

	// Sync pool.
	ccPoolMu.Lock()
	ccPoolInst = nil
	ccHTTPClient = nil
	ccPoolMu.Unlock()

	writeJSON(writer, http.StatusOK, map[string]any{"status": "ok"})
}

// handleClaudeCodeAccountDelete removes an account by ID.
func (server *Server) handleClaudeCodeAccountDelete(writer http.ResponseWriter, request *http.Request, accountID string) {
	if accountID == "" && request != nil {
		accountID = request.URL.Query().Get("id")
	}
	if accountID == "" {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": "Missing account ID"})
		return
	}
	cfg := config.Get()
	accounts := make([]any, 0, len(cfg.ClaudeCode.Accounts))
	for _, a := range cfg.ClaudeCode.Accounts {
		if a.ID != accountID {
			accounts = append(accounts, ccAccountToMap(a))
		}
	}

	ccCfg := map[string]any{
		"enabled":    cfg.ClaudeCode.Enabled,
		"baseUrl":    cfg.ClaudeCode.BaseURL,
		"mode":       cfg.ClaudeCode.Mode,
		"autoImport": cfg.ClaudeCode.AutoImport,
		"accounts":   accounts,
	}
	_, err := config.Save(map[string]any{"claudecode": ccCfg})
	if err != nil {
		writeJSON(writer, http.StatusInternalServerError, map[string]any{"status": "error", "error": err.Error()})
		return
	}

	pool, _ := getOrCreateCCPool(config.Get().ClaudeCode)
	pool.DeleteAccount(accountID)

	writeJSON(writer, http.StatusOK, map[string]any{"status": "ok"})
}

// handleClaudeCodeAccountTest validates an account token against Anthropic /v1/models.
func (server *Server) handleClaudeCodeAccountTest(writer http.ResponseWriter, request *http.Request, accountID string) {
	cfg := config.Get()

	var token string
	for _, a := range cfg.ClaudeCode.Accounts {
		if a.ID == accountID {
			token = a.Token
			break
		}
	}
	if token == "" {
		writeJSON(writer, http.StatusNotFound, map[string]any{"status": "error", "error": "account not found or has no token"})
		return
	}

	client := claudecode.NewClient(claudecode.NormalizeBaseURL(cfg.ClaudeCode.BaseURL), nil)
	if err := client.ValidateAccount(request.Context(), token); err != nil {
		writeJSON(writer, http.StatusOK, map[string]any{
			"status": "error",
			"valid":  false,
			"error":  err.Error(),
		})
		return
	}

	writeJSON(writer, http.StatusOK, map[string]any{
		"status": "ok",
		"valid":  true,
	})
}

// handleClaudeCodeAutoImport discovers local credentials and imports them.
func (server *Server) handleClaudeCodeAutoImport(writer http.ResponseWriter, _ *http.Request) {
	discovered, err := claudecode.DiscoverLocalCredentials("")
	if err != nil {
		writeJSON(writer, http.StatusInternalServerError, map[string]any{"status": "error", "error": err.Error()})
		return
	}

	cfg := config.Get()
	existingIDs := make(map[string]bool)
	for _, a := range cfg.ClaudeCode.Accounts {
		existingIDs[a.ID] = true
	}

	accounts := make([]any, 0, len(cfg.ClaudeCode.Accounts)+len(discovered))
	for _, a := range cfg.ClaudeCode.Accounts {
		accounts = append(accounts, ccAccountToMap(a))
	}

	imported := 0
	for _, d := range discovered {
		if !existingIDs[d.ID] {
			accounts = append(accounts, map[string]any{
				"id":      d.ID,
				"name":    d.Name,
				"token":   d.Token,
				"type":    d.Type,
				"enabled": d.Enabled,
			})
			imported++
		}
	}

	ccCfg := map[string]any{
		"enabled":    cfg.ClaudeCode.Enabled,
		"baseUrl":    cfg.ClaudeCode.BaseURL,
		"mode":       cfg.ClaudeCode.Mode,
		"autoImport": cfg.ClaudeCode.AutoImport,
		"accounts":   accounts,
	}
	_, err = config.Save(map[string]any{"claudecode": ccCfg})
	if err != nil {
		writeJSON(writer, http.StatusInternalServerError, map[string]any{"status": "error", "error": err.Error()})
		return
	}

	ccPoolMu.Lock()
	ccPoolInst = nil
	ccHTTPClient = nil
	ccPoolMu.Unlock()

	writeJSON(writer, http.StatusOK, map[string]any{
		"status":   "ok",
		"imported": imported,
	})
}

// routeClaudeCodeManagement routes /api/claudecode/* requests. Returns true if handled.
func (server *Server) routeClaudeCodeManagement(writer http.ResponseWriter, request *http.Request, path, method string) bool {
	switch {
	case path == "/api/claudecode/config" && method == http.MethodGet:
		server.handleClaudeCodeConfigGet(writer, request)
		return true
	case path == "/api/claudecode/config" && method == http.MethodPost:
		server.handleClaudeCodeConfigPost(writer, request)
		return true
	case path == "/api/claudecode/accounts" && method == http.MethodGet:
		server.handleClaudeCodeAccountsList(writer, request)
		return true
	case path == "/api/claudecode/accounts" && method == http.MethodPost:
		server.handleClaudeCodeAccountsPost(writer, request)
		return true
	case path == "/api/claudecode/accounts" && method == http.MethodDelete:
		id := request.URL.Query().Get("id")
		server.handleClaudeCodeAccountDelete(writer, request, id)
		return true
	case strings.HasPrefix(path, "/api/claudecode/accounts/") && strings.HasSuffix(path, "/test") && method == http.MethodPost:
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/api/claudecode/accounts/"), "/test")
		server.handleClaudeCodeAccountTest(writer, request, id)
		return true
	case strings.HasPrefix(path, "/api/claudecode/accounts/") && strings.HasSuffix(path, "/ratelimits") && method == http.MethodPost:
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/api/claudecode/accounts/"), "/ratelimits")
		server.handleClaudeCodeAccountRateLimits(writer, request, id)
		return true
	case strings.HasPrefix(path, "/api/claudecode/accounts/") && strings.HasSuffix(path, "/refresh") && method == http.MethodPost:
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/api/claudecode/accounts/"), "/refresh")
		server.handleClaudeCodeAccountRefresh(writer, request, id)
		return true
	case strings.HasPrefix(path, "/api/claudecode/accounts/") && method == http.MethodDelete:
		id := strings.TrimPrefix(path, "/api/claudecode/accounts/")
		server.handleClaudeCodeAccountDelete(writer, request, id)
		return true
	case path == "/api/claudecode/ratelimits" && method == http.MethodPost:
		server.handleClaudeCodeAllRateLimits(writer, request)
		return true
	case path == "/api/claudecode/import" && method == http.MethodPost:
		server.handleClaudeCodeAutoImport(writer, request)
		return true
	case path == "/api/claudecode/auth/start" && method == http.MethodPost:
		server.handleClaudeCodeAuthStartPost(writer, request)
		return true
	case path == "/api/claudecode/auth/status" && method == http.MethodGet:
		server.handleClaudeCodeAuthStatusGet(writer, request)
		return true
	case path == "/api/claudecode/auth/complete" && method == http.MethodPost:
		server.handleClaudeCodeAuthCompletePost(writer, request)
		return true
	case path == "/api/claudecode/auth/cancel" && method == http.MethodPost:
		server.handleClaudeCodeAuthCancelPost(writer, request)
		return true
	case (path == "/api/claudecode/models" || path == "/api/claudecode/models/fetch") && method == http.MethodPost:
		server.handleClaudeCodeModelsFetch(writer, request)
		return true
	}
	return false
}

// handleClaudeCodeModelsFetch fetches available Claude Code models via upstream API or fallback catalogue.
func (server *Server) handleClaudeCodeModelsFetch(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		Token   string `json:"token"`
		BaseURL string `json:"baseUrl"`
	}
	if request.Body != nil {
		_ = json.NewDecoder(request.Body).Decode(&body)
	}

	token := strings.TrimSpace(body.Token)
	baseURL := strings.TrimSpace(body.BaseURL)

	cfg := config.Get()
	if baseURL == "" && cfg.ClaudeCode.BaseURL != "" {
		baseURL = cfg.ClaudeCode.BaseURL
	}

	// If token not provided in request payload, attempt to resolve from active accounts in pool or config
	if token == "" {
		for _, a := range cfg.ClaudeCode.Accounts {
			if a.Enabled && strings.TrimSpace(a.Token) != "" {
				token = strings.TrimSpace(a.Token)
				break
			}
		}
	}

	models, err := claudecode.DefaultClient.FetchModels(request.Context(), token, baseURL)
	if err != nil && len(models) == 0 {
		writeJSON(writer, http.StatusInternalServerError, map[string]any{
			"status": "error",
			"error":  err.Error(),
		})
		return
	}

	if models == nil {
		models = []claudecode.DiscoveredModel{}
	}

	writeJSON(writer, http.StatusOK, map[string]any{
		"status": "ok",
		"models": models,
		"total":  len(models),
	})
}

// handleClaudeCodeAccountRateLimits queries active rate limits from Anthropic for a specific account.
func (server *Server) handleClaudeCodeAccountRateLimits(writer http.ResponseWriter, request *http.Request, accountID string) {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": "account_id is required"})
		return
	}

	cfg := config.Get()
	pool, _ := server.getOrCreateCCPool(cfg.ClaudeCode)
	if pool == nil {
		writeJSON(writer, http.StatusInternalServerError, map[string]any{"status": "error", "error": "pool not initialized"})
		return
	}

	var token string
	if acc, ok := pool.GetAccount(accountID); ok {
		token = acc.Token
	}
	if token == "" {
		for _, a := range cfg.ClaudeCode.Accounts {
			if a.ID == accountID {
				token = a.Token
				break
			}
		}
	}

	if token == "" {
		writeJSON(writer, http.StatusNotFound, map[string]any{"status": "error", "error": "account not found or has no token"})
		return
	}

	client := claudecode.NewClient(claudecode.NormalizeBaseURL(cfg.ClaudeCode.BaseURL), nil)
	rl, err := client.FetchRateLimits(request.Context(), token)
	if err != nil && rl.RequestsLimit == 0 && rl.TokensLimit == 0 {
		writeJSON(writer, http.StatusInternalServerError, map[string]any{
			"status": "error",
			"error":  err.Error(),
		})
		return
	}

	pool.UpdateAccountRateLimits(accountID, rl)

	writeJSON(writer, http.StatusOK, map[string]any{
		"status":      "ok",
		"account_id":  accountID,
		"rate_limits": rl,
	})
}

// handleClaudeCodeAccountRefresh forces an immediate OAuth token refresh for an account.
func (server *Server) handleClaudeCodeAccountRefresh(writer http.ResponseWriter, _ *http.Request, accountID string) {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": "account_id is required"})
		return
	}

	cfg := config.Get()
	pool, _ := server.getOrCreateCCPool(cfg.ClaudeCode)
	if pool == nil {
		writeJSON(writer, http.StatusInternalServerError, map[string]any{"status": "error", "error": "pool not initialized"})
		return
	}

	if err := pool.RefreshAccountToken(accountID); err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]any{
			"status": "error",
			"error":  err.Error(),
		})
		return
	}

	if refreshedAcc, ok := pool.GetAccount(accountID); ok {
		server.syncRefreshedAccountToConfig(accountID, refreshedAcc.Token, refreshedAcc.RefreshToken, refreshedAcc.ExpiresAt)
	}

	writeJSON(writer, http.StatusOK, map[string]any{
		"status":     "ok",
		"account_id": accountID,
	})
}

// handleClaudeCodeAllRateLimits probes active rate limits for all enabled accounts.
func (server *Server) handleClaudeCodeAllRateLimits(writer http.ResponseWriter, request *http.Request) {
	cfg := config.Get()
	pool, _ := server.getOrCreateCCPool(cfg.ClaudeCode)
	if pool == nil {
		writeJSON(writer, http.StatusInternalServerError, map[string]any{"status": "error", "error": "pool not initialized"})
		return
	}

	accounts := pool.ListAccounts()
	client := claudecode.NewClient(claudecode.NormalizeBaseURL(cfg.ClaudeCode.BaseURL), nil)

	results := make(map[string]claudecode.RateLimits)
	for _, acc := range accounts {
		if !acc.Enabled || acc.Token == "" {
			continue
		}
		if rl, err := client.FetchRateLimits(request.Context(), acc.Token); err == nil {
			pool.UpdateAccountRateLimits(acc.ID, rl)
			results[acc.ID] = rl
		}
	}

	writeJSON(writer, http.StatusOK, map[string]any{
		"status":      "ok",
		"rate_limits": results,
	})
}

