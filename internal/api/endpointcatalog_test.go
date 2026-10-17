package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"antigravity-go-proxy/internal/config"
)

// catalogUpstream serves a model catalog on GET /v1/models and records the
// model of every POST /v1/messages. catalog is swappable between requests.
type catalogUpstream struct {
	mu       sync.Mutex
	catalog  string
	status   int
	listHits atomic.Int32
	models   []string
	auth     string
}

func (u *catalogUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	u.mu.Lock()
	defer u.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/v1/models"):
		u.listHits.Add(1)
		u.auth = r.Header.Get("Authorization")
		if u.status != 0 {
			w.WriteHeader(u.status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, u.catalog)
	case r.Method == http.MethodPost:
		body, _ := io.ReadAll(r.Body)
		var parsed struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &parsed)
		u.models = append(u.models, parsed.Model)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"m","type":"message","content":[],"usage":{"input_tokens":1,"output_tokens":1}}`)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (u *catalogUpstream) setCatalog(catalog string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.catalog, u.status = catalog, 0
}

func (u *catalogUpstream) forwardedModels() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.models...)
}

const openAIStyleCatalog = `{"object":"list","data":[{"id":"alpha"},{"id":"beta"},{"id":"gamma"}]}`

func newCatalogEndpointServer(t *testing.T, endpoint map[string]any) (*Server, *catalogUpstream, *httptest.Server) {
	t.Helper()
	withConfigDir(t)
	upstream := &catalogUpstream{catalog: openAIStyleCatalog}
	ts := httptest.NewServer(upstream)
	t.Cleanup(ts.Close)
	endpoint["url"] = ts.URL
	endpoint["discoverModels"] = true
	if _, err := config.Save(map[string]any{"customEndpoints": map[string]any{"alpha": endpoint}}); err != nil {
		t.Fatalf("save config: %v", err)
	}
	return newTestServer(t, &fakeUpstream{streamData: standardStream()}, "test-proj"), upstream, ts
}

func postMessagesModel(t *testing.T, server *Server, model string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(
		`{"model":"`+model+`","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "local-key")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	return rec
}

// Every model the endpoint lists reaches that endpoint, with the client's model
// ID untouched. This is the switching behavior: the config entry is keyed by
// one model, the client names another.
func TestEndpointCatalog_RoutesEveryListedModel(t *testing.T) {
	server, upstream, _ := newCatalogEndpointServer(t, map[string]any{})

	for _, model := range []string{"beta", "gamma", "alpha"} {
		if rec := postMessagesModel(t, server, model); rec.Code != http.StatusOK {
			t.Fatalf("model %s: status %d: %s", model, rec.Code, rec.Body.String())
		}
	}
	got := upstream.forwardedModels()
	if strings.Join(got, ",") != "beta,gamma,alpha" {
		t.Fatalf("forwarded models = %v, want [beta gamma alpha]", got)
	}
	if n := upstream.listHits.Load(); n != 1 {
		t.Errorf("catalog fetched %d times, want once (cold start, then cached)", n)
	}
}

// A model the catalog does not list must not be claimed by the endpoint.
func TestEndpointCatalog_UnlistedModelIsNotRouted(t *testing.T) {
	server, _, _ := newCatalogEndpointServer(t, map[string]any{})
	cfg := config.Get()
	if _, ok := server.resolveCustomEndpoint(context.Background(), cfg, "delta"); ok {
		t.Fatal("delta is not in the catalog but resolved to the endpoint")
	}
	if _, ok := server.resolveCustomEndpoint(context.Background(), cfg, "gamma"); !ok {
		t.Fatal("gamma is in the catalog but did not resolve")
	}
}

