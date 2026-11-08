package accounts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"time"

	"antigravity-go-proxy/internal/cloudcode"
)

// controlPlaneCallTimeout bounds each control-plane call on its own: a slow
// call delays generation by at most this much, and a timeout only fails its
// run.
const controlPlaneCallTimeout = 10 * time.Second

// controlPlaneClient is the optional CloudClient capability for the calls agy
// 1.5.2 sends around a conversation whose answers the proxy does not read
// (.reference/client-drift/agy.jsonl). A client without it, such as a test
// fake, sends no control-plane calls.
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

// beginConversation sends agy's start burst the first time session uses
// account: loadCodeAssist; fetchAdminControls, fetchUserInfo and
// retrieveUserQuotaSummary at once; fetchAvailableModels;
// retrieveUserQuotaSummary; listExperiments; loadCodeAssist; cascadeNuxes
// (.reference/client-drift/agy.jsonl; cascadeNuxes is in its later run only).
// Concurrent requests of the session on the account share one burst. Once
// every call has an HTTP answer, whatever its status, the account is started
// for the session; a call without one (a transport error, a timeout, a client
// that has gone) leaves it unstarted, and the session's next request sends
// the burst again.
func (dispatcher *Dispatcher) beginConversation(ctx context.Context, session string, account *Account, client CloudClient, project string) {
	plane, capable := client.(controlPlaneClient)
	if session == "" || !capable || dispatcher.manager.conversationStarted(session, account.Email) {
		return
	}
	dispatcher.shareControlCall(ctx, "start\x00"+session+"\x00"+account.Email, func() {
		// A burst that ended between the check above and this run has
		// already marked the account.
		if dispatcher.manager.conversationStarted(session, account.Email) {
			return
		}
		run := &controlRun{dispatcher: dispatcher, ctx: ctx, account: account, client: client, project: project}
		run.loadCodeAssist()
		together(
			func() { run.withProject("fetchAdminControls", plane.FetchAdminControls) },
			func() { run.withProject("fetchUserInfo", plane.FetchUserInfo) },
			run.quotaSummary,
		)
		run.models()
		run.quotaSummary()
		run.withProject("listExperiments", plane.ListExperiments)
		run.loadCodeAssist()
		run.send("cascadeNuxes", plane.ListCascadeNuxes)
		if run.completed() {
			dispatcher.manager.markConversationStarted(session, account.Email)
		}
	})
}

// beginTurn sends what agy sends before each new prompt: loadCodeAssist, then
// fetchUserInfo and retrieveUserQuotaSummary at once
// (.reference/client-drift/agy.jsonl). Tool continuations and tool-less side
// calls (titles, topic checks) send nothing. Identical prompts of the session
// that arrive together share one run.
func (dispatcher *Dispatcher) beginTurn(ctx context.Context, session string, account *Account, client CloudClient, project string, request map[string]any) {
	plane, capable := client.(controlPlaneClient)
	if session == "" || !capable || !isNewUserPrompt(request) {
		return
	}
	messages, _ := request["messages"].([]any)
	// A decoded JSON body always encodes again.
	prompt, _ := json.Marshal(messages[len(messages)-1])
	digest := sha256.Sum256(prompt)
	dispatcher.shareControlCall(ctx, "turn\x00"+session+"\x00"+hex.EncodeToString(digest[:]), func() {
		run := &controlRun{dispatcher: dispatcher, ctx: ctx, account: account, client: client, project: project}
		run.loadCodeAssist()
		together(
			func() { run.withProject("fetchUserInfo", plane.FetchUserInfo) },
			run.quotaSummary,
		)
	})
}

// isNewUserPrompt reports whether request carries something a person sent to
// a main loop: it declares tools, which title and topic side calls do not,
// and its last message is a user message with more than tool results and
// injected <system-reminder> text. That is ccidentity's prompt rule (promptOf)
// read from the decoded body: an image or document block counts, an empty or
// reminder text block does not.
func isNewUserPrompt(request map[string]any) bool {
	if tools, _ := request["tools"].([]any); len(tools) == 0 {
		return false
	}
	messages, _ := request["messages"].([]any)
	if len(messages) == 0 {
		return false
	}
	last, _ := messages[len(messages)-1].(map[string]any)
	if role, _ := last["role"].(string); role != "user" {
		return false
	}
	switch content := last["content"].(type) {
	case string:
		return isPromptText(content)
	case []any:
		for _, raw := range content {
			block, _ := raw.(map[string]any)
			switch kind, _ := block["type"].(string); kind {
			case "tool_result":
			case "text":
				if text, _ := block["text"].(string); isPromptText(text) {
					return true
				}
			default:
				return true
			}
		}
	}
	return false
}

func isPromptText(text string) bool {
	text = strings.TrimSpace(text)
	return text != "" && !strings.HasPrefix(text, "<system-reminder>")
}

// shareControlCall runs work once for key among the requests that ask for it
// at the same time; the others wait for it to end, or for their own context.
func (dispatcher *Dispatcher) shareControlCall(ctx context.Context, key string, work func()) {
	dispatcher.mu.Lock()
	if done, running := dispatcher.controlCalls[key]; running {
		dispatcher.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
		}
		return
	}
	done := make(chan struct{})
	dispatcher.controlCalls[key] = done
	dispatcher.mu.Unlock()
	defer func() {
		dispatcher.mu.Lock()
		delete(dispatcher.controlCalls, key)
		dispatcher.mu.Unlock()
		close(done)
	}()
	work()
}

// controlRun sends one burst's or one turn's calls for an account and notes
// whether any of them went without an answer.
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
// transport error, a timeout, or a client that has gone, after which nothing
// is sent) marks the run unanswered. Each outcome is logged at debug.
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
