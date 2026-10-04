package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	// Embedded zone data, so claudecode.usage.timezone works on hosts
	// without a system zoneinfo database (about 450KB).
	_ "time/tzdata"

	"antigravity-go-proxy/internal/accounts"
	"antigravity-go-proxy/internal/api"
	"antigravity-go-proxy/internal/auth"
	"antigravity-go-proxy/internal/cloudcode"
	"antigravity-go-proxy/internal/config"
	proxyformat "antigravity-go-proxy/internal/format"
	"antigravity-go-proxy/internal/logger"
	"antigravity-go-proxy/internal/mitm"
	"antigravity-go-proxy/internal/openrouter"
	"antigravity-go-proxy/internal/stats"
	"antigravity-go-proxy/internal/webui"
)

var startPprof func(*slog.Logger)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "stop":
			if err := StopDaemon(); err != nil {
				fmt.Fprintf(os.Stderr, "Error stopping proxy: %v\n", err)
				os.Exit(1)
			}
			fmt.Println("✅ Antigravity proxy stopped.")
			return
		case "restart":
			_ = StopDaemon()
			time.Sleep(500 * time.Millisecond)
			pid, err := StartDaemon(os.Args[2:])
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error restarting proxy daemon: %v\n", err)
				os.Exit(1)
			}
			fmt.Printf("✅ Antigravity proxy restarted (PID: %d)\n", pid)
			return
		case "status":
			listen := envOr("ANTIGRAVITY_PROXY_LISTEN", "127.0.0.1:8080")
			if err := cmdStatus(listen); err != nil {
				fmt.Fprintf(os.Stderr, "Error getting status: %v\n", err)
				os.Exit(1)
			}
			return
		case "web":
			listen := envOr("ANTIGRAVITY_PROXY_LISTEN", "127.0.0.1:8080")
			if err := cmdWeb(listen); err != nil {
				fmt.Fprintf(os.Stderr, "Error opening web UI: %v\n", err)
				os.Exit(1)
			}
			return
		case "accounts":
			if err := cmdAccounts(os.Args[2:]); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
			return
		case "start":
			runServer(os.Args[2:])
			return
		}
	}

	// Default: run start command with current args
	runServer(os.Args[1:])
}

