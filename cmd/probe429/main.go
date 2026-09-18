// Command probe429 identifies the dimension Google's Cloud Code 429 throttle
// is keyed on. It takes a baseline probe, then — while that baseline is
// rejected — re-probes with exactly one dimension changed: model, account,
// project, or endpoint. A success on exactly one axis names the dimension.
//
// It talks to the user's own accounts with ordinary client requests. -burst
// deliberately induces a throttle and is capped; leave it off unless a wave
// is needed on demand.
//
// Not part of the proxy. See
// docs/superpowers/plans/2026-09-17-cloudcode-429-throttle-dimension-spec.md
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"antigravity-go-proxy/internal/accounts"
	"antigravity-go-proxy/internal/auth"
	"antigravity-go-proxy/internal/cloudcode"
)

// maxBurst caps -burst. The point is to trip a short-window throttle, which
// takes a handful of concurrent calls; a larger burst only deepens the hole
// the probe then has to measure.
const maxBurst = 20

func main() {
	model := flag.String("model", "gemini-3.8-flash-high", "baseline upstream model ID")
	altModel := flag.String("alt-model", "gemini-2.5-pro", "model for the model axis")
	altProject := flag.String("alt-project", "", "project ID for the project axis (empty skips the axis)")
	promptKB := flag.Int("kb", 0, "pad the prompt to this many KB")
	burst := flag.Int("burst", 0, "fire N concurrent requests first to induce a throttle (0 = off, max 20)")
	window := flag.Duration("window", 0, "after the matrix, re-probe the baseline until it succeeds, up to this long (0 = off)")
	spacing := flag.Duration("spacing", 30*time.Second, "delay between -window probes")
	out := flag.String("out", "", "append results as JSONL to this path")
	flag.Parse()

	pool, err := loadAccounts()
	if err != nil {
		fmt.Println("load accounts:", err)
		os.Exit(1)
	}
	if len(pool) == 0 {
		fmt.Println("no accounts in the pool")
		os.Exit(1)
	}

	ctx := context.Background()
	primary := pool[0]
	var results []ProbeResult

	if *burst > 0 {
		count := min(*burst, maxBurst)
		fmt.Printf("=== burst: %d concurrent requests on %s to induce a throttle\n", count, primary.Email)
		results = append(results, runBurst(ctx, primary, *model, *promptKB, count)...)
	}

	fmt.Println("=== baseline")
	results = append(results, probe(ctx, AxisBaseline, primary, cloudcode.ProdEndpoint, primary.Project, *model, *promptKB))

	fmt.Println("=== axis: model")
	results = append(results, probe(ctx, AxisModel, primary, cloudcode.ProdEndpoint, primary.Project, *altModel, *promptKB))

	if len(pool) > 1 {
		fmt.Println("=== axis: account")
		second := pool[1]
		results = append(results, probe(ctx, AxisAccount, second, cloudcode.ProdEndpoint, second.Project, *model, *promptKB))
	} else {
		fmt.Println("=== axis: account SKIPPED (pool has one account)")
	}

	if *altProject != "" {
		fmt.Println("=== axis: project")
		results = append(results, probe(ctx, AxisProject, primary, cloudcode.ProdEndpoint, *altProject, *model, *promptKB))
	} else {
		fmt.Println("=== axis: project SKIPPED (pass -alt-project)")
	}

	fmt.Println("=== axis: endpoint")
	results = append(results, probe(ctx, AxisEndpoint, primary, cloudcode.DailyEndpoint, primary.Project, *model, *promptKB))

	verdict := Conclude(results)

	if *window > 0 {
		fmt.Printf("=== window scan: re-probing the baseline every %s for up to %s\n", *spacing, *window)
		results = append(results, scanWindow(ctx, primary, *model, *promptKB, *window, *spacing)...)
	}

	for _, result := range results {
		fmt.Printf("  %-9s %-28s %-24s %-22s status=%d %s\n",
			result.Axis, result.Account, result.Model, shortEndpoint(result.Endpoint), result.Status, result.Error)
	}
	fmt.Println()
	fmt.Println("VERDICT:", verdict)

	if *out != "" {
		if err := writeJSONL(*out, results); err != nil {
			fmt.Println("write results:", err)
			os.Exit(1)
		}
		fmt.Println("results written to", *out)
	}
}

// probeAccount is one pool account with its resolved token and project.
type probeAccount struct {
	Email   string
	Project string
	Client  *cloudcode.Client
}

