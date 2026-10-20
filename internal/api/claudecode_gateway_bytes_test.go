package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"antigravity-go-proxy/internal/claudecode"
	"antigravity-go-proxy/internal/config"
)

const (
	gatewayBytesClientModel  = "sonnet-alias"
	gatewayBytesMatchedModel = "claude-sonnet-5"
	gatewayBytesLimit        = 8000

	gatewayBytesOAuthToken  = "sk-ant-oat01-gateway-bytes"
	gatewayBytesAPIKeyToken = "sk-ant-api03-gateway-bytes"
)

// The members a Claude Code harness writes with JSON.stringify: no HTML
// escaping, nested keys in the order the client chose, which is not
// alphabetical. Go's own encoding of the decoded map would escape the angle
// brackets and ampersand and sort every nested object.
const (
	gatewayBytesMessages = `"messages":[{"role":"user","content":[` +
		`{"type":"text","text":"<system-reminder>a & b</system-reminder>"},` +
		`{"type":"text","text":"hi"}]}]`
	gatewayBytesTools = `"tools":[{"name":"t","description":"d > e",` +
		`"input_schema":{"type":"object","properties":{"z":{"type":"string"},"a":{"type":"string"}}}}]`
)

// gatewayUpstream is a Claude Code upstream that keeps the last /v1/messages
// body exactly as it arrived.
type gatewayUpstream struct {
	*httptest.Server
	mu   sync.Mutex
	body string
	hits int
}

func newGatewayUpstream(t *testing.T) *gatewayUpstream {
	t.Helper()
	upstream := &gatewayUpstream{}
	upstream.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path == "/v1/messages" {
			upstream.mu.Lock()
			upstream.body = string(body)
			upstream.hits++
			upstream.mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_ok","type":"message","role":"assistant","model":"m","content":[],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	t.Cleanup(upstream.Close)
	return upstream
}

// sendToGateway posts body to the proxy as a client addressing the Claude Code
// gateway through gatewayBytesClientModel, and returns the body the upstream
// received. modelMapping, when set, is the proxy's own model mapping: it
// rewrites the decoded request, which is what makes the body "mutated".
func sendToGateway(t *testing.T, token string, modelMapping map[string]any, body string) string {
	t.Helper()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", t.TempDir())
	upstream := newGatewayUpstream(t)

	cfg := config.DefaultConfig()
	cfg.ModelMapping = modelMapping
	cfg.ClaudeCode = claudecode.Config{
		Enabled:  true,
		BaseURL:  upstream.URL,
		Mode:     "pool",
		Accounts: []claudecode.AccountConfig{{ID: "acc1", Token: token, Enabled: true}},
		Allowlist: []claudecode.ModelConfig{{
			ID: gatewayBytesMatchedModel, Alias: gatewayBytesClientModel,
			MaxOutputTokens: gatewayBytesLimit, Enabled: true,
		}},
	}
	server := newDispatchServer(t, cfg, &dispatchRecordingBackend{})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "test-proxy-key")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	if upstream.hits != 1 {
		t.Fatalf("upstream served %d requests, want 1", upstream.hits)
	}
	return upstream.body
}

