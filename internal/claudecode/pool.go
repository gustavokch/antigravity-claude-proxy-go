package claudecode

import (
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
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

// defaultUnifiedSaveInterval is the least time between two store saves
// caused by unified snapshot changes on one account. A status change to or
// from "rejected" is always saved at once.
const defaultUnifiedSaveInterval = 30 * time.Second

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

	// now and persist are test seams; nil means time.Now and
	// SaveStoredAccounts. unifiedSaveInterval zero means the default.
	now                 func() time.Time
	persist             func() error
	unifiedSaveInterval time.Duration

	// saveMu guards the background save state. At most one save goroutine
	// runs at a time; savePending asks it for one more pass.
	saveMu      sync.Mutex
	saving      bool
	savePending bool
	retired     bool
	saveWG      sync.WaitGroup
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

	now := p.clock()
	for _, cfg := range stored {
		acc := p.AddOrUpdateAccount(cfg)
		restoreAccountUnified(acc, cfg.Unified, now)
	}
	return nil
}

// RestoreStoredUnified loads the persisted unified snapshots into the
// accounts already in the pool. Unlike LoadStoredAccounts it never adds an
// account, so a pool built from config picks up its last known subscription
// limits without resurrecting accounts the config no longer lists.
func (p *AccountPool) RestoreStoredUnified() error {
	p.mu.RLock()
	path := p.storagePath
	p.mu.RUnlock()

	stored, err := LoadStoredAccounts(path)
	if err != nil {
		return err
	}

	now := p.clock()
	for _, cfg := range stored {
		if cfg.Unified == nil {
			continue
		}
		p.mu.RLock()
		acc := p.accounts[cfg.ID]
		p.mu.RUnlock()
		if acc != nil {
			restoreAccountUnified(acc, cfg.Unified, now)
		}
	}
	return nil
}

// restoreAccountUnified installs a persisted snapshot on acc unless the
// account already holds unified data, which is newer than anything stored.
func restoreAccountUnified(acc *Account, u *Unified, now time.Time) {
	restored := restoreUnified(u, now)
	if restored == nil {
		return
	}
	acc.mu.Lock()
	defer acc.mu.Unlock()
	if acc.RateLimits.Unified != nil {
		return
	}
	acc.RateLimits.Unified = restored
	if acc.RateLimits.LastUpdated.IsZero() {
		acc.RateLimits.LastUpdated = restored.ObservedAt
	}
}

// persistentSource reports whether accounts from source are written to the
// account store.
func persistentSource(source string) bool {
	return source == "oauth" || source == "manual"
}

// SaveStoredAccounts persists all accounts marked as persistent (source "oauth" or "manual") to disk.
// It is a no-op on a retired pool, and Retire waits for a call already
// writing, so a replaced pool cannot overwrite the new pool's account set
// (for example after a token refresh on a request still holding it).
func (p *AccountPool) SaveStoredAccounts() error {
	p.saveMu.Lock()
	if p.retired {
		p.saveMu.Unlock()
		return nil
	}
	p.saveWG.Add(1)
	p.saveMu.Unlock()
	defer p.saveWG.Done()
	return p.writeStore()
}