// An endpoint that did not opt in is never asked for a catalog.
func TestEndpointCatalog_OptOutNeverFetches(t *testing.T) {
	withConfigDir(t)
	upstream := &catalogUpstream{catalog: openAIStyleCatalog}
	ts := httptest.NewServer(upstream)
	t.Cleanup(ts.Close)
	if _, err := config.Save(map[string]any{"customEndpoints": map[string]any{"alpha": map[string]any{"url": ts.URL}}}); err != nil {
		t.Fatal(err)
	}
	server := newTestServer(t, &fakeUpstream{streamData: standardStream()}, "test-proj")
	if _, ok := server.resolveCustomEndpoint(context.Background(), config.Get(), "beta"); ok {
		t.Fatal("beta resolved without discoverModels")
	}
	if n := upstream.listHits.Load(); n != 0 {
		t.Fatalf("catalog fetched %d times without discoverModels", n)
	}
}

func TestEndpointCatalog_ListedInV1ModelsWithLimits(t *testing.T) {
	_, upstream, _ := newCatalogEndpointServer(t, map[string]any{"apiKey": "ep-secret"})
	server := &Server{backend: &discoveryTestBackend{}, logger: slog.Default(), now: time.Now}
	upstream.setCatalog(`{"data":[{"id":"alpha"},{"id":"beta","display_name":"Beta Model","context_length":131072,"max_output_tokens":8192}]}`)

	entries := discoveryEntriesForID(discoveryModelsRequest(t, server), "beta")
	if len(entries) != 1 {
		t.Fatalf("beta entries = %d, want 1", len(entries))
	}
	entry := entries[0]
	if entry["owned_by"] != "custom" || entry["display_name"] != "Beta Model" {
		t.Errorf("entry = %v", entry)
	}
	if cw, _ := entry["context_window"].(float64); cw != 131072 {
		t.Errorf("context_window = %v, want 131072", entry["context_window"])
	}
	if mo, _ := entry["max_output_tokens"].(float64); mo != 8192 {
		t.Errorf("max_output_tokens = %v, want 8192", entry["max_output_tokens"])
	}
	if upstream.auth != "Bearer ep-secret" {
		t.Errorf("catalog request Authorization = %q, want the endpoint key", upstream.auth)
	}
}

// A catalog that fails to refresh must not unroute models it already listed.
func TestEndpointCatalog_FailedRefreshKeepsPreviousModels(t *testing.T) {
	server, upstream, _ := newCatalogEndpointServer(t, map[string]any{})
	ctx := context.Background()
	cfg := config.Get()
	server.refreshEndpointCatalog(ctx, "alpha", cfg.CustomEndpoints["alpha"], true)

	upstream.mu.Lock()
	upstream.status = http.StatusInternalServerError
	upstream.mu.Unlock()
	server.refreshEndpointCatalog(ctx, "alpha", cfg.CustomEndpoints["alpha"], true)

	if _, ok := server.lookupCustomEndpoint(cfg, "gamma"); !ok {
		t.Fatal("gamma unrouted after a failed refresh")
	}
	status := server.endpointCatalogStatus(cfg)["alpha"].(map[string]any)
	if status["error"] == "" {
		t.Error("status carries no error after a failed refresh")
	}
}

// Repointing an endpoint invalidates its catalog: models listed by the old
// target must not route to the new one.
func TestEndpointCatalog_SourceChangeInvalidates(t *testing.T) {
	server, _, _ := newCatalogEndpointServer(t, map[string]any{})
	cfg := config.Get()
	server.refreshEndpointCatalog(context.Background(), "alpha", cfg.CustomEndpoints["alpha"], true)

	moved := cfg
	moved.CustomEndpoints = map[string]config.EndpointConfig{"alpha": cfg.CustomEndpoints["alpha"]}
	endpoint := moved.CustomEndpoints["alpha"]
	endpoint.URL = "http://127.0.0.1:1"
	moved.CustomEndpoints["alpha"] = endpoint
	if _, ok := server.lookupCustomEndpoint(moved, "gamma"); ok {
		t.Fatal("gamma still routed after the endpoint URL changed")
	}
}

