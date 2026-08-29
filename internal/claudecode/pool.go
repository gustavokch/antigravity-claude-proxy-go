package claudecode

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

var (
	// ErrNoAvailableAccounts is returned when all accounts are disabled, rate-limited, or in cooldown.
	ErrNoAvailableAccounts = errors.New("no available Claude Code accounts")
	// ErrAccountNotFound is returned when referencing a non-existent account ID.
	ErrAccountNotFound = errors.New("claude code account not found")
)

// maxStickyEntries bounds the sticky session map to prevent unbounded memory growth.
const maxStickyEntries = 10000

// TokenRefresher is a callback function to refresh an OAuth access token using a refresh token.
type TokenRefresher func(refreshToken string) (accessToken string, newRefreshToken string, expiresIn int, err error)

// AccountPool manages a collection of Claude Code accounts with sticky routing,
// health tracking, and load balancing.
type AccountPool struct {
	mu             sync.RWMutex
	accounts       map[string]*Account
	sticky         map[string]string // sessionKey -> accountID
	stickyAt       map[string]time.Time
	storagePath    string
	tokenRefresher TokenRefresher
}

// NewAccountPool creates a new AccountPool initialized with the provided accounts.
func NewAccountPool(configs []AccountConfig) *AccountPool {
	p := &AccountPool{
		accounts: make(map[string]*Account),
		sticky:   make(map[string]string),
		stickyAt: make(map[string]time.Time),
	}

	for _, cfg := range configs {
		p.AddOrUpdateAccount(cfg)
	}

	return p
}

// SetStoragePath configures the file path used to persist dynamic accounts.
func (p *AccountPool) SetStoragePath(path string) {
	p.mu.Lock()
	p.storagePath = path
	p.mu.Unlock()
}

// SetTokenRefresher configures the callback function used to refresh access tokens.
func (p *AccountPool) SetTokenRefresher(refresher TokenRefresher) {
	p.mu.Lock()
	p.tokenRefresher = refresher
	p.mu.Unlock()
}

// LoadStoredAccounts loads and merges persistent accounts from storage.
func (p *AccountPool) LoadStoredAccounts() error {
	p.mu.RLock()
	path := p.storagePath
	p.mu.RUnlock()

	stored, err := LoadStoredAccounts(path)
	if err != nil {
		return err
	}

	for _, cfg := range stored {
		p.AddOrUpdateAccount(cfg)
	}
	return nil
}

// SaveStoredAccounts persists all accounts marked as persistent (source "oauth" or "manual") to disk.
func (p *AccountPool) SaveStoredAccounts() error {
	p.mu.RLock()
	path := p.storagePath
	if path == "" {
		path = DefaultStoragePath()
	}

	var toSave []AccountConfig
	for _, acc := range p.accounts {
		acc.mu.RLock()
		// Only persist dynamic accounts (e.g. oauth, manual)
		if acc.Source == "oauth" || acc.Source == "manual" {
			toSave = append(toSave, AccountConfig{
				ID:               acc.ID,
				Name:             acc.Name,
				Token:            acc.Token,
				RefreshToken:     acc.RefreshToken,
				ExpiresAt:        acc.ExpiresAt,
				Email:            acc.Email,
				AccountUUID:      acc.AccountUUID,
				OrganizationUUID: acc.OrganizationUUID,
				Type:             acc.Type,
				Priority:         acc.Priority,
				Enabled:          acc.Enabled,
				Source:           acc.Source,
			})
		}
		acc.mu.RUnlock()
	}
	p.mu.RUnlock()

	return SaveStoredAccounts(path, toSave)
}

// AddOrUpdateAccount adds a new account or updates an existing account by ID.
func (p *AccountPool) AddOrUpdateAccount(cfg AccountConfig) *Account {
	p.mu.Lock()
	defer p.mu.Unlock()

	id := cfg.ID
	if id == "" {
		if cfg.Email != "" {
			id = "cc-" + cfg.Email
		} else if cfg.AccountUUID != "" {
			id = "cc-" + cfg.AccountUUID
		} else {
			id = fmt.Sprintf("cc-acc-%d", time.Now().UnixNano())
		}
		cfg.ID = id
	}

	existing, ok := p.accounts[id]
	if ok {
		existing.mu.Lock()
		if cfg.Name != "" {
			existing.Name = cfg.Name
		}
		if cfg.Token != "" {
			existing.Token = cfg.Token
		}
		if cfg.RefreshToken != "" {
			existing.RefreshToken = cfg.RefreshToken
		}
		if cfg.ExpiresAt != nil {
			existing.ExpiresAt = cfg.ExpiresAt
		}
		if cfg.Email != "" {
			existing.Email = cfg.Email
		}
		if cfg.AccountUUID != "" {
			existing.AccountUUID = cfg.AccountUUID
		}
		if cfg.OrganizationUUID != "" {
			existing.OrganizationUUID = cfg.OrganizationUUID
		}
		if cfg.Type != "" {
			existing.Type = cfg.Type
		}
		if cfg.Priority > 0 {
			existing.Priority = cfg.Priority
		}
		existing.Enabled = cfg.Enabled
		if cfg.Source != "" {
			existing.Source = cfg.Source
		}
		existing.mu.Unlock()
		return existing
	}

	acc := &Account{
		ID:               id,
		Name:             cfg.Name,
		Token:            cfg.Token,
		RefreshToken:     cfg.RefreshToken,
		ExpiresAt:        cfg.ExpiresAt,
		Email:            cfg.Email,
		AccountUUID:      cfg.AccountUUID,
		OrganizationUUID: cfg.OrganizationUUID,
		Type:             cfg.Type,
		Priority:         cfg.Priority,
		Enabled:          cfg.Enabled,
		Source:           cfg.Source,
		CreatedAt:        time.Now(),
	}
	p.accounts[id] = acc
	return acc
}

