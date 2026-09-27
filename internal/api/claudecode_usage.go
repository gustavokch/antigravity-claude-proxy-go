package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"antigravity-go-proxy/internal/claudecode"
	"antigravity-go-proxy/internal/claudecode/ccusage"
	"antigravity-go-proxy/internal/config"
)

// onlinePricingTimeout bounds the optional LiteLLM price download.
const onlinePricingTimeout = 60 * time.Second

// newClaudeCodePricer returns the one pricer behind every Claude Code cost:
// the curated in-repo table first, because it knows the current models,
// then LiteLLM's table.
func newClaudeCodePricer() (*ccusage.ChainPricer, *ccusage.LiteLLMPricer) {
	curated := ccusage.PricerFunc(func(model string) (ccusage.ModelPrice, bool) {
		p, ok := claudecode.LookupModelPricing(model)
		if !ok {
			return ccusage.ModelPrice{}, false
		}
		return ccusage.ModelPrice{Input: p.Prompt, Output: p.Completion, CacheCreate: p.CacheWrite, CacheRead: p.CacheRead}, true
	})
	lite := ccusage.NewLiteLLMPricer()
	return ccusage.NewChainPricer(curated, lite), lite
}

// claudeCodeUsagePricer adapts a ccusage pricer to claudecode.SetPricer.
// A model no pricer knows keeps the curated table's default rates, so
// CallCost never drops to zero for an unrecognised model.
func claudeCodeUsagePricer(p ccusage.Pricer) claudecode.Pricer {
	return func(model string, u claudecode.Usage) float64 {
		if _, ok := p.Find(model); !ok {
			return claudecode.DefaultPricer(model, u)
		}
		return ccusage.TokenCost(model, claudeCodeTokenUsage(u), p)
	}
}

// claudeCodeTokenUsage splits cache writes into 5m and 1h parts; writes the
// split does not account for count as 5m, the API default.
func claudeCodeTokenUsage(u claudecode.Usage) ccusage.TokenUsage {
	cc5m := u.CacheCreate5m
	if rest := u.CacheCreate - u.CacheCreate5m - u.CacheCreate1h; rest > 0 {
		cc5m += rest
	}
	return ccusage.TokenUsage{
		Input:         u.Input,
		Output:        u.Output,
		CacheCreate5m: cc5m,
		CacheCreate1h: u.CacheCreate1h,
		CacheRead:     u.CacheRead,
		Fast:          u.Speed == "fast",
	}
}

// NewClaudeCodeUsage installs the shared Claude Code pricer and builds the
// usage engine from cfg.Usage. It returns a nil engine, which the server
// treats as "tracking off", when usage tracking is disabled. Invalid
// optional settings fall back to their defaults with a warning. With
// onlinePricing set, LiteLLM prices are downloaded in the background until
// ctx is done. The caller starts the engine through Server.StartClaudeCodeUsage
// and stops it with Server.Close.
func NewClaudeCodeUsage(ctx context.Context, cfg claudecode.Config, logger *slog.Logger) (*ccusage.Engine, error) {
	if logger == nil {
		logger = slog.Default()
	}
	chain, lite := newClaudeCodePricer()
	claudecode.SetPricer(claudeCodeUsagePricer(chain))

	usage := cfg.Usage
	if !usage.UsageEnabled() {
		return nil, nil
	}
	dur, err := usage.SessionDuration()
	if err != nil {
		logger.Warn("claudecode usage: invalid sessionHours, using the default", "error", err)
		dur = claudecode.DefaultUsageSessionHours * time.Hour
	}
	mode, err := ccusage.ParseCostMode(usage.CostMode)
	if err != nil {
		logger.Warn("claudecode usage: invalid costMode, using auto", "error", err)
		mode = ccusage.CostModeAuto
	}
	loc := time.Local
	if usage.Timezone != "" {
		if l, err := time.LoadLocation(usage.Timezone); err == nil {
			loc = l
		} else {
			logger.Warn("claudecode usage: invalid timezone, using local time", "timezone", usage.Timezone, "error", err)
		}
	}
	root := usage.LedgerDir
	if root == "" {
		root = filepath.Join(config.GetConfigDir(), claudecode.UsageLedgerDirName)
	}

	engine, err := ccusage.NewEngine(ccusage.EngineOptions{
		LedgerRoot:      root,
		RetentionDays:   usage.RetentionDaysOrDefault(),
		ScanLocalLogs:   usage.LocalLogsEnabled(),
		LocalAccountID:  usage.LocalAccountID,
		Accounts:        claudeCodeUsageAccounts,
		SessionDuration: dur,
		CostMode:        mode,
		Pricer:          chain,
		Location:        loc,
		Logger:          logger,
	})
	if err != nil {
		return nil, fmt.Errorf("claudecode usage engine: %w", err)
	}

	if usage.OnlinePricing {
		go func() {
			rctx, cancel := context.WithTimeout(ctx, onlinePricingTimeout)
			defer cancel()
			// A plain client of its own: this download never shares the
			// Cloud Code transport.
			if err := lite.Refresh(rctx, &http.Client{Timeout: onlinePricingTimeout}); err != nil {
				logger.Warn("claudecode usage: online pricing refresh failed, keeping embedded prices", "error", err)
				return
			}
			chain.Reset()
		}()
	}
	return engine, nil
}

