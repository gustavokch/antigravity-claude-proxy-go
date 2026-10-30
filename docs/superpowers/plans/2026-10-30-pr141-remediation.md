# PR #141 Remediation — agy 1.4.11 Cloud Code wire

**Goal:** The tests that guard PR #141's wire changes fail with a readable message instead of a panic, describe the behavior the client has now, and pin the one Cloud Code body that has no capture yet (`onboardUser`).

**Architecture:** `internal/cloudcode/client.go` sends every call to `DailyEndpoint`. It leaves `Accept-Encoding` and decompression to net/http, sends a streaming body with an unknown length (chunked), and sends `{"metadata":{"ideType":"ANTIGRAVITY"}}` on `loadCodeAssist`. `TestRequestHeadsOnTheWireMatchAgy` reads the raw request head that the production transport writes. These changes are test-only. No wire value changes, so `scripts/client_drift.py` sites stay the same.

**Tech stack:** Go (`go test`, `-race`), Python `unittest` for `scripts/test_client_drift.py`.

**Spec reference:** PR #141 review comment (https://github.com/gustavokch/antigravity-claude-proxy-go/pull/141#issuecomment-6524794093). Ground truth: `.reference/agy-1.4.10-wire-20261030.txt`, `.reference/agy-1.4.11-generation-20261030.txt`. Installed agy 1.4.11 (`strings`: `changelist 1010154931`).

## Findings and decisions

| Finding | Decision |
|---|---|
| 🟡 Claude calls add `anthropic-beta` (dispatcher.go:467, server.go:1199). agy sends none, so the header order differs on Claude. | Deferred to #142. #142 removes both lines and verified Claude still thinks. A change here would conflict with #142, which is stacked on this branch. |
| 🔵 `CALLER_REQUEST_KEYS` excludes `toolConfig` (client_drift.py:657). | Deferred to #142. #142 changes the same line to `{"tools"}` and adds an order check next to it. |
| 🔵 Wire test can panic on `stream.head[2]`/`[5]`. Its server goroutine calls `t.Error`. | Task 1 |
| 🔵 Stale test names: daily→prod fallback, `maybeGunzip`. | Task 2 |
| 🔵 `onboardUser` body and host have no test since the loadCodeAssist assertions were removed. | Task 3 |

## Task 1 — Wire test fails with a message, never a panic

**Files:** Modify `internal/cloudcode/client_test.go` (`TestRequestHeadsOnTheWireMatchAgy`).

**Consumes:** `New`, `LoadCodeAssist`, `StreamGenerateContent`. **Produces:** nothing new.

- Step 1 (red): remove `header.Set("Content-Type", "application/json")` from `New` for a moment, so each head has 5 lines. Run `go test -count=1 -run TestRequestHeadsOnTheWireMatchAgy ./internal/cloudcode`. Expect the two order errors, then `panic: runtime error: index out of range [5] with length 5` at `stream.head[5]`.
- Step 2: change the test:
  - The server goroutine sends `captured{err: err}` on the channel and does not call `t.Error`. The test goroutine reads it and calls `t.Fatal`.
  - The order checks use `t.Fatalf`, so the code that indexes the head runs only after the order matched.
- Step 3: with the `Content-Type` line still removed, run the test again. Expect `FAIL ... unary header order = [Host User-Agent Content-Length Authorization Accept-Encoding]`, with no panic. Put the line back.
- Step 4: `go test -count=1 -race -run TestRequestHeadsOnTheWireMatchAgy ./internal/cloudcode` → PASS.
- Step 5: `git commit -m "test(cloudcode): fail the wire header test instead of panicking on a short head"`

## Task 2 — Test names match the behavior they test

**Files:** Modify `internal/cloudcode/client_test.go`.

- Step 1: rename `TestFetchAvailableModelsHeadersAndDailyFallback` to `TestFetchAvailableModelsHeadersAndEndpointListFallback`. Rename its servers `daily`/`prod` to `first`/`second`. Add a comment: production lists hold only daily (see `TestEndpointsMatchAgyHost`), and this test covers how `DoJSON` walks an explicit list (`LoadCodeAssistAt`, `cmd/cloudcodecheck -endpoint`).
- Step 2: rename `TestDoSSEClosesBodyOnGunzipError` to `TestDoSSEFailsOnCorruptGzipStream`. Add a comment: net/http decodes the stream, so the error comes from the read in `ParseSSE`.
- Step 3: `go test -count=1 -run 'EndpointListFallback|CorruptGzip' ./internal/cloudcode -v` → both PASS.
- Step 4: `git commit -m "test(cloudcode): name the fallback and gzip tests after what they cover now"`

## Task 3 — Pin the uncaptured onboardUser body and host

**Files:** Modify `internal/cloudcode/client_test.go`.

**Consumes:** `OnboardUser`, `Metadata`, `platformEnum`, `ContentEndpoints`.

- Step 1 (test): `TestOnboardUserKeepsUncapturedBodyOnDaily`. A test server reads the raw body. It expects `{"tierId":"free-tier","metadata":{"ideType":9,"platform":<platformEnum()>,"duetProject":"project","pluginType":2}}` on `PathOnboardUser`. The test also checks `ContentEndpoints == []string{DailyEndpoint}`, because `OnboardUser` uses the content list.
- Step 2 (red, mutation): change `PluginType: 2` to `3` in `Metadata` for a moment. Run `go test -count=1 -run TestOnboardUser ./internal/cloudcode`. Expect FAIL with the body diff. Revert the change.
- Step 3: run it again → PASS.
- Step 4: `git commit -m "test(cloudcode): pin the onboardUser body until agy's is captured"`

## Verification

- `go build ./...`, `go vet ./...`, `go test ./...`
- `go test -race ./internal/cloudcode ./internal/accounts ./internal/api ./tools/...`
- `gofmt -l` on the changed Go files → empty
- `(cd scripts && python3 -m unittest test_client_drift test_mitm_header_dump)`
- `trail check`
- `git push fork fix/agy-1.4.10-wire` (never `origin`)

## Handover (operator only)

- `make client-drift` or `scripts/client_drift.py check --clients agy`: needs mitmdump and the real agy. Expected result: only the known `labels` item (closed by #142).
- JA4 gate on a capture of the installed proxy: `tcpdump -i pktap,all -P -w /tmp/go.pcap host daily-cloudcode-pa.googleapis.com -c 30`, then `tshark -r /tmp/go.pcap -Y 'tls.handshake.type==1' -T fields -e tls.handshake.ja4` → `t13d131100_f57a46bbacb6_f50d94e863eb`.
