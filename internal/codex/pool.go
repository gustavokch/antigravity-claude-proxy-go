package codex

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	// ErrNoAvailableAccounts is returned when every account is disabled, in
	// cooldown, waiting for a new login, or excluded by the caller.
	ErrNoAvailableAccounts = errors.New("no available Codex accounts")
	// ErrAccountNotFound is returned for an account ID the pool does not hold.
	ErrAccountNotFound = errors.New("codex account not found")
	// ErrNeedsLogin marks an account whose refresh grant is gone. A refresh
	// error that ends the grant wraps it, and EnsureFresh returns it for such an
	// account without asking the token endpoint again.
	ErrNeedsLogin = errors.New("codex account needs a new login")
)

// maxStickyEntries bounds the sticky session map; past it the session used
// least recently is forgotten.
const maxStickyEntries = 10000

// refreshTimeout bounds one token refresh. The refresh runs detached from the
// request that triggered it (see refresh), so this is its only deadline.
const refreshTimeout = time.Minute

// TokenSet is the result of a token refresh. Empty fields are not known to the
// refresher; the pool keeps the previous value for them.
type TokenSet struct {
	AccessToken, RefreshToken, IDToken string
	ExpiresAt                          time.Time
	Email, AccountID, PlanType         string
}

// TokenRefresher exchanges a refresh token for a new token set.
type TokenRefresher func(ctx context.Context, refreshToken string) (TokenSet, error)

// TokenPersister writes a refreshed token set to config.json. The pool calls it
// before the new access token is used: the refresh token rotated with it, so a
// restart that found only the old one in the config would sign the account out.
type TokenPersister func(accountID string, ts TokenSet) error

// AccountPool holds the Codex accounts and picks one per request: sticky by
// session, then by priority, in-flight count and recent failures. Quota,
// cooldown and counters live only in memory; Inherit carries them across a
// rebuild.
type AccountPool struct {
	mu sync.Mutex
	// sticky maps a session key to its element in stickyOrder, which holds
	// the sessions' accounts most recently used first.
	sticky      map[string]*list.Element
	stickyOrder *list.List
	accounts    map[string]*accountState
	refreshing  map[string]*refreshCall
	refresher   TokenRefresher
	persister   TokenPersister

	// now is a test seam; nil means time.Now.
	now func() time.Time
}

// stickyEntry is one session's account in AccountPool.stickyOrder.
type stickyEntry struct{ key, id string }

type accountState struct {
	Account
	needsLogin          bool
	inFlight            int64
	totalRequests       int64
	consecutiveFailures int
	cooldownUntil       time.Time
	cooldownReason      string
	lastError           string
	quota               *Quota
}

// refreshCall is one token refresh in progress. Everyone who needs the account
// refreshed while it runs waits for it instead of starting another: refresh
// tokens are single-use, and a second exchange of the same one fails with
// refresh_token_reused and signs the account out.
type refreshCall struct {
	done chan struct{}
	from string // the refresh token being exchanged

	// Set before done is closed.
	tokens     TokenSet
	err        error
	terminal   bool
	persistErr error
}

// NewAccountPool builds a pool from the configured accounts. Accounts without
// an ID are skipped; of two accounts with the same ID the first is kept.
func NewAccountPool(cfgs []AccountConfig) *AccountPool {
	p := &AccountPool{
		accounts:    make(map[string]*accountState, len(cfgs)),
		sticky:      make(map[string]*list.Element),
		stickyOrder: list.New(),
		refreshing:  make(map[string]*refreshCall),
	}
	for _, cfg := range cfgs {
		id := strings.TrimSpace(cfg.ID)
		if id == "" || p.accounts[id] != nil {
			continue
		}
		expires, _ := time.Parse(time.RFC3339, strings.TrimSpace(cfg.ExpiresAt))
		p.accounts[id] = &accountState{Account: Account{
			ID:           id,
			Name:         cfg.Name,
			Email:        cfg.Email,
			AccountID:    cfg.AccountID,
			PlanType:     cfg.PlanType,
			Source:       cfg.Source,
			Token:        cfg.Token,
			RefreshToken: cfg.RefreshToken,
			IDToken:      cfg.IDToken,
			ExpiresAt:    expires,
			Priority:     cfg.Priority,
			Enabled:      cfg.Enabled,
		}}
	}
	return p
}

