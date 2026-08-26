package openrouter

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// RankWeights controls how the ranker blends provider signals.
type RankWeights struct {
	Availability float64 // 0..1
	Context      float64 // 0..1
	Latency      float64 // 0..1 (lower latency better)
	Throughput   float64 // 0..1 (higher tps better)
}

// DefaultRankWeights are sensible defaults if config is zero.
func DefaultRankWeights() RankWeights {
	return RankWeights{
		Availability: 0.4,
		Context:      0.15,
		Latency:      0.25,
		Throughput:   0.2,
	}
}

// ProviderOrder is a pinned or custom order from config.
type ProviderOrder struct {
	Mode  string   // "auto" | "pinned" | "custom"
	Pin   string   // provider name when Mode == "pinned"
	Order []string // provider names when Mode == "custom"
}

// RoutingConfig is the runtime routing configuration injected by the server.
type RoutingConfig struct {
	FailureThreshold int         // consecutive failures before sticky is dropped
	RankWeights      RankWeights // weight blend
}

// DefaultRoutingConfig returns sensible defaults.
func DefaultRoutingConfig() RoutingConfig {
	return RoutingConfig{
		FailureThreshold: 10,
		RankWeights:      DefaultRankWeights(),
	}
}

// ranked is an internal ranking entry.
type ranked struct {
	endpoint ProviderEndpoint
	score    float64
}

// EWMA stats per (model, provider).
type providerStats struct {
	latencyMsEWMA float64
	tpsEWMA       float64
	successCount  int64
	failureCount  int64
	consecFails   int
	alpha         float64 // smoothing factor (per-result)
	lastUpdated   time.Time
}

// ProviderRouter is the in-memory routing decision state.
type ProviderRouter struct {
	mu sync.RWMutex

	cfg RoutingConfig

	// Ranks per model (key: modelID)
	ranks    map[string][]ranked
	rankedAt map[string]time.Time

	// Stickiness per (session, model) -> provider
	sticky   map[string]string
	stickyAt map[string]time.Time // last-write time per sticky key, for eviction

	// Stats per (model, provider)
	stats map[string]map[string]*providerStats

	// Persistence (debounced, atomic).
	savePath  string
	saveTimer *time.Timer
}

// routerStateVersion is the on-disk schema version for persisted router state.
const routerStateVersion = 2

// maxStickyEntries bounds the sticky map; oldest entries are evicted past it.
const maxStickyEntries = 10000

// persistedProviderStats is the serializable form of providerStats.
type persistedProviderStats struct {
	LatencyMsEWMA float64   `json:"latencyMsEWMA,omitempty"`
	TPSEWMA       float64   `json:"tpsEWMA,omitempty"`
	SuccessCount  int64     `json:"successCount,omitempty"`
	FailureCount  int64     `json:"failureCount,omitempty"`
	ConsecFails   int       `json:"consecFails,omitempty"`
	LastUpdated   time.Time `json:"lastUpdated,omitempty"`
}

// persistedRouterState is the versioned JSON envelope for router persistence.
type persistedRouterState struct {
	Version  int                                          `json:"version"`
	Sticky   map[string]string                            `json:"sticky,omitempty"`
	StickyAt map[string]time.Time                         `json:"stickyAt,omitempty"`
	Stats    map[string]map[string]persistedProviderStats `json:"stats,omitempty"`
}

// saveDebounce is the minimum interval between automatic saves.
const saveDebounce = 30 * time.Second

// keySticky builds "session|model" key.
func keySticky(session, model string) string {
	return session + "\x00" + model
}

// DefaultRouter is a shared router.
var DefaultRouter = NewProviderRouter(DefaultRoutingConfig())

// NewProviderRouter creates a router.
func NewProviderRouter(cfg RoutingConfig) *ProviderRouter {
	if cfg.FailureThreshold <= 0 {
		cfg.FailureThreshold = 10
	}
	if cfg.RankWeights.Availability == 0 && cfg.RankWeights.Latency == 0 &&
		cfg.RankWeights.Throughput == 0 && cfg.RankWeights.Context == 0 {
		cfg.RankWeights = DefaultRankWeights()
	}
	return &ProviderRouter{
		cfg:      cfg,
		ranks:    map[string][]ranked{},
		rankedAt: map[string]time.Time{},
		sticky:   map[string]string{},
		stickyAt: map[string]time.Time{},
		stats:    map[string]map[string]*providerStats{},
	}
}

