package accounts

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"antigravity-go-proxy/internal/cloudcode"
)

type recoveryClient struct {
	scriptedClient
	send func(context.Context, func(cloudcode.SSEEvent) error) (cloudcode.Response, error)
}

func (client *recoveryClient) StreamGenerateContent(ctx context.Context, _ any, _ cloudcode.RequestOptions, consume func(cloudcode.SSEEvent) error) (cloudcode.Response, error) {
	return client.send(ctx, consume)
}

func recoveryDispatcher(t *testing.T, send func(context.Context, func(cloudcode.SSEEvent) error) (cloudcode.Response, error)) (*Dispatcher, *Account) {
	t.Helper()
	account := testAccount("recovery@example.com")
	manager, err := New(Options{Accounts: []*Account{account}, Strategy: StrategySticky})
	if err != nil {
		t.Fatal(err)
	}
	client := &recoveryClient{send: send}
	dispatcher, err := NewDispatcher(DispatcherOptions{
		Manager: manager, Resolver: &staticResolver{tokens: map[string]string{account.Email: "token"}},
		NewClient: func(string) CloudClient { return client }, SwitchDelay: -1,
		Random: func() float64 { return 1 },
	})
	if err != nil {
		t.Fatal(err)
	}
	dispatcher.cacheCatalog(account.Email, []byte(`{"agentModelSorts":[{"groups":[{"modelIds":["gemini-3.8-flash-high"]}]}],"models":{"gemini-3.8-flash-high":{"supportsThinking":true,"thinkingBudget":24576,"maxOutputTokens":65536}}}`))
	t.Cleanup(dispatcher.Close)
	return dispatcher, account
}

func recoveryRequest() map[string]any {
	return map[string]any{"model": "gemini-3.8-flash-high", "messages": []any{map[string]any{"role": "user", "content": "Reply OK"}}}
}

func bare429() *cloudcode.HTTPError {
	return &cloudcode.HTTPError{StatusCode: http.StatusTooManyRequests, Header: make(http.Header), Body: `{"error":{"code":429,"message":"Resource has been exhausted (e.g. check quota).","status":"RESOURCE_EXHAUSTED"}}`}
}

