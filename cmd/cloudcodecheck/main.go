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

// endpointOverride maps the -endpoint flag to a known Cloud Code host. Free-form
// URLs are refused: the request carries the account's OAuth bearer token.
func endpointOverride(name string) (string, error) {
	switch name {
	case "daily":
		return cloudcode.DailyEndpoint, nil
	case "prod":
		return cloudcode.ProdEndpoint, nil
	}
	return "", fmt.Errorf("unknown endpoint %q: use daily or prod", name)
}

func main() {
	operation := flag.String("operation", "models", "gate operation: models or load")
	project := flag.String("project", "", "optional Cloud Code project ID (models only; agy sends none on load)")
	timeout := flag.Duration("timeout", 30*time.Second, "request timeout")
	details := flag.Bool("details", false, "print the successful response JSON for inspection")
	endpoint := flag.String("endpoint", "", "optional Cloud Code endpoint override: daily or prod")
	flag.Parse()

	var override []string
	if *endpoint != "" {
		host, err := endpointOverride(*endpoint)
		if err != nil {
			log.Fatal(err)
		}
		override = []string{host}
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	credentials, err := (auth.Manager{}).Get(ctx)
	if err != nil {
		log.Fatal(err)
	}
	client := cloudcode.New(cloudcode.Options{AccessToken: credentials.AccessToken, Timeout: *timeout})
	var response cloudcode.Response
	switch *operation {
	case "models":
		if override != nil {
			response, err = client.FetchAvailableModelsAt(ctx, override, *project)
		} else {
			response, err = client.FetchAvailableModels(ctx, *project)
		}
	case "load":
		if override != nil {
			response, err = client.LoadCodeAssistAt(ctx, override)
		} else {
			response, err = client.LoadCodeAssist(ctx)
		}
	default:
		log.Fatalf("unknown operation %q", *operation)
	}
	if err != nil {
		log.Fatal(err)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(response.Body, &document); err != nil {
		log.Fatalf("decode successful Cloud Code response: %v", err)
	}
	if *details {
		var pretty any
		if err := json.Unmarshal(response.Body, &pretty); err != nil {
			log.Fatalf("decode successful Cloud Code response details: %v", err)
		}
		encoded, err := json.MarshalIndent(pretty, "", "  ")
		if err != nil {
			log.Fatalf("encode successful Cloud Code response details: %v", err)
		}
		fmt.Println(string(encoded))
		return
	}
	fmt.Printf("cloudcode_ok operation=%s endpoint=%s status=%d bytes=%d top_level_fields=%d\n",
		*operation, response.Endpoint, response.StatusCode, len(response.Body), len(document))
}
