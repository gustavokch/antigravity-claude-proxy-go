package calibrate

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"antigravity-go-proxy/internal/config"
)

func TestFragmentKeysMatchTheConfigStruct(t *testing.T) {
	entries := rejects("a@example.com", 5, 4, 37)
	for index := range 3 {
		entries = append(entries, rejectRecoverPair("a@example.com", journalStart.Add(time.Duration(index+10)*time.Hour), 20*time.Minute)...)
	}
	fragment := Derive(Journal{Entries: entries}).Fragment()

	// The bucket is nested under accountSelection in the real config, so lift
	// it into the shape config.Config actually declares before decoding.
	nested := map[string]any{}
	for key, value := range fragment {
		if key == "accountSelection.tokenBucket" {
			nested["accountSelection"] = map[string]any{"tokenBucket": value}
			continue
		}
		nested[key] = value
	}

	encoded, err := json.Marshal(nested)
	if err != nil {
		t.Fatalf("marshal fragment: %v", err)
	}

	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var parsed config.Config
	if err := decoder.Decode(&parsed); err != nil {
		t.Fatalf("fragment does not match config.Config: %v\nfragment: %s", err, encoded)
	}

	if parsed.RequestDelayMs != 1667 {
		t.Fatalf("RequestDelayMs after the round trip: got %d, want 1667", parsed.RequestDelayMs)
	}
	if parsed.SharedThrottleWindowMs != 1_200_000 {
		t.Fatalf("SharedThrottleWindowMs after the round trip: got %d, want 1200000", parsed.SharedThrottleWindowMs)
	}
	if len(parsed.CapacityBackoffTiersMs) != 3 {
		t.Fatalf("CapacityBackoffTiersMs after the round trip: got %v, want three tiers", parsed.CapacityBackoffTiersMs)
	}
}

// AccountSelectionConfig.TokenBucket is a map[string]any
// (internal/config/config.go:97), so DisallowUnknownFields cannot police its
// keys. Check them against the defaults the proxy ships instead.
func TestBucketKeysExistInTheDefaultConfig(t *testing.T) {
	entries := rejects("a@example.com", 5, 4, 37)
	fragment := Derive(Journal{Entries: entries}).Fragment()

	bucket, ok := fragment["accountSelection.tokenBucket"].(map[string]any)
	if !ok {
		t.Fatalf("accountSelection.tokenBucket: got %T, want map", fragment["accountSelection.tokenBucket"])
	}
	if len(bucket) == 0 {
		t.Fatal("fragment emitted an empty token bucket")
	}
	defaults := config.DefaultConfig().AccountSelection.TokenBucket
	for key := range bucket {
		if _, present := defaults[key]; !present {
			t.Fatalf("fragment emits tokenBucket key %q, which the default config does not define", key)
		}
	}
}
