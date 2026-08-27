package api

import (
	"encoding/json"
	"net/http"
	"time"

	"antigravity-go-proxy/internal/auth"
	"antigravity-go-proxy/internal/claudecode"
	"antigravity-go-proxy/internal/config"
)

// handleClaudeCodeAuthStartPost starts a new Claude Code OAuth authorization session.
func (server *Server) handleClaudeCodeAuthStartPost(writer http.ResponseWriter, request *http.Request) {
	if server.claudeCodeOAuthMgr == nil {
		writeJSON(writer, http.StatusInternalServerError, map[string]any{
			"status": "error",
			"error":  "Claude Code OAuth manager not initialized",
		})
		return
	}

	mode := "loopback"
	if request.Body != nil {
		var body map[string]string
		_ = json.NewDecoder(request.Body).Decode(&body)
		if m, ok := body["mode"]; ok && m != "" {
			mode = m
		}
	}

	session, err := server.claudeCodeOAuthMgr.StartAuthSession(mode)
	if err != nil {
		writeJSON(writer, http.StatusInternalServerError, map[string]any{
			"status": "error",
			"error":  err.Error(),
		})
		return
	}

	writeJSON(writer, http.StatusOK, map[string]any{
		"status":          "ok",
		"session_id":      session.ID,
		"auth_url":        session.AuthURL,
		"manual_auth_url": session.ManualAuthURL,
		"port":            session.Port,
		"mode":            mode,
	})
}

// handleClaudeCodeAuthStatusGet returns the status of an ongoing OAuth session.
func (server *Server) handleClaudeCodeAuthStatusGet(writer http.ResponseWriter, request *http.Request) {
	if server.claudeCodeOAuthMgr == nil {
		writeJSON(writer, http.StatusInternalServerError, map[string]any{
			"status": "error",
			"error":  "Claude Code OAuth manager not initialized",
		})
		return
	}

	sessionID := request.URL.Query().Get("session_id")
	if sessionID == "" {
		writeJSON(writer, http.StatusBadRequest, map[string]any{
			"status": "error",
			"error":  "missing session_id parameter",
		})
		return
	}

	session, exists := server.claudeCodeOAuthMgr.GetSession(sessionID)
	if !exists || session == nil {
		writeJSON(writer, http.StatusNotFound, map[string]any{
			"status": "expired",
			"error":  "session not found or expired",
		})
		return
	}

	snap := session.Snapshot()

	if snap.Status == "completed" && snap.Account != nil {
		server.registerAuthenticatedClaudeCodeAccount(snap.Account)

		writeJSON(writer, http.StatusOK, map[string]any{
			"status": "completed",
			"account": map[string]any{
				"id":           "cc-" + snap.Account.Email,
				"email":        snap.Account.Email,
				"account_uuid": snap.Account.AccountUUID,
				"expires_at":   snap.Account.ExpiresAt,
			},
		})
		return
	}

	if snap.Status == "failed" {
		writeJSON(writer, http.StatusOK, map[string]any{
			"status": "failed",
			"error":  snap.Error,
		})
		return
	}

	if snap.Status == "expired" {
		writeJSON(writer, http.StatusOK, map[string]any{
			"status": "expired",
			"error":  snap.Error,
		})
		return
	}

	writeJSON(writer, http.StatusOK, map[string]any{
		"status": "pending",
	})
}

// handleClaudeCodeAuthCompletePost completes authentication via manually entered authorization code.
func (server *Server) handleClaudeCodeAuthCompletePost(writer http.ResponseWriter, request *http.Request) {
	if server.claudeCodeOAuthMgr == nil {
		writeJSON(writer, http.StatusInternalServerError, map[string]any{
			"status": "error",
			"error":  "Claude Code OAuth manager not initialized",
		})
		return
	}

	var body struct {
		SessionID string `json:"session_id"`
		Code      string `json:"code"`
	}

	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]any{
			"status": "error",
			"error":  "Invalid JSON request body",
		})
		return
	}

	if body.SessionID == "" || body.Code == "" {
		writeJSON(writer, http.StatusBadRequest, map[string]any{
			"status": "error",
			"error":  "session_id and code are required",
		})
		return
	}

	account, err := server.claudeCodeOAuthMgr.CompleteManualAuth(body.SessionID, body.Code)
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]any{
			"status": "error",
			"error":  err.Error(),
		})
		return
	}

	server.registerAuthenticatedClaudeCodeAccount(account)

	writeJSON(writer, http.StatusOK, map[string]any{
		"status": "ok",
		"account": map[string]any{
			"id":           "cc-" + account.Email,
			"email":        account.Email,
			"account_uuid": account.AccountUUID,
			"expires_at":   account.ExpiresAt,
		},
	})
}

