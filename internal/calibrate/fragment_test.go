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

	// The fragment is printed for an operator to paste into config.json, so it
	// must decode into config.Config exactly as emitted — no key rewriting.
	encoded, err := json.Marshal(fragment)
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

	bucket, ok := bucketOf(fragment)
	if !ok {
		t.Fatalf("accountSelection.tokenBucket: got %v, want a nested map", fragment["accountSelection"])
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
