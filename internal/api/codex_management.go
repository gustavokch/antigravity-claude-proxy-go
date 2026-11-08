package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"antigravity-go-proxy/internal/codex"
	"antigravity-go-proxy/internal/config"
)

// routeCodexManagement routes /api/codex/* requests. Returns true if handled.
func (server *Server) routeCodexManagement(writer http.ResponseWriter, request *http.Request, path, method string) bool {
	const accountPrefix = "/api/codex/accounts/"
	switch {
	case path == "/api/codex/config" && method == http.MethodGet:
		server.handleCodexConfigGet(writer, request)
	case path == "/api/codex/config" && method == http.MethodPost:
		server.handleCodexConfigPost(writer, request)
	case path == "/api/codex/accounts" && method == http.MethodGet:
		server.handleCodexAccountsList(writer, request)
	case path == "/api/codex/accounts" && method == http.MethodPost:
		server.handleCodexAccountUpdate(writer, request)
	case strings.HasPrefix(path, accountPrefix) && strings.HasSuffix(path, "/refresh") && method == http.MethodPost:
		server.handleCodexAccountRefresh(writer, request, strings.TrimSuffix(strings.TrimPrefix(path, accountPrefix), "/refresh"))
	case strings.HasPrefix(path, accountPrefix) && strings.HasSuffix(path, "/test") && method == http.MethodPost:
		server.handleCodexAccountTest(writer, request, strings.TrimSuffix(strings.TrimPrefix(path, accountPrefix), "/test"))
	case strings.HasPrefix(path, accountPrefix) && method == http.MethodDelete:
		server.handleCodexAccountDelete(writer, request, strings.TrimPrefix(path, accountPrefix))
	case path == "/api/codex/auth/start" && method == http.MethodPost:
		server.handleCodexAuthStart(writer, request)
	case path == "/api/codex/auth/status" && method == http.MethodGet:
		server.handleCodexAuthStatus(writer, request)
	case path == "/api/codex/auth/complete" && method == http.MethodPost:
		server.handleCodexAuthComplete(writer, request)
	case path == "/api/codex/auth/cancel" && method == http.MethodPost:
		server.handleCodexAuthCancel(writer, request)
	case path == "/api/codex/models/fetch" && method == http.MethodPost:
		server.handleCodexModelsFetch(writer, request)
	default:
		return false
	}
	return true
}

// handleCodexConfigGet returns the redacted codex config section.
func (server *Server) handleCodexConfigGet(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]any{
		"status": "ok",
		"config": config.GetPublicConfig()["codex"],
	})
}

// handleCodexConfigPost saves the gateway settings: enabled, baseUrl,
// allowlist and identity.
func (server *Server) handleCodexConfigPost(writer http.ResponseWriter, request *http.Request) {
	var body map[string]any
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil || body == nil {
		writeCodexError(writer, http.StatusBadRequest, "Invalid JSON")
		return
	}
	// The UI holds a token-redacted copy of the accounts. Settings saves must
	// never touch them: logins and the account routes own that list.
	delete(body, "accounts")
	if err := validateIdentityOverrides(map[string]any{"codex": body}); err != nil {
		writeCodexError(writer, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateCodexSection(body); err != nil {
		writeCodexError(writer, http.StatusBadRequest, err.Error())
		return
	}
	if err := saveCodexSection(body); err != nil {
		writeCodexError(writer, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"status": "ok"})
}

// validateCodexSection checks the settings of a codex config update that
// would otherwise save fine and then misroute or fail every request: enabled
// must be a boolean, baseUrl an absolute http(s) URL (empty restores the
// default), and the allowlist must decode, with every entry naming an ID and
// no ID or alias claimed twice, case-insensitively, since ResolveModel matches
// that way and would silently pick the first. The identity is checked by
// validateIdentityOverrides.
func validateCodexSection(section map[string]any) error {
	if v, ok := section["enabled"]; ok && v != nil {
		if _, isBool := v.(bool); !isBool {
			return errors.New("codex: enabled must be a boolean")
		}
	}
	if v, ok := section["baseUrl"]; ok && v != nil {
		raw, isString := v.(string)
		if !isString {
			return errors.New("codex: baseUrl must be a string")
		}
		if raw = strings.TrimSpace(raw); raw != "" {
			u, err := url.Parse(raw)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return fmt.Errorf("codex: baseUrl %q must be an absolute http or https URL", raw)
			}
		}
	}
	var allowlist []codex.ModelConfig
	if err := validateJSONSection(section, "allowlist", "codex", &allowlist); err != nil {
		return err
	}
	claimed := make(map[string]string, len(allowlist))
	for i, m := range allowlist {
		id := strings.TrimSpace(m.ID)
		if id == "" {
			return fmt.Errorf("codex: allowlist entry %d has no id", i+1)
		}
		for _, name := range append([]string{id}, m.ExpandAliases()...) {
			key := strings.ToLower(name)
			if owner, dup := claimed[key]; dup {
				return fmt.Errorf("codex: allowlist name %q is used by both %q and %q", name, owner, id)
			}
			claimed[key] = id
		}
	}
	return nil
}

// handleCodexAccountsList returns every account's live pool state.
func (server *Server) handleCodexAccountsList(writer http.ResponseWriter, _ *http.Request) {
	pool, _ := server.codexPoolFor(config.Get().Codex)
	writeJSON(writer, http.StatusOK, map[string]any{
		"status":   "ok",
		"accounts": pool.Snapshots(),
	})
}

// handleCodexAccountUpdate changes an account's enabled flag, priority or
// name. Accounts are only created by a login, so an unknown id is an error and
// credentials cannot be set here.
func (server *Server) handleCodexAccountUpdate(writer http.ResponseWriter, request *http.Request) {
	var body struct {
		ID       string  `json:"id"`
		Enabled  *bool   `json:"enabled"`
		Priority *int    `json:"priority"`
		Name     *string `json:"name"`
	}
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		writeCodexError(writer, http.StatusBadRequest, "Invalid JSON: "+err.Error())
		return
	}
	id := strings.TrimSpace(body.ID)
	if id == "" {
		writeCodexError(writer, http.StatusBadRequest, "id is required")
		return
	}
	if body.Priority != nil && *body.Priority < 0 {
		writeCodexError(writer, http.StatusBadRequest, "priority must not be negative")
		return
	}

	server.codexMu.Lock()
	defer server.codexMu.Unlock()
	accounts := slices.Clone(config.Get().Codex.Accounts)
	i := codexAccountIndex(accounts, id)
	if i < 0 {
		writeCodexError(writer, http.StatusNotFound, "account not found")
		return
	}
	if body.Enabled != nil {
		accounts[i].Enabled = *body.Enabled
	}
	if body.Priority != nil {
		accounts[i].Priority = *body.Priority
	}
	if body.Name != nil {
		accounts[i].Name = strings.TrimSpace(*body.Name)
	}
	if err := saveCodexSection(map[string]any{"accounts": accounts}); err != nil {
		writeCodexError(writer, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"status": "ok"})
}

