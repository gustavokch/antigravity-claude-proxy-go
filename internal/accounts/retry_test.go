package accounts

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"antigravity-go-proxy/internal/auth"
	"antigravity-go-proxy/internal/cloudcode"
	"antigravity-go-proxy/internal/config"
)

func TestParseResetTimeAndClassifiers(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		header http.Header
		body   string
		want   time.Duration
	}{
		{name: "retry seconds", header: http.Header{"Retry-After": {"12"}}, want: 12 * time.Second},
		{name: "retry date", header: http.Header{"Retry-After": {now.Add(45 * time.Second).Format(http.TimeFormat)}}, want: 45 * time.Second},
		{name: "unix reset", header: http.Header{"X-Ratelimit-Reset": {fmt.Sprint(now.Add(time.Minute).Unix())}}, want: time.Minute},
		{name: "quota delay milliseconds", body: `{"quotaResetDelay":"2500ms"}`, want: 2500 * time.Millisecond},
		{name: "quota delay seconds", body: `quotaResetDelay: 3.5s`, want: 3500 * time.Millisecond},
		{name: "human duration", body: `please retry in 1h2m3s`, want: time.Hour + 2*time.Minute + 3*time.Second},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := ParseResetTime(test.header, test.body, now); got != test.want {
				t.Fatalf("got %s, want %s", got, test.want)
			}
		})
	}
	if ClassifyError(`RESOURCE_EXHAUSTED quotaResetDelay`, 429) != ReasonQuota {
		t.Fatal("quota response was not classified as quota")
	}
	bareRPM := `{"error":{"code":429,"message":"Resource has been exhausted (e.g. check quota).","status":"RESOURCE_EXHAUSTED"}}`
	if ClassifyError(bareRPM, 429) != ReasonRateLimit {
		t.Fatalf("bare RESOURCE_EXHAUSTED classified as %s, want RATE_LIMIT_EXCEEDED", ClassifyError(bareRPM, 429))
	}
	if wait := SmartBackoff(ReasonRateLimit, 0, 0); wait != 30*time.Second {
		t.Fatalf("RPM backoff with failures=0 was %s, want 30s", wait)
	}
	if ClassifyError(`MODEL_CAPACITY_EXHAUSTED`, 429) != ReasonCapacity {
		t.Fatal("capacity response was not classified as capacity")
	}
	if !IsPermanentAuthFailure(`{"error":"invalid_grant"}`) || !IsAccountBanned("Account has been disabled for violation of Terms of Service") {
		t.Fatal("permanent failure classifiers did not match")
	}
	body := `{"error":{"status":"PERMISSION_DENIED","details":[{"metadata":{"reason":"VALIDATION_REQUIRED","validation_url":"https://accounts.google.com/signin/continue?x=1"}}]}}`
	if !IsValidationRequired(body) || ExtractVerificationURL(body) != "https://accounts.google.com/signin/continue?x=1" {
		t.Fatalf("verification parsing failed: %q", ExtractVerificationURL(body))
	}
}

type staticResolver struct {
	tokens      map[string]string
	invalidated []string
	mu          sync.Mutex
}

func (resolver *staticResolver) Resolve(_ context.Context, account *Account) (auth.Credentials, error) {
	token := resolver.tokens[account.Email]
	if token == "" {
		return auth.Credentials{}, fmt.Errorf("missing token for %s", account.Email)
	}
	return auth.Credentials{AccessToken: token, Email: account.Email, Expiry: time.Now().Add(time.Hour)}, nil
}

func (resolver *staticResolver) Invalidate(email string) {
	resolver.mu.Lock()
	defer resolver.mu.Unlock()
	resolver.invalidated = append(resolver.invalidated, email)
}

type scriptedResult struct {
	events [][]byte
	err    error
}

type scriptedClient struct {
	mu            sync.Mutex
	results       []scriptedResult
	calls         int
	payload       map[string]any
	requestOption cloudcode.RequestOptions
	modelsBody    []byte
}

func (client *scriptedClient) LoadCodeAssist(context.Context, string) (cloudcode.Response, error) {
	return cloudcode.Response{StatusCode: http.StatusOK, Body: []byte(`{"cloudaicompanionProject":{"id":"discovered"}}`)}, nil
}

