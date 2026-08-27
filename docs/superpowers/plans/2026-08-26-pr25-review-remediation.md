# Remediation Plan: PR #25 Review Feedback

**Goal:** Address code review findings for PR #25 (OpenRouter harness-gate referer attribution, detection robustness, test coverage, and documentation).
**Architecture:** Go standard library HTTP proxy with OpenRouter upstream handler, Alpine.js WebUI, and translation validator.
**Tech Stack:** Go 1.24+, HTML/JS (Alpine.js/Tailwind).

---

## Task 1: Defensive Whitespace Trimming for Spoof Attribution Headers

### Target Files
- Modify: `internal/openrouter/harness.go`
- Modify: `internal/api/server.go`
- Test: `internal/openrouter/harness_test.go`

### Interfaces
- `ApplySpoofHeaders(req *http.Request, title, categories, referer string)`: Trims inputs and falls back to default constants when empty/whitespace.

### Step 1: Write Failing Tests
Add test cases in `internal/openrouter/harness_test.go` for whitespace-only inputs:
```go
t.Run("whitespace-only values fall back to defaults", func(t *testing.T) {
    req, _ := http.NewRequest(http.MethodPost, "https://openrouter.ai/api/v1/chat/completions", nil)
    ApplySpoofHeaders(req, "  ", "\t\n", "   ")

    if got := req.Header.Get(SpoofAppRefererHeader); got != DefaultSpoofAppReferer {
        t.Errorf("HTTP-Referer = %q, want %q", got, DefaultSpoofAppReferer)
    }
    if got := req.Header.Get(SpoofAppTitleHeader); got != DefaultSpoofAppTitle {
        t.Errorf("X-OpenRouter-Title = %q, want %q", got, DefaultSpoofAppTitle)
    }
    if got := req.Header.Get(SpoofAppCategoriesHeader); got != DefaultSpoofAppCategories {
        t.Errorf("X-OpenRouter-Categories = %q, want %q", got, DefaultSpoofAppCategories)
    }
})
```

### Step 2: Run Tests to Confirm Failure
```bash
go test -v ./internal/openrouter -run TestApplySpoofHeaders
```

### Step 3: Minimal Implementation
Update `ApplySpoofHeaders` in `internal/openrouter/harness.go`:
```go
func ApplySpoofHeaders(req *http.Request, title, categories, referer string) {
    if req == nil {
        return
    }
    title = strings.TrimSpace(title)
    if title == "" {
        title = DefaultSpoofAppTitle
    }
    categories = strings.TrimSpace(categories)
    if categories == "" {
        categories = DefaultSpoofAppCategories
    }
    referer = strings.TrimSpace(referer)
    if referer == "" {
        referer = DefaultSpoofAppReferer
    }
    req.Header.Set(SpoofAppRefererHeader, referer)
    req.Header.Set(SpoofAppRefererLegacyHeader, referer)
    req.Header.Set(SpoofAppTitleHeader, title)
    req.Header.Set(SpoofAppTitleLegacyHeader, title)
    req.Header.Set(SpoofAppCategoriesHeader, categories)
}
```
Update `server.go` OpenRouter config extraction to trim strings defensively.

### Step 4: Run Tests to Confirm Pass
```bash
go test -v ./internal/openrouter -run TestApplySpoofHeaders
go test -v ./internal/api -run TestOpenRouterForwarding_HarnessGate
```

### Step 5: Git Commit Command
```bash
git add internal/openrouter/harness.go internal/openrouter/harness_test.go internal/api/server.go
git commit -m "fix(openrouter): trim whitespace before checking spoof header defaults"
```

---

## Task 2: Robust Detection for Harness Gate Errors

### Target Files
- Modify: `internal/openrouter/harness.go`
- Test: `internal/openrouter/harness_test.go`

### Interfaces
- `IsHarnessGateError(body []byte) bool`: Returns true if body contains "agentic harness" or "agentic harnesses".

### Step 1: Write Failing Tests
Add test cases in `internal/openrouter/harness_test.go`:
```go
{
    name: "phrasing without url",
    body: `{"error":{"message":"This model requires an agentic harness."}}`,
    want: true,
},
{
    name: "only available to agentic harnesses",
    body: `{"error":{"message":"Only available to agentic harnesses"}}`,
    want: true,
},
```

### Step 2: Run Tests to Confirm Failure
```bash
go test -v ./internal/openrouter -run TestIsHarnessGateError
```

### Step 3: Minimal Implementation
Update `IsHarnessGateError` in `internal/openrouter/harness.go`:
```go
func IsHarnessGateError(body []byte) bool {
    lower := strings.ToLower(string(body))
    return strings.Contains(lower, "agentic harness")
}
```

### Step 4: Run Tests to Confirm Pass
```bash
go test -v ./internal/openrouter -run TestIsHarnessGateError
```

### Step 5: Git Commit Command
```bash
git add internal/openrouter/harness.go internal/openrouter/harness_test.go
git commit -m "fix(openrouter): generalize harness gate error matching for varying phrasing"
```

---

## Task 3: Translation Test Coverage for `appSpoofFieldReferer`

### Target Files
- Modify: `internal/webui/translations_test.go`

### Step 1: Update Test
Add `"appSpoofFieldReferer"` to `appSpoofKeys` in `TestTranslations_AppSpoofTemplateReferences`.

### Step 2: Run Tests to Confirm Pass
```bash
go test -v ./internal/webui -run TestTranslations_AppSpoofTemplateReferences
```

### Step 3: Git Commit Command
```bash
git add internal/webui/translations_test.go
git commit -m "test(webui): verify appSpoofFieldReferer template references"
```

---

## Task 4: Documentation Update for Referer Spoofing

### Target Files
- Modify: `README.md`

### Step 1: Minimal Implementation
Update `README.md` config JSON example and feature item 5 to include `referer: "https://claude.ai"` and document `HTTP-Referer` / `Referer`.

### Step 2: Verify Syntax / Formatting
Check Markdown formatting.

### Step 3: Git Commit Command
```bash
git add README.md
git commit -m "docs: document referer in OpenRouter appSpoof config"
```