// handleCodexAccountDelete removes an account. Its refresh token is not
// revoked: OpenAI offers no revocation for this client.
func (server *Server) handleCodexAccountDelete(writer http.ResponseWriter, _ *http.Request, accountID string) {
	server.codexMu.Lock()
	defer server.codexMu.Unlock()
	accounts := slices.Clone(config.Get().Codex.Accounts)
	i := codexAccountIndex(accounts, accountID)
	if i < 0 {
		writeCodexError(writer, http.StatusNotFound, "account not found")
		return
	}
	accounts = slices.Delete(accounts, i, i+1)
	if err := saveCodexSection(map[string]any{"accounts": accounts}); err != nil {
		writeCodexError(writer, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"status": "ok"})
}

// handleCodexAccountRefresh exchanges the account's refresh token now. A
// revoked grant marks the account needs_login, which the list then shows.
func (server *Server) handleCodexAccountRefresh(writer http.ResponseWriter, request *http.Request, accountID string) {
	pool, _ := server.codexPoolFor(config.Get().Codex)
	acc, err := pool.ForceRefresh(request.Context(), accountID)
	if err != nil {
		writeCodexAccountError(writer, err)
		return
	}
	var expiresAt any
	if !acc.ExpiresAt.IsZero() {
		expiresAt = acc.ExpiresAt.UTC().Format(time.RFC3339)
	}
	writeJSON(writer, http.StatusOK, map[string]any{"status": "ok", "expiresAt": expiresAt})
}

// handleCodexAccountTest checks that the account can reach the backend by
// reading its model catalog, refreshing the token first when it is close to
// expiry. The usage windows the reply carries are recorded on the account.
func (server *Server) handleCodexAccountTest(writer http.ResponseWriter, request *http.Request, accountID string) {
	cfg := config.Get().Codex
	pool, client := server.codexPoolFor(cfg)
	models, err := server.fetchCodexModels(request, pool, client, cfg, accountID)
	if err != nil {
		writeCodexAccountError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"status": "ok", "models": len(models)})
}

// handleCodexModelsFetch lists the models the first enabled account's catalog
// offers, for the allowlist editor.
func (server *Server) handleCodexModelsFetch(writer http.ResponseWriter, request *http.Request) {
	cfg := config.Get().Codex
	i := slices.IndexFunc(cfg.Accounts, func(a codex.AccountConfig) bool { return a.Enabled })
	if i < 0 {
		writeCodexError(writer, http.StatusBadRequest, "no enabled Codex account: log in with a ChatGPT account first")
		return
	}
	pool, client := server.codexPoolFor(cfg)
	models, err := server.fetchCodexModels(request, pool, client, cfg, cfg.Accounts[i].ID)
	if err != nil {
		writeCodexAccountError(writer, err)
		return
	}
	if models == nil {
		models = []codex.DiscoveredModel{}
	}
	writeJSON(writer, http.StatusOK, map[string]any{"status": "ok", "models": models})
}

// fetchCodexModels reads one account's model catalog with a fresh token and
// records the usage windows its reply carries.
func (server *Server) fetchCodexModels(request *http.Request, pool *codex.AccountPool, client *codex.Client, cfg codex.Config, accountID string) ([]codex.DiscoveredModel, error) {
	acc, err := pool.EnsureFresh(request.Context(), accountID)
	if err != nil {
		return nil, err
	}
	models, header, err := client.FetchModels(request.Context(), acc.Token, acc.AccountID, cfg.Identity)
	if header != nil {
		if q, ok := codex.ParseQuota(header, time.Now()); ok {
			pool.UpdateQuota(acc.ID, q)
		}
	}
	return models, err
}

// writeCodexAccountError reports a failed account operation: an unknown
// account is a 404, anything else (a refused grant, a token endpoint or
// backend failure) a bad gateway, with the cause in the message.
func writeCodexAccountError(writer http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	if errors.Is(err, codex.ErrAccountNotFound) {
		status = http.StatusNotFound
	}
	writeCodexError(writer, status, err.Error())
}
