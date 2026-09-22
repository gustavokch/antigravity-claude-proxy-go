package accounts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"antigravity-go-proxy/internal/auth"
	"antigravity-go-proxy/internal/cloudcode"
	"antigravity-go-proxy/internal/config"
	proxyformat "antigravity-go-proxy/internal/format"
	"antigravity-go-proxy/internal/modelcatalog"
)

type CloudClient interface {
	LoadCodeAssist(context.Context, string) (cloudcode.Response, error)
	FetchAvailableModels(context.Context, string) (cloudcode.Response, error)
	StreamGenerateContent(context.Context, any, cloudcode.RequestOptions, func(cloudcode.SSEEvent) error) (cloudcode.Response, error)
}

type Resolver interface {
	Resolve(context.Context, *Account) (auth.Credentials, error)
	Invalidate(string)
}

// quotaSummaryFetcher and userQuotaFetcher are optional CloudClient
// capabilities. *cloudcode.Client implements both; test fakes that do not
// simply skip the live-quota refresh and keep catalog fractions.
type quotaSummaryFetcher interface {
	RetrieveUserQuotaSummary(context.Context, string) (cloudcode.Response, error)
}

type userQuotaFetcher interface {
	RetrieveUserQuota(context.Context, string) (cloudcode.Response, error)
}

type SleepFunc func(context.Context, time.Duration) error

type DispatcherOptions struct {
	Manager                  *Manager
	Resolver                 Resolver
	Builder                  *proxyformat.Builder
	NewClient                func(string) CloudClient
	ProjectID                string
	MaxRetries               int
	MaxWait                  time.Duration
	CapacityBackoffs         []time.Duration
	MaxCapacityRetries       int
	SwitchDelay              time.Duration
	RequestThrottlingEnabled bool
	RequestDelay             time.Duration
	Sleep                    SleepFunc
	Now                      func() time.Time
	ModelCacheTTL            time.Duration
	// Forensics429 records every upstream 429 verbatim (append-only JSONL)
	// when non-nil and enabled. Nil disables recording.
	Forensics429 *Forensics429Recorder
	// Random supplies the jitter fraction for Decorrelate. Nil uses
	// math/rand/v2, which is safe for concurrent use. Tests inject a constant.
	Random func() float64
}

type accountClient struct {
	token  string
	client CloudClient
}

type Dispatcher struct {
	manager                  *Manager
	resolver                 Resolver
	builder                  *proxyformat.Builder
	newClient                func(string) CloudClient
	projectID                string
	maxRetries               int
	maxWait                  time.Duration
	capacityBackoffs         []time.Duration
	maxCapacityRetries       int
	switchDelay              time.Duration
	requestThrottlingEnabled bool
	requestDelay             time.Duration
	sleep                    SleepFunc
	now                      func() time.Time
	random                   func() float64
	modelCacheTTL            time.Duration
	forensics429             *Forensics429Recorder
	meter                    *throttleMeter

	mu        sync.RWMutex
	clients   map[string]accountClient
	catalog   *modelcatalog.Catalog
	catalogAt time.Time
	// modelsFetch is the shared in-flight catalog fetch; guarded by mu.
	modelsFetch *modelFetchCall
	// liveQuotaRefreshMS is the last live-quota refresh per account email,
	// guarded by mu; it throttles post-request quota RPCs.
	liveQuotaRefreshMS map[string]int64
}

func NewDispatcher(options DispatcherOptions) (*Dispatcher, error) {
	if options.Manager == nil || options.Resolver == nil || options.NewClient == nil {
		return nil, errors.New("account manager, credential resolver, and Cloud Code client factory are required")
	}
	if options.Builder == nil {
		options.Builder = proxyformat.NewBuilder()
	}
	if options.MaxRetries <= 0 {
		options.MaxRetries = 5
	}
	if options.MaxWait <= 0 {
		options.MaxWait = 2 * time.Minute
	}
	if len(options.CapacityBackoffs) == 0 {
		options.CapacityBackoffs = []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 30 * time.Second, time.Minute}
	}
	if options.MaxCapacityRetries <= 0 {
		options.MaxCapacityRetries = 5
	}
	if options.SwitchDelay == 0 {
		options.SwitchDelay = 5 * time.Second
	}
	if options.Sleep == nil {
		options.Sleep = sleepContext
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.ModelCacheTTL <= 0 {
		options.ModelCacheTTL = 5 * time.Minute
	}
	if options.Random == nil {
		options.Random = rand.Float64
	}
	return &Dispatcher{
		manager:                  options.Manager,
		resolver:                 options.Resolver,
		builder:                  options.Builder,
		newClient:                options.NewClient,
		projectID:                options.ProjectID,
		maxRetries:               options.MaxRetries,
		maxWait:                  options.MaxWait,
		capacityBackoffs:         options.CapacityBackoffs,
		maxCapacityRetries:       options.MaxCapacityRetries,
		switchDelay:              options.SwitchDelay,
		requestThrottlingEnabled: options.RequestThrottlingEnabled,
		requestDelay:             options.RequestDelay,
		sleep:                    options.Sleep,
		now:                      options.Now,
		random:                   options.Random,
		modelCacheTTL:            options.ModelCacheTTL,
		forensics429:             options.Forensics429,
		meter:                    newThrottleMeter(nil),
		clients:                  make(map[string]accountClient),
		liveQuotaRefreshMS:       make(map[string]int64),
	}, nil
}

