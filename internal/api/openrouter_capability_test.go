package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"antigravity-go-proxy/internal/openrouter"
)

// toolRoutingEndpoints ranks a tool-incapable provider first, mirroring the
// live minimax/minimax-m3 catalog where GMICloud outranks every provider that
// can actually serve tools.
func toolRoutingEndpoints() []openrouter.ProviderEndpoint {
	return []openrouter.ProviderEndpoint{
		{ProviderName: "notools", ContextLength: 1000000, UptimeLast5m: 0.99, UptimeLast30m: 0.99, UptimeLast1d: 0.99,
			SupportedParameters: []string{"max_tokens", "temperature"}},
		{ProviderName: "tools", ContextLength: 500000, UptimeLast5m: 0.98, UptimeLast30m: 0.98, UptimeLast1d: 0.98,
			SupportedParameters: []string{"max_tokens", "tools", "tool_choice"},
			SupportsToolChoice:  &openrouter.ToolChoiceSupport{None: true, Auto: true, Required: true, Function: true}},
	}
}

// providerRecorder captures the provider order injected into each upstream call.
func providerRecorder(t *testing.T, orders *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		prov, _ := body["provider"].(map[string]any)
		order, _ := prov["order"].([]any)
		if len(order) > 0 {
			s, _ := order[0].(string)
			*orders = append(*orders, s)
		} else {
			*orders = append(*orders, "")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_ok","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
}

func doToolsRequest(t *testing.T, server *Server, toolChoice string) *httptest.ResponseRecorder {
	t.Helper()
	choice := ""
	if toolChoice != "" {
		choice = `,"tool_choice":{"type":"` + toolChoice + `"}`
	}
	payload := `{"model":"claude-3-7-openrouter","max_tokens":1024,` +
		`"messages":[{"role":"user","content":"Hello"}],` +
		`"tools":[{"name":"final_answer","input_schema":{"type":"object"}}]` + choice + `}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "test-proxy-key")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	return rec
}

func TestOpenRouterRouting_ToolsRequestSkipsIncapablePin(t *testing.T) {
	var orders []string
	mockOR := providerRecorder(t, &orders)
	defer mockOR.Close()

	env := setupRoutingTestEnv(t, mockOR.URL, toolRoutingEndpoints())
	env.saveConfig(t, map[string]any{"providerMode": "pinned", "pinnedProvider": "notools"}, nil)
	server := env.newServer(t)

	rec := doToolsRequest(t, server, "any")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(orders) != 1 || orders[0] != "tools" {
		t.Errorf("pinned tool-incapable provider must be replaced; provider orders = %v", orders)
	}
}

func TestOpenRouterRouting_ToolsRequestSkipsIncapableTopRank(t *testing.T) {
	var orders []string
	mockOR := providerRecorder(t, &orders)
	defer mockOR.Close()

	env := setupRoutingTestEnv(t, mockOR.URL, toolRoutingEndpoints())
	env.saveConfig(t, nil, nil) // auto mode
	server := env.newServer(t)

	rec := doToolsRequest(t, server, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(orders) != 1 || orders[0] != "tools" {
		t.Errorf("auto chain must start at a tool-capable provider; provider orders = %v", orders)
	}
}

func TestOpenRouterRouting_NonToolRequestKeepsPin(t *testing.T) {
	var orders []string
	mockOR := providerRecorder(t, &orders)
	defer mockOR.Close()

	env := setupRoutingTestEnv(t, mockOR.URL, toolRoutingEndpoints())
	env.saveConfig(t, map[string]any{"providerMode": "pinned", "pinnedProvider": "notools"}, nil)
	server := env.newServer(t)

	rec := env.doRequest(t, server, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(orders) != 1 || orders[0] != "notools" {
		t.Errorf("request without tools must keep the pin; provider orders = %v", orders)
	}
}

func TestOpenRouterRouting_UpstreamErrorBodyKeepsRoutingFunnel(t *testing.T) {
	funnel := `{"type":"error","error":{"type":"not_found_error","message":"No endpoints found for minimax/minimax-m3.","error_type":"not_found"},"request_id":"gen-123","metadata":{"routing_funnel":[{"step":"Initial Endpoints","endpoint_count":12},{"step":"Supported Parameters","endpoint_count":0,"dropped":["tools"]}]}}`
	mockOR := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(funnel))
	}))
	defer mockOR.Close()

	env := setupRoutingTestEnv(t, mockOR.URL, toolRoutingEndpoints())
	env.saveConfig(t, nil, map[string]any{"maxRetries": 1, "backoffBaseMs": 1, "backoffCapMs": 2})
	server := env.newServer(t)

	rec := env.doRequest(t, server, nil)
	if rec.Code == http.StatusOK {
		t.Fatalf("expected upstream failure to surface, got 200")
	}
	if !strings.Contains(rec.Body.String(), "Supported Parameters") {
		t.Errorf("routing_funnel step must survive truncation, got: %s", rec.Body.String())
	}
}

func TestTruncateCutsOnRuneBoundary(t *testing.T) {
	s := strings.Repeat("é", 10) // 2 bytes per rune, 20 bytes total
	got := truncate(s, 5)        // byte offset 5 lands inside a rune
	body := strings.TrimSuffix(got, "…")
	if !utf8.ValidString(body) {
		t.Errorf("truncate produced invalid UTF-8: %q", body)
	}
	if len(body) > 5 {
		t.Errorf("truncate must not exceed the byte limit, got %d bytes", len(body))
	}
	if plain := truncate("abc", 5); plain != "abc" {
		t.Errorf("short strings pass through unchanged, got %q", plain)
	}
}
