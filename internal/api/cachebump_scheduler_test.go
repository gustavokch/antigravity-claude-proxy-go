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
