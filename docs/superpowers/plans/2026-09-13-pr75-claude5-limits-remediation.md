# PR #75 Remediation Plan — Claude 5 / Fable 1M Context + 128K Max Tokens

**Goal:** Resolve the 4 review findings posted on PR #75 (`feat/claude-5-context-max-tokens`).
**Architecture:** Go proxy; Claude Code model metadata lives in three places that must agree: `internal/claudecode/router.go` (DefaultAllowlist), `internal/claudecode/client.go` (DefaultClaudeCatalogue), `internal/webui/public/js/components/models.js` (importCCDefaults). Routing to upstream Antigravity models via `internal/modelcatalog/catalog.go` routingAliases.
**Tech Stack:** Go, standard `testing` package. Run tests with `go test ./internal/... -run <Name> -v`.
**Spec reference:** PR review comment https://github.com/gustavokch/antigravity-claude-proxy-go/pull/75#issuecomment-5656610805

---

## Task 1: Verify upstream capability before advertising 1M context (risk)

**Files:**
- Modify: `internal/modelcatalog/catalog.go` (routingAliases targets) or `internal/api/server.go` (discovery context_window source)
- Test: `internal/api/models_discovery_test.go`

**Consumes:** `claudecode.ModelConfig.ContextLen`, routingAliases display-name targets.
**Produces:** advertised `context_window` never exceeds the capability of the model the request is actually routed to.

**Step 1 — failing test:** add a consistency test asserting that every Claude Code allowlist entry whose ID resolves through `routingAliases` to an Antigravity display name advertises a `context_window` no larger than that upstream model's known capability. Until upstream capability is confirmed, this test encodes the safe expectation.

```go
func TestDiscoveryContextWindowDoesNotExceedRoutedCapability(t *testing.T) {
    // For each DefaultAllowlist entry, resolve its routing alias target and
    // assert ContextLen <= upstream model's known context. Fails today for
    // the four 1M entries if upstream Sonnet/Opus 4.6 (Thinking) is 200k.
}
```

**Step 2:** `go test ./internal/api/ -run TestDiscoveryContextWindowDoesNotExceedRoutedCapability -v` — confirm failure.
**Step 3 — implementation:** either (a) confirm the Antigravity `Claude Sonnet 4.6 (Thinking)` / `Claude Opus 4.6 (Thinking)` endpoints accept 1M-token requests and document that in the PR, making the test pass as-written; or (b) clamp advertised `context_window` to the routed model's capability in the discovery handler.
**Step 4:** re-run test — pass.
**Step 5:** `git commit -m "fix(discovery): align advertised context_window with routed upstream capability"`

> Decision needed from maintainer before implementation: (a) document-verified 1M upstream, or (b) clamp. Default to (b) if no evidence.

---

## Task 2: Align fable-5-1 alias sets across the three sources (nit)

**Files:**
- Modify: `internal/claudecode/router.go` (DefaultAllowlist entry), `internal/webui/public/js/components/models.js` (importCCDefaults)
- Test: `internal/claudecode/router_test.go`

**Consumes:** `DefaultAllowlist()`, `DefaultClaudeCatalogue()`.
**Produces:** identical alias sets for `claude-fable-5-1` in router defaults, catalogue, and WebUI defaults: `claude-fable-5-1, fable-5-1, claude-fable-5.1, fable-5.1`.

**Step 1 — failing test:**

```go
func TestDefaultAllowlist_Fable51AliasesMatchCatalogue(t *testing.T) {
    var routerEntry, catalogueEntry *ModelConfig // plus DiscoveredModel from DefaultClaudeCatalogue
    // collect both, normalize dots to hyphens, compare as sets — must be equal
}
```

**Step 2:** `go test ./internal/claudecode/ -run TestDefaultAllowlist_Fable51AliasesMatchCatalogue -v` — confirm failure (router has 2 aliases, catalogue has 4).
**Step 3 — implementation:** add `claude-fable-5.1` and `fable-5.1` to the router.go `Aliases` slice for `claude-fable-5-1`; add `fable-5.1` to the models.js alias string.
**Step 4:** re-run test — pass.
**Step 5:** `git commit -m "fix(claudecode): align fable-5-1 alias sets across router, catalogue, and WebUI defaults"`

---

## Task 3: Add clamp-down case to 128K forwarding test (nit)

**Files:**
- Modify: `internal/api/claudecode_proxy_test.go`

**Consumes:** `applyMaxTokensPolicy`, `claudeCodeEntryMaxOutput`.
**Produces:** regression coverage for the clamp-down branch with the new 128000 limit.

**Step 1 — failing test:**

```go
func TestClaudeCodeForwarding_ClampsAbove128K(t *testing.T) {
    limit := claudeCodeEntryMaxOutput(claudecode.Config{}, "claude-opus-5")
    body := []byte(`{"model":"claude-opus-5","max_tokens":200000,"messages":[{"role":"user","content":"hi"}]}`)
    var req map[string]any
    if err := json.Unmarshal(body, &req); err != nil { t.Fatal(err) }
    out := applyMaxTokensPolicy(body, req, 0, limit)
    var parsed map[string]any
    if err := json.Unmarshal(out, &parsed); err != nil { t.Fatal(err) }
    if mt, _ := parsed["max_tokens"].(float64); int(mt) != 128000 {
        t.Errorf("expected clamp to 128000, got %v", parsed["max_tokens"])
    }
}
```

**Step 2:** `go test ./internal/api/ -run TestClaudeCodeForwarding_ClampsAbove128K -v` — should pass immediately (policy already clamps); if it fails, the policy is broken and that is a 🔴 finding upgrade.
**Step 3:** no production change expected; test is pure coverage.
**Step 4:** re-run — pass.
**Step 5:** `git commit -m "test(api): cover max_tokens clamp-down at 128K Claude Code limit"`

---

## Verification gate

```bash
go test ./internal/... 
```

Must be 100% green, then `git push origin feat/claude-5-context-max-tokens`.

**Execution status: COMPLETE (2026-09-13).**
- Task 1: maintainer chose alias removal over clamp — `routingAliases` no longer maps the four 1M models to 4.6 upstreams; honest SelectionError instead. Test: `TestClaude5ModelsDoNotRouteTo46Upstreams`. Commit `499b325`.
- Task 2: router + WebUI alias sets aligned with catalogue (verbatim spelling comparison; dot-normalized comparison would have been vacuous). Commit `eed2a5a`.
- Task 3: clamp-down test passed immediately (pure coverage). Commit `d1235ac`.
- Gate: `go test ./internal/...` 100% green; pushed to `fork feat/claude-5-context-max-tokens`.
