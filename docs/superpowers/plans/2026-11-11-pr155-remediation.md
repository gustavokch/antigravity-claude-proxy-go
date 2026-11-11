# PR #155 Remediation Implementation Plan

**Goal:** Preserve agy's account-level control-plane cadence when title generation is unavailable and after explicit quota refreshes.

**Architecture:** Keep account state and background work in `accounts.Dispatcher`. Expose first-turn eligibility from `format.CloudCodeRequest`, where the conversation counters already live, without changing the existing ACL/checkpoint eligibility or deduplication. Share the existing summary timestamp with explicit catalog/account quota reads.

**Tech Stack:** Go, standard `testing`, `testing/synctest`, existing Cloud Code boundary recorder.

**Spec:** PR https://github.com/gustavokch/antigravity-claude-proxy-go/pull/155 and `.reference/agy-1.5.6-control-plane-20261111.txt`.

## Global Constraints

- No TLS, transport, endpoint, protobuf, credential, or request-envelope changes.
- Start burst remains once per account; unanswered bursts retry.
- Summary refresh interval: 60 seconds per account. Account info interval: five minutes, on first model calls only.
- Background calls retain current timeouts, shutdown ownership, and response handling.
- Existing ACL/checkpoint behavior is out of scope.
- Work only in the existing PR worktree; push only to `fork`.

## Review Focus

- No flash-lite model: new conversations still refresh account data.
- Image-only first prompt: account refresh does not require title text.
- Follow-up after five minutes: no account-info refresh until a first model call.
- Manual account refresh: no redundant summary before 60 seconds.
- Explicit catalog fetch: the same summary timer applies.

## Task 1: Refresh account info independently of title generation

**Files:**
- Modify: `internal/format/agyrequest.go`, `internal/accounts/dispatcher.go`.
- Test: `internal/accounts/agy_sequence_test.go`.

**Interfaces:**
- Consumes: `Dispatcher.StreamGenerateContent`, `CloudCodeRequest`'s derived conversation and labels, existing `refreshAccountInfo`.
- Produces: `func (request *CloudCodeRequest) IsFirstTurn() bool`, true for a labelled request with a derived cascade and zero preceding model calls. Reuse this condition in `Builder.ConversationStart`.

