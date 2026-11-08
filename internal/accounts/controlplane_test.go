package accounts

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"antigravity-go-proxy/internal/cloudcode"
	proxyformat "antigravity-go-proxy/internal/format"
	"antigravity-go-proxy/internal/modelcatalog"
)

// agyStartBurst and agyTurn are agy's control-plane calls, named as
// .reference/client-drift/agy.jsonl names them. Each step holds calls agy
// sends together, which may arrive in any order.
var (
	agyStartBurst = [][]string{
		{"loadCodeAssist"},
		{"fetchAdminControls", "fetchUserInfo", "retrieveUserQuotaSummary"},
		{"fetchAvailableModels"},
		{"retrieveUserQuotaSummary"},
		{"listExperiments"},
		{"loadCodeAssist"},
		{"cascadeNuxes"},
	}
	agyTurn = [][]string{
		{"loadCodeAssist"},
		{"fetchUserInfo", "retrieveUserQuotaSummary"},
	}
)

// sequenceModelsBody is a catalog without a flash-lite tier, so a first turn
// sends no title checkpoint and the calls a client records are the control
// plane's and the model call's only.
const sequenceModelsBody = `{
	"agentModelSorts":[{"groups":[{"modelIds":["gemini-3.8-flash-low"]}]}],
	"models":{"gemini-3.8-flash-low":{"displayName":"Gemini 3.8 Flash (Low)","supportsThinking":true,"thinkingLevel":1,
		"maxTokens":1048576,"maxOutputTokens":65536,"model":"MODEL_PLACEHOLDER_M320","apiProvider":"API_PROVIDER_GOOGLE_GEMINI"}}
}`

const sequenceModel = "gemini-3.8-flash-low"

var controlPlaneNow = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

// agyCallRecorder is a CloudClient with every capability agy's control plane
// uses. It records each call under its agy.jsonl name (a generation under its
// requestType) and the time each control-plane call had before its deadline.
type agyCallRecorder struct {
	modelsBody   []byte
	codeAssist   []byte
	quotaSummary []byte
	// hold, when set, keeps every loadCodeAssist waiting, after it is
	// recorded, until hold is closed.
	hold chan struct{}
	// streamFailures are returned, in order, by the next model calls.
	streamFailures []error

	mu       sync.Mutex
	failures map[string]error
	calls    []string
	budgets  []time.Duration
}

func (client *agyCallRecorder) answer(ctx context.Context, name string, body []byte) (cloudcode.Response, error) {
	client.mu.Lock()
	client.calls = append(client.calls, name)
	if deadline, ok := ctx.Deadline(); ok {
		client.budgets = append(client.budgets, time.Until(deadline))
	}
	err := client.failures[name]
	client.mu.Unlock()
	if err != nil {
		// As cloudcode.Client.DoJSON: no Response beside the error.
		return cloudcode.Response{}, err
	}
	if len(body) == 0 {
		body = []byte(`{}`)
	}
	return cloudcode.Response{StatusCode: http.StatusOK, Body: body}, nil
}

func (client *agyCallRecorder) LoadCodeAssist(ctx context.Context) (cloudcode.Response, error) {
	response, err := client.answer(ctx, "loadCodeAssist", client.codeAssist)
	if client.hold != nil {
		<-client.hold
	}
	return response, err
}

func (client *agyCallRecorder) FetchAvailableModels(ctx context.Context, _ string) (cloudcode.Response, error) {
	return client.answer(ctx, "fetchAvailableModels", client.modelsBody)
}

func (client *agyCallRecorder) RetrieveUserQuotaSummary(ctx context.Context, _ string) (cloudcode.Response, error) {
	return client.answer(ctx, "retrieveUserQuotaSummary", client.quotaSummary)
}

func (client *agyCallRecorder) FetchAdminControls(ctx context.Context, _ string) (cloudcode.Response, error) {
	return client.answer(ctx, "fetchAdminControls", nil)
}

func (client *agyCallRecorder) FetchUserInfo(ctx context.Context, _ string) (cloudcode.Response, error) {
	return client.answer(ctx, "fetchUserInfo", nil)
}

func (client *agyCallRecorder) ListExperiments(ctx context.Context, _ string) (cloudcode.Response, error) {
	return client.answer(ctx, "listExperiments", nil)
}

