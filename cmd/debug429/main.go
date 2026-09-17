// Command debug429 is a one-off diagnostic: it replays a minimal
// gemini-3.8-flash-high request per account and prints the upstream HTTP
// status, rate-limit headers, and the full error body, so the 429 quota
// dimension (user vs project) becomes visible. Not part of the proxy.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"

	"antigravity-go-proxy/internal/accounts"
	"antigravity-go-proxy/internal/auth"
	"antigravity-go-proxy/internal/cloudcode"
)

func main() {
	model := flag.String("model", "gemini-3.8-flash-high", "upstream model ID")
	promptKB := flag.Int("kb", 0, "pad the prompt to this many KB to emulate a large context")
	flag.Parse()

	path, err := accounts.DefaultConfigPath()
	if err != nil {
		fmt.Println("config path:", err)
		return
	}
	file, err := accounts.Load(path)
	if err != nil {
		fmt.Println("load accounts:", err)
		return
	}
	resolver := accounts.NewCredentialResolver(auth.Manager{}, nil)

	for _, account := range file.Accounts {
		fmt.Printf("=== %s (project=%s)\n", account.Email, account.ProjectID)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		credentials, err := resolver.Resolve(ctx, account)
		if err != nil {
			fmt.Println("  resolve:", err)
			cancel()
			continue
		}
		client := cloudcode.New(cloudcode.Options{AccessToken: credentials.AccessToken, Timeout: 30 * time.Second})
		text := "Say OK"
		if *promptKB > 0 {
			text = strings.Repeat("lorem ipsum dolor sit amet ", *promptKB*1024/27) + " Say OK"
		}
		payload := map[string]any{
			"project": account.ProjectID,
			"model":   *model,
			"request": map[string]any{
				"contents": []any{map[string]any{
					"role":  "user",
					"parts": []any{map[string]any{"text": text}},
				}},
			},
		}
		_, requestErr := client.StreamGenerateContent(ctx, payload, cloudcode.RequestOptions{}, func(event cloudcode.SSEEvent) error { return nil })
		if requestErr == nil {
			fmt.Println("  OK: request succeeded")
		} else {
			var httpErr *cloudcode.HTTPError
			if errors.As(requestErr, &httpErr) {
				fmt.Printf("  status=%d endpoint=%s\n", httpErr.StatusCode, httpErr.Endpoint)
				fmt.Printf("  retry-after=%q x-ratelimit-reset=%q\n", httpErr.Header.Get("Retry-After"), httpErr.Header.Get("x-ratelimit-reset"))
				fmt.Printf("  body=%s\n", string(httpErr.Body))
			} else {
				fmt.Println("  error:", requestErr)
			}
		}
		client.CloseIdleConnections()
		cancel()
	}
}