func TestDispatcherBoundsSilentRecovery(t *testing.T) {
	for _, scenario := range []string{"bare rejection", "escalated rejection", "switch delay", "repeated short waits", "pinned selection", "extended cooldown", "capacity backoff", "retry first event", "retry pacing", "extension during retry pacing", "smaller configured window"} {
		t.Run(scenario, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cause := bare429()
				calls := 0
				var sentAt []time.Time
				dispatcher, account := recoveryDispatcher(t, func(ctx context.Context, _ func(cloudcode.SSEEvent) error) (cloudcode.Response, error) {
					calls++
					sentAt = append(sentAt, time.Now())
					if scenario == "retry first event" && calls > 1 {
						<-ctx.Done()
						return cloudcode.Response{}, ctx.Err()
					}
					return cloudcode.Response{}, cause
				})
				ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
				defer cancel()
				wantCalls := 1
				wantWait := 37500 * time.Millisecond
				switch scenario {
				case "escalated rejection":
					dispatcher.manager.IncrementFailure(account)
					wantWait = 75 * time.Second
				case "switch delay":
					dispatcher.switchDelay = 20 * time.Second
				case "repeated short waits":
					wantCalls = 0
					dispatcher.manager.MarkRateLimited(account, "gemini-3.8-flash-high", 2*time.Second)
					dispatcher.sleep = func(ctx context.Context, delay time.Duration) error {
						if err := sleepContext(ctx, delay); err != nil {
							return err
						}
						dispatcher.manager.MarkRateLimited(account, "gemini-3.8-flash-high", 2*time.Second)
						return nil
					}
				case "pinned selection":
					wantCalls = 0
					ctx = WithSessionKey(ctx, "pinned")
					dispatcher.manager.Select("pinned", "gemini-3.8-flash-high")
					dispatcher.manager.MarkRateLimited(account, "gemini-3.8-flash-high", 30*time.Second)
				case "extended cooldown":
					wantCalls = 0
					dispatcher.manager.MarkRateLimited(account, "gemini-3.8-flash-high", 2*time.Second)
					go func() {
						time.Sleep(time.Second)
						dispatcher.manager.MarkRateLimited(account, "gemini-3.8-flash-high", time.Minute)
					}()
				case "capacity backoff":
					cause.StatusCode = http.StatusServiceUnavailable
					cause.Body = `{"error":"MODEL_CAPACITY_EXHAUSTED"}`
					dispatcher.capacityBackoffs = []time.Duration{6 * time.Second}
					wantCalls = 2
				case "retry first event", "retry pacing":
					cause.Header.Set("Retry-After", "2")
					wantWait = 2500 * time.Millisecond
					wantCalls = 2
					if scenario == "retry pacing" {
						dispatcher.requestThrottlingEnabled = true
						dispatcher.requestDelay = 4 * time.Second
					}
				case "extension during retry pacing":
					cause.Header.Set("Retry-After", "2")
					dispatcher.requestThrottlingEnabled, dispatcher.requestDelay = true, 4*time.Second
					go func() {
						time.Sleep(7 * time.Second)
						dispatcher.manager.MarkRateLimited(account, "gemini-3.8-flash-high", 30*time.Second)
					}()
				case "smaller configured window":
					dispatcher.maxWait = time.Second
					cause.Header.Set("Retry-After", "2")
					wantWait = 2500 * time.Millisecond
				}
				start := time.Now()
				_, err := dispatcher.StreamGenerateContent(ctx, recoveryRequest(), func(cloudcode.SSEEvent) error { t.Error("unexpected forwarded event"); return nil })
				elapsed := time.Since(start)
				ceiling := 10 * time.Second
				if scenario == "retry pacing" || scenario == "extension during retry pacing" {
					ceiling += 4 * time.Second
				}
				if scenario == "smaller configured window" {
					ceiling = time.Second
				}
				if elapsed > ceiling || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
					t.Fatalf("recovery took %s, error %v", elapsed, err)
				}
				if calls != wantCalls {
					t.Fatalf("sends=%d, want %d", calls, wantCalls)
				}
				if scenario == "capacity backoff" {
					if cloudcode.FindHTTPError(err) != cause {
						t.Fatalf("capacity cause lost: %v", err)
					}
					return
				}
				var limited *RateLimitError
				if !errors.As(err, &limited) {
					t.Fatalf("want typed rate limit, got %T: %v", err, err)
				}
				if wantCalls > 0 && cloudcode.FindHTTPError(err) != cause {
					t.Fatalf("upstream cause lost: %v", err)
				}
				remaining := dispatcher.manager.MinWait("gemini-3.8-flash-high")
				if limited.RetryAfter < remaining {
					t.Fatalf("retry hint %s precedes eligibility %s", limited.RetryAfter, remaining)
				}
				if scenario == "bare rejection" || scenario == "escalated rejection" || scenario == "switch delay" || scenario == "smaller configured window" {
					if remaining != wantWait-elapsed {
						t.Fatalf("cooldown shortened: remaining=%s elapsed=%s want initial=%s", remaining, elapsed, wantWait)
					}
				}
				if len(sentAt) > 1 && sentAt[1].Sub(sentAt[0]) < wantWait {
					t.Fatalf("retried before applied eligibility: %s < %s", sentAt[1].Sub(sentAt[0]), wantWait)
				}
			})
		})
	}
}

