package api

import (
	"net/http"
	"strings"

	"antigravity-go-proxy/internal/cachebump"
)

// handleCacheBumpGet returns every recorded session with its scheduling
// state plus aggregate counters. Record JSON omits the recorded body and
// headers by tag, so they are never exposed.
func (server *Server) handleCacheBumpGet(writer http.ResponseWriter, request *http.Request) {
	store, sched := server.getCacheBump()
	records := store.Snapshot()

	stats := cachebump.Stats{StopsByReason: map[string]int64{}}
	if sched != nil {
		stats = sched.StatsSnapshot()
	}

	writeJSON(writer, http.StatusOK, map[string]any{
		"records":   records,
		"stats":     stats,
		"bytes":     store.Bytes(),
		"max_bytes": store.MaxBytes(),
	})
}

// handleCacheBumpStop stops bumping for one session across all routes.
func (server *Server) handleCacheBumpStop(writer http.ResponseWriter, request *http.Request, sessionID string) {
	store, sched := server.getCacheBump()
	stopped := 0
	for _, rec := range store.Snapshot() {
		if rec.SessionID != sessionID {
			continue
		}
		if sched != nil {
			sched.StopSession(rec.Key, "manual")
		} else {
			store.Stop(rec.Key, "manual")
		}
		stopped++
	}
	if stopped == 0 {
		writeJSON(writer, http.StatusNotFound, map[string]any{"status": "error", "error": "unknown session"})
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"status": "ok", "stopped": stopped})
}

// handleCacheBumpClear drops every recorded session.
func (server *Server) handleCacheBumpClear(writer http.ResponseWriter, request *http.Request) {
	store, _ := server.getCacheBump()
	store.Clear()
	writeJSON(writer, http.StatusOK, map[string]any{"status": "ok"})
}

// stopSessionID extracts the {sessionID} segment of /api/cache-bump/{id}/stop.
func stopSessionID(path string) string {
	return strings.TrimSuffix(strings.TrimPrefix(path, "/api/cache-bump/"), "/stop")
}