// SetConfig updates the routing configuration (live).
func (r *ProviderRouter) SetConfig(cfg RoutingConfig) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cfg.FailureThreshold <= 0 {
		cfg.FailureThreshold = 10
	}
	if cfg.RankWeights.Availability == 0 && cfg.RankWeights.Latency == 0 &&
		cfg.RankWeights.Throughput == 0 && cfg.RankWeights.Context == 0 {
		cfg.RankWeights = DefaultRankWeights()
	}
	r.cfg = cfg
}

// RefreshRanks replaces the rank list for a model, dropping any prior provider
// not in the new list. Preserves the order returned by the caller (or sorts by
// score if not provided).
func (r *ProviderRouter) RefreshRanks(model string, endpoints []ProviderEndpoint) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refreshRanksLocked(model, endpoints)
}

func (r *ProviderRouter) refreshRanksLocked(model string, endpoints []ProviderEndpoint) {
	// Preserve existing EWMA stats per provider while refreshing ranks.
	rankedList := make([]ranked, 0, len(endpoints))
	for _, ep := range endpoints {
		rankedList = append(rankedList, ranked{
			endpoint: ep,
			score:    0, // will be computed below
		})
	}

	// Apply per-model scoring context.
	maxCtx := 0
	for _, ep := range endpoints {
		if ep.ContextLength > maxCtx {
			maxCtx = ep.ContextLength
		}
	}

	// Pull local stats for this model.
	stats := r.stats[model]
	for i := range rankedList {
		rankedList[i].score = r.scoreLocked(r.cfg.RankWeights, rankedList[i].endpoint, stats, maxCtx)
	}
	sort.SliceStable(rankedList, func(i, j int) bool {
		return rankedList[i].score > rankedList[j].score
	})
	r.ranks[model] = rankedList
	r.rankedAt[model] = time.Now()

	// Drop stickiness for providers no longer in the rank set.
	present := make(map[string]bool, len(rankedList))
	for _, rk := range rankedList {
		present[rk.endpoint.ProviderName] = true
	}
	for k, v := range r.sticky {
		// Only drop if the key belongs to this model
		if strings.HasSuffix(k, "\x00"+model) {
			if !present[v] {
				delete(r.sticky, k)
				delete(r.stickyAt, k)
			}
		}
	}

	// Initialize stats map for any new providers.
	if _, ok := r.stats[model]; !ok {
		r.stats[model] = map[string]*providerStats{}
	}
	for _, rk := range rankedList {
		if _, ok := r.stats[model][rk.endpoint.ProviderName]; !ok {
			r.stats[model][rk.endpoint.ProviderName] = &providerStats{alpha: 0.3}
		}
	}
}

func (r *ProviderRouter) scoreLocked(w RankWeights, ep ProviderEndpoint, stats map[string]*providerStats, maxCtx int) float64 {
	var (
		availPart   float64
		contextPart float64
		latencyPart float64
		tpsPart     float64
	)

	// Availability: 0..1 (penalty for unhealthy).
	if !ep.Healthy() {
		availPart = 0
	} else {
		availPart = ep.AvailabilityScore()
	}

	// Context: 0..1 relative to max observed.
	if maxCtx > 0 {
		contextPart = float64(ep.ContextLength) / float64(maxCtx)
	}

	// Local latency: lower better.  Cold start = 0.5.
	if s, ok := stats[ep.ProviderName]; ok && s.successCount > 0 && s.latencyMsEWMA > 0 {
		// Map 100ms..10s onto 1..0
		ms := s.latencyMsEWMA
		if ms < 100 {
			ms = 100
		}
		if ms > 10000 {
			ms = 10000
		}
		latencyPart = 1.0 - (ms-100)/9900.0
	} else {
		latencyPart = 0.5
	}

	// Local TPS: higher better. Cold start = 0.5.
	if s, ok := stats[ep.ProviderName]; ok && s.successCount > 0 && s.tpsEWMA > 0 {
		t := s.tpsEWMA
		if t < 1 {
			t = 1
		}
		if t > 200 {
			t = 200
		}
		tpsPart = (t - 1) / 199.0
	} else {
		tpsPart = 0.5
	}

	// Use API priors when present and local empty.
	if w.Latency > 0 && ep.LatencyLast30mMs > 0 {
		if s, ok := stats[ep.ProviderName]; !ok || s.successCount == 0 {
			ms := ep.LatencyLast30mMs
			if ms < 100 {
				ms = 100
			}
			if ms > 10000 {
				ms = 10000
			}
			latencyPart = 1.0 - (ms-100)/9900.0
		}
	}
	if w.Throughput > 0 && ep.ThroughputLast30mTPS > 0 {
		if s, ok := stats[ep.ProviderName]; !ok || s.successCount == 0 {
			t := ep.ThroughputLast30mTPS
			if t < 1 {
				t = 1
			}
			if t > 200 {
				t = 200
			}
			tpsPart = (t - 1) / 199.0
		}
	}

	return w.Availability*availPart +
		w.Context*contextPart +
		w.Latency*latencyPart +
		w.Throughput*tpsPart
}

