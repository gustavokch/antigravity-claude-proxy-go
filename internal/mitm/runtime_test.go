package mitm

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"
)

func TestValidateListen(t *testing.T) {
	good := []string{"127.0.0.1:8092", "[::1]:8092", "localhost:8092", "127.0.0.1:0"}
	bad := []string{"0.0.0.0:8092", ":8092", "192.168.1.5:8092", "example.com:8092", "127.0.0.1", "127.0.0.1:99999", "127.0.0.1:x"}
	for _, addr := range good {
		if err := ValidateListen(addr); err != nil {
			t.Errorf("ValidateListen(%q) = %v, want nil", addr, err)
		}
	}
	for _, addr := range bad {
		if err := ValidateListen(addr); err == nil {
			t.Errorf("ValidateListen(%q) = nil, want error", addr)
		}
	}
}

func TestStartRuntimeServesAndShutsDown(t *testing.T) {
	rt, err := StartRuntime(RuntimeConfig{Dir: filepath.Join(t.TempDir(), "mitm"), Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", rt.Addr)
	if err != nil {
		t.Fatalf("listener not reachable: %v", err)
	}
	conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := rt.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := net.DialTimeout("tcp", rt.Addr, 200*time.Millisecond); err == nil {
		t.Fatal("listener still accepting after Shutdown")
	}
}

func TestStartRuntimeRefusesNonLoopback(t *testing.T) {
	if _, err := StartRuntime(RuntimeConfig{Dir: t.TempDir(), Listen: "0.0.0.0:0"}); err == nil {
		t.Fatal("non-loopback listen must be refused")
	}
}
