# PR #56 Remediation Plan: Cloud Code SSE Body Close, JA4 Interface Default, Reference Hygiene

**Goal:** Remediate review findings on PR #56 (feat/agy-mitm-header-capture): prevent response body leak in DoSSE when decompression fails, restore standard \`en0\` Darwin capture interface in \`verify-ja4.sh\`, sanitize local username paths, and format reference JSON documents.

**Architecture:** Go client networking (\`internal/cloudcode\`), test suite, shell scripts, and reference documentation.

**Tech Stack:** Go 1.24+, \`net/http\`, \`compress/gzip\`, Bash, \`jq\`.

---

## Task 1: Fix Response Body Leak in Cloud Code DoSSE Error Path

**Target Files:**
- Modify: \`internal/cloudcode/client.go\`
- Test: \`internal/cloudcode/client_test.go\`

**Consumes / Produces:**
- Consumes: \`http.Response\` with invalid gzip header
- Produces: Closed response body without socket leak

### Step 1: Write Failing Test
In \`internal/cloudcode/client_test.go\`, add \`TestDoSSEClosesBodyOnGunzipError\`:
```go
func TestDoSSEClosesBodyOnGunzipError(t *testing.T) {
	t.Parallel()
	bodyClosed := false
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Encoding", "gzip")
		// Invalid gzip payload to trigger maybeGunzip failure
		_, _ = writer.Write([]byte("not-gzip-data"))
	}))
	defer server.Close()

	client := New(Options{AccessToken: "token", HTTPClient: server.Client()})
	client.contentEndpoints = []string{server.URL}
	_, err := client.StreamGenerateContent(context.Background(), map[string]string{"x": "y"}, RequestOptions{}, func(event SSEEvent) error {
		return nil
	})
	if err == nil {
		t.Fatal("expected error on corrupt gzip stream, got nil")
	}
}
```

### Step 2: Run Test to Confirm Status / Add Tracking
Run: \`go test -v ./internal/cloudcode -run TestDoSSEClosesBodyOnGunzipError\`

### Step 3: Minimal Implementation
In \`internal/cloudcode/client.go\` in \`DoSSE\`:
```go
		streamReader, streamErr := maybeGunzip(response)
		if streamErr != nil {
			_ = response.Body.Close()
			failures = append(failures, fmt.Errorf("open Cloud Code stream from %s: %w", endpoint, streamErr))
			continue
		}
```

### Step 4: Run Test to Confirm Pass
Run: \`go test -v ./internal/cloudcode -run TestDoSSE\`

### Step 5: Git Commit
\`git commit -m "fix(cloudcode): close response body when DoSSE decompression fails"\`

---

## Task 2: Restore Darwin Default Capture Interface in verify-ja4.sh

**Target Files:**
- Modify: \`scripts/verify-ja4.sh\`

**Consumes / Produces:**
- Consumes: \`ANTIGRAVITY_CAPTURE_IFACE\` env var with default \`en0\`
- Produces: Standard Wi-Fi interface capture on macOS

### Step 1: Minimal Implementation
In \`scripts/verify-ja4.sh\`:
```bash
if [[ "$(uname -s)" == "Darwin" ]]; then
  CAP_IFACE="${ANTIGRAVITY_CAPTURE_IFACE:-en0}"
else
  CAP_IFACE="any"
fi
```

### Step 2: Verification
Run: \`bash -n scripts/verify-ja4.sh\`

### Step 3: Git Commit
\`git commit -m "fix(scripts): restore default en0 interface in verify-ja4.sh"\`

---

## Task 3: Sanitize Reference Documentation & Format JSON

**Target Files:**
- Modify: \`.reference/agy-headers-20260903.txt\`
- Modify: \`.reference/fingerprint-recheck-20260903.txt\`
- Modify: \`.reference/cloudcode-models-20260903.json\`

**Consumes / Produces:**
- Consumes: Raw reference dumps
- Produces: Sanitized paths (\`$HOME/\`) and formatted JSON

### Step 1: Minimal Implementation
- Update \`.reference/agy-headers-20260903.txt\` with SUPERSEDED header and replace \`/Users/gus/\` with \`$HOME/\`.
- Update \`.reference/fingerprint-recheck-20260903.txt\` replacing \`/Users/gus/\` with \`$HOME/\`.
- Format \`.reference/cloudcode-models-20260903.json\` using standard 2-space indentation.

### Step 2: Verification
Run: \`grep -rn "/Users/gus" .reference/ || true\`

### Step 3: Git Commit
\`git commit -m "docs(fingerprint): sanitize paths, format JSON, and annotate superseded headers doc"\`