func TestDispatcherRecoveryBudgetEndsAtFirstEvent(t *testing.T) {
	for _, recovered := range []bool{false, true} {
		t.Run(map[bool]string{false: "healthy slow first attempt", true: "recovered metadata event"}[recovered], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls, events := 0, 0
				dispatcher, _ := recoveryDispatcher(t, func(ctx context.Context, consume func(cloudcode.SSEEvent) error) (cloudcode.Response, error) {
					calls++
					if recovered && calls == 1 {
						err := bare429()
						err.Header.Set("Retry-After", "2")
						return cloudcode.Response{}, err
					}
					if !recovered {
						time.Sleep(15 * time.Second)
					}
					if err := consume(cloudcode.SSEEvent{Data: []byte(`{"response":{"usageMetadata":{"promptTokenCount":1}}}`)}); err != nil {
						return cloudcode.Response{}, err
					}
					time.Sleep(20 * time.Second)
					if err := ctx.Err(); err != nil {
						return cloudcode.Response{}, err
					}
					return cloudcode.Response{StatusCode: http.StatusOK}, consume(cloudcode.SSEEvent{Data: []byte(`{"response":{"candidates":[{"content":{"parts":[{"text":"OK"}]},"finishReason":"STOP"}]}}`)})
				})
				_, err := dispatcher.StreamGenerateContent(context.Background(), recoveryRequest(), func(cloudcode.SSEEvent) error { events++; return nil })
				if err != nil || events != 2 {
					t.Fatalf("stream was capped: events=%d error=%v", events, err)
				}
			})
		})
	}
}

func TestDispatcherRecoveryPreservesCancellation(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(map[bool]string{false: "client", true: "shutdown"}[shutdown], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				dispatcher, account := recoveryDispatcher(t, func(context.Context, func(cloudcode.SSEEvent) error) (cloudcode.Response, error) {
					t.Error("sent while cooling down")
					return cloudcode.Response{}, nil
				})
				dispatcher.manager.MarkRateLimited(account, "gemini-3.8-flash-high", 5*time.Second)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				go func() {
					time.Sleep(time.Second)
					if shutdown {
						dispatcher.Close()
					} else {
						cancel()
					}
				}()
				start := time.Now()
				_, err := dispatcher.StreamGenerateContent(ctx, recoveryRequest(), func(cloudcode.SSEEvent) error { return nil })
				want := context.Canceled
				if shutdown {
					want = errDispatcherClosed
				}
				if !errors.Is(err, want) || time.Since(start) != time.Second {
					t.Fatalf("elapsed=%s error=%v, want %v", time.Since(start), err, want)
				}
			})
		})
	}
}

func TestRecoveryAdviceDoesNotAdvanceRoundRobin(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pool := []*Account{testAccount("a@example.com"), testAccount("b@example.com"), testAccount("c@example.com")}
		manager, err := New(Options{Accounts: pool, Strategy: StrategyRoundRobin})
		if err != nil {
			t.Fatal(err)
		}
		var sends []string
		dispatcher, err := NewDispatcher(DispatcherOptions{
			Manager: manager, Resolver: accountTokenResolver{}, SwitchDelay: 20 * time.Second,
			NewClient: func(email string) CloudClient {
				return &recoveryClient{send: func(_ context.Context, consume func(cloudcode.SSEEvent) error) (cloudcode.Response, error) {
					sends = append(sends, email)
					if email == "b@example.com" {
						return cloudcode.Response{}, bare429()
					}
					return cloudcode.Response{StatusCode: http.StatusOK}, consume(cloudcode.SSEEvent{Data: []byte(`{}`)})
				}}
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(dispatcher.Close)
		dispatcher.cacheCatalog(pool[0].Email, []byte(sequenceModelsBody))
		request := map[string]any{"model": sequenceModel}
		if _, err := dispatcher.StreamGenerateContent(context.Background(), request, func(cloudcode.SSEEvent) error { return nil }); err == nil {
			t.Fatal("want the rejected request to fail before its switch delay")
		}
		ctx, metadata := cloudcode.WithExecutionMetadata(context.Background())
		if _, err := dispatcher.StreamGenerateContent(ctx, request, func(cloudcode.SSEEvent) error { return nil }); err != nil {
			t.Fatal(err)
		}
		if metadata.Account != "a@example.com" {
			t.Fatalf("retry advice advanced rotation: next account=%s sends=%v, want a@example.com after the selected-but-unsent c@example.com", metadata.Account, sends)
		}
	})
}

func TestRecoveryExpiryDuringStartBurstStopsGeneration(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		account := testAccount("startup@example.com")
		manager, err := New(Options{Accounts: []*Account{account}, Strategy: StrategySticky})
		if err != nil {
			t.Fatal(err)
		}
		client := &agyCallRecorder{modelsBody: []byte(sequenceModelsBody), hang: true}
		dispatcher, err := NewDispatcher(DispatcherOptions{Manager: manager, Resolver: accountTokenResolver{}, NewClient: func(string) CloudClient { return client }})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(dispatcher.Close)
		dispatcher.cacheCatalog(account.Email, []byte(sequenceModelsBody))
		manager.MarkRateLimited(account, sequenceModel, 2*time.Second)
		_, err = dispatcher.StreamGenerateContent(context.Background(), map[string]any{"model": sequenceModel}, func(cloudcode.SSEEvent) error { t.Error("event after expiry"); return nil })
		var limited *RateLimitError
		if !errors.As(err, &limited) {
			t.Fatalf("lost blocked-selection outcome: %v", err)
		}
		for _, call := range client.recorded() {
			if call == "agent" {
				t.Fatalf("generation started after recovery expiry: %v", client.recorded())
			}
		}
	})
}