// SetTokenRefresher installs the function that exchanges refresh tokens.
func (p *AccountPool) SetTokenRefresher(r TokenRefresher) {
	p.mu.Lock()
	p.refresher = r
	p.mu.Unlock()
}

// SetTokenPersister installs the function that saves refreshed tokens.
func (p *AccountPool) SetTokenPersister(s TokenPersister) {
	p.mu.Lock()
	p.persister = s
	p.mu.Unlock()
}

func (p *AccountPool) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}

// Inherit carries the runtime state of old's accounts into the accounts of p
// with the same ID: quota, cooldown, request and failure counters, the last
// error, and the needs-login mark while the config still holds the grant that
// failed. In-flight counts stay with the pool a request acquired its account
// from, which is the pool it releases it to.
//
// Two token cases are covered as well. When old holds a later token set than
// the config p was built from (a refresh the rebuild did not see), p takes it.
// When old is refreshing an account, p waits for that refresh instead of
// starting its own with the refresh token old is already spending, and takes
// its result.
func (p *AccountPool) Inherit(old *AccountPool) {
	if old == nil || old == p {
		return
	}
	old.mu.Lock()
	prev := make(map[string]accountState, len(old.accounts))
	for id, st := range old.accounts {
		prev[id] = *st
	}
	calls := make(map[string]*refreshCall, len(old.refreshing))
	for id, call := range old.refreshing {
		calls[id] = call
	}
	old.mu.Unlock()

	p.mu.Lock()
	defer p.mu.Unlock()
	for id, st := range p.accounts {
		o, ok := prev[id]
		if !ok {
			continue
		}
		st.totalRequests = o.totalRequests
		st.consecutiveFailures = o.consecutiveFailures
		st.cooldownUntil = o.cooldownUntil
		st.cooldownReason = o.cooldownReason
		st.quota = o.quota
		if o.ExpiresAt.After(st.ExpiresAt) && o.RefreshToken != st.RefreshToken {
			st.Token, st.RefreshToken, st.IDToken, st.ExpiresAt = o.Token, o.RefreshToken, o.IDToken, o.ExpiresAt
		}
		if o.RefreshToken == st.RefreshToken {
			// The config still holds the grant old judged; a new login
			// replaces it and starts with a clean slate.
			st.needsLogin = o.needsLogin
			st.lastError = o.lastError
		}
		if call := calls[id]; call != nil && call.from == st.RefreshToken && p.refreshing[id] == nil {
			p.refreshing[id] = call
			go func() {
				<-call.done
				p.settle(id, call)
			}()
		}
	}
}

// selectable reports whether the pool may hand st out for a request. An
// account with only a refresh token is selectable: EnsureFresh mints its access
// token.
func (st *accountState) selectable(now time.Time) bool {
	return st.Enabled && !st.needsLogin && !st.cooldownUntil.After(now) &&
		(st.Token != "" || st.RefreshToken != "")
}

// SelectAccount picks an account for a request. A session that already has an
// account keeps it while it stays selectable and not excluded; otherwise the
// selectable account with the lowest priority number wins, ties going to fewer
// requests in flight, then fewer consecutive failures, then fewer requests in
// total. The choice becomes the session's sticky account.
func (p *AccountPool) SelectAccount(sessionKey string, excluded map[string]bool) (Account, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.clock()

	if sessionKey != "" {
		if el, ok := p.sticky[sessionKey]; ok {
			id := el.Value.(*stickyEntry).id
			if st := p.accounts[id]; st != nil && !excluded[id] && st.selectable(now) {
				p.stickyOrder.MoveToFront(el)
				return st.Account, nil
			}
		}
	}

	candidates := make([]*accountState, 0, len(p.accounts))
	for id, st := range p.accounts {
		if !excluded[id] && st.selectable(now) {
			candidates = append(candidates, st)
		}
	}
	if len(candidates) == 0 {
		return Account{}, ErrNoAvailableAccounts
	}
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		if a.inFlight != b.inFlight {
			return a.inFlight < b.inFlight
		}
		if a.consecutiveFailures != b.consecutiveFailures {
			return a.consecutiveFailures < b.consecutiveFailures
		}
		if a.totalRequests != b.totalRequests {
			return a.totalRequests < b.totalRequests
		}
		return a.ID < b.ID
	})
	selected := candidates[0]
	if sessionKey != "" {
		p.pin(sessionKey, selected.ID)
	}
	return selected.Account, nil
}

