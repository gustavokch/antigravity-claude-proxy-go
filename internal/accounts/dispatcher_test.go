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
	release chan struct{}
}

func (c *blockingModelsClient) LoadCodeAssist(ctx context.Context, project string) (cloudcode.Response, error) {
	return cloudcode.Response{Body: []byte(`{"cloudaicompanionProject":{"id":"project"}}`)}, nil
}

func (c *blockingModelsClient) FetchAvailableModels(ctx context.Context, project string) (cloudcode.Response, error) {
	c.calls.Add(1)
	select {
	case <-c.release:
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