// An entry keyed by the model wins over a catalog that also lists it.
func TestEndpointCatalog_ExplicitEntryWinsOverCatalog(t *testing.T) {
	withConfigDir(t)
	catalogHost := &catalogUpstream{catalog: openAIStyleCatalog}
	catalogTS := httptest.NewServer(catalogHost)
	t.Cleanup(catalogTS.Close)
	pinned := &catalogUpstream{}
	pinnedTS := httptest.NewServer(pinned)
	t.Cleanup(pinnedTS.Close)
	if _, err := config.Save(map[string]any{"customEndpoints": map[string]any{
		"alpha": map[string]any{"url": catalogTS.URL, "discoverModels": true},
		"beta":  map[string]any{"url": pinnedTS.URL},
	}}); err != nil {
		t.Fatal(err)
	}
	server := newTestServer(t, &fakeUpstream{streamData: standardStream()}, "test-proj")
	endpoint, ok := server.resolveCustomEndpoint(context.Background(), config.Get(), "beta")
	if !ok || endpoint.URL != pinnedTS.URL {
		t.Fatalf("beta resolved to %q, want the explicit entry %q", endpoint.URL, pinnedTS.URL)
	}
}

func TestEndpointModelsURL(t *testing.T) {
	for _, tc := range []struct {
		name     string
		endpoint config.EndpointConfig
		want     string
	}{
		{"bare host", config.EndpointConfig{URL: "http://h:8080"}, "http://h:8080/v1/models"},
		{"messages path", config.EndpointConfig{URL: "http://h/v1/messages"}, "http://h/v1/models"},
		{"chat completions", config.EndpointConfig{URL: "http://h/v1/chat/completions"}, "http://h/v1/models"},
		{"v1 base", config.EndpointConfig{URL: "http://h/api/v1"}, "http://h/api/v1/models"},
		{"query kept", config.EndpointConfig{URL: "http://h?api-version=1"}, "http://h/v1/models?api-version=1"},
		{"override", config.EndpointConfig{URL: "http://h", ModelsURL: "https://c/list"}, "https://c/list"},
	} {
		got, err := endpointModelsURL(tc.endpoint)
		if err != nil || got.String() != tc.want {
			t.Errorf("%s: got %v, %v; want %s", tc.name, got, err, tc.want)
		}
	}
	if _, err := endpointModelsURL(config.EndpointConfig{URL: "http://h", ModelsURL: "ftp://x/y"}); err == nil {
		t.Error("non-http modelsUrl accepted")
	}
}

