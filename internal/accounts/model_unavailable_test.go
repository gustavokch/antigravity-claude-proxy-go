package accounts

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"antigravity-go-proxy/internal/cloudcode"
)

// upstream404 is the answer Cloud Code gives for a model an account does not
// serve (observed 2026-10-12 for claude-sonnet-5-5-high).
func upstream404() error {
	return &cloudcode.HTTPError{
		StatusCode: http.StatusNotFound,
		Status:     "404 Not Found",
		Body:       `{"error":{"code":404,"message":"Requested entity was not found.","status":"NOT_FOUND"}}`,
	}
}

func claude55Request() map[string]any {
	request := testRequest()
	request["model"] = recoveryModel
	return request
}

func generationCalls(client *scriptedClient) int {
	client.mu.Lock()
	defer client.mu.Unlock()
	return client.calls
}

// notFoundPool builds n sticky accounts whose model list publishes
// recoveryModel and whose generation always answers 404.
func notFoundPool(t *testing.T, n int) (*Dispatcher, *Manager, []*scriptedClient) {
	t.Helper()
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	accs := make([]*Account, 0, n)
	tokens := make(map[string]string, n)
	byToken := make(map[string]*scriptedClient, n)
	clients := make([]*scriptedClient, 0, n)
	for i := range n {
		email := fmt.Sprintf("acc%d@example.com", i)
		client := &scriptedClient{modelsBody: []byte(catalogWithModel), results: []scriptedResult{{err: upstream404()}}}
		accs = append(accs, testAccount(email))
		tokens[email] = "tok-" + email
		byToken["tok-"+email] = client
		clients = append(clients, client)
	}
	manager, err := New(Options{Accounts: accs, Strategy: StrategySticky, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	dispatcher := newTestDispatcher(t, manager, &staticResolver{tokens: tokens}, byToken, now, func(context.Context, time.Duration) error { return nil })
	return dispatcher, manager, clients
}

func isUpstream404(err error) bool {
	upstream := cloudcode.FindHTTPError(err)
	return upstream != nil && upstream.StatusCode == http.StatusNotFound
}

// With a single account a 404 has nowhere to rotate. The client must get the
// upstream error, not a 24-hour rate limit, and the next request must still
// reach the upstream.
func TestStreamGenerateContent_404WithoutAlternativeReturnsUpstreamError(t *testing.T) {
	t.Parallel()
	dispatcher, manager, clients := notFoundPool(t, 1)
	for attempt := 1; attempt <= 2; attempt++ {
		_, err := dispatcher.StreamGenerateContent(context.Background(), claude55Request(), func(cloudcode.SSEEvent) error { return nil })
		var rateLimit *RateLimitError
		if errors.As(err, &rateLimit) {
			t.Fatalf("attempt %d: got %v, want the upstream 404", attempt, err)
		}
		if !isUpstream404(err) {
			t.Fatalf("attempt %d: err = %v, want the upstream 404", attempt, err)
		}
	}
	if got := generationCalls(clients[0]); got != 2 {
		t.Errorf("upstream generation calls = %d, want 2 (the second request must not be blocked)", got)
	}
	if limits := manager.Snapshot()[0].Limits; len(limits) != 0 {
		t.Errorf("limits recorded after a 404 with no alternative: %v", limits)
	}
}

// With two accounts the first 404 may rotate, because the second account can
// still serve, so the first account is blocked. The second 404 has nowhere to
// go and is returned, and the last candidate stays unblocked.
func TestStreamGenerateContent_404OnEveryAccountBlocksOnlyTheFirst(t *testing.T) {
	t.Parallel()
	dispatcher, manager, clients := notFoundPool(t, 2)
	for attempt := 1; attempt <= 2; attempt++ {
		_, err := dispatcher.StreamGenerateContent(context.Background(), claude55Request(), func(cloudcode.SSEEvent) error { return nil })
		if !isUpstream404(err) {
			t.Fatalf("attempt %d: err = %v, want the upstream 404", attempt, err)
		}
	}
	snapshot := manager.Snapshot()
	if _, blocked := snapshot[0].Limits[recoveryModel]; !blocked {
		t.Errorf("first account not blocked after the rotating 404: %v", snapshot[0].Limits)
	}
	if len(snapshot[1].Limits) != 0 {
		t.Errorf("last candidate blocked: %v", snapshot[1].Limits)
	}
	if got := generationCalls(clients[0]); got != 1 {
		t.Errorf("first account generation calls = %d, want 1", got)
	}
	if got := generationCalls(clients[1]); got != 2 {
		t.Errorf("second account generation calls = %d, want 2", got)
	}
}

// A 404 on the model-listing call has no model to block and no account to
// prefer: it fails on the first call instead of repeating against the same
// account until the retry budget is spent.
func TestFetchAvailableModels_404FailsFast(t *testing.T) {
	t.Parallel()
	dispatcher, _, clients := notFoundPool(t, 1)
	clients[0].mu.Lock()
	clients[0].modelsErr = upstream404()
	clients[0].mu.Unlock()
	_, err := dispatcher.fetchAvailableModels(context.Background())
	if !isUpstream404(err) {
		t.Fatalf("err = %v, want the upstream 404", err)
	}
	if got := clients[0].modelsCallCount(); got != 1 {
		t.Errorf("upstream list calls = %d, want 1", got)
	}
}
