# Plan: PR #68 review remediation — bash-classifier fallback

Date: 2026-09-10. Source: review comment on
https://github.com/gustavokch/antigravity-claude-proxy-go/pull/68

## Goal

Close the two correctness bugs and the risk/nit findings from the review of PR #68
without changing the feature's intent: while `ANTIGRAVITY_PROXY_CLASSIFIER_FALLBACK`
is on and the account-backed dispatch path has no capacity, recognized Claude Code
bash-classifier calls get an instant canned "allow" verdict instead of stalling.

## Architecture

- `internal/classifier` stays pure: detection (fingerprint on the system prompt and the
  per-request footer) plus canned verdict construction. No I/O, no config.
- `internal/api/server.go` owns the gating decision: flag on, request non-streaming,
  request actually bound for the account-backed dispatcher, and no account capacity.
- `internal/config` owns the env flag. Unchanged by this remediation.

## Tech stack

Go stdlib only. Table-driven tests. `go test ./...`, `go vet ./...` as the gate.

## Task 1: Move the classifier gate below alternate-backend routing

Target files:
- Modify: `internal/api/server.go`
- Test: `internal/api/classifier_fallback_test.go`

Consumes: `classifier.Detect`, `classifier.Stub`, `Manager.Available`.
Produces: gating that only ever fires on the account-backed dispatch path.

Problem: the block sits at line 825, above the Kimi / ClaudeCode / OpenRouter /
custom-endpoint routing (lines 840-899). Those backends never consult
`accountManager`, so a classifier call routed to a healthy custom endpoint is
answered with a canned verdict based on unrelated Google-account capacity.

Step 1 — write the failing test. Rework the fixture so the "stub" cases run on the
account-backed path, and add a case asserting a custom-endpoint-routed classifier
request still reaches its backend:

```go
func TestMessages_ClassifierFallback_CustomEndpointRequestIsNeverStubbed(t *testing.T) {
	t.Setenv("ANTIGRAVITY_PROXY_CLASSIFIER_FALLBACK", "1")
	backendHit := false
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendHit = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"type":"message","content":[{"type":"text","text":"ok"}]}`))
	}))
	defer backend.Close()

	server := newClassifierFallbackTestServer("test-classifier-model", backend.URL)
	rec := postClassifierMessages(t, server, classifierShapedBody(t, "test-classifier-model", classifierStage1Footer))

	if !backendHit {
		t.Fatalf("custom endpoint does not use account capacity; classifier request must dispatch, not be stubbed; status=%d body=%s", rec.Code, rec.Body.String())
	}
}
```

Step 2 — run it, confirm it fails:
`go test ./internal/api/ -run ClassifierFallback -v`

Step 3 — implementation. Delete the block at `server.go:825-839` and reinsert it
immediately before `var send streamSender` (line 901), after every alternate-backend
early return. Keep `model` as the already-resolved value.

Step 4 — the remaining stub/fast-fail tests must now drive the account-backed path.
Replace `newClassifierFallbackTestServer`'s custom-endpoint wiring for those cases
with a `server.backend` stub sender, and assert the stub short-circuits before the
sender is invoked.

Step 5 — run to green, then commit:
`git commit -m "fix(api): gate classifier fallback on the account-backed path only"`

## Task 2: Never stub a streaming request

Target files:
- Modify: `internal/api/server.go`
- Test: `internal/api/classifier_fallback_test.go`

Step 1 — failing test: a classifier-shaped body with `"stream": true` and no capacity
must not receive `application/json`; it must dispatch normally.

Step 2 — confirm failure.

Step 3 — implementation: add `stream, _ := anthropicRequest["stream"].(bool)` to the
gate and skip the whole block when `stream` is true.

Step 4 — green.

Step 5 — `git commit -m "fix(api): never stub a streaming classifier request"`

## Task 3: Index-independent detection plus a cheap prefilter

Target files:
- Modify: `internal/classifier/classifier.go`
- Test: `internal/classifier/classifier_test.go`

Step 1 — failing test: a body whose monitor prompt sits at `system[2]` (an extra
leading block) must still be detected as `KindStage1Severity`.

Step 2 — confirm failure.

Step 3 — implementation: replace the `System[1]` index check with a scan over all
system blocks; add `bytes.Contains(body, []byte(monitorPromptPrefix))` before the
`json.Unmarshal` so ordinary traffic pays only a substring scan.

Step 4 — green.

Step 5 — `git commit -m "fix(classifier): detect monitor prompt at any system index"`

## Task 4: Non-retryable fast-fail, logging, honest usage count

Target files:
- Modify: `internal/api/server.go`, `internal/classifier/classifier.go`
- Test: `internal/api/classifier_fallback_test.go`

Step 1 — failing test: the unsupported-variant path returns 400
`invalid_request_error`, not 429.

Step 2 — confirm failure.

Step 3 — implementation:
- swap the 429 for a 400 `invalid_request_error` (a 429 invites the client's own
  backoff, which is the stall this feature removes);
- `slog.Warn` on both the stub and the fast-fail, carrying kind and model, so the
  security downgrade is auditable;
- replace `"output_tokens": len(verdictText)` with a small fixed count.

Step 4 — green.

Step 5 — `git commit -m "fix(classifier): non-retryable fast-fail, log stub, fix usage count"`

## Task 5: Docs

Update the README behaviour matrix and `docs/classifier-fallback-notes.md` to state
that only account-backed, non-streaming traffic is ever gated, and that the
unsupported variant now returns a non-retryable 400.

`git commit -m "docs: correct classifier fallback scope and fast-fail status"`

## Verification

`go build ./... && go vet ./... && go test ./...` — all packages green before push.