// writeStore writes the persistent accounts to the store. Callers must
// have checked retirement and be counted in saveWG.
func (p *AccountPool) writeStore() error {
	p.mu.RLock()
	path := p.storagePath
	if path == "" {
		path = DefaultStoragePath()
	}

	var toSave []AccountConfig
	for _, acc := range p.accounts {
		acc.mu.RLock()
		// Only persist dynamic accounts (e.g. oauth, manual)
		if persistentSource(acc.Source) {
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
				// Shared, never mutated, so safe to marshal unlocked.
				Unified: acc.RateLimits.Unified,
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
// Empty extractions (no limit headers) are ignored so a headerless 200
// cannot clobber the last good reading back to a false-full 1.0.
func (p *AccountPool) UpdateAccountRateLimits(accountID string, rl RateLimits) {
	p.mu.RLock()
	acc, ok := p.accounts[accountID]
	p.mu.RUnlock()

	if ok && acc != nil && (rl.HasLimits() || rl.RetryAfter > 0) {
		acc.mu.Lock()
		save := p.setRateLimitsLocked(acc, rl, p.clock())
		acc.mu.Unlock()
		if save {
			p.requestSave()
		}
	}
}

// setRateLimitsLocked stores rl on acc, carrying over a live unified
// snapshot, and reports whether the account store should be saved because
// the unified snapshot changed. Saves are throttled per account to one per
// unifiedSaveInterval, except that a status change to or from "rejected" is
// always saved. acc.mu must be held for writing.
func (p *AccountPool) setRateLimitsLocked(acc *Account, rl RateLimits, now time.Time) bool {
	prev := acc.RateLimits.Unified
	acc.RateLimits = mergeRateLimits(acc.RateLimits, rl, now)
	next := acc.RateLimits.Unified
	if next == nil || next == prev || !persistentSource(acc.Source) {
		return false
	}
	flipped := unifiedRejected(prev) != unifiedRejected(next)
	if !flipped && !acc.unifiedSavedAt.IsZero() && now.Sub(acc.unifiedSavedAt) < p.saveInterval() {
		return false
	}
	acc.unifiedSavedAt = now
	return true
}

func unifiedRejected(u *Unified) bool {
	return u != nil && strings.EqualFold(u.Status, "rejected")
}

func (p *AccountPool) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

func (p *AccountPool) saveInterval() time.Duration {
	if p.unifiedSaveInterval > 0 {
		return p.unifiedSaveInterval
	}
	return defaultUnifiedSaveInterval
}

// requestSave saves the account store in the background so the request
// that changed the snapshot never waits on disk. Requests made while a save
// is running collapse into one more pass, which reads the state current at
// that time. Nothing is saved unless a storage path was configured, and the
// goroutine exits as soon as no pass is pending.
func (p *AccountPool) requestSave() {
	p.mu.RLock()
	path := p.storagePath
	p.mu.RUnlock()
	if path == "" {
		return
	}

	p.saveMu.Lock()
	if p.retired {
		p.saveMu.Unlock()
		return
	}
	p.savePending = true
	if p.saving {
		p.saveMu.Unlock()
		return
	}
	p.saving = true
	p.saveWG.Add(1)
	p.saveMu.Unlock()

	go p.saveLoop()
}

func (p *AccountPool) saveLoop() {
	defer p.saveWG.Done()
	for {
		p.saveMu.Lock()
		if !p.savePending || p.retired {
			p.savePending = false
			p.saving = false
			p.saveMu.Unlock()
			return
		}
		p.savePending = false
		p.saveMu.Unlock()

		persist := p.persist
		if persist == nil {
			// saveLoop is already counted in saveWG and checked
			// retirement above, so write directly.
			persist = p.writeStore
		}
		if err := persist(); err != nil {
			slog.Warn("claudecode: failed to persist unified limit snapshot", "error", err)
		}
	}
}

// Retire stops p from saving the account store: background saves and
// direct SaveStoredAccounts calls (including those made after a token
// refresh) become no-ops, a queued background pass is dropped, and Retire
// waits for any write already in progress to finish. Call it on a pool that
// is being replaced, before the replacement can save, so the old pool's
// account set can never overwrite the new one on disk. Later changes to a
// retired pool, refreshed tokens included, are kept in memory only. Retire
// is idempotent.
func (p *AccountPool) Retire() {
	p.saveMu.Lock()
	p.retired = true
	p.savePending = false
	p.saveMu.Unlock()
	p.saveWG.Wait()
}

// InheritUnified copies each account's unified snapshot from old into the
// account with the same ID in p, where p's account holds none yet. The
// *Unified is shared, never mutated, so a pointer copy is safe. Use it when
// a pool is rebuilt so a config change does not blank the live
// subscription windows of accounts that survive it.
func (p *AccountPool) InheritUnified(old *AccountPool) {
	if old == nil || old == p {
		return
	}
	for _, prev := range old.ListAccounts() {
		prev.mu.RLock()
		u := prev.RateLimits.Unified
		prev.mu.RUnlock()
		if u == nil {
			continue
		}
		p.mu.RLock()
		acc := p.accounts[prev.ID]
		p.mu.RUnlock()
		if acc == nil {
			continue
		}
		acc.mu.Lock()
		if acc.RateLimits.Unified == nil {
			acc.RateLimits.Unified = u
			if acc.RateLimits.LastUpdated.IsZero() {
				acc.RateLimits.LastUpdated = u.ObservedAt
			}
		}
		acc.mu.Unlock()
	}
}

// waitSaves blocks until no background save is running. Tests use it.
func (p *AccountPool) waitSaves() {
	p.saveWG.Wait()
}

// mergeRateLimits returns next, carrying over prev's unified snapshot when
// next has no unified data and prev's still describes a live window. A
// response with only classic headers must not wipe the subscription windows;
// only fresh unified headers replace them. The *Unified is shared with prev
// (and any snapshots taken from it), so it is carried as-is, never mutated.
func mergeRateLimits(prev, next RateLimits, now time.Time) RateLimits {
	if next.Unified == nil && unifiedLive(prev.Unified, now) {
		next.Unified = prev.Unified
	}
	return next
}

// unifiedLive reports whether u has at least one reset (5h, 7d or overall)
// still in the future.
func unifiedLive(u *Unified, now time.Time) bool {
	if u == nil {
		return false
	}
	return u.FiveHour.Reset.After(now) || u.SevenDay.Reset.After(now) || u.Reset.After(now)
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
		save := false
		if rl.HasLimits() || rl.RetryAfter > 0 {
			save = p.setRateLimitsLocked(acc, rl, p.clock())
		}
		acc.mu.Unlock()
		if save {
			p.requestSave()
		}
	}
}

// RecordRateLimit puts the account into cooldown based on rate-limit headers or default duration.
func (p *AccountPool) RecordRateLimit(accountID string, rl RateLimits, defaultCooldown time.Duration) {
	p.mu.RLock()
	acc, ok := p.accounts[accountID]
	p.mu.RUnlock()

	if ok {
		// Registered first so it runs after the unlock below.
		save := false
		defer func() {
			if save {
				p.requestSave()
			}
		}()
		acc.mu.Lock()
		defer acc.mu.Unlock()

		now := p.clock()
		acc.TotalErrors++
		if rl.HasLimits() || rl.RetryAfter > 0 {
			save = p.setRateLimitsLocked(acc, rl, now)
		}

		cooldown := defaultCooldown
		if u := rl.Unified; u != nil && strings.EqualFold(u.Status, "rejected") && u.BindingReset().After(now) {
			// A rejected subscription window stays closed until its reset,
			// often hours away; retrying every few seconds only burns
			// requests. A longer retry-after still wins.
			cooldown = u.BindingReset().Sub(now)
			if ra := time.Duration(rl.RetryAfter) * time.Second; ra > cooldown {
				cooldown = ra
			}
		} else if rl.RetryAfter > 0 {
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

		// A shorter 429 must not cut an existing longer cooldown short.
		if until := now.Add(cooldown); until.After(acc.CooldownUntil) {
			acc.CooldownUntil = until
		}
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
		// Only ever extend: a 5xx must not cut a long unified cooldown short.
		if acc.ConsecutiveFailures >= 3 || is5xx {
			if until := p.clock().Add(defaultCooldown); until.After(acc.CooldownUntil) {
				acc.CooldownUntil = until
			}
		}
	}
}
