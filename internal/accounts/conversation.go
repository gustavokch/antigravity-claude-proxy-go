package accounts

import (
	"context"
	"log/slog"
	"slices"
	"time"
)

const (
	pinIdleTTL = time.Hour
	maxPins    = 10000
)

// pin keeps one conversation on one account. It holds emails, not *Account,
// so a reload leaves it valid: an email no longer in the pool reads as a
// removed account.
type pin struct {
	home     string            // account email
	overflow map[string]string // rateLimitModelKey(model) -> account email
	started  map[string]bool   // account emails whose start burst ran for this session
	lastSeen time.Time
}

// targetState is what a pinned account can do for one model right now.
type targetState int

const (
	targetUsable targetState = iota
	// targetWait: a plain rate limit that lifts within manager.pinWait, with
	// no other failure since the account's last success.
	targetWait
	// targetGone: removed, disabled or invalid. The pin rebinds.
	targetGone
	// targetBlocked: anything longer, or a repeat limit. The model moves to
	// an overflow account.
	targetBlocked
)

type pinTarget struct {
	account *Account
	state   targetState
	wait    time.Duration
	reason  string
}

// selectPinnedLocked keeps a session on its pinned account. The strategy
// chooses only when the pin is created, when its account is gone, and when a
// model has to move to an overflow account. An overflow entry stays after the
// home account recovers: moving back would start another cascade.
func (manager *Manager) selectPinnedLocked(session, model string) Selection {
	now := manager.now()
	entry := manager.pins[session]
	if entry != nil && now.Sub(entry.lastSeen) > pinIdleTTL {
		delete(manager.pins, session)
		entry = nil
	}
	if entry == nil {
		selection := manager.pickLocked(model, nil)
		if selection.Account != nil {
			manager.addPinLocked(session, &pin{home: selection.Account.Email, lastSeen: now})
			logPinChange("session pinned", session, model, "", selection.Account.Email, "new")
		}
		return selection
	}
	entry.lastSeen = now
	key := rateLimitModelKey(model)
	email, overflowed := entry.overflow[key]
	if !overflowed {
		email = entry.home
	}
	target := manager.pinTargetLocked(email, model)
	switch target.state {
	case targetUsable:
		target.account.LastUsedMS = now.UnixMilli()
		return Selection{Account: target.account}
	case targetWait:
		return Selection{Wait: target.wait}
	case targetBlocked:
		selection := manager.pickLocked(model, map[string]bool{email: true})
		if selection.Account == nil {
			// Nothing else serves the model: the pin stays as it is and the
			// answer is the one a request without a session gets.
			return manager.pickLocked(model, nil)
		}
		if entry.overflow == nil {
			entry.overflow = make(map[string]string)
		}
		entry.overflow[key] = selection.Account.Email
		logPinChange("session overflow", session, model, email, selection.Account.Email, target.reason)
		return selection
	}
	selection := manager.pickLocked(model, nil)
	if selection.Account == nil {
		return selection
	}
	if overflowed {
		entry.overflow[key] = selection.Account.Email
		logPinChange("session overflow", session, model, email, selection.Account.Email, target.reason)
	} else {
		entry.home = selection.Account.Email
		logPinChange("session rebound", session, model, email, selection.Account.Email, target.reason)
	}
	return selection
}

// pinTargetLocked classifies the pinned account for model. Only a plain rate
// limit (MarkRateLimited) that lifts within manager.pinWait is worth waiting
// for, and only once: each upstream 429 adds one to ConsecutiveFailure, which
// only a success clears, so a count of two or more means the account failed
// before this limit, typically with a limit the session already waited out.
// A repeat limit, a longer one, a failure lock, an exhausted quota, the 404
// lock, a cooldown, or a model the account does not list moves the model to
// another account.
func (manager *Manager) pinTargetLocked(email, model string) pinTarget {
	account := manager.accountLocked(email)
	switch {
	case account == nil:
		return pinTarget{state: targetGone, reason: "removed"}
	case !account.Enabled:
		return pinTarget{account: account, state: targetGone, reason: "disabled"}
	case account.IsInvalid:
		return pinTarget{account: account, state: targetGone, reason: "invalid"}
	}
	poolSupports := manager.poolSupportsModelLocked(model)
	if manager.usableLocked(account, model, poolSupports) {
		return pinTarget{account: account, state: targetUsable}
	}
	nowMS := manager.now().UnixMilli()
	blocked := pinTarget{account: account, state: targetBlocked}
	limit := account.ModelRateLimits[rateLimitModelKey(model)]
	quota, _ := modelQuotaFor(account, model)
	switch {
	case account.CoolingDownUntilMS > nowMS:
		blocked.reason = "cooling down"
	case !servesModel(account, model, poolSupports):
		blocked.reason = "model not listed"
	case quota.ExhaustedUntilMS > nowMS:
		blocked.reason = "quota exhausted"
	case limit == nil:
		blocked.reason = "unusable"
	case limit.failure:
		blocked.reason = "failure lock"
	default:
		wait := time.Duration(limit.ResetTimeMS-nowMS) * time.Millisecond
		switch {
		case wait > manager.pinWait:
			blocked.reason = "rate limited " + wait.Round(time.Second).String()
		case account.ConsecutiveFailure >= 2:
			blocked.reason = "rate limited again"
		default:
			return pinTarget{account: account, state: targetWait, wait: max(wait, time.Millisecond)}
		}
	}
	return blocked
}