func (client *scriptedClient) FetchAvailableModels(context.Context, string) (cloudcode.Response, error) {
	body := client.modelsBody
	if len(body) == 0 {
		body = []byte(`{
			"defaultAgentModelId":"claude-sonnet-4-6",
			"agentModelSorts":[{"groups":[{"modelIds":["claude-sonnet-4-6"]}]}],
			"models":{"claude-sonnet-4-6":{"displayName":"Claude Sonnet 4.6 (Thinking)","supportsThinking":true,"thinkingBudget":1024,"maxTokens":250000,"maxOutputTokens":64000}}
		}`)
	}
	return cloudcode.Response{StatusCode: http.StatusOK, Body: body}, nil
}

func (client *scriptedClient) StreamGenerateContent(_ context.Context, payload any, options cloudcode.RequestOptions, consume func(cloudcode.SSEEvent) error) (cloudcode.Response, error) {
	client.mu.Lock()
	index := client.calls
	client.calls++
	if object, ok := payload.(map[string]any); ok {
		client.payload = object
	}
	client.requestOption = options
	result := client.results[min(index, len(client.results)-1)]
	client.mu.Unlock()
	for _, data := range result.events {
		if err := consume(cloudcode.SSEEvent{Data: data}); err != nil {
			return cloudcode.Response{StatusCode: http.StatusOK}, err
		}
	}
	return cloudcode.Response{Endpoint: cloudcode.DailyEndpoint, StatusCode: http.StatusOK}, result.err
}

