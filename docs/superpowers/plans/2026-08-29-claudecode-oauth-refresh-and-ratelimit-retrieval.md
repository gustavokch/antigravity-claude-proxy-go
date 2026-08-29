# Claude Code Automatic OAuth Refresh and Rate-Limit Retrieval Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement robust automatic OAuth token refresh (proactive background worker, just-in-time refresh before expiry, and 401 unauthorized retry recovery) and proactive rate-limit retrieval (granular input/output token headers, active probe endpoint, and background synchronization) for the Claude Code gateway.

**Architecture:** 
- Enhance `internal/claudecode/ratelimit.go` to parse both unified and granular Anthropic rate-limit headers (`input-tokens`, `output-tokens`, `requests`, `resets`).
- Add active rate-limit probing (`FetchRateLimits`) to `internal/claudecode/client.go` to query Anthropic API rate limits without full inference.
- Wire `AccountPool` in `internal/claudecode/pool.go` with a centralized token refresher, proactive expiration checking (`RefreshTokenIfNeeded`), on-demand forced refresh (`RefreshAccountToken`), and bulk background expiration renewal (`RefreshAllExpiringTokens`).
- Enhance `internal/claudecode/discovery.go` to extract OAuth tokens, refresh tokens, and expiration metadata from `~/.claude.json`.
- Integrate automatic token refresh into `internal/api/claudecode_proxy.go` (just-in-time preflight + 401 unauthorized automatic retry) and sync refreshed credentials back to both dynamic storage and `config.json`.
- Expose management API endpoints in `internal/api/claudecode_management.go` for on-demand account rate-limit retrieval (`POST /api/claudecode/accounts/{id}/ratelimits`) and manual token refresh (`POST /api/claudecode/accounts/{id}/refresh`).
- Add a periodic background worker in `internal/api/server.go` to proactively refresh expiring OAuth tokens.

**Tech Stack:** Go 1.24+, standard library (`net/http`, `sync`, `time`, `encoding/json`, `log/slog`), existing `antigravity-go-proxy` packages (`internal/claudecode`, `internal/auth`, `internal/api`, `internal/config`).

**Spec:** N/A (Feature specification described herein).

## Global Constraints

- Never break existing transparent proxy forwarding for Claude Code or OpenRouter.
- Ensure all token access, refresh operations, and rate-limit state mutations are strictly thread-safe with `sync.RWMutex` / `sync.Mutex`.
- Never expose plaintext OAuth access tokens or refresh tokens in API responses; keep tokens masked in snapshots.
- Ensure refreshed tokens are atomically persisted to both `claudecode_accounts.json` and `config.json`.
- Adhere to ASD-STE100 Simplified Technical English guidelines in logs, comments, and commit messages.

---

### Task 1: Granular Rate-Limit Header Models & Extraction

**Files:**
- Modify: `internal/claudecode/ratelimit.go`
- Modify: `internal/claudecode/types.go`
- Test: `internal/claudecode/ratelimit_test.go`

**Interfaces:**
- Produces:
  - `claudecode.RateLimits` with `InputTokensLimit`, `InputTokensRemaining`, `InputTokensReset`, `OutputTokensLimit`, `OutputTokensRemaining`, `OutputTokensReset`, `RequestsLimit`, `RequestsRemaining`, `RequestsReset`, `TokensLimit`, `TokensRemaining`, `TokensReset`, `RetryAfter`, `LastUpdated`
  - `ExtractRateLimits(h http.Header) RateLimits`
  - `(rl RateLimits) IsRateLimited(now time.Time) bool`

- [ ] **Step 1: Write the failing tests for granular rate-limit extraction**

Add test cases in `internal/claudecode/ratelimit_test.go` verifying extraction of `anthropic-ratelimit-input-tokens-*` and `anthropic-ratelimit-output-tokens-*` headers, as well as `IsRateLimited(now)`.

```go
func TestExtractRateLimits_GranularTokens(t *testing.T) {
	h := make(http.Header)
	h.Set("anthropic-ratelimit-requests-limit", "1000")
	h.Set("anthropic-ratelimit-requests-remaining", "990")
	h.Set("anthropic-ratelimit-requests-reset", "2026-08-29T15:04:05Z")
	h.Set("anthropic-ratelimit-input-tokens-limit", "500000")
	h.Set("anthropic-ratelimit-input-tokens-remaining", "450000")
	h.Set("anthropic-ratelimit-input-tokens-reset", "2026-08-29T15:05:00Z")
	h.Set("anthropic-ratelimit-output-tokens-limit", "100000")
	h.Set("anthropic-ratelimit-output-tokens-remaining", "95000")
	h.Set("anthropic-ratelimit-output-tokens-reset", "2026-08-29T15:06:00Z")
	h.Set("anthropic-ratelimit-tokens-limit", "600000")
	h.Set("anthropic-ratelimit-tokens-remaining", "545000")
	h.Set("anthropic-ratelimit-tokens-reset", "2026-08-29T15:06:00Z")

	rl := ExtractRateLimits(h)

	if rl.InputTokensLimit != 500000 || rl.InputTokensRemaining != 450000 {
		t.Errorf("input tokens mismatch: limit=%d, rem=%d", rl.InputTokensLimit, rl.InputTokensRemaining)
	}
	if rl.OutputTokensLimit != 100000 || rl.OutputTokensRemaining != 95000 {
		t.Errorf("output tokens mismatch: limit=%d, rem=%d", rl.OutputTokensLimit, rl.OutputTokensRemaining)
	}
	if rl.TokensLimit != 600000 || rl.TokensRemaining != 545000 {
		t.Errorf("unified tokens mismatch: limit=%d, rem=%d", rl.TokensLimit, rl.TokensRemaining)
	}
	if rl.IsRateLimited(time.Now()) {
		t.Errorf("expected not rate limited")
	}
}

func TestRateLimits_IsRateLimited(t *testing.T) {
	now := time.Now()
	rl := RateLimits{
		RequestsRemaining: 0,
		RequestsReset:     now.Add(30 * time.Second),
	}
	if !rl.IsRateLimited(now) {
		t.Errorf("expected IsRateLimited=true when RequestsRemaining is 0 and Reset in future")
	}

	rl2 := RateLimits{
		InputTokensRemaining: 0,
		InputTokensReset:     now.Add(30 * time.Second),
	}
	if !rl2.IsRateLimited(now) {
		t.Errorf("expected IsRateLimited=true when InputTokensRemaining is 0 and Reset in future")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -v ./internal/claudecode -run "TestExtractRateLimits_GranularTokens|TestRateLimits_IsRateLimited"`
