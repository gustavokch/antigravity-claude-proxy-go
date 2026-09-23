package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"antigravity-go-proxy/internal/ccidentity"
	"antigravity-go-proxy/internal/config"
	"antigravity-go-proxy/internal/headroom"
	"antigravity-go-proxy/internal/headroom/stages/ccr"
)

// capturedForward is what the upstream received on one proxied request.
type capturedForward struct {
	path          string
	userAgent     string
	app           string
	apiKey        string
	stainlessOS   string
	beta          string
	requestClass  string
	body          []byte
	sawSessionID  string
	sawRequestID  string
	authorization string
}

// customEndpointUpstream starts a target server and returns it with a recorder.
func customEndpointUpstream(t *testing.T) (*httptest.Server, *capturedForward) {
	t.Helper()
	got := &capturedForward{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path = r.URL.RequestURI()
		got.userAgent = r.Header.Get("User-Agent")
		got.app = r.Header.Get("x-app")
		got.apiKey = r.Header.Get("x-api-key")
		got.stainlessOS = headerFold(r.Header, "X-Stainless-OS")
		got.beta = headerFold(r.Header, "anthropic-beta")
		got.requestClass = r.Header.Get("x-claude-code-request-class")
		got.sawSessionID = r.Header.Get("X-Claude-Code-Session-Id")
		got.sawRequestID = r.Header.Get("x-client-request-id")
		got.authorization = r.Header.Get("Authorization")
		got.body, _ = io.ReadAll(r.Body)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"type":"message","id":"msg_ok","content":[{"type":"text","text":"ok"}]}`))
	}))
	t.Cleanup(server.Close)
	return server, got
}

// headerFold reads a header without canonicalising the lookup key, because the
// captured names are X-Stainless-OS and the lowercase anthropic-* family.
func headerFold(h http.Header, name string) string {
	if values, ok := h[name]; ok && len(values) > 0 {
		return strings.Join(values, ",")
	}
	for key, values := range h {
		if strings.EqualFold(key, name) && len(values) > 0 {
			return strings.Join(values, ",")
		}
	}
	return ""
}

// serveCustomEndpointRequest drives one /v1/messages request through the handler
// with a deliberately foreign client fingerprint.
func serveCustomEndpointRequest(t *testing.T, handler http.Handler) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-custom-model",
		"temperature":0.7,
		"system":"be helpful",
		"messages":[{"role":"user","content":"hello"}]
	}`))
	req.Header.Set("x-api-key", "local-key")
	req.Header.Set("User-Agent", "cursor/1.2.3")
	req.Header.Set("x-app", "cursor")
	req.Header.Set("X-Title", "Cursor")
	req.Header.Set("X-Stainless-Lang", "python")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// saveCustomEndpoint stores one endpoint. It carries no apiKey by default:
// a configured key turns normalization off (see customEndpointIdentity), so a
// test that wants normalization must not have one, and a test that wants the
// key must ask for it.
func saveCustomEndpoint(t *testing.T, endpointURL string, extra map[string]any) {
	t.Helper()
	entry := map[string]any{"url": endpointURL}
	for k, v := range extra {
		entry[k] = v
	}
	if _, err := config.Save(map[string]any{
		"customEndpoints": map[string]any{"claude-custom-model": entry},
	}); err != nil {
		t.Fatalf("save custom endpoint config: %v", err)
	}
}

func withConfigDir(t *testing.T) {
	t.Helper()
	origCfg := config.Get()
	t.Cleanup(func() { config.SetForTest(origCfg) })
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)
}

