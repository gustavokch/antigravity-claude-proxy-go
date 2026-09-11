package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandler(t *testing.T) {
	handler := Handler()

	t.Run("serve root index.html", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected status 200, got %d", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "Antigravity") {
			t.Errorf("expected body to contain 'Antigravity'")
		}
	})

	t.Run("serve static asset", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/favicon.svg", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected status 200, got %d", rec.Code)
		}
	})

	t.Run("assets served with no-cache to prevent stale JS after upgrade", func(t *testing.T) {
		paths := []string{
			"/js/components/models.js",
			"/js/components/classifier-config.js",
			"/css/style.css",
			"/favicon.svg",
		}
		for _, p := range paths {
			req := httptest.NewRequest(http.MethodGet, p, nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("%s: expected status 200, got %d", p, rec.Code)
			}
			if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
				t.Errorf("%s: expected Cache-Control no-cache, got %q", p, cc)
			}
		}
	})

	t.Run("serve classifier component and settings view", func(t *testing.T) {
		// Test classifier-config.js
		req := httptest.NewRequest(http.MethodGet, "/js/components/classifier-config.js", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected status 200, got %d", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "classifierConfig") {
			t.Errorf("expected classifier-config.js to contain 'classifierConfig'")
		}

		// Test settings.html contains classifier panel
		req = httptest.NewRequest(http.MethodGet, "/views/settings.html", nil)
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected status 200, got %d", rec.Code)
		}
		body := rec.Body.String()
		if !strings.Contains(body, "tabClassifier") {
			t.Errorf("expected settings.html to contain 'tabClassifier'")
		}
		if !strings.Contains(body, "classifierConfig") {
			t.Errorf("expected settings.html to contain 'classifierConfig'")
		}

		// Test index.html includes script tag
		req = httptest.NewRequest(http.MethodGet, "/", nil)
		rec = httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if !strings.Contains(rec.Body.String(), "classifier-config.js") {
			t.Errorf("expected index.html to include 'classifier-config.js'")
		}
	})

	t.Run("fallback to index on unknown route", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/unknown/spa/path", nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("expected status 200, got %d", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "<!DOCTYPE html>") {
			t.Errorf("expected fallback to index.html")
		}
	})
}
