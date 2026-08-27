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

// handleClaudeCodeConfigPost saves ClaudeCode gateway config.
func (server *Server) handleClaudeCodeConfigPost(writer http.ResponseWriter, request *http.Request) {
	var body map[string]any
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"status": "error", "error": "Invalid JSON"})
		return
	}

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
		acc := map[string]any{
			"id":       a.ID,
			"name":     a.Name,
			"token":    a.Token,
			"type":     a.Type,
			"priority": a.Priority,
			"enabled":  a.Enabled,
		}
		accounts = append(accounts, acc)
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
func (server *Server) handleClaudeCodeAccountDelete(writer http.ResponseWriter, _ *http.Request, accountID string) {
	cfg := config.Get()
	accounts := make([]any, 0, len(cfg.ClaudeCode.Accounts))
	for _, a := range cfg.ClaudeCode.Accounts {
		if a.ID != accountID {
			acc := map[string]any{
				"id":       a.ID,
				"name":     a.Name,
				"token":    a.Token,
				"type":     a.Type,
				"priority": a.Priority,
				"enabled":  a.Enabled,
			}
			accounts = append(accounts, acc)
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
		acc := map[string]any{
			"id":       a.ID,
			"name":     a.Name,
			"token":    a.Token,
			"type":     a.Type,
			"priority": a.Priority,
			"enabled":  a.Enabled,
		}
		accounts = append(accounts, acc)
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
	case strings.HasPrefix(path, "/api/claudecode/accounts/") && strings.HasSuffix(path, "/test") && method == http.MethodPost:
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/api/claudecode/accounts/"), "/test")
		server.handleClaudeCodeAccountTest(writer, request, id)
		return true
	case strings.HasPrefix(path, "/api/claudecode/accounts/") && method == http.MethodDelete:
		id := strings.TrimPrefix(path, "/api/claudecode/accounts/")
		server.handleClaudeCodeAccountDelete(writer, request, id)
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
	}
	return false
}
