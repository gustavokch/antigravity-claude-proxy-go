package accounts

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"antigravity-go-proxy/internal/cloudcode"
)

// controlPlaneCallTimeout bounds each control-plane call on its own. A call
// that times out goes unanswered and the run sends its next call; generation
// waits for the whole run, which controlPlaneRunTimeout bounds.
const controlPlaneCallTimeout = 10 * time.Second

// controlPlaneRunTimeout bounds one start burst as a whole. Its steps run one
// after another, so the per-call bound alone would let a burst of seven slow
// steps hold a request for 70 s.
const controlPlaneRunTimeout = 15 * time.Second

// quotaSummaryEvery and userInfoEvery are the shortest time agy 1.5.6 leaves
// between two retrieveUserQuotaSummary, or two fetchUserInfo, calls for its
// account while it runs. It sends the summary beside a model call, and
// fetchUserInfo with fetchAvailableModels and loadCodeAssist when a
// conversation starts, each only once its last one is this old: the capture
// skips a summary 59.7 s after the last one and sends it 67.8 s after, and
// skips fetchUserInfo 258 s after and sends it 308 s after
// (.reference/agy-1.5.6-control-plane-20261111.txt).
const (
	quotaSummaryEvery = time.Minute
	userInfoEvery     = 5 * time.Minute
)

// controlPlaneClient is the optional CloudClient capability for the calls of
// agy's start burst whose answers the proxy does not read
// (.reference/client-drift/agy.jsonl). A client without it, such as a test
// fake, sends no start burst.
type controlPlaneClient interface {
	FetchAdminControls(context.Context, string) (cloudcode.Response, error)
	FetchUserInfo(context.Context, string) (cloudcode.Response, error)
	ListExperiments(context.Context, string) (cloudcode.Response, error)
	ListCascadeNuxes(context.Context) (cloudcode.Response, error)
}

// The production client must have both capabilities: without one, the
// capability checks would skip those calls on every request, silently.
var (
	_ controlPlaneClient  = (*cloudcode.Client)(nil)
	_ quotaSummaryFetcher = (*cloudcode.Client)(nil)
)

// startAccount sends agy's start burst the first time this proxy uses
// account: loadCodeAssist; fetchAdminControls, fetchUserInfo and
// retrieveUserQuotaSummary at once; fetchAvailableModels;
// retrieveUserQuotaSummary; listExperiments; loadCodeAssist; cascadeNuxes.
// agy sends it once, when the process starts, and never again for a new
// conversation or a new prompt (.reference/agy-1.5.6-control-plane-20261111.txt),
// so the proxy, one long-lived client for every session, sends it once per
// account. Concurrent requests on the account share one burst. Once every
// call has an HTTP answer, whatever its status, the account is started; a
// call without one (a transport error, a timeout, a client that has gone)
// leaves it unstarted, and the account's next request sends the burst again.
// The burst stops after controlPlaneRunTimeout, and a call without an answer
// by then, or not yet sent, counts as unanswered. Its summaries and
// fetchUserInfo count for quotaSummaryEvery and userInfoEvery.
func (dispatcher *Dispatcher) startAccount(ctx context.Context, account *Account, client CloudClient, project string) {
	plane, capable := client.(controlPlaneClient)
	if !capable || dispatcher.accountStarted(account.Email) {
		return
	}
	dispatcher.shareControlCall(ctx, "start\x00"+account.Email, func() {
		// A burst that ended between the check above and this run has
		// already marked the account.
		if dispatcher.accountStarted(account.Email) {
			return
		}
		runCtx, cancel := context.WithTimeout(ctx, controlPlaneRunTimeout)
		defer cancel()
		run := &controlRun{dispatcher: dispatcher, ctx: runCtx, account: account, client: client, project: project}
		run.loadCodeAssist()
		together(
			func() { run.withProject("fetchAdminControls", plane.FetchAdminControls) },
			func() {
				dispatcher.sentNow(dispatcher.userInfoAt, account.Email)
				run.withProject("fetchUserInfo", plane.FetchUserInfo)
			},
			run.quotaSummary,
		)
		run.models()
		run.quotaSummary()
		run.withProject("listExperiments", plane.ListExperiments)
		run.loadCodeAssist()
		run.send("cascadeNuxes", plane.ListCascadeNuxes)
		if run.completed() {
			dispatcher.markAccountStarted(account.Email)
		}
	})
}

// accountStarted reports whether a start burst for email got every answer.
func (dispatcher *Dispatcher) accountStarted(email string) bool {
	dispatcher.mu.RLock()
	defer dispatcher.mu.RUnlock()
	return dispatcher.started[email]
}

func (dispatcher *Dispatcher) markAccountStarted(email string) {
	dispatcher.mu.Lock()
	dispatcher.started[email] = true
	dispatcher.mu.Unlock()
}

