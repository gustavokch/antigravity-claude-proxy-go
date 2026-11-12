package accounts

import (
	"context"
	"errors"
	"sync"
	"time"
)

var errRecoveryWindow = errors.New("hidden recovery window exhausted")

// recoveryWindow is armed only after a failure or blocked selection. Its timer
// and first-event boundary share a lock: either expiry wins, or the stream owns
// its normal lifetime before the consumer sees the event.
type recoveryWindow struct {
	ctx          context.Context
	cancel       context.CancelCauseFunc
	stopLifetime func() bool
	timer        *time.Timer
	deadline     time.Time
	limit        time.Duration
	now          func() time.Time
	mu           sync.Mutex
	ended        bool
}

func (window *recoveryWindow) arm(lifetime context.Context) {
	if window.cancel != nil {
		return
	}
	window.ctx, window.cancel = context.WithCancelCause(window.ctx)
	window.deadline = window.now().Add(window.limit)
	window.stopLifetime = context.AfterFunc(lifetime, func() { window.cancel(errDispatcherClosed) })
	window.timer = time.AfterFunc(window.limit, func() {
		window.mu.Lock()
		defer window.mu.Unlock()
		if !window.ended {
			window.cancel(errRecoveryWindow)
		}
	})
}

func (window *recoveryWindow) sleep(delay time.Duration, sleep SleepFunc) error {
	if err := context.Cause(window.ctx); err != nil {
		return err
	}
	if window.cancel != nil && delay >= window.deadline.Sub(window.now()) {
		return errRecoveryWindow
	}
	if err := sleep(window.ctx, delay); err != nil {
		if cause := context.Cause(window.ctx); cause != nil {
			return cause
		}
		return err
	}
	return context.Cause(window.ctx)
}

func (window *recoveryWindow) firstEvent() error {
	if window.cancel == nil {
		return nil
	}
	window.mu.Lock()
	defer window.mu.Unlock()
	if cause := context.Cause(window.ctx); cause != nil {
		return cause
	}
	window.ended = true
	window.timer.Stop()
	return nil
}

func (window *recoveryWindow) close() {
	if window.cancel != nil {
		window.mu.Lock()
		window.ended = true
		window.timer.Stop()
		window.mu.Unlock()
		window.stopLifetime()
		window.cancel(context.Canceled)
	}
}

func (manager *Manager) recoveryWait(account *Account, model string) time.Duration {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	now := manager.now().UnixMilli()
	wait := manager.sharedThrottleWaitLocked(rateLimitModelKey(model))
	if limit := account.ModelRateLimits[rateLimitModelKey(model)]; limit != nil && limit.IsRateLimited {
		wait = max(wait, time.Duration(limit.ResetTimeMS-now)*time.Millisecond)
	}
	return max(wait, time.Duration(account.CoolingDownUntilMS-now)*time.Millisecond)
}

// retryEligibility reads the selection policy without spending a token,
// advancing rotation, or moving a conversation's pin.
func (manager *Manager) retryEligibility(session, model string) (time.Duration, bool) {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	key := rateLimitModelKey(model)
	if wait := manager.sharedThrottleWaitLocked(key); wait > 0 {
		return wait, true
	}
	if entry := manager.pins[session]; entry != nil && manager.now().Sub(entry.lastSeen) <= pinIdleTTL {
		email := entry.home
		if overflow := entry.overflow[key]; overflow != "" {
			email = overflow
		}
		if target := manager.pinTargetLocked(email, model); target.state == targetWait {
			return target.wait, false
		}
	}
	poolSupports := manager.poolSupportsModelLocked(model)
	var minimum time.Duration
	now := manager.now().UnixMilli()
	for _, account := range manager.accounts {
		if manager.usableLocked(account, model, poolSupports) {
			return 0, false
		}
		if !account.Enabled || account.IsInvalid || !servesModel(account, model, poolSupports) {
			continue
		}
		wait := max(time.Duration(account.CoolingDownUntilMS-now)*time.Millisecond, 0)
		if limit := account.ModelRateLimits[key]; limit != nil && limit.IsRateLimited {
			wait = max(wait, time.Duration(limit.ResetTimeMS-now)*time.Millisecond)
		}
		if wait > 0 && (minimum == 0 || wait < minimum) {
			minimum = wait
		}
	}
	if manager.strategy == StrategySticky && manager.currentIndex >= 0 && manager.currentIndex < len(manager.accounts) {
		current := manager.accounts[manager.currentIndex]
		if limit := current.ModelRateLimits[key]; current.Enabled && !current.IsInvalid && limit != nil {
			if wait := time.Duration(limit.ResetTimeMS-now) * time.Millisecond; wait > 0 && wait <= maxStickyWait {
				return wait, false
			}
		}
	}
	return minimum, false
}