// GetRanks returns a copy of the rank list for a model.
func (r *ProviderRouter) GetRanks(model string) []RankedProvider {
	r.mu.RLock()
	defer r.mu.RUnlock()
	list, ok := r.ranks[model]
	if !ok {
		return nil
	}
	out := make([]RankedProvider, len(list))
	for i, rk := range list {
		out[i] = RankedProvider{
			Provider:   rk.endpoint.ProviderName,
			Tag:        rk.endpoint.Tag,
			ContextLen: rk.endpoint.ContextLength,
			Score:      rk.score,
			Endpoint:   rk.endpoint,
		}
	}
	return out
}

// RankedProvider is the public read shape of a rank entry.
type RankedProvider struct {
	Provider   string
	Tag        string
	ContextLen int
	Score      float64
	Endpoint   ProviderEndpoint
}

// Select returns the provider name to use for a request.
func (r *ProviderRouter) Select(session, model string, order ProviderOrder) string {
	chain := r.SelectChain(session, model, order)
	if len(chain) == 0 {
		return ""
	}
	return chain[0]
}

// SelectChain returns the ordered failover candidates for a request:
// pinned → [pin]; custom → configured order (filtered to ranked providers);
// auto → healthy sticky first, then ranked providers (healthy, under failure
// threshold). The first candidate becomes the sticky provider for the session.
func (r *ProviderRouter) SelectChain(session, model string, order ProviderOrder) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	defer r.scheduleSaveLocked()
	r.pruneStickyLocked()
	now := time.Now()

	// Pinned mode ignores everything else.
	if order.Mode == "pinned" && strings.TrimSpace(order.Pin) != "" {
		k := keySticky(session, model)
		r.sticky[k] = order.Pin
		r.stickyAt[k] = now
		return []string{order.Pin}
	}

	// Custom mode: configured order, filtered to ranked providers.
	if order.Mode == "custom" {
		var out []string
		for _, p := range order.Order {
			if p != "" && r.providerInRanksLocked(model, p) {
				out = append(out, p)
			}
		}
		if len(out) > 0 {
			k := keySticky(session, model)
			r.sticky[k] = out[0]
			r.stickyAt[k] = now
			return out
		}
	}

	// Auto mode.
	var chain []string
	seen := map[string]bool{}
	add := func(p string) {
		if p != "" && !seen[p] && r.providerHealthyUnderThresholdLocked(model, p) {
			seen[p] = true
			chain = append(chain, p)
		}
	}

	// Sticky first if alive and under threshold.
	if sticky, ok := r.sticky[keySticky(session, model)]; ok {
		if r.providerHealthyUnderThresholdLocked(model, sticky) {
			add(sticky)
		} else {
			k := keySticky(session, model)
			delete(r.sticky, k)
			delete(r.stickyAt, k)
		}
	}

	// Then ranked order.
	for _, rk := range r.ranks[model] {
		add(rk.endpoint.ProviderName)
	}

	if len(chain) > 0 {
		k := keySticky(session, model)
		r.sticky[k] = chain[0]
		r.stickyAt[k] = now
	}
	return chain
}