// pin makes id the session's account and the session the most recently used.
// Past maxStickyEntries the least recently used session is forgotten. The
// caller holds p.mu.
func (p *AccountPool) pin(key, id string) {
	if el, ok := p.sticky[key]; ok {
		el.Value.(*stickyEntry).id = id
		p.stickyOrder.MoveToFront(el)
		return
	}
	p.sticky[key] = p.stickyOrder.PushFront(&stickyEntry{key, id})
	if p.stickyOrder.Len() > maxStickyEntries {
		oldest := p.stickyOrder.Back()
		p.stickyOrder.Remove(oldest)
		delete(p.sticky, oldest.Value.(*stickyEntry).key)
	}
}

// EnsureFresh returns the account with an access token that has more than
// RefreshLead left, refreshing it first when it has less (or none). A refresh
// that fails without ending the grant still returns the current token while it
// has not expired, so a token endpoint hiccup a day before expiry costs no
// request. An account whose grant is gone returns ErrNeedsLogin.
func (p *AccountPool) EnsureFresh(ctx context.Context, id string) (Account, error) {
	return p.refresh(ctx, id, false)
}

// ForceRefresh exchanges the account's refresh token now, whatever its access
// token's expiry: after an upstream 401, or on demand from the management API.
// It joins a refresh already in progress rather than starting a second one.
func (p *AccountPool) ForceRefresh(ctx context.Context, id string) (Account, error) {
	return p.refresh(ctx, id, true)
}

func (p *AccountPool) refresh(ctx context.Context, id string, force bool) (Account, error) {
	p.mu.Lock()
	st, ok := p.accounts[id]
	if !ok {
		p.mu.Unlock()
		return Account{}, ErrAccountNotFound
	}
	if !force {
		if st.needsLogin {
			p.mu.Unlock()
			return Account{}, fmt.Errorf("codex account %s: %w", id, ErrNeedsLogin)
		}
		if st.Token != "" && (st.ExpiresAt.IsZero() || st.ExpiresAt.Sub(p.clock()) > RefreshLead) {
			acc := st.Account
			p.mu.Unlock()
			return acc, nil
		}
	}
	call := p.refreshing[id]
	if call == nil {
		call = &refreshCall{done: make(chan struct{}), from: st.RefreshToken}
		p.refreshing[id] = call
		// The exchange runs on a context detached from the caller's: the
		// token endpoint may already have rotated the refresh token when the
		// caller gives up, and abandoning the reply would lose the only copy
		// of its successor.
		go p.runRefresh(context.WithoutCancel(ctx), id, call, p.refresher, p.persister)
	}
	p.mu.Unlock()

	select {
	case <-call.done:
	case <-ctx.Done():
		return Account{}, ctx.Err()
	}
	p.settle(id, call)

	p.mu.Lock()
	defer p.mu.Unlock()
	st, ok = p.accounts[id]
	if !ok {
		return Account{}, ErrAccountNotFound
	}
	if call.err != nil {
		if !force && !call.terminal && st.Token != "" && (st.ExpiresAt.IsZero() || st.ExpiresAt.After(p.clock())) {
			slog.Warn("codex: token refresh failed; using the current token until it expires",
				"account", id, "expiresAt", st.ExpiresAt, "error", call.err)
			return st.Account, nil
		}
		return Account{}, call.err
	}
	return st.Account, nil
}