// TestCustomEndpoint_AnthropicShapedEndpointIsNormalized is the end-to-end case
// the feature exists for: a foreign harness reaches an Anthropic-shaped relay and
// the relay sees vanilla Claude Code.
func TestCustomEndpoint_AnthropicShapedEndpointIsNormalized(t *testing.T) {
	withConfigDir(t)

	// The path is what makes this endpoint Anthropic-shaped; a bare host would
	// not match isAnthropicEndpoint and would keep the client's headers.
	target, got := customEndpointUpstream(t)
	saveCustomEndpoint(t, target.URL+"/v1/messages", nil)

	h := newTestHandler(t, &fakeUpstream{streamData: standardStream()}, "test-proj")
	rec := serveCustomEndpointRequest(t, h)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	if got.userAgent != ccidentity.MessagesUserAgent {
		t.Errorf("User-Agent = %q, want the captured %q", got.userAgent, ccidentity.MessagesUserAgent)
	}
	if got.app != "cli" {
		t.Errorf("x-app = %q, want cli", got.app)
	}
	if got.stainlessOS != "Linux" {
		t.Errorf("X-Stainless-OS = %q, want the captured value", got.stainlessOS)
	}
	if got.requestClass != "main" {
		t.Errorf("x-claude-code-request-class = %q, want main", got.requestClass)
	}
	if got.beta != strings.Join(ccidentity.Betas, ",") {
		t.Errorf("anthropic-beta = %q, want the captured 13-entry order", got.beta)
	}
	if got.sawSessionID == "" || got.sawRequestID == "" {
		t.Errorf("session id = %q, request id = %q; both must be set", got.sawSessionID, got.sawRequestID)
	}
	// The captured OAuth request carries no x-api-key, and this endpoint
	// configures no key of its own, so nothing should reach the upstream under
	// either name. An endpoint that does configure one is not normalized at all;
	// TestCustomEndpointAPIKeySurvivesNormalization covers that.
	if got.apiKey != "" {
		t.Errorf("x-api-key = %q; the captured request has none", got.apiKey)
	}
	if got.authorization != "" {
		t.Errorf("Authorization = %q; no credential was configured", got.authorization)
	}

	var body map[string]any
	if err := json.Unmarshal(got.body, &body); err != nil {
		t.Fatalf("upstream body is not JSON: %v", err)
	}
	if _, present := body["temperature"]; present {
		t.Error("temperature reached the upstream; Claude Code does not send it")
	}
	metadata, _ := body["metadata"].(map[string]any)
	userID, _ := metadata["user_id"].(string)
	if !strings.HasPrefix(userID, `{"device_id":"`) {
		t.Errorf("metadata.user_id = %q, want the captured stringified JSON object", userID)
	}
	system, _ := body["system"].([]any)
	if len(system) == 0 {
		t.Fatal("system is empty; block 0 must carry the billing header")
	}
	first, _ := system[0].(map[string]any)
	text, _ := first["text"].(string)
	if !strings.HasPrefix(text, "x-anthropic-billing-header: cc_version=2.1.280.") {
		t.Errorf("system[0] = %q, want the generated billing header", text)
	}
}

// TestCustomEndpoint_NormalizedRequestCarriesTheCapturedQuery pins the query
// string the capture records. defaults.go states a /v1/messages request without
// ?beta=true does not match the captured traffic, and the pooled gateway path
// already sends it, so the custom-endpoint path must too.
func TestCustomEndpoint_NormalizedRequestCarriesTheCapturedQuery(t *testing.T) {
	withConfigDir(t)

	target, got := customEndpointUpstream(t)
	saveCustomEndpoint(t, target.URL+"/v1/messages", nil)

	h := newTestHandler(t, &fakeUpstream{streamData: standardStream()}, "test-proj")
	if rec := serveCustomEndpointRequest(t, h); rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(got.path, "beta=true") {
		t.Errorf("upstream saw %q, want the captured ?beta=true", got.path)
	}
}

