package api

import (
	"context"
	"testing"
	"time"

	"antigravity-go-proxy/internal/config"
)

// Regression for PR #64: cmd/proxy called StartCacheBumpScheduler
// synchronously during startup, so the main goroutine parked in the
// scheduler's loop forever and ListenAndServe never ran — the proxy bound
// no port ("starting the proxy hangs, connection refused").
//
// The contract must match StartClaudeCodeBackgroundWorker: the loop runs in
// an internally spawned goroutine and the call returns immediately.
func TestStartCacheBumpSchedulerDoesNotBlockCaller(t *testing.T) {
	original := config.Get()
	defer config.SetForTest(original)

	// Pin the enabled path: a disabled feature would skip every tick, so the
	// blocking loop this guards against is only reachable with bumps on.
	cfg := original
	cfg.CacheBump.Enabled = true
	config.SetForTest(cfg)

	server := &Server{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	returned := make(chan struct{})
	go func() {
		server.StartCacheBumpScheduler(ctx)
		close(returned)
	}()

	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("StartCacheBumpScheduler blocked the caller; startup would never reach ListenAndServe")
	}
}

// A disabled feature must cost nothing at startup: the loop resolves the
// store and scheduler per tick, after the Enabled check, so the WebUI switch
// also decides whether they are ever built. Resolving them once when the
// goroutine starts would allocate a store the proxy never uses and freeze the
// scheduler knobs at their startup values.
func TestStartCacheBumpSchedulerAllocatesNothingWhileDisabled(t *testing.T) {
	original := config.Get()
	defer config.SetForTest(original)

	cfg := original
	cfg.CacheBump.Enabled = false
	config.SetForTest(cfg)

	server := &Server{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	server.StartCacheBumpScheduler(ctx)

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		server.mu.Lock()
		store := server.cacheBumpStore
		server.mu.Unlock()
		if store != nil {
			t.Fatal("scheduler built the cache bump store while the feature is disabled")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
