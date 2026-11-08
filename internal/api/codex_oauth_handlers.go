package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"antigravity-go-proxy/internal/auth"
	"antigravity-go-proxy/internal/codex"
	"antigravity-go-proxy/internal/config"
)

// handleCodexAuthStart starts a ChatGPT login. The session's mode is the one it
// runs in: a loopback request falls back to manual when the callback port is
// taken.
func (server *Server) handleCodexAuthStart(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		Mode string `json:"mode"`
	}
	if request.Body != nil {
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			writeCodexError(writer, http.StatusBadRequest, "Invalid JSON")
			return
		}
	}
	session, err := server.codexOAuthMgr.StartAuthSession(body.Mode)
	if err != nil {
		slog.Error("failed to start Codex auth session", "mode", body.Mode, "error", err)
		writeCodexError(writer, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"status":     "ok",
		"session_id": session.ID,
		"auth_url":   session.AuthURL,
		"mode":       session.Mode,
	})
}

// handleCodexAuthStatus reports a login session. A loopback login registers
// its account before the session turns "completed", so a poll that sees that
// status finds the account in the list.
func (server *Server) handleCodexAuthStatus(writer http.ResponseWriter, request *http.Request) {
	sessionID := strings.TrimSpace(request.URL.Query().Get("session_id"))
	if sessionID == "" {
		writeCodexError(writer, http.StatusBadRequest, "missing session_id parameter")
		return
	}
	snap, ok := server.codexOAuthMgr.GetSession(sessionID)
	if !ok {
		writeCodexError(writer, http.StatusNotFound, "auth session not found or expired")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"status": "ok", "session": snap})
}

// handleCodexAuthComplete finishes a login from the URL the browser was
// redirected to (or the bare code) and registers the account.
func (server *Server) handleCodexAuthComplete(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		SessionID   string `json:"session_id"`
		RedirectURL string `json:"redirect_url"`
	}
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		writeCodexError(writer, http.StatusBadRequest, "Invalid JSON")
		return
	}
	if strings.TrimSpace(body.SessionID) == "" || strings.TrimSpace(body.RedirectURL) == "" {
		writeCodexError(writer, http.StatusBadRequest, "session_id and redirect_url are required")
		return
	}
	result, err := server.codexOAuthMgr.CompleteManualAuth(strings.TrimSpace(body.SessionID), body.RedirectURL)
	if err != nil {
		slog.Error("Codex manual auth completion failed", "session_id", body.SessionID, "error", err)
		writeCodexError(writer, http.StatusBadRequest, err.Error())
		return
	}
	account, err := server.registerCodexAccount(result)
	if err != nil {
		writeCodexError(writer, http.StatusInternalServerError, "login succeeded but the account was not saved: "+err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"status": "ok",
		"account": map[string]any{
			"id":       account.ID,
			"email":    account.Email,
			"planType": account.PlanType,
		},
	})
}

// handleCodexAuthCancel ends a pending login and frees its callback port.
func (server *Server) handleCodexAuthCancel(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		writeCodexError(writer, http.StatusBadRequest, "Invalid JSON")
		return
	}
	if strings.TrimSpace(body.SessionID) == "" {
		writeCodexError(writer, http.StatusBadRequest, "session_id is required")
		return
	}
	if err := server.codexOAuthMgr.CancelSession(strings.TrimSpace(body.SessionID)); err != nil {
		writeCodexError(writer, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"status": "ok"})
}

// registerCodexLogin is the OAuth manager's OnComplete hook for loopback
// logins. Nothing waits on its result, so a failed save is logged; the
// session still completes, since its tokens exist either way.
func (server *Server) registerCodexLogin(result *auth.CodexAccountResult) {
	if _, err := server.registerCodexAccount(result); err != nil {
		slog.Error("Codex login succeeded but the account was not saved", "email", result.Email, "error", err)
	}
}

// registerCodexAccount saves a ChatGPT login into codex.accounts and turns the
// gateway on. A login of an account already configured replaces its tokens and
// enables it, keeping its ID, name and priority. Otherwise a new account is
// added as codex-<email>, enabled, at priority 1.
func (server *Server) registerCodexAccount(result *auth.CodexAccountResult) (codex.AccountConfig, error) {
	if result == nil {
		return codex.AccountConfig{}, errors.New("no login result")
	}
	server.codexMu.Lock()
	defer server.codexMu.Unlock()

	accounts := slices.Clone(config.Get().Codex.Accounts)
	i := codexLoginMatch(accounts, result)
	if i < 0 {
		accounts = append(accounts, codex.AccountConfig{
			ID:       codexLoginID(accounts, result),
			Priority: 1,
		})
		i = len(accounts) - 1
	}
	acc := &accounts[i]
	acc.Token = result.AccessToken
	acc.RefreshToken = result.RefreshToken
	acc.IDToken = result.IDToken
	acc.ExpiresAt = ""
	if !result.ExpiresAt.IsZero() {
		acc.ExpiresAt = result.ExpiresAt.UTC().Format(time.RFC3339)
	}
	if result.Email != "" {
		acc.Email = result.Email
	}
	if result.AccountID != "" {
		acc.AccountID = result.AccountID
	}
	if result.PlanType != "" {
		acc.PlanType = result.PlanType
	}
	acc.Source = "oauth"
	acc.Enabled = true

	if err := saveCodexSection(map[string]any{"enabled": true, "accounts": accounts}); err != nil {
		return codex.AccountConfig{}, err
	}
	slog.Info("Codex account registered", "id", acc.ID, "email", acc.Email, "plan_type", acc.PlanType)
	return *acc, nil
}

// codexLoginMatch finds the configured account a login belongs to: the same
// ChatGPT account ID. On a Team or Business plan that ID names the workspace
// and every member shares it, so a login whose email differs from the stored
// one is a different member and gets an account of its own. Returns -1 when
// no account matches.
func codexLoginMatch(accounts []codex.AccountConfig, result *auth.CodexAccountResult) int {
	if result.AccountID == "" {
		return -1
	}
	return slices.IndexFunc(accounts, func(a codex.AccountConfig) bool {
		if a.AccountID != result.AccountID {
			return false
		}
		return a.Email == "" || result.Email == "" || strings.EqualFold(a.Email, result.Email)
	})
}

// codexLoginID picks the ID of a newly logged-in account: codex-<email>, or
// codex-<accountId> without an email. The same email in another workspace
// collides with the first account's ID, so the account ID's first eight
// characters are appended; a counter settles anything still taken.
func codexLoginID(accounts []codex.AccountConfig, result *auth.CodexAccountResult) string {
	taken := func(id string) bool { return codexAccountIndex(accounts, id) >= 0 }
	base := "codex-account"
	switch {
	case result.Email != "":
		base = "codex-" + result.Email
	case result.AccountID != "":
		base = "codex-" + result.AccountID
	}
	if !taken(base) {
		return base
	}
	if result.AccountID != "" {
		suffix := result.AccountID
		if len(suffix) > 8 {
			suffix = suffix[:8]
		}
		if id := base + "-" + suffix; !taken(id) {
			return id
		}
	}
	for n := 2; ; n++ {
		if id := base + "-" + strconv.Itoa(n); !taken(id) {
			return id
		}
	}
}

// writeCodexError writes the management API's error shape.
func writeCodexError(writer http.ResponseWriter, status int, message string) {
	writeJSON(writer, status, map[string]any{"status": "error", "error": message})
}
