# PR #103 Review Remediation

**Goal:** Resolve the two verified nits from the PR #103 review.
**Architecture:** Local invariant fix in `RequestMetrics.usage()`; test-list coverage fix.
**Tech Stack:** Go, `go test`.
**Spec:** https://github.com/gustavokch/antigravity-claude-proxy-go/pull/103#issuecomment-5861576326

## Task 1: `usage()` keeps cache split within total
- Modify: `internal/claudecode/observability.go`
- Test: `internal/claudecode/observability_test.go`
1. Test: `RequestMetrics{CacheCreationTokens: 10, CacheCreation1hTokens: 15}.usage()` gives `CacheCreate=15, CacheCreate5m=0, CacheCreate1h=15`.
2. `go test ./internal/claudecode -run TestRequestMetrics_UsageSplit` fails.
3. Raise `CacheCreate` to `cw1h` when `cw1h` is larger (same rule as `usageAccumulator.result()`).
4. Test passes.
5. Commit `fix(claudecode): keep request metrics cache split within total`.

## Task 2: `resetsIn` in locale parity list
- Modify: `internal/webui/translations_test.go`
1. Add `"resetsIn"` to `quotaUsageKeys`.
2. `go test ./internal/webui -run Translation` passes (key exists in en and pt).
3. Commit `test(webui): cover resetsIn in quota locale parity`.
