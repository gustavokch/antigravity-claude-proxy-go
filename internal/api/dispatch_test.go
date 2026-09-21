package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"antigravity-go-proxy/internal/accounts"
	"antigravity-go-proxy/internal/claudecode"
	"antigravity-go-proxy/internal/config"
	proxyformat "antigravity-go-proxy/internal/format"
)

// dispatchModel is the colliding model ID every gateway in the fixture
// accepts. It is in Zen's Anthropic-wire subset, so Zen can claim it.
const dispatchModel = "claude-sonnet-4-6"

// dispatchProbe records which gateway upstream was hit and which model ID
// was forwarded to it.
type dispatchProbe struct {
	mu     sync.Mutex
	hits   map[string]int
	models map[string]string
}

func newDispatchProbe() *dispatchProbe {
	return &dispatchProbe{hits: map[string]int{}, models: map[string]string{}}
}

func (p *dispatchProbe) record(name string, body []byte) {
	var req map[string]any
	_ = json.Unmarshal(body, &req)
	model, _ := req["model"].(string)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.hits[name]++
	p.models[name] = model
}

func (p *dispatchProbe) count(name string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.hits[name]
}

func (p *dispatchProbe) model(name string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.models[name]
}

func (p *dispatchProbe) total() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, c := range p.hits {
		n += c
	}
	return n
}