func (dispatcher *Dispatcher) UpdateConfig(cfg config.Config) {
	dispatcher.mu.Lock()
	defer dispatcher.mu.Unlock()

	if cfg.MaxRetries > 0 {
		dispatcher.maxRetries = cfg.MaxRetries
	}
	if cfg.MaxWaitBeforeErrorMs > 0 {
		dispatcher.maxWait = time.Duration(cfg.MaxWaitBeforeErrorMs) * time.Millisecond
	}
	if cfg.MaxCapacityRetries > 0 {
		dispatcher.maxCapacityRetries = cfg.MaxCapacityRetries
	}
	if cfg.SwitchAccountDelayMs > 0 {
		dispatcher.switchDelay = time.Duration(cfg.SwitchAccountDelayMs) * time.Millisecond
	}
	if len(cfg.CapacityBackoffTiersMs) > 0 {
		backoffs := make([]time.Duration, len(cfg.CapacityBackoffTiersMs))
		for i, ms := range cfg.CapacityBackoffTiersMs {
			backoffs[i] = time.Duration(ms) * time.Millisecond
		}
		dispatcher.capacityBackoffs = backoffs
	}
	dispatcher.requestThrottlingEnabled = cfg.RequestThrottlingEnabled
	if cfg.RequestDelayMs > 0 {
		dispatcher.requestDelay = time.Duration(cfg.RequestDelayMs) * time.Millisecond
	} else {
		dispatcher.requestDelay = 0
	}
	if cfg.Upstream429ForensicsEnabled {
		if dispatcher.forensics429 == nil || !dispatcher.forensics429.Enabled() {
			dispatcher.forensics429 = NewForensics429Recorder(filepath.Join(config.GetConfigDir(), "forensics", "upstream-429.jsonl"))
		}
	} else {
		dispatcher.forensics429 = nil
	}

	if dispatcher.requestThrottlingEnabled && dispatcher.requestDelay >= fetchModelsTimeout {
		slog.Warn("[Dispatcher] requestDelayMs paces every generation slower than one request per catalog-fetch budget; lower it unless this is deliberate",
			"requestDelayMs", dispatcher.requestDelay.Milliseconds(),
			"fetchModelsTimeoutMs", fetchModelsTimeout.Milliseconds())
	}
}

// fetchModelsTimeout bounds a decoupled catalog fetch, including any OAuth
// token refresh it triggers, so work abandoned by one disconnected client
// still completes and cannot run forever.
const fetchModelsTimeout = 30 * time.Second

// modelFetchCall is one shared catalog fetch. Concurrent FetchAvailableModels
// callers attach to the same call; done is closed once response and err are
// set, so any number of callers — or none, if they all disconnected — can
// observe the result.
type modelFetchCall struct {
	done     chan struct{}
	response cloudcode.Response
	err      error
}

// FetchAvailableModels returns the upstream model catalog. All concurrent
// callers share a single in-flight fetch running on a background context
// bounded by fetchModelsTimeout (see internal/openrouter/client.go for the
// same pattern). When the caller's context ends first, the caller receives
// its context error while the shared fetch continues and still refreshes
// the catalog cache and account quotas.
func (dispatcher *Dispatcher) FetchAvailableModels(ctx context.Context) (cloudcode.Response, error) {
	call := dispatcher.startModelFetch()
	select {
	case <-call.done:
		return call.response, call.err
	case <-ctx.Done():
		return cloudcode.Response{}, ctx.Err()
	}
}

// startModelFetch returns the in-progress shared fetch, starting one when no
// fetch is running. Writing response/err before closing done makes the
// result visible to every waiter without extra locking.
func (dispatcher *Dispatcher) startModelFetch() *modelFetchCall {
	dispatcher.mu.Lock()
	if call := dispatcher.modelsFetch; call != nil {
		dispatcher.mu.Unlock()
		return call
	}
	call := &modelFetchCall{done: make(chan struct{})}
	dispatcher.modelsFetch = call
	dispatcher.mu.Unlock()

	fetchCtx, cancel := context.WithTimeout(context.Background(), fetchModelsTimeout)
	go func() {
		// cancel() lives inside the goroutine: cancelling from the caller
		// would abort the fetch the moment an abandoned caller returns.
		defer cancel()
		call.response, call.err = dispatcher.fetchAvailableModels(fetchCtx)
		close(call.done)
		dispatcher.mu.Lock()
		if dispatcher.modelsFetch == call {
			dispatcher.modelsFetch = nil
		}
		dispatcher.mu.Unlock()
	}()
	return call
}

// catalogRetryPauseCeiling caps the spacing between catalog-fetch retries. The
// full requestDelayMs cannot apply here: a delay above fetchModelsTimeout would
// guarantee "context deadline exceeded" on every refresh. Dropping the pause
// entirely is the other extreme — the loop would then fire one immediate
// list-models call per account during a 429 wave.
const catalogRetryPauseCeiling = time.Second

