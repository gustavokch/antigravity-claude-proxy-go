package api

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"antigravity-go-proxy/internal/auth"
	"antigravity-go-proxy/internal/config"
)

// handleKimiAuthStartPost starts a new Kimi Code device-flow login.
func (server *Server) handleKimiAuthStartPost(writer http.ResponseWriter, request *http.Request) {
	if server.kimiOAuthMgr == nil {
		slog.Error("Kimi auth start called but OAuth manager not initialized")
		writeJSON(writer, http.StatusInternalServerError, map[string]any{
			"status": "error",
			"error":  "Kimi OAuth manager not initialized",
		})
		return
	}

	// The body is ignored: there are no parameters (no region concept).
	session, err := server.kimiOAuthMgr.StartDeviceAuth(request.Context())
	if err != nil {
		slog.Error("failed to start Kimi auth session", "error", err)
		writeJSON(writer, http.StatusBadGateway, map[string]any{
			"status": "error",
			"error":  err.Error(),
		})
		return
	}

	slog.Info("Kimi Code auth session created via API", "session_id", session.ID)

	writeJSON(writer, http.StatusOK, map[string]any{
		"status":                    "ok",
		"session_id":                session.ID,
		"user_code":                 session.Device.UserCode,
		"verification_uri":          session.Device.VerificationURI,
		"verification_uri_complete": session.Device.VerificationURIComplete,
		"expires_in":                session.Device.ExpiresIn,
		"interval":                  session.Device.Interval,
	})
}

// handleKimiAuthStatusGet returns the status of an ongoing device-flow login.
func (server *Server) handleKimiAuthStatusGet(writer http.ResponseWriter, request *http.Request) {
	if server.kimiOAuthMgr == nil {
		writeJSON(writer, http.StatusInternalServerError, map[string]any{
			"status": "error",
			"error":  "Kimi OAuth manager not initialized",
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

	session, exists := server.kimiOAuthMgr.GetSession(sessionID)
	if !exists || session == nil {
		writeJSON(writer, http.StatusNotFound, map[string]any{
			"status": "expired",
			"error":  "session not found or expired",
		})
		return
	}

	snap := session.Snapshot()

	if snap.Status == "completed" {
		if session.ClaimCompletion() {
			if err := server.registerAuthenticatedKimiOAuth(snap); err != nil {
				writeJSON(writer, http.StatusInternalServerError, map[string]any{
					"status": "error",
					"error":  "Failed to save Kimi login: " + err.Error(),
				})
				return
			}
		}
		account := map[string]any{"expires_at": ""}
		if snap.Token != nil {
			account["expires_at"] = snap.Token.ExpiresAt.Format(time.RFC3339)
		}
		if snap.User != nil {
			account["email"] = snap.User.Email
			account["user_id"] = snap.User.UserID
			account["nickname"] = snap.User.Nickname
		} else {
			account["email"] = ""
			account["user_id"] = ""
			account["nickname"] = ""
		}
		writeJSON(writer, http.StatusOK, map[string]any{
			"status":  "completed",
			"account": account,
		})
		return
	}

	switch snap.Status {
	case "expired", "denied", "cancelled", "error":
		writeJSON(writer, http.StatusOK, map[string]any{
			"status": snap.Status,
			"error":  snap.Error,
		})
		return
	}

	writeJSON(writer, http.StatusOK, map[string]any{
		"status": "pending",
	})
}

// handleKimiAuthCancelPost cancels a pending device-flow login.
func (server *Server) handleKimiAuthCancelPost(writer http.ResponseWriter, request *http.Request) {
	if server.kimiOAuthMgr == nil {
		writeJSON(writer, http.StatusInternalServerError, map[string]any{
			"status": "error",
			"error":  "Kimi OAuth manager not initialized",
		})
		return
	}

	var body struct {
		SessionID string `json:"session_id"`
	}
	if request.Body != nil {
		_ = json.NewDecoder(request.Body).Decode(&body)
	}
	if body.SessionID != "" {
		server.kimiOAuthMgr.CancelSession(body.SessionID)
	}
	writeJSON(writer, http.StatusOK, map[string]any{"status": "ok"})
}

// handleKimiAuthLogoutPost clears the stored Kimi Code credential.
func (server *Server) handleKimiAuthLogoutPost(writer http.ResponseWriter, request *http.Request) {
	server.kimiRefreshMu.Lock()
	err := server.saveKimiLocked(map[string]any{"oauth": nil})
	server.kimiUnsaved, server.kimiUnsavedFrom = nil, ""
	server.kimiRefreshMu.Unlock()
	if err != nil {
		writeJSON(writer, http.StatusInternalServerError, map[string]any{
			"status": "error",
			"error":  "Failed to save config: " + err.Error(),
		})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"status": "ok",
		"config": config.GetPublicConfig()["kimi"],
	})
}

// registerAuthenticatedKimiOAuth persists a completed device-flow login.
func (server *Server) registerAuthenticatedKimiOAuth(snap auth.KimiAuthSessionSnapshot) error {
	if snap.Token == nil {
		return nil
	}
	o := &config.KimiOAuthConfig{
		Token:        snap.Token.AccessToken,
		RefreshToken: snap.Token.RefreshToken,
		ExpiresAt:    &snap.Token.ExpiresAt,
		OAuthHost:    snap.OAuthHost,
		BaseURL:      snap.BaseURL,
	}
	if snap.User != nil {
		o.Email, o.UserID, o.Nickname = snap.User.Email, snap.User.UserID, snap.User.Nickname
	}
	server.kimiRefreshMu.Lock()
	defer server.kimiRefreshMu.Unlock()
	return server.saveKimiLocked(map[string]any{"enabled": true, "oauth": kimiOAuthMap(o)})
}