// probeUpstream returns an httptest server emulating an Anthropic
// /v1/messages endpoint for one gateway. It also serves the Claude Code pool
// (which sends its account token as x-api-key) and the custom-endpoint
// reverse proxy.
func probeUpstream(t *testing.T, probe *dispatchProbe, name string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		// Only dispatch forwards count: the OpenRouter catalog warmup also
		// polls its base URL on other paths and must not pollute the probe.
		if r.URL.Path == "/v1/messages" {
			probe.record(name, body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"msg_probe","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
}

// dispatchUpstreams spins up one upstream per gateway plus a recording
// account-backed backend, and returns the servers alongside the probe.
func dispatchUpstreams(t *testing.T, probe *dispatchProbe) (kimi, zen, cc, or, custom *httptest.Server, backend *dispatchRecordingBackend) {
	t.Helper()
	kimi = probeUpstream(t, probe, "kimi")
	zen = probeUpstream(t, probe, "zen")
	cc = probeUpstream(t, probe, "claudecode")
	or = probeUpstream(t, probe, "openrouter")
	custom = probeUpstream(t, probe, "custom")
	for _, s := range []*httptest.Server{kimi, zen, cc, or, custom} {
		t.Cleanup(s.Close)
	}
	backend = &dispatchRecordingBackend{}
	return kimi, zen, cc, or, custom, backend
}

// dispatchBaseConfig points every gateway at its own upstream, all serving
// the same colliding model ID. This is the first fixture in the repository
// that enables two gateways at the same time.
func dispatchBaseConfig(kimiURL, zenURL, ccURL, orURL, customURL string) config.Config {
	cfg := config.DefaultConfig()
	cfg.Kimi = config.KimiConfig{
		Enabled: true, BaseURL: kimiURL, APIKey: "k-kimi",
		Allowlist: []config.KimiModelConfig{{ID: dispatchModel, Enabled: true}},
	}
	cfg.Zen = config.ZenConfig{
		Enabled: true, BaseURL: zenURL, APIKey: "k-zen",
		Allowlist: []config.ZenModelConfig{{ID: dispatchModel, Enabled: true}},
	}
	cfg.ClaudeCode = claudecode.Config{
		Enabled: true, BaseURL: ccURL, Mode: "pool",
		Accounts:  []claudecode.AccountConfig{{ID: "acc1", Token: "tok", Enabled: true}},
		Allowlist: []claudecode.ModelConfig{{ID: dispatchModel, Enabled: true}},
	}
	cfg.OpenRouter = config.OpenRouterConfig{
		Enabled: true, BaseURL: orURL, APIKey: "k-or",
		Allowlist: []config.OpenRouterModelConfig{{ID: dispatchModel, Enabled: true}},
	}
	cfg.CustomEndpoints = map[string]config.EndpointConfig{
		dispatchModel: {URL: customURL},
	}
	return cfg
}

// newDispatchServer installs cfg, resets cross-test gateway state, and
// returns a server whose account-backed path records into backend.
func newDispatchServer(t *testing.T, cfg config.Config, backend *dispatchRecordingBackend) *Server {
	t.Helper()
	t.Setenv("OPENCODE_API_KEY", "")
	orig := config.Get()
	t.Cleanup(func() { config.SetForTest(orig) })
	config.SetForTest(cfg)
	ccPoolMu.Lock()
	ccPoolInst = nil
	ccHTTPClient = nil
	ccPoolMu.Unlock()
	resetZenKeylessWarning()
	server, err := New(Options{
		APIKey:  "test-proxy-key",
		Backend: backend,
		Builder: proxyformat.NewBuilder(),
		Now:     time.Now,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return server
}

func postDispatchMessages(t *testing.T, server *Server, model string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"model":` + strconv_Quote(model) + `,"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "test-proxy-key")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	return rec
}

func strconv_Quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func requireDispatchWinner(t *testing.T, probe *dispatchProbe, backend *dispatchRecordingBackend, rec *httptest.ResponseRecorder, winner string) {
	t.Helper()
	requireDispatchWinnerDelta(t, probe, nil, backend, rec, winner)
}

// requireDispatchWinnerDelta asserts one request produced exactly one new
// gateway hit, on winner, relative to the snapshot taken before the request
// (nil means "since the probe was created").
func requireDispatchWinnerDelta(t *testing.T, probe *dispatchProbe, before map[string]int, backend *dispatchRecordingBackend, rec *httptest.ResponseRecorder, winner string) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	probe.mu.Lock()
	defer probe.mu.Unlock()
	for name, c := range probe.hits {
		want := before[name]
		if name == winner {
			want++
		}
		if c != want {
			t.Errorf("gateway %q hits = %d, want %d (all hits: %+v)", name, c, want, probe.hits)
		}
	}
	if before[winner] == 0 && probe.hits[winner] != 1 {
		t.Errorf("gateway %q hits = %d, want 1 (all hits: %+v)", winner, probe.hits[winner], probe.hits)
	}
	if backend.hit {
		t.Error("account-backed backend was hit; a gateway should have handled the request")
	}
}

func (p *dispatchProbe) snapshot() map[string]int {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make(map[string]int, len(p.hits))
	for k, v := range p.hits {
		out[k] = v
	}
	return out
}

func TestDispatch_DefaultOrderUnchanged(t *testing.T) {
	cases := []struct {
		name    string
		disable func(*config.Config)
		winner  string
	}{
		{"all enabled, kimi first", func(*config.Config) {}, "kimi"},
		{"kimi disabled, zen wins", func(c *config.Config) { c.Kimi.Enabled = false }, "zen"},
		{"zen disabled, kimi wins", func(c *config.Config) { c.Zen.Enabled = false }, "kimi"},
		{"claudecode disabled, kimi wins", func(c *config.Config) { c.ClaudeCode.Enabled = false }, "kimi"},
		{"openrouter disabled, kimi wins", func(c *config.Config) { c.OpenRouter.Enabled = false }, "kimi"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newDispatchProbe()
			k, z, c, o, cu, b := dispatchUpstreams(t, p)
			ccfg := dispatchBaseConfig(k.URL, z.URL, c.URL, o.URL, cu.URL)
			tc.disable(&ccfg)
			srv := newDispatchServer(t, ccfg, b)
			rec := postDispatchMessages(t, srv, dispatchModel)
			requireDispatchWinner(t, p, b, rec, tc.winner)
		})
	}
}

func TestDispatch_GlobalOrderReorders(t *testing.T) {
	probe := newDispatchProbe()
	kimi, zen, cc, or, custom, backend := dispatchUpstreams(t, probe)
	cfg := dispatchBaseConfig(kimi.URL, zen.URL, cc.URL, or.URL, custom.URL)
	cfg.GatewayOrder.Order = []config.GatewayID{"openrouter", "kimi", "zen", "claudecode", "custom", "cloudcode"}
	server := newDispatchServer(t, cfg, backend)
	rec := postDispatchMessages(t, server, dispatchModel)
	requireDispatchWinner(t, probe, backend, rec, "openrouter")
}

func TestDispatch_PerModelOverrideBeatsGlobal(t *testing.T) {
	probe := newDispatchProbe()
	kimi, zen, cc, or, custom, backend := dispatchUpstreams(t, probe)
	cfg := dispatchBaseConfig(kimi.URL, zen.URL, cc.URL, or.URL, custom.URL)
	cfg.Kimi.Allowlist = append(cfg.Kimi.Allowlist, config.KimiModelConfig{ID: "kimi-only-model", Enabled: true})
	cfg.GatewayOrder.Order = []config.GatewayID{"openrouter", "kimi", "zen", "claudecode", "custom", "cloudcode"}
	cfg.GatewayOrder.ByModel = map[string][]config.GatewayID{
		dispatchModel: {"openrouter", "kimi", "zen", "claudecode", "custom", "cloudcode"},
	}
	server := newDispatchServer(t, cfg, backend)

	rec := postDispatchMessages(t, server, dispatchModel)
	requireDispatchWinner(t, probe, backend, rec, "openrouter")

	before := probe.snapshot()
	rec2 := postDispatchMessages(t, server, "kimi-only-model")
	requireDispatchWinnerDelta(t, probe, before, backend, rec2, "kimi")
}

func TestDispatch_OmittedProviderStillDispatches(t *testing.T) {
	probe := newDispatchProbe()
	kimi, zen, cc, or, custom, backend := dispatchUpstreams(t, probe)
	cfg := dispatchBaseConfig(kimi.URL, zen.URL, cc.URL, or.URL, custom.URL)
	cfg.Kimi.Allowlist = []config.KimiModelConfig{{ID: "kimi-only-model", Enabled: true}}
	cfg.Zen.Allowlist = []config.ZenModelConfig{{ID: "kimi-only-model", Enabled: true}}
	cfg.OpenRouter.Allowlist = []config.OpenRouterModelConfig{{ID: "kimi-only-model", Enabled: true}}
	cfg.ClaudeCode.Allowlist = []claudecode.ModelConfig{{ID: "cc-only-model", Enabled: true}}
	delete(cfg.CustomEndpoints, dispatchModel)
	cfg.CustomEndpoints["kimi-only-model"] = config.EndpointConfig{URL: custom.URL}
	cfg.GatewayOrder.Order = []config.GatewayID{"kimi"}
	server := newDispatchServer(t, cfg, backend)

	rec := postDispatchMessages(t, server, "cc-only-model")
	requireDispatchWinner(t, probe, backend, rec, "claudecode")
}

func TestDispatch_UnknownProviderIDsIgnored(t *testing.T) {
	probe := newDispatchProbe()
	kimi, zen, cc, or, custom, backend := dispatchUpstreams(t, probe)
	cfg := dispatchBaseConfig(kimi.URL, zen.URL, cc.URL, or.URL, custom.URL)
	cfg.GatewayOrder.Order = []config.GatewayID{"nope", "kimi", "zen", "claudecode", "openrouter", "custom", "cloudcode"}
	server := newDispatchServer(t, cfg, backend)
	rec := postDispatchMessages(t, server, dispatchModel)
	requireDispatchWinner(t, probe, backend, rec, "kimi")
}

func TestDispatch_OverrideCannotWidenAMatcher(t *testing.T) {
	probe := newDispatchProbe()
	kimi, zen, cc, or, custom, backend := dispatchUpstreams(t, probe)
	cfg := dispatchBaseConfig(kimi.URL, zen.URL, cc.URL, or.URL, custom.URL)
	cfg.GatewayOrder.ByModel = map[string][]config.GatewayID{
		dispatchModel: {"openrouter"},
	}
	server := newDispatchServer(t, cfg, backend)

	// OpenRouter compares literally, so the [1m] spelling is not its match;
	// Kimi strips the suffix and claims the request.
	rec := postDispatchMessages(t, server, dispatchModel+"[1m]")
	requireDispatchWinner(t, probe, backend, rec, "kimi")
	if got := probe.model("kimi"); got != dispatchModel {
		t.Errorf("forwarded model = %q, want canonical %q", got, dispatchModel)
	}
}

func TestDispatch_1mSuffixOverrideKey(t *testing.T) {
	probe := newDispatchProbe()
	kimi, zen, cc, or, custom, backend := dispatchUpstreams(t, probe)
	cfg := dispatchBaseConfig(kimi.URL, zen.URL, cc.URL, or.URL, custom.URL)
	cfg.GatewayOrder.ByModel = map[string][]config.GatewayID{
		"  Claude-Sonnet-4-6[1M] ": {"kimi", "zen", "claudecode", "openrouter", "custom", "cloudcode"},
	}
	server := newDispatchServer(t, cfg, backend)
	rec := postDispatchMessages(t, server, dispatchModel+"[1m]")
	requireDispatchWinner(t, probe, backend, rec, "kimi")
	if got := probe.model("kimi"); got != dispatchModel {
		t.Errorf("forwarded model = %q, want canonical %q", got, dispatchModel)
	}
}

func TestDispatch_CloudCodeTerminalStopsTheWalk(t *testing.T) {
	probe := newDispatchProbe()
	kimi, zen, cc, or, custom, _ := dispatchUpstreams(t, probe)
	cfg := dispatchBaseConfig(kimi.URL, zen.URL, cc.URL, or.URL, custom.URL)
	cfg.GatewayOrder.Order = []config.GatewayID{"cloudcode", "kimi"}
	// The account-backed path needs a real upstream client, not the
	// recording backend: serve through a fakeUpstream handler.
	t.Setenv("OPENCODE_API_KEY", "")
	orig := config.Get()
	t.Cleanup(func() { config.SetForTest(orig) })
	config.SetForTest(cfg)
	resetZenKeylessWarning()
	upstream := &fakeUpstream{streamData: standardStream()}
	handler := newTestHandler(t, upstream, "test-proj")

	body := `{"model":"` + dispatchModel + `","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "local-key")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if total := probe.total(); total != 0 {
		t.Errorf("gateway hits = %d, want 0: cloudcode stops the walk", total)
	}
	if upstream.streamCalls != 1 {
		t.Errorf("account-backed stream calls = %d, want 1", upstream.streamCalls)
	}
}

func TestDispatch_DisabledGatewaySkippedEvenWhenListed(t *testing.T) {
	probe := newDispatchProbe()
	kimi, zen, cc, or, custom, backend := dispatchUpstreams(t, probe)
	cfg := dispatchBaseConfig(kimi.URL, zen.URL, cc.URL, or.URL, custom.URL)
	cfg.Kimi.Enabled = false
	cfg.GatewayOrder.Order = []config.GatewayID{"kimi", "zen", "claudecode", "openrouter", "custom", "cloudcode"}
	server := newDispatchServer(t, cfg, backend)
	rec := postDispatchMessages(t, server, dispatchModel)
	requireDispatchWinner(t, probe, backend, rec, "zen")
}

func TestDispatch_CustomEndpointHonorsOrder(t *testing.T) {
	for _, tc := range []struct {
		name   string
		order  []config.GatewayID
		winner string
	}{
		{"custom first", []config.GatewayID{"custom", "kimi", "zen", "claudecode", "openrouter", "cloudcode"}, "custom"},
		{"kimi first", []config.GatewayID{"kimi", "custom", "zen", "claudecode", "openrouter", "cloudcode"}, "kimi"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probe := newDispatchProbe()
			kimi, zen, cc, or, custom, backend := dispatchUpstreams(t, probe)
			cfg := dispatchBaseConfig(kimi.URL, zen.URL, cc.URL, or.URL, custom.URL)
			cfg.GatewayOrder.Order = tc.order
			server := newDispatchServer(t, cfg, backend)
			rec := postDispatchMessages(t, server, dispatchModel)
			requireDispatchWinner(t, probe, backend, rec, tc.winner)
		})
	}
}

func TestDispatch_OpenAIChatCompletionsSharesTheOrder(t *testing.T) {
	probe := newDispatchProbe()
	kimi, zen, cc, or, custom, backend := dispatchUpstreams(t, probe)
	cfg := dispatchBaseConfig(kimi.URL, zen.URL, cc.URL, or.URL, custom.URL)
	delete(cfg.CustomEndpoints, dispatchModel)
	cfg.GatewayOrder.Order = []config.GatewayID{"openrouter", "kimi", "zen", "claudecode", "custom", "cloudcode"}
	server := newDispatchServer(t, cfg, backend)

	body := `{"model":"` + dispatchModel + `","max_tokens":1024,"messages":[{"role":"user","content":"Hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-proxy-key")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if got := probe.count("openrouter"); got != 1 {
		t.Errorf("openrouter hits = %d, want 1 (all hits: %+v)", got, probe.hits)
	}
	if total := probe.total(); total != 1 {
		t.Errorf("total gateway hits = %d, want exactly 1", total)
	}
}

func TestDispatch_ForwardedModelUsesProviderCanonicalID(t *testing.T) {
	t.Run("kimi alias forwards canonical ID", func(t *testing.T) {
		probe := newDispatchProbe()
		kimi, zen, cc, or, custom, backend := dispatchUpstreams(t, probe)
		cfg := dispatchBaseConfig(kimi.URL, zen.URL, cc.URL, or.URL, custom.URL)
		cfg.Kimi.Allowlist = []config.KimiModelConfig{{ID: dispatchModel, Alias: "k2-alias", Enabled: true}}
		cfg.Zen.Enabled = false
		cfg.ClaudeCode.Enabled = false
		cfg.OpenRouter.Enabled = false
		delete(cfg.CustomEndpoints, dispatchModel)
		server := newDispatchServer(t, cfg, backend)
		rec := postDispatchMessages(t, server, "k2-alias")
		requireDispatchWinner(t, probe, backend, rec, "kimi")
		if got := probe.model("kimi"); got != dispatchModel {
			t.Errorf("forwarded model = %q, want canonical %q", got, dispatchModel)
		}
	})

	t.Run("zen opencode prefix forwards canonical spelling", func(t *testing.T) {
		probe := newDispatchProbe()
		kimi, zen, cc, or, custom, backend := dispatchUpstreams(t, probe)
		cfg := dispatchBaseConfig(kimi.URL, zen.URL, cc.URL, or.URL, custom.URL)
		cfg.Kimi.Enabled = false
		cfg.ClaudeCode.Enabled = false
		cfg.OpenRouter.Enabled = false
		delete(cfg.CustomEndpoints, dispatchModel)
		server := newDispatchServer(t, cfg, backend)
		rec := postDispatchMessages(t, server, "opencode/"+dispatchModel)
		requireDispatchWinner(t, probe, backend, rec, "zen")
		if got := probe.model("zen"); got != dispatchModel {
			t.Errorf("forwarded model = %q, want canonical %q", got, dispatchModel)
		}
	})
}

func TestDispatch_ZenKeylessConfigFallsThrough(t *testing.T) {
	probe := newDispatchProbe()
	kimi, zen, cc, or, custom, backend := dispatchUpstreams(t, probe)
	cfg := dispatchBaseConfig(kimi.URL, zen.URL, cc.URL, or.URL, custom.URL)
	cfg.Zen.APIKey = ""
	cfg.GatewayOrder.Order = []config.GatewayID{"zen", "kimi", "claudecode", "openrouter", "custom", "cloudcode"}
	server := newDispatchServer(t, cfg, backend)
	rec := postDispatchMessages(t, server, dispatchModel)
	requireDispatchWinner(t, probe, backend, rec, "kimi")
}

func TestDispatch_ZenNonWireEntryFallsThrough(t *testing.T) {
	probe := newDispatchProbe()
	kimi, zen, cc, or, custom, backend := dispatchUpstreams(t, probe)
	cfg := dispatchBaseConfig(kimi.URL, zen.URL, cc.URL, or.URL, custom.URL)
	const nonWire = "gpt-zzz-nonwire"
	cfg.Zen.Allowlist = []config.ZenModelConfig{{ID: nonWire, Enabled: true}}
	cfg.Kimi.Allowlist = []config.KimiModelConfig{{ID: nonWire, Enabled: true}}
	cfg.ClaudeCode.Enabled = false
	cfg.OpenRouter.Enabled = false
	cfg.CustomEndpoints = map[string]config.EndpointConfig{}
	cfg.GatewayOrder.Order = []config.GatewayID{"zen", "kimi", "claudecode", "openrouter", "custom", "cloudcode"}
	server := newDispatchServer(t, cfg, backend)
	rec := postDispatchMessages(t, server, nonWire)
	requireDispatchWinner(t, probe, backend, rec, "kimi")
}

func TestGatewayHandlers_CoverEveryKnownGateway(t *testing.T) {
	if _, ok := gatewayHandlers[config.GatewayCloudCode]; ok {
		t.Error("cloudcode must have no handler: it is the terminal account-backed path")
	}
	for _, id := range config.KnownGatewayIDs() {
		if id == config.GatewayCloudCode {
			continue
		}
		if _, ok := gatewayHandlers[id]; !ok {
			t.Errorf("known gateway %q has no handler", id)
		}
	}
	for id := range gatewayHandlers {
		if !config.IsKnownGatewayID(id) {
			t.Errorf("handler for unknown gateway %q", id)
		}
	}
}

func TestDispatch_ClassifierFallbackStillLosesToGatewaysUnderDefaultOrder(t *testing.T) {
	t.Setenv("ANTIGRAVITY_PROXY_CLASSIFIER_FALLBACK", "1")
	probe := newDispatchProbe()
	kimi, zen, cc, or, custom, _ := dispatchUpstreams(t, probe)
	cfg := dispatchBaseConfig(kimi.URL, zen.URL, cc.URL, or.URL, custom.URL)
	cfg.Classifier.Enabled = true
	manager, err := accounts.New(accounts.Options{Accounts: []*accounts.Account{}})
	if err != nil {
		t.Fatalf("accounts.New: %v", err)
	}
	t.Setenv("OPENCODE_API_KEY", "")
	orig := config.Get()
	t.Cleanup(func() { config.SetForTest(orig) })
	config.SetForTest(cfg)
	resetZenKeylessWarning()
	server, err := New(Options{
		APIKey: "test-proxy-key", Backend: &dispatchRecordingBackend{},
		Builder: proxyformat.NewBuilder(), Now: time.Now, AccountManager: manager,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	backend := server.backend.(*dispatchRecordingBackend)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(classifierShapedBody(t, dispatchModel, classifierStage1Footer))))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "test-proxy-key")
	server.Handler().ServeHTTP(rec, req)

	requireDispatchWinner(t, probe, backend, rec, "kimi")
	if strings.Contains(rec.Body.String(), "<severity>0</severity>") {
		t.Error("response is a classifier stub; the gateway should have won")
	}
}

func TestDispatch_ClassifierRerouteChangesTheOverrideKey(t *testing.T) {
	probe := newDispatchProbe()
	kimi, zen, cc, or, custom, backend := dispatchUpstreams(t, probe)
	cfg := dispatchBaseConfig(kimi.URL, zen.URL, cc.URL, or.URL, custom.URL)
	const target = "reroute-target-model"
	cfg.Kimi.Allowlist = []config.KimiModelConfig{{ID: target, Enabled: true}}
	cfg.Zen.Allowlist = []config.ZenModelConfig{{ID: target, Enabled: true}}
	cfg.OpenRouter.Allowlist = []config.OpenRouterModelConfig{{ID: target, Enabled: true}}
	cfg.ClaudeCode.Allowlist = []claudecode.ModelConfig{{ID: target, Enabled: true}}
	cfg.CustomEndpoints = map[string]config.EndpointConfig{}
	cfg.Classifier.Enabled = true
	cfg.Classifier.Action = config.ActionRerouteOnly
	cfg.Classifier.Variants = map[string]config.ClassifierVariantConfig{
		// MaxTokens travels with the reroute: the shaped body carries none
		// and the target model has no known limit.
		"stage1-severity": {TargetModel: target, MaxTokens: 64},
	}
	cfg.GatewayOrder.Order = []config.GatewayID{"kimi", "zen", "claudecode", "openrouter", "custom", "cloudcode"}
	cfg.GatewayOrder.ByModel = map[string][]config.GatewayID{
		target: {"openrouter", "kimi", "zen", "claudecode", "custom", "cloudcode"},
	}
	server := newDispatchServer(t, cfg, backend)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(classifierShapedBody(t, "source-model", classifierStage1Footer))))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "test-proxy-key")
	server.Handler().ServeHTTP(rec, req)
	requireDispatchWinner(t, probe, backend, rec, "openrouter")
	if got := probe.model("openrouter"); got != target {
		t.Errorf("forwarded model = %q, want reroute target %q", got, target)
	}
}

