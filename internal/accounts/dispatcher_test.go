package accounts

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"antigravity-go-proxy/internal/auth"
	"antigravity-go-proxy/internal/cloudcode"
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