// due reports whether the account's last call recorded in last is at least
// every old, or was never sent, and records it as sent now when it is, so
// that concurrent requests send it once.
func (dispatcher *Dispatcher) due(last map[string]time.Time, email string, every time.Duration) bool {
	now := dispatcher.now()
	dispatcher.mu.Lock()
	defer dispatcher.mu.Unlock()
	if at, sent := last[email]; sent && now.Sub(at) < every {
		return false
	}
	last[email] = now
	return true
}

// sentNow records, in last, a call sent for the account now.
func (dispatcher *Dispatcher) sentNow(last map[string]time.Time, email string) {
	now := dispatcher.now()
	dispatcher.mu.Lock()
	last[email] = now
	dispatcher.mu.Unlock()
}

// refreshQuotaSummary sends retrieveUserQuotaSummary beside a model call once
// the account's last one is quotaSummaryEvery old.
func (dispatcher *Dispatcher) refreshQuotaSummary(ctx context.Context, account *Account, client CloudClient, project string) {
	fetcher, ok := client.(quotaSummaryFetcher)
	if !ok || !dispatcher.due(dispatcher.quotaSummaryAt, account.Email, quotaSummaryEvery) {
		return
	}
	dispatcher.sendBeside(ctx, "retrieveUserQuotaSummary", account, func(ctx context.Context) (cloudcode.Response, error) {
		return fetcher.RetrieveUserQuotaSummary(ctx, project)
	}, func(body []byte) { dispatcher.applyQuotaSummary(account.Email, body) })
}

// refreshAccountInfo sends, when a conversation starts once the account's
// last fetchUserInfo is userInfoEvery old, what agy sends then:
// fetchUserInfo, fetchAvailableModels and loadCodeAssist, one after another
// (.reference/agy-1.5.6-control-plane-20261111.txt). It runs on a goroutine
// that outlives the request, as sendBeside's calls do; the catalog and
// subscription answers are applied as the start burst's are.
func (dispatcher *Dispatcher) refreshAccountInfo(ctx context.Context, account *Account, client CloudClient, project string) {
	plane, ok := client.(controlPlaneClient)
	if !ok || !dispatcher.due(dispatcher.userInfoAt, account.Email, userInfoEvery) {
		return
	}
	dispatcher.goBackground(func() {
		detached, cancel := dispatcher.detach(ctx)
		defer cancel()
		runCtx, cancelTimeout := context.WithTimeout(detached, controlPlaneRunTimeout)
		defer cancelTimeout()
		run := &controlRun{dispatcher: dispatcher, ctx: runCtx, account: account, client: client, project: project}
		run.withProject("fetchUserInfo", plane.FetchUserInfo)
		run.models()
		run.loadCodeAssist()
	})
}

// sendBeside makes one control-plane call for account on a goroutine that
// outlives the request: agy sends these next to a model call and does not
// wait for them, so neither the request nor its outcome depends on them. A
// 2xx answer's body goes to apply, when set; a failure is only logged.
func (dispatcher *Dispatcher) sendBeside(ctx context.Context, name string, account *Account, call func(context.Context) (cloudcode.Response, error), apply func([]byte)) {
	dispatcher.goBackground(func() {
		detached, cancel := dispatcher.detach(ctx)
		defer cancel()
		ctx, cancelTimeout := context.WithTimeout(detached, controlPlaneCallTimeout)
		defer cancelTimeout()
		response, err := dispatcher.metered(account.Email, func() (cloudcode.Response, error) {
			return call(ctx)
		})
		if err != nil {
			slog.Debug("control-plane call failed", "call", name, "account", account.Email, "error", err)
			return
		}
		slog.Debug("control-plane call", "call", name, "account", account.Email, "status", response.StatusCode)
		if apply != nil {
			apply(response.Body)
		}
	})
}

// controlCall is one in-flight run of shareControlCall.
type controlCall struct {
	done      chan struct{}
	abandoned bool // set before done closes: the leader's request ended mid-run
}

// shareControlCall runs work once for key among the requests that ask for it
// at the same time; the others wait for it to end, or for their own context.
// A run whose leader's request ended before work returned was cut short for
// that request's sake only, so a waiter whose own request is still there
// takes the leader's place and runs work itself. A run that ended any other
// way, at its deadline too, stands for every request that waited on it.
func (dispatcher *Dispatcher) shareControlCall(ctx context.Context, key string, work func()) {
	dispatcher.mu.Lock()
	for {
		call, running := dispatcher.controlCalls[key]
		if !running {
			break
		}
		dispatcher.mu.Unlock()
		select {
		case <-call.done:
		case <-ctx.Done():
			return
		}
		// abandoned is written before done closes, so it is settled here.
		if !call.abandoned || ctx.Err() != nil {
			return
		}
		dispatcher.mu.Lock()
	}
	call := &controlCall{done: make(chan struct{})}
	dispatcher.controlCalls[key] = call
	dispatcher.mu.Unlock()
	// Deferred, so that a panic in work still releases the waiters.
	defer func() {
		call.abandoned = ctx.Err() != nil
		dispatcher.mu.Lock()
		delete(dispatcher.controlCalls, key)
		dispatcher.mu.Unlock()
		close(call.done)
	}()
	work()
}

