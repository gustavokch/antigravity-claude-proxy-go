package accounts

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"antigravity-go-proxy/internal/cloudcode"
	"antigravity-go-proxy/internal/modelcatalog"
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

// A session's first prompt sends agy's start burst and the model call, and
// the tool continuation after it only the model call, in that order. Every
// endpoint of them is in both generation runs of agy.jsonl (cascadeNuxes in
// the later run only); the capture's order is not compared.
func TestDispatcherFollowsAgysCallSequence(t *testing.T) {
	t.Parallel()
	want := slices.Concat(agyStartBurst, [][]string{{"agent"}, {"agent"}})
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

// agy sends no control-plane call before a prompt: after the start burst, a
// tool-less side call, a tool continuation, a new prompt and a request
// without a session send only the model call.
func TestDispatcherSendsOnlyTheModelCallAfterTheStartBurst(t *testing.T) {
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

	assertSteps(t, client.recorded(), [][]string{{"agent"}, {"agent"}, {"agent"}, {"agent"}})
}

// Outside its start burst agy sends retrieveUserQuotaSummary beside a model
// call once the last one is a minute old, and fetchUserInfo,
// fetchAvailableModels and loadCodeAssist when a conversation starts once the
// last fetchUserInfo is five minutes old.
func TestDispatcherSpacesTheCallsAgyRepeats(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const email = "a@example.com"
		// agyModelsBody names a flash-lite tier, so a conversation's first
		// turn sends its title checkpoint, as agy's does.
		client := &agyCallRecorder{modelsBody: []byte(agyModelsBody)}
		dispatcher := newControlPlaneDispatcher(t, map[string]*agyCallRecorder{email: client}, email)
		catalog, err := modelcatalog.Parse([]byte(agyModelsBody))
		if err != nil {
			t.Fatal(err)
		}
		dispatcher.storeCatalog("", catalog)
		wait := func(d time.Duration) {
			if err := dispatcher.sleep(context.Background(), d); err != nil {
				t.Fatal(err)
			}
		}
		expect := func(step string, want map[string]int) {
			t.Helper()
			// The calls beside the model call run on their own goroutines.
			synctest.Wait()
			got := map[string]int{}
			for _, call := range client.recorded() {
				got[call]++
			}
			client.reset()
			if !maps.Equal(got, want) {
				t.Fatalf("%s: calls %v, want %v", step, got, want)
			}
		}

		first := promptRequest("Fix the login bug")
		sendTurn(t, dispatcher, "session-1", first)
		expect("first prompt", map[string]int{
			"loadCodeAssist": 2, "fetchAdminControls": 1, "fetchUserInfo": 1, "retrieveUserQuotaSummary": 2,
			"fetchAvailableModels": 1, "listExperiments": 1, "cascadeNuxes": 1, "checkpoint": 1, "agent": 1,
		})

		wait(59 * time.Second)
		second := followUp(first, "Now add a test")
		sendTurn(t, dispatcher, "session-1", second)
		expect("59 s after the burst", map[string]int{"agent": 1})

		wait(2 * time.Second)
		sendTurn(t, dispatcher, "session-1", followUp(second, "And a changelog entry"))
		expect("61 s after the burst", map[string]int{"agent": 1, "retrieveUserQuotaSummary": 1})

		wait(5 * time.Minute)
		sendTurn(t, dispatcher, "session-2", promptRequest("Explain the build"))
		expect("a conversation five minutes on", map[string]int{
			"checkpoint": 1, "agent": 1, "retrieveUserQuotaSummary": 1,
			"fetchUserInfo": 1, "fetchAvailableModels": 1, "loadCodeAssist": 1,
		})

		sendTurn(t, dispatcher, "session-3", promptRequest("Review the diff"))
		expect("a conversation right after", map[string]int{"checkpoint": 1, "agent": 1})
	})
}

// The start burst is per account: a request without a session sends it, a
// later session on the same account does not, and the account a session
// overflows to gets its own.
func TestDispatcherSendsTheStartBurstOncePerAccount(t *testing.T) {
	t.Parallel()
	home := &agyCallRecorder{modelsBody: []byte(sequenceModelsBody)}
	overflow := &agyCallRecorder{modelsBody: []byte(sequenceModelsBody)}
	dispatcher := newControlPlaneDispatcher(t, map[string]*agyCallRecorder{"a@example.com": home, "b@example.com": overflow},
		"a@example.com", "b@example.com")
	firstTurn := slices.Concat(agyStartBurst, [][]string{{"agent"}})

	first := promptRequest("Fix the login bug")
	sendTurn(t, dispatcher, "", promptRequest("Summarize the diff"))
	sendTurn(t, dispatcher, "session-1", first)
	sendTurn(t, dispatcher, "session-2", promptRequest("Explain the build"))
	assertSteps(t, home.recorded(), slices.Concat(firstTurn, [][]string{{"agent"}, {"agent"}}))

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