func TestDispatch_HotReload(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CONFIG_DIR", tmpDir)
	t.Setenv("HOME", tmpDir)
	t.Setenv("OPENCODE_API_KEY", "")
	orig := config.Get()
	t.Cleanup(func() { config.SetForTest(orig) })

	probe := newDispatchProbe()
	kimi, zen, cc, or, custom, backend := dispatchUpstreams(t, probe)
	base := dispatchBaseConfig(kimi.URL, zen.URL, cc.URL, or.URL, custom.URL)
	if _, err := config.Save(map[string]any{
		"kimi": map[string]any{
			"enabled": true, "apiKey": "k-kimi", "baseUrl": kimi.URL,
			"allowlist": []map[string]any{{"id": dispatchModel, "enabled": true}},
		},
		"zen": map[string]any{
			"enabled": true, "apiKey": "k-zen", "baseUrl": zen.URL,
			"allowlist": []map[string]any{{"id": dispatchModel, "enabled": true}},
		},
		"claudecode": map[string]any{
			"enabled": true, "baseUrl": cc.URL, "mode": "pool",
			"accounts":  []map[string]any{{"id": "acc1", "token": "tok", "enabled": true}},
			"allowlist": []map[string]any{{"id": dispatchModel, "enabled": true}},
		},
		"openrouter": map[string]any{
			"enabled": true, "apiKey": "k-or", "baseUrl": or.URL,
			"allowlist": []map[string]any{{"id": dispatchModel, "enabled": true}},
		},
		"customEndpoints": map[string]any{
			dispatchModel: map[string]any{"url": custom.URL},
		},
	}); err != nil {
		t.Fatalf("config.Save: %v", err)
	}
	_ = base

	ccPoolMu.Lock()
	ccPoolInst = nil
	ccHTTPClient = nil
	ccPoolMu.Unlock()
	resetZenKeylessWarning()
	server, err := New(Options{
		APIKey: "test-proxy-key", Backend: backend,
		Builder: proxyformat.NewBuilder(), Now: time.Now,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rec := postDispatchMessages(t, server, dispatchModel)
	requireDispatchWinner(t, probe, backend, rec, "kimi")

	if _, err := config.Save(map[string]any{
		"gatewayOrder": map[string]any{
			"order": []string{"openrouter", "kimi", "zen", "claudecode", "custom", "cloudcode"},
		},
	}); err != nil {
		t.Fatalf("config.Save order: %v", err)
	}

	before := probe.snapshot()
	rec2 := postDispatchMessages(t, server, dispatchModel)
	requireDispatchWinnerDelta(t, probe, before, backend, rec2, "openrouter")
}


