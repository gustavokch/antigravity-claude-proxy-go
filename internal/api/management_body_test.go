package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestManagementBodyCap covers the single cap handleManagement puts on every
// management request body. A known ContentLength is refused up front with 413; a
// chunked body of unknown length is cut off by MaxBytesReader and reaches the
// handler as a decode error, i.e. the 400 that handler already returns for a body
// it cannot parse.
func TestManagementBodyCap(t *testing.T) {
	srv, _, _ := newTestServerWithManager(t)
	handler := srv.Handler()

	t.Run("known length over cap is 413", func(t *testing.T) {
		body := strings.Repeat("a", maxManagementRequestBody+1)
		req := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(body))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("expected 413, got %d: %s", rec.Code, rec.Body.String())
		}
		var res map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatalf("body is not json: %v (%s)", err, rec.Body.String())
		}
		if res["status"] != "error" {
			t.Errorf("expected status error, got %v", res["status"])
		}
	})

	t.Run("chunked body over cap is refused by the handler", func(t *testing.T) {
		body := strings.Repeat("a", maxManagementRequestBody+1)
		req := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(body))
		req.ContentLength = -1
		req.TransferEncoding = []string{"chunked"}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("body under the cap still works", func(t *testing.T) {
		body := `{"logLevel":"warn"}`
		req := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(body))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
	})
}

// TestAccountsImportCapsArrayLength bounds one import request. The body cap
// alone would still admit millions of two-byte objects, and every account the
// pool gains is walked on later selections, so the array has its own limit.
func TestAccountsImportCapsArrayLength(t *testing.T) {
	srv, mgr, _ := newTestServerWithManager(t)
	handler := srv.Handler()

	importBody := func(n int) string {
		var b strings.Builder
		b.WriteByte('[')
		for i := 0; i < n; i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `{"email":"user%d@example.com","refresh_token":"tok-%d"}`, i, i)
		}
		b.WriteByte(']')
		return b.String()
	}

	t.Run("over the cap is refused", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/accounts/import", strings.NewReader(importBody(maxImportedAccounts+1)))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "too large") {
			t.Errorf("expected a too-large message, got %s", rec.Body.String())
		}
	})

	t.Run("over the cap is refused before any item is decoded", func(t *testing.T) {
		// maxImportedAccounts+1 items that are not objects. Today the whole body
		// is decoded into []map[string]any first, so this fails as "non-empty
		// array". With the array held as raw items, the length cap answers first.
		body := "[" + strings.TrimSuffix(strings.Repeat("1,", maxImportedAccounts+1), ",") + "]"
		req := httptest.NewRequest(http.MethodPost, "/api/accounts/import", strings.NewReader(body))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "too large") {
			t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
		}
	})
	t.Run("a malformed item under the cap is still one 400", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/accounts/import", strings.NewReader(`[1]`))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "non-empty array") {
			t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
		}
	})

	t.Run("at the cap is applied and persisted once", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/accounts/import", strings.NewReader(importBody(maxImportedAccounts)))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		var res struct {
			Status  string `json:"status"`
			Results struct {
				Added   []any `json:"added"`
				Updated []any `json:"updated"`
				Failed  []any `json:"failed"`
			} `json:"results"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatalf("body is not json: %v (%s)", err, rec.Body.String())
		}
		if res.Status != "ok" {
			t.Fatalf("expected status ok, got %q", res.Status)
		}
		if len(res.Results.Failed) != 0 {
			t.Errorf("expected no failures, got %v", res.Results.Failed)
		}
		if got := len(res.Results.Added) + len(res.Results.Updated); got != maxImportedAccounts {
			t.Errorf("expected %d accounts applied, got %d", maxImportedAccounts, got)
		}
		if n := len(mgr.GetAllAccounts()); n != maxImportedAccounts+1 {
			t.Errorf("expected the pool to hold %d accounts, got %d", maxImportedAccounts+1, n)
		}

		// The whole import is one disk write, so the file must carry every
		// account, not just the last one.
		path := filepath.Join(os.Getenv("ANTIGRAVITY_CONFIG_DIR"), "accounts.json")
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		var onDisk struct {
			Accounts []json.RawMessage `json:"accounts"`
		}
		if err := json.Unmarshal(raw, &onDisk); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		if len(onDisk.Accounts) != maxImportedAccounts+1 {
			t.Errorf("expected %d accounts on disk, got %d", maxImportedAccounts+1, len(onDisk.Accounts))
		}
	})
}