// controlRun sends one start burst's calls for an account and notes whether
// any of them went without an answer.
type controlRun struct {
	dispatcher *Dispatcher
	ctx        context.Context
	account    *Account
	client     CloudClient
	project    string

	mu         sync.Mutex
	unanswered bool
}

// send makes one call under its own timeout, counted by the throttle meter,
// and returns the body of a 2xx answer. Any HTTP answer, an error status too,
// counts as answered: a call that always answers 4xx for an account must not
// make each of its requests resend the burst. A call without an answer (a
// transport error, a timeout, or a run whose client has gone or whose
// deadline has passed, after which nothing is sent) marks the run
// unanswered. Each outcome is logged at debug.
func (run *controlRun) send(name string, call func(context.Context) (cloudcode.Response, error)) ([]byte, bool) {
	if run.ctx.Err() != nil {
		run.markUnanswered()
		return nil, false
	}
	ctx, cancel := context.WithTimeout(run.ctx, controlPlaneCallTimeout)
	defer cancel()
	response, err := run.dispatcher.metered(run.account.Email, func() (cloudcode.Response, error) {
		return call(ctx)
	})
	if err != nil {
		// DoJSON returns no Response with a non-2xx answer; its status is
		// on the HTTPError.
		if upstream := cloudcode.FindHTTPError(err); upstream != nil {
			slog.Debug("control-plane call", "call", name, "account", run.account.Email, "status", upstream.StatusCode)
			return nil, false
		}
		slog.Debug("control-plane call got no answer", "call", name, "account", run.account.Email, "error", err)
		run.markUnanswered()
		return nil, false
	}
	slog.Debug("control-plane call", "call", name, "account", run.account.Email, "status", response.StatusCode)
	return response.Body, response.StatusCode >= 200 && response.StatusCode < 300
}

// withProject sends a call that names the account's project.
func (run *controlRun) withProject(name string, call func(context.Context, string) (cloudcode.Response, error)) ([]byte, bool) {
	return run.send(name, func(ctx context.Context) (cloudcode.Response, error) {
		return call(ctx, run.project)
	})
}

func (run *controlRun) loadCodeAssist() {
	if body, ok := run.send("loadCodeAssist", run.client.LoadCodeAssist); ok {
		run.dispatcher.applyCodeAssist(run.account, body)
	}
}

func (run *controlRun) quotaSummary() {
	fetcher, ok := run.client.(quotaSummaryFetcher)
	if !ok {
		return
	}
	run.dispatcher.sentNow(run.dispatcher.quotaSummaryAt, run.account.Email)
	if body, ok := run.withProject("retrieveUserQuotaSummary", fetcher.RetrieveUserQuotaSummary); ok {
		run.dispatcher.applyQuotaSummary(run.account.Email, body)
	}
}

func (run *controlRun) models() {
	if body, ok := run.withProject("fetchAvailableModels", run.client.FetchAvailableModels); ok {
		run.dispatcher.cacheCatalog(run.account.Email, body)
		run.dispatcher.updateAccountQuota(run.account, body)
	}
}

func (run *controlRun) markUnanswered() {
	run.mu.Lock()
	run.unanswered = true
	run.mu.Unlock()
}

// completed reports whether every call of the run got an HTTP answer.
func (run *controlRun) completed() bool {
	run.mu.Lock()
	defer run.mu.Unlock()
	return !run.unanswered
}

// together sends calls at once and waits for all of them: agy sends these
// calls concurrently, the capture holds them in both orders.
func together(calls ...func()) {
	var group sync.WaitGroup
	for _, call := range calls {
		group.Go(call)
	}
	group.Wait()
}

// applyCodeAssist records what a loadCodeAssist answer says about account:
// its tier and, when the answer names one, its Cloud Code project.
func (dispatcher *Dispatcher) applyCodeAssist(account *Account, body []byte) Subscription {
	sub := ExtractSubscription(body, dispatcher.now())
	if sub.ProjectID != "" {
		dispatcher.manager.CacheProject(account, sub.ProjectID)
	}
	if err := dispatcher.manager.UpdateSubscription(account.Email, sub); err != nil {
		slog.Debug("saving the account's subscription failed", "account", account.Email, "error", err)
	}
	return sub
}

// applyQuotaSummary merges a retrieveUserQuotaSummary answer. Its buckets are
// shared pools (gemini-5h, 3p-weekly, ...), so they land in Quota.Pools and
// never surface as model rows.
func (dispatcher *Dispatcher) applyQuotaSummary(email string, body []byte) {
	for _, bucket := range cloudcode.ParseQuotaSummary(body) {
		dispatcher.mergeQuotaReading(email, strings.ToLower(strings.TrimSpace(bucket.ID)),
			bucket.RemainingFraction, bucket.RemainingAmount, bucket.ResetTime, true)
	}
}