// DeleteAccount removes an account by ID and evicts any sticky sessions mapped to it.
func (p *AccountPool) DeleteAccount(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	if _, ok := p.accounts[id]; !ok {
		return false
	}
	delete(p.accounts, id)

	for k, v := range p.sticky {
		if v == id {
			delete(p.sticky, k)
			delete(p.stickyAt, k)
		}
	}

	return true
}

// RefreshTokenIfNeeded checks if an account's token is close to expiry and refreshes it if a refresher is configured.
func (p *AccountPool) RefreshTokenIfNeeded(acc *Account) error {
	acc.mu.Lock()
	refreshToken := acc.RefreshToken
	expiresAt := acc.ExpiresAt
	p.mu.RLock()
	refresher := p.tokenRefresher
	p.mu.RUnlock()

	if refreshToken == "" || refresher == nil {
		acc.mu.Unlock()
		return nil
	}

	// Refresh if within 5 minutes of expiration
	needsRefresh := expiresAt != nil && time.Until(*expiresAt) < 5*time.Minute
	if !needsRefresh {
		acc.mu.Unlock()
		return nil
	}

	acc.mu.Unlock()

	newToken, newRefreshToken, expiresIn, err := refresher(refreshToken)
	if err != nil {
		return fmt.Errorf("refresh token for %s: %w", acc.ID, err)
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
	var errs []error
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
			} else {
				errs = append(errs, fmt.Errorf("account %s: %w", id, err))
			}
		}
	}

	return refreshedIDs, errors.Join(errs...)
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

// GetAccount retrieves a single account by ID.
func (p *AccountPool) GetAccount(id string) (*Account, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	acc, ok := p.accounts[id]
	return acc, ok
}

// ListAccounts returns all accounts in the pool.
func (p *AccountPool) ListAccounts() []*Account {
	p.mu.RLock()
	defer p.mu.RUnlock()

	res := make([]*Account, 0, len(p.accounts))
	for _, acc := range p.accounts {
		res = append(res, acc)
	}
	return res
}

// Snapshots returns immutable snapshots of all accounts in the pool.
func (p *AccountPool) Snapshots() []AccountSnapshot {
	p.mu.RLock()
	defer p.mu.RUnlock()

	snaps := make([]AccountSnapshot, 0, len(p.accounts))
	for _, acc := range p.accounts {
		snaps = append(snaps, acc.Snapshot())
	}

	// Sort snapshots by Priority ascending, then Name
	sort.Slice(snaps, func(i, j int) bool {
		if snaps[i].Priority != snaps[j].Priority {
			return snaps[i].Priority < snaps[j].Priority
		}
		return snaps[i].Name < snaps[j].Name
	})

	return snaps
}

// SelectAccount picks the most suitable healthy account, respecting sticky session affinity and exclusions.
func (p *AccountPool) SelectAccount(sessionKey string, excludedIDs map[string]bool) (*Account, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()

	// 1. Try sticky account if sessionKey is provided
	if sessionKey != "" {
		if stickyID, ok := p.sticky[sessionKey]; ok {
			if !excludedIDs[stickyID] {
				if acc, exists := p.accounts[stickyID]; exists && isAccountHealthy(acc, now) {
					p.stickyAt[sessionKey] = now
					return acc, nil
				}
			}
		}
	}

	// 2. Gather healthy candidate accounts
	candidates := make([]*Account, 0, len(p.accounts))
	for id, acc := range p.accounts {
		if excludedIDs[id] {
			continue
		}
		if isAccountHealthy(acc, now) {
			candidates = append(candidates, acc)
		}
	}

	if len(candidates) == 0 {
		return nil, ErrNoAvailableAccounts
	}

	// 3. Rank candidates:
	// Priority asc -> InFlight asc -> ConsecutiveFailures asc -> TotalRequests asc
	sort.Slice(candidates, func(i, j int) bool {
		c1, c2 := candidates[i], candidates[j]
		c1.mu.RLock()
		c2.mu.RLock()
		defer c1.mu.RUnlock()
		defer c2.mu.RUnlock()

		if c1.Priority != c2.Priority {
			return c1.Priority < c2.Priority
		}
		if c1.InFlight != c2.InFlight {
			return c1.InFlight < c2.InFlight
		}
		if c1.ConsecutiveFailures != c2.ConsecutiveFailures {
			return c1.ConsecutiveFailures < c2.ConsecutiveFailures
		}
		return c1.TotalRequests < c2.TotalRequests
	})

	selected := candidates[0]

	// Update sticky mapping if sessionKey provided
	if sessionKey != "" {
		if len(p.sticky) >= maxStickyEntries {
			p.evictOldestSticky(maxStickyEntries / 10)
		}
		p.sticky[sessionKey] = selected.ID
		p.stickyAt[sessionKey] = now
	}

	return selected, nil
}

