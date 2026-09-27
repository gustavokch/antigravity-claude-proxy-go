//go:build ignore

// gen_pricing regenerates litellm_claude.json, the Claude-only pricing
// snapshot LiteLLMPricer embeds. It downloads LiteLLM's price table
// (https://github.com/BerriAI/litellm, MIT License), keeps the Claude keys
// and the fields the pricer reads, and writes them sorted and indented so the
// diff of a refresh is reviewable.
//
// Run it with `go generate ./internal/claudecode/ccusage/`.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

const sourceURL = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"

// keyPrefixes must match claudeKeyPrefixes in pricing.go.
var keyPrefixes = []string{"claude-", "anthropic.claude-", "anthropic/claude-"}

// fields are the LiteLLM fields LiteLLMPricer reads, besides
// provider_specific_entry.fast.
var fields = []string{
	"input_cost_per_token",
	"output_cost_per_token",
	"cache_creation_input_token_cost",
	"cache_read_input_token_cost",
	"input_cost_per_token_above_200k_tokens",
	"output_cost_per_token_above_200k_tokens",
	"cache_creation_input_token_cost_above_200k_tokens",
	"cache_read_input_token_cost_above_200k_tokens",
	"max_input_tokens",
}

func main() {
	client := &http.Client{Timeout: time.Minute}
	resp, err := client.Get(sourceURL)
	if err != nil {
		log.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Fatalf("GET %s: %s", sourceURL, resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Fatal(err)
	}

	var all map[string]json.RawMessage
	if err := json.Unmarshal(body, &all); err != nil {
		log.Fatal(err)
	}

	out := map[string]map[string]json.RawMessage{}
	for key, raw := range all {
		if !hasKeyPrefix(key) {
			continue
		}
		var entry map[string]json.RawMessage
		if err := json.Unmarshal(raw, &entry); err != nil {
			continue
		}
		// Entries without both base rates cannot be priced.
		if !isNumber(entry["input_cost_per_token"]) || !isNumber(entry["output_cost_per_token"]) {
			continue
		}
		kept := map[string]json.RawMessage{}
		for _, f := range fields {
			if v, ok := entry[f]; ok && !bytes.Equal(v, []byte("null")) {
				kept[f] = v
			}
		}
		if pse, ok := entry["provider_specific_entry"]; ok {
			var specific map[string]json.RawMessage
			if json.Unmarshal(pse, &specific) == nil && isNumber(specific["fast"]) {
				kept["provider_specific_entry"] = json.RawMessage(`{"fast":` + string(specific["fast"]) + `}`)
			}
		}
		out[key] = kept
	}
	if len(out) == 0 {
		log.Fatal("no Claude pricing entries found")
	}

	// encoding/json sorts map keys, which keeps the output deterministic.
	data, err := json.MarshalIndent(out, "", "\t")
	if err != nil {
		log.Fatal(err)
	}
	data = append(data, '\n')
	if err := os.WriteFile("litellm_claude.json", data, 0o644); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("wrote %d Claude pricing entries\n", len(out))
}

func hasKeyPrefix(key string) bool {
	for _, p := range keyPrefixes {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

func isNumber(raw json.RawMessage) bool {
	var f float64
	return len(raw) > 0 && !bytes.Equal(raw, []byte("null")) && json.Unmarshal(raw, &f) == nil
}