func TestRecoveryBlockedPoolPreservesCredentialCause(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		first, second := testAccount("first@example.com"), testAccount("second@example.com")
		manager, err := New(Options{Accounts: []*Account{first, second}, Strategy: StrategySticky})
		if err != nil {
			t.Fatal(err)
		}
		manager.IncrementFailure(second)
		manager.IncrementFailure(second)
		credentialErr := errors.New("temporary credential store failure")
		resolver := &failingCredentialResolver{err: credentialErr, failing: second.Email}
		dispatcher, err := NewDispatcher(DispatcherOptions{
			Manager: manager, Resolver: resolver, SwitchDelay: -1,
			NewClient: func(string) CloudClient {
				return &recoveryClient{send: func(context.Context, func(cloudcode.SSEEvent) error) (cloudcode.Response, error) {
					return cloudcode.Response{}, bare429()
				}}
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(dispatcher.Close)
		dispatcher.cacheCatalog(first.Email, []byte(sequenceModelsBody))
		_, err = dispatcher.StreamGenerateContent(context.Background(), map[string]any{"model": sequenceModel}, func(cloudcode.SSEEvent) error { return nil })
		var limited *RateLimitError
		if !errors.As(err, &limited) || !errors.Is(err, credentialErr) || limited.RetryAfter < manager.MinWait(sequenceModel) {
			t.Fatalf("blocked pool lost its retry outcome or credential cause: %v", err)
		}
	})
}

func TestRecoveryUsesTheManagersExistingEligibility(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		dispatcher, account := recoveryDispatcher(t, func(_ context.Context, consume func(cloudcode.SSEEvent) error) (cloudcode.Response, error) {
			calls++
			if calls == 1 {
				rejection := bare429()
				rejection.Header.Set("Retry-After", "2")
				return cloudcode.Response{}, rejection
			}
			return cloudcode.Response{StatusCode: http.StatusOK}, consume(cloudcode.SSEEvent{Data: []byte(`{}`)})
		})
		account.Quota.Models = map[string]ModelQuota{"gemini-3.8-flash-high": {ExhaustedUntilMS: time.Now().Add(time.Hour).UnixMilli()}}
		_, err := dispatcher.StreamGenerateContent(context.Background(), recoveryRequest(), func(cloudcode.SSEEvent) error { return nil })
		if err != nil || calls != 2 {
			t.Fatalf("retry imposed an eligibility rule Select does not use: sends=%d error=%v", calls, err)
		}
	})
}
