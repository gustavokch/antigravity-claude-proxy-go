package accounts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"antigravity-go-proxy/internal/auth"
	"antigravity-go-proxy/internal/cloudcode"
	"antigravity-go-proxy/internal/config"
)

const testCatalogBody = `{"models":{"gemini-2.5-pro":{"displayName":"Gemini 2.5 Pro"}},"agentModelSorts":[{"groups":[{"modelIds":["gemini-2.5-pro"]}]}]}`

type stubResolver struct{}

func (stubResolver) Resolve(ctx context.Context, account *Account) (auth.Credentials, error) {
	return auth.Credentials{AccessToken: "token", Email: account.Email}, nil
}

func (stubResolver) Invalidate(string) {}

// blockingModelsClient blocks every FetchAvailableModels call on its release
// channel so tests control when the upstream fetch completes.
type blockingModelsClient struct {
	calls   atomic.Int32
	fail    atomic.Bool
	release chan struct{}
}

func (c *blockingModelsClient) LoadCodeAssist(ctx context.Context, project string) (cloudcode.Response, error) {
	return cloudcode.Response{Body: []byte(`{"cloudaicompanionProject":{"id":"project"}}`)}, nil
}

func (c *blockingModelsClient) FetchAvailableModels(ctx context.Context, project string) (cloudcode.Response, error) {
	c.calls.Add(1)
	select {
	case <-c.release:
		if c.fail.Load() {
			return cloudcode.Response{}, errors.New("upstream unavailable")
		}
		return cloudcode.Response{Body: []byte(testCatalogBody)}, nil
	case <-ctx.Done():
		return cloudcode.Response{}, ctx.Err()
	}
}

func (c *blockingModelsClient) StreamGenerateContent(ctx context.Context, request any, options cloudcode.RequestOptions, consume func(cloudcode.SSEEvent) error) (cloudcode.Response, error) {
	return cloudcode.Response{}, nil
}

func newDispatcherWithClient(t *testing.T, client CloudClient) *Dispatcher {
	t.Helper()
	manager, err := New(Options{Accounts: []*Account{{Email: "test@example.com", Enabled: true}}})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	dispatcher, err := NewDispatcher(DispatcherOptions{
		Manager:   manager,
		Resolver:  stubResolver{},
		NewClient: func(string) CloudClient { return client },
	})
	if err != nil {
		t.Fatalf("new dispatcher: %v", err)
	}
	return dispatcher
}