func (client *agyCallRecorder) ListCascadeNuxes(ctx context.Context) (cloudcode.Response, error) {
	return client.answer(ctx, "cascadeNuxes", nil)
}

func (client *agyCallRecorder) StreamGenerateContent(_ context.Context, payload any, _ cloudcode.RequestOptions, consume func(cloudcode.SSEEvent) error) (cloudcode.Response, error) {
	client.mu.Lock()
	client.calls = append(client.calls, payload.(proxyformat.CloudCodeRequest).RequestType)
	var failure error
	if len(client.streamFailures) > 0 {
		failure, client.streamFailures = client.streamFailures[0], client.streamFailures[1:]
	}
	client.mu.Unlock()
	if failure != nil {
		return cloudcode.Response{}, failure
	}
	if err := consume(cloudcode.SSEEvent{Data: agyEvent(1, "STOP")}); err != nil {
		return cloudcode.Response{StatusCode: http.StatusOK}, err
	}
	return cloudcode.Response{Endpoint: cloudcode.DailyEndpoint, StatusCode: http.StatusOK}, nil
}

func (client *agyCallRecorder) recorded() []string {
	client.mu.Lock()
	defer client.mu.Unlock()
	return slices.Clone(client.calls)
}

func (client *agyCallRecorder) deadlines() []time.Duration {
	client.mu.Lock()
	defer client.mu.Unlock()
	return slices.Clone(client.budgets)
}

// setFailure makes every later call named name return err; a nil err makes
// it answer again.
func (client *agyCallRecorder) setFailure(name string, err error) {
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.failures == nil {
		client.failures = make(map[string]error)
	}
	if err == nil {
		delete(client.failures, name)
		return
	}
	client.failures[name] = err
}

func (client *agyCallRecorder) reset() {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.calls, client.budgets = nil, nil
}

// newControlPlaneDispatcher builds a sticky pool of emails, in that order,
// each served by its recorder, with the catalog already cached so that no
// request fetches it. Its clock starts at controlPlaneNow and moves only when
// the dispatcher sleeps.
func newControlPlaneDispatcher(t *testing.T, clients map[string]*agyCallRecorder, emails ...string) *Dispatcher {
	t.Helper()
	pool := make([]*Account, 0, len(emails))
	tokens := make(map[string]string, len(emails))
	for _, email := range emails {
		pool = append(pool, testAccount(email))
		tokens[email] = "token-" + email
	}
	var clock sync.Mutex
	now := controlPlaneNow
	readClock := func() time.Time {
		clock.Lock()
		defer clock.Unlock()
		return now
	}
	manager, err := New(Options{Accounts: pool, Strategy: StrategySticky, Now: readClock})
	if err != nil {
		t.Fatal(err)
	}
	dispatcher, err := NewDispatcher(DispatcherOptions{
		Manager: manager, Resolver: &staticResolver{tokens: tokens},
		Now: readClock,
		Sleep: func(_ context.Context, wait time.Duration) error {
			clock.Lock()
			now = now.Add(wait)
			clock.Unlock()
			return nil
		},
		Random: func() float64 { return 0 },
		NewClient: func(token string) CloudClient {
			client := clients[strings.TrimPrefix(token, "token-")]
			if client == nil {
				t.Fatalf("unexpected token %q", token)
			}
			return client
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dispatcher.Close)
	catalog, err := modelcatalog.Parse([]byte(sequenceModelsBody))
	if err != nil {
		t.Fatal(err)
	}
	dispatcher.storeCatalog("", catalog)
	return dispatcher
}

// assertSteps checks calls against steps in order; the calls of one step may
// come in any order.
func assertSteps(t *testing.T, calls []string, steps [][]string) {
	t.Helper()
	rest := calls
	for index, step := range steps {
		if len(rest) < len(step) {
			t.Fatalf("calls %v end before step %d %v", calls, index, step)
		}
		got, want := slices.Clone(rest[:len(step)]), slices.Clone(step)
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Fatalf("step %d = %v, want %v (calls %v)", index, rest[:len(step)], step, calls)
		}
		rest = rest[len(step):]
	}
	if len(rest) > 0 {
		t.Fatalf("calls %v go on past the steps with %v", calls, rest)
	}
}

func toolList() []any {
	return []any{map[string]any{"name": "Read", "input_schema": map[string]any{"type": "object"}}}
}

func TestIsNewUserPrompt(t *testing.T) {
	t.Parallel()
	user := func(content any) map[string]any { return map[string]any{"role": "user", "content": content} }
	block := func(kind, text string) map[string]any { return map[string]any{"type": kind, "text": text} }
	toolResult := map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "package login"}
	reminder := block("text", "<system-reminder>\nlogin.go is short.\n</system-reminder>")
	for _, test := range []struct {
		name     string
		tools    []any
		messages []any
		want     bool
	}{
		{"typed string", toolList(), []any{user("Fix the login bug")}, true},
		{"text block", toolList(), []any{user([]any{block("text", "Fix the login bug")})}, true},
		{"image block", toolList(), []any{user([]any{map[string]any{"type": "image", "source": map[string]any{}}})}, true},
		{"tool result with typed text", toolList(), []any{user([]any{toolResult, block("text", "stop, use the other file")})}, true},
		{"tool result", toolList(), []any{user([]any{toolResult})}, false},
		{"tool result with reminder", toolList(), []any{user([]any{toolResult, reminder})}, false},
		{"document block", toolList(), []any{user([]any{toolResult, map[string]any{"type": "document", "source": map[string]any{}}})}, true},
		{"tool result with empty text", toolList(), []any{user([]any{toolResult, block("text", "")})}, false},
		{"blank string", toolList(), []any{user("  ")}, false},
		{"no tools", nil, []any{user("Write a five-word title")}, false},
		{"assistant last", toolList(), []any{user("hi"), map[string]any{"role": "assistant", "content": "hello"}}, false},
		{"no messages", toolList(), nil, false},
	} {
		request := map[string]any{"messages": test.messages}
		if test.tools != nil {
			request["tools"] = test.tools
		}
		if got := isNewUserPrompt(request); got != test.want {
			t.Errorf("%s: isNewUserPrompt = %v, want %v", test.name, got, test.want)
		}
	}
}

