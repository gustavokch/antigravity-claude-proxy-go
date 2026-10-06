// Command paramprobe measures which GenerationConfig shapes Cloud Code accepts
// for the models the proxy serves. Each probe sends one tiny request with a
// hand-built generationConfig and records the upstream status, so a proxy change
// that alters the wire shape can cite evidence instead of a doc reading.
//
// It talks to the user's own accounts with ordinary client requests, on the same
// generation host the proxy uses. The -heavy cases spend real thinking tokens;
// leave them off unless the effort sweep is wanted.
//
// Not part of the proxy. See
// docs/superpowers/plans/2026-10-05-api-parameter-parity.md (Task 1).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"antigravity-go-proxy/internal/accounts"
	"antigravity-go-proxy/internal/auth"
	"antigravity-go-proxy/internal/cloudcode"
	proxyformat "antigravity-go-proxy/internal/format"
)

func main() {
	claude := flag.String("claude", "claude-opus-4-6-thinking", "upstream Claude thinking model ID")
	tier := flag.String("tier", "gemini-3.8-flash-high", "upstream thinkingLevel-style Gemini model ID")
	heavy := flag.Bool("heavy", false, "also run the effort sweep (spends real thinking tokens)")
	only := flag.String("gate", "", "run only the probes of this gate (GA, GB, ...)")
	out := flag.String("out", "", "append results as JSONL to this path")
	list := flag.Bool("list", false, "print the probe matrix and exit without calling upstream")
	flag.Parse()

	cases := buildCases(Models{Claude: *claude, Tier: *tier})
	if *list {
		for _, c := range cases {
			fmt.Printf("%-28s %s heavy=%-5v %s\n", c.ID, c.Gate, c.Heavy, c.Question)
		}
		return
	}

	account, err := loadAccount()
	if err != nil {
		fmt.Println("load account:", err)
		os.Exit(1)
	}

	ctx := context.Background()
	var record *json.Encoder
	if *out != "" {
		file, err := os.OpenFile(*out, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			fmt.Println("open results file:", err)
			os.Exit(1)
		}
		defer file.Close()
		record = json.NewEncoder(file)
	}
	var results []Result
	for _, c := range cases {
		if (c.Heavy && !*heavy) || (*only != "" && !strings.EqualFold(*only, c.Gate)) {
			continue
		}
		fmt.Printf("=== %s (%s)\n", c.ID, c.Model)
		result := run(ctx, account, c)
		fmt.Printf("    status=%d %s thinkingBlocks=%d thinkingTokens=%d %s\n",
			result.Status, result.Outcome(), result.ThinkingBlocks, result.ThinkingTokens, result.Error)
		results = append(results, result)
		// Write each result as it arrives: an interrupt must not lose the
		// results of -heavy cases that already spent thinking tokens.
		if record != nil {
			if err := record.Encode(result); err != nil {
				fmt.Println("write result:", err)
				os.Exit(1)
			}
		}
	}

	fmt.Println()
	for _, line := range Summary(results) {
		fmt.Println(line)
	}
	if record != nil {
		fmt.Println("results written to", *out)
	}
}

type poolAccount struct {
	Email   string
	Project string
	Client  *cloudcode.Client
}

// loadAccount resolves the first enabled, valid pool account's credentials.
func loadAccount() (poolAccount, error) {
	path, err := accounts.DefaultConfigPath()
	if err != nil {
		return poolAccount{}, err
	}
	file, err := accounts.Load(path)
	if err != nil {
		return poolAccount{}, err
	}
	resolver := accounts.NewCredentialResolver(auth.Manager{}, nil)
	for _, account := range file.Accounts {
		if !account.Enabled || account.IsInvalid {
			fmt.Printf("  skip %s: disabled or marked invalid\n", account.Email)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		credentials, err := resolver.Resolve(ctx, account)
		cancel()
		if err != nil {
			fmt.Printf("  skip %s: resolve: %v\n", account.Email, err)
			continue
		}
		return poolAccount{
			Email:   account.Email,
			Project: account.ProjectID,
			Client:  cloudcode.New(cloudcode.Options{AccessToken: credentials.AccessToken, Timeout: 120 * time.Second}),
		}, nil
	}
	return poolAccount{}, errors.New("no usable account in the pool")
}

// requestOptions adds the beta header the proxy sends for Claude thinking
// routes (accounts.Dispatcher.StreamGenerateContent does the same).
func requestOptions(c Case) cloudcode.RequestOptions {
	if proxyformat.GetModelFamily(c.Model) != proxyformat.FamilyClaude {
		return cloudcode.RequestOptions{}
	}
	headers := make(http.Header)
	headers.Set("anthropic-beta", "interleaved-thinking-2025-05-14")
	return cloudcode.RequestOptions{Headers: headers}
}

func run(ctx context.Context, account poolAccount, c Case) Result {
	result := Result{ID: c.ID, Gate: c.Gate, Question: c.Question, Model: c.Model, At: time.Now()}
	accumulator := proxyformat.NewThinkingAccumulator()
	started := time.Now()
	_, err := account.Client.StreamGenerateContent(ctx, buildPayload(c, account.Project, account.Email), requestOptions(c),
		func(event cloudcode.SSEEvent) error { return accumulator.Consume(event.Data) })
	result.LatencyMS = time.Since(started).Milliseconds()
	if err != nil {
		var upstream *cloudcode.HTTPError
		if errors.As(err, &upstream) {
			result.Status = upstream.StatusCode
			result.Error = truncate(strings.Join(strings.Fields(upstream.Body), " "), 300)
			return result
		}
		result.Error = err.Error()
		return result
	}
	result.Status = http.StatusOK
	response := accumulator.Response(c.Model, proxyformat.NewSignatureCache(), "msg_paramprobe")
	result.StopReason, _ = response["stop_reason"].(string)
	blocks, _ := response["content"].([]any)
	for _, raw := range blocks {
		if block, ok := raw.(map[string]any); ok && block["type"] == "thinking" {
			result.ThinkingBlocks++
		}
	}
	result.ThinkingTokens = accumulator.ThinkingTokens()
	result.OutputTokens = accumulator.OutputTokens()
	return result
}

func truncate(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return text[:limit] + "..."
}