func TestFetchAvailableModelsSurvivesCallerCancel(t *testing.T) {
	release := make(chan struct{})
	client := &blockingModelsClient{release: release}
	dispatcher := newDispatcherWithClient(t, client)

	callerCtx, cancelCaller := context.WithCancel(context.Background())
	defer cancelCaller()

	type outcome struct {
		response cloudcode.Response
		err      error
	}
	resultCh := make(chan outcome, 1)
	go func() {
		response, err := dispatcher.FetchAvailableModels(callerCtx)
		resultCh <- outcome{response: response, err: err}
	}()

	// Cancel the caller while the upstream fetch is still blocked.
	cancelCaller()
	select {
	case result := <-resultCh:
		if !errors.Is(result.err, context.Canceled) {
			t.Fatalf("FetchAvailableModels error=%v; want context.Canceled", result.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("FetchAvailableModels did not return after caller cancel")
	}

	// The decoupled fetch must keep running: release it and wait for the
	// shared catalog cache to be populated even though the caller left.
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		dispatcher.mu.RLock()
		cached := dispatcher.catalog
		dispatcher.mu.RUnlock()
		if cached != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("background fetch never cached the catalog after caller cancel")
}

func TestConcurrentModelFetchesShareOneUpstreamCall(t *testing.T) {
	release := make(chan struct{})
	client := &blockingModelsClient{release: release}
	dispatcher := newDispatcherWithClient(t, client)

	// Leader: starts the shared fetch; the upstream call blocks on release.
	first := dispatcher.startModelFetch()

	// Joiner: arrives while the fetch is in flight and must attach to the
	// same call instead of starting a second upstream request.
	second := dispatcher.startModelFetch()
	if first != second {
		t.Fatal("second caller started a new upstream fetch instead of sharing the in-flight call")
	}

	// Let the shared fetch finish and check the result.
	close(release)
	select {
	case <-first.done:
	case <-time.After(5 * time.Second):
		t.Fatal("shared catalog fetch did not complete")
	}
	if first.err != nil {
		t.Fatalf("shared fetch error: %v", first.err)
	}
	if got := client.calls.Load(); got != 1 {
		t.Fatalf("upstream FetchAvailableModels calls=%d; want 1", got)
	}

	// Once complete, the slot frees so the next caller starts a fresh fetch.
	dispatcher.mu.RLock()
	pending := dispatcher.modelsFetch
	dispatcher.mu.RUnlock()
	if pending != nil {
		t.Fatal("modelsFetch still set after the shared fetch completed")
	}
}

func TestSharedModelFetchErrorFreesSlotForNextCaller(t *testing.T) {
	release := make(chan struct{})
	client := &blockingModelsClient{release: release}
	dispatcher := newDispatcherWithClient(t, client)

	// Start the shared fetch, then make the upstream fail on release.
	first := dispatcher.startModelFetch()
	client.fail.Store(true)
	close(release)
	select {
	case <-first.done:
	case <-time.After(5 * time.Second):
		t.Fatal("shared catalog fetch did not complete")
	}
	if first.err == nil {
		t.Fatal("shared fetch succeeded; want upstream error")
	}
	callsAfterDeadFetch := client.calls.Load()

	// The failed call must free the slot; otherwise every later caller
	// would attach to the dead call and receive its error forever. The
	// goroutine clears the slot after closing done, so poll briefly.
	deadline := time.Now().Add(5 * time.Second)
	for {
		dispatcher.mu.RLock()
		pending := dispatcher.modelsFetch
		dispatcher.mu.RUnlock()
		if pending == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("modelsFetch still set after the shared fetch failed")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The next caller starts a fresh upstream fetch, not the dead call.
	second := dispatcher.startModelFetch()
	if second == first {
		t.Fatal("startModelFetch returned the failed call instead of a fresh fetch")
	}
	select {
	case <-second.done:
	case <-time.After(5 * time.Second):
		t.Fatal("fresh fetch did not complete")
	}
	if got := client.calls.Load(); got <= callsAfterDeadFetch {
		t.Fatalf("upstream FetchAvailableModels calls=%d; want more than %d from the fresh fetch", got, callsAfterDeadFetch)
	}
}

func TestUpdateAccountQuotaPopulatesGemini38FlashFamily(t *testing.T) {
	t.Parallel()

	t.Run("tiered 3.8 expands to selectable 3.8 tiers with quota", func(t *testing.T) {
		manager, err := New(Options{Accounts: []*Account{{Email: "user@example.com", Enabled: true}}})
		if err != nil {
			t.Fatal(err)
		}
		dispatcher, err := NewDispatcher(DispatcherOptions{
			Manager:   manager,
			Resolver:  stubResolver{},
			NewClient: func(string) CloudClient { return nil },
		})
		if err != nil {
			t.Fatal(err)
		}

		tieredBody := []byte(`{
			"defaultAgentModelId":"gemini-3.8-flash-high",
			"agentModelSorts":[{"displayName":"Recommended","groups":[{"modelIds":["gemini-3.8-flash-high"]}]}],
			"models":{
				"gemini-3.8-flash-tiered":{"supportsThinking":true,"quotaInfo":{"remainingFraction":0.85,"resetTime":"2026-09-05T12:00:00Z"}}
			}
		}`)

		acc := manager.GetAllAccounts()[0]
		dispatcher.updateAccountQuota(acc, tieredBody)

		updated := manager.GetAllAccounts()[0]
		for _, tier := range []string{"gemini-3.8-flash-high", "gemini-3.8-flash-medium", "gemini-3.8-flash-low"} {
			q, ok := updated.Quota.Models[tier]
			if !ok {
				t.Fatalf("expected quota for %q, got map: %v", tier, updated.Quota.Models)
			}
			if q.RemainingFraction == nil || *q.RemainingFraction != 0.85 {
				t.Fatalf("expected fraction 0.85 for %q, got %v", tier, q.RemainingFraction)
			}
			if q.ResetTime != "2026-09-05T12:00:00Z" {
				t.Fatalf("expected reset time 2026-09-05T12:00:00Z for %q, got %q", tier, q.ResetTime)
			}
		}
	})

	t.Run("fallback from 3.7 populates 3.8 family quota", func(t *testing.T) {
		manager, err := New(Options{Accounts: []*Account{{Email: "user2@example.com", Enabled: true}}})
		if err != nil {
			t.Fatal(err)
		}
		dispatcher, err := NewDispatcher(DispatcherOptions{
			Manager:   manager,
			Resolver:  stubResolver{},
			NewClient: func(string) CloudClient { return nil },
		})
		if err != nil {
			t.Fatal(err)
		}

		fallbackBody := []byte(`{
			"defaultAgentModelId":"gemini-3.7-flash-high",
			"agentModelSorts":[{"displayName":"Recommended","groups":[{"modelIds":["gemini-3.7-flash-high","gemini-3.7-flash-medium","gemini-3.7-flash-low"]}]}],
			"models":{
				"gemini-3.7-flash-high":{"displayName":"Gemini 3.7 Flash (High)","supportsThinking":true,"quotaInfo":{"remainingFraction":0.60,"resetTime":"2026-09-05T14:00:00Z"}},
				"gemini-3.7-flash-medium":{"displayName":"Gemini 3.7 Flash (Medium)","supportsThinking":true,"quotaInfo":{"remainingFraction":0.70,"resetTime":"2026-09-05T14:00:00Z"}},
				"gemini-3.7-flash-low":{"displayName":"Gemini 3.7 Flash (Low)","supportsThinking":true,"quotaInfo":{"remainingFraction":0.90,"resetTime":"2026-09-05T14:00:00Z"}}
			}
		}`)

		acc := manager.GetAllAccounts()[0]
		dispatcher.updateAccountQuota(acc, fallbackBody)

		updated := manager.GetAllAccounts()[0]
		tiers := map[string]float64{
			"gemini-3.8-flash-high":   0.60,
			"gemini-3.8-flash-medium": 0.70,
			"gemini-3.8-flash-low":    0.90,
		}
		for tier, expectedFrac := range tiers {
			q, ok := updated.Quota.Models[tier]
			if !ok {
				t.Fatalf("expected quota for fallback tier %q, got map: %v", tier, updated.Quota.Models)
			}
			if q.RemainingFraction == nil || *q.RemainingFraction != expectedFrac {
				t.Fatalf("expected fraction %f for %q, got %v", expectedFrac, tier, q.RemainingFraction)
			}
		}
	})
}

func TestUpdateAccountQuota_FallbackNilFractionStaysUnknown(t *testing.T) {
	manager, err := New(Options{Accounts: []*Account{{Email: "test@example.com", Enabled: true}}})
	if err != nil {
		t.Fatal(err)
	}
	dispatcher, err := NewDispatcher(DispatcherOptions{
		Manager:   manager,
		Resolver:  stubResolver{},
		NewClient: func(string) CloudClient { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	// agentModelSorts.modelIds has the wrong type, so modelcatalog.Parse
	// fails and updateAccountQuota takes the non-catalog fallback branch.
	fallbackBody := []byte(`{"agentModelSorts":[{"groups":[{"modelIds":"not-an-array"}]}],"models":{"gemini-3.7-flash-high":{"quotaInfo":{"resetTime":"2026-09-20T01:00:00Z"}}}}`)

	dispatcher.updateAccountQuota(manager.GetAllAccounts()[0], fallbackBody)

	got, ok := manager.GetAllAccounts()[0].Quota.Models["gemini-3.7-flash-high"]
	if !ok {
		t.Fatalf("reset-only entry must be recorded: %+v", manager.GetAllAccounts()[0].Quota.Models)
	}
	if got.RemainingFraction != nil {
		t.Fatalf("nil fraction must stay nil, got %f", *got.RemainingFraction)
	}
	if got.ResetTime != "2026-09-20T01:00:00Z" {
		t.Fatalf("reset time not preserved: %+v", got)
	}
}

func TestStreamGenerateContent_ExecutionMetadataUpdatedOnSuccess(t *testing.T) {
	release := make(chan struct{})
	close(release)
	client := &blockingModelsClient{release: release}
	manager, err := New(Options{
		Accounts: []*Account{
			{Email: "acc1@example.com", Enabled: true},
		},
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	dispatcher, err := NewDispatcher(DispatcherOptions{
		Manager:   manager,
		Resolver:  stubResolver{},
		NewClient: func(string) CloudClient { return client },
		ProjectID: "test-proj-123",
	})
	if err != nil {
		t.Fatalf("new dispatcher: %v", err)
	}

	ctx, meta := cloudcode.WithExecutionMetadata(context.Background())
	req := map[string]any{
		"model": "gemini-2.5-pro",
	}

	_, err = dispatcher.StreamGenerateContent(ctx, req, func(cloudcode.SSEEvent) error {
		return nil
	})
	if err != nil {
		t.Fatalf("StreamGenerateContent failed: %v", err)
	}

	if meta.Account != "acc1@example.com" {
		t.Errorf("meta.Account = %q, want %q", meta.Account, "acc1@example.com")
	}
	if meta.ProjectID != "test-proj-123" {
		t.Errorf("meta.ProjectID = %q, want %q", meta.ProjectID, "test-proj-123")
	}
}

type failoverClient struct {
	blockingModelsClient
	email string
}

func (c *failoverClient) StreamGenerateContent(ctx context.Context, request any, options cloudcode.RequestOptions, consume func(cloudcode.SSEEvent) error) (cloudcode.Response, error) {
	if c.email == "acc1@example.com" {
		return cloudcode.Response{}, &cloudcode.HTTPError{
			StatusCode: 429,
			Body:       `{"error":{"message":"quota exceeded"}}`,
		}
	}
	return cloudcode.Response{}, nil
}

func TestStreamGenerateContent_ExecutionMetadataUpdatedOnFailover(t *testing.T) {
	release := make(chan struct{})
	close(release)
	manager, err := New(Options{
		Accounts: []*Account{
			{Email: "acc1@example.com", Enabled: true},
			{Email: "acc2@example.com", Enabled: true},
		},
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	dispatcher, err := NewDispatcher(DispatcherOptions{
		Manager:  manager,
		Resolver: stubResolver{},
		NewClient: func(email string) CloudClient {
			return &failoverClient{
				blockingModelsClient: blockingModelsClient{release: release},
				email:                email,
			}
		},
		ProjectID: "test-proj-456",
	})
	if err != nil {
		t.Fatalf("new dispatcher: %v", err)
	}

	ctx, meta := cloudcode.WithExecutionMetadata(context.Background())
	req := map[string]any{
		"model": "gemini-2.5-pro",
	}

	_, err = dispatcher.StreamGenerateContent(ctx, req, func(cloudcode.SSEEvent) error {
		return nil
	})
	if err != nil {
		t.Fatalf("StreamGenerateContent failed: %v", err)
	}

	if meta.Account != "acc2@example.com" {
		t.Errorf("meta.Account = %q, want %q", meta.Account, "acc2@example.com")
	}
	if meta.ProjectID != "test-proj-456" {
		t.Errorf("meta.ProjectID = %q, want %q", meta.ProjectID, "test-proj-456")
	}
}

// alwaysRateLimitedModelsClient fails every FetchAvailableModels call with a
// 429 HTTPError and counts the calls so tests can prove the dispatcher
// rotated across accounts instead of retrying the same one.
type alwaysRateLimitedModelsClient struct {
	calls atomic.Int32
}

func (c *alwaysRateLimitedModelsClient) LoadCodeAssist(ctx context.Context, project string) (cloudcode.Response, error) {
	return cloudcode.Response{Body: []byte(`{"cloudaicompanionProject":{"id":"project"}}`)}, nil
}

func (c *alwaysRateLimitedModelsClient) FetchAvailableModels(ctx context.Context, project string) (cloudcode.Response, error) {
	c.calls.Add(1)
	return cloudcode.Response{}, &cloudcode.HTTPError{
		StatusCode: http.StatusTooManyRequests,
		Status:     "429 Too Many Requests",
		Body:       `{"error":{"code":429,"message":"quota exceeded","status":"RESOURCE_EXHAUSTED"}}`,
	}
}

func (c *alwaysRateLimitedModelsClient) StreamGenerateContent(ctx context.Context, request any, options cloudcode.RequestOptions, consume func(cloudcode.SSEEvent) error) (cloudcode.Response, error) {
	return cloudcode.Response{}, nil
}

func TestFetchAvailableModels_RateLimitedAccountRotates(t *testing.T) {
	client := &alwaysRateLimitedModelsClient{}
	manager, err := New(Options{Accounts: []*Account{
		{Email: "first@example.com", Enabled: true},
		{Email: "second@example.com", Enabled: true},
	}, Strategy: StrategySticky})
	if err != nil {
		t.Fatal(err)
	}
	dispatcher, err := NewDispatcher(DispatcherOptions{
		Manager:   manager,
		Resolver:  stubResolver{},
		NewClient: func(string) CloudClient { return client },
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = dispatcher.FetchAvailableModels(context.Background())
	if err == nil {
		t.Fatal("want error after both accounts 429")
	}
	// The first account's 429 must mark it so the retry goes to the second;
	// pre-fix, the write is dropped and the loop retries the same account
	// (or, with maxRetries < Count()+1, never reaches the second).
	if got := client.calls.Load(); got < 2 {
		t.Fatalf("upstream calls = %d, want >= 2 (rotation across accounts)", got)
	}
	if limit := manager.accounts[0].ModelRateLimits[""]; limit == nil {
		t.Fatal("the 429 on the listing path must write the empty-model namespace")
	}
}

// The 429 warn log must carry the raw upstream body (truncated): the body
// holds the quota dimension (user vs project) that the classified fields
// alone cannot show.
func TestStreamLogsUpstreamBodyOn429(t *testing.T) {
	var logBuffer bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuffer, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	client := &scriptedClient{
		modelsBody: []byte(testCatalogBody),
		results: []scriptedResult{{
			err: &cloudcode.HTTPError{StatusCode: 429, Status: "429", Body: `{"error":{"code":429,"message":"Quota exceeded for aicode-consumers per project per minute","status":"RESOURCE_EXHAUSTED"}}`},
		}},
	}
	manager, err := New(Options{Accounts: []*Account{{Email: "a@example.com", Enabled: true}}})
	if err != nil {
		t.Fatal(err)
	}
	dispatcher, err := NewDispatcher(DispatcherOptions{
		Manager:   manager,
		Resolver:  stubResolver{},
		NewClient: func(string) CloudClient { return client },
		MaxWait:   time.Millisecond,
		Sleep:     func(context.Context, time.Duration) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dispatcher.StreamGenerateContent(context.Background(), map[string]any{"model": "gemini-2.5-pro"}, func(cloudcode.SSEEvent) error { return nil }); err == nil {
		t.Fatal("want error after upstream 429")
	}
	if !strings.Contains(logBuffer.String(), "aicode-consumers per project per minute") {
		t.Fatalf("429 warn log must include the upstream body; got %q", logBuffer.String())
	}
}

// Bodies longer than the log cap must be truncated so a multi-KB error page
// cannot flood the log.
func TestTruncateBodyForLog(t *testing.T) {
	long := strings.Repeat("x", maxLoggedBodyLen+100)
	if got := truncateBodyForLog(long); len(got) > maxLoggedBodyLen {
		t.Fatalf("truncateBodyForLog len = %d, want <= %d", len(got), maxLoggedBodyLen)
	}
	if got := truncateBodyForLog("short"); got != "short" {
		t.Fatalf("truncateBodyForLog altered a short body: %q", got)
	}
}

// With forensics enabled, one upstream 429 must land verbatim in the JSONL
// record; the record must not contain any credential material.
func TestStreamRecords429Forensics(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upstream-429.jsonl")
	body := `{"error":{"code":429,"message":"Quota exceeded for aicode-consumers per project per minute","status":"RESOURCE_EXHAUSTED"}}`
	client := &scriptedClient{
		modelsBody: []byte(testCatalogBody),
		results: []scriptedResult{{
			err: &cloudcode.HTTPError{StatusCode: 429, Status: "429", Endpoint: "https://cloudcode-pa.googleapis.com/v1internal:streamGenerateContent?alt=sse", Body: body},
		}},
	}
	manager, err := New(Options{Accounts: []*Account{{Email: "a@example.com", Enabled: true}}})
	if err != nil {
		t.Fatal(err)
	}
	dispatcher, err := NewDispatcher(DispatcherOptions{
		Manager:      manager,
		Resolver:     stubResolver{},
		NewClient:    func(string) CloudClient { return client },
		MaxWait:      time.Millisecond,
		Sleep:        func(context.Context, time.Duration) error { return nil },
		Forensics429: NewForensics429Recorder(path),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dispatcher.StreamGenerateContent(context.Background(), map[string]any{"model": "gemini-2.5-pro"}, func(cloudcode.SSEEvent) error { return nil }); err == nil {
		t.Fatal("want error after upstream 429")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("forensics record missing: %v", err)
	}
	var entry Forensics429Entry
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(contents))), &entry); err != nil {
		t.Fatalf("forensics line is not valid JSON: %v", err)
	}
	if entry.Body != body {
		t.Fatalf("forensics body = %q, want verbatim upstream body", entry.Body)
	}
	if entry.Account != "a@example.com" || entry.Status != 429 || entry.Model != "gemini-2.5-pro" {
		t.Fatalf("forensics metadata mismatch: %+v", entry)
	}
	if strings.Contains(string(contents), `"token"`) {
		t.Fatal("forensics record must never contain credential material")
	}
}

// The WebUI config save reaches the dispatcher through UpdateConfig: the
// toggle must arm and disarm the recorder at runtime, without a restart.
func TestUpdateConfigToggles429Forensics(t *testing.T) {
	manager, err := New(Options{Accounts: []*Account{{Email: "a@example.com", Enabled: true}}})
	if err != nil {
		t.Fatal(err)
	}
	dispatcher, err := NewDispatcher(DispatcherOptions{
		Manager:   manager,
		Resolver:  stubResolver{},
		NewClient: func(string) CloudClient { return &scriptedClient{} },
	})
	if err != nil {
		t.Fatal(err)
	}
	dispatcher.UpdateConfig(config.Config{Upstream429ForensicsEnabled: true})
	dispatcher.mu.RLock()
	armed := dispatcher.forensics429 != nil && dispatcher.forensics429.Enabled()
	dispatcher.mu.RUnlock()
	if !armed {
		t.Fatal("enabling upstream429ForensicsEnabled must arm the recorder at runtime")
	}
	dispatcher.UpdateConfig(config.Config{Upstream429ForensicsEnabled: false})
	dispatcher.mu.RLock()
	disarmed := dispatcher.forensics429 == nil
	dispatcher.mu.RUnlock()
	if !disarmed {
		t.Fatal("disabling upstream429ForensicsEnabled must disarm the recorder at runtime")
	}
}

func TestModelFetchCountsAgainstTheAccountRate(t *testing.T) {
	release := make(chan struct{})
	close(release)
	dispatcher := newDispatcherWithClient(t, &blockingModelsClient{release: release})

	if _, err := dispatcher.FetchAvailableModels(context.Background()); err != nil {
		t.Fatalf("fetch available models: %v", err)
	}

	inFlight, priorMinute := dispatcher.meter.Observe("test@example.com")
	if priorMinute == 0 {
		t.Fatal("the model fetch was not counted against the account's minute window — the derived RPM ceiling would read high")
	}
	if inFlight != 0 {
		t.Fatalf("inFlight after the call returned: got %d, want 0", inFlight)
	}
}