- [ ] Step 1: Add `TestDispatcherRefreshesAccountInfoWithoutATitle`, with subtests for no flash-lite tier and an image-only prompt. Use this exact core timeline per subtest (the image case replaces the new request's user content with an image block):

```go
first := promptRequest("Fix the login bug")
sendTurn(t, dispatcher, "session-1", first)
synctest.Wait()
client.reset()
if err := dispatcher.sleep(context.Background(), 5*time.Minute); err != nil {
    t.Fatal(err)
}
sendTurn(t, dispatcher, "session-1", followUp(first, "Add a test"))
synctest.Wait()
assertSteps(t, withoutAgent(client.recorded()), [][]string{{"retrieveUserQuotaSummary"}})
client.reset()
sendTurn(t, dispatcher, "session-2", next)
synctest.Wait()
assertSteps(t, withoutAgent(client.recorded()), [][]string{{"fetchUserInfo"}, {"fetchAvailableModels"}, {"loadCodeAssist"}})
client.reset()
sendTurn(t, dispatcher, "session-3", promptRequest("Review the diff"))
synctest.Wait()
assertSteps(t, withoutAgent(client.recorded()), nil)
```

`withoutAgent` is a test-local closure filtering only the agent call from a cloned recorder result. In the image case, retain the flash-lite catalog to prove missing title text independently; use another image-only request for session-3 so it does not produce an unrelated checkpoint.

- [ ] Step 2: Run `go test ./internal/accounts -run '^TestDispatcherRefreshesAccountInfoWithoutATitle$' -count=1`. Expected: FAIL, account-info calls absent on the new conversation.
- [ ] Step 3: Implement `IsFirstTurn` in `internal/format/agyrequest.go`; reuse it in the existing checkpoint gate. In `Dispatcher.startConversation`, call `refreshAccountInfo` for first turns before the title-dependent early return. Remove the old refresh call below that return. Preserve all ACL/checkpoint conditions and deduplication.
- [ ] Step 4: Run `go test ./internal/accounts ./internal/format -run 'TestDispatcherRefreshesAccountInfoWithoutATitle|TestDispatcherSpacesTheCallsAgyRepeats|TestConversationStart|TestDispatcherSendsAgysConversationStartOnce' -count=1`. Expected: PASS.
- [ ] Step 5: Commit `git add internal/format/agyrequest.go internal/accounts/dispatcher.go internal/accounts/agy_sequence_test.go docs/superpowers/plans/2026-11-11-pr155-remediation.md && git commit -m 'fix(accounts): refresh account info without a title checkpoint'`.

## Task 2: Include explicit quota reads in the summary cadence

**Files:**
- Modify: `internal/accounts/dispatcher.go`, `README.md`, `CONTEXT.md`.
- Test: `internal/accounts/agy_sequence_test.go`.

**Interfaces:**
- Consumes: public `Dispatcher.RefreshAccount` and `Dispatcher.FetchAvailableModels`, existing `sentNow` and `quotaSummaryAt`.
- Produces: all summary sends outside the intentional start burst restart the same account timer; explicit refreshes remain unconditional.

- [ ] Step 1: Add `TestDispatcherSpacesSummaryAfterExplicitRefresh`, table-driven over public account refresh and catalog fetch. Use synctest and the existing recorder. Warm the account through generation, advance one minute, invoke the selected explicit refresh, reset the recorder, then use these assertions:

```go
second := followUp(first, "Add a test")
sendTurn(t, dispatcher, "session-1", second)
synctest.Wait()
assertSteps(t, client.recorded(), [][]string{{"agent"}})
client.reset()
if err := dispatcher.sleep(context.Background(), 59*time.Second); err != nil {
    t.Fatal(err)
}
third := followUp(second, "Review the change")
sendTurn(t, dispatcher, "session-1", third)
synctest.Wait()
assertSteps(t, client.recorded(), [][]string{{"agent"}})
client.reset()
if err := dispatcher.sleep(context.Background(), time.Second); err != nil {
    t.Fatal(err)
}
sendTurn(t, dispatcher, "session-1", followUp(third, "Explain the result"))
synctest.Wait()
calls := client.recorded()
slices.Sort(calls)
if !slices.Equal(calls, []string{"agent", "retrieveUserQuotaSummary"}) {
    t.Fatalf("calls at 60 seconds = %v", calls)
}
```

- [ ] Step 2: Run `go test ./internal/accounts -run '^TestDispatcherSpacesSummaryAfterExplicitRefresh$' -count=1`. Expected: FAIL, immediate redundant summary after either explicit refresh.
- [ ] Step 3: In `Dispatcher.refreshLiveQuota`, record `sentNow(quotaSummaryAt, account.Email)` immediately before `RetrieveUserQuotaSummary`. Do not throttle manual refreshes or change per-model quota behavior.
- [ ] Step 4: Run `go test ./internal/accounts -run 'TestDispatcherSpacesSummaryAfterExplicitRefresh|TestDispatcherSpacesTheCallsAgyRepeats|TestRefreshLiveQuota' -count=1`. Expected: PASS. Update the active README control-plane description and CONTEXT terms to the new burst/refresh cadence; retain historical capture/spec files unchanged.
- [ ] Step 5: Commit `git add internal/accounts/dispatcher.go internal/accounts/agy_sequence_test.go README.md CONTEXT.md docs/superpowers/plans/2026-11-11-pr155-remediation.md && git commit -m 'fix(accounts): include explicit quota reads in refresh cadence'`.

## Final Verification and Delivery

- Run the complete Go suite with race detection: `go test -json -race ./...`; count test outcomes from its JSON output.
- Run the existing packet JA4 gate once; report any environment limitation explicitly, never claim capture parity from a unit test.
- Run `trail check --base fork/main` before commits/push, recording any unavailable gate.
- Push `git push fork fix/control-plane-once-per-account` only after the suite is green.
- Post the remediation commits and observed verification to PR #155.

## Execution Record

- Initial review published on PR #155. Plan execution is inline as requested.
- The initial review also checks standards and captured behavior independently; only evidence-backed consumer-visible findings enter remediation.
- Task 1 proof: both missing-title subtests failed with no `fetchUserInfo` call before the fix. The focused accounts/format gate then passed (7 top-level tests); checkpoint/ACL gates retain their prior behavior.
