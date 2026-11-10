package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"antigravity-go-proxy/internal/cloudcode"
)

func TestJA4ProbeReachesCloudCodeWithoutAccountCredentials(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != cloudcode.PathLoadCodeAssist {
			t.Errorf("request = %s %s, want POST %s", r.Method, r.URL.Path, cloudcode.PathLoadCodeAssist)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer clientprobe-not-a-token" {
			t.Errorf("Authorization = %q, want the probe's invalid token", got)
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer server.Close()

	status, err := probeJA4(context.Background(), server.URL)
	if err != nil {
		t.Fatalf("probe failed before the authentication rejection: %v", err)
	}
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
}

func TestJA4ProbePreservesTransportFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := probeJA4(ctx, cloudcode.DailyEndpoint)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}