// TestCustomEndpoint_NormalizationFailureFailsClosed pins the error discipline
// ccidentity.ErrNotAnObject's own doc comment demands: an unnormalised body is
// the fingerprint normalization exists to remove, so forwarding it is worse than
// refusing. The SendMessage path already returns an error here; this path used to
// log and forward the client's body AND headers.
//
// forwardToCustomEndpoint is called directly because the router reaches it only
// after reading "model" out of the body, which means it never hands it a
// non-object today. That makes this a contract test rather than a live-path one:
// the function must not depend on a guarantee its caller happens to provide.
func TestCustomEndpoint_NormalizationFailureFailsClosed(t *testing.T) {
	withConfigDir(t)

	target, got := customEndpointUpstream(t)
	endpoint := config.EndpointConfig{URL: target.URL + "/v1/messages"}

	server := newTestServer(t, &fakeUpstream{streamData: standardStream()}, "test-proj")
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`null`))
	req.Header.Set("User-Agent", "cursor/1.2.3")
	rec := httptest.NewRecorder()
	server.forwardToCustomEndpoint(rec, req, endpoint, "claude-custom-model", []byte(`null`))

	if rec.Code == http.StatusOK {
		t.Errorf("status = 200; a body that cannot be normalized must not be forwarded")
	}
	if got.userAgent != "" {
		t.Errorf("upstream saw User-Agent %q; it should have seen no request at all", got.userAgent)
	}
}

