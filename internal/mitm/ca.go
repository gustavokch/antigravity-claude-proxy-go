// Package mitm is an observe-only forward proxy for Claude Code cloud-session
// traffic. It terminates TLS for a small set of Anthropic-owned hosts with a
// locally generated, name-constrained CA, relays HTTP/1.1 byte for byte, and
// reports masked route summaries to a registry. It never rewrites traffic.
package mitm

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// PermittedDomains are the only domains the CA may issue for. The same list
// is embedded in the CA certificate as a critical name constraint.
var PermittedDomains = []string{"anthropic.com", "claude.ai", "claude.com"}

// ErrHostNotPermitted is returned when a leaf is requested for a host outside
// PermittedDomains.
var ErrHostNotPermitted = errors.New("mitm: host is not in the permitted domain set")

const (
	caCertFile = "ca.pem"
	caKeyFile  = "ca-key.pem"
	caLifetime = 365 * 24 * time.Hour
	// A CA with less than this much life left is regenerated on load.
	caRenewWindow = 7 * 24 * time.Hour
	leafLifetime  = 24 * time.Hour
	// maxCachedLeaves bounds the leaf cache, which is keyed by client-chosen
	// host names.
	maxCachedLeaves = 256
)

// HostPermitted reports whether host equals, or is a subdomain of, a
// permitted domain.
func HostPermitted(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	for _, domain := range PermittedDomains {
		if host == domain || strings.HasSuffix(host, "."+domain) {
			return true
		}
	}
	return false
}

// CA is a local certificate authority. Only its certificate is ever exported.
type CA struct {
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	certPEM []byte
	now     func() time.Time

	mu     sync.Mutex
	leaves map[string]*tls.Certificate
}

// LoadOrCreateCA loads the CA from dir, creating it (dir 0700, key 0600) when
// either file is missing, unreadable as PEM, or close to expiry.
func LoadOrCreateCA(dir string, now func() time.Time) (*CA, error) {
	if now == nil {
		now = time.Now
	}
	if ca, err := loadCA(dir, now); err == nil {
		return ca, nil
	}
	return createCA(dir, now)
}

func loadCA(dir string, now func() time.Time) (*CA, error) {
	certPEM, err := os.ReadFile(filepath.Join(dir, caCertFile))
	if err != nil {
		return nil, err
	}
	keyPEM, err := os.ReadFile(filepath.Join(dir, caKeyFile))
	if err != nil {
		return nil, err
	}
	certBlock, _ := pem.Decode(certPEM)
	keyBlock, _ := pem.Decode(keyPEM)
	if certBlock == nil || keyBlock == nil {
		return nil, errors.New("mitm: CA files are not PEM")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, err
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, err
	}
	if now().Add(caRenewWindow).After(cert.NotAfter) {
		return nil, errors.New("mitm: CA is close to expiry")
	}
	// Export the certificate re-encoded from the parsed value, never the raw
	// file: an extra PEM block in ca.pem (a cert+key bundle) must not be served.
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
	return &CA{cert: cert, key: key, certPEM: certPEM, now: now, leaves: map[string]*tls.Certificate{}}, nil
}

func createCA(dir string, now func() time.Time) (*CA, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("mitm: create CA dir: %w", err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	template := &x509.Certificate{
		SerialNumber:                serial,
		Subject:                     pkix.Name{CommonName: "antigravity-proxy local MITM CA"},
		NotBefore:                   now().Add(-time.Hour),
		NotAfter:                    now().Add(caLifetime),
		IsCA:                        true,
		BasicConstraintsValid:       true,
		MaxPathLen:                  0,
		MaxPathLenZero:              true,
		KeyUsage:                    x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		PermittedDNSDomainsCritical: true,
		PermittedDNSDomains:         PermittedDomains,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(filepath.Join(dir, caKeyFile), keyPEM, 0o600); err != nil {
		return nil, fmt.Errorf("mitm: write CA key: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, caCertFile), certPEM, 0o644); err != nil {
		return nil, fmt.Errorf("mitm: write CA cert: %w", err)
	}
	return &CA{cert: cert, key: key, certPEM: certPEM, now: now, leaves: map[string]*tls.Certificate{}}, nil
}

func randomSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
}

// CertPEM returns the CA certificate in PEM form. The key is never exposed.
func (ca *CA) CertPEM() []byte { return append([]byte(nil), ca.certPEM...) }

// Fingerprint is the hex SHA-256 of the CA certificate.
func (ca *CA) Fingerprint() string {
	sum := sha256.Sum256(ca.cert.Raw)
	return hex.EncodeToString(sum[:])
}

// NotAfter is the CA certificate expiry.
func (ca *CA) NotAfter() time.Time { return ca.cert.NotAfter }

// LeafFor returns a cached or freshly minted TLS certificate for host.
func (ca *CA) LeafFor(host string) (*tls.Certificate, error) {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if !HostPermitted(host) {
		return nil, fmt.Errorf("%w: %q", ErrHostNotPermitted, host)
	}
	ca.mu.Lock()
	defer ca.mu.Unlock()
	if leaf, ok := ca.leaves[host]; ok && leaf.Leaf != nil && ca.now().Add(time.Hour).Before(leaf.Leaf.NotAfter) {
		return leaf, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    ca.now().Add(-time.Hour),
		NotAfter:     ca.now().Add(leafLifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return nil, err
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	leaf := &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: parsed}
	if len(ca.leaves) >= maxCachedLeaves {
		ca.pruneLeavesLocked()
	}
	ca.leaves[host] = leaf
	return leaf, nil
}

// pruneLeavesLocked makes room for one more cached leaf: expired leaves go
// first, then arbitrary ones, so a client cycling through host names cannot
// grow the cache without bound.
func (ca *CA) pruneLeavesLocked() {
	now := ca.now()
	for host, leaf := range ca.leaves {
		if leaf.Leaf == nil || !now.Add(time.Hour).Before(leaf.Leaf.NotAfter) {
			delete(ca.leaves, host)
		}
	}
	for host := range ca.leaves {
		if len(ca.leaves) < maxCachedLeaves {
			break
		}
		delete(ca.leaves, host)
	}
}