func (p *AccountPool) evictOldestSticky(count int) {
	if count <= 0 {
		count = 1
	}
	type entry struct {
		key string
		at  time.Time
	}
	entries := make([]entry, 0, len(p.stickyAt))
	for k, at := range p.stickyAt {
		entries = append(entries, entry{key: k, at: at})
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].at.Before(entries[j].at)
	})
	limit := count
	if limit > len(entries) {
		limit = len(entries)
	}
	for i := 0; i < limit; i++ {
		delete(p.sticky, entries[i].key)
		delete(p.stickyAt, entries[i].key)
	}
}

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

// Acquire increments the in-flight counter for the account.
func (p *AccountPool) Acquire(accountID string) {
	p.mu.RLock()
	acc, ok := p.accounts[accountID]
	p.mu.RUnlock()

	if ok {
		acc.mu.Lock()
		acc.InFlight++
		acc.TotalRequests++
		acc.LastUsed = time.Now()
		acc.mu.Unlock()
	}
}

// Release decrements the in-flight counter for the account.
func (p *AccountPool) Release(accountID string) {
	p.mu.RLock()
	acc, ok := p.accounts[accountID]
	p.mu.RUnlock()

	if ok {
		acc.mu.Lock()
		if acc.InFlight > 0 {
			acc.InFlight--
		}
		acc.mu.Unlock()
	}
}

// RecordSuccess records a successful request outcome and updates usage/rate-limits.
func (p *AccountPool) RecordSuccess(accountID string, tokens int64, cost float64, rl RateLimits) {
	p.mu.RLock()
	acc, ok := p.accounts[accountID]
	p.mu.RUnlock()

	if ok {
		acc.mu.Lock()
		acc.ConsecutiveFailures = 0
		acc.TotalTokens += tokens
		acc.TotalCost += cost
		if !rl.LastUpdated.IsZero() {
			acc.RateLimits = rl
		}
		acc.mu.Unlock()
	}
}

// RecordRateLimit puts the account into cooldown based on rate-limit headers or default duration.
func (p *AccountPool) RecordRateLimit(accountID string, rl RateLimits, defaultCooldown time.Duration) {
	p.mu.RLock()
	acc, ok := p.accounts[accountID]
	p.mu.RUnlock()

	if ok {
		acc.mu.Lock()
		defer acc.mu.Unlock()

		acc.TotalErrors++
		if !rl.LastUpdated.IsZero() {
			acc.RateLimits = rl
		}

		now := time.Now()
		cooldown := defaultCooldown
		if rl.RetryAfter > 0 {
			cooldown = time.Duration(rl.RetryAfter) * time.Second
		} else if rl.TokensReset.After(now) {
			diff := rl.TokensReset.Sub(now)
			if diff > cooldown {
				cooldown = diff
			}
		} else if rl.RequestsReset.After(now) {
			diff := rl.RequestsReset.Sub(now)
			if diff > cooldown {
				cooldown = diff
			}
		}

		if cooldown <= 0 {
			cooldown = defaultCooldown
		}

		acc.CooldownUntil = now.Add(cooldown)
	}
}

// RecordFailure increments error counter and triggers cooldown if threshold is breached.
func (p *AccountPool) RecordFailure(accountID string, is5xx bool, defaultCooldown time.Duration) {
	p.mu.RLock()
	acc, ok := p.accounts[accountID]
	p.mu.RUnlock()

	if ok {
		acc.mu.Lock()
		defer acc.mu.Unlock()

		acc.TotalErrors++
		acc.ConsecutiveFailures++

		// If consecutive failures reach 3 or on server error, set temporary cooldown
		if acc.ConsecutiveFailures >= 3 || is5xx {
			acc.CooldownUntil = time.Now().Add(defaultCooldown)
		}
	}
}