// runRefresh exchanges call.from and saves the result. It records the outcome
// on call and settles it on p before waking the waiters, so they read the new
// tokens from the pool.
func (p *AccountPool) runRefresh(ctx context.Context, id string, call *refreshCall, refresher TokenRefresher, persister TokenPersister) {
	defer close(call.done)
	defer p.settle(id, call)

	switch {
	case refresher == nil:
		call.err = fmt.Errorf("codex account %s: no token refresher configured", id)
		return
	case call.from == "":
		call.err = fmt.Errorf("codex account %s has no refresh token: %w", id, ErrNeedsLogin)
		call.terminal = true
		return
	}

	ctx, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()
	ts, err := refresher(ctx, call.from)
	if err == nil && ts.AccessToken == "" {
		err = errors.New("token refresh returned no access token")
	}
	if err != nil {
		call.terminal = isGrantGone(err)
		if call.terminal {
			call.err = fmt.Errorf("codex account %s: %w: %w", id, ErrNeedsLogin, err)
		} else {
			call.err = fmt.Errorf("codex account %s: token refresh: %w", id, err)
		}
		return
	}

	p.mu.Lock()
	if st := p.accounts[id]; st != nil {
		ts = mergeTokens(st.Account, ts)
	}
	p.mu.Unlock()
	call.tokens = ts
	if persister != nil {
		if err := persister(id, ts); err != nil {
			// The new tokens are kept in memory anyway: the old refresh token
			// is spent, so dropping its successor would sign the account out
			// now instead of at the next restart.
			call.persistErr = err
			slog.Error("codex: refreshed token not saved; a restart will need a new login",
				"account", id, "error", err)
		}
	}
}

// mergeTokens fills the fields the refresher left empty from the account.
func mergeTokens(acc Account, ts TokenSet) TokenSet {
	keep := func(next, prev string) string {
		if next == "" {
			return prev
		}
		return next
	}
	ts.RefreshToken = keep(ts.RefreshToken, acc.RefreshToken)
	ts.IDToken = keep(ts.IDToken, acc.IDToken)
	ts.Email = keep(ts.Email, acc.Email)
	ts.AccountID = keep(ts.AccountID, acc.AccountID)
	ts.PlanType = keep(ts.PlanType, acc.PlanType)
	return ts
}

// settle applies a finished refresh to p, once: whoever gets here first (the
// refreshing goroutine, or a waiter of a refresh inherited from another pool)
// applies it, later calls find it gone. A result is dropped when the account
// has meanwhile been given a different refresh token, by a new login.
func (p *AccountPool) settle(id string, call *refreshCall) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.refreshing[id] != call {
		return
	}
	delete(p.refreshing, id)
	st := p.accounts[id]
	if st == nil || st.RefreshToken != call.from {
		return
	}
	if call.err != nil {
		st.lastError = call.err.Error()
		if call.terminal {
			st.needsLogin = true
		}
		return
	}
	ts := call.tokens
	st.Token, st.RefreshToken, st.IDToken, st.ExpiresAt = ts.AccessToken, ts.RefreshToken, ts.IDToken, ts.ExpiresAt
	st.Email, st.AccountID, st.PlanType = ts.Email, ts.AccountID, ts.PlanType
	st.needsLogin = false
	st.lastError = ""
	if call.persistErr != nil {
		st.lastError = "refreshed token not saved: " + call.persistErr.Error()
	}
}

// grantGoneMarkers are the OAuth error codes that mean the refresh token can
// never be exchanged again.
var grantGoneMarkers = []string{
	"invalid_grant",
	"refresh_token_reused",
	"refresh_token_expired",
	"refresh_token_invalidated",
	"token_revoked",
	"(status 401)",
}