func (dispatcher *Dispatcher) fetchAvailableModels(ctx context.Context) (cloudcode.Response, error) {
	var lastError error
	for attempt := 0; attempt < max(dispatcher.maxRetries, dispatcher.manager.Count()+1); attempt++ {
		// Retries across accounts keep a bounded spacing rather than the full
		// requestDelayMs: this fetch runs on a context bounded by
		// fetchModelsTimeout, so a delay above that budget would guarantee
		// "context deadline exceeded" on every catalog refresh (and through
		// it, on every Cloud Code request past the catalog TTL). The first
		// attempt pays nothing.
		if attempt > 0 {
			dispatcher.mu.RLock()
			throttling := dispatcher.requestThrottlingEnabled
			delay := dispatcher.requestDelay
			dispatcher.mu.RUnlock()
			if throttling && delay > 0 {
				if err := dispatcher.sleep(ctx, min(delay, catalogRetryPauseCeiling)); err != nil {
					return cloudcode.Response{}, err
				}
			}
		}
		selection := dispatcher.manager.Select("")
		if selection.Account == nil {
			return cloudcode.Response{}, errors.New("no accounts available")
		}
		credentials, err := dispatcher.resolver.Resolve(ctx, selection.Account)
		if err != nil {
			// For model catalog refresh, use MarkFailure (not MarkInvalid) — a
			// transient credential error should not permanently disable an account
			// based solely on a background list-models call.
			dispatcher.manager.MarkFailure(selection.Account, "")
			lastError = err
			continue
		}
		modelsClient := dispatcher.client(selection.Account, credentials.AccessToken)
		response, err := dispatcher.metered(selection.Account.Email, func() (cloudcode.Response, error) {
			return modelsClient.FetchAvailableModels(ctx, dispatcher.project(selection.Account))
		})
		if err == nil {
			dispatcher.manager.MarkSuccess(selection.Account, "")
			dispatcher.cacheCatalog(response.Body)
			dispatcher.updateAccountQuota(selection.Account, response.Body)
			dispatcher.refreshLiveQuota(ctx, selection.Account, modelsClient, dispatcher.project(selection.Account))
			return response, nil
		}
		lastError = err
		if !dispatcher.rotateForError(selection.Account, "", err) {
			return cloudcode.Response{}, err
		}
	}
	return cloudcode.Response{}, fmt.Errorf("model listing exhausted account retries: %w", lastError)
}