// claudeCodeUsageAccounts lists the configured Claude Code accounts for
// local-usage attribution.
func claudeCodeUsageAccounts() []ccusage.AccountRef {
	accts := config.Get().ClaudeCode.Accounts
	out := make([]ccusage.AccountRef, 0, len(accts))
	for _, a := range accts {
		out = append(out, ccusage.AccountRef{ID: a.ID, AutoImport: isAutoImportedAccount(a)})
	}
	return out
}

// isAutoImportedAccount reports whether an account came from the local
// Claude Code login. Imports saved before the source was persisted are
// recognised by the "auto-" ID prefix discovery gives them.
func isAutoImportedAccount(a claudecode.AccountConfig) bool {
	return a.Source == "auto_import" || strings.HasPrefix(a.ID, "auto-")
}

// claudeCodeUsageAnchors turns a unified snapshot into the 5-hour window
// start the engine anchors blocks on: the 5h reset minus five hours.
func claudeCodeUsageAnchors(u *claudecode.Unified) []time.Time {
	if u == nil || u.FiveHour.Reset.IsZero() {
		return nil
	}
	return []time.Time{u.FiveHour.Reset.Add(-5 * time.Hour)}
}

// StartClaudeCodeUsage starts the usage engine's file poller until ctx is
// done or Close is called. It is a no-op without an engine.
func (server *Server) StartClaudeCodeUsage(ctx context.Context) {
	server.ccUsage.Start(ctx)
}

// ClaudeCodeUsage returns the usage engine, or nil when tracking is off.
func (server *Server) ClaudeCodeUsage() *ccusage.Engine { return server.ccUsage }

// Close releases what the server holds beyond in-flight requests: it stops
// the usage engine and flushes and closes its ledger. Call it after the HTTP
// server has shut down. It is safe to call more than once.
func (server *Server) Close() error {
	return server.ccUsage.Close()
}

// recordClaudeCodeUsage sends one served response to the usage engine: the
// response itself and one entry per advisor iteration, each costed by the
// shared pricer. It never blocks on disk and does nothing without an engine
// or usage.
func (server *Server) recordClaudeCodeUsage(accountID, sessionID, requestedModel, origin string, u claudecode.Usage) {
	if server.ccUsage == nil {
		return
	}
	if u.Input == 0 && u.Output == 0 && u.CacheRead == 0 && u.CacheCreate == 0 && len(u.Iterations) == 0 {
		return
	}
	model := u.Model
	if model == "" {
		model = requestedModel
	}
	if u.RequestID == "" {
		server.warnMissingRequestID(accountID, origin)
	}
	now := time.Now()
	cost := claudecode.UsageCost(model, u)
	server.ccUsage.Record(ccusage.Entry{
		Timestamp:     now,
		SessionID:     sessionID,
		RequestID:     u.RequestID,
		MessageID:     u.MessageID,
		Model:         model,
		Speed:         u.Speed,
		Input:         u.Input,
		Output:        u.Output,
		CacheCreate:   u.CacheCreate,
		CacheCreate5m: u.CacheCreate5m,
		CacheCreate1h: u.CacheCreate1h,
		CacheRead:     u.CacheRead,
		CostUSD:       &cost,
		AccountID:     accountID,
		Origin:        origin,
	})

	// Advisor iterations are separate entries, keyed as Claude Code's own
	// transcripts key them so the two dedupe.
	if u.MessageID == "" {
		return
	}
	n := 0
	for _, it := range u.Iterations {
		if it.Type != "advisor_message" || it.Model == "" {
			continue
		}
		iu := claudecode.Usage{
			Model: it.Model, Input: it.Input, Output: it.Output, CacheRead: it.CacheRead,
			CacheCreate: it.CacheCreate, CacheCreate5m: it.CacheCreate5m, CacheCreate1h: it.CacheCreate1h, Speed: it.Speed,
		}
		itCost := claudecode.UsageCost(it.Model, iu)
		server.ccUsage.Record(ccusage.Entry{
			Timestamp:     now,
			SessionID:     sessionID,
			RequestID:     u.RequestID,
			MessageID:     fmt.Sprintf("%s:advisor:%d", u.MessageID, n),
			Model:         it.Model,
			Speed:         it.Speed,
			Input:         it.Input,
			Output:        it.Output,
			CacheCreate:   it.CacheCreate,
			CacheCreate5m: it.CacheCreate5m,
			CacheCreate1h: it.CacheCreate1h,
			CacheRead:     it.CacheRead,
			CostUSD:       &itCost,
			AccountID:     accountID,
			Origin:        origin,
		})
		n++
	}
}

// missingRequestIDWarned makes the missing request-id warning once per
// process; later occurrences log at debug.
var missingRequestIDWarned atomic.Bool

// warnMissingRequestID notes a served response without a request-id
// header. Without it the ledger entry cannot dedupe against Claude Code's
// transcript of the same turn, so that turn may be counted twice.
func (server *Server) warnMissingRequestID(accountID, origin string) {
	logger := server.logger
	if logger == nil {
		logger = slog.Default()
	}
	if missingRequestIDWarned.CompareAndSwap(false, true) {
		logger.Warn("claudecode usage: upstream response has no request-id; it cannot dedupe against local transcripts", "account", accountID, "origin", origin)
		return
	}
	logger.Debug("claudecode usage: upstream response has no request-id", "account", accountID, "origin", origin)
}
