// quotaprobe dumps raw RetrieveUserQuotaSummary / RetrieveUserQuota /
// FetchAvailableModels payloads for quota-reading debugging. Read-only.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"time"

	"antigravity-go-proxy/internal/auth"
	"antigravity-go-proxy/internal/cloudcode"
)

func main() {
	project := flag.String("project", "", "optional Cloud Code project ID")
	timeout := flag.Duration("timeout", 30*time.Second, "request timeout")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	credentials, err := (auth.Manager{}).Get(ctx)
	if err != nil {
		log.Fatal(err)
	}
	client := cloudcode.New(cloudcode.Options{AccessToken: credentials.AccessToken, Timeout: *timeout})

	call := func(name string, fetch func() (cloudcode.Response, error)) {
		fmt.Printf("===== %s =====\n", name)
		response, err := fetch()
		if err != nil {
			fmt.Println("ERROR:", err)
			return
		}
		var pretty any
		if jsonErr := json.Unmarshal(response.Body, &pretty); jsonErr != nil {
			fmt.Println("NON-JSON:", string(response.Body[:min(len(response.Body), 2000)]))
			return
		}
		encoded, _ := json.MarshalIndent(pretty, "", "  ")
		fmt.Println(string(encoded))
	}

	call("retrieveUserQuotaSummary", func() (cloudcode.Response, error) {
		return client.RetrieveUserQuotaSummary(ctx, *project)
	})
	call("retrieveUserQuota", func() (cloudcode.Response, error) {
		return client.RetrieveUserQuota(ctx, *project)
	})
	call("fetchAvailableModels", func() (cloudcode.Response, error) {
		return client.FetchAvailableModels(ctx, *project)
	})
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
