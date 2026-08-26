# PR #26 Review Remediation Plan

- **Goal:** Remediate review finding on PR #26 by adding explicit test coverage pinning default spoof constants (`DefaultSpoofAppReferer`, `DefaultSpoofAppTitle`, `DefaultSpoofAppCategories`) to guard against silent regression.
- **Architecture:** OpenRouter Gateway harness attribution headers (`internal/openrouter/harness.go`, `internal/openrouter/harness_test.go`).
- **Tech Stack:** Go (Standard Library `testing`, `net/http`).
- **Spec Reference:** PR #26 diff, OpenRouter App Attribution spec.

---

## Task 1: Pin Default Spoof Attribution Constants in Test Suite

- **Target Files:**
  - Modify: `internal/openrouter/harness_test.go`
- **Consumes / Produces Interfaces:**
  - Consumes: `openrouter.DefaultSpoofAppReferer`, `openrouter.DefaultSpoofAppTitle`, `openrouter.DefaultSpoofAppCategories`
  - Produces: Explicit value assertions in `TestDefaultSpoofConstants`

### Step 1: Write failing test
Add test `TestDefaultSpoofConstants` in `internal/openrouter/harness_test.go`:
```go
func TestDefaultSpoofConstants(t *testing.T) {
	if DefaultSpoofAppReferer != "https://claude.ai/code" {
		t.Errorf("DefaultSpoofAppReferer = %q, want %q", DefaultSpoofAppReferer, "https://claude.ai/code")
	}
	if DefaultSpoofAppTitle != "Claude Code" {
		t.Errorf("DefaultSpoofAppTitle = %q, want %q", DefaultSpoofAppTitle, "Claude Code")
	}
	if DefaultSpoofAppCategories != "cli-agent" {
		t.Errorf("DefaultSpoofAppCategories = %q, want %q", DefaultSpoofAppCategories, "cli-agent")
	}
}
```

### Step 2: Run test to confirm failure / pass
```bash
go test -v -run TestDefaultSpoofConstants ./internal/openrouter
```

### Step 3: Minimal implementation
Update `internal/openrouter/harness_test.go` with the constant assertions.

### Step 4: Run test to confirm pass
```bash
go test -v -run TestDefaultSpoofConstants ./internal/openrouter
go test ./...
```

### Step 5: Git commit command
```bash
git add internal/openrouter/harness_test.go docs/superpowers/plans/2026-08-26-pr26-review-remediation.md
git commit -m "test(openrouter): assert default spoof attribution constants"
```
