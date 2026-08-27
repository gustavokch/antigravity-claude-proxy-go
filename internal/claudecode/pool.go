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

// AccountPool manages a collection of Claude Code accounts with sticky routing,
// health tracking, and load balancing.
type AccountPool struct {
	mu       sync.RWMutex
	accounts map[string]*Account
	sticky   map[string]string // sessionKey -> accountID
	stickyAt map[string]time.Time
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

// AddOrUpdateAccount adds a new account or updates an existing account by ID.
func (p *AccountPool) AddOrUpdateAccount(cfg AccountConfig) *Account {
	p.mu.Lock()
	defer p.mu.Unlock()

	id := cfg.ID
	if id == "" {
		id = fmt.Sprintf("cc-acc-%d", time.Now().UnixNano())
		cfg.ID = id
	}

	existing, ok := p.accounts[id]
	if ok {
		existing.mu.Lock()
		existing.Name = cfg.Name
		existing.Token = cfg.Token
		existing.Type = cfg.Type
		existing.Priority = cfg.Priority
		existing.Enabled = cfg.Enabled
		if cfg.Source != "" {
			existing.Source = cfg.Source
		}
		existing.mu.Unlock()
		return existing
	}

	acc := &Account{
		ID:        id,
		Name:      cfg.Name,
		Token:     cfg.Token,
		Type:      cfg.Type,
		Priority:  cfg.Priority,
		Enabled:   cfg.Enabled,
		Source:    cfg.Source,
		CreatedAt: time.Now(),
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
		p.sticky[sessionKey] = selected.ID
		p.stickyAt[sessionKey] = now
	}

	return selected, nil
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
	// Check rate limits if reset is in future
	if acc.RateLimits.RequestsRemaining == 0 && acc.RateLimits.RequestsReset.After(now) {
		return false
	}
	if acc.RateLimits.TokensRemaining == 0 && acc.RateLimits.TokensReset.After(now) {
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
