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
	"sync"
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

func (c *blockingModelsClient) LoadCodeAssist(ctx context.Context) (cloudcode.Response, error) {
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
	t.Cleanup(dispatcher.Close)
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

	t.Run("synthesized Argon is recorded in account quota with nil fraction", func(t *testing.T) {
		manager, err := New(Options{Accounts: []*Account{{Email: "argon@example.com", Enabled: true}}})
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

		body := []byte(`{
			"defaultAgentModelId":"gemini-3.8-flash-high",
			"agentModelSorts":[{"groups":[{"modelIds":["gemini-3.8-flash-high","gemini-3.8-flash-medium","gemini-3.8-flash-low"]}]}],
			"models":{
				"gemini-3.8-flash-high":{"displayName":"Gemini 3.8 Flash (High)","supportsThinking":true,"thinkingBudget":16000,"maxTokens":1048576,"maxOutputTokens":65536,"quotaInfo":{"remainingFraction":0.8,"resetTime":"2026-10-16T12:00:00Z"}},
				"gemini-3.8-flash-medium":{"displayName":"Gemini 3.8 Flash (Medium)","supportsThinking":true,"thinkingBudget":8000,"maxTokens":1048576,"maxOutputTokens":65536,"quotaInfo":{"remainingFraction":0.8,"resetTime":"2026-10-16T12:00:00Z"}},
				"gemini-3.8-flash-low":{"displayName":"Gemini 3.8 Flash (Low)","supportsThinking":true,"thinkingBudget":1024,"maxTokens":1048576,"maxOutputTokens":65536,"quotaInfo":{"remainingFraction":0.8,"resetTime":"2026-10-16T12:00:00Z"}}
			}
		}`)

		acc := manager.GetAllAccounts()[0]
		dispatcher.updateAccountQuota(acc, body)

		updated := manager.GetAllAccounts()[0]
		for _, tier := range []string{"gemini-4-argon-medium", "gemini-4-argon-high", "gemini-4-argon-low"} {
			q, ok := updated.Quota.Models[tier]
			if !ok {
				t.Fatalf("expected quota entry for %q, got map: %v", tier, updated.Quota.Models)
			}
			if q.RemainingFraction != nil {
				t.Errorf("expected nil fraction for %q, got %v", tier, *q.RemainingFraction)
			}
			if q.ResetTime != "" {
				t.Errorf("expected empty reset time for %q, got %q", tier, q.ResetTime)
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

func (c *alwaysRateLimitedModelsClient) LoadCodeAssist(ctx context.Context) (cloudcode.Response, error) {
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

type notFoundThenSuccessClient struct {
	blockingModelsClient
	email string
}

func (c *notFoundThenSuccessClient) StreamGenerateContent(ctx context.Context, request any, options cloudcode.RequestOptions, consume func(cloudcode.SSEEvent) error) (cloudcode.Response, error) {
	if c.email == "acc1@example.com" {
		return cloudcode.Response{}, &cloudcode.HTTPError{
			StatusCode: 404,
			Body:       `{"error":{"code":404,"message":"Requested entity was not found.","status":"NOT_FOUND"}}`,
		}
	}
	return cloudcode.Response{}, nil
}

type accountTokenResolver struct{}

func (accountTokenResolver) Resolve(ctx context.Context, account *Account) (auth.Credentials, error) {
	return auth.Credentials{AccessToken: account.Email, Email: account.Email}, nil
}

func (accountTokenResolver) Invalidate(string) {}

func TestStreamGenerateContent_404RotatesToCapableAccount(t *testing.T) {
	t.Parallel()
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
		Resolver: accountTokenResolver{},
		NewClient: func(email string) CloudClient {
			return &notFoundThenSuccessClient{
				blockingModelsClient: blockingModelsClient{release: release},
				email:                email,
			}
		},
		ProjectID: "test-proj-404",
	})
	if err != nil {
		t.Fatalf("new dispatcher: %v", err)
	}
	dispatcher.cacheCatalog("acc1@example.com", []byte(`{
		"models": {
			"claude-sonnet-5-5-high": {"displayName": "Claude Sonnet 5.5 (High)"}
		},
		"agentModelSorts": [{"groups": [{"modelIds": ["claude-sonnet-5-5-high"]}]}]
	}`))

	ctx, meta := cloudcode.WithExecutionMetadata(context.Background())
	req := map[string]any{
		"model": "claude-sonnet-5-5-high",
	}

	_, err = dispatcher.StreamGenerateContent(ctx, req, func(cloudcode.SSEEvent) error {
		return nil
	})
	if err != nil {
		t.Fatalf("StreamGenerateContent failed on 404 failover: %v", err)
	}

	if meta.Account != "acc2@example.com" {
		t.Errorf("meta.Account = %q, want %q", meta.Account, "acc2@example.com")
	}
}

// The shared catalog is the union of each account's latest list: a model one
// account publishes survives another account's refresh, and leaves when its
// own publisher's next list drops it.
func TestStoreCatalog_UnionsPerAccountSnapshots(t *testing.T) {
	t.Parallel()
	manager, err := New(Options{Accounts: []*Account{{Email: "acc1@example.com", Enabled: true}}})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	dispatcher, err := NewDispatcher(DispatcherOptions{
		Manager:   manager,
		Resolver:  stubResolver{},
		NewClient: func(string) CloudClient { return &blockingModelsClient{} },
	})
	if err != nil {
		t.Fatalf("new dispatcher: %v", err)
	}
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	dispatcher.now = func() time.Time { return now }

	with55 := []byte(`{"models":{"claude-sonnet-5-5-high":{"displayName":"Claude Sonnet 5.5 (High)"}},
		"agentModelSorts":[{"groups":[{"modelIds":["claude-sonnet-5-5-high"]}]}]}`)
	only46 := []byte(`{"models":{"claude-sonnet-4-6":{"displayName":"Claude Sonnet 4.6"}},
		"agentModelSorts":[{"groups":[{"modelIds":["claude-sonnet-4-6"]}]}]}`)
	resolves := func(id string) bool {
		_, err := dispatcher.CachedCatalog().Resolve(id)
		return err == nil
	}

	dispatcher.cacheCatalog("acc1@example.com", with55)
	dispatcher.cacheCatalog("acc2@example.com", only46)
	if !resolves("claude-sonnet-5-5-high") || !resolves("claude-sonnet-4-6") {
		t.Fatal("union lost a model published by one account")
	}

	// acc1 withdraws 5.5: its next list replaces its snapshot.
	dispatcher.cacheCatalog("acc1@example.com", only46)
	if resolves("claude-sonnet-5-5-high") {
		t.Error("withdrawn model still resolves after its publisher refetched")
	}
	if !resolves("claude-sonnet-4-6") {
		t.Error("claude-sonnet-4-6 lost")
	}

	// A snapshot older than modelCacheTTL no longer contributes.
	dispatcher.cacheCatalog("acc3@example.com", with55)
	now = now.Add(dispatcher.modelCacheTTL + time.Second)
	dispatcher.cacheCatalog("acc2@example.com", only46)
	if resolves("claude-sonnet-5-5-high") {
		t.Error("snapshot older than modelCacheTTL still contributes")
	}
}

// Regression guard for round-1 item 1: resolveModel must not read the catalog
// fields without the dispatcher lock. Run under -race.
func TestResolveModelDoesNotRaceStoreCatalog(t *testing.T) {
	t.Parallel()
	dispatcher, _, _, _, _ := recoveryPool(t, catalogWithModel)
	catalog := dispatcher.CachedCatalog()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				dispatcher.storeCatalog("a@example.com", catalog)
			}
		}
	}()
	for range 200 {
		_, _ = dispatcher.resolveModel(context.Background(), "unknown-model-xyz", nil)
	}
	close(stop)
	wg.Wait()
}

type cancelingResolver struct{}

func (cancelingResolver) Resolve(ctx context.Context, account *Account) (auth.Credentials, error) {
	if ctx.Err() != nil {
		return auth.Credentials{}, ctx.Err()
	}
	return auth.Credentials{AccessToken: "token", Email: account.Email}, nil
}

func (cancelingResolver) Invalidate(string) {}

func TestFetchAvailableModelsForAccount_DoesNotRotateOnContextCancellation(t *testing.T) {
	t.Parallel()
	manager, err := New(Options{
		Accounts: []*Account{{Email: "acc@example.com", Enabled: true}},
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	dispatcher, err := NewDispatcher(DispatcherOptions{
		Manager:  manager,
		Resolver: cancelingResolver{},
		NewClient: func(string) CloudClient {
			return &blockingModelsClient{release: make(chan struct{})}
		},
	})
	if err != nil {
		t.Fatalf("new dispatcher: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	acc := manager.accounts[0]
	_, err = dispatcher.fetchAvailableModelsForAccount(ctx, acc)
	if err == nil {
		t.Fatal("expected context error")
	}

	if failures := manager.FailureCount(acc); failures != 0 {
		t.Errorf("expected 0 failures on cancelled context, got %d", failures)
	}
}

// Cancelling while the fetch is in flight (past the entry guard) must not
// penalize the account: the caller left, the account did not fail.
func TestFetchAvailableModelsForAccount_NoPenaltyWhenCancelledMidFetch(t *testing.T) {
	t.Parallel()
	manager, err := New(Options{
		Accounts: []*Account{{Email: "acc@example.com", Enabled: true}},
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	client := &scriptedClient{modelsHang: true}
	dispatcher, err := NewDispatcher(DispatcherOptions{
		Manager:   manager,
		Resolver:  stubResolver{},
		NewClient: func(string) CloudClient { return client },
	})
	if err != nil {
		t.Fatalf("new dispatcher: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for client.modelsCallCount() == 0 {
			client.mu.Lock()
			started := client.modelsBudget > 0
			client.mu.Unlock()
			if started {
				break
			}
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()

	acc := manager.accounts[0]
	if _, err := dispatcher.fetchAvailableModelsForAccount(ctx, acc); err == nil {
		t.Fatal("expected context error")
	}
	if failures := manager.FailureCount(acc); failures != 0 {
		t.Errorf("failures after mid-fetch cancel = %d, want 0", failures)
	}
}

// A conversation stays on one account under round-robin: the session key on
// the context reaches Select, and a second session takes the next account.
// Requests without a key keep rotating per request.
func TestStreamGenerateContentKeepsASessionOnItsAccount(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	close(release)
	manager, err := New(Options{
		Accounts: []*Account{
			{Email: "acc1@example.com", Enabled: true},
			{Email: "acc2@example.com", Enabled: true},
		},
		Strategy: StrategyRoundRobin,
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	dispatcher, err := NewDispatcher(DispatcherOptions{
		Manager:   manager,
		Resolver:  accountTokenResolver{},
		NewClient: func(string) CloudClient { return &blockingModelsClient{release: release} },
		ProjectID: "test-proj-affinity",
	})
	if err != nil {
		t.Fatalf("new dispatcher: %v", err)
	}
	t.Cleanup(dispatcher.Close)
	// A cached catalog keeps resolveModel from selecting an account for a
	// catalog fetch, so the round-robin cursor moves only for generation.
	dispatcher.cacheCatalog("acc1@example.com", []byte(testCatalogBody))

	served := func(session string) string {
		t.Helper()
		ctx, meta := cloudcode.WithExecutionMetadata(WithSessionKey(context.Background(), session))
		if _, err := dispatcher.StreamGenerateContent(ctx, map[string]any{"model": "gemini-2.5-pro"}, func(cloudcode.SSEEvent) error { return nil }); err != nil {
			t.Fatalf("StreamGenerateContent(session %q): %v", session, err)
		}
		return meta.Account
	}

	first := served("session-one")
	for turn := range 3 {
		if got := served("session-one"); got != first {
			t.Fatalf("turn %d of session-one served by %s, want %s", turn+2, got, first)
		}
	}
	if second := served("session-two"); second == first {
		t.Fatalf("session-two served by %s like session-one; round-robin must rotate per new session", second)
	}
	if a, b := served(""), served(""); a == b {
		t.Fatalf("two requests without a session were both served by %s; they must rotate per request", a)
	}
}

// A pinned session waits for its account only as long as the Dispatcher
// would. A limit longer than maxWait moves the session to another account
// instead of failing with a RateLimitError, and a maxWait lowered by
// UpdateConfig applies to pins at once.
func TestStreamGenerateContentMovesAPinnedSessionPastMaxWait(t *testing.T) {
	t.Parallel()
	errWaited := errors.New("dispatcher slept waiting for the pinned account")
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	setup := func(t *testing.T, maxWait time.Duration) (*Dispatcher, map[string]*Account) {
		t.Helper()
		pool := map[string]*Account{
			"acc1@example.com": {Email: "acc1@example.com", Enabled: true},
			"acc2@example.com": {Email: "acc2@example.com", Enabled: true},
		}
		manager, err := New(Options{
			Accounts: []*Account{pool["acc1@example.com"], pool["acc2@example.com"]},
			Strategy: StrategyRoundRobin,
			Now:      func() time.Time { return now },
		})
		if err != nil {
			t.Fatalf("new manager: %v", err)
		}
		release := make(chan struct{})
		close(release)
		dispatcher, err := NewDispatcher(DispatcherOptions{
			Manager:   manager,
			Resolver:  accountTokenResolver{},
			NewClient: func(string) CloudClient { return &blockingModelsClient{release: release} },
			ProjectID: "test-proj-affinity",
			MaxWait:   maxWait,
			Now:       func() time.Time { return now },
			// The clock never moves, so a wait would never end: fail instead.
			Sleep: func(context.Context, time.Duration) error { return errWaited },
		})
		if err != nil {
			t.Fatalf("new dispatcher: %v", err)
		}
		t.Cleanup(dispatcher.Close)
		dispatcher.cacheCatalog("acc1@example.com", []byte(testCatalogBody))
		return dispatcher, pool
	}
	serve := func(t *testing.T, dispatcher *Dispatcher, session string) string {
		t.Helper()
		ctx, meta := cloudcode.WithExecutionMetadata(WithSessionKey(context.Background(), session))
		if _, err := dispatcher.StreamGenerateContent(ctx, map[string]any{"model": "gemini-2.5-pro"}, func(cloudcode.SSEEvent) error { return nil }); err != nil {
			t.Fatalf("StreamGenerateContent(session %q): %v", session, err)
		}
		return meta.Account
	}

	t.Run("MaxWait at construction", func(t *testing.T) {
		t.Parallel()
		dispatcher, pool := setup(t, 30*time.Second)
		home := serve(t, dispatcher, "s1")
		dispatcher.manager.MarkRateLimited(pool[home], "gemini-2.5-pro", time.Minute)
		if got := serve(t, dispatcher, "s1"); got == "" || got == home {
			t.Fatalf("home %s limited for 60s with MaxWait 30s: served by %q, want the other account", home, got)
		}
	})

	t.Run("maxWait lowered by UpdateConfig", func(t *testing.T) {
		t.Parallel()
		dispatcher, pool := setup(t, 0) // NewDispatcher's 2-minute default
		dispatcher.UpdateConfig(config.Config{MaxWaitBeforeErrorMs: 20000})
		home := serve(t, dispatcher, "s1")
		dispatcher.manager.MarkRateLimited(pool[home], "gemini-2.5-pro", 25*time.Second)
		if got := serve(t, dispatcher, "s1"); got == "" || got == home {
			t.Fatalf("home %s limited for 25s after maxWait dropped to 20s: served by %q, want the other account", home, got)
		}
	})
}

// failingCredentialResolver answers like accountTokenResolver until fail
// names an account; from then on that account's credentials fail with err,
// and each refusal is counted.
type failingCredentialResolver struct {
	err error

	mu       sync.Mutex
	failing  string
	refusals int
}

func (resolver *failingCredentialResolver) Resolve(_ context.Context, account *Account) (auth.Credentials, error) {
	resolver.mu.Lock()
	defer resolver.mu.Unlock()
	if account.Email == resolver.failing {
		resolver.refusals++
		return auth.Credentials{}, resolver.err
	}
	return auth.Credentials{AccessToken: account.Email, Email: account.Email}, nil
}

func (*failingCredentialResolver) Invalidate(string) {}

func (resolver *failingCredentialResolver) fail(email string) {
	resolver.mu.Lock()
	defer resolver.mu.Unlock()
	resolver.failing = email
}

func (resolver *failingCredentialResolver) refused() int {
	resolver.mu.Lock()
	defer resolver.mu.Unlock()
	return resolver.refusals
}

// A pinned session leaves an account whose credentials keep failing. A
// transient credential error counts toward the model's failure lock like any
// other failure, so the third one moves the session to another account
// instead of the request spending every attempt on the pinned one.
func TestStreamGenerateContentPinnedSessionLeavesAnAccountWhoseCredentialsFail(t *testing.T) {
	t.Parallel()
	// Transient: neither IsPermanentAuthFailure nor invalid_grant, so the
	// account stays valid and the pin does not simply rebind.
	resolver := &failingCredentialResolver{err: errors.New("token refresh: 503")}
	pool := map[string]*Account{
		"acc1@example.com": {Email: "acc1@example.com", Enabled: true},
		"acc2@example.com": {Email: "acc2@example.com", Enabled: true},
	}
	manager, err := New(Options{
		Accounts: []*Account{pool["acc1@example.com"], pool["acc2@example.com"]},
		Strategy: StrategyRoundRobin,
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	release := make(chan struct{})
	close(release)
	// NewDispatcher's default MaxRetries is 5, so a request gets
	// max(5, Count()+1) = 5 attempts: room for the three failures that set the
	// failure lock and the attempt on the overflow account.
	dispatcher, err := NewDispatcher(DispatcherOptions{
		Manager:   manager,
		Resolver:  resolver,
		NewClient: func(string) CloudClient { return &blockingModelsClient{release: release} },
		ProjectID: "test-proj-affinity",
	})
	if err != nil {
		t.Fatalf("new dispatcher: %v", err)
	}
	t.Cleanup(dispatcher.Close)
	dispatcher.cacheCatalog("acc1@example.com", []byte(testCatalogBody))
	serve := func() (string, error) {
		ctx, meta := cloudcode.WithExecutionMetadata(WithSessionKey(context.Background(), "s1"))
		_, err := dispatcher.StreamGenerateContent(ctx, map[string]any{"model": "gemini-2.5-pro"}, func(cloudcode.SSEEvent) error { return nil })
		return meta.Account, err
	}

	home, err := serve()
	if err != nil {
		t.Fatalf("first request of s1: %v", err)
	}
	resolver.fail(home)
	got, err := serve()
	if err != nil || got == "" || got == home {
		t.Fatalf("home %s whose credentials fail: served by %q (error %v), want the other account", home, got, err)
	}
	if refused := resolver.refused(); refused > 3 {
		t.Fatalf("home %s was asked for credentials %d times, want at most 3", home, refused)
	}
	if pool[home].IsInvalid {
		t.Fatalf("home %s marked invalid by a transient credential error", home)
	}
}

// retryAfterClient answers a model call with a 429 carrying Retry-After: 30
// while it has rejections left, a negative count never running out, and with
// an empty success otherwise. It counts model calls.
type retryAfterClient struct {
	blockingModelsClient

	mu          sync.Mutex
	rejections  int
	generations int
}

func (c *retryAfterClient) StreamGenerateContent(context.Context, any, cloudcode.RequestOptions, func(cloudcode.SSEEvent) error) (cloudcode.Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.generations++
	if c.rejections == 0 {
		return cloudcode.Response{}, nil
	}
	if c.rejections > 0 {
		c.rejections--
	}
	return cloudcode.Response{}, &cloudcode.HTTPError{
		StatusCode: http.StatusTooManyRequests,
		Status:     "429 Too Many Requests",
		Header:     http.Header{"Retry-After": []string{"30"}},
		Body:       `{"error":{"code":429,"message":"Resource has been exhausted (e.g. check quota).","status":"RESOURCE_EXHAUSTED"}}`,
	}
}

// reject sets the 429s still to come and restarts the call count.
func (c *retryAfterClient) reject(rejections int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rejections, c.generations = rejections, 0
}

func (c *retryAfterClient) calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.generations
}

// pinnedRetryAfterDispatcher builds a round-robin pool of two accounts, each
// served by its retryAfterClient, on a clock that moves only when the
// dispatcher sleeps, and pins session s1 with a first request. It returns the
// clients by email and the email of s1's home account.
func pinnedRetryAfterDispatcher(t *testing.T) (*Dispatcher, map[string]*retryAfterClient, string) {
	t.Helper()
	release := make(chan struct{})
	close(release)
	clients := map[string]*retryAfterClient{
		"acc1@example.com": {blockingModelsClient: blockingModelsClient{release: release}},
		"acc2@example.com": {blockingModelsClient: blockingModelsClient{release: release}},
	}
	var clock sync.Mutex
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	readClock := func() time.Time {
		clock.Lock()
		defer clock.Unlock()
		return now
	}
	manager, err := New(Options{
		Accounts: []*Account{
			{Email: "acc1@example.com", Enabled: true},
			{Email: "acc2@example.com", Enabled: true},
		},
		Strategy: StrategyRoundRobin,
		Now:      readClock,
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	dispatcher, err := NewDispatcher(DispatcherOptions{
		Manager:   manager,
		Resolver:  accountTokenResolver{},
		NewClient: func(email string) CloudClient { return clients[email] },
		ProjectID: "test-proj-affinity",
		Now:       readClock,
		Sleep: func(_ context.Context, wait time.Duration) error {
			clock.Lock()
			now = now.Add(wait)
			clock.Unlock()
			return nil
		},
		Random: func() float64 { return 0 },
	})
	if err != nil {
		t.Fatalf("new dispatcher: %v", err)
	}
	t.Cleanup(dispatcher.Close)
	dispatcher.cacheCatalog("acc1@example.com", []byte(testCatalogBody))
	home, err := serveSession(dispatcher, "s1")
	if err != nil {
		t.Fatalf("first request of s1: %v", err)
	}
	return dispatcher, clients, home
}

// serveSession sends one request of session and returns the account that
// served it.
func serveSession(dispatcher *Dispatcher, session string) (string, error) {
	ctx, meta := cloudcode.WithExecutionMetadata(WithSessionKey(context.Background(), session))
	_, err := dispatcher.StreamGenerateContent(ctx, map[string]any{"model": "gemini-2.5-pro"}, func(cloudcode.SSEEvent) error { return nil })
	return meta.Account, err
}

// A pinned request waits out one 429 on its account. When the account
// answers 429 again, with no success between, the session moves to the
// other account, which serves the request.
func TestStreamGenerateContentPinnedSessionMovesAfterARepeatedLimit(t *testing.T) {
	t.Parallel()
	dispatcher, clients, home := pinnedRetryAfterDispatcher(t)
	clients[home].reject(-1)

	got, err := serveSession(dispatcher, "s1")
	if err != nil || got == "" || got == home {
		t.Fatalf("home %s answering 429 Retry-After: 30 on every call: served by %q (error %v), want the other account", home, got, err)
	}
	if calls := clients[home].calls(); calls > 2 {
		t.Fatalf("home %s got %d model calls, want at most 2: one wait, then the move", home, calls)
	}
}

// A single 429 on the pinned account is waited out: the request is served
// there after the wait, and the other account gets nothing.
func TestStreamGenerateContentPinnedSessionWaitsOutOneLimit(t *testing.T) {
	t.Parallel()
	dispatcher, clients, home := pinnedRetryAfterDispatcher(t)
	clients[home].reject(1)

	got, err := serveSession(dispatcher, "s1")
	if err != nil || got != home {
		t.Fatalf("home %s answering one 429: served by %q (error %v), want the home after one wait", home, got, err)
	}
	if calls := clients[home].calls(); calls != 2 {
		t.Fatalf("home %s got %d model calls, want 2: the 429 and the call after the wait", home, calls)
	}
	for email, client := range clients {
		if email != home && client.calls() != 0 {
			t.Fatalf("%s got %d model calls; the session must not move for one limit", email, client.calls())
		}
	}
}
