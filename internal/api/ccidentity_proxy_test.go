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

func saveCustomEndpoint(t *testing.T, endpointURL string, extra map[string]any) {
	t.Helper()
	entry := map[string]any{"url": endpointURL, "apiKey": "target-secret-key"}
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
	// The captured OAuth request carries Authorization alone. This endpoint's key
	// is sent as both, and normalization must strip the x-api-key.
	if got.apiKey != "" {
		t.Errorf("x-api-key = %q; the captured request has none", got.apiKey)
	}
	if !strings.Contains(got.authorization, "target-secret-key") {
		t.Errorf("Authorization = %q, want the endpoint key preserved", got.authorization)
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
