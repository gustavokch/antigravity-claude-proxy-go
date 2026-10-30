package accounts

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"antigravity-go-proxy/internal/cloudcode"
	proxyformat "antigravity-go-proxy/internal/format"
)

// agyClient is a CloudClient with agy's conversation-start capability. It
// records every generation payload and trajectory ACL.
type agyClient struct {
	modelsBody []byte
	events     [][]byte

	mu       sync.Mutex
	payloads []proxyformat.CloudCodeRequest
	acls     []string
}

func (client *agyClient) LoadCodeAssist(context.Context) (cloudcode.Response, error) {
	return cloudcode.Response{StatusCode: http.StatusOK, Body: []byte(`{"cloudaicompanionProject":"aicode-consumers"}`)}, nil
}

func (client *agyClient) FetchAvailableModels(context.Context, string) (cloudcode.Response, error) {
	return cloudcode.Response{StatusCode: http.StatusOK, Body: client.modelsBody}, nil
}

func (client *agyClient) StreamGenerateContent(_ context.Context, payload any, _ cloudcode.RequestOptions, consume func(cloudcode.SSEEvent) error) (cloudcode.Response, error) {
	client.mu.Lock()
	client.payloads = append(client.payloads, payload.(proxyformat.CloudCodeRequest))
	client.mu.Unlock()
	for _, data := range client.events {
		if err := consume(cloudcode.SSEEvent{Data: data}); err != nil {
			return cloudcode.Response{StatusCode: http.StatusOK}, err
		}
	}
	return cloudcode.Response{Endpoint: cloudcode.DailyEndpoint, StatusCode: http.StatusOK}, nil
}

func (client *agyClient) WriteTrajectoryAcls(_ context.Context, trajectoryID string) (cloudcode.Response, error) {
	client.mu.Lock()
	client.acls = append(client.acls, trajectoryID)
	client.mu.Unlock()
	return cloudcode.Response{StatusCode: http.StatusOK, Body: []byte(`{}`)}, nil
}

func (client *agyClient) byType(requestType string) []proxyformat.CloudCodeRequest {
	client.mu.Lock()
	defer client.mu.Unlock()
	var out []proxyformat.CloudCodeRequest
	for _, payload := range client.payloads {
		if payload.RequestType == requestType {
			out = append(out, payload)
		}
	}
	return out
}

func agyEvent(candidates int, finish string) []byte {
	candidate := map[string]any{"content": map[string]any{"role": "model", "parts": []any{map[string]any{"text": "x"}}}}
	if finish != "" {
		candidate["finishReason"] = finish
	}
	encoded, _ := json.Marshal(map[string]any{"response": map[string]any{
		"candidates": []any{candidate}, "usageMetadata": map[string]any{"candidatesTokenCount": candidates},
	}})
	return encoded
}

const agyModelsBody = `{
	"agentModelSorts":[{"groups":[{"modelIds":["gemini-3.8-flash-low"]}]}],
	"tieredModelIds":{"flashLite":["gemini-3.5-flash-lite"]},
	"models":{"gemini-3.8-flash-low":{"displayName":"Gemini 3.8 Flash (Low)","supportsThinking":true,"thinkingLevel":1,
		"maxTokens":1048576,"maxOutputTokens":65536,"model":"MODEL_PLACEHOLDER_M320","apiProvider":"API_PROVIDER_GOOGLE_GEMINI"}}
}`

