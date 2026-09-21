package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"antigravity-go-proxy/internal/config"
)

func postConfigGatewayOrder(t *testing.T, srv *Server, gatewayOrderBlob string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"gatewayOrder":` + gatewayOrderBlob + `}`
	request := httptest.NewRequest(http.MethodPost, "/api/config", strings.NewReader(body))
	recorder := httptest.NewRecorder()
	srv.handleConfigSave(recorder, request)
	return recorder
}

func decodeGatewayOrderResponse(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var res map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("response is not JSON: %v\nbody: %s", err, rec.Body.String())
	}
	return res
}

func TestConfigSaveGatewayOrder_Accepts(t *testing.T) {
	cases := []struct {
		name string
		blob string
	}{
		{"full list", `{"order":["kimi","zen","claudecode","openrouter","custom","cloudcode"]}`},
		{"partial order", `{"order":["openrouter"]}`},
		{"mixed-case byModel key", `{"byModel":{"  Claude-Sonnet-5[1M] ":["openrouter"]}}`},
		{"empty order equals unset", `{"order":[]}`},
		{"byModel naming cloudcode", `{"byModel":{"m":["cloudcode","kimi"]}}`},
		{"unknown model name is forward planning", `{"byModel":{"future-model-9":["kimi"]}}`},
		{"case and whitespace IDs normalised", `{"order":[" Kimi ","ZEN"]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, _ := newTestServerWithManager(t)
			rec := postConfigGatewayOrder(t, srv, tc.blob)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestConfigSaveGatewayOrder_Rejects(t *testing.T) {
	cases := []struct {
		name string
		blob string
		want string
	}{
		{"unknown provider", `{"order":["nope","kimi"]}`, "nope"},
		{"duplicate entry", `{"order":["kimi","kimi"]}`, "duplicate"},
		{"duplicate after normalisation", `{"order":["Kimi ","kimi"]}`, "duplicate"},
		{"empty model key", `{"byModel":{"   ":["kimi"]}}`, "empty"},
		{"byModel value not an array", `{"byModel":{"m":"kimi"}}`, "array"},
		{"byModel value not strings", `{"byModel":{"m":[42]}}`, "array"},
		{"order not an array", `{"order":"kimi"}`, "array"},
		{"gatewayOrder not an object", `"kimi"`, "object"},
		{"unknown provider in byModel", `{"byModel":{"m":["nope"]}}`, "nope"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, _ := newTestServerWithManager(t)
			rec := postConfigGatewayOrder(t, srv, tc.blob)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(strings.ToLower(rec.Body.String()), strings.ToLower(tc.want)) {
				t.Errorf("error should name %q, got %s", tc.want, rec.Body.String())
			}
		})
	}
}

func TestConfigSaveGatewayOrder_UnknownProviderErrorListsValidIDs(t *testing.T) {
	srv, _, _ := newTestServerWithManager(t)
	rec := postConfigGatewayOrder(t, srv, `{"order":["nope"]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	for _, id := range []string{"kimi", "zen", "claudecode", "openrouter", "custom", "cloudcode"} {
		if !strings.Contains(rec.Body.String(), id) {
			t.Errorf("error should list %q, got %s", id, rec.Body.String())
		}
	}
}

func TestConfigSaveGatewayOrder_PartialOrderKeepsByModel(t *testing.T) {
	srv, _, _ := newTestServerWithManager(t)
	rec := postConfigGatewayOrder(t, srv, `{"order":["kimi"],"byModel":{"m":["openrouter"]}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("seed status = %d; body = %s", rec.Code, rec.Body.String())
	}
	rec2 := postConfigGatewayOrder(t, srv, `{"order":["openrouter"]}`)
	if rec2.Code != http.StatusOK {
		t.Fatalf("partial status = %d; body = %s", rec2.Code, rec2.Body.String())
	}
	cfg := config.Get()
	if got := cfg.GatewayOrder.Effective("m"); got[0] != config.GatewayOpenRouter {
		t.Errorf("Effective(m)[0] = %v, want openrouter: partial order POST discarded byModel", got[0])
	}
	if got := cfg.GatewayOrder.Effective("other"); got[0] != config.GatewayOpenRouter {
		t.Errorf("Effective(other)[0] = %v, want openrouter", got[0])
	}
}

func TestConfigSaveGatewayOrder_ByModelKeyNormalisedOnSave(t *testing.T) {
	srv, _, _ := newTestServerWithManager(t)
	rec := postConfigGatewayOrder(t, srv, `{"byModel":{"  Claude-Sonnet-5[1M] ":["openrouter"]}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", rec.Code, rec.Body.String())
	}
	cfg := config.Get()
	if got := cfg.GatewayOrder.Effective("claude-sonnet-5"); got[0] != config.GatewayOpenRouter {
		t.Errorf("Effective = %v, want openrouter first", got)
	}
}

func TestConfigSaveGatewayOrder_HotReload(t *testing.T) {
	srv, _, _ := newTestServerWithManager(t)
	rec := postConfigGatewayOrder(t, srv, `{"order":["openrouter","kimi","zen","claudecode","custom","cloudcode"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", rec.Code, rec.Body.String())
	}
	// config.Get() is read once per request, so the saved order governs the
	// next request with no restart and no applyXConfig push.
	if got := config.Get().GatewayOrder.Effective("m"); got[0] != config.GatewayOpenRouter {
		t.Errorf("Effective(m)[0] = %v, want openrouter immediately after save", got[0])
	}
}

func TestConfigGet_ExposesKnownProvidersAndDefaultOrder(t *testing.T) {
	srv, _, _ := newTestServerWithManager(t)
	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	rec := httptest.NewRecorder()
	srv.handleConfigGet(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", rec.Code, rec.Body.String())
	}
	res := decodeGatewayOrderResponse(t, rec)
	inner, ok := res["config"].(map[string]any)
	if !ok {
		t.Fatalf("response has no config object: %s", rec.Body.String())
	}
	known, ok := inner["knownProviders"].([]any)
	if !ok || len(known) != 6 {
		t.Fatalf("knownProviders = %v, want six IDs", inner["knownProviders"])
	}
	for _, id := range []string{"kimi", "zen", "claudecode", "openrouter", "custom", "cloudcode"} {
		found := false
		for _, k := range known {
			if k == id {
				found = true
			}
		}
		if !found {
			t.Errorf("knownProviders missing %q: %v", id, known)
		}
	}
	goCfg, ok := inner["gatewayOrder"].(map[string]any)
	if !ok {
		t.Fatalf("config has no gatewayOrder: %v", inner)
	}
	order, ok := goCfg["order"].([]any)
	if !ok || len(order) == 0 {
		t.Fatalf("gatewayOrder.order = %v, want the default order", goCfg["order"])
	}
	if order[0] != "kimi" {
		t.Errorf("default order[0] = %v, want kimi", order[0])
	}
}