// isGrantGone reports whether a refresh error ends the account's grant. The
// refresher reports a non-2xx reply as "token refresh failed (status N): body",
// so the status and the OAuth error code are both in the text.
func isGrantGone(err error) bool {
	if errors.Is(err, ErrNeedsLogin) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range grantGoneMarkers {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// Acquire counts a request starting on the account.
func (p *AccountPool) Acquire(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if st := p.accounts[id]; st != nil {
		st.inFlight++
		st.totalRequests++
	}
}

// Release counts a request on the account as finished.
func (p *AccountPool) Release(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if st := p.accounts[id]; st != nil && st.inFlight > 0 {
		st.inFlight--
	}
}

// RecordSuccess clears the account's failure streak.
func (p *AccountPool) RecordSuccess(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if st := p.accounts[id]; st != nil {
		st.consecutiveFailures = 0
	}
}

// RecordFailure counts a failed request. A server error, or a third failure in
// a row, puts the account in cooldown; a cooldown already running for longer is
// kept.
func (p *AccountPool) RecordFailure(id string, is5xx bool, cooldown time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.accounts[id]
	if st == nil {
		return
	}
	st.consecutiveFailures++
	if is5xx || st.consecutiveFailures >= 3 {
		reason := "consecutive_failures"
		if is5xx {
			reason = "server_error"
		}
		st.extendCooldown(p.clock().Add(cooldown), reason)
	}
}

// RecordCooldown keeps the account out of selection until the given time: the
// reset of a spent usage window, or a Retry-After. A cooldown already running
// for longer is kept.
func (p *AccountPool) RecordCooldown(id string, until time.Time, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if st := p.accounts[id]; st != nil {
		st.extendCooldown(until, reason)
	}
}

func (st *accountState) extendCooldown(until time.Time, reason string) {
	if until.After(st.cooldownUntil) {
		st.cooldownUntil = until
		st.cooldownReason = reason
	}
}

// CooldownEnd returns when the first account comes back from its cooldown,
// when cooldowns are all that keep every account out: no account is
// selectable, and an enabled, signed-in account with a credential is cooling
// down. ok is false otherwise, for a pool with nothing to wait for.
func (p *AccountPool) CooldownEnd() (until time.Time, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.clock()
	for _, st := range p.accounts {
		if st.selectable(now) {
			return time.Time{}, false
		}
		// Not selectable, yet enabled, signed in and holding a credential:
		// only its cooldown keeps it out.
		if st.Enabled && !st.needsLogin && (st.Token != "" || st.RefreshToken != "") &&
			(until.IsZero() || st.cooldownUntil.Before(until)) {
			until = st.cooldownUntil
		}
	}
	return until, !until.IsZero()
}

// UpdateQuota stores the account's latest usage reading.
func (p *AccountPool) UpdateQuota(id string, q Quota) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if st := p.accounts[id]; st != nil {
		st.quota = &q
	}
}

// GetAccount returns a copy of one account.
func (p *AccountPool) GetAccount(id string) (Account, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st, ok := p.accounts[id]
	if !ok {
		return Account{}, false
	}
	return st.Account, true
}

// Snapshots returns every account's management view, by priority, then name,
// then ID.
func (p *AccountPool) Snapshots() []AccountSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.clock()
	snaps := make([]AccountSnapshot, 0, len(p.accounts))
	for _, st := range p.accounts {
		snaps = append(snaps, st.snapshot(now))
	}
	sort.Slice(snaps, func(i, j int) bool {
		a, b := snaps[i], snaps[j]
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.ID < b.ID
	})
	return snaps
}

func (st *accountState) snapshot(now time.Time) AccountSnapshot {
	snap := AccountSnapshot{
		ID:                  st.ID,
		Name:                st.Name,
		Email:               st.Email,
		AccountID:           st.AccountID,
		PlanType:            st.PlanType,
		Enabled:             st.Enabled,
		Priority:            st.Priority,
		Source:              st.Source,
		Status:              "healthy",
		ExpiresAt:           st.ExpiresAt,
		InFlight:            st.inFlight,
		TotalRequests:       st.totalRequests,
		ConsecutiveFailures: st.consecutiveFailures,
		LastError:           st.lastError,
	}
	if st.quota != nil {
		q := *st.quota
		snap.Quota = &q
	}
	if st.cooldownUntil.After(now) {
		snap.CooldownUntil = st.cooldownUntil
		snap.CooldownReason = st.cooldownReason
	}
	switch {
	case !st.Enabled:
		snap.Status = "disabled"
	case st.needsLogin:
		snap.Status = "needs_login"
	case st.cooldownUntil.After(now):
		snap.Status = "cooldown"
	}
	return snap
}
