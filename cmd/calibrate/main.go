// Command calibrate derives the proxy's pacing and cooldown settings from the
// 429 journal the dispatcher writes in production, and prints them. It never
// writes config.json: a thin or skewed sample must not silently repace the
// proxy.
//
// -probe-daily draws one rejection from daily-cloudcode-pa.googleapis.com,
// which is the only endpoint observed to state its own retry delay. It is a
// fallback for a journal with too few recovery pairs, and is capped at ten
// requests.
//
// Not part of the proxy. See
// docs/superpowers/specs/2026-09-18-throttle-calibration-design.md
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"antigravity-go-proxy/internal/accounts"
	"antigravity-go-proxy/internal/auth"
	"antigravity-go-proxy/internal/calibrate"
	"antigravity-go-proxy/internal/cloudcode"
)

const exitAccountProtected = 3

func main() {
	journalPath := flag.String("journal", calibrate.DefaultJournalPath(), "path to the 429 journal")
	probeDaily := flag.Bool("probe-daily", false, "draw one rejection from the daily endpoint to read its stated retry delay")
	dryRun := flag.Bool("dry-run", false, "read and derive without making any network call (implies -probe-daily=false)")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	journal, err := calibrate.ReadJournal(*journalPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "read journal:", err)
		os.Exit(1)
	}
	fmt.Printf("journal: %s (%d record(s))\n\n", *journalPath, len(journal.Entries))

	result := calibrate.Derive(journal)

	if *probeDaily && !*dryRun {
		delay, err := probeDailyEndpoint(ctx)
		switch {
		case errors.Is(err, calibrate.ErrAccountProtected):
			fmt.Fprintln(os.Stderr, "probe stopped: the upstream response signals account jeopardy, not an ordinary throttle")
			os.Exit(exitAccountProtected)
		case err != nil:
			fmt.Fprintln(os.Stderr, "probe:", err)
		default:
			result.RecoverDaily = delay
		}
	}

	fmt.Print(result.Report())

	fragment := result.Fragment()
	if len(fragment) == 0 {
		fmt.Println("\nno setting could be derived from this journal — nothing to suggest")
		return
	}
	encoded, err := json.MarshalIndent(fragment, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "render fragment:", err)
		os.Exit(1)
	}
	fmt.Printf("\nsuggested config.json values (apply by hand):\n%s\n", encoded)
}

// probeDailyEndpoint sends minimal requests to the daily endpoint until it
// rejects with a stated delay, and returns that delay. It stops at
// MaxProbeRequests, and re-resolves credentials once on a 401 — a long run
// outlives a token, and treating that as a permanent auth failure would abort
// a healthy probe.
func probeDailyEndpoint(ctx context.Context) (time.Duration, error) {
	path, err := accounts.DefaultConfigPath()
	if err != nil {
		return 0, err
	}
	file, err := accounts.Load(path)
	if err != nil {
		return 0, err
	}
	if len(file.Accounts) == 0 {
		return 0, errors.New("no accounts in the pool")
	}
	account := file.Accounts[0]
	resolver := accounts.NewCredentialResolver(auth.Manager{}, nil)

	resolve := func() (*cloudcode.Client, error) {
		resolveCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		credentials, err := resolver.Resolve(resolveCtx, account)
		if err != nil {
			return nil, err
		}
		return cloudcode.New(cloudcode.Options{AccessToken: credentials.AccessToken, Timeout: 60 * time.Second}), nil
	}

	client, err := resolve()
	if err != nil {
		return 0, err
	}

	payload := map[string]any{
		"project": account.ProjectID,
		"model":   "gemini-3.8-flash-high",
		"request": map[string]any{
			"contents": []any{map[string]any{
				"role":  "user",
				"parts": []any{map[string]any{"text": "Say OK"}},
			}},
		},
	}

	reauthorized := false
	for attempt := range calibrate.MaxProbeRequests {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		_, requestErr := client.DoSSE(ctx, []string{cloudcode.DailyEndpoint}, cloudcode.PathStreamGenerate,
			payload, cloudcode.RequestOptions{}, func(cloudcode.SSEEvent) error { return nil })
		if requestErr == nil {
			fmt.Printf("  probe %d/%d: accepted\n", attempt+1, calibrate.MaxProbeRequests)
			continue
		}
		var upstreamError *cloudcode.HTTPError
		if !errors.As(requestErr, &upstreamError) {
			return 0, requestErr
		}
		if err := calibrate.GuardBody(upstreamError.Body); err != nil {
			return 0, err
		}
		if upstreamError.StatusCode == http.StatusUnauthorized && !reauthorized {
			reauthorized = true
			fmt.Println("  probe: 401, re-resolving credentials once")
			client, err = resolve()
			if err != nil {
				return 0, fmt.Errorf("re-resolve after 401: %w", err)
			}
			continue
		}
		if upstreamError.StatusCode != http.StatusTooManyRequests {
			// Anything else is a broken request, not a throttle — a retired
			// model id, a bad project. Repeating it nine more times buys
			// nothing.
			return 0, fmt.Errorf("probe: upstream returned %d, which is not a throttle", upstreamError.StatusCode)
		}
		if delay, ok := calibrate.ParseRetryDelay(upstreamError.Body); ok {
			fmt.Printf("  probe %d/%d: rejected, stated delay %s\n", attempt+1, calibrate.MaxProbeRequests, delay.Round(time.Second))
			return delay, nil
		}
		fmt.Printf("  probe %d/%d: rejected with status %d and no stated delay\n", attempt+1, calibrate.MaxProbeRequests, upstreamError.StatusCode)
	}
	return 0, fmt.Errorf("no rejection stated a delay within %d requests", calibrate.MaxProbeRequests)
}
