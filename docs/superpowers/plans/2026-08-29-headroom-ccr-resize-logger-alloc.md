# Headroom CCR Store Resizing and Zero-Allocation Request Logging Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix the remaining headroom decoupling issues: dynamically resize the in-memory CCR store on config reload via `applyHeadroomConfig`, and eliminate per-request child logger allocations by passing `request_id` as an inline structured attribute.

**Architecture:** `CCRStore` gains thread-safe `SetMaxMB`, `SetMaxBytes`, and `MaxBytes` methods that evict LRU entries when capacity shrinks; `Server.applyHeadroomConfig` is updated to invoke `SetMaxMB`. For logging, `RequestContext` carries `RequestID uint64` while holding the shared base `*slog.Logger` (`module=headroom`), and stages emit `"request_id", reqCtx.RequestID` as an inline attribute inside debug guards, avoiding child logger cloning on the hot request path.

**Tech Stack:** Go 1.24+, `log/slog`, `container/list`, `sync`, standard library testing / `testing.AllocsPerRun`.

**Spec:** `docs/superpowers/specs/2026-08-29-headroom-decoupling-observability-design.md` and `docs/superpowers/plans/2026-08-29-pr44-headroom-stage-remediation.md`.

## Global Constraints

- ASD-STE100 Simplified Technical English.
- No new external dependencies.
- Zero allocations on the disabled logging path (`DEBUG` off).
- Strict backward compatibility for `CCRStore`, `Engine`, and `RequestContext`.
- All tests must pass: `go test -v ./internal/...`.

---

### Task 1: CCRStore Dynamic Resizing and Server Config Propagation

**Files:**
- Modify: `internal/headroom/stages/ccr/store.go`
- Modify: `internal/api/server.go`
- Test: `internal/headroom/stages/ccr/store_test.go`
- Test: `internal/api/server_test.go`
- Test: `internal/api/management_test.go`

**Interfaces:**
- Consumes: `CCRStore`, `config.HeadroomConfig`, `Server.ccrStore`
- Produces:
  - `CCRStore.SetMaxBytes(maxBytes int64)`
  - `CCRStore.SetMaxMB(maxMB int)`
  - `CCRStore.MaxBytes() int64`
  - `Server.applyHeadroomConfig(cfg config.HeadroomConfig)` (now resizing `server.ccrStore`)

- [ ] **Step 1: Write failing tests for CCRStore resizing and server config application**

Add in `internal/headroom/stages/ccr/store_test.go`:
```go
func TestCCRStore_SetMaxBytesShrinksAndEvictsOldest(t *testing.T) {
	store := NewCCRStore(1000)

	id1, ok1 := store.Put("payload_one_1234567890") // 22 bytes
	id2, ok2 := store.Put("payload_two_1234567890") // 22 bytes
	id3, ok3 := store.Put("payload_three_1234567890") // 24 bytes
	if !ok1 || !ok2 || !ok3 {
		t.Fatalf("failed initial puts")
	}
	if store.Size() != 3 {
		t.Fatalf("expected 3 items, got %d", store.Size())
	}

	// Shrink capacity so only the newest 1-2 items fit (e.g. 50 bytes)
	store.SetMaxBytes(50)

	if store.MaxBytes() != 50 {
		t.Fatalf("expected maxBytes 50, got %d", store.MaxBytes())
	}
	if store.Bytes() > 50 {
		t.Fatalf("expected store bytes <= 50, got %d", store.Bytes())
	}
	// id1 (oldest) should have been evicted
	if _, found := store.Get(id1); found {
		t.Errorf("expected id1 to be evicted after shrink")
	}
	if _, found := store.Get(id3); !found {
		t.Errorf("expected newest item id3 to remain")
	}
}

func TestCCRStore_SetMaxMBZeroOrNegativeUsesDefault(t *testing.T) {
	store := NewCCRStore(100)
	store.SetMaxMB(0)
	expectedBytes := int64(defaultMaxStoreMB) * 1024 * 1024
	if store.MaxBytes() != expectedBytes {
		t.Fatalf("expected %d bytes, got %d", expectedBytes, store.MaxBytes())
	}
}
```

Add in `internal/api/server_test.go`:
```go
func TestServer_ApplyHeadroomConfigResizesCCRStore(t *testing.T) {
	srv := &Server{
		ccrStore: ccr.NewCCRStoreFromMB(10),
	}
	srv.applyHeadroomConfig(config.HeadroomConfig{
		CCR: headroom.CCRConfig{
			MaxStoreMB: 25,
		},
	})
	if got := srv.ccrStore.MaxBytes(); got != int64(25)*1024*1024 {
		t.Fatalf("expected CCRStore maxBytes to be 25MB (%d), got %d", int64(25)*1024*1024, got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -v ./internal/headroom/stages/ccr -run "TestCCRStore_SetMax"`