// Anthropic's list is paged (has_more/last_id, default 20 entries); a catalog
// read that stops at page one silently drops models.
func TestFetchEndpointModels_FollowsAnthropicPages(t *testing.T) {
	var afterIDs []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		after := r.URL.Query().Get("after_id")
		afterIDs = append(afterIDs, after)
		if r.URL.Query().Get("limit") != "1000" {
			t.Errorf("limit = %q, want 1000", r.URL.Query().Get("limit"))
		}
		if r.Header.Get("anthropic-version") == "" {
			t.Error("anthropic-version missing")
		}
		if after == "" {
			_, _ = io.WriteString(w, `{"data":[{"id":"m1","max_input_tokens":200000,"max_tokens":64000},{"id":"m2"}],"has_more":true,"last_id":"m2"}`)
			return
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"m3"},{"id":"m1"}],"has_more":false,"last_id":"m3"}`)
	}))
	t.Cleanup(ts.Close)

	models, err := fetchEndpointModels(context.Background(), config.EndpointConfig{URL: ts.URL + "/v1/messages"})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	if strings.Join(ids, ",") != "m1,m2,m3" {
		t.Fatalf("ids = %v, want [m1 m2 m3] (paged, deduplicated)", ids)
	}
	if models[0].ContextLen != 200000 || models[0].MaxOutputTokens != 64000 {
		t.Errorf("limits = %d/%d, want 200000/64000", models[0].ContextLen, models[0].MaxOutputTokens)
	}
	if len(afterIDs) != 2 || afterIDs[1] != "m2" {
		t.Errorf("after_id sequence = %v, want [\"\" m2]", afterIDs)
	}
}

func TestFetchEndpointModels_RejectsNonCatalogs(t *testing.T) {
	for name, body := range map[string]string{
		"empty list": `{"data":[]}`,
		"html":       `<html>nope</html>`,
	} {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) }))
		if _, err := fetchEndpointModels(context.Background(), config.EndpointConfig{URL: ts.URL}); err == nil {
			t.Errorf("%s: accepted as a catalog", name)
		}
		ts.Close()
	}
}

func TestValidateCatalogFields(t *testing.T) {
	for name, tc := range map[string]struct {
		entry map[string]any
		ok    bool
	}{
		"valid":           {map[string]any{"discoverModels": true, "modelsUrl": "https://h/v1/models"}, true},
		"unset":           {map[string]any{}, true},
		"empty modelsUrl": {map[string]any{"modelsUrl": ""}, true},
		"string bool":     {map[string]any{"discoverModels": "yes"}, false},
		"relative url":    {map[string]any{"modelsUrl": "/v1/models"}, false},
		"non-http url":    {map[string]any{"modelsUrl": "file:///etc/passwd"}, false},
		"non-string url":  {map[string]any{"modelsUrl": 5.0}, false},
	} {
		err := validateCustomEndpointLimits(map[string]any{"customEndpoints": map[string]any{"m": tc.entry}})
		if (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok=%v", name, err, tc.ok)
		}
	}
}

// net/http strips Authorization on a cross-host redirect but forwards
// x-api-key, so a followed redirect would hand the endpoint key to whatever
// host the endpoint names. The catalog fetch must not follow redirects at all.
func TestFetchEndpointModels_DoesNotFollowRedirects(t *testing.T) {
	var sinkKey atomic.Value
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sinkKey.Store(r.Header.Get("x-api-key"))
		_, _ = io.WriteString(w, openAIStyleCatalog)
	}))
	t.Cleanup(sink.Close)
	// Another host name for the same listener: the redirect is cross-host.
	target := strings.Replace(sink.URL, "127.0.0.1", "localhost", 1) + "/v1/models"
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target, http.StatusFound)
	}))
	t.Cleanup(source.Close)

	_, err := fetchEndpointModels(context.Background(), config.EndpointConfig{URL: source.URL, APIKey: "ep-secret"})
	if got, _ := sinkKey.Load().(string); got != "" {
		t.Fatalf("redirect target received x-api-key %q", got)
	}
	if err == nil {
		t.Error("a redirect was accepted as a catalog")
	}
}

// After the first fetch fails, the request path stops fetching: a dead
// endpoint must not add a fetch timeout to unrelated requests every retry
// window. The worker owns retries.
func TestEndpointCatalog_FailedFirstFetchLeavesRetriesToWorker(t *testing.T) {
	_, upstream, _ := newCatalogEndpointServer(t, map[string]any{})
	upstream.mu.Lock()
	upstream.status = http.StatusServiceUnavailable
	upstream.mu.Unlock()
	var clock atomic.Value
	clock.Store(time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC))
	advance := func() { clock.Store(clock.Load().(time.Time).Add(endpointCatalogRetry + time.Second)) }
	server := &Server{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		now:    func() time.Time { return clock.Load().(time.Time) },
	}
	cfg := config.Get()
	ctx := context.Background()

	if _, ok := server.resolveCustomEndpoint(ctx, cfg, "beta"); ok {
		t.Fatal("beta resolved from a failing catalog")
	}
	if n := upstream.listHits.Load(); n != 1 {
		t.Fatalf("first request made %d catalog fetches, want 1 (the first fetch)", n)
	}

	advance()
	server.resolveCustomEndpoint(ctx, cfg, "unrelated-model")
	if n := upstream.listHits.Load(); n != 1 {
		t.Fatalf("request path refetched a failed catalog (%d fetches); only the worker may retry", n)
	}

	upstream.setCatalog(openAIStyleCatalog)
	server.refreshEndpointCatalogs(ctx, false)
	if n := upstream.listHits.Load(); n != 2 {
		t.Fatalf("worker pass made %d total fetches, want 2", n)
	}
	if _, ok := server.resolveCustomEndpoint(ctx, cfg, "beta"); !ok {
		t.Fatal("beta not routed after the worker recovered the catalog")
	}
}

// Requests that arrive while the first fetch is in flight join it and route
// once it lands; they do not start fetches of their own. This is what the
// first-request wait exists for, and Task 2 must not lose it.
func TestEndpointCatalog_RequestsJoinTheFirstFetch(t *testing.T) {
	withConfigDir(t)
	var hits atomic.Int32
	release := make(chan struct{})
	var releaseOnce sync.Once
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		<-release
		_, _ = io.WriteString(w, openAIStyleCatalog)
	}))
	t.Cleanup(ts.Close)
	// Registered after ts.Close, so it runs first: Close waits for handlers.
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	if _, err := config.Save(map[string]any{"customEndpoints": map[string]any{
		"alpha": map[string]any{"url": ts.URL, "discoverModels": true},
	}}); err != nil {
		t.Fatal(err)
	}
	server := &Server{logger: slog.New(slog.NewTextHandler(io.Discard, nil)), now: time.Now}
	cfg := config.Get()

	results := make([]bool, 8)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, results[i] = server.resolveCustomEndpoint(context.Background(), cfg, "beta")
		}()
	}
	for hits.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // let the other callers reach the in-flight wait
	releaseOnce.Do(func() { close(release) })
	wg.Wait()

	for i, ok := range results {
		if !ok {
			t.Errorf("caller %d did not route beta after the shared fetch", i)
		}
	}
	if n := hits.Load(); n != 1 {
		t.Errorf("catalog fetched %d times, want 1 shared fetch", n)
	}
}

// A catalog that lists an ID another gateway's allowlist names must not take
// it, on any entry point. gatewayOrder puts custom first so that /v1/messages
// and /v1/models would hand the ID to the catalog without the rule;
// /v1/chat/completions takes the custom shortcut regardless of order.
func TestEndpointCatalog_NeverClaimsAnotherGatewaysModel(t *testing.T) {
	server, upstream, _ := newCatalogEndpointServer(t, map[string]any{})
	if _, err := config.Save(map[string]any{
		"kimi": map[string]any{
			"enabled":   true,
			"baseUrl":   "http://127.0.0.1:1",
			"allowlist": []any{map[string]any{"id": "beta", "enabled": true}},
		},
		"gatewayOrder": map[string]any{"order": []any{"custom", "kimi"}},
	}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Get()

	if _, ok := server.resolveCustomEndpoint(context.Background(), cfg, "beta"); ok {
		t.Error("catalog claimed beta, which the Kimi allowlist owns")
	}
	if _, ok := server.resolveCustomEndpoint(context.Background(), cfg, "gamma"); !ok {
		t.Fatal("gamma, owned by no gateway, did not resolve through the catalog")
	}

	for _, model := range []string{"gamma", "beta"} {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
			`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-api-key", "local-key")
		server.Handler().ServeHTTP(httptest.NewRecorder(), req)
	}
	if got := strings.Join(upstream.forwardedModels(), ","); got != "gamma" {
		t.Errorf("custom endpoint received chat/completions models %q, want only gamma", got)
	}

	discovery := &Server{backend: &discoveryTestBackend{}, logger: slog.Default(), now: time.Now}
	models := discoveryModelsRequest(t, discovery)
	for _, entry := range discoveryEntriesForID(models, "beta") {
		if entry["owned_by"] == "custom" {
			t.Errorf("/v1/models advertises beta as custom: %v", entry)
		}
	}
	if entries := discoveryEntriesForID(models, "gamma"); len(entries) != 1 || entries[0]["owned_by"] != "custom" {
		t.Errorf("gamma entries = %v, want one custom entry", entries)
	}
}
