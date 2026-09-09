package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"antigravity-go-proxy/internal/cachebump"
)

func seedCacheBumpRecord(t *testing.T, srv *Server, route cachebump.Route, sessionID string) cachebump.Record {
	t.Helper()
	store, _ := srv.getCacheBump()
	now := time.Now()
	rec := cachebump.Record{
		Key:       cachebump.RecordKey(route, sessionID),
		SessionID: sessionID,
		Route:     route,
		Model:     "claude-sonnet-5",
		Body:      []byte(`{"messages":[]}`),
		Headers:   nil,
		TTL:       5 * time.Minute,
		LastSeen:  now,
		NextBump:  now.Add(time.Minute),
	}
	store.Upsert(rec)
	return rec
}

func TestCacheBumpManagement_GetRecords(t *testing.T) {
	srv, _, _ := newTestServerWithManager(t)
	handler := srv.Handler()
	seedCacheBumpRecord(t, srv, cachebump.RouteClaudeCode, "sess-mgmt")
	seedCacheBumpRecord(t, srv, cachebump.RouteKimi, "sess-mgmt")

	req := httptest.NewRequest(http.MethodGet, "/api/cache-bump", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Records []struct {
			Key       string `json:"key"`
			SessionID string `json:"session_id"`
			Route     string `json:"route"`
			Model     string `json:"model"`
			Stopped   bool   `json:"stopped"`
		} `json:"records"`
		Stats cachebump.Stats `json:"stats"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if len(resp.Records) != 2 {
		t.Fatalf("expected 2 records, got %d", len(resp.Records))
	}
	for _, r := range resp.Records {
		if r.SessionID != "sess-mgmt" {
			t.Errorf("unexpected session %q", r.SessionID)
		}
	}
}

func TestCacheBumpManagement_RecordsNeverCarryBody(t *testing.T) {
	srv, _, _ := newTestServerWithManager(t)
	handler := srv.Handler()
	seedCacheBumpRecord(t, srv, cachebump.RouteClaudeCode, "sess-body")

	req := httptest.NewRequest(http.MethodGet, "/api/cache-bump", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	payload := rec.Body.String()
	if bytes.Contains([]byte(payload), []byte(`"messages"`)) || bytes.Contains([]byte(payload), []byte(`"body"`)) {
		t.Errorf("management response leaked body content: %s", payload)
	}
}

func TestCacheBumpManagement_StopSession(t *testing.T) {
	srv, _, _ := newTestServerWithManager(t)
	handler := srv.Handler()
	seedCacheBumpRecord(t, srv, cachebump.RouteClaudeCode, "sess-stop")

	req := httptest.NewRequest(http.MethodPost, "/api/cache-bump/sess-stop/stop", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	store, sched := srv.getCacheBump()
	got, ok := store.Get(cachebump.RecordKey(cachebump.RouteClaudeCode, "sess-stop"))
	if !ok {
		t.Fatal("record missing")
	}
	if !got.Stopped || got.StopReason != "manual" {
		t.Errorf("expected manual stop, got %+v", got)
	}
	// Manual stops must be visible in the aggregate stats, not only on the
	// record: the stop goes through the scheduler for that reason.
	if got := sched.StatsSnapshot().StopsByReason["manual"]; got != 1 {
		t.Errorf("expected stats to count the manual stop, got %d", got)
	}
}

func TestCacheBumpManagement_StopUnknownSession(t *testing.T) {
	srv, _, _ := newTestServerWithManager(t)
	handler := srv.Handler()

	req := httptest.NewRequest(http.MethodPost, "/api/cache-bump/nope/stop", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec.Code)
	}
}

func TestCacheBumpManagement_ClearAll(t *testing.T) {
	srv, _, _ := newTestServerWithManager(t)
	handler := srv.Handler()
	seedCacheBumpRecord(t, srv, cachebump.RouteClaudeCode, "sess-clear")
	seedCacheBumpRecord(t, srv, cachebump.RouteKimi, "sess-clear2")

	req := httptest.NewRequest(http.MethodDelete, "/api/cache-bump", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	store, _ := srv.getCacheBump()
	if store.Len() != 0 {
		t.Errorf("expected store cleared, got %d records", store.Len())
	}
}
