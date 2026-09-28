package api

import (
	"net/http"
	"strings"
	"time"
)

// handleMitmStatus reports whether the forward proxy is running, its CA
// identity (never the key) and its counters.
func (server *Server) handleMitmStatus(writer http.ResponseWriter, request *http.Request) {
	rt := server.mitm
	if rt == nil {
		writeJSON(writer, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"enabled":       true,
		"listen":        rt.Addr,
		"caFingerprint": rt.CA.Fingerprint(),
		"caNotAfter":    rt.CA.NotAfter().UTC().Format(time.RFC3339),
		"stats":         rt.Server.Stats(),
	})
}

// handleMitmCA serves the CA certificate only.
func (server *Server) handleMitmCA(writer http.ResponseWriter, request *http.Request) {
	rt := server.mitm
	if rt == nil {
		writeJSON(writer, http.StatusNotFound, map[string]any{"status": "error", "error": "forward proxy is not running"})
		return
	}
	writer.Header().Set("Content-Type", "application/x-pem-file")
	writer.Header().Set("Content-Disposition", `attachment; filename="antigravity-proxy-mitm-ca.pem"`)
	_, _ = writer.Write(rt.CA.CertPEM())
}

// handleCloudSessionsList lists observed Claude Code cloud sessions.
func (server *Server) handleCloudSessionsList(writer http.ResponseWriter, request *http.Request) {
	rt := server.mitm
	if rt == nil {
		writeJSON(writer, http.StatusOK, map[string]any{"enabled": false, "sessions": []any{}})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"enabled": true, "sessions": rt.Registry.List()})
}

// handleCloudSessionGet returns one observed session by its display id.
func (server *Server) handleCloudSessionGet(writer http.ResponseWriter, request *http.Request, id string) {
	rt := server.mitm
	if rt != nil {
		if session, ok := rt.Registry.Get(strings.TrimSpace(id)); ok {
			writeJSON(writer, http.StatusOK, session)
			return
		}
	}
	writeJSON(writer, http.StatusNotFound, map[string]any{"status": "error", "error": "unknown session"})
}