// handleClaudeCodeAuthCancelPost cancels a pending OAuth session.
func (server *Server) handleClaudeCodeAuthCancelPost(writer http.ResponseWriter, request *http.Request) {
	if server.claudeCodeOAuthMgr == nil {
		writeJSON(writer, http.StatusInternalServerError, map[string]any{
			"status": "error",
			"error":  "Claude Code OAuth manager not initialized",
		})
		return
	}

	var body struct {
		SessionID string `json:"session_id"`
	}
	_ = json.NewDecoder(request.Body).Decode(&body)

	if body.SessionID != "" {
		server.claudeCodeOAuthMgr.CancelSession(body.SessionID)
	}

	writeJSON(writer, http.StatusOK, map[string]any{
		"status": "ok",
	})
}

// registerAuthenticatedClaudeCodeAccount registers and persists the new OAuth account.
func (server *Server) registerAuthenticatedClaudeCodeAccount(accResult *auth.ClaudeCodeAccountResult) {
	if accResult == nil {
		return
	}

	accountID := "cc-" + accResult.Email
	if accResult.Email == "" {
		accountID = "cc-" + accResult.AccountUUID
	}
	name := "Claude Code (" + accResult.Email + ")"
	if accResult.Email == "" {
		name = "Claude Code"
	}

	exp := accResult.ExpiresAt

	// 1. Update config.ClaudeCode.Accounts
	cfg := config.Get()
	var accountsList []any
	found := false

	for _, a := range cfg.ClaudeCode.Accounts {
		if a.ID == accountID {
			accountsList = append(accountsList, map[string]any{
				"id":               accountID,
				"name":             name,
				"token":            accResult.AccessToken,
				"refreshToken":     accResult.RefreshToken,
				"expiresAt":        exp.Format(time.RFC3339),
				"email":            accResult.Email,
				"accountUuid":      accResult.AccountUUID,
				"organizationUuid": accResult.OrganizationUUID,
				"type":             "oauth",
				"priority":         a.Priority,
				"enabled":          true,
				"source":           "oauth",
			})
			found = true
		} else {
			accountsList = append(accountsList, map[string]any{
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
			})
		}
	}

	if !found {
		accountsList = append(accountsList, map[string]any{
			"id":               accountID,
			"name":             name,
			"token":            accResult.AccessToken,
			"refreshToken":     accResult.RefreshToken,
			"expiresAt":        exp.Format(time.RFC3339),
			"email":            accResult.Email,
			"accountUuid":      accResult.AccountUUID,
			"organizationUuid": accResult.OrganizationUUID,
			"type":             "oauth",
			"priority":         1,
			"enabled":          true,
			"source":           "oauth",
		})
	}

	ccCfg := map[string]any{
		"enabled":    true,
		"baseUrl":    cfg.ClaudeCode.BaseURL,
		"mode":       cfg.ClaudeCode.Mode,
		"autoImport": cfg.ClaudeCode.AutoImport,
		"accounts":   accountsList,
	}

	_, _ = config.Save(map[string]any{"claudecode": ccCfg})

	// 2. Add or update in Claude Code Pool
	pool, _ := getOrCreateCCPool(config.Get().ClaudeCode)
	if pool != nil {
		if server.claudeCodeOAuthMgr != nil {
			pool.SetTokenRefresher(func(refreshToken string) (string, string, int, error) {
				resp, err := server.claudeCodeOAuthMgr.RefreshToken(refreshToken)
				if err != nil {
					return "", "", 0, err
				}
				return resp.AccessToken, resp.RefreshToken, resp.ExpiresIn, nil
			})
		}

		pool.AddOrUpdateAccount(claudecode.AccountConfig{
			ID:               accountID,
			Name:             name,
			Token:            accResult.AccessToken,
			RefreshToken:     accResult.RefreshToken,
			ExpiresAt:        &exp,
			Email:            accResult.Email,
			AccountUUID:      accResult.AccountUUID,
			OrganizationUUID: accResult.OrganizationUUID,
			Type:             "oauth",
			Priority:         1,
			Enabled:          true,
			Source:           "oauth",
		})
		_ = pool.SaveStoredAccounts()
	}
}
