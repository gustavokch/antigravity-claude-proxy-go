package accounts

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"antigravity-go-proxy/internal/config"
	"antigravity-go-proxy/internal/modelcatalog"
)

const (
	StrategySticky     = "sticky"
	StrategyRoundRobin = "round-robin"
	StrategyHybrid     = "hybrid"
	DefaultStrategy    = StrategyHybrid
	maxStickyWait      = 2 * time.Minute
)

type RateLimit struct {
	IsRateLimited bool  `json:"isRateLimited"`
	ResetTimeMS   int64 `json:"-"`
	ActualResetMS int64 `json:"actualResetMs,omitempty"`
}

func (limit *RateLimit) UnmarshalJSON(data []byte) error {
	var raw struct {
		IsRateLimited bool            `json:"isRateLimited"`
		ResetTime     json.RawMessage `json:"resetTime"`
		ActualResetMS int64           `json:"actualResetMs"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	limit.IsRateLimited = raw.IsRateLimited
	limit.ActualResetMS = raw.ActualResetMS
	if len(raw.ResetTime) > 0 && string(raw.ResetTime) != "null" {
		var number float64
		if err := json.Unmarshal(raw.ResetTime, &number); err == nil {
			limit.ResetTimeMS = int64(number)
		}
	}
	return nil
}

type ModelQuota struct {
	RemainingFraction *float64 `json:"remainingFraction"`
	ResetTime         string   `json:"resetTime,omitempty"`
	// ExhaustedUntilMS holds an upstream "individual quota reached" 429 until
	// its own reset time. Cloud Code keeps reporting remainingFraction 1 for a
	// model it is already refusing, so without this marker the next live
	// refresh restores a phantom 100%.
	ExhaustedUntilMS int64 `json:"exhaustedUntilMs,omitempty"`
}

type Quota struct {
	Models      map[string]ModelQuota `json:"models"`
	Pools       map[string]ModelQuota `json:"pools,omitempty"`
	LastChecked any                   `json:"lastChecked"`
}

type Subscription struct {
	Tier       string `json:"tier"`
	ProjectID  string `json:"projectId"`
	DetectedAt any    `json:"detectedAt,omitempty"`
}

type Account struct {
	Email          string
	Source         string
	Enabled        bool
	DBPath         string
	RefreshToken   string
	APIKey         string
	AgyTokenPath   string
	ProjectID      string
	Subscription   Subscription
	Quota          Quota
	QuotaThreshold *float64
	ModelThreshold map[string]float64
	// Credits holds absolute remaining credit balances by credit-type name
	// (e.g. "GOOGLE_ONE_AI") as last reported by GenerateContent responses.
	// Unlike Quota fractions (which upstream holds at 1 until exhaustion),
	// these decrease with use and are the visible consumption signal.
	Credits map[string]int64

	LastUsedMS         int64
	IsInvalid          bool
	InvalidReason      string
	VerifyURL          string
	ModelRateLimits    map[string]*RateLimit
	ConsecutiveFailure int
	CoolingDownUntilMS int64
	CooldownReason     string
}

type diskAccount struct {
	Email                string                `json:"email"`
	Source               string                `json:"source"`
	Enabled              *bool                 `json:"enabled"`
	DBPath               string                `json:"dbPath"`
	RefreshToken         string                `json:"refreshToken"`
	APIKey               string                `json:"apiKey"`
	AgyTokenPath         string                `json:"agyTokenPath"`
	ProjectID            string                `json:"projectId"`
	Subscription         Subscription          `json:"subscription"`
	Quota                Quota                 `json:"quota"`
	QuotaThreshold       *float64              `json:"quotaThreshold"`
	ModelQuotaThresholds map[string]float64    `json:"modelQuotaThresholds"`
	Credits              map[string]int64      `json:"credits,omitempty"`
	LastUsed             json.RawMessage       `json:"lastUsed"`
	IsInvalid            bool                  `json:"isInvalid"`
	InvalidReason        string                `json:"invalidReason"`
	VerifyURL            string                `json:"verifyUrl"`
	ModelRateLimits      map[string]*RateLimit `json:"modelRateLimits"`
}

type File struct {
	Accounts    []*Account
	Settings    map[string]any
	ActiveIndex int
}

func DefaultConfigPath() (string, error) {
	return filepath.Join(config.GetConfigDir(), "accounts.json"), nil
}

// Load reads the account-pool configuration without ever opening it for
// writing. Invalid state is reset on startup unless a verification URL requires
// user action.
func Load(path string) (File, error) {
	if path == "" {
		var err error
		path, err = DefaultConfigPath()
		if err != nil {
			return File{}, err
		}
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return File{}, fmt.Errorf("read account configuration: %w", err)
	}
	var document struct {
		Accounts    []diskAccount  `json:"accounts"`
		Settings    map[string]any `json:"settings"`
		ActiveIndex int            `json:"activeIndex"`
	}
	if err := json.Unmarshal(contents, &document); err != nil {
		return File{}, fmt.Errorf("decode account configuration: %w", err)
	}
	result := File{Settings: document.Settings, ActiveIndex: document.ActiveIndex}
	for _, stored := range document.Accounts {
		enabled := stored.Enabled == nil || *stored.Enabled
		account := &Account{
			Email: stored.Email, Source: stored.Source, Enabled: enabled,
			DBPath: stored.DBPath, RefreshToken: stored.RefreshToken, APIKey: stored.APIKey,
			AgyTokenPath: stored.AgyTokenPath, ProjectID: stored.ProjectID,
			Subscription: stored.Subscription, Quota: stored.Quota,
			QuotaThreshold: stored.QuotaThreshold, ModelThreshold: stored.ModelQuotaThresholds,
			Credits: stored.Credits,
			VerifyURL: stored.VerifyURL, ModelRateLimits: stored.ModelRateLimits,
		}
		if account.Source == "" {
			account.Source = "database"
		}
		if account.ModelRateLimits == nil {
			account.ModelRateLimits = make(map[string]*RateLimit)
		}
		if account.ModelThreshold == nil {
			account.ModelThreshold = make(map[string]float64)
		}
		if account.Quota.Models == nil {
			account.Quota.Models = make(map[string]ModelQuota)
		}
		if stored.VerifyURL != "" {
			account.IsInvalid = stored.IsInvalid
			account.InvalidReason = stored.InvalidReason
		} else if stored.IsInvalid {
			// accounts.json had isInvalid=true but no verifyURL — reset it on startup
			// (agy or a previous proxy run may have written this; we clear it so a
			// simple restart recovers the account without user intervention)
			slog.Warn("account was marked invalid in accounts.json without verifyURL — resetting on startup",
				"email", stored.Email, "reason", stored.InvalidReason)
		}
		account.LastUsedMS = parseMilliseconds(stored.LastUsed)
		result.Accounts = append(result.Accounts, account)
	}
	if result.ActiveIndex < 0 || result.ActiveIndex >= len(result.Accounts) {
		result.ActiveIndex = 0
	}
	return result, nil
}

type Options struct {
	Accounts             []*Account
	ActiveIndex          int
	Strategy             string
	ConfigPath           string
	Settings             map[string]any
	SelectionConfig      config.AccountSelectionConfig
	GlobalQuotaThreshold float64
	SharedThrottleWindow time.Duration
	Now                  func() time.Time
}

type Selection struct {
	Account *Account
	Wait    time.Duration
}

type healthRecord struct {
	Score       float64
	LastUpdated time.Time
}

type tokenBucket struct {
	Tokens      float64
	LastUpdated time.Time
}

type sharedThrottle struct {
	untilMS int64
	recent  map[string]int64 // account email -> last rejection unix ms
}

type Manager struct {
	mu                   sync.RWMutex
	configPath           string
	accounts             []*Account
	settings             map[string]any
	strategy             string
	selectionConfig      config.AccountSelectionConfig
	globalQuotaThreshold float64
	currentIndex         int
	cursor               int
	now                  func() time.Time
	health               map[string]healthRecord
	buckets              map[string]tokenBucket
	projects             map[string]string
	sharedThrottleWindow time.Duration
	modelThrottles       map[string]*sharedThrottle
}

func mapFloat(m map[string]any, key string, fallback float64) float64 {
	if m == nil {
		return fallback
	}
	v, ok := m[key]
	if !ok || v == nil {
		return fallback
	}
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case json.Number:
		if f, err := n.Float64(); err == nil {
			return f
		}
	}
	return fallback
}

func New(options Options) (*Manager, error) {
	strategy := normalizeStrategy(options.Strategy)
	if options.Now == nil {
		options.Now = time.Now
	}
	for _, account := range options.Accounts {
		if account == nil {
			return nil, errors.New("account configuration contains a null account")
		}
		if account.Source == "" {
			account.Source = "database"
		}
		if account.ModelRateLimits == nil {
			account.ModelRateLimits = make(map[string]*RateLimit)
		}
		if account.ModelThreshold == nil {
			account.ModelThreshold = make(map[string]float64)
		}
		if account.Quota.Models == nil {
			account.Quota.Models = make(map[string]ModelQuota)
		}
	}
	if options.ActiveIndex < 0 || options.ActiveIndex >= len(options.Accounts) {
		options.ActiveIndex = 0
	}
	selectionConfig := options.SelectionConfig
	if selectionConfig.Strategy == "" {
		selectionConfig = config.Get().AccountSelection
	}
	globalQuotaThreshold := options.GlobalQuotaThreshold
	if globalQuotaThreshold <= 0 {
		globalQuotaThreshold = config.Get().GlobalQuotaThreshold
	}
	sharedThrottleWindow := options.SharedThrottleWindow
	if sharedThrottleWindow <= 0 {
		if ms := config.Get().SharedThrottleWindowMs; ms > 0 {
			sharedThrottleWindow = time.Duration(ms) * time.Millisecond
		} else {
			sharedThrottleWindow = 10 * time.Second
		}
	}
	return &Manager{
		configPath:           options.ConfigPath,
		accounts:             options.Accounts,
		settings:             options.Settings,
		strategy:             strategy,
		selectionConfig:      selectionConfig,
		globalQuotaThreshold: globalQuotaThreshold,
		currentIndex:         options.ActiveIndex,
		now:                  options.Now,
		health:               make(map[string]healthRecord),
		buckets:              make(map[string]tokenBucket),
		projects:             make(map[string]string),
		sharedThrottleWindow: sharedThrottleWindow,
		modelThrottles:       make(map[string]*sharedThrottle),
	}, nil
}

func NewFromFile(path, strategy string, now func() time.Time) (*Manager, error) {
	file, err := Load(path)
	if err != nil {
		return nil, err
	}
	return New(Options{Accounts: file.Accounts, ActiveIndex: file.ActiveIndex, Settings: file.Settings, ConfigPath: path, Strategy: strategy, Now: now})
}

// NewDefault uses the optional account-pool configuration when it exists.
// Otherwise it starts with an empty pool if no accounts are configured.
func NewDefault(path, strategy string, now func() time.Time) (*Manager, error) {
	if path != "" {
		return NewFromFile(path, strategy, now)
	}
	configPath, err := DefaultConfigPath()
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(configPath); err == nil {
		return NewFromFile(configPath, strategy, now)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect account configuration: %w", err)
	}

	// No accounts file found; start with an empty pool (allow web UI / API to add accounts)
	return New(Options{
		ConfigPath: configPath,
		Strategy:   strategy,
		Now:        now,
	})
}

func (manager *Manager) Count() int {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	return len(manager.accounts)
}

func (manager *Manager) Select(model string) Selection {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.clearExpiredLocked()
	if wait := manager.sharedThrottleWaitLocked(rateLimitModelKey(model)); wait > 0 {
		return Selection{Wait: wait}
	}
	switch manager.strategy {
	case StrategySticky:
		return manager.selectStickyLocked(model)
	case StrategyRoundRobin:
		return manager.selectRoundRobinLocked(model)
	default:
		return manager.selectHybridLocked(model)
	}
}

func (manager *Manager) Available(model string) int {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if len(manager.accounts) == 0 {
		return 0 // No accounts available
	}
	manager.clearExpiredLocked()
	if manager.sharedThrottleWaitLocked(rateLimitModelKey(model)) > 0 {
		return 0
	}
	count := 0
	for _, account := range manager.accounts {
		if manager.usableLocked(account, model) {
			count++
		}
	}
	return count
}

func (manager *Manager) AllInvalid() bool {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	enabled := 0
	invalid := 0
	for _, account := range manager.accounts {
		if account.Enabled {
			enabled++
			if account.IsInvalid {
				invalid++
			}
		}
	}
	return enabled > 0 && enabled == invalid
}

// ModelKeyCandidates returns the ordered candidate keys to search for quota or thresholds:
// 1. Exact model string
// 2. Model with [1m] suffix stripped
// 3. Lowercased stripped model — the canonical key, identical to rateLimitModelKey
// Duplicate entries and empty strings are omitted.
//
// All model-keyed maps (ModelRateLimits, Quota.Models, ModelThreshold) share this
// normalization scheme, with rateLimitModelKey naming the canonical write key.
func ModelKeyCandidates(model string) []string {
	stripped := modelcatalog.Strip1mSuffix(model)
	lower := strings.ToLower(stripped)
	seen := make(map[string]struct{}, 3)
	candidates := make([]string, 0, 3)
	// If stripping [1m] produces an empty string, the input was suffix-only (e.g. "[1m]")
	// and must not be treated as a valid candidate.
	list := []string{model, stripped, lower}
	if stripped == "" {
		list = []string{}
	}
	for _, k := range list {
		if k == "" {
			continue
		}
		if _, exists := seen[k]; !exists {
			seen[k] = struct{}{}
			candidates = append(candidates, k)
		}
	}
	return candidates
}

// rateLimitModelKey returns the canonical key: the last, most-normalized candidate.
func rateLimitModelKey(model string) string {
	candidates := ModelKeyCandidates(model)
	if len(candidates) == 0 {
		return ""
	}
	return candidates[len(candidates)-1]
}

func (manager *Manager) MinWait(model string) time.Duration {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	now := manager.now().UnixMilli()
	minimum := int64(0)
	key := rateLimitModelKey(model)
	for _, account := range manager.accounts {
		if limit := account.ModelRateLimits[key]; limit != nil && limit.IsRateLimited && limit.ResetTimeMS > now {
			wait := limit.ResetTimeMS - now
			if minimum == 0 || wait < minimum {
				minimum = wait
			}
		}
	}
	return time.Duration(minimum) * time.Millisecond
}

// recordSharedThrottleLocked notes one rejection and promotes the model to a
// pool-wide throttle once two distinct accounts are rejected inside the
// window. A single-account pool can never be "shared" — there is nothing to
// rotate to — so it is skipped.
func (manager *Manager) recordSharedThrottleLocked(key, email string, wait time.Duration) {
	if key == "" || len(manager.accounts) < 2 {
		return
	}
	if manager.modelThrottles == nil {
		manager.modelThrottles = make(map[string]*sharedThrottle)
	}
	nowMS := manager.now().UnixMilli()
	entry := manager.modelThrottles[key]
	if entry == nil {
		entry = &sharedThrottle{recent: make(map[string]int64)}
		manager.modelThrottles[key] = entry
	}
	entry.recent[email] = nowMS

	cutoff := nowMS - manager.sharedThrottleWindow.Milliseconds()
	for recorded, at := range entry.recent {
		if at < cutoff {
			delete(entry.recent, recorded)
		}
	}
	if len(entry.recent) < 2 {
		return
	}
	if until := nowMS + wait.Milliseconds(); until > entry.untilMS {
		entry.untilMS = until
		slog.Warn("shared upstream throttle",
			"model", key, "accounts", len(entry.recent), "wait", wait.Round(time.Second))
	}
}

// SharedThrottleWait returns how long the whole pool must wait on a model, or
// 0 when no shared throttle is active.
func (manager *Manager) SharedThrottleWait(model string) time.Duration {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	return manager.sharedThrottleWaitLocked(rateLimitModelKey(model))
}

func (manager *Manager) sharedThrottleWaitLocked(key string) time.Duration {
	entry := manager.modelThrottles[key]
	if entry == nil {
		return 0
	}
	remaining := entry.untilMS - manager.now().UnixMilli()
	if remaining <= 0 {
		return 0
	}
	return time.Duration(remaining) * time.Millisecond
}

// SharedThrottles returns every model under an active pool-wide throttle and
// how long each has left. Used by the status output.
func (manager *Manager) SharedThrottles() map[string]time.Duration {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	active := make(map[string]time.Duration)
	for key := range manager.modelThrottles {
		if wait := manager.sharedThrottleWaitLocked(key); wait > 0 {
			active[key] = wait
		}
	}
	return active
}

func (manager *Manager) MarkRateLimited(account *Account, model string, wait time.Duration) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if wait <= 0 {
		wait = 10 * time.Second
	}
	// Guard on the raw input, not the normalized key: a non-blank model
	// string that strips to empty (suffix only, e.g. "[1m]") must not write
	// into the empty-model namespace that model listing uses deliberately.
	// An actual "" caller means the listing namespace and must be recorded.
	key := rateLimitModelKey(model)
	if key == "" && strings.TrimSpace(model) != "" {
		account.ConsecutiveFailure++
		manager.recordRateLimitLocked(account.Email)
		return
	}
	account.ModelRateLimits[key] = &RateLimit{
		IsRateLimited: true, ResetTimeMS: manager.now().Add(wait).UnixMilli(), ActualResetMS: wait.Milliseconds(),
	}
	manager.recordSharedThrottleLocked(key, account.Email, wait)
	account.ConsecutiveFailure++
	manager.recordRateLimitLocked(account.Email)
}

func (manager *Manager) MarkInvalid(account *Account, reason, verifyURL string) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	account.IsInvalid = true
	account.InvalidReason = reason
	account.VerifyURL = verifyURL
	manager.recordFailureLocked(account.Email)
	slog.Warn("account marked invalid", "email", account.Email, "reason", reason, "verifyURL", verifyURL)
}

func (manager *Manager) MarkFailure(account *Account, model string) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	account.ConsecutiveFailure++
	manager.recordFailureLocked(account.Email)
	// Guard on the normalized key, not the raw argument: "[1m]" or whitespace
	// passes a raw != "" check but normalizes to "", which would collide with
	// the empty-model namespace model listing uses.
	key := rateLimitModelKey(model)
	if key != "" && account.ConsecutiveFailure >= 3 {
		account.ModelRateLimits[key] = &RateLimit{
			IsRateLimited: true, ResetTimeMS: manager.now().Add(time.Minute).UnixMilli(), ActualResetMS: time.Minute.Milliseconds(),
		}
	}
}

func (manager *Manager) MarkSuccess(account *Account, model string) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	account.ConsecutiveFailure = 0
	// Delete the namespace only for a real key or an actual "" caller (the
	// listing namespace): a suffix-only success like "[1m]" normalizes to ""
	// and must not clear a legitimate empty-model entry.
	if key := rateLimitModelKey(model); key != "" || strings.TrimSpace(model) == "" {
		delete(account.ModelRateLimits, key)
		delete(manager.modelThrottles, key)
	}
	manager.recordSuccessLocked(account.Email)
}

func (manager *Manager) IncrementFailure(account *Account) int {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	account.ConsecutiveFailure++
	return account.ConsecutiveFailure
}

func (manager *Manager) FailureCount(account *Account) int {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	return account.ConsecutiveFailure
}

func (manager *Manager) Project(account *Account) string {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	if project := manager.projects[account.Email]; project != "" {
		return project
	}
	if parts := strings.Split(account.RefreshToken, "|"); len(parts) >= 3 && parts[2] != "" {
		return parts[2]
	}
	if account.ProjectID != "" {
		return account.ProjectID
	}
	return account.Subscription.ProjectID
}

type rawTier struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

func ExtractSubscription(body []byte, now time.Time) Subscription {
	if now.IsZero() {
		now = time.Now()
	}
	sub := Subscription{
		Tier:       "free",
		DetectedAt: now.UTC().Format(time.RFC3339),
	}
	if len(body) == 0 {
		return sub
	}

	var document map[string]json.RawMessage
	if err := json.Unmarshal(body, &document); err != nil {
		return sub
	}

	if raw, exists := document["cloudaicompanionProject"]; exists && len(raw) > 0 {
		var projectStr string
		if err := json.Unmarshal(raw, &projectStr); err == nil && projectStr != "" {
			sub.ProjectID = projectStr
		} else {
			var projectObj struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(raw, &projectObj); err == nil && projectObj.ID != "" {
				sub.ProjectID = projectObj.ID
			}
		}
	}

	var g1Tier string
	if raw, exists := document["g1Tier"]; exists && len(raw) > 0 {
		_ = json.Unmarshal(raw, &g1Tier)
	}

	var currentTier rawTier
	if raw, exists := document["currentTier"]; exists && len(raw) > 0 {
		_ = json.Unmarshal(raw, &currentTier)
	}

	var paidTier *rawTier
	if raw, exists := document["paidTier"]; exists && len(raw) > 0 && string(raw) != "null" {
		var t rawTier
		if err := json.Unmarshal(raw, &t); err == nil {
			paidTier = &t
		}
	}

	sub.Tier = normalizeTier(currentTier, paidTier, g1Tier)
	return sub
}

func normalizeTier(current rawTier, paid *rawTier, g1Tier string) string {
	g1Upper := strings.ToUpper(g1Tier)
	currIDLower := strings.ToLower(current.ID)
	currNameLower := strings.ToLower(current.Name)

	paidIDLower := ""
	paidNameLower := ""
	if paid != nil {
		paidIDLower = strings.ToLower(paid.ID)
		paidNameLower = strings.ToLower(paid.Name)
	}

	if g1Upper == "ULTRA" ||
		strings.Contains(currIDLower, "ultra") || strings.Contains(currNameLower, "ultra") ||
		strings.Contains(paidIDLower, "ultra") || strings.Contains(paidNameLower, "ultra") {
		return "ultra"
	}

	if paid != nil && (paid.ID != "" || paid.Name != "") {
		return "pro"
	}
	if g1Upper == "PRO" || g1Upper == "PREMIUM" || g1Upper == "GOOGLE_ONE_AI_PREMIUM" {
		return "pro"
	}
	for _, term := range []string{"pro", "paid", "gdp", "g1", "individual", "premium", "enterprise", "business"} {
		if strings.Contains(currIDLower, term) || strings.Contains(currNameLower, term) {
			return "pro"
		}
	}
	if currIDLower != "" && currIDLower != "tier-free" && currIDLower != "free" && currIDLower != "free-tier" && currIDLower != "tier_free" {
		return "pro"
	}

	return "free"
}

func (manager *Manager) CacheProject(account *Account, project string) {
	manager.mu.Lock()
	manager.projects[account.Email] = project
	for _, acc := range manager.accounts {
		if acc.Email == account.Email {
			acc.ProjectID = project
			acc.Subscription.ProjectID = project
			break
		}
	}
	manager.mu.Unlock()
	_ = manager.SaveToDisk()
}

func (manager *Manager) UpdateSubscription(email string, subscription Subscription) error {
	manager.mu.Lock()
	found := false
	for _, acc := range manager.accounts {
		if acc.Email == email {
			if subscription.Tier != "" {
				acc.Subscription.Tier = subscription.Tier
			}
			if subscription.ProjectID != "" {
				acc.Subscription.ProjectID = subscription.ProjectID
				acc.ProjectID = subscription.ProjectID
			}
			if subscription.DetectedAt != "" {
				acc.Subscription.DetectedAt = subscription.DetectedAt
			}
			found = true
			break
		}
	}
	manager.mu.Unlock()
	if !found {
		return fmt.Errorf("account %s not found", email)
	}
	return manager.SaveToDisk()
}

type Snapshot struct {
	Email         string
	Enabled       bool
	Invalid       bool
	InvalidReason string
	VerifyURL     string
	Limits        map[string]RateLimit
}

func (manager *Manager) Snapshot() []Snapshot {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	result := make([]Snapshot, 0, len(manager.accounts))
	for _, account := range manager.accounts {
		limits := make(map[string]RateLimit, len(account.ModelRateLimits))
		for model, limit := range account.ModelRateLimits {
			if limit != nil {
				limits[model] = *limit
			}
		}
		result = append(result, Snapshot{
			Email: account.Email, Enabled: account.Enabled, Invalid: account.IsInvalid,
			InvalidReason: account.InvalidReason, VerifyURL: account.VerifyURL, Limits: limits,
		})
	}
	return result
}

func (manager *Manager) selectStickyLocked(model string) Selection {
	if len(manager.accounts) == 0 {
		return Selection{} // No accounts available
	}
	if manager.currentIndex < 0 || manager.currentIndex >= len(manager.accounts) {
		manager.currentIndex = 0
	}
	current := manager.accounts[manager.currentIndex]
	if manager.usableLocked(current, model) {
		current.LastUsedMS = manager.now().UnixMilli()
		return Selection{Account: current}
	}
	for offset := 1; offset <= len(manager.accounts); offset++ {
		index := (manager.currentIndex + offset) % len(manager.accounts)
		if manager.usableLocked(manager.accounts[index], model) {
			manager.currentIndex = index
			manager.accounts[index].LastUsedMS = manager.now().UnixMilli()
			return Selection{Account: manager.accounts[index]}
		}
	}
	if limit := current.ModelRateLimits[rateLimitModelKey(model)]; current.Enabled && !current.IsInvalid && limit != nil {
		wait := time.Duration(limit.ResetTimeMS-manager.now().UnixMilli()) * time.Millisecond
		if wait > 0 && wait <= maxStickyWait {
			return Selection{Wait: wait}
		}
	}
	return Selection{}
}

func (manager *Manager) selectRoundRobinLocked(model string) Selection {
	if len(manager.accounts) == 0 {
		return Selection{} // No accounts available
	}
	if manager.cursor >= len(manager.accounts) {
		manager.cursor = 0
	}
	start := (manager.cursor + 1) % len(manager.accounts)
	for offset := range len(manager.accounts) {
		index := (start + offset) % len(manager.accounts)
		account := manager.accounts[index]
		if manager.usableLocked(account, model) {
			manager.cursor = index
			manager.currentIndex = index
			account.LastUsedMS = manager.now().UnixMilli()
			return Selection{Account: account}
		}
	}
	return Selection{}
}

func (manager *Manager) selectHybridLocked(model string) Selection {
	type candidate struct {
		account *Account
		index   int
		score   float64
	}
	candidates := make([]candidate, 0)
	minUsable := mapFloat(manager.selectionConfig.HealthScore, "minUsable", 50)
	for index, account := range manager.accounts {
		if !manager.usableLocked(account, model) || manager.healthScoreLocked(account.Email) < minUsable || manager.tokensLocked(account.Email) < 1 {
			continue
		}
		if manager.quotaCriticalLocked(account, model) {
			continue
		}
		candidates = append(candidates, candidate{account: account, index: index, score: manager.scoreLocked(account, model)})
	}
	if len(candidates) == 0 {
		for index, account := range manager.accounts {
			if manager.usableLocked(account, model) {
				candidates = append(candidates, candidate{account: account, index: index, score: manager.scoreLocked(account, model)})
			}
		}
	}
	if len(candidates) == 0 {
		return Selection{}
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].score > candidates[j].score })
	best := candidates[0]
	manager.consumeTokenLocked(best.account.Email)
	best.account.LastUsedMS = manager.now().UnixMilli()
	manager.currentIndex = best.index
	return Selection{Account: best.account}
}

func (manager *Manager) usableLocked(account *Account, model string) bool {
	if account == nil || !account.Enabled || account.IsInvalid {
		return false
	}
	now := manager.now().UnixMilli()
	if account.CoolingDownUntilMS > now {
		return false
	}
	if limit := account.ModelRateLimits[rateLimitModelKey(model)]; limit != nil && limit.IsRateLimited && limit.ResetTimeMS > now {
		return false
	}
	return true
}

func (manager *Manager) clearExpiredLocked() {
	now := manager.now().UnixMilli()
	for _, account := range manager.accounts {
		if account.CoolingDownUntilMS <= now {
			account.CoolingDownUntilMS = 0
			account.CooldownReason = ""
		}
		for model, limit := range account.ModelRateLimits {
			if limit == nil || limit.ResetTimeMS <= now {
				delete(account.ModelRateLimits, model)
			}
		}
	}
	cutoff := now - manager.sharedThrottleWindow.Milliseconds()
	for key, entry := range manager.modelThrottles {
		if entry == nil {
			delete(manager.modelThrottles, key)
			continue
		}
		if entry.untilMS > 0 && entry.untilMS <= now {
			entry.untilMS = 0
		}
		for email, at := range entry.recent {
			if at < cutoff {
				delete(entry.recent, email)
			}
		}
		if entry.untilMS <= now && len(entry.recent) == 0 {
			delete(manager.modelThrottles, key)
		}
	}
}

func (manager *Manager) healthScoreLocked(email string) float64 {
	record, exists := manager.health[email]
	hs := manager.selectionConfig.HealthScore
	initial := mapFloat(hs, "initial", 70)
	recovery := mapFloat(hs, "recoveryPerHour", 10)
	maxScore := mapFloat(hs, "maxScore", 100)
	if !exists {
		return initial
	}
	hours := manager.now().Sub(record.LastUpdated).Hours()
	return min(maxScore, record.Score+hours*recovery)
}

func (manager *Manager) recordSuccessLocked(email string) {
	hs := manager.selectionConfig.HealthScore
	reward := mapFloat(hs, "successReward", 1)
	maxScore := mapFloat(hs, "maxScore", 100)
	manager.health[email] = healthRecord{
		Score:       min(maxScore, manager.healthScoreLocked(email)+reward),
		LastUpdated: manager.now(),
	}
}

func (manager *Manager) recordRateLimitLocked(email string) {
	hs := manager.selectionConfig.HealthScore
	penalty := mapFloat(hs, "rateLimitPenalty", -10)
	var newScore float64
	if penalty < 0 {
		newScore = manager.healthScoreLocked(email) + penalty
	} else {
		newScore = manager.healthScoreLocked(email) - penalty
	}
	manager.health[email] = healthRecord{
		Score:       max(0, newScore),
		LastUpdated: manager.now(),
	}
}

func (manager *Manager) recordFailureLocked(email string) {
	hs := manager.selectionConfig.HealthScore
	penalty := mapFloat(hs, "failurePenalty", -20)
	var newScore float64
	if penalty < 0 {
		newScore = manager.healthScoreLocked(email) + penalty
	} else {
		newScore = manager.healthScoreLocked(email) - penalty
	}
	manager.health[email] = healthRecord{
		Score:       max(0, newScore),
		LastUpdated: manager.now(),
	}
}

func (manager *Manager) tokensLocked(email string) float64 {
	tb := manager.selectionConfig.TokenBucket
	maxTokens := mapFloat(tb, "maxTokens", 50)
	if maxTokens <= 0 {
		maxTokens = 50
	}
	rate := mapFloat(tb, "tokensPerMinute", 6)
	initial := mapFloat(tb, "initialTokens", maxTokens)
	bucket, exists := manager.buckets[email]
	if !exists {
		return initial
	}
	return min(maxTokens, bucket.Tokens+manager.now().Sub(bucket.LastUpdated).Minutes()*rate)
}

func (manager *Manager) consumeTokenLocked(email string) {
	manager.buckets[email] = tokenBucket{
		Tokens:      max(0, manager.tokensLocked(email)-1),
		LastUpdated: manager.now(),
	}
}

func modelQuotaFor(account *Account, model string) (ModelQuota, bool) {
	if account == nil || account.Quota.Models == nil {
		return ModelQuota{}, false
	}
	for _, candidate := range ModelKeyCandidates(model) {
		if q, exists := account.Quota.Models[candidate]; exists {
			return q, true
		}
	}
	return ModelQuota{}, false
}

func modelThresholdFor(account *Account, model string) (float64, bool) {
	if account == nil || account.ModelThreshold == nil {
		return 0, false
	}
	for _, candidate := range ModelKeyCandidates(model) {
		if v, exists := account.ModelThreshold[candidate]; exists && v > 0 {
			return v, true
		}
	}
	return 0, false
}

func parseQuotaResetTime(resetTime string) (time.Time, bool) {
	if resetTime == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if parsed, err := time.Parse(layout, resetTime); err == nil {
			return parsed, true
		}
	}
	return time.Time{}, false
}

// quotaWindowOpen reports whether the record's reset time is still ahead of now,
// and whether a reset time was known at all.
func quotaWindowOpen(quota ModelQuota, now time.Time) (open bool, known bool) {
	parsed, ok := parseQuotaResetTime(quota.ResetTime)
	if !ok {
		return false, false
	}
	return now.Before(parsed), true
}

// quotaUsableLocked reports whether this quota record may be trusted.
//
// An EXHAUSTED record stays authoritative until its window resets, even once it
// falls outside the quotaFresh window — that is the point of tracking ResetTime.
// Every other record, including one reporting spare capacity, must still be
// fresh: otherwise a hours-old "100% remaining" snapshot reads as live capacity
// and the scheduler routes traffic into a 429.
func (manager *Manager) quotaUsableLocked(quota ModelQuota, lastChecked any, threshold float64) bool {
	now := manager.now()
	if open, known := quotaWindowOpen(quota, now); known && open &&
		quota.RemainingFraction != nil && *quota.RemainingFraction <= threshold {
		return true
	}
	return quotaFresh(lastChecked, now)
}

func (manager *Manager) quotaCriticalLocked(account *Account, model string) bool {
	quota, exists := modelQuotaFor(account, model)
	if !exists || quota.RemainingFraction == nil {
		return false
	}

	threshold := 0.05
	if manager.globalQuotaThreshold > 0 {
		threshold = manager.globalQuotaThreshold
	} else if qCfg := manager.selectionConfig.Quota; qCfg != nil {
		threshold = mapFloat(qCfg, "criticalThreshold", 0.05)
	}
	if value, ok := modelThresholdFor(account, model); ok {
		threshold = value
	} else if account.QuotaThreshold != nil && *account.QuotaThreshold > 0 {
		threshold = *account.QuotaThreshold
	}

	if !manager.quotaUsableLocked(quota, account.Quota.LastChecked, threshold) {
		return false
	}
	return *quota.RemainingFraction <= threshold
}

func (manager *Manager) scoreLocked(account *Account, model string) float64 {
	w := manager.selectionConfig.Weights
	wHealth := mapFloat(w, "health", 2)
	wTokens := mapFloat(w, "tokens", 5)
	wQuota := mapFloat(w, "quota", 3)
	wLru := mapFloat(w, "lru", 0.1)

	tb := manager.selectionConfig.TokenBucket
	maxTokens := mapFloat(tb, "maxTokens", 50)
	if maxTokens <= 0 {
		maxTokens = 50
	}

	health := manager.healthScoreLocked(account.Email) * wHealth
	tokens := manager.tokensLocked(account.Email) / maxTokens * 100 * wTokens
	quotaScore := 50.0
	if quota, exists := modelQuotaFor(account, model); exists && quota.RemainingFraction != nil {
		quotaScore = *quota.RemainingFraction * 100
		if !quotaFresh(account.Quota.LastChecked, manager.now()) {
			quotaScore *= .9
		}
	}
	lru := min(float64(time.Hour.Milliseconds()), float64(manager.now().UnixMilli()-account.LastUsedMS)) / 1000 * wLru
	return health + tokens + quotaScore*wQuota + lru
}

func normalizeStrategy(strategy string) string {
	switch strings.ToLower(strategy) {
	case StrategySticky:
		return StrategySticky
	case StrategyRoundRobin, "roundrobin":
		return StrategyRoundRobin
	default:
		return DefaultStrategy
	}
}

func parseMilliseconds(raw json.RawMessage) int64 {
	if len(raw) == 0 || string(raw) == "null" {
		return 0
	}
	var number float64
	if err := json.Unmarshal(raw, &number); err == nil {
		return int64(number)
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		if parsed, err := time.Parse(time.RFC3339Nano, text); err == nil {
			return parsed.UnixMilli()
		}
	}
	return 0
}

func quotaFresh(value any, now time.Time) bool {
	switch typed := value.(type) {
	case float64:
		return now.UnixMilli()-int64(typed) < (5 * time.Minute).Milliseconds()
	case string:
		parsed, err := time.Parse(time.RFC3339Nano, typed)
		return err == nil && now.Sub(parsed) < 5*time.Minute
	default:
		return false
	}
}

// Save persists the account configuration to disk.
func Save(path string, accounts []*Account, settings map[string]any, activeIndex int) error {
	if path == "" {
		var err error
		path, err = DefaultConfigPath()
		if err != nil {
			return err
		}
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}

	diskAccounts := make([]diskAccount, 0, len(accounts))
	for _, acc := range accounts {
		enabled := acc.Enabled
		da := diskAccount{
			Email:                acc.Email,
			Source:               acc.Source,
			Enabled:              &enabled,
			DBPath:               acc.DBPath,
			RefreshToken:         acc.RefreshToken,
			APIKey:               acc.APIKey,
			AgyTokenPath:         acc.AgyTokenPath,
			ProjectID:            acc.ProjectID,
			Subscription:         acc.Subscription,
			Quota:                acc.Quota,
			QuotaThreshold:       acc.QuotaThreshold,
			ModelQuotaThresholds: acc.ModelThreshold,
			Credits:              acc.Credits,
			IsInvalid:            acc.IsInvalid,
			InvalidReason:        acc.InvalidReason,
			VerifyURL:            acc.VerifyURL,
			ModelRateLimits:      acc.ModelRateLimits,
		}
		if acc.LastUsedMS > 0 {
			da.LastUsed = json.RawMessage(fmt.Sprintf("%d", acc.LastUsedMS))
		}
		diskAccounts = append(diskAccounts, da)
	}

	doc := struct {
		Accounts    []diskAccount  `json:"accounts"`
		Settings    map[string]any `json:"settings"`
		ActiveIndex int            `json:"activeIndex"`
	}{
		Accounts:    diskAccounts,
		Settings:    settings,
		ActiveIndex: activeIndex,
	}

	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal account config: %w", err)
	}

	tmpFile := path + ".tmp"
	if err := os.WriteFile(tmpFile, data, 0600); err != nil {
		return fmt.Errorf("write temp account config: %w", err)
	}
	return os.Rename(tmpFile, path)
}

func (manager *Manager) SaveOAuthAccount(email, refreshToken string) error {
	acc := &Account{
		Email:        email,
		Source:       "oauth",
		RefreshToken: refreshToken,
		Enabled:      true,
	}
	return manager.AddOrUpdateAccount(acc)
}

func cloneAccount(acc *Account) *Account {
	if acc == nil {
		return nil
	}
	cloned := *acc
	if acc.ModelRateLimits != nil {
		cloned.ModelRateLimits = make(map[string]*RateLimit, len(acc.ModelRateLimits))
		for k, v := range acc.ModelRateLimits {
			if v != nil {
				val := *v
				cloned.ModelRateLimits[k] = &val
			}
		}
	}
	if acc.ModelThreshold != nil {
		cloned.ModelThreshold = make(map[string]float64, len(acc.ModelThreshold))
		for k, v := range acc.ModelThreshold {
			cloned.ModelThreshold[k] = v
		}
	}
	if acc.Quota.Models != nil {
		clonedModels := make(map[string]ModelQuota, len(acc.Quota.Models))
		for k, v := range acc.Quota.Models {
			clonedModels[k] = v
		}
		cloned.Quota.Models = clonedModels
	}
	if acc.Credits != nil {
		clonedCredits := make(map[string]int64, len(acc.Credits))
		for k, v := range acc.Credits {
			clonedCredits[k] = v
		}
		cloned.Credits = clonedCredits
	}
	return &cloned
}

func (manager *Manager) SaveToDisk() error {
	manager.mu.RLock()
	path := manager.configPath
	if path == "" {
		manager.mu.RUnlock()
		return nil
	}
	accounts := make([]*Account, len(manager.accounts))
	for i, acc := range manager.accounts {
		accounts[i] = cloneAccount(acc)
	}
	settings := manager.settings
	activeIndex := manager.currentIndex
	manager.mu.RUnlock()

	return Save(path, accounts, settings, activeIndex)
}

func (manager *Manager) ConfigPath() string {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	return manager.configPath
}

func (manager *Manager) SetConfigPath(path string) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.configPath = path
}

func (manager *Manager) GetAllAccounts() []*Account {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	result := make([]*Account, len(manager.accounts))
	for i, acc := range manager.accounts {
		result[i] = cloneAccount(acc)
	}
	return result
}

func (manager *Manager) GetSettings() map[string]any {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	if manager.settings == nil {
		return make(map[string]any)
	}
	result := make(map[string]any, len(manager.settings))
	for k, v := range manager.settings {
		result[k] = v
	}
	return result
}

func (manager *Manager) GetStrategy() string {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	return manager.strategy
}

func (manager *Manager) SetStrategy(strategy string) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.strategy = normalizeStrategy(strategy)
	manager.selectionConfig.Strategy = manager.strategy
}

func (manager *Manager) SetSelectionConfig(cfg config.AccountSelectionConfig, globalThreshold float64) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.selectionConfig = cfg
	manager.globalQuotaThreshold = globalThreshold
	if cfg.Strategy != "" {
		manager.strategy = normalizeStrategy(cfg.Strategy)
	}
}

func (manager *Manager) GetSelectionConfig() config.AccountSelectionConfig {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	return manager.selectionConfig
}

func (manager *Manager) GlobalQuotaThreshold() float64 {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	return manager.globalQuotaThreshold
}

func (manager *Manager) ActiveIndex() int {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	return manager.currentIndex
}

func (manager *Manager) ClearInvalid(email string) {
	manager.mu.Lock()
	for _, acc := range manager.accounts {
		if acc.Email == email {
			acc.IsInvalid = false
			acc.InvalidReason = ""
			acc.VerifyURL = ""
			break
		}
	}
	manager.mu.Unlock()
	_ = manager.SaveToDisk()
}

func (manager *Manager) ClearTokenCache(email string) {
	manager.mu.Lock()
	delete(manager.projects, email)
	delete(manager.health, email)
	delete(manager.buckets, email)
	manager.mu.Unlock()
}

func (manager *Manager) ClearProjectCache(email string) {
	manager.mu.Lock()
	delete(manager.projects, email)
	manager.mu.Unlock()
}

func (manager *Manager) SetAccountEnabled(email string, enabled bool) error {
	manager.mu.Lock()
	found := false
	for _, acc := range manager.accounts {
		if acc.Email == email {
			acc.Enabled = enabled
			found = true
			break
		}
	}
	manager.mu.Unlock()
	if !found {
		return fmt.Errorf("account %s not found", email)
	}
	return manager.SaveToDisk()
}

func (manager *Manager) RemoveAccount(email string) error {
	manager.mu.Lock()
	index := -1
	for i, acc := range manager.accounts {
		if acc.Email == email {
			index = i
			break
		}
	}
	if index == -1 {
		manager.mu.Unlock()
		return fmt.Errorf("account %s not found", email)
	}
	manager.accounts = append(manager.accounts[:index], manager.accounts[index+1:]...)
	if manager.currentIndex >= len(manager.accounts) {
		manager.currentIndex = max(0, len(manager.accounts)-1)
	}
	delete(manager.projects, email)
	delete(manager.health, email)
	delete(manager.buckets, email)
	manager.mu.Unlock()
	return manager.SaveToDisk()
}

func (manager *Manager) AddOrUpdateAccount(account *Account) error {
	if account == nil || account.Email == "" {
		return errors.New("invalid account data: email is required")
	}
	manager.mu.Lock()
	if account.Source == "" {
		account.Source = "oauth"
	}
	if account.ModelRateLimits == nil {
		account.ModelRateLimits = make(map[string]*RateLimit)
	}
	if account.ModelThreshold == nil {
		account.ModelThreshold = make(map[string]float64)
	}
	if account.Quota.Models == nil {
		account.Quota.Models = make(map[string]ModelQuota)
	}

	found := false
	for i, existing := range manager.accounts {
		if existing.Email == account.Email {
			account.Enabled = true
			account.IsInvalid = false
			account.InvalidReason = ""
			account.VerifyURL = ""
			manager.accounts[i] = account
			found = true
			break
		}
	}
	if !found {
		account.Enabled = true
		manager.accounts = append(manager.accounts, account)
	}
	manager.mu.Unlock()
	return manager.SaveToDisk()
}

func (manager *Manager) UpdateThresholds(email string, quotaThreshold *float64, modelThreshold map[string]float64) error {
	manager.mu.Lock()
	found := false
	for _, acc := range manager.accounts {
		if acc.Email == email {
			acc.QuotaThreshold = quotaThreshold
			if modelThreshold != nil {
				acc.ModelThreshold = modelThreshold
			}
			found = true
			break
		}
	}
	manager.mu.Unlock()
	if !found {
		return fmt.Errorf("account %s not found", email)
	}
	return manager.SaveToDisk()
}

func (manager *Manager) UpdateAccountQuota(email string, quota Quota, subscription *Subscription) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	for _, acc := range manager.accounts {
		if acc.Email == email {
			acc.Quota = quota
			if subscription != nil {
				acc.Subscription = *subscription
			}
			break
		}
	}
}

// MergeQuotaFraction additively records one live quota reading under key.
// Quota-summary buckets are authoritative over static catalog fractions, so a
// merged reading overwrites whatever fetchAvailableModels stored. A nil
// fraction paired with a reset time records exhaustion (0.0), mirroring
// updateAccountQuota. Keys with neither fraction nor reset are ignored.
func (manager *Manager) MergeQuotaFraction(email, key string, fraction *float64, resetTime string) {
	manager.mergeQuota(email, key, fraction, resetTime, false)
}

// MergeQuotaPool records one live quota-pool reading under key. Pools are
// shared upstream buckets (e.g. gemini-5h, 3p-weekly) — never models — so
// they are kept out of Quota.Models and cannot surface as model rows.
func (manager *Manager) MergeQuotaPool(email, key string, fraction *float64, resetTime string) {
	manager.mergeQuota(email, key, fraction, resetTime, true)
}

// quotaPoolsForModel names the shared upstream buckets a model is charged
// against. Upstream publishes the grouping only as prose
// ("Models within this group: Gemini Flash, Gemini Pro"), never as model ids,
// so the family prefix is the only available mapping. A model outside both
// published groups (the chat_* and tab_* internal ids) maps to nothing.
func quotaPoolsForModel(model string) []string {
	switch {
	case strings.HasPrefix(model, "gemini-"):
		return []string{"gemini-weekly", "gemini-5h"}
	case strings.HasPrefix(model, "claude-"), strings.HasPrefix(model, "gpt-oss"):
		return []string{"3p-weekly", "3p-5h"}
	default:
		return nil
	}
}

// MarkQuotaExhausted records an upstream "individual quota reached" 429 for
// one model: fraction 0, held until resetTime. Live quota readings cannot
// clobber it before then, because upstream reports the exhausted model at
// remainingFraction 1 for the whole cooldown.
//
// The model's shared pools follow it down, so the group bars stop reading
// 100% while every request against the group is being refused. Only pools a
// live refresh already wrote are touched: an exhaustion must not invent pool
// rows for an account that was never polled. The 429's own reset time is
// carried onto the pool, which is deliberately not the pool's published
// window — upstream gives no per-pool consumption signal to reconcile with,
// and the 429 reset is the one time that is known to be real.
func (manager *Manager) MarkQuotaExhausted(email, key, resetTime string) {
	key = strings.ToLower(strings.TrimSpace(key))
	if key == "" || resetTime == "" {
		return
	}
	reset, err := time.Parse(time.RFC3339, resetTime)
	if err != nil {
		return
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	for _, acc := range manager.accounts {
		if acc.Email != email {
			continue
		}
		if acc.Quota.Models == nil {
			acc.Quota.Models = make(map[string]ModelQuota)
		}
		exhausted := func() ModelQuota {
			zero := 0.0
			return ModelQuota{RemainingFraction: &zero, ResetTime: resetTime, ExhaustedUntilMS: reset.UnixMilli()}
		}
		acc.Quota.Models[key] = exhausted()
		for _, pool := range quotaPoolsForModel(key) {
			if _, seen := acc.Quota.Pools[pool]; !seen {
				continue
			}
			acc.Quota.Pools[pool] = exhausted()
		}
		acc.Quota.LastChecked = manager.now().UnixMilli()
		break
	}
}

func (manager *Manager) mergeQuota(email, key string, fraction *float64, resetTime string, pools bool) {
	if key == "" || (fraction == nil && resetTime == "") {
		return
	}
	if fraction == nil {
		zero := 0.0
		fraction = &zero
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	for _, acc := range manager.accounts {
		if acc.Email != email {
			continue
		}
		target := &acc.Quota.Models
		if pools {
			target = &acc.Quota.Pools
		}
		if *target == nil {
			*target = make(map[string]ModelQuota)
		}
		if previous, seen := (*target)[key]; seen && previous.ExhaustedUntilMS > manager.now().UnixMilli() {
			// A recorded exhaustion outranks the live reading until it expires.
			break
		}
		(*target)[key] = ModelQuota{RemainingFraction: fraction, ResetTime: resetTime}
		acc.Quota.LastChecked = manager.now().UnixMilli()
		break
	}
}

// UpdateAccountCredits stores absolute remaining credit balances by
// credit-type name. Empty updates are ignored so a response without a credit
// signal never wipes the last known balances.
func (manager *Manager) UpdateAccountCredits(email string, balances map[string]int64) {
	if len(balances) == 0 {
		return
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	for _, acc := range manager.accounts {
		if acc.Email != email {
			continue
		}
		if acc.Credits == nil {
			acc.Credits = make(map[string]int64, len(balances))
		}
		for k, v := range balances {
			acc.Credits[k] = v
		}
		break
	}
}

func (manager *Manager) Reload(path string) error {
	if path == "" {
		path = manager.ConfigPath()
	}
	file, err := Load(path)
	if err != nil {
		return err
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.configPath = path
	manager.accounts = file.Accounts
	manager.settings = file.Settings
	manager.currentIndex = file.ActiveIndex
	return nil
}

func (manager *Manager) GetStatus() map[string]any {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.clearExpiredLocked()

	now := manager.now().UnixMilli()
	availableCount := 0
	invalidCount := 0
	rateLimitedCount := 0

	accountList := make([]map[string]any, 0, len(manager.accounts))
	for _, acc := range manager.accounts {
		if acc.Enabled && !acc.IsInvalid {
			availableCount++
		}
		if acc.IsInvalid {
			invalidCount++
		}
		isRateLimited := false
		for _, rl := range acc.ModelRateLimits {
			if rl != nil && rl.IsRateLimited && rl.ResetTimeMS > now {
				isRateLimited = true
				break
			}
		}
		if isRateLimited {
			rateLimitedCount++
		}

		limits := make(map[string]any)
		for mID, q := range acc.Quota.Models {
			remStr := "N/A"
			if q.RemainingFraction != nil {
				remStr = fmt.Sprintf("%d%%", int(*q.RemainingFraction*100))
			}
			limits[mID] = map[string]any{
				"remaining":         remStr,
				"remainingFraction": q.RemainingFraction,
				"resetTime":         q.ResetTime,
			}
		}

		item := map[string]any{
			"email":                acc.Email,
			"source":               acc.Source,
			"enabled":              acc.Enabled,
			"projectId":            acc.ProjectID,
			"modelRateLimits":      acc.ModelRateLimits,
			"isInvalid":            acc.IsInvalid,
			"invalidReason":        acc.InvalidReason,
			"verifyUrl":            acc.VerifyURL,
			"lastUsed":             acc.LastUsedMS,
			"subscription":         acc.Subscription,
			"quota":                acc.Quota,
			"credits":              acc.Credits,
			"limits":               limits,
			"quotaThreshold":       acc.QuotaThreshold,
			"modelQuotaThresholds": acc.ModelThreshold,
		}
		accountList = append(accountList, item)
	}

	summary := fmt.Sprintf("%d total, %d available, %d rate-limited, %d invalid",
		len(manager.accounts), availableCount, rateLimitedCount, invalidCount)

	return map[string]any{
		"total":       len(manager.accounts),
		"available":   availableCount,
		"rateLimited": rateLimitedCount,
		"invalid":     invalidCount,
		"summary":     summary,
		"accounts":    accountList,
	}
}

func (manager *Manager) GetStrategyHealthData() map[string]any {
	manager.mu.RLock()
	defer manager.mu.RUnlock()

	accounts := make([]map[string]any, 0, len(manager.accounts))
	for _, acc := range manager.accounts {
		accounts = append(accounts, map[string]any{
			"email":       acc.Email,
			"healthScore": manager.healthScoreLocked(acc.Email),
			"tokens":      manager.tokensLocked(acc.Email),
			"failures":    acc.ConsecutiveFailure,
			"isInvalid":   acc.IsInvalid,
			"enabled":     acc.Enabled,
			"lastUsed":    acc.LastUsedMS,
			"rateLimits":  acc.ModelRateLimits,
		})
	}

	return map[string]any{
		"strategy": manager.strategy,
		"trackers": map[string]any{
			"accounts": accounts,
		},
	}
}
