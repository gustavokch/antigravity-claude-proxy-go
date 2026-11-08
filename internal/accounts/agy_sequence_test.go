package accounts

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"antigravity-go-proxy/internal/cloudcode"
)

// agyGenerationRuns reads .reference/client-drift/agy.jsonl as call names,
// split into its generation runs: a run ends with its last agent call. A
// generation is named by its requestType, any other call by its RPC name.
// A recapture that drops an endpoint the proxy sends fails the tests that
// read it.
func agyGenerationRuns(t *testing.T) [][]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", ".reference", "client-drift", "agy.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var runs [][]string
	var run []string
	for line := range strings.Lines(string(data)) {
		var record struct {
			Path        string `json:"path"`
			RequestBody struct {
				RequestType string `json:"requestType"`
			} `json:"request_body"`
		}
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("agy.jsonl: %v", err)
		}
		name := record.RequestBody.RequestType
		if !strings.Contains(record.Path, "streamGenerateContent") {
			path, _, _ := strings.Cut(record.Path, "?")
			name = path[strings.LastIndexAny(path, ":/")+1:]
		}
		if len(run) > 0 && run[len(run)-1] == "agent" && name != "agent" {
			runs = append(runs, run)
			run = nil
		}
		run = append(run, name)
	}
	return append(runs, run)
}

// missingCalls lists the calls of steps that run never makes. Only the set is
// compared: runs of one agy version reorder calls beyond its concurrent
// groups and send retrieveUserQuotaSummary a varying number of times.
func missingCalls(run []string, steps [][]string, except ...string) []string {
	var missing []string
	for _, step := range steps {
		for _, call := range step {
			if !slices.Contains(run, call) && !slices.Contains(except, call) && !slices.Contains(missing, call) {
				missing = append(missing, call)
			}
		}
	}
	return missing
}

// promptRequest is a first prompt of Claude Code's main loop: typed text and
// the tool list every main-loop request declares.
func promptRequest(text string) map[string]any {
	return map[string]any{
		"model": sequenceModel, "max_tokens": float64(1024), "tools": toolList(),
		"messages": []any{map[string]any{"role": "user", "content": text}},
	}
}

// toolContinuation is request answered with a tool call and followed by the
// tool's result, with the reminder block Claude Code appends to it.
func toolContinuation(request map[string]any) map[string]any {
	next := cloneRequest(request)
	next["messages"] = append(slices.Clone(request["messages"].([]any)),
		map[string]any{"role": "assistant", "content": []any{map[string]any{
			"type": "tool_use", "id": "toolu_1", "name": "Read", "input": map[string]any{"file_path": "login.go"}}}},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "package login"},
			map[string]any{"type": "text", "text": "<system-reminder>\nlogin.go is short.\n</system-reminder>"},
		}},
	)
	return next
}

// followUp is request answered with text and followed by a new prompt.
func followUp(request map[string]any, text string) map[string]any {
	next := cloneRequest(request)
	next["messages"] = append(slices.Clone(request["messages"].([]any)),
		map[string]any{"role": "assistant", "content": "Done."},
		map[string]any{"role": "user", "content": text},
	)
	return next
}

