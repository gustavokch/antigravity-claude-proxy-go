package accounts

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"antigravity-go-proxy/internal/cloudcode"
	proxyformat "antigravity-go-proxy/internal/format"
)

// errDispatcherClosed refuses a stream that would have to outlive its client
// request once Close has run.
var errDispatcherClosed = errors.New("account dispatcher is closed")

// Close ends the work that outlives client requests (a stream still read past
// its client's output limit, agy's conversation-start calls): it cancels their
// context and waits for them. Call it once the HTTP server has drained.
func (dispatcher *Dispatcher) Close() {
	dispatcher.backgroundMu.Lock()
	dispatcher.closed = true
	dispatcher.backgroundMu.Unlock()
	if dispatcher.endLifetime != nil {
		dispatcher.endLifetime()
	}
	dispatcher.background.Wait()
}

// goBackground runs work on a goroutine that Close waits for. After Close it
// runs nothing and returns false.
func (dispatcher *Dispatcher) goBackground(work func()) bool {
	dispatcher.backgroundMu.Lock()
	defer dispatcher.backgroundMu.Unlock()
	if dispatcher.closed {
		return false
	}
	dispatcher.background.Add(1)
	go func() {
		defer dispatcher.background.Done()
		work()
	}()
	return true
}

// detach returns a context with ctx's values that the client cannot cancel and
// that Close cancels.
func (dispatcher *Dispatcher) detach(ctx context.Context) (context.Context, context.CancelFunc) {
	detached, cancel := context.WithCancel(context.WithoutCancel(ctx))
	if dispatcher.lifetime == nil {
		return detached, cancel
	}
	stop := context.AfterFunc(dispatcher.lifetime, cancel)
	return detached, func() {
		stop()
		cancel()
	}
}

// sendStream sends one streamGenerateContent on account and passes its events
// to consume. The caller has counted the send in the meter; sendStream ends it
// when upstream ends, and records what a clean stream reported (settle).
// events is the number of events passed on.
//
// With a guard (the client asked for fewer tokens than the catalog maximum
// the request carries), the client gets a MAX_TOKENS finish at its limit and
// sendStream returns, while upstream is read to its end in the background:
// agy sends the catalog maximum and reads every stream to its end, and closing
// the body early would reset the HTTP/2 stream, which agy never does. The
// client's context cancels the call until the finish; after it, only Close
// does, and a stream that hangs keeps only the transport's own timeouts. The
// read keeps the account's in-flight count until upstream ends, because
// upstream still serves the request, and records the credits and the quota
// it reports at the end. consume then runs on the reader's goroutine, never
// after sendStream returns.
func (dispatcher *Dispatcher) sendStream(ctx context.Context, account *Account, client CloudClient, project string,
	payload proxyformat.CloudCodeRequest, options cloudcode.RequestOptions, guard *proxyformat.OutputGuard,
	consume func(cloudcode.SSEEvent) error) (response cloudcode.Response, events int, err error) {
	if guard == nil {
		var credits []cloudcode.CreditBalance
		response, err = func() (cloudcode.Response, error) {
			// Deferred, so a panic in consume cannot strand the in-flight count.
			defer dispatcher.meter.End(account.Email)
			return client.StreamGenerateContent(ctx, payload, options, func(event cloudcode.SSEEvent) error {
				events++
				credits = creditsIn(event, credits)
				return consume(event)
			})
		}()
		if err == nil {
			dispatcher.settle(ctx, account, client, project, credits)
		}
		return response, events, err
	}

	upstreamCtx, cancel := dispatcher.detach(ctx)
	// Until the client has its finish, its cancel still reaches upstream.
	stopClient := context.AfterFunc(ctx, cancel)
	finished := make(chan struct{}) // closed once the client has its finish
	done := make(chan struct{})     // closed once the upstream call returned
	var panicked any
	past := false // the client has its finish; the caller reads it only after done
	started := dispatcher.goBackground(func() {
		defer close(done)
		defer cancel()
		defer stopClient()
		defer func() {
			value := recover()
			switch {
			case value == nil:
			case past:
				slog.Error("panic in a stream read past the client's output limit", "account", account.Email, "panic", value)
			default:
				// Raised again on the client's goroutine, as a panic in
				// consume was before.
				panicked = value
			}
		}()
		var credits []cloudcode.CreditBalance
		upstreamResponse, readErr := func() (cloudcode.Response, error) {
			defer dispatcher.meter.End(account.Email)
			return client.StreamGenerateContent(upstreamCtx, payload, options, func(event cloudcode.SSEEvent) error {
				credits = creditsIn(event, credits)
				if past {
					return nil
				}
				events++
				if err := consume(event); err != nil {
					return err
				}
				if final, reached := guard.Observe(event.Data); reached {
					if err := consume(cloudcode.SSEEvent{Data: final}); err != nil {
						return err
					}
					stopClient()
					past = true
					close(finished)
				}
				return nil
			})
		}()
		if readErr == nil {
			dispatcher.settle(upstreamCtx, account, client, project, credits)
		}
		if past {
			if readErr != nil {
				slog.Debug("stream read past the client's output limit ended with an error", "account", account.Email, "error", readErr)
			}
			return
		}
		response, err = upstreamResponse, readErr
	})
	if !started {
		cancel()
		stopClient()
		dispatcher.meter.End(account.Email)
		return cloudcode.Response{}, 0, errDispatcherClosed
	}
	select {
	case <-finished:
	case <-done:
		if panicked != nil {
			panic(panicked)
		}
		if !past {
			return response, events, err
		}
	}
	// upstream answered 200 with events; its headers arrive with the end of
	// the call, which the client no longer waits for.
	return cloudcode.Response{StatusCode: http.StatusOK}, events, nil
}

// creditsIn returns the remaining credits event reports, or last when it
// reports none: remaining_credits is the only per-request consumption signal
// upstream sends.
func creditsIn(event cloudcode.SSEEvent, last []cloudcode.CreditBalance) []cloudcode.CreditBalance {
	if len(event.Data) > 0 {
		if balances, ok := cloudcode.ParseRemainingCredits(event.Data); ok {
			return balances
		}
	}
	return last
}

// settle records what a stream that ended cleanly reported: the remaining
// credits, and a live quota reading taken after the generation.
func (dispatcher *Dispatcher) settle(ctx context.Context, account *Account, client CloudClient, project string, credits []cloudcode.CreditBalance) {
	if len(credits) > 0 {
		balances := make(map[string]int64, len(credits))
		for _, balance := range credits {
			balances[balance.CreditType] = balance.Amount
		}
		dispatcher.manager.UpdateAccountCredits(account.Email, balances)
	}
	dispatcher.refreshLiveQuotaThrottled(ctx, account, client, project)
}