Expected: FAIL with compilation errors (fields missing on `RateLimits`).

- [ ] **Step 3: Update `RateLimits` struct and header constants**

Update `internal/claudecode/types.go` and `internal/claudecode/ratelimit.go` to include granular rate limit constants and fields:

```go
// In internal/claudecode/types.go:
type RateLimits struct {
	RequestsLimit        int64     `json:"requestsLimit"`
	RequestsRemaining    int64     `json:"requestsRemaining"`
	RequestsReset        time.Time `json:"requestsReset"`
	TokensLimit          int64     `json:"tokensLimit"`
	TokensRemaining      int64     `json:"tokensRemaining"`
	TokensReset          time.Time `json:"tokensReset"`
	InputTokensLimit     int64     `json:"inputTokensLimit,omitempty"`
	InputTokensRemaining int64     `json:"inputTokensRemaining,omitempty"`
	InputTokensReset     time.Time `json:"inputTokensReset,omitempty"`
	OutputTokensLimit    int64     `json:"outputTokensLimit,omitempty"`
	OutputTokensRemaining int64    `json:"outputTokensRemaining,omitempty"`
	OutputTokensReset    time.Time `json:"outputTokensReset,omitempty"`
	RetryAfter           int       `json:"retryAfter,omitempty"` // Seconds
	LastUpdated          time.Time `json:"lastUpdated"`
}

// In internal/claudecode/ratelimit.go:
const (
	HeaderRequestsLimit        = "anthropic-ratelimit-requests-limit"
	HeaderRequestsRemaining    = "anthropic-ratelimit-requests-remaining"
	HeaderRequestsReset        = "anthropic-ratelimit-requests-reset"
	HeaderTokensLimit          = "anthropic-ratelimit-tokens-limit"
	HeaderTokensRemaining      = "anthropic-ratelimit-tokens-remaining"
	HeaderTokensReset          = "anthropic-ratelimit-tokens-reset"
	HeaderInputTokensLimit     = "anthropic-ratelimit-input-tokens-limit"
	HeaderInputTokensRemaining = "anthropic-ratelimit-input-tokens-remaining"
	HeaderInputTokensReset     = "anthropic-ratelimit-input-tokens-reset"
	HeaderOutputTokensLimit    = "anthropic-ratelimit-output-tokens-limit"
	HeaderOutputTokensRemaining = "anthropic-ratelimit-output-tokens-remaining"
	HeaderOutputTokensReset    = "anthropic-ratelimit-output-tokens-reset"
	HeaderRetryAfter           = "retry-after"
)

// ExtractRateLimits extracts standard and granular rate-limit headers.
func ExtractRateLimits(h http.Header) RateLimits {
	rl := RateLimits{
		LastUpdated: time.Now(),
	}

	if val := h.Get(HeaderRequestsLimit); val != "" {
		if n, err := strconv.ParseInt(strings.TrimSpace(val), 10, 64); err == nil {
			rl.RequestsLimit = n
		}
	}
	if val := h.Get(HeaderRequestsRemaining); val != "" {
		if n, err := strconv.ParseInt(strings.TrimSpace(val), 10, 64); err == nil {
			rl.RequestsRemaining = n
		}
	}
	if val := h.Get(HeaderRequestsReset); val != "" {
		rl.RequestsReset = parseTimestamp(val)
	}

	if val := h.Get(HeaderTokensLimit); val != "" {
		if n, err := strconv.ParseInt(strings.TrimSpace(val), 10, 64); err == nil {
			rl.TokensLimit = n
		}
	}
	if val := h.Get(HeaderTokensRemaining); val != "" {
		if n, err := strconv.ParseInt(strings.TrimSpace(val), 10, 64); err == nil {
			rl.TokensRemaining = n
		}
	}
	if val := h.Get(HeaderTokensReset); val != "" {
		rl.TokensReset = parseTimestamp(val)
	}

	if val := h.Get(HeaderInputTokensLimit); val != "" {
		if n, err := strconv.ParseInt(strings.TrimSpace(val), 10, 64); err == nil {
			rl.InputTokensLimit = n
		}
	}
	if val := h.Get(HeaderInputTokensRemaining); val != "" {
		if n, err := strconv.ParseInt(strings.TrimSpace(val), 10, 64); err == nil {
			rl.InputTokensRemaining = n
		}
	}
	if val := h.Get(HeaderInputTokensReset); val != "" {
		rl.InputTokensReset = parseTimestamp(val)
	}

	if val := h.Get(HeaderOutputTokensLimit); val != "" {
		if n, err := strconv.ParseInt(strings.TrimSpace(val), 10, 64); err == nil {
			rl.OutputTokensLimit = n
		}
	}
	if val := h.Get(HeaderOutputTokensRemaining); val != "" {
		if n, err := strconv.ParseInt(strings.TrimSpace(val), 10, 64); err == nil {
			rl.OutputTokensRemaining = n
		}
	}
	if val := h.Get(HeaderOutputTokensReset); val != "" {
		rl.OutputTokensReset = parseTimestamp(val)
	}

	if val := h.Get(HeaderRetryAfter); val != "" {
		rl.RetryAfter = parseRetryAfter(val)
	}

	return rl
}

// IsRateLimited returns true if any rate limit has 0 remaining and reset is in the future.
func (rl RateLimits) IsRateLimited(now time.Time) bool {
	if rl.RequestsLimit > 0 && rl.RequestsRemaining == 0 && rl.RequestsReset.After(now) {
		return true
	}
	if rl.TokensLimit > 0 && rl.TokensRemaining == 0 && rl.TokensReset.After(now) {
		return true
	}
	if rl.InputTokensLimit > 0 && rl.InputTokensRemaining == 0 && rl.InputTokensReset.After(now) {
		return true
	}
	if rl.OutputTokensLimit > 0 && rl.OutputTokensRemaining == 0 && rl.OutputTokensReset.After(now) {
		return true
	}
	return false
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -v ./internal/claudecode -run "TestExtractRateLimits.*|TestRateLimits.*"`
Expected: PASS