// The request carries the catalog maximum, as agy sends it, and the client's
// max_tokens ends the response where upstream would have: the event that
// crosses it, then a MAX_TOKENS finish, and nothing after.
func TestDispatcherEndsTheStreamAtTheClientOutputLimit(t *testing.T) {
	t.Parallel()
	client := &agyClient{modelsBody: []byte(agyModelsBody), events: [][]byte{agyEvent(30, ""), agyEvent(120, ""), agyEvent(150, "STOP")}}
	dispatcher := newDispatcherWithClient(t, client)
	var delivered [][]byte
	_, err := dispatcher.StreamGenerateContent(context.Background(), map[string]any{
		"model": "gemini-3.8-flash-low", "max_tokens": float64(100),
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}, func(event cloudcode.SSEEvent) error {
		delivered = append(delivered, event.Data)
		return nil
	})
	if err != nil {
		t.Fatalf("a response ended at the client's limit is a success, got %v", err)
	}
	agent := client.byType("agent")
	if len(agent) != 1 {
		t.Fatalf("agent calls = %d", len(agent))
	}
	if got := agent[0].Request["generationConfig"].(map[string]any)["maxOutputTokens"]; got != 65536 {
		t.Fatalf("maxOutputTokens = %v, want the catalog maximum", got)
	}
	if len(delivered) != 3 {
		t.Fatalf("delivered %d events, want the two before the limit and the MAX_TOKENS finish", len(delivered))
	}
	var final struct {
		Response struct {
			Candidates []struct {
				FinishReason string `json:"finishReason"`
			} `json:"candidates"`
		} `json:"response"`
	}
	if err := json.Unmarshal(delivered[2], &final); err != nil || len(final.Response.Candidates) != 1 || final.Response.Candidates[0].FinishReason != "MAX_TOKENS" {
		t.Fatalf("final event = %s", delivered[2])
	}
}