func (dispatcher *Dispatcher) StreamGenerateContent(ctx context.Context, request map[string]any, consume func(cloudcode.SSEEvent) error) (cloudcode.Response, error) {
	requestedModel, _ := request["model"].(string)
	stream, _ := request["stream"].(bool)
	slog.Info(fmt.Sprintf("[API] Request for model: %s, stream: %v", requestedModel, stream))

	modelDetails, err := dispatcher.resolveModel(ctx, requestedModel, request)
	if err != nil {
		return cloudcode.Response{}, err
	}
	if modelDetails.ID != requestedModel {
		slog.Info(fmt.Sprintf("[Server] Mapping model %s -> %s", requestedModel, modelDetails.ID))
	}
	request = cloneRequest(request)
	request["model"] = modelDetails.GetUpstreamID()
	model := modelDetails.ID
	maxAttempts := max(dispatcher.maxRetries, dispatcher.manager.Count()+1)
	var lastError error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		selection := dispatcher.manager.Select(model)
		if selection.Account == nil {
			if dispatcher.manager.AllInvalid() {
				return cloudcode.Response{}, errors.New("all accounts are invalid and require user intervention")
			}
			wait := selection.Wait
			if wait == 0 {
				wait = dispatcher.manager.MinWait(model)
			}
			if wait > 0 {
				slog.Warn(fmt.Sprintf("[Server] All accounts rate-limited for %s. Wait %s", model, wait.Round(time.Second)))
			}
			if wait > 0 && wait <= dispatcher.maxWait {
				if err := dispatcher.sleep(ctx, wait+500*time.Millisecond); err != nil {
					return cloudcode.Response{}, err
				}
				attempt--
				continue
			}
			if wait > dispatcher.maxWait {
				return cloudcode.Response{}, &RateLimitError{
					Model:      model,
					RetryAfter: wait,
					Shared:     dispatcher.manager.SharedThrottleWait(model) > 0,
				}
			}
			return cloudcode.Response{}, errors.New("no accounts available")
		}
		if selection.Wait > 0 {
			if err := dispatcher.sleep(ctx, selection.Wait); err != nil {
				return cloudcode.Response{}, err
			}
		}
		account := selection.Account
		credentials, err := dispatcher.resolver.Resolve(ctx, account)
		if err != nil {
			dispatcher.handleCredentialError(account, err)
			lastError = err
			continue
		}
		client := dispatcher.client(account, credentials.AccessToken)
		project, err := dispatcher.resolveProject(ctx, account, client)
		if err != nil {
			dispatcher.manager.MarkFailure(account, model)
			lastError = err
			continue
		}
		payload := dispatcher.builder.BuildCloudCodeRequestWithModel(request, project, credentials.Email, proxyformat.ModelOptions{
			SupportsThinking: modelDetails.SupportsThinking, ThinkingBudget: modelDetails.ThinkingBudget,
			MinThinkingBudget: modelDetails.MinThinkingBudget, MaxOutputTokens: modelDetails.MaxOutputTokens,
			ThinkingLevel: modelDetails.ThinkingLevel,
		})
		options := cloudcode.RequestOptions{}
		if proxyformat.GetModelFamily(model) == proxyformat.FamilyClaude && modelDetails.SupportsThinking {
			options.Headers = make(http.Header)
			options.Headers.Set("anthropic-beta", "interleaved-thinking-2025-05-14")
		}

		capacityAttempt := 0
		for {
			eventCount := 0
			dispatcher.mu.RLock()
			throttling := dispatcher.requestThrottlingEnabled
			delay := dispatcher.requestDelay
			dispatcher.mu.RUnlock()
			if throttling && delay > 0 {
				if err := dispatcher.sleep(ctx, delay); err != nil {
					return cloudcode.Response{}, err
				}
			}
			inFlight, priorMinute := dispatcher.meter.Begin(account.Email)
			// The release is deferred inside its own scope so a panic in the
			// consume callback cannot strand the account's in-flight count.
			// The wrapper also snoops the stream for remaining_credits: the
			// only per-request consumption signal upstream sends.
			var lastCredits []cloudcode.CreditBalance
			response, requestErr := func() (cloudcode.Response, error) {
				defer dispatcher.meter.End(account.Email)
				return client.StreamGenerateContent(ctx, payload, options, func(event cloudcode.SSEEvent) error {
					eventCount++
					if len(event.Data) > 0 {
						if balances, ok := cloudcode.ParseRemainingCredits(event.Data); ok {
							lastCredits = balances
						}
					}
					return consume(event)
				})
			}()
			if requestErr == nil {
				if len(lastCredits) > 0 {
					balances := make(map[string]int64, len(lastCredits))
					for _, b := range lastCredits {
						balances[b.CreditType] = b.Amount
					}
					dispatcher.manager.UpdateAccountCredits(account.Email, balances)
				}
				dispatcher.refreshLiveQuotaThrottled(ctx, account, client, project)
				if dispatcher.meter.TakeRecovery(account.Email) {
					dispatcher.recordRecovery(account, project, model, inFlight, priorMinute)
				}
				dispatcher.manager.MarkSuccess(account, model)
				cloudcode.SetExecutionMetadata(ctx, account.Email, project)
				return response, nil
			}
			lastError = requestErr
			if eventCount > 0 {
				cloudcode.SetExecutionMetadata(ctx, account.Email, project)
				return response, requestErr
			}
			upstreamError := findHTTPError(requestErr)
			if upstreamError != nil && isCapacityHTTPError(upstreamError) && capacityAttempt < dispatcher.maxCapacityRetries {
				wait := ParseResetTime(upstreamError.Header, upstreamError.Body, dispatcher.now())
				if wait == 0 {
					wait = dispatcher.capacityBackoffs[min(capacityAttempt, len(dispatcher.capacityBackoffs)-1)]
				}
				capacityAttempt++
				dispatcher.manager.IncrementFailure(account)
				if err := dispatcher.sleep(ctx, wait); err != nil {
					return cloudcode.Response{}, err
				}
				continue
			}
			if upstreamError != nil && upstreamError.StatusCode == http.StatusTooManyRequests {
				reason := ClassifyError(upstreamError.Body, upstreamError.StatusCode)
				reset := ParseResetTime(upstreamError.Header, upstreamError.Body, dispatcher.now())
				failures := dispatcher.manager.FailureCount(account)
				wait := Decorrelate(SmartBackoff(reason, reset, failures), dispatcher.random)
				if reason == ReasonCapacity && capacityAttempt >= dispatcher.maxCapacityRetries {
					dispatcher.manager.MarkRateLimited(account, model, 15*time.Second)
					break
				}
				if wait <= 10*time.Second && failures == 0 {
					dispatcher.manager.IncrementFailure(account)
					if err := dispatcher.sleep(ctx, max(reset, time.Second)); err != nil {
						return cloudcode.Response{}, err
					}
					continue
				}
				if wait > 10*time.Second && dispatcher.switchDelay > 0 {
					if err := dispatcher.sleep(ctx, dispatcher.switchDelay); err != nil {
						return cloudcode.Response{}, err
					}
				}
				slog.Warn("upstream 429", "model", model, "reason", reason, "wait", wait.Round(time.Second), "serverReset", reset, "failures", failures, "body", truncateBodyForLog(upstreamError.Body))
				dispatcher.record429(account, project, model, upstreamError, wait, failures, inFlight, priorMinute)
				dispatcher.manager.MarkRateLimited(account, model, wait)
				break
			}
			if dispatcher.rotateForError(account, model, requestErr) {
				break
			}
			return cloudcode.Response{}, requestErr
		}
	}
	return cloudcode.Response{}, fmt.Errorf("max retries exceeded: %w", lastError)
}