// pruneStickyLocked evicts the oldest sticky entries when the map grows past
// maxStickyEntries. Entries without a timestamp sort oldest. Caller must hold
// r.mu.
func (r *ProviderRouter) pruneStickyLocked() {
	if len(r.sticky) <= maxStickyEntries {
		return
	}
	type entry struct {
		key string
		at  time.Time
	}
	entries := make([]entry, 0, len(r.sticky))
	for k := range r.sticky {
		entries = append(entries, entry{key: k, at: r.stickyAt[k]})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].at.Before(entries[j].at) })
	for _, e := range entries[:len(entries)-maxStickyEntries] {
		delete(r.sticky, e.key)
		delete(r.stickyAt, e.key)
	}
}

// providerInRanksLocked reports whether a provider name appears in the current ranks.
func (r *ProviderRouter) providerInRanksLocked(model, provider string) bool {
	ranks, ok := r.ranks[model]
	if !ok {
		return false
	}
	for _, rk := range ranks {
		if rk.endpoint.ProviderName == provider {
			return true
		}
	}
	return false
}

// providerHealthyUnderThresholdLocked reports whether the provider is currently
// usable (healthy status + failure count under threshold).
func (r *ProviderRouter) providerHealthyUnderThresholdLocked(model, provider string) bool {
	// Check rank for unhealthy status.
	for _, rk := range r.ranks[model] {
		if rk.endpoint.ProviderName == provider && !rk.endpoint.Healthy() {
			return false
		}
	}
	if stats, ok := r.stats[model]; ok {
		if s, ok := stats[provider]; ok {
			if s.consecFails >= r.cfg.FailureThreshold {
				return false
			}
		}
	}
	return true
}

// RecordResult updates the per-provider EWMA stats and resets/increments failure counters.
func (r *ProviderRouter) RecordResult(model, provider string, ok bool, latency time.Duration, tokens int) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.stats[model]; !exists {
		r.stats[model] = map[string]*providerStats{}
	}
	s, exists := r.stats[model][provider]
	if !exists {
		s = &providerStats{alpha: 0.3}
		r.stats[model][provider] = s
	}
	s.lastUpdated = time.Now()
	r.scheduleSaveLocked()

	if ok {
		s.successCount++
		s.consecFails = 0
		if latency > 0 {
			ms := float64(latency.Milliseconds())
			if s.latencyMsEWMA == 0 {
				s.latencyMsEWMA = ms
			} else {
				s.latencyMsEWMA = ewma(s.latencyMsEWMA, ms, s.alpha)
			}
		}
		if tokens > 0 && latency > 0 {
			tps := float64(tokens) / latency.Seconds()
			if s.tpsEWMA == 0 {
				s.tpsEWMA = tps
			} else {
				s.tpsEWMA = ewma(s.tpsEWMA, tps, s.alpha)
			}
		}
	} else {
		s.failureCount++
		s.consecFails++
		// Threshold break: clear stickiness for all sessions on this pair.
		if s.consecFails >= r.cfg.FailureThreshold {
			for k, v := range r.sticky {
				// Sticky key format is "session\x00model" — match by suffix.
				if strings.HasSuffix(k, "\x00"+model) && v == provider {
					delete(r.sticky, k)
					delete(r.stickyAt, k)
				}
			}
		}
	}
}

// Stats returns a snapshot of stats for a (model, provider).
type ProviderStatsSnapshot struct {
	LatencyMsEWMA float64 `json:"latencyMsEWMA"`
	TPSEWMA       float64 `json:"tpsEWMA"`
	SuccessCount  int64   `json:"successCount"`
	FailureCount  int64   `json:"failureCount"`
	ConsecFails   int     `json:"consecFails"`
}

// Stats returns a snapshot of stats for a model.
func (r *ProviderRouter) Stats(model string) map[string]ProviderStatsSnapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := map[string]ProviderStatsSnapshot{}
	stats, ok := r.stats[model]
	if !ok {
		return out
	}
	for k, s := range stats {
		out[k] = ProviderStatsSnapshot{
			LatencyMsEWMA: s.latencyMsEWMA,
			TPSEWMA:       s.tpsEWMA,
			SuccessCount:  s.successCount,
			FailureCount:  s.failureCount,
			ConsecFails:   s.consecFails,
		}
	}
	return out
}

// StickyProvider returns the sticky provider for a session+model, if any.
func (r *ProviderRouter) StickyProvider(session, model string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	v, ok := r.sticky[keySticky(session, model)]
	return v, ok
}