// Concurrent requests of one session on one account share one start burst,
// sent in agy's order with a 10 s deadline on each call; the session's later
// requests send none, and another session gets its own.
func TestBeginConversationSendsTheStartBurstOncePerSessionAndAccount(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const email = "a@example.com"
		client := &agyCallRecorder{modelsBody: []byte(sequenceModelsBody), hold: make(chan struct{})}
		dispatcher := newControlPlaneDispatcher(t, map[string]*agyCallRecorder{email: client}, email)
		account := dispatcher.manager.accounts[0]
		dispatcher.manager.Select("session-1", sequenceModel)

		var requests sync.WaitGroup
		for range 8 {
			requests.Go(func() {
				dispatcher.beginConversation(context.Background(), "session-1", account, client, "p")
			})
		}
		// Every request is blocked: one in the burst's first loadCodeAssist,
		// the others waiting for that burst.
		synctest.Wait()
		close(client.hold)
		requests.Wait()

		assertSteps(t, client.recorded(), agyStartBurst)
		for index, budget := range client.deadlines() {
			if budget != controlPlaneCallTimeout {
				t.Errorf("call %d had %s before its deadline, want %s", index, budget, controlPlaneCallTimeout)
			}
		}
		if !dispatcher.manager.conversationStarted("session-1", email) {
			t.Fatal("a burst whose calls all answered 2xx left the account unstarted")
		}

		client.reset()
		dispatcher.beginConversation(context.Background(), "session-1", account, client, "p")
		if calls := client.recorded(); len(calls) != 0 {
			t.Fatalf("a started session sent %v", calls)
		}
		dispatcher.manager.Select("session-2", sequenceModel)
		dispatcher.beginConversation(context.Background(), "session-2", account, client, "p")
		assertSteps(t, client.recorded(), agyStartBurst)
	})
}

