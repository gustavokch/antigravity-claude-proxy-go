package zen

import (
	"context"
	"crypto/tls"
	_ "embed"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"

	utls "github.com/refraction-networking/utls"
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

	specOnce sync.Once
	bunSpec  *utls.ClientHelloSpec
	specErr  error
)

// bunHelloSpec fingerprints the captured Bun ClientHello once at first use.
func bunHelloSpec() (*utls.ClientHelloSpec, error) {
	specOnce.Do(func() {
		fp := &utls.Fingerprinter{}
		bunSpec, specErr = fp.RawClientHello(opencodeClientHello)
		if specErr == nil && bunSpec != nil {
			if alpnErr := validateSpecALPN(bunSpec); alpnErr != nil {
				bunSpec, specErr = nil, alpnErr
			}
		}
		if specErr != nil {
			slog.Error("zen tls: captured opencode ClientHello cannot be fingerprinted; TLS disguise unavailable", "error", specErr)
		}
	})
	return bunSpec, specErr
}

// specALPN returns the ALPN protocols the replayed hello will offer.
func specALPN(spec *utls.ClientHelloSpec) []string {
	for _, ext := range spec.Extensions {
		if alpn, ok := ext.(*utls.ALPNExtension); ok {
			return alpn.AlpnProtocols
		}
	}
	return nil
}

// validateSpecALPN rejects a captured ClientHello whose ALPN list is not
// exactly ["http/1.1"]. JA4 encodes the ALPN count, not the protocols, so a
// regenerated capture advertising h2 would pass the fingerprint gate while
// net/http's custom-dial path has no HTTP/2 handler — every request would
// hang. A mismatch leaves the disguise unavailable instead of broken.
func validateSpecALPN(spec *utls.ClientHelloSpec) error {
	alpn := specALPN(spec)
	if len(alpn) != 1 || alpn[0] != "http/1.1" {
		return fmt.Errorf("captured ClientHello offers ALPN %v, want [http/1.1]", alpn)
	}
	return nil
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

// TLSClient returns the shared client for zen-bound requests:
// http.DefaultClient while the TLS disguise is off, a utls-backed client
// once it is enabled. The returned client is stable per configuration and
// carries no timeout — streaming callers own their deadlines.
func TLSClient() *http.Client {
	tlsMu.Lock()
	defer tlsMu.Unlock()
	if !tlsCfg.Enabled {
		return http.DefaultClient
	}
	if tlsClient == nil {
		if transport := transportLocked(); transport != nil {
			tlsClient = &http.Client{Transport: transport}
		}
	}
	if tlsClient == nil {
		return http.DefaultClient
	}
	return tlsClient
}

// Transport returns the shared utls-backed transport for zen-bound requests,
// or nil when the disguise is off or the captured hello cannot be
// fingerprinted — callers pass the result straight to http.Client/ReverseProxy,
// which fall back to the default transport on nil. One transport is cached
// per configuration so keep-alive pools survive across requests; it is a
// clone of http.DefaultTransport (proxy from the environment, 30s dial
// timeout, 10s TLS handshake timeout, 90s idle timeout) with DialTLSContext
// pointed at the captured hello. The shared default transport itself is
// never mutated.
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
	spec, err := bunHelloSpec()
	if err != nil || spec == nil {
		return nil
	}
	if tlsTransport == nil {
		tr := &http.Transport{Proxy: http.ProxyFromEnvironment}
		if base, ok := http.DefaultTransport.(*http.Transport); ok {
			tr = base.Clone()
		}
		tr.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialOpencodeTLS(ctx, network, addr, spec)
		}
		tlsTransport = tr
	}
	return tlsTransport
}

// spoofedConn exposes the utls handshake result to net/http as a
// crypto/tls.ConnectionState (net/http type-asserts the dialed connection
// for it; utls carries its own copy of the type). Only net.Conn methods are
// promoted, so net/http does not try to re-run the handshake.
type spoofedConn struct {
	net.Conn
	state tls.ConnectionState
}

func (c *spoofedConn) ConnectionState() tls.ConnectionState { return c.state }

// dialOpencodeTLS dials TCP, wraps the connection in a utls client speaking
// the captured Bun ClientHello, and completes the handshake before returning.
func dialOpencodeTLS(ctx context.Context, network, addr string, spec *utls.ClientHelloSpec) (net.Conn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("zen tls: split %q: %w", addr, err)
	}
	raw, err := (&net.Dialer{}).DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	uconn := utls.UClient(raw, &utls.Config{ServerName: host, MinVersion: tls.VersionTLS12}, utls.HelloCustom)
	if err := uconn.ApplyPreset(spec); err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("zen tls: apply captured ClientHello: %w", err)
	}
	// The capture carries the original target's SNI; re-point it at the
	// host actually being dialed so certificate verification matches.
	uconn.SetSNI(host)
	if err := uconn.HandshakeContext(ctx); err != nil {
		_ = uconn.Close()
		return nil, fmt.Errorf("zen tls: utls handshake: %w", err)
	}
	state := uconn.ConnectionState()
	return &spoofedConn{Conn: uconn, state: tls.ConnectionState{
		Version:            state.Version,
		HandshakeComplete:  state.HandshakeComplete,
		DidResume:          state.DidResume,
		CipherSuite:        state.CipherSuite,
		NegotiatedProtocol: state.NegotiatedProtocol,
		ServerName:         state.ServerName,
	}}, nil
}