// SetSticky forces a session+model to a specific provider (used by per-model config saves).
func (r *ProviderRouter) SetSticky(session, model, provider string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := keySticky(session, model)
	if provider == "" {
		delete(r.sticky, k)
		delete(r.stickyAt, k)
		return
	}
	r.pruneStickyLocked()
	r.sticky[k] = provider
	r.stickyAt[k] = time.Now()
}

func ewma(prev, sample, alpha float64) float64 {
	if alpha <= 0 || alpha >= 1 {
		return sample
	}
	return alpha*sample + (1-alpha)*prev
}

// EnablePersistence loads any existing state from path and schedules future
// debounced saves to the same path. A missing file is a clean start; a corrupt
// or wrong-version file is ignored (clean start).
func (r *ProviderRouter) EnablePersistence(path string) {
	r.mu.Lock()
	r.savePath = path
	r.mu.Unlock()
	_ = r.LoadFrom(path)
}

// LoadFrom restores sticky assignments and EWMA stats from path.
func (r *ProviderRouter) LoadFrom(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read router state: %w", err)
	}
	var state persistedRouterState
	if err := json.Unmarshal(data, &state); err != nil {
		// Corrupt file — clean start, do not fail startup.
		return nil
	}
	if state.Version != routerStateVersion {
		return nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	for k, v := range state.Sticky {
		r.sticky[k] = v
	}
	for k, at := range state.StickyAt {
		if _, ok := r.sticky[k]; ok {
			r.stickyAt[k] = at
		}
	}
	for model, providers := range state.Stats {
		if _, ok := r.stats[model]; !ok {
			r.stats[model] = map[string]*providerStats{}
		}
		for provider, ps := range providers {
			r.stats[model][provider] = &providerStats{
				latencyMsEWMA: ps.LatencyMsEWMA,
				tpsEWMA:       ps.TPSEWMA,
				successCount:  ps.SuccessCount,
				failureCount:  ps.FailureCount,
				consecFails:   ps.ConsecFails,
				alpha:         0.3,
				lastUpdated:   ps.LastUpdated,
			}
		}
	}
	return nil
}

// SaveTo writes the router state to path atomically (tmp + rename).
func (r *ProviderRouter) SaveTo(path string) error {
	r.mu.RLock()
	state := persistedRouterState{
		Version:  routerStateVersion,
		Sticky:   make(map[string]string, len(r.sticky)),
		StickyAt: make(map[string]time.Time, len(r.stickyAt)),
		Stats:    make(map[string]map[string]persistedProviderStats, len(r.stats)),
	}
	for k, v := range r.sticky {
		state.Sticky[k] = v
	}
	for k, at := range r.stickyAt {
		if _, ok := r.sticky[k]; ok {
			state.StickyAt[k] = at
		}
	}
	for model, providers := range r.stats {
		out := make(map[string]persistedProviderStats, len(providers))
		for provider, s := range providers {
			out[provider] = persistedProviderStats{
				LatencyMsEWMA: s.latencyMsEWMA,
				TPSEWMA:       s.tpsEWMA,
				SuccessCount:  s.successCount,
				FailureCount:  s.failureCount,
				ConsecFails:   s.consecFails,
				LastUpdated:   s.lastUpdated,
			}
		}
		state.Stats[model] = out
	}
	r.mu.RUnlock()

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal router state: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create router state dir: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write router state: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename router state: %w", err)
	}
	return nil
}

// scheduleSaveLocked queues a debounced save if persistence is enabled.
// Caller must hold r.mu.
func (r *ProviderRouter) scheduleSaveLocked() {
	if r.savePath == "" || r.saveTimer != nil {
		return
	}
	path := r.savePath
	r.saveTimer = time.AfterFunc(saveDebounce, func() {
		r.mu.Lock()
		r.saveTimer = nil
		r.mu.Unlock()
		_ = r.SaveTo(path)
	})
}

// FlushSave forces an immediate save when persistence is enabled (shutdown hook).
func (r *ProviderRouter) FlushSave() {
	r.mu.Lock()
	if r.saveTimer != nil {
		r.saveTimer.Stop()
		r.saveTimer = nil
	}
	path := r.savePath
	r.mu.Unlock()
	if path != "" {
		_ = r.SaveTo(path)
	}
}