func runServer(args []string) {
	fs := flag.NewFlagSet("antigravity-proxy", flag.ExitOnError)
	listen := fs.String("listen", envOr("ANTIGRAVITY_PROXY_LISTEN", "127.0.0.1:8080"), "HTTP listen address")
	port := fs.Int("port", 0, "HTTP listen port (overrides port in -listen)")
	apiKey := fs.String("api-key", envOr("ANTIGRAVITY_PROXY_API_KEY", os.Getenv("API_KEY")), "optional local API key")
	projectID := fs.String("project", os.Getenv("AGY_PROJECT_ID"), "optional managed Cloud Code project ID")
	accountsPath := fs.String("accounts", os.Getenv("ANTIGRAVITY_ACCOUNTS_FILE"), "optional account-pool JSON path (default ~/.config/antigravity-proxy/accounts.json when present)")
	strategy := fs.String("strategy", envOr("ACCOUNT_STRATEGY", accounts.DefaultStrategy), "account strategy: sticky, round-robin, or hybrid")
	upstreamTimeout := fs.Duration("upstream-timeout", 5*time.Minute, "Cloud Code request timeout")
	pprof := fs.Bool("pprof", false, "enable pprof server on localhost:6060")
	daemon := fs.Bool("daemon", false, "run proxy server in background daemon mode")
	_ = fs.Parse(args)

	// Load configuration file. Keys in config.json (apiKey, apiKeys) are read
	// live by the server on every request; only the -api-key flag /
	// ANTIGRAVITY_PROXY_API_KEY value is passed through Options.APIKey, so a
	// config edit can revoke or rotate a key without a restart.
	cfg, _ := config.Load()
	if *port > 0 {
		*listen = fmt.Sprintf("127.0.0.1:%d", *port)
	}

	// Daemon mode
	if *daemon {
		pid, err := StartDaemon(args)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to start daemon: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("✅ Antigravity proxy daemon started (PID: %d, Listen: %s)\n", pid, *listen)
		return
	}

	// Write PID file for foreground / daemon runner
	_ = WritePIDFile(os.Getpid())
	defer RemovePIDFile()

	// Broadcaster and logger
	broadcaster := logger.NewBroadcaster(500)
	logLevel := slog.LevelInfo
	if cfg.Debug || cfg.DevMode || os.Getenv("DEBUG") != "" || os.Getenv("ANTIGRAVITY_DEV_MODE") == "true" {
		logLevel = slog.LevelDebug
	} else if cfg.LogLevel != "" {
		switch strings.ToLower(cfg.LogLevel) {
		case "debug":
			logLevel = slog.LevelDebug
		case "warn", "warning":
			logLevel = slog.LevelWarn
		case "error":
			logLevel = slog.LevelError
		case "info":
			logLevel = slog.LevelInfo
		}
	}
	baseHandler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel})
	streamHandler := logger.NewStreamHandler(baseHandler, broadcaster)
	slogger := slog.New(streamHandler)
	slog.SetDefault(slogger)

	// Usage statistics tracker
	statsPath := filepath.Join(config.GetConfigDir(), "usage-history.json")
	tracker, err := stats.NewTracker(statsPath)
	if err != nil {
		slogger.Warn("initialize stats tracker failed", "error", err)
	} else {
		tracker.StartAutoSave(1 * time.Minute)
		defer tracker.Close()
	}

	if *pprof && startPprof != nil {
		startPprof(slogger)
	}

	strategyExplicit := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "strategy" {
			strategyExplicit = true
		}
	})
	effectiveStrategy := *strategy
	if !strategyExplicit && os.Getenv("ACCOUNT_STRATEGY") == "" && cfg.AccountSelection.Strategy != "" {
		effectiveStrategy = cfg.AccountSelection.Strategy
	}

	accountManager, err := accounts.NewDefault(*accountsPath, effectiveStrategy, nil)
	if err != nil {
		slogger.Error("load account pool", "error", err)
		os.Exit(2)
	}
	accountManager.SetSelectionConfig(cfg.AccountSelection, cfg.GlobalQuotaThreshold)

	var backoffs []time.Duration
	if len(cfg.CapacityBackoffTiersMs) > 0 {
		backoffs = make([]time.Duration, len(cfg.CapacityBackoffTiersMs))
		for i, ms := range cfg.CapacityBackoffTiersMs {
			backoffs[i] = time.Duration(ms) * time.Millisecond
		}
	}
	var maxWait time.Duration
	if cfg.MaxWaitBeforeErrorMs > 0 {
		maxWait = time.Duration(cfg.MaxWaitBeforeErrorMs) * time.Millisecond
	}
	var switchDelay time.Duration
	if cfg.SwitchAccountDelayMs > 0 {
		switchDelay = time.Duration(cfg.SwitchAccountDelayMs) * time.Millisecond
	}
	var requestDelay time.Duration
	if cfg.RequestDelayMs > 0 {
		requestDelay = time.Duration(cfg.RequestDelayMs) * time.Millisecond
	}

	builder := proxyformat.NewBuilder()
	var forensics429 *accounts.Forensics429Recorder
	if cfg.Upstream429ForensicsEnabled {
		forensicsPath := filepath.Join(config.GetConfigDir(), "forensics", "upstream-429.jsonl")
		forensics429 = accounts.NewForensics429Recorder(forensicsPath)
		slogger.Info("upstream 429 forensics enabled", "path", forensicsPath)
	}
	dispatcher, err := accounts.NewDispatcher(accounts.DispatcherOptions{
		Manager:                  accountManager,
		Resolver:                 accounts.NewCredentialResolver(auth.Manager{}, nil),
		Builder:                  builder,
		ProjectID:                *projectID,
		MaxRetries:               cfg.MaxRetries,
		MaxWait:                  maxWait,
		CapacityBackoffs:         backoffs,
		MaxCapacityRetries:       cfg.MaxCapacityRetries,
		SwitchDelay:              switchDelay,
		RequestThrottlingEnabled: cfg.RequestThrottlingEnabled,
		RequestDelay:             requestDelay,
		Forensics429:             forensics429,
		NewClient: func(accessToken string) accounts.CloudClient {
			return cloudcode.New(cloudcode.Options{AccessToken: accessToken, Timeout: *upstreamTimeout})
		},
	})
	if err != nil {
		slogger.Error("initialize account dispatcher", "error", err)
		os.Exit(2)
	}

	oauthMgr := auth.NewOAuthManager(accountManager)
	uiHandler := webui.Handler()

	bgCtx, bgCancel := context.WithCancel(context.Background())
	defer bgCancel()

	// The usage engine also installs the shared Claude Code pricer, so it is
	// built even when tracking is off (it then returns nil).
	ccUsage, err := api.NewClaudeCodeUsage(bgCtx, cfg.ClaudeCode, slogger)
	if err != nil {
		slogger.Warn("claudecode usage tracking disabled", "error", err)
		ccUsage = nil
	}

	// The observe-only Claude Code forward proxy. A failure to start it is
	// logged and never stops the main proxy.
	var mitmRuntime *mitm.Runtime
	if cfg.Mitm.Enabled {
		if err := cfg.Mitm.Validate(); err != nil {
			slogger.Warn("mitm forward proxy disabled: invalid config", "error", err)
		} else if rt, err := mitm.StartRuntime(mitm.RuntimeConfig{
			Dir:         filepath.Join(config.GetConfigDir(), "mitm"),
			Listen:      cfg.Mitm.Listen,
			RegistryMax: cfg.Mitm.RegistryMax,
			RegistryTTL: time.Duration(cfg.Mitm.RegistryTTLMinutes) * time.Minute,
			Logger:      slogger,
		}); err != nil {
			slogger.Warn("mitm forward proxy disabled", "error", err)
		} else {
			mitmRuntime = rt
			slogger.Info("mitm forward proxy listening", "address", rt.Addr, "caFingerprint", rt.CA.Fingerprint())
		}
	}

	handler, err := api.New(api.Options{
		APIKey:         *apiKey,
		Backend:        dispatcher,
		Builder:        builder,
		Logger:         slogger,
		AccountManager: accountManager,
		Broadcaster:    broadcaster,
		WebUI:          uiHandler,
		OAuthHandler:   oauthMgr,
		Tracker:        tracker,
		CCUsage:        ccUsage,
		Mitm:           mitmRuntime,
	})
	if err != nil {
		slogger.Error("invalid proxy configuration", "error", err)
		os.Exit(2)
	}

	handler.StartClaudeCodeBackgroundWorker(bgCtx)
	handler.StartCacheBumpScheduler(bgCtx)
	handler.StartClaudeCodeUsage(bgCtx)

	httpServer := newHTTPServer(*listen, handler.Handler())

	shutdownSignals := make(chan os.Signal, 1)
	signal.Notify(shutdownSignals, syscall.SIGINT, syscall.SIGTERM)
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-shutdownSignals
		// Restore the default handling, so a second Ctrl-C force-quits a
		// shutdown that hangs.
		signal.Stop(shutdownSignals)
		slogger.Info("shutting down proxy server...")
		if tracker != nil {
			_ = tracker.Close()
		}
		// Persist routing state after Shutdown drains in-flight requests, so
		// results recorded during drain are not lost.
		defer openrouter.DefaultRouter.FlushSave()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(ctx); err != nil {
			slogger.Error("graceful shutdown failed", "error", err)
		}
		if mitmRuntime != nil {
			if err := mitmRuntime.Shutdown(ctx); err != nil {
				slogger.Warn("mitm forward proxy shutdown", "error", err)
			}
		}
		// After the drain, so usage recorded by the last requests reaches
		// the ledger.
		if err := handler.Close(); err != nil {
			slogger.Warn("close usage ledger", "error", err)
		}
	}()

	slogger.Info("Antigravity proxy server listening", "address", *listen, "accounts", accountManager.Count(), "strategy", *strategy)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slogger.Error("HTTP server failed", "error", err)
		os.Exit(1)
	}
	// ListenAndServe returns as soon as Shutdown starts; wait for the drain
	// and the ledger flush before exiting.
	<-shutdownDone
}

// newHTTPServer builds the proxy listener. It must never set WriteTimeout:
// that deadline covers the full response write and kills long SSE streams
// mid-generation (same failure as a total http.Client Timeout on the
// upstream side: the client sees a stream ending without message_stop).
// ReadTimeout/ReadHeaderTimeout only bound request reads; IdleTimeout only
// applies to idle keep-alive connections, so long streams are unaffected.
func newHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