func (dispatcher *Dispatcher) resolveModel(ctx context.Context, requested string, request map[string]any) (modelcatalog.Model, error) {
	dispatcher.mu.RLock()
	catalog := dispatcher.catalog
	fresh := catalog != nil && dispatcher.now().Sub(dispatcher.catalogAt) < dispatcher.modelCacheTTL
	dispatcher.mu.RUnlock()
	if !fresh {
		response, err := dispatcher.FetchAvailableModels(ctx)
		if err != nil {
			// Degrade, do not fail: a stale catalog resolves models correctly
			// for every model that already existed. Returning here instead
			// turned any refresh failure into a 504 on every request past the
			// TTL. Only a total absence of catalog is fatal.
			if catalog == nil {
				return modelcatalog.Model{}, fmt.Errorf("refresh selectable models: %w", err)
			}
			slog.Warn("[Server] Model catalog refresh failed; serving the stale catalog", "error", err)
			return catalog.ResolveWithRequest(requested, request)
		}
		// fetchAvailableModels already parsed and cached the body via
		// cacheCatalog; read it back instead of parsing the body twice. The
		// parse remains the fallback for when the cached parse stored nothing,
		// so a malformed body still surfaces its decode error rather than a
		// nil dereference.
		if catalog = dispatcher.CachedCatalog(); catalog == nil {
			catalog, err = modelcatalog.Parse(response.Body)
			if err != nil {
				return modelcatalog.Model{}, err
			}
			dispatcher.storeCatalog(catalog)
		}
	}
	return catalog.ResolveWithRequest(requested, request)
}

func (dispatcher *Dispatcher) cacheCatalog(body []byte) {
	catalog, err := modelcatalog.Parse(body)
	if err == nil {
		dispatcher.storeCatalog(catalog)
	}
}

func (dispatcher *Dispatcher) storeCatalog(catalog *modelcatalog.Catalog) {
	dispatcher.mu.Lock()
	defer dispatcher.mu.Unlock()
	dispatcher.catalog = catalog
	dispatcher.catalogAt = dispatcher.now()
}

// CachedCatalog returns the last successfully fetched model catalog without
// triggering an upstream refresh. It returns nil when no fetch has succeeded
// yet. Read-only status endpoints (e.g. /account-limits) prefer this over a
// blocking FetchAvailableModels so a poll can never stall on upstream I/O.
func (dispatcher *Dispatcher) CachedCatalog() *modelcatalog.Catalog {
	dispatcher.mu.RLock()
	defer dispatcher.mu.RUnlock()
	return dispatcher.catalog
}

// RefreshCatalogIfStale starts a background catalog refresh when the cached
// catalog is missing or older than modelCacheTTL, and reports whether one was
// requested. It never blocks: startModelFetch single-flights the work on a
// context bounded by fetchModelsTimeout. Status endpoints call this so a
// non-blocking poll still drives the quota refresh that rides along with a
// successful fetch (updateAccountQuota + refreshLiveQuota).
func (dispatcher *Dispatcher) RefreshCatalogIfStale() bool {
	dispatcher.mu.RLock()
	fresh := dispatcher.catalog != nil && dispatcher.now().Sub(dispatcher.catalogAt) < dispatcher.modelCacheTTL
	dispatcher.mu.RUnlock()
	if fresh {
		return false
	}
	dispatcher.startModelFetch()
	return true
}

func cloneRequest(request map[string]any) map[string]any {
	cloned := make(map[string]any, len(request))
	for key, value := range request {
		cloned[key] = value
	}
	return cloned
}

// metered brackets one upstream call with the throttle meter, so every request
// an account makes counts toward its rate rather than only the streaming ones.
// A ceiling derived from a partial count reads higher than the account really
// tolerated, which is the unsafe direction.
func (dispatcher *Dispatcher) metered(email string, call func() (cloudcode.Response, error)) (cloudcode.Response, error) {
	dispatcher.meter.Begin(email)
	defer dispatcher.meter.End(email)
	return call()
}

func (dispatcher *Dispatcher) resolveProject(ctx context.Context, account *Account, client CloudClient) (string, error) {
	if dispatcher.projectID != "" {
		return dispatcher.projectID, nil
	}
	if project := dispatcher.manager.Project(account); project != "" {
		return project, nil
	}
	response, err := dispatcher.metered(account.Email, func() (cloudcode.Response, error) {
		return client.LoadCodeAssist(ctx, "")
	})
	if err != nil {
		return "", fmt.Errorf("discover project for %s: %w", account.Email, err)
	}
	sub := ExtractSubscription(response.Body, dispatcher.now())
	project := sub.ProjectID
	if project == "" {
		var document map[string]any
		if err := json.Unmarshal(response.Body, &document); err == nil {
			project = textValue(document["cloudaicompanionProject"])
			if object, ok := document["cloudaicompanionProject"].(map[string]any); ok && project == "" {
				project = textValue(object["id"])
			}
		}
	}
	if project == "" {
		return "", fmt.Errorf("loadCodeAssist response for %s did not include a Cloud Code project", account.Email)
	}
	sub.ProjectID = project
	_ = dispatcher.manager.UpdateSubscription(account.Email, sub)
	dispatcher.manager.CacheProject(account, project)
	return project, nil
}

