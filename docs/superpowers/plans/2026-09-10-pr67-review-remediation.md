# PR #67 Review Remediation Plan

## Goal
Remediate findings from PR #67 review:
1. Recognize Anthropic domain hosts in `isAnthropicEndpoint` to prevent routing `/v1/chat/completions` directly to Anthropic base URLs without translation.
2. Return OpenAI-formatted error payloads from `forwardToCustomEndpoint` when routing failures occur on `/v1/chat/completions`.
3. Prevent leakage of proxy auth headers upstream when `endpoint.APIKey` is empty.
4. Fail with `writeOpenAIError` if `json.Marshal` fails when updating mapped models in `chatCompletions`.
5. Merge target and client query strings in reverse proxy director instead of unconditionally overwriting.

## Architecture & Tech Stack
- Go 1.24 HTTP handler and reverse proxy pipeline in `internal/api/server.go` and `internal/api/openai_proxy.go`.
- Unit tests using standard `net/http/httptest` in `internal/api/server_test.go` and `internal/api/openai_proxy_test.go`.

---

## Task 1: Recognize Anthropic hostnames in `isAnthropicEndpoint`

### Target Files
- Modify: `internal/api/server.go`
- Test: `internal/api/server_test.go`

### Step 1: Write failing test
Add test `TestTransparentForwarding_ChatCompletionsToAnthropicBaseURL` asserting that setting `https://api.anthropic.com` or mock server mimicking `api.anthropic.com` triggers translation to `/v1/messages` and does not forward raw OpenAI payload.

### Step 2: Run test to confirm failure
```bash
go test -run TestTransparentForwarding_ChatCompletionsToAnthropicBaseURL ./internal/api/...
```

### Step 3: Minimal implementation
In `internal/api/server.go`:
Extend `isAnthropicEndpoint(endpointURL string)`:
```go
func isAnthropicEndpoint(endpointURL string) bool {
	parsed, err := url.Parse(endpointURL)
	if err != nil {
		return false
	}
	clean := strings.TrimRight(parsed.Path, "/")
	if clean == "/messages" || strings.HasSuffix(clean, "/messages") {
		return true
	}
	host := strings.ToLower(parsed.Hostname())
	return host == "api.anthropic.com" || host == "anthropic.com" || strings.HasSuffix(host, ".anthropic.com")
}
```

### Step 4: Run test to confirm pass
```bash
go test -run TestTransparentForwarding_ChatCompletionsToAnthropicBaseURL ./internal/api/...
```

### Step 5: Git commit command
```bash
git add internal/api/server.go internal/api/server_test.go
git commit -m "fix(api): identify anthropic hostnames in isAnthropicEndpoint"
```

---

## Task 2: Return OpenAI error format in `forwardToCustomEndpoint` for `/v1/chat/completions`

### Target Files
- Modify: `internal/api/server.go`
- Test: `internal/api/server_test.go`

### Step 1: Write failing test
Add test `TestTransparentForwarding_ChatCompletionsErrorFormat` checking that an invalid URL or proxy connection failure returns `{"error":{"message":...,"type":...}}` rather than `{"type":"error",...}`.

### Step 2: Run test to confirm failure
```bash
go test -run TestTransparentForwarding_ChatCompletionsErrorFormat ./internal/api/...
```

### Step 3: Minimal implementation
In `internal/api/server.go`:
Determine if request is messages request (`isMessagesRequest := strings.HasSuffix(request.URL.Path, "/messages")`).
If resolution or proxy error occurs and `!isMessagesRequest`:
Call `writeOpenAIError(writer, status, errType, msg)`.

### Step 4: Run test to confirm pass
```bash
go test -run TestTransparentForwarding_ChatCompletionsErrorFormat ./internal/api/...
```

### Step 5: Git commit command
```bash
git add internal/api/server.go internal/api/server_test.go
git commit -m "fix(api): write openai error envelopes for chat completions custom endpoint failures"
```

---

## Task 3: Prevent proxy auth credentials leak when endpoint APIKey is empty

### Target Files
- Modify: `internal/api/server.go`
- Test: `internal/api/server_test.go`

### Step 1: Write failing test
Add test `TestTransparentForwarding_StripsProxyAuthWhenNoAPIKey` where proxy is configured with `APIKey: "local-proxy-secret"`, client sends `Authorization: Bearer local-proxy-secret` and `x-api-key: local-proxy-secret`, and custom endpoint has empty `APIKey`. Verify mock target does not receive proxy secret.

### Step 2: Run test to confirm failure
```bash
go test -run TestTransparentForwarding_StripsProxyAuthWhenNoAPIKey ./internal/api/...
```

### Step 3: Minimal implementation
In `internal/api/server.go`:
In Director:
```go
if endpoint.APIKey != "" {
	req.Header.Set("Authorization", "Bearer "+endpoint.APIKey)
	req.Header.Set("x-api-key", endpoint.APIKey)
} else {
	cfg := config.Get()
	if cfg.APIKey != "" {
		if req.Header.Get("Authorization") == "Bearer "+cfg.APIKey {
			req.Header.Del("Authorization")
		}
		if req.Header.Get("x-api-key") == cfg.APIKey {
			req.Header.Del("x-api-key")
		}
	}
}
```

### Step 4: Run test to confirm pass
```bash
go test -run TestTransparentForwarding_StripsProxyAuthWhenNoAPIKey ./internal/api/...
```

### Step 5: Git commit command
```bash
git add internal/api/server.go internal/api/server_test.go
git commit -m "fix(api): strip proxy credentials when custom endpoint has empty apiKey"
```

---

## Task 4: Fail on `json.Marshal` failure when updating mapped model & query string preservation

### Target Files
- Modify: `internal/api/openai_proxy.go`, `internal/api/server.go`
- Test: `internal/api/server_test.go`

### Step 1: Write failing test
Add test `TestTransparentForwarding_QueryStringPreserved` verifying query params on client request and target URL are preserved/merged.

### Step 2: Run test to confirm failure
```bash
go test -run TestTransparentForwarding_QueryStringPreserved ./internal/api/...
```

### Step 3: Minimal implementation
In `internal/api/openai_proxy.go`:
```go
if model != requestModel {
	openaiRequest["model"] = model
	updated, err := json.Marshal(openaiRequest)
	if err != nil {
		writeOpenAIError(writer, http.StatusBadRequest, "invalid_request_error", "Failed to marshal request with mapped model: "+err.Error())
		return
	}
	forwardBody = updated
}
```

In `internal/api/server.go`:
Merge query parameters in Director:
```go
targetQuery := targetURL.RawQuery
if targetQuery == "" || req.URL.RawQuery == "" {
	req.URL.RawQuery = targetQuery + req.URL.RawQuery
} else {
	req.URL.RawQuery = targetQuery + "&" + req.URL.RawQuery
}
```

### Step 4: Run test to confirm pass
```bash
go test -run TestTransparentForwarding_QueryStringPreserved ./internal/api/...
```

### Step 5: Git commit command
```bash
git add internal/api/openai_proxy.go internal/api/server.go internal/api/server_test.go
git commit -m "fix(api): preserve query parameters and guard model marshal error"
```
