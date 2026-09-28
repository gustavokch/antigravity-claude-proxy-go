package mitm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"
)

// ValidateListen accepts only loopback listen addresses: the CA key and the
// decrypted traffic must never be reachable from another machine.
func ValidateListen(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("mitm listen %q must be host:port", addr)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 0 || n > 65535 {
		return fmt.Errorf("mitm listen port %q is not valid", port)
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("mitm listen host %q must be a loopback address (127.0.0.1, ::1 or localhost)", host)
}

// RuntimeConfig is what StartRuntime needs; it deliberately mirrors no other
// package's config type.
type RuntimeConfig struct {
	Dir         string // directory holding the CA files
	Listen      string
	RegistryMax int
	RegistryTTL time.Duration
	Logger      *slog.Logger
}

// Runtime bundles a running forward proxy with what the management API reads.
type Runtime struct {
	CA       *CA
	Registry *Registry
	Server   *Server
	// Addr is the address actually bound (useful when Listen used port 0).
	Addr string
}

// StartRuntime loads or creates the CA, binds the listener and serves in the
// background. Any error leaves nothing running.
func StartRuntime(cfg RuntimeConfig) (*Runtime, error) {
	if err := ValidateListen(cfg.Listen); err != nil {
		return nil, err
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	ca, err := LoadOrCreateCA(cfg.Dir, nil)
	if err != nil {
		return nil, err
	}
	registry := NewRegistry(cfg.RegistryMax, cfg.RegistryTTL, nil)
	srv, err := New(Options{CA: ca, Registry: registry, Logger: cfg.Logger})
	if err != nil {
		return nil, err
	}
	l, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return nil, fmt.Errorf("mitm: listen: %w", err)
	}
	rt := &Runtime{CA: ca, Registry: registry, Server: srv, Addr: l.Addr().String()}
	go func() {
		if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
			cfg.Logger.Error("mitm forward proxy stopped", "error", err)
		}
	}()
	return rt, nil
}

// Shutdown stops the listener and closes live connections.
func (r *Runtime) Shutdown(ctx context.Context) error { return r.Server.Shutdown(ctx) }