func (dispatcher *Dispatcher) RefreshAccount(ctx context.Context, email string) (*Account, error) {
	dispatcher.manager.ClearTokenCache(email)
	dispatcher.manager.ClearProjectCache(email)
	dispatcher.resolver.Invalidate(email)

	var targetAccount *Account
	for _, acc := range dispatcher.manager.GetAllAccounts() {
		if acc.Email == email {
			targetAccount = acc
			break
		}
	}
	if targetAccount == nil {
		return nil, fmt.Errorf("account %s not found", email)
	}

	credentials, err := dispatcher.resolver.Resolve(ctx, targetAccount)
	if err != nil {
		return nil, fmt.Errorf("resolve credentials for %s: %w", email, err)
	}

	client := dispatcher.client(targetAccount, credentials.AccessToken)
	response, err := dispatcher.metered(email, func() (cloudcode.Response, error) {
		return client.LoadCodeAssist(ctx, "")
	})
	if err != nil {
		return nil, fmt.Errorf("load code assist for %s: %w", email, err)
	}

	sub := ExtractSubscription(response.Body, dispatcher.now())
	if sub.ProjectID != "" {
		dispatcher.manager.CacheProject(targetAccount, sub.ProjectID)
	}
	_ = dispatcher.manager.UpdateSubscription(email, sub)

	project := sub.ProjectID
	if project == "" {
		project = dispatcher.project(targetAccount)
	}

	quotaResponse, err := dispatcher.metered(email, func() (cloudcode.Response, error) {
		return client.FetchAvailableModels(ctx, project)
	})
	if err == nil && len(quotaResponse.Body) > 0 {
		dispatcher.updateAccountQuota(targetAccount, quotaResponse.Body)
		dispatcher.refreshLiveQuota(ctx, targetAccount, client, project)
	}

	if targetAccount.IsInvalid && targetAccount.VerifyURL != "" {
		dispatcher.manager.ClearInvalid(email)
	}

	for _, acc := range dispatcher.manager.GetAllAccounts() {
		if acc.Email == email {
			return acc, nil
		}
	}
	return targetAccount, nil
}

func (dispatcher *Dispatcher) client(account *Account, token string) CloudClient {
	dispatcher.mu.RLock()
	entry, exists := dispatcher.clients[account.Email]
	dispatcher.mu.RUnlock()
	if exists && entry.token == token {
		return entry.client
	}

	dispatcher.mu.Lock()
	defer dispatcher.mu.Unlock()
	entry, exists = dispatcher.clients[account.Email]
	if exists && entry.token == token {
		return entry.client
	}
	if exists {
		if closer, ok := entry.client.(interface{ CloseIdleConnections() }); ok {
			closer.CloseIdleConnections()
		}
	}
	client := dispatcher.newClient(token)
	dispatcher.clients[account.Email] = accountClient{token: token, client: client}
	return client
}

func (dispatcher *Dispatcher) project(account *Account) string {
	if dispatcher.projectID != "" {
		return dispatcher.projectID
	}
	return dispatcher.manager.Project(account)
}

func (dispatcher *Dispatcher) handleCredentialError(account *Account, err error) {
	if IsPermanentAuthFailure(err.Error()) || containsAny(strings.ToLower(err.Error()), "auth_invalid", "invalid_grant") {
		dispatcher.manager.MarkInvalid(account, "Credentials are invalid - re-authentication required", "")
	} else {
		slog.Warn("credential error — marking account as failed", "email", account.Email, "error", err)
		dispatcher.manager.MarkFailure(account, "")
	}
}

func (dispatcher *Dispatcher) rotateForError(account *Account, model string, err error) bool {
	if isCanceled(err) {
		return false
	}
	upstreamError := findHTTPError(err)
	if upstreamError == nil {
		dispatcher.manager.MarkFailure(account, model)
		return true
	}
	body := upstreamError.Body
	switch upstreamError.StatusCode {
	case http.StatusUnauthorized:
		dispatcher.resolver.Invalidate(account.Email)
		if IsPermanentAuthFailure(body) {
			dispatcher.manager.MarkInvalid(account, "Token revoked - re-authentication required", "")
		} else {
			dispatcher.manager.MarkFailure(account, model)
		}
		return true
	case http.StatusForbidden:
		if IsValidationRequired(body) {
			dispatcher.manager.MarkInvalid(account, "Account requires verification", ExtractVerificationURL(body))
			return true
		}
		if IsAccountBanned(body) {
			dispatcher.manager.MarkInvalid(account, "Account banned — Gemini disabled for Terms of Service violation", "")
			return true
		}
		return false
	case http.StatusBadRequest, http.StatusNotFound:
		return false
	case http.StatusTooManyRequests:
		wait := Decorrelate(SmartBackoff(ClassifyError(body, upstreamError.StatusCode), ParseResetTime(upstreamError.Header, body, dispatcher.now()), dispatcher.manager.FailureCount(account)), dispatcher.random)
		email := ""
		if account != nil {
			email = account.Email
		}
		inFlight, priorMinute := dispatcher.meter.ObserveRejection(email)
		dispatcher.record429(account, "", model, upstreamError, wait, dispatcher.manager.FailureCount(account), inFlight, priorMinute)
		dispatcher.manager.MarkRateLimited(account, model, wait)
		return true
	default:
		if upstreamError.StatusCode >= 500 {
			dispatcher.manager.MarkFailure(account, model)
			return true
		}
		return false
	}
}