// setPinWait sets the longest rate limit a pinned session waits out. The
// Dispatcher passes its maxWait: it answers a longer wait with a
// RateLimitError, even when another account could serve the request. The
// bound never exceeds maxStickyWait, and a value of zero or less means
// maxStickyWait.
func (manager *Manager) setPinWait(d time.Duration) {
	if d <= 0 || d > maxStickyWait {
		d = maxStickyWait
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.pinWait = d
}

// addPinLocked stores a new pin. Idle pins are swept here, once per new
// conversation rather than on every request, and the same pass finds the
// least recently used pin, which is evicted at the cap. Every pin is stored
// here, so the map never holds more than maxPins.
func (manager *Manager) addPinLocked(session string, entry *pin) {
	if manager.pins == nil {
		manager.pins = make(map[string]*pin)
	}
	var oldest *pin
	oldestKey := ""
	for key, existing := range manager.pins {
		switch {
		case entry.lastSeen.Sub(existing.lastSeen) > pinIdleTTL:
			delete(manager.pins, key)
		case oldest == nil || existing.lastSeen.Before(oldest.lastSeen):
			oldest, oldestKey = existing, key
		}
	}
	if len(manager.pins) >= maxPins {
		delete(manager.pins, oldestKey)
	}
	manager.pins[session] = entry
}

func (manager *Manager) accountLocked(email string) *Account {
	for _, account := range manager.accounts {
		if account.Email == email {
			return account
		}
	}
	return nil
}

// conversationStarted reports whether the start burst already ran for session
// on the account email.
func (manager *Manager) conversationStarted(session, email string) bool {
	if session == "" {
		return false
	}
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	entry := manager.pins[session]
	return entry != nil && entry.started[email]
}

// markConversationStarted records a completed start burst. A session without
// a pin has nothing to record on.
func (manager *Manager) markConversationStarted(session, email string) {
	if session == "" {
		return
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	entry := manager.pins[session]
	if entry == nil {
		return
	}
	if entry.started == nil {
		entry.started = make(map[string]bool)
	}
	entry.started[email] = true
}

// ActiveSessions returns, per account email, how many live session pins use
// that account as home or as an overflow account.
func (manager *Manager) ActiveSessions() map[string]int {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	now := manager.now()
	counts := make(map[string]int)
	var counted []string
	for _, entry := range manager.pins {
		if now.Sub(entry.lastSeen) > pinIdleTTL {
			continue
		}
		counted = append(counted[:0], entry.home)
		counts[entry.home]++
		for _, email := range entry.overflow {
			if !slices.Contains(counted, email) {
				counted = append(counted, email)
				counts[email]++
			}
		}
	}
	return counts
}

func logPinChange(message, session, model, from, to, reason string) {
	slog.Info(message, "session", sessionPrefix(session), "model", model, "from", from, "to", to, "reason", reason)
}

// sessionPrefix keeps logs correlatable without writing whole session ids.
func sessionPrefix(session string) string {
	if len(session) > 8 {
		return session[:8]
	}
	return session
}

type sessionKeyContextKey struct{}

// WithSessionKey returns ctx carrying the conversation's session id, which
// Select pins to one account. An empty key leaves ctx unchanged.
func WithSessionKey(ctx context.Context, key string) context.Context {
	if key == "" {
		return ctx
	}
	return context.WithValue(ctx, sessionKeyContextKey{}, key)
}

// SessionKey returns the session id stored by WithSessionKey, or "".
func SessionKey(ctx context.Context) string {
	key, _ := ctx.Value(sessionKeyContextKey{}).(string)
	return key
}
