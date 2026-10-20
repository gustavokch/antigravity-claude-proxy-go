package zen

import (
	_ "embed"
	"log/slog"
	"net/http"
	"sync"

	"antigravity-go-proxy/internal/tlsreplay"
	"antigravity-go-proxy/internal/transport"
)

// The Zen free-tier gate discriminates on the TLS handshake: the genuine
// OpenCode client is Bun (BoringSSL), every third-party stack gets 403 even
// with byte-faithful headers. This file replays the captured Bun ClientHello
// through utls. The captured record includes the TLS record header, as
// FingerprintClientHello requires.
//
//go:embed opencode-clienthello.bin
var opencodeClientHello []byte

// ZenTLSConfig is the live TLS-fingerprint disguise for zen-bound requests.
type ZenTLSConfig struct {
	Enabled bool
}

var (
	tlsMu        sync.RWMutex
	tlsCfg       ZenTLSConfig
	tlsClient    *http.Client
	tlsTransport *http.Transport

	helloOnce sync.Once
	bunHello  *tlsreplay.Hello
	helloErr  error
)

// bunReplay fingerprints the captured Bun ClientHello once at first use. The
// replayed hello must offer exactly http/1.1: tlsreplay rejects any other ALPN
// list, because an h2 capture would pass the JA4 gate while the custom-dial
// path cannot serve HTTP/2.
func bunReplay() (*tlsreplay.Hello, error) {
	helloOnce.Do(func() {
		bunHello, helloErr = tlsreplay.New(opencodeClientHello, []string{"http/1.1"})
		if helloErr != nil {
			slog.Error("zen tls: captured opencode ClientHello cannot be fingerprinted; TLS disguise unavailable", "error", helloErr)
		}
	})
	return bunHello, helloErr
}

// SetTLSConfig replaces the live TLS disguise configuration. Safe for
// concurrent use with request handling; called from the config hook only.
func SetTLSConfig(cfg ZenTLSConfig) {
	tlsMu.Lock()
	tlsCfg = cfg
	tlsClient = nil
	tlsTransport = nil
	tlsMu.Unlock()
}

// GetTLSConfig returns a copy of the live TLS configuration.
func GetTLSConfig() ZenTLSConfig {
	tlsMu.RLock()
	defer tlsMu.RUnlock()
	return tlsCfg
}

// TLSClient returns the shared client for zen-bound requests: the shared plain
// client while the TLS disguise is off, a utls-backed client once it is enabled.
// Neither leg is http.DefaultClient, whose transport puts no bound on the wait
// for response headers. The returned client is stable per configuration and
// carries no timeout — streaming callers own their deadlines.
func TLSClient() *http.Client {
	tlsMu.Lock()
	defer tlsMu.Unlock()
	if !tlsCfg.Enabled {
		return plainClient
	}
	if tlsClient == nil {
		if transport := transportLocked(); transport != nil {
			tlsClient = &http.Client{Transport: transport}
		}
	}
	if tlsClient == nil {
		// no captured hello to fingerprint: still bounded
		return plainClient
	}
	return tlsClient
}

// Transport returns the shared utls-backed transport for zen-bound requests,
// or nil when the disguise is off or the captured hello cannot be
// fingerprinted — callers pass the result straight to http.Client/ReverseProxy,
// which fall back to the default transport on nil. One transport is cached
// per configuration so keep-alive pools survive across requests; it is
// transport.Upstream() (a clone of http.DefaultTransport with a bound on time to
// response headers) with DialTLSContext pointed at the captured hello. The shared
// default transport itself is never mutated.
func Transport() *http.Transport {
	tlsMu.Lock()
	defer tlsMu.Unlock()
	return transportLocked()
}

// transportLocked builds the cached transport; callers hold tlsMu.
func transportLocked() *http.Transport {
	if !tlsCfg.Enabled {
		return nil
	}
	hello, err := bunReplay()
	if err != nil || hello == nil {
		return nil
	}
	if tlsTransport == nil {
		tlsTransport = hello.Transport(transport.Upstream())
	}
	return tlsTransport
}