// An error status is an answer: the burst completes and the account is
// started, so an account whose listExperiments always answers 403 does not
// resend all nine calls on each of the session's requests.
func TestBeginConversationCountsAnErrorStatusAsAnAnswer(t *testing.T) {
	t.Parallel()
	const email = "a@example.com"
	client := &agyCallRecorder{modelsBody: []byte(sequenceModelsBody)}
	client.setFailure("listExperiments", errors.Join(&cloudcode.HTTPError{
		StatusCode: http.StatusForbidden, Status: "403 Forbidden", Body: `{"error":{"code":403,"status":"PERMISSION_DENIED"}}`}))
	dispatcher := newControlPlaneDispatcher(t, map[string]*agyCallRecorder{email: client}, email)
	account := dispatcher.manager.accounts[0]
	dispatcher.manager.Select("session-1", sequenceModel)

	dispatcher.beginConversation(context.Background(), "session-1", account, client, "p")
	assertSteps(t, client.recorded(), agyStartBurst)
	if !dispatcher.manager.conversationStarted("session-1", email) {
		t.Fatal("a burst whose calls all got an HTTP answer left the account unstarted")
	}

	client.reset()
	dispatcher.beginConversation(context.Background(), "session-1", account, client, "p")
	if calls := client.recorded(); len(calls) != 0 {
		t.Fatalf("the session's next request sent %v", calls)
	}
}

// A call without an answer does not cut the burst short, but it leaves the
// account unstarted, so the session's next request sends the whole burst
// again.
func TestBeginConversationRetriesABurstWithAnUnansweredCall(t *testing.T) {
	t.Parallel()
	const email = "a@example.com"
	client := &agyCallRecorder{modelsBody: []byte(sequenceModelsBody)}
	client.setFailure("listExperiments", errors.New("Cloud Code request to https://daily-cloudcode-pa.googleapis.com: read: connection reset by peer"))
	dispatcher := newControlPlaneDispatcher(t, map[string]*agyCallRecorder{email: client}, email)
	account := dispatcher.manager.accounts[0]
	dispatcher.manager.Select("session-1", sequenceModel)

	dispatcher.beginConversation(context.Background(), "session-1", account, client, "p")
	assertSteps(t, client.recorded(), agyStartBurst)
	if dispatcher.manager.conversationStarted("session-1", email) {
		t.Fatal("a burst with an unanswered call marked the account started")
	}

	client.reset()
	client.setFailure("listExperiments", nil)
	dispatcher.beginConversation(context.Background(), "session-1", account, client, "p")
	assertSteps(t, client.recorded(), agyStartBurst)
	if !dispatcher.manager.conversationStarted("session-1", email) {
		t.Fatal("the resent burst got every answer and left the account unstarted")
	}
}

// The burst runs on the request's context: a client that has gone sends
// nothing, and the account stays unstarted.
func TestBeginConversationStopsWithTheClient(t *testing.T) {
	t.Parallel()
	const email = "a@example.com"
	client := &agyCallRecorder{modelsBody: []byte(sequenceModelsBody)}
	dispatcher := newControlPlaneDispatcher(t, map[string]*agyCallRecorder{email: client}, email)
	dispatcher.manager.Select("session-1", sequenceModel)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	dispatcher.beginConversation(ctx, "session-1", dispatcher.manager.accounts[0], client, "p")
	if calls := client.recorded(); len(calls) != 0 {
		t.Fatalf("a request whose client left sent %v", calls)
	}
	if dispatcher.manager.conversationStarted("session-1", email) {
		t.Fatal("a burst that sent nothing marked the account started")
	}
}

// Only a new prompt in a session's main loop sends the turn calls: tool
// continuations, tool-less side calls and requests without a session send
// none.
func TestBeginTurnSendsTheTurnCallsBeforeANewPromptOnly(t *testing.T) {
	t.Parallel()
	const email = "a@example.com"
	client := &agyCallRecorder{modelsBody: []byte(sequenceModelsBody)}
	dispatcher := newControlPlaneDispatcher(t, map[string]*agyCallRecorder{email: client}, email)
	account := dispatcher.manager.accounts[0]
	prompt := map[string]any{"tools": toolList(), "messages": []any{map[string]any{"role": "user", "content": "Fix the login bug"}}}
	continuation := map[string]any{"tools": toolList(), "messages": []any{
		map[string]any{"role": "user", "content": "Fix the login bug"},
		map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "toolu_1", "name": "Read", "input": map[string]any{}}}},
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "package login"}}},
	}}
	title := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "Write a five-word title for: Fix the login bug"}}}

	for _, request := range []map[string]any{continuation, title} {
		dispatcher.beginTurn(context.Background(), "session-1", account, client, "p", request)
	}
	dispatcher.beginTurn(context.Background(), "", account, client, "p", prompt)
	if calls := client.recorded(); len(calls) != 0 {
		t.Fatalf("requests that are no new prompt in a session sent %v", calls)
	}
	dispatcher.beginTurn(context.Background(), "session-1", account, client, "p", prompt)
	assertSteps(t, client.recorded(), agyTurn)
}

