package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"antigravity-go-proxy/internal/auth"
	"antigravity-go-proxy/internal/config"
)

func newAPIKeysTestServer(t *testing.T, upstream *fakeUpstream, apiKey string, logBuf *bytes.Buffer) *Server {
	t.Helper()
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	server, err := New(Options{
		APIKey:    apiKey,
		ProjectID: "test-proj",
		Now:       func() time.Time { return now },
		Credentials: func(context.Context) (auth.Credentials, error) {
			return auth.Credentials{AccessToken: "access-token", Email: "user@example.com", Expiry: now.Add(time.Hour)}, nil
		},
		NewUpstream: func(string) Upstream { return upstream },
		Logger:      slog.New(slog.NewTextHandler(logBuf, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func apiKeysTestRequest(key, bearer string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-sonnet-4-6","max_tokens":128,
		"messages":[{"role":"user","content":"hello"}]
	}`))
	if key != "" {
		req.Header.Set("x-api-key", key)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	return req
}

func withAPIKeysConfig(t *testing.T, cfg config.Config) {
	t.Helper()
	orig := config.Get()
	t.Cleanup(func() { config.SetForTest(orig) })
	config.SetForTest(cfg)
}

func multiKeyConfig() config.Config {
	cfg := config.DefaultConfig()
	cfg.APIKeys = []config.APIKeyEntry{
		{ID: "gus", Label: "gus", Key: "gus-secret", Enabled: true},
		{ID: "friend", Label: "friend", Key: "friend-secret", Enabled: true},
		{ID: "ex", Label: "ex", Key: "ex-secret", Enabled: false},
	}
	return cfg
}

func TestMultiKeyAuthAcceptsRejects(t *testing.T) {
	withAPIKeysConfig(t, multiKeyConfig())

	cases := []struct {
		name    string
		key     string
		bearer  string
		want    int
		wantLog string
	}{
		{"owner x-api-key accepted", "gus-secret", "", http.StatusOK, "client=gus"},
		{"friend bearer accepted", "", "friend-secret", http.StatusOK, "client=friend"},
		{"wrong key rejected", "nope", "", http.StatusUnauthorized, ""},
		{"disabled key ignored", "ex-secret", "", http.StatusUnauthorized, ""},
		{"missing key rejected", "", "", http.StatusUnauthorized, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			upstream := &fakeUpstream{streamData: standardStream()}
			var logBuf bytes.Buffer
			server := newAPIKeysTestServer(t, upstream, "", &logBuf)
			rec := httptest.NewRecorder()
			server.Handler().ServeHTTP(rec, apiKeysTestRequest(tc.key, tc.bearer))
			if rec.Code != tc.want {
				t.Fatalf("status=%d want %d body=%s", rec.Code, tc.want, rec.Body.String())
			}
			if tc.wantLog != "" && !strings.Contains(logBuf.String(), tc.wantLog) {
				t.Fatalf("log missing %q: %s", tc.wantLog, logBuf.String())
			}
		})
	}
}

func TestLegacySingleAPIKeyStillWorks(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.APIKey = "legacy-secret"
	withAPIKeysConfig(t, cfg)

	upstream := &fakeUpstream{streamData: standardStream()}
	var logBuf bytes.Buffer
	server := newAPIKeysTestServer(t, upstream, "", &logBuf)

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, apiKeysTestRequest("legacy-secret", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("legacy key status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(logBuf.String(), "client=default") {
		t.Fatalf("log missing client=default: %s", logBuf.String())
	}

	recBad := httptest.NewRecorder()
	server.Handler().ServeHTTP(recBad, apiKeysTestRequest("wrong", ""))
	if recBad.Code != http.StatusUnauthorized {
		t.Fatalf("wrong key status=%d want 401", recBad.Code)
	}
}

func TestLegacyIgnoredWhenAPIKeysPresent(t *testing.T) {
	cfg := multiKeyConfig()
	cfg.APIKey = "legacy-secret"
	withAPIKeysConfig(t, cfg)

	upstream := &fakeUpstream{streamData: standardStream()}
	var logBuf bytes.Buffer
	server := newAPIKeysTestServer(t, upstream, "", &logBuf)

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, apiKeysTestRequest("legacy-secret", ""))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("legacy key with apiKeys set status=%d want 401", rec.Code)
	}

	recOK := httptest.NewRecorder()
	server.Handler().ServeHTTP(recOK, apiKeysTestRequest("gus-secret", ""))
	if recOK.Code != http.StatusOK {
		t.Fatalf("gus key status=%d body=%s", recOK.Code, recOK.Body.String())
	}
}

func TestOpenProxyWhenNoKeys(t *testing.T) {
	withAPIKeysConfig(t, config.DefaultConfig())

	upstream := &fakeUpstream{streamData: standardStream()}
	var logBuf bytes.Buffer
	server := newAPIKeysTestServer(t, upstream, "", &logBuf)

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, apiKeysTestRequest("", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("open proxy status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(logBuf.String(), "client=open") {
		t.Fatalf("log missing client=open: %s", logBuf.String())
	}
}

func TestFlagAPIKeyMergedAsImplicitEntry(t *testing.T) {
	withAPIKeysConfig(t, config.DefaultConfig())

	upstream := &fakeUpstream{streamData: standardStream()}
	var logBuf bytes.Buffer
	server := newAPIKeysTestServer(t, upstream, "flag-key", &logBuf)

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, apiKeysTestRequest("flag-key", ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("flag key status=%d body=%s", rec.Code, rec.Body.String())
	}

	recNo := httptest.NewRecorder()
	server.Handler().ServeHTTP(recNo, apiKeysTestRequest("", ""))
	if recNo.Code != http.StatusUnauthorized {
		t.Fatalf("missing key status=%d want 401", recNo.Code)
	}
}

// Keys are configured, none of them usable: the proxy must stay closed. Open
// is reserved for "no key material anywhere"; treating an all-disabled list
// like an empty one turns a revoked or mistyped key into an open proxy.
func TestKeysConfiguredButNoneUsableRejectsEveryone(t *testing.T) {
	cases := []struct{ name, apiKeysJSON string }{
		{"all disabled", `[{"id":"a","label":"a","key":"a-secret","enabled":false}]`},
		{"empty key", `[{"id":"a","label":"a","key":"","enabled":true}]`},
		{"enabled omitted", `[{"id":"a","label":"a","key":"a-secret"}]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			if err := json.Unmarshal([]byte(`{"apiKeys":`+tc.apiKeysJSON+`}`), &cfg); err != nil {
				t.Fatal(err)
			}
			withAPIKeysConfig(t, cfg)
			for _, key := range []string{"", "a-secret", "nope"} {
				server := newAPIKeysTestServer(t, &fakeUpstream{streamData: standardStream()}, "", &bytes.Buffer{})
				rec := httptest.NewRecorder()
				server.Handler().ServeHTTP(rec, apiKeysTestRequest(key, ""))
				if rec.Code != http.StatusUnauthorized {
					t.Fatalf("key %q: status=%d want 401 (keys configured, none usable)", key, rec.Code)
				}
			}
		})
	}
}

// The legacy apiKey is superseded by apiKeys for as long as any entry exists,
// in any state. Disabling the last entry must not resurrect the old shared key.
func TestLegacyKeyIgnoredWhileAnyAPIKeysEntryExists(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.APIKey = "legacy-secret"
	cfg.APIKeys = []config.APIKeyEntry{{ID: "ex", Label: "ex", Key: "ex-secret", Enabled: false}}
	withAPIKeysConfig(t, cfg)

	for _, key := range []string{"legacy-secret", "ex-secret"} {
		server := newAPIKeysTestServer(t, &fakeUpstream{streamData: standardStream()}, "", &bytes.Buffer{})
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, apiKeysTestRequest(key, ""))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("key %q: status=%d want 401", key, rec.Code)
		}
	}
}

// The -api-key flag / env value is an out-of-band credential honored next to
// apiKeys. When it equals an enabled entry's key the entry's label wins.
func TestFlagKeyHonoredAlongsideAPIKeys(t *testing.T) {
	withAPIKeysConfig(t, multiKeyConfig())

	cases := []struct {
		name, flagKey, sent, wantLog string
	}{
		{"distinct flag key", "ops-secret", "ops-secret", "client=default"},
		{"flag key equals entry key", "gus-secret", "gus-secret", "client=gus"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logBuf bytes.Buffer
			server := newAPIKeysTestServer(t, &fakeUpstream{streamData: standardStream()}, tc.flagKey, &logBuf)
			rec := httptest.NewRecorder()
			server.Handler().ServeHTTP(rec, apiKeysTestRequest(tc.sent, ""))
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(logBuf.String(), tc.wantLog) {
				t.Fatalf("log missing %q: %s", tc.wantLog, logBuf.String())
			}
		})
	}
}
