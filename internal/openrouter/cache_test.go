package openrouter

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func boolPtr(b bool) *bool {
	return &b
}

func TestClampCacheTTL(t *testing.T) {
	tests := []struct {
		name string
		in   int
		want int
	}{
		{"zero default", 0, DefaultCacheTTLSeconds},
		{"negative default", -5, DefaultCacheTTLSeconds},
		{"min bound", 1, 1},
		{"valid mid", 600, 600},
		{"max bound", 86400, 86400},
		{"above max clamped", 100000, 86400},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClampCacheTTL(tt.in); got != tt.want {
				t.Errorf("ClampCacheTTL(%d) = %d; want %d", tt.in, got, tt.want)
			}
		})
	}
}

func TestResolveResponseCacheConfig(t *testing.T) {
	t.Run("defaults when completely unset", func(t *testing.T) {
		global := ResponseCacheConfig{}
		got := ResolveResponseCacheConfig(global, nil)

		if got.Enabled != false {
			t.Errorf("expected Enabled=false, got %v", got.Enabled)
		}
		if got.TTLSeconds != DefaultCacheTTLSeconds {
			t.Errorf("expected TTLSeconds=%d, got %d", DefaultCacheTTLSeconds, got.TTLSeconds)
		}
		if got.AllowClientOverride != true {
			t.Errorf("expected AllowClientOverride=true, got %v", got.AllowClientOverride)
		}
	})

	t.Run("global config applied", func(t *testing.T) {
		global := ResponseCacheConfig{
			Enabled:             boolPtr(true),
			TTLSeconds:          120,
			AllowClientOverride: boolPtr(false),
		}
		got := ResolveResponseCacheConfig(global, nil)

		if got.Enabled != true {
			t.Errorf("expected Enabled=true, got %v", got.Enabled)
		}
		if got.TTLSeconds != 120 {
			t.Errorf("expected TTLSeconds=120, got %d", got.TTLSeconds)
		}
		if got.AllowClientOverride != false {
			t.Errorf("expected AllowClientOverride=false, got %v", got.AllowClientOverride)
		}
	})

	t.Run("partial model config inherits global fields", func(t *testing.T) {
		global := ResponseCacheConfig{
			Enabled:             boolPtr(true),
			TTLSeconds:          300,
			AllowClientOverride: boolPtr(false),
		}
		// Model only overrides TTL
		model := &ResponseCacheConfig{
			TTLSeconds: 600,
		}
		got := ResolveResponseCacheConfig(global, model)

		if got.Enabled != true {
			t.Errorf("expected Enabled=true (inherited), got %v", got.Enabled)
		}
		if got.TTLSeconds != 600 {
			t.Errorf("expected TTLSeconds=600 (overridden), got %d", got.TTLSeconds)
		}
		if got.AllowClientOverride != false {
			t.Errorf("expected AllowClientOverride=false (inherited), got %v", got.AllowClientOverride)
		}
	})

	t.Run("model overrides enabled and client override", func(t *testing.T) {
		global := ResponseCacheConfig{
			Enabled:             boolPtr(true),
			TTLSeconds:          300,
			AllowClientOverride: boolPtr(false),
		}
		model := &ResponseCacheConfig{
			Enabled:             boolPtr(false),
			AllowClientOverride: boolPtr(true),
		}
		got := ResolveResponseCacheConfig(global, model)

		if got.Enabled != false {
			t.Errorf("expected Enabled=false (model win), got %v", got.Enabled)
		}
		if got.TTLSeconds != 300 {
			t.Errorf("expected TTLSeconds=300 (inherited), got %d", got.TTLSeconds)
		}
		if got.AllowClientOverride != true {
			t.Errorf("expected AllowClientOverride=true (model win), got %v", got.AllowClientOverride)
		}
	})
}