Expected: FAIL with compilation error (SetMaxBytes / SetMaxMB / MaxBytes undefined)

- [ ] **Step 3: Implement `SetMaxBytes`, `SetMaxMB`, and `MaxBytes` on `CCRStore` and update `applyHeadroomConfig`**

In `internal/headroom/stages/ccr/store.go`:
```go
// SetMaxBytes dynamically resizes the store capacity in bytes and evicts LRU entries if currentBytes exceeds maxBytes.
func (s *CCRStore) SetMaxBytes(maxBytes int64) {
	if maxBytes <= 0 {
		maxBytes = int64(defaultMaxStoreMB) * 1024 * 1024
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	s.maxBytes = maxBytes
	for s.currentBytes > s.maxBytes && s.ll.Len() > 0 {
		s.evictOldestLocked()
	}
}

// SetMaxMB dynamically resizes the store capacity in megabytes.
func (s *CCRStore) SetMaxMB(maxMB int) {
	if maxMB <= 0 {
		maxMB = defaultMaxStoreMB
	}
	s.SetMaxBytes(int64(maxMB) * 1024 * 1024)
}

// MaxBytes returns the current maximum capacity in bytes.
func (s *CCRStore) MaxBytes() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maxBytes
}
```

In `internal/api/server.go:176`:
```go
func (server *Server) applyHeadroomConfig(cfg config.HeadroomConfig) {
	if server.headroom != nil {
		server.headroom.UpdateConfig(cfg)
	}
	if server.ccrStore != nil {
		server.ccrStore.SetMaxMB(cfg.CCR.MaxStoreMB)
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -v ./internal/headroom/stages/ccr ./internal/api -run "TestCCRStore|TestServer_ApplyHeadroomConfig|TestManagement_SaveHeadroomConfig"`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/headroom/stages/ccr/store.go internal/headroom/stages/ccr/store_test.go internal/api/server.go internal/api/server_test.go
git commit -m "fix(headroom): dynamically resize CCR store on headroom config reload"
```

---

### Task 2: Zero-Allocation Request Logging via Inline `request_id`

**Files:**
- Modify: `internal/headroom/types.go`
- Modify: `internal/headroom/engine.go`
- Modify: `internal/headroom/stages/crusher/stage.go`
- Modify: `internal/headroom/stages/smart/stage.go`
- Modify: `internal/headroom/stages/code/stage.go`
- Modify: `internal/headroom/stages/shaper/stage.go`
- Modify: `internal/headroom/stages/ccr/stage.go`
- Test: `internal/headroom/engine_test.go`
- Test: `internal/headroom/types_test.go`
- Test: `internal/headroom/stages/crusher/crusher_test.go`
- Test: `internal/headroom/stages/smart/smart_test.go`
- Test: `internal/headroom/stages/code/code_test.go`
- Test: `internal/headroom/stages/shaper/shaper_test.go`
- Test: `internal/headroom/stages/ccr/ccr_test.go`

**Interfaces:**
- Consumes: `Engine.seq`, `Engine.logger`, `RequestContext`
- Produces:
  - `RequestContext.RequestID uint64`
  - `RequestContext.Logger *slog.Logger` (base logger with `module=headroom`, un-cloned)
  - Inline `"request_id", reqCtx.RequestID` in guarded debug and error logs.

- [ ] **Step 1: Write failing benchmark / alloc test pinning zero child logger allocations on `Engine.Process`**

Update `TestEngine_SummaryLogAllocatesNothingWhenDebugDisabled` in `internal/headroom/engine_test.go`:
```go
func TestEngine_ProcessZeroLoggerAllocsWhenDebugDisabled(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx := context.Background()
	req := map[string]any{"messages": []any{}}

	engine := NewEngine(Config{Enabled: true}, logger, &mockStage{name: "mock"})

	// Warm up
	_, _ = engine.Process(ctx, req)

	// An empty Process with disabled debug should only allocate the RequestContext struct (1 alloc).
	allocs := testing.AllocsPerRun(200, func() {
		_, _ = engine.Process(ctx, req)
	})

	if allocs > 1.0 {
		t.Fatalf("expected <= 1 alloc (RequestContext struct only), got %.0f allocs", allocs)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test -v ./internal/headroom -run "TestEngine_ProcessZeroLoggerAllocsWhenDebugDisabled"`
Expected: FAIL (currently allocates ~4-5 allocs per run due to `e.logger.With("request_id", ...)`).

- [ ] **Step 3: Update `RequestContext`, `Engine.Process`, and stages to use `RequestID` and inline attribute logging**

1. In `internal/headroom/types.go`:
Add `RequestID uint64` to `RequestContext`:
```go
type RequestContext struct {
	Request map[string]any

	// RequestID is the monotonic request identifier assigned by Engine.
	RequestID uint64

	// Logger is the base logger configured with module=headroom.
	Logger *slog.Logger
    ...
```

2. In `internal/headroom/engine.go`:
```go
func (e *Engine) Process(ctx context.Context, req map[string]any) (*RequestContext, error) {
	cfg := e.GetConfig()

	reqCtx := &RequestContext{
		Request:           req,
		RequestID:         e.seq.Add(1),
		FrozenPrefixIndex: FrozenPrefixIndex(req, cfg.LiveTurns),
		Logger:            e.logger,
	}
	if cfg.PreserveVerbatimReads || (cfg.OutputShaper.Enabled && cfg.OutputShaper.EffortRouting) {
		reqCtx.Verbatim = NewToolInspector(req)
	}

	start := time.Now()
	if err := e.pipeline.Run(ctx, reqCtx, &cfg); err != nil {
		reqCtx.Log().Error("headroom pipeline failed", "request_id", reqCtx.RequestID, "error", err)
		return nil, err
	}

	if log := reqCtx.Log(); (reqCtx.BytesBefore > 0 || reqCtx.EffortClamped) && log.Enabled(ctx, slog.LevelDebug) {
		saved := reqCtx.BytesBefore - reqCtx.BytesAfter
		var savedPct float64
		if reqCtx.BytesBefore > 0 {
			savedPct = float64(saved) / float64(reqCtx.BytesBefore) * 100
		}
		log.Debug("headroom compression summary",
			"request_id", reqCtx.RequestID,
			"rewrites", reqCtx.RewritesCount,
			"bytes_before", reqCtx.BytesBefore,
			"bytes_after", reqCtx.BytesAfter,
			"saved_bytes", saved,
			"saved_pct", savedPct,
			"chunks_stored", reqCtx.ChunksStored,
			"effort_clamped", reqCtx.EffortClamped,
			"continuation", reqCtx.ContinuationKind,
			"verbatim_skipped", reqCtx.VerbatimSkipped,
			"duration_ms", float64(time.Since(start).Microseconds())/1000.0,
		)
	}
	return reqCtx, nil
}
```

3. In `internal/headroom/stages/crusher/stage.go`:
```go
			if dbg {
				log.Debug("command_crusher compressed tool output",
					"stage", s.Name(),
					"request_id", reqCtx.RequestID,
					"signature", detectSignature(before).String(),
					"ordinal", ord,
					"bytes_before", len(before),
					"bytes_after", len(after))
			}
```

4. In `internal/headroom/stages/smart/stage.go`:
```go
			if dbg {
				log.Debug("smart_crusher compacted tool output",
					"stage", s.Name(),
					"request_id", reqCtx.RequestID,
					"mode", mode,
					"ordinal", ord,
					"bytes_before", len(before),
					"bytes_after", len(after))
			}
```

5. In `internal/headroom/stages/code/stage.go`:
```go
			if dbg {
				log.Debug("code_compressor pruned tool output",
					"stage", s.Name(),
					"request_id", reqCtx.RequestID,
					"ordinal", ord,
					"bytes_before", len(before),
					"bytes_after", len(after))
			}
```

6. In `internal/headroom/stages/shaper/stage.go`:
```go
	if reqCtx.EffortClamped {
		log := reqCtx.Log()
		if log.Enabled(ctx, slog.LevelDebug) {
			log.Debug("output_shaper clamped thinking budget",
				"stage", s.Name(),
				"request_id", reqCtx.RequestID,
				"continuation_kind", kind.String(),
				"original_budget", original,
				"clamped_budget", clamped)
		}
	}
```

7. In `internal/headroom/stages/ccr/stage.go`:
```go
				if dbg {
					log.Debug("ccr demoted chunk",
						"stage", s.Name(),
						"request_id", reqCtx.RequestID,
						"chunk_id", id,
						"ordinal", ord,
						"message_index", idx,
						"chunk_bytes", len(before),
						"store_bytes", s.store.Bytes())
				}
```

- [ ] **Step 4: Run all headroom unit and stage tests**

Run: `go test -v ./internal/headroom/...`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/headroom/
git commit -m "perf(headroom): eliminate per-request child logger allocations"
```

---

### Task 3: Full Test Suite Verification and Benchmarks

**Files:**
- Test: all `internal/...` packages

- [ ] **Step 1: Run whole repository test suite**

Run: `go test -v ./internal/...`
Expected: ALL PASS

- [ ] **Step 2: Run benchmarks to verify memory efficiency**

Run: `go test -bench=BenchmarkEngine -benchmem ./internal/headroom/...`
Expected: Benchmark runs cleanly with reduced allocations.

- [ ] **Step 3: Run static analysis and formatting check**

Run: `go vet ./...` and `test -z "$(gofmt -s -l .)"`
Expected: Clean output.