func loadAccounts() ([]probeAccount, error) {
	path, err := accounts.DefaultConfigPath()
	if err != nil {
		return nil, err
	}
	file, err := accounts.Load(path)
	if err != nil {
		return nil, err
	}
	resolver := accounts.NewCredentialResolver(auth.Manager{}, nil)
	var pool []probeAccount
	for _, account := range file.Accounts {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		credentials, err := resolver.Resolve(ctx, account)
		cancel()
		if err != nil {
			fmt.Printf("  skip %s: resolve: %v\n", account.Email, err)
			continue
		}
		pool = append(pool, probeAccount{
			Email:   account.Email,
			Project: account.ProjectID,
			Client:  cloudcode.New(cloudcode.Options{AccessToken: credentials.AccessToken, Timeout: 60 * time.Second}),
		})
	}
	return pool, nil
}

// probe makes exactly one upstream call with every dimension pinned, so the
// caller controls which one varies. It calls DoSSE with a single-element
// endpoint list on purpose: the client's normal failover would silently move
// off the endpoint the endpoint axis is measuring.
func probe(ctx context.Context, axis string, account probeAccount, endpoint, project, model string, promptKB int) ProbeResult {
	text := "Say OK"
	if promptKB > 0 {
		text = strings.Repeat("lorem ipsum dolor sit amet ", promptKB*1024/27) + " Say OK"
	}
	payload := map[string]any{
		"project": project,
		"model":   model,
		"request": map[string]any{
			"contents": []any{map[string]any{
				"role":  "user",
				"parts": []any{map[string]any{"text": text}},
			}},
		},
	}
	result := ProbeResult{
		Axis: axis, Account: account.Email, Project: project,
		Model: model, Endpoint: endpoint, At: time.Now(),
	}
	started := time.Now()
	response, err := account.Client.DoSSE(ctx, []string{endpoint}, cloudcode.PathStreamGenerate, payload,
		cloudcode.RequestOptions{}, func(cloudcode.SSEEvent) error { return nil })
	result.LatencyMS = time.Since(started).Milliseconds()
	if err == nil {
		result.Status = response.StatusCode
		if result.Status == 0 {
			result.Status = 200
		}
		return result
	}
	var upstreamError *cloudcode.HTTPError
	if errors.As(err, &upstreamError) {
		result.Status = upstreamError.StatusCode
		result.Error = strings.Join(strings.Fields(upstreamError.Body), " ")
		return result
	}
	result.Error = err.Error()
	return result
}

// runBurst fires count concurrent baseline requests to trip a short-window
// throttle on demand, so the matrix does not have to wait for a natural wave.
func runBurst(ctx context.Context, account probeAccount, model string, promptKB, count int) []ProbeResult {
	results := make([]ProbeResult, count)
	var group sync.WaitGroup
	for i := range count {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			results[index] = probe(ctx, fmt.Sprintf("burst-%d", index), account, cloudcode.ProdEndpoint, account.Project, model, promptKB)
		}(i)
	}
	group.Wait()
	throttled := 0
	for _, result := range results {
		if result.Throttled() {
			throttled++
		}
	}
	fmt.Printf("  burst: %d/%d rejected with 429\n", throttled, count)
	return results
}

// scanWindow measures how long the throttle actually holds: it re-probes the
// baseline until it succeeds or the budget runs out. This is the number the
// proxy's backoff ladder has to cover.
func scanWindow(ctx context.Context, account probeAccount, model string, promptKB int, budget, spacing time.Duration) []ProbeResult {
	var results []ProbeResult
	deadline := time.Now().Add(budget)
	started := time.Now()
	for attempt := 0; time.Now().Before(deadline); attempt++ {
		result := probe(ctx, fmt.Sprintf("window-%d", attempt), account, cloudcode.ProdEndpoint, account.Project, model, promptKB)
		results = append(results, result)
		if result.Succeeded() {
			fmt.Printf("  recovered after %s\n", time.Since(started).Round(time.Second))
			return results
		}
		select {
		case <-ctx.Done():
			return results
		case <-time.After(spacing):
		}
	}
	fmt.Printf("  still throttled after %s\n", budget)
	return results
}

func writeJSONL(path string, results []ProbeResult) error {
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	encoder := json.NewEncoder(file)
	for _, result := range results {
		if err := encoder.Encode(result); err != nil {
			return err
		}
	}
	return nil
}

func shortEndpoint(endpoint string) string {
	return strings.TrimPrefix(strings.TrimPrefix(endpoint, "https://"), "www.")
}