func TestFetchAvailableModelsTransientCredentialErrorDoesNotBlockCatalog(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	account := testAccount("transient@example.com")
	manager, err := New(Options{Accounts: []*Account{account}, Strategy: StrategyHybrid, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	// Resolver fails with a transient credential error. The dispatcher should
	// retry, but it must not leave the account invalid or rate-limited on model
	// "" (which Select("") checks and would make catalog refresh fail with
	// "no accounts available").
	dispatcher := newTestDispatcher(t, manager, &staticResolver{}, map[string]*scriptedClient{}, now, func(context.Context, time.Duration) error { return nil })
	_, err = dispatcher.FetchAvailableModels(context.Background())
	if err == nil {
		t.Fatal("expected error from exhausted retries")
	}
	if strings.Contains(err.Error(), "no accounts available") {
		t.Fatalf("unexpected 'no accounts available' from empty-model rate limit: %v", err)
	}
	snapshot := manager.Snapshot()[0]
	if snapshot.Invalid {
		t.Fatalf("account was permanently invalidated by a catalog refresh failure: %#v", snapshot)
	}
	if _, ok := snapshot.Limits[""]; ok {
		t.Fatalf("empty-model rate limit was created: %#v", snapshot.Limits)
	}
}

func TestDispatcherUsesAgyAgentRouteAndLiveOutputLimit(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 15, 4, 0, 0, 0, time.UTC)
	account := testAccount("models@example.com")
	manager, err := New(Options{Accounts: []*Account{account}, Strategy: StrategySticky, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	modelsBody := []byte(`{
		"agentModelSorts":[{"groups":[{"modelIds":["gemini-pro-agent","claude-opus-4-6-thinking"]}]}],
		"models":{
			"gemini-3.1-pro-high":{"displayName":"Gemini 3.1 Pro (High)","supportsThinking":true,"thinkingBudget":10001,"maxOutputTokens":65535},
			"gemini-pro-agent":{"displayName":"Gemini 3.1 Pro (High)","supportsThinking":true,"thinkingBudget":10001,"maxTokens":1048576,"maxOutputTokens":65535},
			"claude-opus-4-6-thinking":{"displayName":"Claude Opus 4.6 (Thinking)","supportsThinking":true,"thinkingBudget":1024,"maxTokens":250000,"maxOutputTokens":64000}
		}
	}`)
	client := &scriptedClient{modelsBody: modelsBody, results: []scriptedResult{{events: [][]byte{[]byte(`{}`)}}, {events: [][]byte{[]byte(`{}`)}}}}
	dispatcher := newTestDispatcher(t, manager, &staticResolver{tokens: map[string]string{account.Email: "token"}}, map[string]*scriptedClient{"token": client}, now, func(context.Context, time.Duration) error { return nil })

	request := testRequest()
	request["model"] = "gemini-3.1-pro-high"
	if _, err := dispatcher.StreamGenerateContent(context.Background(), request, func(cloudcode.SSEEvent) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if client.payload["model"] != "gemini-pro-agent" {
		t.Fatalf("upstream model=%v", client.payload["model"])
	}

	request["model"] = "claude-opus-4-6-thinking"
	request["max_tokens"] = float64(128000)
	request["thinking"] = map[string]any{"type": "adaptive", "display": "summarized"}
	if _, err := dispatcher.StreamGenerateContent(context.Background(), request, func(cloudcode.SSEEvent) error { return nil }); err != nil {
		t.Fatal(err)
	}
	inner := client.payload["request"].(map[string]any)
	generation := inner["generationConfig"].(map[string]any)
	if generation["maxOutputTokens"] != 64000 {
		t.Fatalf("maxOutputTokens=%v", generation["maxOutputTokens"])
	}
	thinking := generation["thinkingConfig"].(map[string]any)
	if thinking["thinking_budget"] != 1024 {
		t.Fatalf("thinkingConfig=%#v", thinking)
	}
	if client.requestOption.Headers.Get("anthropic-beta") != "interleaved-thinking-2025-05-14" {
		t.Fatalf("headers=%v", client.requestOption.Headers)
	}
}

func TestForcedQuota429CoolsDownAndRotates(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	first := testAccount("first@example.com")
	second := testAccount("second@example.com")
	manager, err := New(Options{Accounts: []*Account{first, second}, Strategy: StrategySticky, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	resolver := &staticResolver{tokens: map[string]string{first.Email: "first-token", second.Email: "second-token"}}
	quotaError := &cloudcode.HTTPError{
		Endpoint: cloudcode.DailyEndpoint, StatusCode: http.StatusTooManyRequests, Status: "429 Too Many Requests",
		Header: http.Header{"Retry-After": {"60"}}, Body: `{"error":{"status":"RESOURCE_EXHAUSTED","message":"quota exhausted"}}`,
	}
	clients := map[string]*scriptedClient{
		"first-token":  {results: []scriptedResult{{err: quotaError}}},
		"second-token": {results: []scriptedResult{{events: [][]byte{[]byte(`{"response":{"candidates":[]}}`)}}}},
	}
	var sleeps []time.Duration
	dispatcher := newTestDispatcher(t, manager, resolver, clients, now, func(_ context.Context, duration time.Duration) error {
		sleeps = append(sleeps, duration)
		return nil
	})
	var events int
	response, err := dispatcher.StreamGenerateContent(context.Background(), testRequest(), func(cloudcode.SSEEvent) error {
		events++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || clients["first-token"].calls != 1 || clients["second-token"].calls != 1 || events != 1 {
		t.Fatalf("response=%#v first=%d second=%d events=%d", response, clients["first-token"].calls, clients["second-token"].calls, events)
	}
	limit := manager.Snapshot()[0].Limits["claude-sonnet-4-6"]
	if !limit.IsRateLimited || limit.ActualResetMS != time.Minute.Milliseconds() {
		t.Fatalf("first account cooldown=%#v", limit)
	}
	if len(sleeps) != 1 || sleeps[0] != 5*time.Second {
		t.Fatalf("switch sleeps=%v", sleeps)
	}
	if clients["second-token"].payload["project"] != second.ProjectID {
		t.Fatalf("rotated payload project=%v", clients["second-token"].payload["project"])
	}
}

func TestCapacityRetriesSameAccountWithTieredBackoff(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	account := testAccount("capacity@example.com")
	manager, err := New(Options{Accounts: []*Account{account}, Strategy: StrategySticky, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	resolver := &staticResolver{tokens: map[string]string{account.Email: "token"}}
	capacityError := &cloudcode.HTTPError{StatusCode: http.StatusTooManyRequests, Status: "429", Body: `{"error":"MODEL_CAPACITY_EXHAUSTED"}`, Header: make(http.Header)}
	client := &scriptedClient{results: []scriptedResult{{err: capacityError}, {err: capacityError}, {events: [][]byte{[]byte(`{}`)}}}}
	var sleeps []time.Duration
	dispatcher := newTestDispatcher(t, manager, resolver, map[string]*scriptedClient{"token": client}, now, func(_ context.Context, duration time.Duration) error {
		sleeps = append(sleeps, duration)
		return nil
	})
	dispatcher.capacityBackoffs = []time.Duration{time.Second, 2 * time.Second}
	if _, err := dispatcher.StreamGenerateContent(context.Background(), testRequest(), func(cloudcode.SSEEvent) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if client.calls != 3 || !reflect.DeepEqual(sleeps, []time.Duration{time.Second, 2 * time.Second}) {
		t.Fatalf("calls=%d sleeps=%v", client.calls, sleeps)
	}
	if len(manager.Snapshot()[0].Limits) != 0 {
		t.Fatalf("successful capacity retry left a cooldown: %#v", manager.Snapshot()[0])
	}
}

func TestVerificationAndPermanentAuthFailuresInvalidateAndRotate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		failure    *cloudcode.HTTPError
		wantReason string
		wantURL    string
		wantToken  bool
	}{
		{
			name: "verification", failure: &cloudcode.HTTPError{StatusCode: http.StatusForbidden, Status: "403", Body: `{"error":{"status":"PERMISSION_DENIED","message":"VALIDATION_REQUIRED","details":[{"metadata":{"validation_url":"https://accounts.google.com/signin/continue?x=1"}}]}}`},
			wantReason: "Account requires verification", wantURL: "https://accounts.google.com/signin/continue?x=1",
		},
		{
			name: "tos ban", failure: &cloudcode.HTTPError{StatusCode: http.StatusForbidden, Status: "403", Body: `The account has been disabled for violation of Terms of Service`},
			wantReason: "Account banned — Gemini disabled for Terms of Service violation",
		},
		{
			name: "revoked token", failure: &cloudcode.HTTPError{StatusCode: http.StatusUnauthorized, Status: "401", Body: `{"error":"invalid_grant: token revoked"}`},
			wantReason: "Token revoked - re-authentication required", wantToken: true,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
			first := testAccount("bad@example.com")
			second := testAccount("good@example.com")
			manager, err := New(Options{Accounts: []*Account{first, second}, Strategy: StrategySticky, Now: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			resolver := &staticResolver{tokens: map[string]string{first.Email: "bad", second.Email: "good"}}
			clients := map[string]*scriptedClient{
				"bad":  {results: []scriptedResult{{err: test.failure}}},
				"good": {results: []scriptedResult{{events: [][]byte{[]byte(`{}`)}}}},
			}
			dispatcher := newTestDispatcher(t, manager, resolver, clients, now, func(context.Context, time.Duration) error { return nil })
			if _, err := dispatcher.StreamGenerateContent(context.Background(), testRequest(), func(cloudcode.SSEEvent) error { return nil }); err != nil {
				t.Fatal(err)
			}
			snapshot := manager.Snapshot()[0]
			if !snapshot.Invalid || snapshot.InvalidReason != test.wantReason || snapshot.VerifyURL != test.wantURL {
				t.Fatalf("snapshot=%#v", snapshot)
			}
			resolver.mu.Lock()
			invalidated := append([]string(nil), resolver.invalidated...)
			resolver.mu.Unlock()
			if test.wantToken != reflect.DeepEqual(invalidated, []string{first.Email}) {
				t.Fatalf("invalidated tokens=%v", invalidated)
			}
		})
	}
}

func newTestDispatcher(t *testing.T, manager *Manager, resolver Resolver, clients map[string]*scriptedClient, now time.Time, sleep SleepFunc) *Dispatcher {
	t.Helper()
	dispatcher, err := NewDispatcher(DispatcherOptions{
		Manager: manager, Resolver: resolver, MaxRetries: 5, Sleep: sleep, Now: func() time.Time { return now },
		Random: func() float64 { return 0 },
		NewClient: func(token string) CloudClient {
			client := clients[token]
			if client == nil {
				t.Fatalf("unexpected token %q", token)
			}
			return client
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return dispatcher
}

func testRequest() map[string]any {
	return map[string]any{
		"model": "claude-sonnet-4-6", "max_tokens": float64(128),
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}
}

func TestDispatcherRequestThrottlingAndConfigUpdate(t *testing.T) {
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	account := testAccount("throttled@example.com")
	manager, err := New(Options{Accounts: []*Account{account}, Strategy: StrategySticky, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}

	var sleepDurations []time.Duration
	var mu sync.Mutex
	sleep := func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		sleepDurations = append(sleepDurations, d)
		mu.Unlock()
		return nil
	}

	client := &scriptedClient{
		results: []scriptedResult{
			{events: [][]byte{[]byte(`{}`)}},
		},
	}

	resolver := &staticResolver{tokens: map[string]string{"throttled@example.com": "tok-throttled"}}
	dispatcher := newTestDispatcher(t, manager, resolver, map[string]*scriptedClient{"tok-throttled": client}, now, sleep)

	// Update config to enable throttling with 250ms delay
	dispatcher.UpdateConfig(config.Config{
		RequestThrottlingEnabled: true,
		RequestDelayMs:           250,
	})

	_, err = dispatcher.StreamGenerateContent(context.Background(), testRequest(), func(e cloudcode.SSEEvent) error { return nil })
	if err != nil {
		t.Fatalf("StreamGenerateContent failed: %v", err)
	}

	mu.Lock()
	throttled := false
	for _, d := range sleepDurations {
		if d == 250*time.Millisecond {
			throttled = true
			break
		}
	}
	mu.Unlock()

	if !throttled {
		t.Errorf("expected 250ms throttling delay in sleep durations, got: %v", sleepDurations)
	}
}

// The catalog fetch is a rare background refresh on a 30s budget, not
// hot-loop generation: request throttling must not gate it. A requestDelayMs
// above fetchModelsTimeout (e.g. 60s) otherwise guarantees "context deadline
// exceeded" on every refresh, and through it on every Cloud Code request past
// the catalog TTL.
func TestFetchAvailableModelsIgnoresRequestThrottle(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	account := testAccount("catalog-throttle@example.com")
	manager, err := New(Options{Accounts: []*Account{account}, Strategy: StrategySticky, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var sleepDurations []time.Duration
	sleep := func(_ context.Context, d time.Duration) error {
		mu.Lock()
		defer mu.Unlock()
		sleepDurations = append(sleepDurations, d)
		return nil
	}

	client := &scriptedClient{}
	resolver := &staticResolver{tokens: map[string]string{account.Email: "tok-catalog"}}
	dispatcher := newTestDispatcher(t, manager, resolver, map[string]*scriptedClient{"tok-catalog": client}, now, sleep)
	dispatcher.UpdateConfig(config.Config{
		RequestThrottlingEnabled: true,
		RequestDelayMs:           60000,
	})

	if _, err := dispatcher.FetchAvailableModels(context.Background()); err != nil {
		t.Fatalf("FetchAvailableModels failed: %v", err)
	}
	if dispatcher.CachedCatalog() == nil {
		t.Fatal("expected the fetch to populate the cached catalog")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(sleepDurations) != 0 {
		t.Fatalf("catalog fetch slept %d time(s) despite throttling exemption — a requestDelayMs above fetchModelsTimeout would deadlock every refresh", len(sleepDurations))
	}
}

// A flat cooldown made the proxy re-probe a throttled model ~38 times across
// the 2026-09-17 wave. Each tier must strictly exceed the previous one.
func TestSmartBackoffRateLimitEscalates(t *testing.T) {
	previous := time.Duration(0)
	for failures := range len(rateLimitTiers) {
		wait := SmartBackoff(ReasonRateLimit, 0, failures)
		if wait <= previous {
			t.Fatalf("SmartBackoff(failures=%d) = %s, want more than the previous tier %s", failures, wait, previous)
		}
		previous = wait
	}
	capped := SmartBackoff(ReasonRateLimit, 0, len(rateLimitTiers)+5)
	if capped != rateLimitTiers[len(rateLimitTiers)-1] {
		t.Fatalf("SmartBackoff past the last tier = %s, want the cap %s", capped, rateLimitTiers[len(rateLimitTiers)-1])
	}
}

// The dispatcher fast-retries the SAME account when the computed wait is
// <= 10s. A first tier at or below that boundary would turn a rate limit into
// a tight retry loop on the account that was just rejected.
func TestSmartBackoffRateLimitFirstTierExceedsFastRetryWindow(t *testing.T) {
	if got := SmartBackoff(ReasonRateLimit, 0, 0); got <= 10*time.Second {
		t.Fatalf("first rate-limit tier = %s, want more than 10s", got)
	}
}

// Jitter must only add: shortening a server-specified reset would retry
// before upstream said we may.
func TestDecorrelateOnlyAdds(t *testing.T) {
	base := 30 * time.Second
	if got := Decorrelate(base, func() float64 { return 0 }); got != base {
		t.Fatalf("Decorrelate at random=0 = %s, want %s", got, base)
	}
	got := Decorrelate(base, func() float64 { return 1 })
	if got <= base {
		t.Fatalf("Decorrelate at random=1 = %s, want more than %s", got, base)
	}
	if got > base+base/4 {
		t.Fatalf("Decorrelate at random=1 = %s, want at most %s", got, base+base/4)
	}
}

func TestDecorrelateHandlesNilSourceAndZero(t *testing.T) {
	if got := Decorrelate(30*time.Second, nil); got != 30*time.Second {
		t.Fatalf("Decorrelate with a nil source = %s, want the input unchanged", got)
	}
	if got := Decorrelate(0, func() float64 { return 1 }); got != 0 {
		t.Fatalf("Decorrelate(0) = %s, want 0", got)
	}
}
