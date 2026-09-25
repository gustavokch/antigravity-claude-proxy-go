# Remediation Plan: PR #97 Final Pass

**Date**: 2026-09-24
**PR**: #97 (`fix(cloudcode): resolve upstream 429 fallback and corrupted thought signature`)
**Branch**: `fix/upstream-429-thought-signature` (head `48b7ea4`)
**Spec reference**: PR #97 review comments
[5825045358](https://github.com/gustavokch/antigravity-claude-proxy-go/pull/97#issuecomment-5825045358)
(final pass) and
[5825182227](https://github.com/gustavokch/antigravity-claude-proxy-go/pull/97#issuecomment-5825182227)
(live run). AGENTS.md "The one rule that matters most" (agy disguise).
**Supersedes**: the round-1 plan `2026-09-24-pr97-review-remediation.md`. All three of its tasks are already on the branch (`159a594`, `a00cd49`, `48b7ea4`).

## Goal

1. Content generation goes to exactly one host, and that host is **agy's**:
   `DailyEndpoint`. PR #97 currently pins it to `ProdEndpoint`.
2. Metadata RPCs (`onboardUser`, `fetchAvailableModels`, `retrieveUserQuota*`)
   go back to main's `[ProdEndpoint, DailyEndpoint]` order. PR #97 dropped their
   fallback even though their failures have nothing to do with thought
   signatures.
3. Remove the forwarding wrapper `accounts.findHTTPError` and the test that
   only exercises that wrapper.
4. Make `TestFindHTTPError` table-driven and nil-safe, and write the full
   precedence order into the doc comment.
5. Packet-verify SNI/JA4 for generation. Update the PR body and AGENTS.md.

## Evidence (live run, 2026-09-24)

`agy 1.2.10 --print --log-file=/tmp/agy-endpoint-run.log`, every Cloud Code URL
in the log:

```
4 https://daily-cloudcode-pa.googleapis.com/v1internal:loadCodeAssist
1 https://daily-cloudcode-pa.googleapis.com/v1internal:fetchAvailableModels
2 https://daily-cloudcode-pa.googleapis.com/v1internal:streamGenerateContent?alt=sse
```

There are no `cloudcode-pa` (prod) URLs. The checked-in agy 1.1.2 baseline
shows the same host (`.reference/agy-current-baseline.txt:L27-L35`). The proxy
first diverged in `3510aad` (2026-08-17, prod-first for everything). PR #97 then
made generation prod-only.

Why exactly one host: according to the PR's own root-cause analysis, a thought
signature issued by one host is rejected by the other (`INVALID_ARGUMENT:
Corrupted thought signature`). Falling back across hosts therefore turns a
retryable 429 into a permanent 400.

## Architecture & Tech Stack

- Go 1.27rc2, standard library only (`net/http`, `errors`, `testing`,
  `net/http/httptest`, `sync/atomic`).
- `internal/cloudcode/client.go`: endpoint lists, the `Client` struct,
  `New`, `GenerateContent`/`StreamGenerateContent`, `FindHTTPError`.
- `internal/accounts/dispatcher.go`: 429/5xx/auth rotation through
  `FindHTTPError` (L455, L734).
- TLS is not touched (the transport keeps an empty `tls.Config{}`). Only the
  request host changes, and JA4 does not encode the hostname: the `d` in
  `t13d...` only records that SNI is present.

## Cutover risk (document; no code change)

- **Thinking parts**: not affected. `SignatureCache` lives only in memory
  (`internal/format/signature_cache.go`), so after a deploy restart
  `stripInvalidThinkingBlocks` (`internal/format/thinking.go:L247-L255`) drops
  Gemini thinking parts whose signatures it no longer knows.
- **Function calls**: a `thoughtSignature` is forwarded exactly as the client
  sent it (`internal/format/content.go:L60-L76`). A tool loop that is running
  during the deploy sends a prod-issued signature to daily and gets one
  `Corrupted thought signature` 400. Recovery is one user re-prompt. According
  to the `internal/format/toggles.go:L16-L19` comment, Gemini only validates
  function-call signatures from the current turn; this is not independently
  verified.

## Not in scope

- SNI parity for metadata RPCs. agy also sends `loadCodeAssist` and
  `fetchAvailableModels` to daily, while the proxy has sent them prod-first
  since `3510aad`. That is an older, separate decision; this PR only restores
  main's behavior.

---

## Tasks

### Task 1: Pin generation to `DailyEndpoint`; restore the metadata fallback

- **Target files**:
  - Modify: `internal/cloudcode/client.go` (L38-L41 vars, L56-L57 struct,
    L205-L206 `New`, L263 `GenerateContent`, L267 `StreamGenerateContent`)
  - Test: `internal/cloudcode/client_test.go` (imports L3-L18; L156, L178
    stream tests; replace L207-L218)
- **Consumes / Produces**:
  - Produces the exported `cloudcode.GenerationEndpoints []string` (default
    `[DailyEndpoint]`) and the private field `Client.generationEndpoints`.
  - `ContentEndpoints` goes back to `[ProdEndpoint, DailyEndpoint]`.
    `ProvisioningEndpoints` is unchanged.
  - Nothing outside `internal/cloudcode` reads `ContentEndpoints` (`cmd/*`
    refers to `DailyEndpoint`/`ProdEndpoint` directly).

- **Step 1: Write failing tests**

  Add `"sync/atomic"` to the imports in `client_test.go`.

  Replace `TestContentTargetsProductionAndProvisioningIncludesDailyFallback`
  (L207-L218) with:

  ```go
  func TestEndpointDefaults(t *testing.T) {
  	t.Parallel()
  	// Generation is pinned to agy's host with no cross-host fallback: agy
  	// 1.2.10 sends streamGenerateContent to daily (SNI parity), and a thought
  	// signature issued by one host is rejected by the other.
  	if want := []string{DailyEndpoint}; !reflect.DeepEqual(GenerationEndpoints, want) {
  		t.Errorf("GenerationEndpoints = %#v, want %#v", GenerationEndpoints, want)
  	}
  	if want := []string{ProdEndpoint, DailyEndpoint}; !reflect.DeepEqual(ContentEndpoints, want) {
  		t.Errorf("ContentEndpoints = %#v, want %#v", ContentEndpoints, want)
  	}
  	if want := []string{ProdEndpoint, DailyEndpoint}; !reflect.DeepEqual(ProvisioningEndpoints, want) {
  		t.Errorf("ProvisioningEndpoints = %#v, want %#v", ProvisioningEndpoints, want)
  	}
  }

  func TestGenerationTargetsOnlyGenerationEndpoints(t *testing.T) {
  	t.Parallel()
  	var metadataCalls, generationCalls atomic.Int32
  	metadata := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
  		metadataCalls.Add(1)
  		_, _ = writer.Write([]byte(`{}`))
  	}))
  	defer metadata.Close()
  	generation := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
  		generationCalls.Add(1)
  		http.Error(writer, `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED"}}`, http.StatusTooManyRequests)
  	}))
  	defer generation.Close()

  	client := New(Options{AccessToken: "token", HTTPClient: generation.Client()})
  	client.contentEndpoints = []string{metadata.URL}
  	client.generationEndpoints = []string{generation.URL}

  	_, streamErr := client.StreamGenerateContent(context.Background(), map[string]string{"x": "y"}, RequestOptions{}, func(SSEEvent) error { return nil })
  	_, jsonErr := client.GenerateContent(context.Background(), map[string]string{"x": "y"}, RequestOptions{})
  	for name, err := range map[string]error{"stream": streamErr, "json": jsonErr} {
  		if got := FindHTTPError(err); got == nil || got.StatusCode != http.StatusTooManyRequests {
  			t.Errorf("%s error = %v, want upstream 429", name, err)
  		}
  	}
  	if metadataCalls.Load() != 0 || generationCalls.Load() != 2 {
  		t.Fatalf("metadata=%d generation=%d, want 0 and 2: a generation 429 must not reach another host",
  			metadataCalls.Load(), generationCalls.Load())
  	}
  }
  ```

  In `TestDoSSEDecompressesGzipStream` (L156) and
  `TestDoSSEClosesBodyOnGunzipError` (L178), change
  `client.contentEndpoints = []string{server.URL}` to
  `client.generationEndpoints = []string{server.URL}`. Without this change,
  once Step 3 lands those tests would dial the real daily host.

- **Step 2: Run tests to confirm failure**

  ```bash
  go test ./internal/cloudcode -run 'TestEndpointDefaults|TestGenerationTargetsOnlyGenerationEndpoints|TestDoSSE' -v
  ```

  Expected: a build failure, `client.generationEndpoints undefined` and
  `undefined: GenerationEndpoints`.

- **Step 3: Minimal implementation** (`internal/cloudcode/client.go`)

  ```go
  var (
  	// GenerationEndpoints carries generateContent/streamGenerateContent. It
  	// holds exactly one host: agy 1.2.10 generates on daily (SNI parity), and
  	// a thought signature issued by one host is rejected by the other
  	// ("Corrupted thought signature"), so a cross-host fallback turns a
  	// retryable 429 into a permanent 400.
  	GenerationEndpoints   = []string{DailyEndpoint}
  	ContentEndpoints      = []string{ProdEndpoint, DailyEndpoint}
  	ProvisioningEndpoints = []string{ProdEndpoint, DailyEndpoint}
  )
  ```

  - Add the struct field `generationEndpoints []string` next to
    `contentEndpoints` (L56).
  - In `New`, add `generationEndpoints: append([]string(nil), GenerationEndpoints...),`.
  - In `GenerateContent` (L263) and `StreamGenerateContent` (L267), replace
    `c.contentEndpoints` with `c.generationEndpoints`.
  - Run `gofmt -w internal/cloudcode/client.go internal/cloudcode/client_test.go`.

- **Step 4: Run tests to confirm pass**

  ```bash
  go test ./internal/cloudcode -v -run 'TestEndpointDefaults|TestGenerationTargetsOnlyGenerationEndpoints|TestDoSSE|TestFetchAvailableModelsHeadersAndDailyFallback'
  go test -race ./internal/cloudcode
  ```

- **Step 5: Commit**

  ```bash
  git add internal/cloudcode/client.go internal/cloudcode/client_test.go
  git commit -m "fix(cloudcode): pin generation to agy's daily host, restore metadata fallback"
  ```

---

### Task 2: Drop the `accounts.findHTTPError` forwarding wrapper

- **Target files**:
  - Modify: `internal/accounts/dispatcher.go` (L455, L734, delete L974-L976)
  - Test: `internal/accounts/dispatcher_test.go` (delete `"fmt"` import L8 and
    `TestFindHTTPErrorPrioritizes429` L640-L651)
- **Consumes / Produces**: consumes `cloudcode.FindHTTPError`. No new API.
  Only L455 and L734 call the wrapper (`graft callers findHTTPError`).
- **Step 1: Test change**: no new test. This is a refactor that keeps behavior.
  The deleted test only exercised the one-line forward. Task 3 moves its case
  (`fmt.Errorf("max retries exceeded: %w", errors.Join(err400, err429))`) into
  `TestFindHTTPError`. `fmt` has no other use in `dispatcher_test.go`.
- **Step 2: Baseline**: `go test ./internal/accounts` (green).
- **Step 3: Minimal implementation**:
  - L455: `upstreamError := cloudcode.FindHTTPError(requestErr)`
  - L734: `upstreamError := cloudcode.FindHTTPError(err)`
  - Delete `func findHTTPError` (L974-L976). `errors` stays imported
    (still used, e.g. L349).
- **Step 4: Run tests to confirm pass**:
  `go vet ./internal/accounts && go test ./internal/accounts`
- **Step 5: Commit**:

  ```bash
  git add internal/accounts/dispatcher.go internal/accounts/dispatcher_test.go
  git commit -m "refactor(accounts): call cloudcode.FindHTTPError directly"
  ```

---

### Task 3: Table-driven, nil-safe `TestFindHTTPError`; document precedence

- **Target files**:
  - Modify: `internal/cloudcode/client.go` (doc comment L121-L123)
  - Test: `internal/cloudcode/client_test.go` (replace `TestFindHTTPError`,
    L266-L332)
- **Consumes / Produces**: `cloudcode.FindHTTPError(err error) *HTTPError`,
  behavior unchanged.
- **Step 1: Rewrite the test**. The old version formats `got.StatusCode` in its
  failure branches, so a nil result panics instead of reporting the mismatch.

  ```go
  func TestFindHTTPError(t *testing.T) {
  	t.Parallel()
  	err400 := &HTTPError{StatusCode: http.StatusBadRequest, Status: "400", Body: "Corrupted thought signature"}
  	err401 := &HTTPError{StatusCode: http.StatusUnauthorized, Status: "401", Body: "Unauthorized"}
  	err403 := &HTTPError{StatusCode: http.StatusForbidden, Status: "403", Body: "Forbidden"}
  	err429 := &HTTPError{StatusCode: http.StatusTooManyRequests, Status: "429", Body: "RESOURCE_EXHAUSTED"}
  	err500 := &HTTPError{StatusCode: http.StatusInternalServerError, Status: "500", Body: "Internal Error"}
  	var typedNil *HTTPError

  	cases := []struct {
  		name string
  		err  error
  		want *HTTPError
  	}{
  		{"nil", nil, nil},
  		{"unrelated", errors.New("other"), nil},
  		{"typed nil", error(typedNil), nil},
  		{"single", err429, err429},
  		{"wrapped", fmt.Errorf("outer: %w", err429), err429},
  		{"429 beats earlier 400", errors.Join(err400, err429), err429},
  		{"429 beats earlier 500", errors.Join(err500, err429), err429},
  		{"401 beats earlier 500", errors.Join(err500, err401), err401},
  		{"403 beats earlier 500", errors.Join(err500, err403), err403},
  		{"500 beats earlier 400", errors.Join(err400, err500), err500},
  		{"typed nil skipped in join", errors.Join(error(typedNil), err429), err429},
  		{"429 inside wrapped join", fmt.Errorf("max retries exceeded: %w", errors.Join(err400, err429)), err429},
  	}
  	for _, tc := range cases {
  		if got := FindHTTPError(tc.err); got != tc.want {
  			t.Errorf("%s: FindHTTPError = %v, want %v", tc.name, got, tc.want)
  		}
  	}
  }
  ```

- **Step 2: Prove the table catches a regression**: temporarily swap the 401/403
  and 5xx loops in `FindHTTPError`, then run
  `go test ./internal/cloudcode -run TestFindHTTPError -v`. Expected: the
  `401 beats earlier 500` and `403 beats earlier 500` rows fail and are
  reported without panicking. Revert the swap.
- **Step 3: Minimal implementation**: replace the doc comment (L121-L123):

  ```go
  // FindHTTPError returns the most actionable *HTTPError in err's tree,
  // walking both Unwrap() error and Unwrap() []error (errors.Join).
  // Precedence: 429 (rotate with backoff) > 401/403 (invalidate the
  // account) > 5xx > first found. Typed-nil *HTTPError values are skipped.
  ```

- **Step 4: Run tests to confirm pass**:
  `go test ./internal/cloudcode -run TestFindHTTPError -v`
- **Step 5: Commit**:

  ```bash
  git add internal/cloudcode/client.go internal/cloudcode/client_test.go
  git commit -m "test(cloudcode): table-drive FindHTTPError, document precedence"
  ```

---

### Task 4: Packet gate, docs, PR body

- **Target files**:
  - Modify: `AGENTS.md` ("Key facts" → `Target host` line)
  - Create: `.reference/fingerprint-recheck-20260924.txt` (same convention as
    `fingerprint-recheck-20260715.txt`)
  - PR #97 body (`gh pr edit`)
- **Step 1: Full suite gate** (must be fully green):

  ```bash
  go build -o bin/proxy ./cmd/proxy
  go vet ./...
  gofmt -l .            # must print nothing
  go test ./...
  ```

- **Step 2: Packet gate** (needs sudo; uses the throwaway
  `/tmp/pr97-endpoint-capture.sh`):

  ```bash
  go build -o /tmp/pr97proxy ./cmd/proxy
  sudo bash /tmp/pr97-endpoint-capture.sh
  ```

  Expected in the ClientHello table:
  - The `agy` rows and the `pr97proxy` generation rows are both
    `daily-cloudcode-pa.googleapis.com`, with no ALPN and JA4
    `t13d131100_f57a46bbacb6_f50d94e863eb`.
  - `pr97proxy` rows for `cloudcode-pa.googleapis.com` are allowed only for
    metadata RPCs (prod-first since `3510aad`, not in scope).

  Save the table and the agy URL counts to
  `.reference/fingerprint-recheck-20260924.txt`.

- **Step 3: AGENTS.md**: replace
  `Target host: cloudcode-pa.googleapis.com:443 (daily fallback: daily-cloudcode-pa.googleapis.com)`
  with: generation goes only to `daily-cloudcode-pa.googleapis.com:443` (agy
  1.2.10 parity; thought signatures are tied to the issuing host, so there is no
  cross-host fallback); metadata and provisioning go to
  `cloudcode-pa.googleapis.com` and then daily.

- **Step 4: PR body**: rewrite root cause #1's fix to say that generation is
  pinned to `DailyEndpoint` (agy's host, per the live-run log) with no
  fallback, and that the metadata RPCs are unchanged from main. Add the cutover
  note (one 400 for each tool loop running during the deploy; recovered by
  re-prompting) and the packet-gate result.

  ```bash
  gh pr edit 97 --body-file /tmp/pr97-body.md
  ```

- **Step 5: Commit and push**:

  ```bash
  git add AGENTS.md .reference/fingerprint-recheck-20260924.txt
  git commit -m "docs: record agy 1.2.10 daily generation host and packet recheck"
  git push fork fix/upstream-429-thought-signature
  ```

  Afterwards, delete the throwaway files: `/tmp/pr97proxy`,
  `/tmp/pr97-endpoint-capture.sh`, `/tmp/pr97-endpoint-capture/`,
  `/tmp/agy-endpoint-run.log`.