// Identical prompts of one session that arrive together share one run of
// the turn calls.
func TestBeginTurnSharesOneRunAmongIdenticalPrompts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const email = "a@example.com"
		client := &agyCallRecorder{modelsBody: []byte(sequenceModelsBody), hold: make(chan struct{})}
		dispatcher := newControlPlaneDispatcher(t, map[string]*agyCallRecorder{email: client}, email)
		account := dispatcher.manager.accounts[0]
		prompt := map[string]any{"tools": toolList(), "messages": []any{map[string]any{"role": "user", "content": "Fix the login bug"}}}

		var requests sync.WaitGroup
		for range 4 {
			requests.Go(func() {
				dispatcher.beginTurn(context.Background(), "session-1", account, client, "p", prompt)
			})
		}
		synctest.Wait()
		close(client.hold)
		requests.Wait()
		assertSteps(t, client.recorded(), agyTurn)
	})
}

// Identical prompts of one session on different accounts are separate runs:
// each account gets its own turn calls.
func TestBeginTurnRunsPerAccount(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hold := make(chan struct{})
		first := &agyCallRecorder{modelsBody: []byte(sequenceModelsBody), hold: hold}
		second := &agyCallRecorder{modelsBody: []byte(sequenceModelsBody), hold: hold}
		dispatcher := newControlPlaneDispatcher(t, map[string]*agyCallRecorder{"a@example.com": first, "b@example.com": second},
			"a@example.com", "b@example.com")
		prompt := map[string]any{"tools": toolList(), "messages": []any{map[string]any{"role": "user", "content": "Fix the login bug"}}}

		var requests sync.WaitGroup
		for index, client := range []*agyCallRecorder{first, second} {
			account := dispatcher.manager.accounts[index]
			requests.Go(func() {
				dispatcher.beginTurn(context.Background(), "session-1", account, client, "p", prompt)
			})
		}
		synctest.Wait()
		close(hold)
		requests.Wait()
		assertSteps(t, first.recorded(), agyTurn)
		assertSteps(t, second.recorded(), agyTurn)
	})
}

// loadCodeAssist answers set the account's tier and project, quota summaries
// its pools, and the burst's model list its catalog snapshot and model quota.
func TestControlPlaneAnswersUpdateTheAccount(t *testing.T) {
	t.Parallel()
	const email = "a@example.com"
	client := &agyCallRecorder{
		modelsBody: []byte(sequenceModelsBody),
		codeAssist: []byte(`{"cloudaicompanionProject":"proj-live","currentTier":{"id":"standard-tier"},"paidTier":{"id":"g1-pro-tier","name":"Google AI Pro"}}`),
		quotaSummary: []byte(`{"groups":[{"displayName":"Gemini Models","buckets":[{"bucketId":"gemini-5h",
			"displayName":"Five Hour Limit Remaining","remainingFraction":0.4,"resetTime":"2026-11-07T17:00:00Z","window":"5h"}]}]}`),
	}
	dispatcher := newControlPlaneDispatcher(t, map[string]*agyCallRecorder{email: client}, email)
	account := dispatcher.manager.accounts[0]
	dispatcher.manager.Select("session-1", sequenceModel)

	dispatcher.beginConversation(context.Background(), "session-1", account, client, "project-a@example.com")

	if got := dispatcher.manager.Project(account); got != "proj-live" {
		t.Errorf("project = %q, want the one loadCodeAssist named", got)
	}
	if account.Subscription.Tier != "pro" {
		t.Errorf("tier = %q, want pro from the paid tier", account.Subscription.Tier)
	}
	if pool, ok := account.Quota.Pools["gemini-5h"]; !ok || pool.RemainingFraction == nil || *pool.RemainingFraction != 0.4 {
		t.Errorf("gemini-5h pool = %+v (present %v), want the summary's 0.4", pool, ok)
	}
	if _, ok := account.Quota.Models[sequenceModel]; !ok {
		t.Errorf("model quota = %+v, want the burst's model list", account.Quota.Models)
	}
	dispatcher.mu.RLock()
	_, snapshot := dispatcher.catalogs[email]
	dispatcher.mu.RUnlock()
	if !snapshot {
		t.Error("the burst's model list did not become the account's catalog snapshot")
	}
}