// A conversation's first turn sends agy's start calls once, with the
// trajectory its labels carry; a repeat of the first turn and a later turn
// send none.
func TestDispatcherSendsAgysConversationStartOnce(t *testing.T) {
	t.Parallel()
	client := &agyClient{modelsBody: []byte(agyModelsBody), events: [][]byte{agyEvent(1, "STOP")}}
	dispatcher := newDispatcherWithClient(t, client)
	first := []any{map[string]any{"role": "user", "content": "Fix the login bug"}}
	later := append(append([]any(nil), first...),
		map[string]any{"role": "assistant", "content": "done"}, map[string]any{"role": "user", "content": "thanks"})
	for _, messages := range [][]any{first, first, later} {
		request := map[string]any{"model": "gemini-3.8-flash-low", "max_tokens": float64(1024), "messages": messages}
		if _, err := dispatcher.StreamGenerateContent(context.Background(), request, func(cloudcode.SSEEvent) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	// The start calls run beside the first model call; the later calls
	// decided synchronously not to send any, so once these arrive the counts
	// are final.
	deadline := time.Now().Add(5 * time.Second)
	for len(client.byType("checkpoint")) == 0 || func() bool { client.mu.Lock(); defer client.mu.Unlock(); return len(client.acls) == 0 }() {
		if time.Now().After(deadline) {
			t.Fatal("the conversation start calls never arrived")
		}
		time.Sleep(5 * time.Millisecond)
	}
	agent := client.byType("agent")
	checkpoints := client.byType("checkpoint")
	client.mu.Lock()
	acls := append([]string(nil), client.acls...)
	client.mu.Unlock()
	if len(agent) != 3 || len(checkpoints) != 1 || len(acls) != 1 {
		t.Fatalf("agent=%d checkpoint=%d acl=%d, want 3, 1, 1", len(agent), len(checkpoints), len(acls))
	}
	if labels := agent[0].Request["labels"].(map[string]string); acls[0] != labels["trajectory_id"] {
		t.Fatalf("ACL trajectory %q, first call's labels %v", acls[0], labels)
	}
	if checkpoints[0].Model != "gemini-3.5-flash-lite" || checkpoints[0].Project != "aicode-consumers" {
		t.Fatalf("checkpoint = %#v", checkpoints[0])
	}
}

// agy builds its requestId 1-160 ms before the request leaves. The
// dispatcher builds the payload before the throttle delay, so it stamps the
// id again when it sends.
func TestDispatcherStampsTheRequestIDWhenItSends(t *testing.T) {
	t.Parallel()
	var clock atomic.Int64
	clock.Store(1793346815000)
	now := func() time.Time { return time.UnixMilli(clock.Load()) }
	builder := proxyformat.NewBuilder()
	builder.Now = now
	manager, err := New(Options{Accounts: []*Account{{Email: "test@example.com", Enabled: true}}})
	if err != nil {
		t.Fatal(err)
	}
	client := &agyClient{modelsBody: []byte(agyModelsBody), events: [][]byte{agyEvent(1, "STOP")}}
	dispatcher, err := NewDispatcher(DispatcherOptions{
		Manager: manager, Resolver: stubResolver{}, NewClient: func(string) CloudClient { return client },
		Builder: builder, Now: now, RequestThrottlingEnabled: true, RequestDelay: 200 * time.Millisecond,
		Sleep: func(_ context.Context, wait time.Duration) error {
			clock.Add(wait.Milliseconds())
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := map[string]any{"model": "gemini-3.8-flash-low", "max_tokens": float64(1024),
		"messages": []any{map[string]any{"role": "user", "content": "hello"}}}
	if _, err := dispatcher.StreamGenerateContent(context.Background(), request, func(cloudcode.SSEEvent) error { return nil }); err != nil {
		t.Fatal(err)
	}
	agent := client.byType("agent")
	if len(agent) != 1 {
		t.Fatalf("agent calls = %d", len(agent))
	}
	if parts := strings.Split(agent[0].RequestID, "/"); len(parts) != 5 || parts[2] != "1793346815200" {
		t.Fatalf("requestId = %q, want the send time 1793346815200 (after the 200 ms throttle delay)", agent[0].RequestID)
	}
}

// upstreamTail is a CloudClient whose stream sends head, waits for release,
// then sends tail and ends: upstream generating past the client's limit. Like
// cloudcode.Client.DoSSE, it stops reading at the first consume error or when
// its context ends.
type upstreamTail struct {
	agyClient
	head, tail [][]byte
	release    chan struct{}
	ended      chan struct{} // closed when the stream call returns
	// Set before ended is closed.
	readToEOF, cancelled bool
}

func newUpstreamTail(head, tail [][]byte) *upstreamTail {
	return &upstreamTail{
		agyClient: agyClient{modelsBody: []byte(agyModelsBody)},
		head:      head, tail: tail, release: make(chan struct{}), ended: make(chan struct{}),
	}
}

func (upstream *upstreamTail) StreamGenerateContent(ctx context.Context, payload any, options cloudcode.RequestOptions, consume func(cloudcode.SSEEvent) error) (cloudcode.Response, error) {
	if built, ok := payload.(proxyformat.CloudCodeRequest); !ok || built.RequestType != "agent" {
		return upstream.agyClient.StreamGenerateContent(ctx, payload, options, func(cloudcode.SSEEvent) error { return nil })
	}
	defer close(upstream.ended)
	send := func(events [][]byte) error {
		for _, data := range events {
			if err := consume(cloudcode.SSEEvent{Data: data}); err != nil {
				return err
			}
		}
		return nil
	}
	if err := send(upstream.head); err != nil {
		return cloudcode.Response{StatusCode: http.StatusOK}, err
	}
	select {
	case <-upstream.release:
	case <-ctx.Done():
		upstream.cancelled = true
		return cloudcode.Response{StatusCode: http.StatusOK}, ctx.Err()
	}
	if err := send(upstream.tail); err != nil {
		return cloudcode.Response{StatusCode: http.StatusOK}, err
	}
	upstream.cancelled = ctx.Err() != nil
	upstream.readToEOF = !upstream.cancelled
	return cloudcode.Response{StatusCode: http.StatusOK}, nil
}

func waitFor(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// At the client's max_tokens the client gets its finish at once, and upstream
// is read to its end, as agy reads every stream: no cancel, also after the
// client's request context ends. The read holds the account's in-flight slot
// until upstream ends, and the credits upstream reports at the end count.
func TestDispatcherFinishesAtTheClientLimitAndReadsUpstreamToTheEnd(t *testing.T) {
	t.Parallel()
	credits := []byte(`{"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"x"}]},"finishReason":"STOP"}],` +
		`"usageMetadata":{"candidatesTokenCount":160}},"remainingCredits":[{"creditType":"GOOGLE_ONE_AI","creditAmount":555}]}`)
	upstream := newUpstreamTail([][]byte{agyEvent(30, ""), agyEvent(120, "")}, [][]byte{agyEvent(150, ""), credits})
	dispatcher := newDispatcherWithClient(t, upstream)
	clientCtx, disconnect := context.WithCancel(context.Background())
	defer disconnect()
	var delivered [][]byte
	_, err := dispatcher.StreamGenerateContent(clientCtx, map[string]any{
		"model": "gemini-3.8-flash-low", "max_tokens": float64(100),
		"messages": []any{map[string]any{"role": "user", "content": "hello"}},
	}, func(event cloudcode.SSEEvent) error {
		delivered = append(delivered, event.Data)
		return nil
	})
	if err != nil {
		t.Fatalf("a response ended at the client's limit is a success, got %v", err)
	}
	select {
	case <-upstream.ended:
		t.Fatalf("the client got its finish only after upstream ended (read to EOF %v, cancelled %v)", upstream.readToEOF, upstream.cancelled)
	default:
	}
	if len(delivered) != 3 || !strings.Contains(string(delivered[2]), `"MAX_TOKENS"`) {
		t.Fatalf("delivered %d events, want the two before the limit and the MAX_TOKENS finish", len(delivered))
	}
	if inFlight, _ := dispatcher.meter.Observe("test@example.com"); inFlight != 1 {
		t.Fatalf("in flight = %d while upstream still generates, want 1", inFlight)
	}

	disconnect() // the client leaves once it has its finish
	close(upstream.release)
	select {
	case <-upstream.ended:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream never ended")
	}
	if !upstream.readToEOF || upstream.cancelled {
		t.Fatalf("upstream read to EOF %v, cancelled %v; want read to the end, never cancelled", upstream.readToEOF, upstream.cancelled)
	}
	waitFor(t, "the in-flight slot and the trailing credits", func() bool {
		inFlight, _ := dispatcher.meter.Observe("test@example.com")
		dispatcher.manager.mu.RLock()
		defer dispatcher.manager.mu.RUnlock()
		return inFlight == 0 && dispatcher.manager.accounts[0].Credits["GOOGLE_ONE_AI"] == 555
	})
	if len(delivered) != 3 {
		t.Fatalf("%d events reached the client; nothing may follow the finish", len(delivered))
	}
}

// Close ends a read that runs past the client's limit, so a shutdown leaves
// no goroutine behind even when upstream never ends; after Close a request
// that would need one is refused.
func TestDispatcherCloseEndsAReadPastTheClientLimit(t *testing.T) {
	t.Parallel()
	upstream := newUpstreamTail([][]byte{agyEvent(30, ""), agyEvent(120, "")}, nil) // never released
	dispatcher := newDispatcherWithClient(t, upstream)
	request := map[string]any{"model": "gemini-3.8-flash-low", "max_tokens": float64(100),
		"messages": []any{map[string]any{"role": "user", "content": "hello"}}}
	if _, err := dispatcher.StreamGenerateContent(context.Background(), request, func(cloudcode.SSEEvent) error { return nil }); err != nil {
		t.Fatal(err)
	}
	closed := make(chan struct{})
	go func() {
		dispatcher.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not end the read past the client's limit")
	}
	if !upstream.cancelled {
		t.Fatal("Close returned while the read was still running")
	}
	if inFlight, _ := dispatcher.meter.Observe("test@example.com"); inFlight != 0 {
		t.Fatalf("in flight = %d after Close", inFlight)
	}
	if _, err := dispatcher.StreamGenerateContent(context.Background(), request, func(cloudcode.SSEEvent) error { return nil }); !errors.Is(err, errDispatcherClosed) {
		t.Fatalf("a request after Close = %v, want errDispatcherClosed", err)
	}
}

// A panic in the client's consume before the finish is raised on the
// client's goroutine, as before; it stops upstream and frees the in-flight
// slot.
func TestDispatcherRaisesAConsumePanicOnTheCallersGoroutine(t *testing.T) {
	t.Parallel()
	upstream := newUpstreamTail([][]byte{agyEvent(30, "")}, nil) // never released
	dispatcher := newDispatcherWithClient(t, upstream)
	request := map[string]any{"model": "gemini-3.8-flash-low", "max_tokens": float64(100),
		"messages": []any{map[string]any{"role": "user", "content": "hello"}}}
	recovered := func() (value any) {
		defer func() { value = recover() }()
		_, _ = dispatcher.StreamGenerateContent(context.Background(), request, func(cloudcode.SSEEvent) error { panic("client write") })
		return nil
	}()
	if recovered != "client write" {
		t.Fatalf("recovered %v, want the consume panic", recovered)
	}
	select {
	case <-upstream.ended:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream kept running after the panic")
	}
	waitFor(t, "the in-flight slot", func() bool {
		inFlight, _ := dispatcher.meter.Observe("test@example.com")
		return inFlight == 0
	})
}