func (dispatcher *Dispatcher) updateAccountQuota(account *Account, body []byte) {
	if account == nil || len(body) == 0 {
		return
	}
	catalog, err := modelcatalog.Parse(body)
	if err != nil {
		var doc struct {
			Models map[string]struct {
				QuotaInfo struct {
					RemainingFraction *float64 `json:"remainingFraction"`
					ResetTime         string   `json:"resetTime"`
				} `json:"quotaInfo"`
			} `json:"models"`
		}
		if err := json.Unmarshal(body, &doc); err != nil || len(doc.Models) == 0 {
			return
		}
		modelsQuota := make(map[string]ModelQuota, len(doc.Models))
		for mID, mData := range doc.Models {
			// Nil fraction stays nil (unknown), mirroring modelcatalog.Parse;
			// a reset-time-only entry is still recorded so the UI shows N/A.
			fraction := mData.QuotaInfo.RemainingFraction
			if fraction != nil || mData.QuotaInfo.ResetTime != "" {
				modelsQuota[mID] = ModelQuota{
					RemainingFraction: fraction,
					ResetTime:         mData.QuotaInfo.ResetTime,
				}
			}
		}
		quota := Quota{
			Models:      modelsQuota,
			LastChecked: dispatcher.now().UnixMilli(),
		}
		dispatcher.manager.UpdateAccountQuota(account.Email, quota, nil)
		return
	}

	modelsQuota := make(map[string]ModelQuota)
	for _, m := range catalog.Selectable() {
		if m.QuotaRemainingFraction != nil || m.QuotaResetTime != "" {
			modelsQuota[m.ID] = ModelQuota{
				RemainingFraction: m.QuotaRemainingFraction,
				ResetTime:         m.QuotaResetTime,
			}
		}
	}
	quota := Quota{
		Models:      modelsQuota,
		LastChecked: dispatcher.now().UnixMilli(),
	}
	dispatcher.manager.UpdateAccountQuota(account.Email, quota, nil)
}

// liveQuotaRefreshInterval throttles the two quota RPCs issued after a
// successful GenerateContent: upstream readings move slowly and
// per-request refreshes would triple hot-loop request cost. The catalog
// fetch path (fetchAvailableModels) and manual RefreshAccount stay
// unthrottled — they call refreshLiveQuota directly.
const liveQuotaRefreshInterval = time.Minute

// recordLiveQuotaRefresh stamps the per-account throttle window. Forced
// (unthrottled) refreshes stamp too: the post-request wrapper then skips
// RPCs the catalog fetch path just issued.
func (dispatcher *Dispatcher) recordLiveQuotaRefresh(email string) {
	dispatcher.mu.Lock()
	defer dispatcher.mu.Unlock()
	if dispatcher.liveQuotaRefreshMS == nil {
		dispatcher.liveQuotaRefreshMS = make(map[string]int64)
	}
	dispatcher.liveQuotaRefreshMS[email] = dispatcher.now().UnixMilli()
}

// refreshLiveQuotaThrottled runs refreshLiveQuota at most once per
// liveQuotaRefreshInterval per account.
func (dispatcher *Dispatcher) refreshLiveQuotaThrottled(ctx context.Context, account *Account, client CloudClient, project string) {
	if account == nil {
		return
	}
	dispatcher.mu.Lock()
	now := dispatcher.now().UnixMilli()
	last, seen := dispatcher.liveQuotaRefreshMS[account.Email]
	if seen && now-last < liveQuotaRefreshInterval.Milliseconds() {
		dispatcher.mu.Unlock()
		return
	}
	if dispatcher.liveQuotaRefreshMS == nil {
		dispatcher.liveQuotaRefreshMS = make(map[string]int64)
	}
	dispatcher.liveQuotaRefreshMS[account.Email] = now
	dispatcher.mu.Unlock()
	dispatcher.refreshLiveQuota(ctx, account, client, project)
}