// An API-key account is forwarded as it arrives, so the upstream body is the
// client's bytes with the two values the proxy decides replaced: the matched
// model id and the clamped max_tokens. Nothing else may move, including the
// formatting a re-encoding would normalise (1.0, spaces, the trailing newline).
func TestClaudeCodeGatewayForwardsTheClientBytesToAnAPIKeyAccount(t *testing.T) {
	cases := []struct {
		name   string
		client string
		want   string
	}{
		{
			name: "max_tokens above the limit is clamped in place",
			client: `{"model":"` + gatewayBytesClientModel + `","max_tokens":64000,"temperature":1.0,` +
				gatewayBytesMessages + `,` + gatewayBytesTools + ` }` + "\n",
			want: `{"model":"` + gatewayBytesMatchedModel + `","max_tokens":8000,"temperature":1.0,` +
				gatewayBytesMessages + `,` + gatewayBytesTools + ` }` + "\n",
		},
		{
			name: "max_tokens within the limit keeps the client's own bytes",
			client: `{"model" : "` + gatewayBytesClientModel + `", "max_tokens" : 1024,` +
				gatewayBytesMessages + `}`,
			want: `{"model" : "` + gatewayBytesMatchedModel + `", "max_tokens" : 1024,` +
				gatewayBytesMessages + `}`,
		},
		{
			name: "an omitted max_tokens is appended with the model's limit",
			client: `{"model":"` + gatewayBytesClientModel + `",` +
				gatewayBytesMessages + `}`,
			want: `{"model":"` + gatewayBytesMatchedModel + `",` +
				gatewayBytesMessages + `,"max_tokens":8000}`,
		},
		{
			name: "member order is the client's, not the captured order",
			client: `{"max_tokens":16,"model":"` + gatewayBytesMatchedModel + `",` +
				gatewayBytesMessages + `}`,
			want: `{"max_tokens":16,"model":"` + gatewayBytesMatchedModel + `",` +
				gatewayBytesMessages + `}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sendToGateway(t, gatewayBytesAPIKeyToken, nil, tc.client)
			if got != tc.want {
				t.Errorf("upstream body differs from the client's bytes:\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}

// An OAuth account is normalised: top-level members follow the captured order
// and the client's values are compacted. Normalisation does not re-encode what
// it does not rewrite, so the caller's nested key order and characters reach
// the upstream as sent.
func TestClaudeCodeGatewayNormalisedRequestKeepsTheClientValues(t *testing.T) {
	client := `{"model":"` + gatewayBytesClientModel + `","max_tokens":64000,` +
		gatewayBytesMessages + `,` + gatewayBytesTools + `}`

	got := sendToGateway(t, gatewayBytesOAuthToken, nil, client)

	if !strings.HasPrefix(got, `{"model":"`+gatewayBytesMatchedModel+`",`) {
		t.Errorf("model is not the first member, or is not the matched id: %.120s", got)
	}
	if !strings.Contains(got, `"max_tokens":8000`) {
		t.Errorf("max_tokens was not clamped to %d: %s", gatewayBytesLimit, got)
	}
	for _, want := range []string{gatewayBytesMessages, gatewayBytesTools} {
		if !strings.Contains(got, want) {
			t.Errorf("the client's member did not reach the upstream as sent:\nwant: %s\n got: %s", want, got)
		}
	}
	for _, escaped := range []string{`\u003c`, `\u003e`, `\u0026`} {
		if strings.Contains(got, escaped) {
			t.Errorf("upstream body carries the HTML escape %s: %s", escaped, got)
		}
	}
}

// A body the proxy rewrote is encoded from the decoded map, so its nested keys
// are sorted. It still must not be HTML-escaped: no JavaScript client sends
// \u003c, and the upstream sees every <system-reminder> block.
func TestClaudeCodeGatewayMutatedBodyIsNotHTMLEscaped(t *testing.T) {
	mapping := map[string]any{"mapped-by-proxy": gatewayBytesClientModel}
	client := `{"model":"mapped-by-proxy","max_tokens":64000,` + gatewayBytesMessages + `}`

	for name, token := range map[string]string{
		"api key": gatewayBytesAPIKeyToken,
		"oauth":   gatewayBytesOAuthToken,
	} {
		t.Run(name, func(t *testing.T) {
			got := sendToGateway(t, token, mapping, client)

			if !strings.Contains(got, `"model":"`+gatewayBytesMatchedModel+`"`) {
				t.Errorf("model is not the matched id: %s", got)
			}
			if !strings.Contains(got, `"max_tokens":8000`) {
				t.Errorf("max_tokens was not clamped to %d: %s", gatewayBytesLimit, got)
			}
			if !strings.Contains(got, `<system-reminder>a & b</system-reminder>`) {
				t.Errorf("system-reminder text is not intact: %s", got)
			}
			for _, escaped := range []string{`\u003c`, `\u003e`, `\u0026`} {
				if strings.Contains(got, escaped) {
					t.Errorf("upstream body carries the HTML escape %s: %s", escaped, got)
				}
			}
		})
	}
}