// TestClaudeCodeConfigPost_RejectsControlCharsInIdentityOverrides pins that a
// bad override is refused at save time.
//
// The six overrides become outbound header values. Go's transport rejects a
// value containing a CR or LF with "invalid header field value", so one bad
// paste in the settings panel would break every request to the gateway with an
// error naming neither the field nor the panel. The WebUI trims surrounding
// whitespace, which does not touch an embedded newline.
func TestClaudeCodeConfigPost_RejectsControlCharsInIdentityOverrides(t *testing.T) {
	srv, _, _ := newTestServerWithManager(t)
	seedCCConfig(t)

	before := config.Get().ClaudeCode.Identity
	rec := doJSON(t, srv, http.MethodPost, "/api/claudecode/config",
		`{"enabled":true,"identity":{"userAgent":"claude-cli/2.1.280\r\nX-Injected: 1"}}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "userAgent") {
		t.Errorf("error %q does not name the offending field", rec.Body.String())
	}
	if got := config.Get().ClaudeCode.Identity; got != before {
		t.Errorf("the rejected value was stored anyway: %+v", got)
	}
}

// TestClaudeCodeConfigPost_AcceptsCleanIdentityOverrides is the other half: the
// check must not reject the values the panel is for.
func TestClaudeCodeConfigPost_AcceptsCleanIdentityOverrides(t *testing.T) {
	srv, _, _ := newTestServerWithManager(t)
	seedCCConfig(t)

	rec := doJSON(t, srv, http.MethodPost, "/api/claudecode/config",
		`{"enabled":true,"identity":{"entrypoint":"cli","stainlessOs":"Darwin"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := config.Get().ClaudeCode.Identity.StainlessOS; got != "Darwin" {
		t.Errorf("stainlessOs = %q, want Darwin", got)
	}
}

// TestConfigPost_RejectsControlCharsInEndpointIdentity is the same check on the
// other config surface: a custom endpoint carries its own identity overrides, and
// they reach header values by the same route.
func TestConfigPost_RejectsControlCharsInEndpointIdentity(t *testing.T) {
	srv, _, _ := newTestServerWithManager(t)

	rec := doJSON(t, srv, http.MethodPost, "/api/config", `{"customEndpoints":{"m":{`+
		`"url":"https://example.com/v1/messages",`+
		`"identity":{"stainlessOs":"Darwin\nX-Injected: 1"}}}}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "stainlessOs") {
		t.Errorf("error %q does not name the offending field", rec.Body.String())
	}
	if _, present := config.Get().CustomEndpoints["m"]; present {
		t.Error("the rejected endpoint was stored anyway")
	}
}

// TestCustomEndpoint_NonAnthropicShapeKeepsClientHeaders pins the gate: an
// endpoint that does not speak the Anthropic wire is not told a lie about its own
// request format.
func TestCustomEndpoint_NonAnthropicShapeKeepsClientHeaders(t *testing.T) {
	withConfigDir(t)

	target, got := customEndpointUpstream(t)
	saveCustomEndpoint(t, target.URL, nil) // bare host: no /messages path

	h := newTestHandler(t, &fakeUpstream{streamData: standardStream()}, "test-proj")
	if rec := serveCustomEndpointRequest(t, h); rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	if got.userAgent != "cursor/1.2.3" {
		t.Errorf("User-Agent = %q; a non-Anthropic endpoint must see the client's own value", got.userAgent)
	}
	if got.app != "cursor" {
		t.Errorf("x-app = %q, want the client's own value passed through", got.app)
	}
	if got.requestClass != "" {
		t.Errorf("x-claude-code-request-class = %q; it must not be invented here", got.requestClass)
	}
}

// TestCustomEndpoint_DisabledIdentityKeepsClientHeaders pins the opt-out, which
// is a DISABLE flag so that the zero value still means normalize.
func TestCustomEndpoint_DisabledIdentityKeepsClientHeaders(t *testing.T) {
	withConfigDir(t)

	target, got := customEndpointUpstream(t)
	saveCustomEndpoint(t, target.URL+"/v1/messages", map[string]any{
		"identity": map[string]any{"disabled": true},
	})

	h := newTestHandler(t, &fakeUpstream{streamData: standardStream()}, "test-proj")
	if rec := serveCustomEndpointRequest(t, h); rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	if got.userAgent != "cursor/1.2.3" {
		t.Errorf("User-Agent = %q, want the client's own value when identity is disabled", got.userAgent)
	}
	if got.requestClass != "" {
		t.Errorf("x-claude-code-request-class = %q; disabled identity must add nothing", got.requestClass)
	}
}

// ccrEnabledServer wires the headroom engine and store that
// forwardToCustomEndpoint's first branch requires, so the sender path can be
// driven instead of the reverse-proxy one.
func ccrEnabledServer(t *testing.T) *Server {
	t.Helper()
	server := newTestServer(t, &fakeUpstream{streamData: standardStream()}, "test-proj")
	store := ccr.NewCCRStore(1024 * 1024)
	server.ccrStore = store
	server.headroom = headroom.NewEngine(headroom.Config{
		Enabled: true,
		CCR:     headroom.CCRConfig{Enabled: true},
	}, nil, ccr.NewStage(store))
	return server
}

// TestCustomEndpointAPIKeySurvivesNormalization is the credential regression.
//
// x-api-key is on the omit list (internal/ccidentity/defaults.go), ApplyHeaders
// deletes every omitted name, and both custom-endpoint paths set the key before
// ApplyHeaders runs. An endpoint that authenticates by API key therefore lost
// its credential the moment normalization was switched on by default, and only
// relays that also accept the Authorization Bearer header set beside it kept
// working.
//
// Both paths are driven because each sets the key and calls ApplyHeaders in its
// own block.
func TestCustomEndpointAPIKeySurvivesNormalization(t *testing.T) {
	t.Run("reverse proxy path", func(t *testing.T) {
		withConfigDir(t)

		target, got := customEndpointUpstream(t)
		saveCustomEndpoint(t, target.URL+"/v1/messages", map[string]any{"apiKey": "target-secret-key"})

		h := newTestHandler(t, &fakeUpstream{streamData: standardStream()}, "test-proj")
		if rec := serveCustomEndpointRequest(t, h); rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		if got.apiKey != "target-secret-key" {
			t.Errorf("x-api-key = %q, want the configured key; the endpoint cannot authenticate without it", got.apiKey)
		}
	})

	t.Run("ccr sender path", func(t *testing.T) {
		withConfigDir(t)

		target, got := customEndpointUpstream(t)
		endpoint := config.EndpointConfig{URL: target.URL + "/v1/messages", APIKey: "target-secret-key"}

		server := ccrEnabledServer(t)
		body := `{"model":"claude-custom-model","stream":false,"messages":[{"role":"user","content":"hello"}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
		req.Header.Set("User-Agent", "cursor/1.2.3")
		rec := httptest.NewRecorder()
		server.forwardToCustomEndpoint(rec, req, endpoint, "claude-custom-model", []byte(body))

		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		if got.apiKey != "target-secret-key" {
			t.Errorf("x-api-key = %q, want the configured key; the endpoint cannot authenticate without it", got.apiKey)
		}
	})
}

// TestCustomEndpointWithoutAPIKeyStillNormalizes keeps the fix narrow: the
// refusal is about a credential that normalization would delete, not about
// custom endpoints in general.
func TestCustomEndpointWithoutAPIKeyStillNormalizes(t *testing.T) {
	withConfigDir(t)

	target, got := customEndpointUpstream(t)
	saveCustomEndpoint(t, target.URL+"/v1/messages", nil)

	h := newTestHandler(t, &fakeUpstream{streamData: standardStream()}, "test-proj")
	if rec := serveCustomEndpointRequest(t, h); rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	if got.userAgent != ccidentity.MessagesUserAgent {
		t.Errorf("User-Agent = %q, want the captured %q", got.userAgent, ccidentity.MessagesUserAgent)
	}
	if got.requestClass != "main" {
		t.Errorf("x-claude-code-request-class = %q, want main", got.requestClass)
	}
	if got.apiKey != "" {
		t.Errorf("x-api-key = %q; with no configured key the omit list applies", got.apiKey)
	}
}

// TestCustomEndpoint_IdentityOverridesAreHonoured covers the configurable
// platform values, which default to the Linux capture container.
func TestCustomEndpoint_IdentityOverridesAreHonoured(t *testing.T) {
	withConfigDir(t)

	target, got := customEndpointUpstream(t)
	saveCustomEndpoint(t, target.URL+"/v1/messages", map[string]any{
		"identity": map[string]any{
			"stainlessOs":             "macOS",
			"stainlessRuntimeVersion": "v99.0.0",
			"entrypoint":              "cli",
		},
	})

	h := newTestHandler(t, &fakeUpstream{streamData: standardStream()}, "test-proj")
	if rec := serveCustomEndpointRequest(t, h); rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	if got.stainlessOS != "macOS" {
		t.Errorf("X-Stainless-OS = %q, want the configured override", got.stainlessOS)
	}
	if got.userAgent != "claude-cli/2.1.280 (external, cli)" {
		t.Errorf("User-Agent = %q, want the constructed cli form", got.userAgent)
	}
}

// TestIdentityValidationIsTheSameOnBothConfigSurfaces pins that the two config
// endpoints reject the same payloads.
//
// /api/config validates through validateIdentityField and
// /api/claudecode/config had its own inline copy of the same marshal ->
// unmarshal -> Validate shape. Two copies drift, and this one guards the
// control-character rejection that keeps a bad paste from breaking every
// request to the gateway at the transport layer.
func TestIdentityValidationIsTheSameOnBothConfigSurfaces(t *testing.T) {
	cases := []struct {
		name     string
		identity string
		field    string
	}{
		{"control character", `{"userAgent":"claude-cli/2.1.280\r\nX-Injected: 1"}`, "userAgent"},
		{"control character in another field", `{"stainlessOs":"Darwin\nX-Injected: 1"}`, "stainlessOs"},
		{"identity is not an object", `"claude-cli"`, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("claudecode config", func(t *testing.T) {
				srv, _, _ := newTestServerWithManager(t)
				seedCCConfig(t)
				rec := doJSON(t, srv, http.MethodPost, "/api/claudecode/config",
					`{"enabled":true,"identity":`+tc.identity+`}`)
				if rec.Code != http.StatusBadRequest {
					t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
				}
				if tc.field != "" && !strings.Contains(rec.Body.String(), tc.field) {
					t.Errorf("error %q does not name the offending field", rec.Body.String())
				}
			})

			t.Run("endpoint config", func(t *testing.T) {
				srv, _, _ := newTestServerWithManager(t)
				rec := doJSON(t, srv, http.MethodPost, "/api/config",
					`{"customEndpoints":{"m":{"url":"https://example.com/v1/messages","identity":`+tc.identity+`}}}`)
				if rec.Code != http.StatusBadRequest {
					t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
				}
				if tc.field != "" && !strings.Contains(rec.Body.String(), tc.field) {
					t.Errorf("error %q does not name the offending field", rec.Body.String())
				}
				if _, present := config.Get().CustomEndpoints["m"]; present {
					t.Error("the rejected endpoint was stored anyway")
				}
			})
		})
	}
}