- [ ] **Step 5: Commit changes**

```bash
git add internal/claudecode/types.go internal/claudecode/ratelimit.go internal/claudecode/ratelimit_test.go
git commit -m "feat(claudecode): support granular input/output token rate-limit headers"
```

---

### Task 2: Active Rate-Limit Retrieval Probe in Client

**Files:**
- Modify: `internal/claudecode/client.go`
- Test: `internal/claudecode/client_test.go`

**Interfaces:**
- Produces:
  - `(c *Client) FetchRateLimits(ctx context.Context, token string) (RateLimits, error)`

- [ ] **Step 1: Write the failing test for FetchRateLimits**

Add test in `internal/claudecode/client_test.go`:

```go
func TestClient_FetchRateLimits(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer sk-ant-oat-test" {
			t.Errorf("unexpected auth header: %s", r.Header.Get("Authorization"))
		}
		w.Header().Set("anthropic-ratelimit-requests-limit", "500")
		w.Header().Set("anthropic-ratelimit-requests-remaining", "495")
		w.Header().Set("anthropic-ratelimit-tokens-limit", "200000")
		w.Header().Set("anthropic-ratelimit-tokens-remaining", "198000")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[{"id":"claude-3-7-sonnet"}]}`))
	}))
	defer ts.Close()

	client := NewClient(ts.URL, nil)
	rl, err := client.FetchRateLimits(context.Background(), "sk-ant-oat-test")
	if err != nil {
		t.Fatalf("FetchRateLimits failed: %v", err)
	}

	if rl.RequestsLimit != 500 || rl.RequestsRemaining != 495 {
		t.Errorf("unexpected requests limit: %+v", rl)
	}
	if rl.TokensLimit != 200000 || rl.TokensRemaining != 198000 {
		t.Errorf("unexpected tokens limit: %+v", rl)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -v ./internal/claudecode -run "TestClient_FetchRateLimits"`
Expected: FAIL with `client.FetchRateLimits undefined`.

- [ ] **Step 3: Implement `FetchRateLimits` on `Client`**

Add `FetchRateLimits` in `internal/claudecode/client.go`:

```go
// FetchRateLimits performs a lightweight query to Anthropic /v1/models to extract active rate-limit headers.
func (c *Client) FetchRateLimits(ctx context.Context, token string) (RateLimits, error) {
	cleanToken := strings.TrimSpace(token)
	if cleanToken == "" {
		return RateLimits{}, errors.New("cannot fetch rate limits with empty token")
	}

	url := fmt.Sprintf("%s/v1/models", c.baseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return RateLimits{}, fmt.Errorf("create rate-limit probe request: %w", err)
	}

	req.Header.Set("anthropic-version", DefaultAnthropicVersion)
	req.Header.Set("User-Agent", "Claude-Code/2.1.246")
	ApplyAuthHeaders(req, cleanToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return RateLimits{}, fmt.Errorf("execute rate-limit probe: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	rl := ExtractRateLimits(resp.Header)
	if resp.StatusCode >= 400 && resp.StatusCode != http.StatusTooManyRequests {
		return rl, fmt.Errorf("rate-limit probe returned status %d", resp.StatusCode)
	}

	return rl, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -v ./internal/claudecode -run "TestClient_FetchRateLimits"`
Expected: PASS

- [ ] **Step 5: Commit changes**

```bash
git add internal/claudecode/client.go internal/claudecode/client_test.go
git commit -m "feat(claudecode): add FetchRateLimits probe method to client"
```

---

### Task 3: Account Pool OAuth Refresh and Rate-Limit Updaters

**Files:**
- Modify: `internal/claudecode/pool.go`
- Test: `internal/claudecode/pool_test.go`

**Interfaces:**
- Produces:
  - `(p *AccountPool) RefreshAccountToken(accountID string) error`
  - `(p *AccountPool) RefreshAllExpiringTokens(window time.Duration) ([]string, error)`
  - `(p *AccountPool) UpdateAccountRateLimits(accountID string, rl RateLimits)`
  - Updated `isAccountHealthy(acc *Account, now time.Time) bool` using `rl.IsRateLimited(now)`

- [ ] **Step 1: Write failing tests for RefreshAccountToken and RefreshAllExpiringTokens**

Add tests in `internal/claudecode/pool_test.go`:

```go
func TestAccountPool_RefreshTokenIfNeeded_AndForceRefresh(t *testing.T) {
	exp := time.Now().Add(2 * time.Minute) // Expiring in 2m (within 5m window)
	pool := NewAccountPool([]AccountConfig{
		{
			ID:           "acc-oauth",
			Name:         "OAuth Acc",
			Token:        "old-access-tok",
			RefreshToken: "valid-refresh-tok",
			ExpiresAt:    &exp,
			Type:         "oauth",
			Enabled:      true,
		},
	})

	refreshedCount := 0
	pool.SetTokenRefresher(func(refreshToken string) (string, string, int, error) {
		refreshedCount++
		if refreshToken != "valid-refresh-tok" {
			t.Errorf("unexpected refresh token: %s", refreshToken)
		}
		return "new-access-tok", "new-refresh-tok", 3600, nil
	})

	acc, _ := pool.GetAccount("acc-oauth")
	err := pool.RefreshTokenIfNeeded(acc)
	if err != nil {
		t.Fatalf("RefreshTokenIfNeeded failed: %v", err)
	}

	if refreshedCount != 1 {
		t.Errorf("expected 1 refresh, got %d", refreshedCount)
	}

	acc.mu.RLock()
	if acc.Token != "new-access-tok" || acc.RefreshToken != "new-refresh-tok" {
		t.Errorf("tokens not updated: token=%s, refresh=%s", acc.Token, acc.RefreshToken)
	}
	acc.mu.RUnlock()

	// Force refresh test
	err = pool.RefreshAccountToken("acc-oauth")
	if err != nil {
		t.Fatalf("RefreshAccountToken failed: %v", err)
	}
	if refreshedCount != 2 {
		t.Errorf("expected 2 refreshes, got %d", refreshedCount)
	}
}

func TestAccountPool_RefreshAllExpiringTokens(t *testing.T) {
	expiringSoon := time.Now().Add(10 * time.Minute)
	fresh := time.Now().Add(5 * time.Hour)

	pool := NewAccountPool([]AccountConfig{
		{
			ID:           "acc-1",
			Token:        "tok-1",
			RefreshToken: "ref-1",
			ExpiresAt:    &expiringSoon,
			Enabled:      true,
		},
		{
			ID:           "acc-2",
			Token:        "tok-2",
			RefreshToken: "ref-2",
			ExpiresAt:    &fresh,
			Enabled:      true,
		},
	})

	pool.SetTokenRefresher(func(refreshToken string) (string, string, int, error) {
		return "refreshed-" + refreshToken, "new-" + refreshToken, 7200, nil
	})

	refreshedIDs, err := pool.RefreshAllExpiringTokens(15 * time.Minute)
	if err != nil {
		t.Fatalf("RefreshAllExpiringTokens failed: %v", err)
	}

	if len(refreshedIDs) != 1 || refreshedIDs[0] != "acc-1" {
		t.Errorf("expected only acc-1 refreshed, got: %v", refreshedIDs)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -v ./internal/claudecode -run "TestAccountPool_RefreshTokenIfNeeded_AndForceRefresh|TestAccountPool_RefreshAllExpiringTokens"`
Expected: FAIL with missing methods.

- [ ] **Step 3: Implement pool refresh methods and update `isAccountHealthy`**

Update `internal/claudecode/pool.go`:

```go
// RefreshAccountToken forces an immediate token refresh for an account regardless of expiry timestamp.
func (p *AccountPool) RefreshAccountToken(accountID string) error {
	p.mu.RLock()
	acc, exists := p.accounts[accountID]
	refresher := p.tokenRefresher
	p.mu.RUnlock()

	if !exists || acc == nil {
		return ErrAccountNotFound
	}
	if refresher == nil {
		return errors.New("no token refresher configured for pool")
	}

	acc.mu.RLock()
	refreshToken := acc.RefreshToken
	acc.mu.RUnlock()

	if refreshToken == "" {
		return fmt.Errorf("account %s has no refresh token", accountID)
	}

	newToken, newRefreshToken, expiresIn, err := refresher(refreshToken)
	if err != nil {
		return fmt.Errorf("refresh token for %s: %w", accountID, err)
	}

	acc.mu.Lock()
	acc.Token = newToken
	if newRefreshToken != "" {
		acc.RefreshToken = newRefreshToken
	}
	if expiresIn > 0 {
		newExp := time.Now().Add(time.Duration(expiresIn) * time.Second)
		acc.ExpiresAt = &newExp
	}
	acc.mu.Unlock()

	_ = p.SaveStoredAccounts()
	return nil
}

// RefreshAllExpiringTokens scans all enabled accounts and refreshes those expiring within window.
func (p *AccountPool) RefreshAllExpiringTokens(window time.Duration) ([]string, error) {
	p.mu.RLock()
	refresher := p.tokenRefresher
	accounts := make([]*Account, 0, len(p.accounts))
	for _, acc := range p.accounts {
		accounts = append(accounts, acc)
	}
	p.mu.RUnlock()

	if refresher == nil {
		return nil, nil
	}

	var refreshedIDs []string
	now := time.Now()

	for _, acc := range accounts {
		acc.mu.RLock()
		enabled := acc.Enabled
		refreshToken := acc.RefreshToken
		expiresAt := acc.ExpiresAt
		id := acc.ID
		acc.mu.RUnlock()

		if !enabled || refreshToken == "" || expiresAt == nil {
			continue
		}

		if expiresAt.Sub(now) <= window {
			if err := p.RefreshAccountToken(id); err == nil {
				refreshedIDs = append(refreshedIDs, id)
			}
		}
	}

	return refreshedIDs, nil
}

// UpdateAccountRateLimits updates the cached rate limits for an account.
func (p *AccountPool) UpdateAccountRateLimits(accountID string, rl RateLimits) {
	p.mu.RLock()
	acc, ok := p.accounts[accountID]
	p.mu.RUnlock()

	if ok && acc != nil {
		acc.mu.Lock()
		acc.RateLimits = rl
		acc.mu.Unlock()
	}
}

// isAccountHealthy checks enablement, cooldowns, and granular rate limits.
func isAccountHealthy(acc *Account, now time.Time) bool {
	acc.mu.RLock()
	defer acc.mu.RUnlock()

	if !acc.Enabled {
		return false
	}
	if acc.CooldownUntil.After(now) {
		return false
	}
	if acc.RateLimits.IsRateLimited(now) {
		return false
	}
	return true
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -v ./internal/claudecode -run "TestAccountPool.*"`
Expected: PASS

- [ ] **Step 5: Commit changes**

```bash
git add internal/claudecode/pool.go internal/claudecode/pool_test.go
git commit -m "feat(claudecode): add force refresh and bulk expiring token refresh to pool"
```

---

### Task 4: Extract Refresh Token & Metadata in Claude CLI Discovery

**Files:**
- Modify: `internal/claudecode/discovery.go`
- Test: `internal/claudecode/discovery_test.go`

**Interfaces:**
- Produces:
  - Updated `extractTokensFromJSON(data []byte, sourceLabel string, target map[string]AccountConfig)` parsing `oauthAccount`, `refreshToken`, `expiresAt`, `accountUuid`.

- [ ] **Step 1: Write the failing test for discovery OAuth fields**

Add test in `internal/claudecode/discovery_test.go`:

```go
func TestDiscoverLocalCredentials_OAuthWithRefreshToken(t *testing.T) {
	tempDir := t.TempDir()
	claudeJSON := `{
		"oauthAccount": {
			"accountUuid": "uuid-1234",
			"emailAddress": "dev@example.com",
			"organizationUuid": "org-5678"
		},
		"oauthToken": "sk-ant-oat-token-abc",
		"refreshToken": "ant-refresh-token-xyz",
		"expiresAt": "2026-08-29T18:00:00Z"
	}`

	err := os.WriteFile(filepath.Join(tempDir, ".claude.json"), []byte(claudeJSON), 0600)
	if err != nil {
		t.Fatalf("failed to write test .claude.json: %v", err)
	}

	accounts, err := DiscoverLocalCredentials(tempDir)
	if err != nil {
		t.Fatalf("DiscoverLocalCredentials failed: %v", err)
	}

	if len(accounts) == 0 {
		t.Fatalf("expected at least 1 discovered account, got 0")
	}

	var oauthAcc *AccountConfig
	for _, a := range accounts {
		if a.Token == "sk-ant-oat-token-abc" {
			accCopy := a
			oauthAcc = &accCopy
			break
		}
	}

	if oauthAcc == nil {
		t.Fatalf("expected oauth account with token sk-ant-oat-token-abc")
	}
	if oauthAcc.Type != "oauth" {
		t.Errorf("expected type=oauth, got %s", oauthAcc.Type)
	}
	if oauthAcc.RefreshToken != "ant-refresh-token-xyz" {
		t.Errorf("expected refreshToken=ant-refresh-token-xyz, got %s", oauthAcc.RefreshToken)
	}
	if oauthAcc.Email != "dev@example.com" {
		t.Errorf("expected email=dev@example.com, got %s", oauthAcc.Email)
	}
	if oauthAcc.AccountUUID != "uuid-1234" {
		t.Errorf("expected accountUuid=uuid-1234, got %s", oauthAcc.AccountUUID)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -v ./internal/claudecode -run "TestDiscoverLocalCredentials_OAuthWithRefreshToken"`
Expected: FAIL (missing fields or type not set to oauth).

- [ ] **Step 3: Enhance `extractTokensFromJSON` to extract nested OAuth metadata**

Update `internal/claudecode/discovery.go`:

```go
func extractTokensFromJSON(data []byte, sourceLabel string, target map[string]AccountConfig) {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return
	}

	var email, accountUUID, orgUUID, refreshToken string
	var expiresAt *time.Time

	// Check oauthAccount object
	if oauthAcc, ok := raw["oauthAccount"].(map[string]any); ok {
		if em, ok := oauthAcc["emailAddress"].(string); ok {
			email = strings.TrimSpace(em)
		}
		if au, ok := oauthAcc["accountUuid"].(string); ok {
			accountUUID = strings.TrimSpace(au)
		}
		if ou, ok := oauthAcc["organizationUuid"].(string); ok {
			orgUUID = strings.TrimSpace(ou)
		}
	}

	if refTok, ok := raw["refreshToken"].(string); ok {
		refreshToken = strings.TrimSpace(refTok)
	} else if refTok, ok := raw["refresh_token"].(string); ok {
		refreshToken = strings.TrimSpace(refTok)
	}

	if expVal, ok := raw["expiresAt"]; ok {
		if expStr, ok := expVal.(string); ok {
			if t, err := time.Parse(time.RFC3339, expStr); err == nil {
				expiresAt = &t
			}
		} else if expNum, ok := expVal.(float64); ok {
			t := time.Unix(int64(expNum), 0)
			expiresAt = &t
		}
	}

	tokenKeys := []string{
		"sessionKey", "setup_token", "setupToken", "token",
		"oauth_token", "oauthToken", "apiKey", "api_key", "anthropicApiKey",
	}

	for _, key := range tokenKeys {
		if val, ok := raw[key]; ok {
			if strVal, isStr := val.(string); isStr {
				cleanToken := strings.TrimSpace(strVal)
				if cleanToken != "" && !strings.HasPrefix(cleanToken, "test") {
					if _, exists := target[cleanToken]; !exists {
						tokenType := "setup_token"
						if IsOAuthToken(cleanToken) || key == "oauthToken" || key == "oauth_token" || refreshToken != "" {
							tokenType = "oauth"
						} else if strings.HasPrefix(cleanToken, "sk-ant-api") {
							tokenType = "api_key"
						}
						id := generateTokenID(cleanToken)
						name := fmt.Sprintf("Claude CLI (%s)", sourceLabel)
						if email != "" {
							name = fmt.Sprintf("Claude Code (%s)", email)
						}
						target[cleanToken] = AccountConfig{
							ID:               fmt.Sprintf("auto-%s-%s", sourceLabel, id),
							Name:             name,
							Token:            cleanToken,
							RefreshToken:     refreshToken,
							ExpiresAt:        expiresAt,
							Email:            email,
							AccountUUID:      accountUUID,
							OrganizationUUID: orgUUID,
							Type:             tokenType,
							Priority:         1,
							Enabled:          true,
							Source:           "auto_import",
						}
					}
				}
			}
		}
	}

	// Also inspect nested env map if present
	if envRaw, ok := raw["env"].(map[string]any); ok {
		for _, key := range tokenKeys {
			if val, ok := envRaw[key]; ok {
				if strVal, isStr := val.(string); isStr {
					cleanToken := strings.TrimSpace(strVal)
					if cleanToken != "" && !strings.HasPrefix(cleanToken, "test") {
						if _, exists := target[cleanToken]; !exists {
							tokenType := "setup_token"
							if IsOAuthToken(cleanToken) {
								tokenType = "oauth"
							} else if strings.HasPrefix(cleanToken, "sk-ant-api") {
								tokenType = "api_key"
							}
							id := generateTokenID(cleanToken)
							target[cleanToken] = AccountConfig{
								ID:       fmt.Sprintf("auto-env-%s-%s", sourceLabel, id),
								Name:     fmt.Sprintf("Claude CLI Env (%s)", sourceLabel),
								Token:    cleanToken,
								Type:     tokenType,
								Priority: 1,
								Enabled:  true,
								Source:   "auto_import",
							}
						}
					}
				}
			}
		}
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -v ./internal/claudecode -run "TestDiscoverLocalCredentials.*"`
Expected: PASS

- [ ] **Step 5: Commit changes**

```bash
git add internal/claudecode/discovery.go internal/claudecode/discovery_test.go
git commit -m "feat(claudecode): extract OAuth refresh tokens and metadata during discovery"
```

---

### Task 5: Just-In-Time Refresh & 401 Unauthorized Retry in Proxy Routing

**Files:**
- Modify: `internal/api/claudecode_proxy.go`
- Modify: `internal/api/claudecode_oauth_handlers.go`
- Test: `internal/api/claudecode_proxy_test.go`

**Interfaces:**
- Consumes:
  - `pool.RefreshTokenIfNeeded(acc)`
  - `pool.RefreshAccountToken(acc.ID)`
- Produces:
  - Automatic just-in-time token refresh before sending request in `forwardToClaudeCode`.
  - Automatic refresh and retry upon upstream HTTP 401 Unauthorized with token expiration.
  - Server helper `syncRefreshedAccountToConfig(acc *claudecode.Account)`.

- [ ] **Step 1: Write failing test for proxy 401 retry on expired token**

Add test in `internal/api/claudecode_proxy_test.go`:

```go
func TestForwardToClaudeCode_AutoRefreshOn401(t *testing.T) {
	reqCount := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqCount++
		authHdr := r.Header.Get("Authorization")
		if reqCount == 1 {
			// First request returns 401 expired token
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"OAuth token has expired"}}`))
			return
		}
		// Second request with refreshed token succeeds
		if authHdr != "Bearer refreshed-token" {
			t.Errorf("expected Bearer refreshed-token, got %s", authHdr)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"msg_123","type":"message","usage":{"input_tokens":10,"output_tokens":20}}`))
	}))
	defer ts.Close()

	oauthMgr := auth.NewClaudeCodeOAuthManager()
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "refreshed-token",
			"refresh_token": "new-refresh-token",
			"expires_in":    3600,
		})
	}))
	defer tokenSrv.Close()
	oauthMgr.SetEndpoints("", tokenSrv.URL, "", nil)

	srv, err := New(Options{
		APIKey: "test-key",
		Credentials: func(ctx context.Context) (auth.Credentials, error) {
			return auth.Credentials{AccessToken: "token"}, nil
		},
		NewUpstream: func(s string) Upstream { return nil },
		ClaudeCodeOAuthMgr: oauthMgr,
	})
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	cfg := claudecode.Config{
		Enabled: true,
		BaseURL: ts.URL,
		Accounts: []claudecode.AccountConfig{
			{
				ID:           "acc-oauth",
				Name:         "OAuth Acc",
				Token:        "expired-token",
				RefreshToken: "valid-refresh",
				Type:         "oauth",
				Priority:     1,
				Enabled:      true,
			},
		},
		Allowlist: claudecode.DefaultAllowlist(),
		Routing:   claudecode.DefaultRoutingConfig(),
	}

	reqBody := []byte(`{"model":"claude-3-7-sonnet-20250219","messages":[{"role":"user","content":"hello"}]}`)
	httpReq := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(reqBody))
	w := httptest.NewRecorder()

	srv.forwardToClaudeCode(w, httpReq, cfg, reqBody, "claude-3-7-sonnet-20250219")

	if w.Code != http.StatusOK {
		t.Errorf("expected status 200 OK after auto-refresh retry, got %d: %s", w.Code, w.Body.String())
	}
	if reqCount != 2 {
		t.Errorf("expected 2 upstream requests (initial 401 + retry), got %d", reqCount)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -v ./internal/api -run "TestForwardToClaudeCode_AutoRefreshOn401"`
Expected: FAIL with 401 or 503 instead of 200.

- [ ] **Step 3: Update `getOrCreateCCPool` and `forwardToClaudeCode` with JIT refresh & 401 retry**

In `internal/api/claudecode_proxy.go`:
1. Ensure `getOrCreateCCPool` sets `tokenRefresher` whenever initialized:
```go
func (server *Server) getOrCreateCCPool(cfg claudecode.Config) (*claudecode.AccountPool, *claudecode.Client) {
	ccPoolMu.Lock()
	defer ccPoolMu.Unlock()

	key := cfg.BaseURL
	if ccPoolInst == nil || ccPoolKey != key || len(ccPoolCfg.Accounts) != len(cfg.Accounts) {
		ccPoolInst = claudecode.NewAccountPool(cfg.Accounts)
		ccHTTPClient = claudecode.NewClient(claudecode.NormalizeBaseURL(cfg.BaseURL), nil)
		ccPoolKey = key
		ccPoolCfg = cfg

		if server != nil && server.claudeCodeOAuthMgr != nil {
			ccPoolInst.SetTokenRefresher(func(refreshToken string) (string, string, int, error) {
				resp, err := server.claudeCodeOAuthMgr.RefreshToken(refreshToken)
				if err != nil {
					return "", "", 0, err
				}
				return resp.AccessToken, resp.RefreshToken, resp.ExpiresIn, nil
			})
		}
	}
	if ccHTTPClient == nil {
		ccHTTPClient = claudecode.NewClient(claudecode.NormalizeBaseURL(cfg.BaseURL), nil)
	}
	return ccPoolInst, ccHTTPClient
}
```
2. In `forwardToClaudeCode`, check `_ = pool.RefreshTokenIfNeeded(acc)` before `client.SendMessage`.
3. If `resp.StatusCode == http.StatusUnauthorized`, check if `acc.RefreshToken != ""` and attempt `pool.RefreshAccountToken(acc.ID)` then retry the request with the refreshed token once.
4. Add `syncRefreshedAccountToConfig` to persist updated token and refresh token to `config.json`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -v ./internal/api -run "TestForwardToClaudeCode.*|TestClaudeCode.*"`
Expected: PASS

- [ ] **Step 5: Commit changes**

```bash
git add internal/api/claudecode_proxy.go internal/api/claudecode_oauth_handlers.go internal/api/claudecode_proxy_test.go
git commit -m "feat(api): add just-in-time token refresh and 401 retry to Claude Code proxy"
```

---

### Task 6: Rate-Limit Probing & Refresh Management Endpoints

**Files:**
- Modify: `internal/api/claudecode_management.go`
- Test: `internal/api/claudecode_management_test.go`

**Interfaces:**
- Produces:
  - `POST /api/claudecode/accounts/:id/ratelimits` - queries live rate limits from Anthropic for an account and updates pool.
  - `POST /api/claudecode/accounts/:id/refresh` - forces an immediate OAuth token refresh for the account.
  - `POST /api/claudecode/ratelimits` - queries live rate limits for all enabled accounts.

- [ ] **Step 1: Write failing tests for rate limit probe and refresh endpoints**

Add tests in `internal/api/claudecode_management_test.go`:

```go
func TestClaudeCodeManagement_AccountRateLimitsAndRefresh(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("anthropic-ratelimit-requests-limit", "100")
		w.Header().Set("anthropic-ratelimit-requests-remaining", "90")
		w.Header().Set("anthropic-ratelimit-input-tokens-limit", "50000")
		w.Header().Set("anthropic-ratelimit-input-tokens-remaining", "45000")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[{"id":"claude-3-7-sonnet"}]}`))
	}))
	defer ts.Close()

	oauthMgr := auth.NewClaudeCodeOAuthManager()
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "new-tok",
			"refresh_token": "new-ref",
			"expires_in":    3600,
		})
	}))
	defer tokenSrv.Close()
	oauthMgr.SetEndpoints("", tokenSrv.URL, "", nil)

	srv, err := New(Options{
		APIKey: "test-key",
		Credentials: func(ctx context.Context) (auth.Credentials, error) {
			return auth.Credentials{AccessToken: "tok"}, nil
		},
		NewUpstream: func(s string) Upstream { return nil },
		ClaudeCodeOAuthMgr: oauthMgr,
	})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	config.SetForTest(config.Config{
		ClaudeCode: claudecode.Config{
			Enabled: true,
			BaseURL: ts.URL,
			Accounts: []claudecode.AccountConfig{
				{
					ID:           "acc-test-rl",
					Name:         "Test RL",
					Token:        "tok-test",
					RefreshToken: "ref-test",
					Type:         "oauth",
					Enabled:      true,
				},
			},
		},
	})

	// 1. POST /api/claudecode/accounts/acc-test-rl/ratelimits
	rlReq := httptest.NewRequest(http.MethodPost, "/api/claudecode/accounts/acc-test-rl/ratelimits", nil)
	w := httptest.NewRecorder()
	srv.routeClaudeCodeManagement(w, rlReq, "/api/claudecode/accounts/acc-test-rl/ratelimits", http.MethodPost)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w.Code, w.Body.String())
	}

	var rlResp struct {
		Status     string                `json:"status"`
		RateLimits claudecode.RateLimits `json:"rate_limits"`
	}
	if err := json.NewDecoder(w.Body).Decode(&rlResp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if rlResp.RateLimits.RequestsLimit != 100 || rlResp.RateLimits.RequestsRemaining != 90 {
		t.Errorf("unexpected rate limits: %+v", rlResp.RateLimits)
	}

	// 2. POST /api/claudecode/accounts/acc-test-rl/refresh
	refReq := httptest.NewRequest(http.MethodPost, "/api/claudecode/accounts/acc-test-rl/refresh", nil)
	w2 := httptest.NewRecorder()
	srv.routeClaudeCodeManagement(w2, refReq, "/api/claudecode/accounts/acc-test-rl/refresh", http.MethodPost)

	if w2.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d: %s", w2.Code, w2.Body.String())
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -v ./internal/api -run "TestClaudeCodeManagement_AccountRateLimitsAndRefresh"`
Expected: FAIL (routes unhandled).

- [ ] **Step 3: Implement handlers and route registration**

Add in `internal/api/claudecode_management.go`:
- `handleClaudeCodeAccountRateLimits(writer http.ResponseWriter, request *http.Request, accountID string)`
- `handleClaudeCodeAccountRefresh(writer http.ResponseWriter, request *http.Request, accountID string)`
- Register routes in `routeClaudeCodeManagement`:
  - `strings.HasPrefix(path, "/api/claudecode/accounts/") && strings.HasSuffix(path, "/ratelimits") && method == http.MethodPost`
  - `strings.HasPrefix(path, "/api/claudecode/accounts/") && strings.HasSuffix(path, "/refresh") && method == http.MethodPost`
  - `path == "/api/claudecode/ratelimits" && method == http.MethodPost`

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -v ./internal/api -run "TestClaudeCodeManagement.*"`
Expected: PASS

- [ ] **Step 5: Commit changes**

```bash
git add internal/api/claudecode_management.go internal/api/claudecode_management_test.go
git commit -m "feat(api): add account rate-limits probe and token refresh management endpoints"
```

---

### Task 7: Background Token Refresh & Rate-Limit Sync Worker in Server

**Files:**
- Modify: `internal/api/server.go`
- Test: `internal/api/server_test.go`

**Interfaces:**
- Produces:
  - `(server *Server) StartClaudeCodeBackgroundWorker(ctx context.Context)` - background goroutine running every 5 minutes to refresh tokens expiring in <15 minutes.

- [ ] **Step 1: Write test for background refresh worker**

Add test in `internal/api/server_test.go`:

```go
func TestServer_ClaudeCodeBackgroundWorker(t *testing.T) {
	refreshed := make(chan string, 1)
	oauthMgr := auth.NewClaudeCodeOAuthManager()
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshed <- "refreshed"
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "new-tok",
			"refresh_token": "new-ref",
			"expires_in":    3600,
		})
	}))
	defer tokenSrv.Close()
	oauthMgr.SetEndpoints("", tokenSrv.URL, "", nil)

	srv, err := New(Options{
		APIKey: "key",
		Credentials: func(ctx context.Context) (auth.Credentials, error) {
			return auth.Credentials{AccessToken: "tok"}, nil
		},
		NewUpstream: func(s string) Upstream { return nil },
		ClaudeCodeOAuthMgr: oauthMgr,
	})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	exp := time.Now().Add(5 * time.Minute)
	config.SetForTest(config.Config{
		ClaudeCode: claudecode.Config{
			Enabled: true,
			Accounts: []claudecode.AccountConfig{
				{
					ID:           "acc-bg",
					Token:        "tok-bg",
					RefreshToken: "ref-bg",
					ExpiresAt:    &exp,
					Type:         "oauth",
					Enabled:      true,
				},
			},
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Execute one background check tick
	srv.tickClaudeCodeBackgroundWorker()

	select {
	case <-refreshed:
		// success
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout waiting for background token refresh")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -v ./internal/api -run "TestServer_ClaudeCodeBackgroundWorker"`
Expected: FAIL (`tickClaudeCodeBackgroundWorker undefined`).

- [ ] **Step 3: Implement background worker loop and tick helper**

In `internal/api/server.go`:
```go
func (server *Server) tickClaudeCodeBackgroundWorker() {
	cfg := config.Get()
	if !cfg.ClaudeCode.Enabled {
		return
	}
	pool, _ := server.getOrCreateCCPool(cfg.ClaudeCode)
	if pool == nil {
		return
	}

	// Proactively refresh tokens expiring in <= 15 minutes
	refreshed, err := pool.RefreshAllExpiringTokens(15 * time.Minute)
	if err != nil {
		if server.logger != nil {
			server.logger.Warn("background token refresh failed", "error", err)
		}
		return
	}
	if len(refreshed) > 0 && server.logger != nil {
		server.logger.Info("background refreshed Claude Code tokens", "count", len(refreshed), "accounts", refreshed)
	}
}

func (server *Server) StartClaudeCodeBackgroundWorker(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Minute)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				server.tickClaudeCodeBackgroundWorker()
			}
		}
	}()
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test -v ./internal/api -run "TestServer_ClaudeCodeBackgroundWorker"`
Expected: PASS

- [ ] **Step 5: Commit changes**

```bash
git add internal/api/server.go internal/api/server_test.go
git commit -m "feat(api): add background OAuth token refresh worker to server"
```

---

### Task 8: End-to-End Verification & Full Test Suite Pass

**Files:**
- Test: `internal/...`

- [ ] **Step 1: Run full unit and integration test suite**

Run: `go test -v ./internal/...`
Expected: All packages pass with 100% green tests.

- [ ] **Step 2: Verify binary builds cleanly**

Run: `go build -o /dev/null ./cmd/proxy`
Expected: Zero compilation or linking errors.

- [ ] **Step 3: Commit final plan verification checkpoint**

```bash
git commit --allow-empty -m "chore(claudecode): verify automated OAuth refresh and rate-limit retrieval integration"
```