func TestApplyResponseCacheHeaders(t *testing.T) {
	t.Run("client override allowed", func(t *testing.T) {
		upReq := httptest.NewRequest("POST", "https://openrouter.ai/api/v1/messages", nil)
		inHeader := http.Header{}
		inHeader.Set(HeaderCache, "true")
		inHeader.Set(HeaderCacheTTL, "60")
		inHeader.Set(HeaderCacheClear, "true")

		cfg := ResolvedResponseCacheConfig{
			Enabled:             false,
			TTLSeconds:          300,
			AllowClientOverride: true,
		}
		ApplyResponseCacheHeaders(upReq, inHeader, cfg)

		if got := upReq.Header.Get(HeaderCache); got != "true" {
			t.Errorf("expected %s='true', got %q", HeaderCache, got)
		}
		if got := upReq.Header.Get(HeaderCacheTTL); got != "60" {
			t.Errorf("expected %s='60', got %q", HeaderCacheTTL, got)
		}
		if got := upReq.Header.Get(HeaderCacheClear); got != "true" {
			t.Errorf("expected %s='true', got %q", HeaderCacheClear, got)
		}
	})

	t.Run("client override false disables caching and strips Clear", func(t *testing.T) {
		upReq := httptest.NewRequest("POST", "https://openrouter.ai/api/v1/messages", nil)
		inHeader := http.Header{}
		inHeader.Set(HeaderCache, "false")
		inHeader.Set(HeaderCacheClear, "true")

		cfg := ResolvedResponseCacheConfig{
			Enabled:             true,
			TTLSeconds:          300,
			AllowClientOverride: true,
		}
		ApplyResponseCacheHeaders(upReq, inHeader, cfg)

		if got := upReq.Header.Get(HeaderCache); got != "false" {
			t.Errorf("expected %s='false', got %q", HeaderCache, got)
		}
		if got := upReq.Header.Get(HeaderCacheClear); got != "" {
			t.Errorf("expected %s to be stripped when cache=false, got %q", HeaderCacheClear, got)
		}
	})

	t.Run("proxy config used when override disallowed", func(t *testing.T) {
		upReq := httptest.NewRequest("POST", "https://openrouter.ai/api/v1/messages", nil)
		inHeader := http.Header{}
		inHeader.Set(HeaderCache, "false") // client attempts to disable
		inHeader.Set(HeaderCacheTTL, "60")
		inHeader.Set(HeaderCacheClear, "true")

		cfg := ResolvedResponseCacheConfig{
			Enabled:             true,
			TTLSeconds:          600,
			AllowClientOverride: false,
		}
		ApplyResponseCacheHeaders(upReq, inHeader, cfg)

		if got := upReq.Header.Get(HeaderCache); got != "true" {
			t.Errorf("expected %s='true', got %q", HeaderCache, got)
		}
		if got := upReq.Header.Get(HeaderCacheTTL); got != "600" {
			t.Errorf("expected %s='600', got %q", HeaderCacheTTL, got)
		}
		if got := upReq.Header.Get(HeaderCacheClear); got != "true" {
			t.Errorf("expected %s='true' (forwarded when caching is on), got %q", HeaderCacheClear, got)
		}
	})

	t.Run("all headers stripped when override disallowed and proxy caching disabled", func(t *testing.T) {
		upReq := httptest.NewRequest("POST", "https://openrouter.ai/api/v1/messages", nil)
		inHeader := http.Header{}
		inHeader.Set(HeaderCache, "true")
		inHeader.Set(HeaderCacheTTL, "60")
		inHeader.Set(HeaderCacheClear, "true")

		cfg := ResolvedResponseCacheConfig{
			Enabled:             false,
			TTLSeconds:          300,
			AllowClientOverride: false,
		}
		ApplyResponseCacheHeaders(upReq, inHeader, cfg)

		if got := upReq.Header.Get(HeaderCache); got != "" {
			t.Errorf("expected %s to be empty, got %q", HeaderCache, got)
		}
		if got := upReq.Header.Get(HeaderCacheTTL); got != "" {
			t.Errorf("expected %s to be empty, got %q", HeaderCacheTTL, got)
		}
		if got := upReq.Header.Get(HeaderCacheClear); got != "" {
			t.Errorf("expected %s to be empty, got %q", HeaderCacheClear, got)
		}
	})
}

