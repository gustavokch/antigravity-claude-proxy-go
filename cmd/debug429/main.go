// Command debug429 is a one-off diagnostic: it replays a minimal
// gemini-3.8-flash-high request per account and prints the upstream HTTP
// status, rate-limit headers, and the full error body, so the 429 quota
// dimension (user vs project) becomes visible. Not part of the proxy.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"

	"antigravity-go-proxy/internal/accounts"
	"antigravity-go-proxy/internal/auth"
	"antigravity-go-proxy/internal/cloudcode"
	proxyformat "antigravity-go-proxy/internal/format"
	"antigravity-go-proxy/internal/modelcatalog"
)

func main() {
	model := flag.String("model", "gemini-3.8-flash-high", "upstream model ID")
	promptKB := flag.Int("kb", 0, "pad the prompt to this many KB to emulate a large context")
	thinkingLevel := flag.String("thinking", "", "emit generationConfig.thinkingConfig with this thinkingLevel (e.g. HIGH)")
	fetchCat := flag.Bool("catalog", false, "fetch and print catalog models")
	proxyfmt := flag.Bool("proxyfmt", false, "format request using proxy's format.Builder")
	toolsFlag := flag.Bool("tools", false, "include Anthropic-style tools in request")
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
		if *fetchCat {
			resp, err := client.FetchAvailableModels(ctx, account.ProjectID)
			if err != nil {
				fmt.Println("  fetch models:", err)
			} else {
				var parsed map[string]any
				_ = json.Unmarshal(resp.Body, &parsed)
				models, _ := parsed["models"].(map[string]any)
				fmt.Println("  upstream models count:", len(models))
				for k := range models {
					if strings.Contains(k, "flash") || strings.Contains(k, "3.8") {
						fmt.Printf("    - %s\n", k)
					}
				}
				cat, err := modelcatalog.Parse(resp.Body)
				if err != nil {
					fmt.Println("  parse catalog:", err)
				} else {
					for _, target := range []string{"gemini-3.8-flash-high", "gemini-3.8-flash", "gemini-3.7-flash-high"} {
						m, err := cat.Resolve(target)
						if err != nil {
							fmt.Printf("    Resolve(%q): err=%v\n", target, err)
						} else {
							fmt.Printf("    Resolve(%q): ID=%q UpstreamID=%q ThinkingLevel=%q\n", target, m.ID, m.GetUpstreamID(), m.ThinkingLevel)
						}
					}
				}
			}
			client.CloseIdleConnections()
			cancel()
			continue
		}
		text := "Say OK"
		if *promptKB > 0 {
			text = strings.Repeat("lorem ipsum dolor sit amet ", *promptKB*1024/27) + " Say OK"
		}
		request := map[string]any{
			"contents": []any{map[string]any{
				"role":  "user",
				"parts": []any{map[string]any{"text": text}},
			}},
		}
		if *thinkingLevel != "" {
			request["generationConfig"] = map[string]any{
				"thinkingConfig": map[string]any{
					"includeThoughts": true,
					"thinkingLevel":   *thinkingLevel,
				},
			}
		}
		payload := map[string]any{
			"project": account.ProjectID,
			"model":   *model,
			"request": request,
		}
		if *proxyfmt {
			builder := proxyformat.NewBuilder()
			req := map[string]any{
				"model": *model,
				"messages": []any{
					map[string]any{
						"role":    "user",
						"content": text,
					},
				},
			}
			if *toolsFlag {
				req["tools"] = []any{
					map[string]any{
						"name":        "bash",
						"description": "Run a shell command",
						"input_schema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"command": map[string]any{"type": "string"},
							},
							"required": []any{"command"},
						},
					},
					map[string]any{
						"name":        "read_file",
						"description": "Read a file from disk",
						"input_schema": map[string]any{
							"type": "object",
							"properties": map[string]any{
								"path": map[string]any{"type": "string"},
							},
							"required": []any{"path"},
						},
					},
				}
			}
			payload = builder.BuildCloudCodeRequestWithModel(req, account.ProjectID, account.Email, proxyformat.ModelOptions{
				SupportsThinking: true,
				ThinkingLevel:    *thinkingLevel,
			})
		}
		_, requestErr := client.StreamGenerateContent(ctx, payload, cloudcode.RequestOptions{}, func(event cloudcode.SSEEvent) error {
			fmt.Printf("  event: %.300s\n", string(event.Data))
			return nil
		})
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
