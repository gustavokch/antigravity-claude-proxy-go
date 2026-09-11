# Plan: PR #68 second pass review remediation — bash-classifier fallback

Date: 2026-09-10. Source: second pass review comment on
https://github.com/gustavokch/antigravity-claude-proxy-go/pull/68

## Goal

Resolve the correctness bug, risk findings, and nits identified in the second review pass of PR #68:
1. Prevent `server.accountManager == nil` from stubbing requests when no account manager is configured.
2. Require active `accountManager` with zero available capacity for fallback to engage, and test with real exhausted manager.
3. Make `hasMonitorPrompt` resilient against leading whitespace or markdown headers.
4. Make footer detection inspect all message content blocks rather than strictly the last block.
5. Use instance `server.logger` for warning logs.

## Architecture

- `internal/classifier`: Pure detection and stub generation.
- `internal/api/server.go`: Quota gating and dispatch decisions.
- `internal/api/classifier_fallback_test.go`: End-to-end handler tests with real `accounts.Manager`.

## Tech Stack

Go stdlib. `go test ./...` and `go vet ./...`.

## Task 1: Fix `noCapacity` check and wire exhausted account manager in test fixture

Target files:
- Modify: `internal/api/server.go`
- Modify: `internal/api/classifier_fallback_test.go`

Problem: `noCapacity := server.accountManager == nil || server.accountManager.Available(model) == 0`.
When `accountManager` is nil (e.g. single-account / direct credential deployments), `noCapacity` evaluates to true, stubbing all classifier calls even with full upstream capacity.
Also, the test fixtures relied on `accountManager == nil` instead of testing an instantiated manager reporting zero capacity.

Step 1: Write failing tests in `internal/api/classifier_fallback_test.go`:
- Test that `server.accountManager == nil` dispatches normally (not stubbed).
- Update `newAccountBackedTestServer` to wire an empty/exhausted `accountManager` so stub tests test real manager exhaustion.

Step 2: Run test to confirm failure:
`go test ./internal/api/ -run ClassifierFallback -v`

Step 3: Implementation in `internal/api/server.go`:
Change `noCapacity` to:
`noCapacity := server.accountManager != nil && server.accountManager.Available(model) == 0`

Step 4: Run test to confirm pass.

Step 5: Commit:
`git commit -m "fix(api): do not stub classifier calls when account manager is nil"`

## Task 2: Resilient system prompt monitor detection

Target files:
- Modify: `internal/classifier/classifier.go`
- Modify: `internal/classifier/classifier_test.go`

Problem: `hasMonitorPrompt` uses `strings.HasPrefix(block.Text, monitorPromptPrefix)`. If upstream system prompt has a leading newline, whitespace, or markdown header (`# Security Monitor`), detection fails despite passing `bytes.Contains`.

Step 1: Write failing test in `internal/classifier/classifier_test.go`:
- System block containing leading newline and title before `monitorPromptPrefix` must be detected.

Step 2: Run test to confirm failure:
`go test ./internal/classifier/ -run Detect -v`

Step 3: Implementation in `internal/classifier/classifier.go`:
Change `strings.HasPrefix` to `strings.Contains(block.Text, monitorPromptPrefix)`.

Step 4: Run test to confirm pass.

Step 5: Commit:
`git commit -m "fix(classifier): scan system block for monitor prompt with strings.Contains"`

## Task 3: Resilient footer detection across content blocks

Target files:
- Modify: `internal/classifier/classifier.go`
- Modify: `internal/classifier/classifier_test.go`

Problem: `lastBlockText` strictly takes `blocks[len(blocks)-1]`. If an auxiliary block (e.g. cache control or empty block) is appended, detection fails.

Step 1: Write failing test in `internal/classifier/classifier_test.go`:
- Message content with footer followed by a trailing context/empty block must still be detected.

Step 2: Run test to confirm failure:
`go test ./internal/classifier/ -run Detect -v`

Step 3: Implementation in `internal/classifier/classifier.go`:
Parse content blocks and search across blocks in reverse for footer markers.

Step 4: Run test to confirm pass.

Step 5: Commit:
`git commit -m "fix(classifier): search content blocks for footer markers"`

## Task 4: Use instance logger in server.go

Target files:
- Modify: `internal/api/server.go`

Problem: Calls package-level `slog.Warn`, bypassing instance `server.logger`.

Step 1: Implementation in `internal/api/server.go`:
Use `server.logger.Warn` with nil check/fallback.

Step 2: Run all tests to confirm pass:
`go test ./...`

Step 3: Commit:
`git commit -m "fix(api): use server.logger for classifier fallback logs"`

## Verification

`go build ./... && go vet ./... && go test ./...` across the whole repository.