func TestApplyResponseCacheHeaders_ClientOverrideNormalization(t *testing.T) {
	apply := func(t *testing.T, in http.Header, cfg ResolvedResponseCacheConfig) *http.Request {
		t.Helper()
		upReq := httptest.NewRequest("POST", "https://openrouter.ai/api/v1/messages", nil)
		ApplyResponseCacheHeaders(upReq, in, cfg)
		return upReq
	}

	t.Run("client TTL clamped to max", func(t *testing.T) {
		in := http.Header{}
		in.Set(HeaderCache, "true")
		in.Set(HeaderCacheTTL, "999999")

		upReq := apply(t, in, ResolvedResponseCacheConfig{TTLSeconds: 300, AllowClientOverride: true})

		if got := upReq.Header.Get(HeaderCacheTTL); got != "86400" {
			t.Errorf("expected %s='86400' (clamped), got %q", HeaderCacheTTL, got)
		}
	})

	t.Run("unparseable client TTL falls back to config TTL", func(t *testing.T) {
		in := http.Header{}
		in.Set(HeaderCache, "true")
		in.Set(HeaderCacheTTL, "not-a-number")

		upReq := apply(t, in, ResolvedResponseCacheConfig{TTLSeconds: 300, AllowClientOverride: true})

		if got := upReq.Header.Get(HeaderCacheTTL); got != "300" {
			t.Errorf("expected %s='300' (config fallback), got %q", HeaderCacheTTL, got)
		}
	})

	t.Run("client cache flag is case-insensitive", func(t *testing.T) {
		in := http.Header{}
		in.Set(HeaderCache, "TRUE")
		in.Set(HeaderCacheClear, "True")

		upReq := apply(t, in, ResolvedResponseCacheConfig{TTLSeconds: 300, AllowClientOverride: true})

		if got := upReq.Header.Get(HeaderCache); got != "true" {
			t.Errorf("expected %s='true' (normalized), got %q", HeaderCache, got)
		}
		if got := upReq.Header.Get(HeaderCacheClear); got != "true" {
			t.Errorf("expected %s='true' (normalized), got %q", HeaderCacheClear, got)
		}
	})

	t.Run("unrecognized client cache value falls back to proxy config", func(t *testing.T) {
		in := http.Header{}
		in.Set(HeaderCache, "maybe")

		upReq := apply(t, in, ResolvedResponseCacheConfig{Enabled: true, TTLSeconds: 600, AllowClientOverride: true})

		if got := upReq.Header.Get(HeaderCache); got != "true" {
			t.Errorf("expected %s='true' (config), got %q", HeaderCache, got)
		}
		if got := upReq.Header.Get(HeaderCacheTTL); got != "600" {
			t.Errorf("expected %s='600' (config), got %q", HeaderCacheTTL, got)
		}
	})

	t.Run("client TTL alone is honored when caching is on", func(t *testing.T) {
		in := http.Header{}
		in.Set(HeaderCacheTTL, "120")

		upReq := apply(t, in, ResolvedResponseCacheConfig{Enabled: true, TTLSeconds: 600, AllowClientOverride: true})

		if got := upReq.Header.Get(HeaderCache); got != "true" {
			t.Errorf("expected %s='true', got %q", HeaderCache, got)
		}
		if got := upReq.Header.Get(HeaderCacheTTL); got != "120" {
			t.Errorf("expected %s='120' (client TTL), got %q", HeaderCacheTTL, got)
		}
	})

	t.Run("client TTL alone ignored when override denied", func(t *testing.T) {
		in := http.Header{}
		in.Set(HeaderCacheTTL, "120")

		upReq := apply(t, in, ResolvedResponseCacheConfig{Enabled: true, TTLSeconds: 600, AllowClientOverride: false})

		if got := upReq.Header.Get(HeaderCacheTTL); got != "600" {
			t.Errorf("expected %s='600' (config wins), got %q", HeaderCacheTTL, got)
		}
	})

	t.Run("client TTL alone does not enable caching on its own", func(t *testing.T) {
		in := http.Header{}
		in.Set(HeaderCacheTTL, "120")

		upReq := apply(t, in, ResolvedResponseCacheConfig{Enabled: false, TTLSeconds: 300, AllowClientOverride: true})

		if got := upReq.Header.Get(HeaderCache); got != "" {
			t.Errorf("expected %s to be empty, got %q", HeaderCache, got)
		}
		if got := upReq.Header.Get(HeaderCacheTTL); got != "" {
			t.Errorf("expected %s to be empty, got %q", HeaderCacheTTL, got)
		}
	})
}

func TestExtractResponseCacheHeaders(t *testing.T) {
	t.Run("extracts hit headers", func(t *testing.T) {
		h := http.Header{}
		h.Set(HeaderCacheStatus, "HIT")
		h.Set(HeaderCacheAge, "42")
		h.Set(HeaderCacheTTL, "258")
		h.Set(HeaderCacheSourceID, "gen-abc-123")

		info := ExtractResponseCacheHeaders(h)
		if info.Status != "HIT" {
			t.Errorf("expected Status=HIT, got %q", info.Status)
		}
		if info.Age != 42 {
			t.Errorf("expected Age=42, got %d", info.Age)
		}
		if info.TTL != 258 {
			t.Errorf("expected TTL=258, got %d", info.TTL)
		}
		if info.SourceID != "gen-abc-123" {
			t.Errorf("expected SourceID=gen-abc-123, got %q", info.SourceID)
		}
	})

	t.Run("extracts miss headers with lowercase status", func(t *testing.T) {
		h := http.Header{}
		h.Set(HeaderCacheStatus, "miss")
		h.Set(HeaderCacheTTL, "300")

		info := ExtractResponseCacheHeaders(h)
		if info.Status != "MISS" {
			t.Errorf("expected Status=MISS, got %q", info.Status)
		}
		if info.Age != 0 {
			t.Errorf("expected Age=0, got %d", info.Age)
		}
		if info.TTL != 300 {
			t.Errorf("expected TTL=300, got %d", info.TTL)
		}
	})

	t.Run("handles missing headers", func(t *testing.T) {
		h := http.Header{}
		info := ExtractResponseCacheHeaders(h)
		if info.Status != "" {
			t.Errorf("expected empty Status, got %q", info.Status)
		}
		if info.Age != 0 || info.TTL != 0 || info.SourceID != "" {
			t.Errorf("expected zero values, got %+v", info)
		}
	})
}