// refreshLiveQuota best-effort merges live quota readings over the static
// catalog fractions. RetrieveUserQuotaSummary group buckets are shared pools
// (gemini-5h, 3p-weekly, ...) — they land in Quota.Pools, never in
// Quota.Models, so they cannot surface as phantom model rows.
// RetrieveUserQuota buckets are model-keyed and merge into Quota.Models.
// Upstream holds catalog remainingFraction at 1 until exhaustion, so without
// this the dashboard never shows consumption. Failures are swallowed: the
// catalog fractions remain the fallback. Clients without the capability
// (test fakes) skip silently.
func (dispatcher *Dispatcher) refreshLiveQuota(ctx context.Context, account *Account, client CloudClient, project string) {
	if account == nil || client == nil {
		return
	}
	dispatcher.recordLiveQuotaRefresh(account.Email)
	if fetcher, ok := client.(quotaSummaryFetcher); ok {
		if response, err := fetcher.RetrieveUserQuotaSummary(ctx, project); err == nil &&
			response.StatusCode >= 200 && response.StatusCode < 300 {
			for _, bucket := range cloudcode.ParseQuotaSummary(response.Body) {
				dispatcher.mergeQuotaReading(account.Email,
					strings.ToLower(strings.TrimSpace(bucket.ID)),
					bucket.RemainingFraction, bucket.RemainingAmount, bucket.ResetTime, true)
			}
		}
	}
	if fetcher, ok := client.(userQuotaFetcher); ok {
		if response, err := fetcher.RetrieveUserQuota(ctx, project); err == nil &&
			response.StatusCode >= 200 && response.StatusCode < 300 {
			for _, bucket := range cloudcode.ParseUserQuota(response.Body) {
				dispatcher.mergeQuotaReading(account.Email,
					strings.ToLower(strings.TrimSpace(bucket.ModelID)),
					bucket.RemainingFraction, bucket.RemainingAmount, bucket.ResetTime, false)
			}
		}
	}
}

// mergeQuotaReading stores one live reading under a single canonical key.
// An amount-only reading is actionable only at zero (exhausted): a positive
// amount without its total cannot produce a fraction, so it is skipped
// rather than fabricated.
func (dispatcher *Dispatcher) mergeQuotaReading(email, key string, fraction *float64, amount *int64, resetTime string, pool bool) {
	if fraction == nil {
		if amount == nil || *amount != 0 {
			return
		}
	}
	if pool {
		dispatcher.manager.MergeQuotaPool(email, key, fraction, resetTime)
		return
	}
	dispatcher.manager.MergeQuotaFraction(email, key, fraction, resetTime)
}

func findHTTPError(err error) *cloudcode.HTTPError {
	var upstreamError *cloudcode.HTTPError
	if errors.As(err, &upstreamError) {
		return upstreamError
	}
	return nil
}

// maxLoggedBodyLen caps upstream error bodies in logs: enough to keep the
// quota dimension and message, short enough that an error page cannot flood
// the log.
const maxLoggedBodyLen = 512

func truncateBodyForLog(body string) string {
	compact := strings.Join(strings.Fields(body), " ")
	if len(compact) <= maxLoggedBodyLen {
		return compact
	}
	return compact[:maxLoggedBodyLen]
}

// record429 persists one upstream 429 verbatim for later throttle-dimension
// analysis, together with how hard the account was being driven when it was
// rejected. No-op when forensics is disabled.
func (dispatcher *Dispatcher) record429(account *Account, project, model string, upstreamError *cloudcode.HTTPError, wait time.Duration, failures, inFlight, priorMinute int) {
	dispatcher.mu.RLock()
	recorder := dispatcher.forensics429
	dispatcher.mu.RUnlock()
	email := ""
	if account != nil {
		email = account.Email
	}
	dispatcher.meter.MarkRejected(email)
	if recorder == nil || !recorder.Enabled() {
		return
	}
	recorder.Record(Forensics429Entry{
		Timestamp:           dispatcher.now(),
		Account:             email,
		Project:             project,
		Model:               model,
		Endpoint:            upstreamError.Endpoint,
		Status:              upstreamError.StatusCode,
		Outcome:             OutcomeReject,
		Reason:              string(ClassifyError(upstreamError.Body, upstreamError.StatusCode)),
		InFlight:            inFlight,
		PriorMinuteRequests: priorMinute,
		Headers:             forensicsHeaders(upstreamError.Header),
		Body:                upstreamError.Body,
		AppliedWait:         wait.Round(time.Second).String(),
		Failures:            failures,
	})
}

// recordRecovery persists the first success on an account that had been
// throttled. The gap from its matching reject record is the only direct
// measurement of how long the throttle held.
func (dispatcher *Dispatcher) recordRecovery(account *Account, project, model string, inFlight, priorMinute int) {
	dispatcher.mu.RLock()
	recorder := dispatcher.forensics429
	dispatcher.mu.RUnlock()
	if recorder == nil || !recorder.Enabled() {
		return
	}
	email := ""
	if account != nil {
		email = account.Email
	}
	recorder.Record(Forensics429Entry{
		Timestamp:           dispatcher.now(),
		Account:             email,
		Project:             project,
		Model:               model,
		Status:              http.StatusOK,
		Outcome:             OutcomeRecover,
		InFlight:            inFlight,
		PriorMinuteRequests: priorMinute,
	})
}

func isCanceled(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func isCapacityHTTPError(err *cloudcode.HTTPError) bool {
	return (err.StatusCode == http.StatusTooManyRequests || err.StatusCode == http.StatusServiceUnavailable || err.StatusCode == 529) && IsCapacityExhausted(err.Body)
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func textValue(value any) string {
	text, _ := value.(string)
	return text
}