func sendTurn(t *testing.T, dispatcher *Dispatcher, session string, request map[string]any) {
	t.Helper()
	ctx := context.Background()
	if session != "" {
		ctx = WithSessionKey(ctx, session)
	}
	if _, err := dispatcher.StreamGenerateContent(ctx, request, func(cloudcode.SSEEvent) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

// A session's first prompt sends agy's start burst, the turn calls and the
// model call, and the tool continuation after it only the model call, in
// that order. Every endpoint of them is in both generation runs of agy.jsonl
// (cascadeNuxes in the later run only); the capture's order is not compared.
func TestDispatcherFollowsAgysCallSequence(t *testing.T) {
	t.Parallel()
	want := slices.Concat(agyStartBurst, agyTurn, [][]string{{"agent"}, {"agent"}})
	runs := agyGenerationRuns(t)
	if len(runs) != 2 {
		t.Fatalf("agy.jsonl holds %d generation runs, want 2: %v", len(runs), runs)
	}
	if missing := missingCalls(runs[0], want, "cascadeNuxes"); len(missing) > 0 {
		t.Fatalf("agy's first run %v never calls %v", runs[0], missing)
	}
	if missing := missingCalls(runs[1], want); len(missing) > 0 {
		t.Fatalf("agy's second run %v never calls %v", runs[1], missing)
	}

	const email = "a@example.com"
	client := &agyCallRecorder{modelsBody: []byte(sequenceModelsBody)}
	dispatcher := newControlPlaneDispatcher(t, map[string]*agyCallRecorder{email: client}, email)
	first := promptRequest("Fix the login bug")
	sendTurn(t, dispatcher, "session-1", first)
	sendTurn(t, dispatcher, "session-1", toolContinuation(first))
	assertSteps(t, client.recorded(), want)
}

// After the first turn, only a new prompt sends the turn calls: a tool-less
// side call, a tool continuation and a request without a session send only
// the model call.
func TestDispatcherSendsTurnCallsOnlyBeforeANewPrompt(t *testing.T) {
	t.Parallel()
	const email = "a@example.com"
	client := &agyCallRecorder{modelsBody: []byte(sequenceModelsBody)}
	dispatcher := newControlPlaneDispatcher(t, map[string]*agyCallRecorder{email: client}, email)
	first := promptRequest("Fix the login bug")
	sendTurn(t, dispatcher, "session-1", first)
	client.reset()

	title := map[string]any{"model": sequenceModel, "max_tokens": float64(64),
		"messages": []any{map[string]any{"role": "user", "content": "Write a five-word title for: Fix the login bug"}}}
	second := followUp(toolContinuation(first), "Now add a test")
	sendTurn(t, dispatcher, "session-1", title)
	sendTurn(t, dispatcher, "session-1", toolContinuation(first))
	sendTurn(t, dispatcher, "session-1", second)
	sendTurn(t, dispatcher, "", followUp(second, "And a changelog entry"))

	assertSteps(t, client.recorded(), slices.Concat([][]string{{"agent"}, {"agent"}}, agyTurn, [][]string{{"agent"}, {"agent"}}))
}

// A second prompt takes exactly one quota summary, before generation and
// none after the stream, and the turn calls leave the per-model quota as the
// start burst's model list set it: summary buckets are pools.
func TestDispatcherTakesOneQuotaSummaryPerPromptAndLeavesModelQuota(t *testing.T) {
	t.Parallel()
	const email = "a@example.com"
	client := &agyCallRecorder{
		modelsBody: []byte(sequenceModelsBody),
		quotaSummary: []byte(`{"groups":[{"displayName":"Gemini Models","buckets":[{"bucketId":"gemini-5h",
			"displayName":"Five Hour Limit Remaining","remainingFraction":0.4,"resetTime":"2026-11-07T17:00:00Z","window":"5h"}]}]}`),
	}
	dispatcher := newControlPlaneDispatcher(t, map[string]*agyCallRecorder{email: client}, email)
	first := promptRequest("Fix the login bug")
	sendTurn(t, dispatcher, "session-1", first)
	account := dispatcher.manager.accounts[0]
	before, err := json.Marshal(account.Quota.Models)
	if err != nil {
		t.Fatal(err)
	}
	client.reset()

	sendTurn(t, dispatcher, "session-1", followUp(first, "Now add a test"))

	calls := client.recorded()
	assertSteps(t, calls, slices.Concat(agyTurn, [][]string{{"agent"}}))
	if summaries := slices.Index(calls, "retrieveUserQuotaSummary"); summaries < 0 || summaries > slices.Index(calls, "agent") ||
		len(slices.DeleteFunc(slices.Clone(calls), func(call string) bool { return call != "retrieveUserQuotaSummary" })) != 1 {
		t.Fatalf("calls %v, want one retrieveUserQuotaSummary before the model call", calls)
	}
	after, err := json.Marshal(account.Quota.Models)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("the turn calls changed the model quota from %s to %s", before, after)
	}
}

// A prompt retried on the same account, after a 429 the session waits out
// on its pinned account, sends the turn calls once.
func TestDispatcherSendsTurnCallsOncePerAccountForARetriedPrompt(t *testing.T) {
	t.Parallel()
	const email = "a@example.com"
	client := &agyCallRecorder{modelsBody: []byte(sequenceModelsBody)}
	dispatcher := newControlPlaneDispatcher(t, map[string]*agyCallRecorder{email: client}, email)
	first := promptRequest("Fix the login bug")
	sendTurn(t, dispatcher, "session-1", first)
	client.reset()
	client.streamFailures = []error{&cloudcode.HTTPError{StatusCode: 429, Status: "429",
		Body: `{"error":{"code":429,"message":"Quota exceeded, quotaResetDelay 30s","status":"RESOURCE_EXHAUSTED"}}`}}

	sendTurn(t, dispatcher, "session-1", followUp(first, "Now add a test"))

	assertSteps(t, client.recorded(), slices.Concat(agyTurn, [][]string{{"agent"}, {"agent"}}))
}

// The start burst is per session and account: a second session on the same
// account gets its own, and so does the account a session overflows to.
func TestDispatcherSendsTheStartBurstPerSessionAndAccount(t *testing.T) {
	t.Parallel()
	home := &agyCallRecorder{modelsBody: []byte(sequenceModelsBody)}
	overflow := &agyCallRecorder{modelsBody: []byte(sequenceModelsBody)}
	dispatcher := newControlPlaneDispatcher(t, map[string]*agyCallRecorder{"a@example.com": home, "b@example.com": overflow},
		"a@example.com", "b@example.com")
	firstTurn := slices.Concat(agyStartBurst, agyTurn, [][]string{{"agent"}})

	first := promptRequest("Fix the login bug")
	sendTurn(t, dispatcher, "session-1", first)
	sendTurn(t, dispatcher, "session-2", promptRequest("Explain the build"))
	assertSteps(t, home.recorded(), slices.Concat(firstTurn, firstTurn))

	// A limit longer than maxStickyWait moves the session's model to the
	// other account.
	home.reset()
	dispatcher.manager.MarkRateLimited(dispatcher.manager.accounts[0], sequenceModel, time.Hour)
	sendTurn(t, dispatcher, "session-1", followUp(first, "Now add a test"))
	if calls := home.recorded(); len(calls) != 0 {
		t.Fatalf("the rate-limited home account got %v", calls)
	}
	assertSteps(t, overflow.recorded(), firstTurn)
}
