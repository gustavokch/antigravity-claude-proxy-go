package mitm

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func newTestCA(t *testing.T) *CA {
	t.Helper()
	ca, err := LoadOrCreateCA(filepath.Join(t.TempDir(), "mitm"), nil)
	if err != nil {
		t.Fatal(err)
	}
	return ca
}

func TestLeafVerifiesForPermittedHost(t *testing.T) {
	ca := newTestCA(t)
	leaf, err := ca.LeafFor("api.anthropic.com")
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	if _, err := leaf.Leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: "api.anthropic.com"}); err != nil {
		t.Fatalf("leaf should verify: %v", err)
	}
}

func TestLeafRefusedOutsideConstraint(t *testing.T) {
	ca := newTestCA(t)
	for _, host := range []string{"evil.com", "anthropic.com.evil.com", "notanthropic.com", "example.org"} {
		if _, err := ca.LeafFor(host); !errors.Is(err, ErrHostNotPermitted) {
			t.Errorf("LeafFor(%q) err = %v, want ErrHostNotPermitted", host, err)
		}
	}
}

// The constraint must be enforced by the certificate itself, not only by
// LeafFor: a leaf hand-signed with the CA key for another domain must fail
// verification.
func TestCAConstraintEmbeddedInCertificate(t *testing.T) {
	ca := newTestCA(t)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	serial, _ := randomSerial()
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: "evil.com"}, DNSNames: []string{"evil.com"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	evil, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	if _, err := evil.Verify(x509.VerifyOptions{Roots: pool, DNSName: "evil.com"}); err == nil {
		t.Fatal("hand-signed leaf for evil.com verified; name constraint is not enforced")
	}
}

func TestCAPersistsAndKeyIsPrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "mitm")
	first, err := LoadOrCreateCA(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadOrCreateCA(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.Fingerprint() != second.Fingerprint() {
		t.Fatal("reload produced a different CA")
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(dir, caKeyFile))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("key mode = %v, want 0600", info.Mode().Perm())
		}
	}
}

func TestCARegeneratedWhenNearExpiry(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "mitm")
	past := func() time.Time { return time.Now().Add(-364 * 24 * time.Hour) }
	old, err := LoadOrCreateCA(dir, past)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := LoadOrCreateCA(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if old.Fingerprint() == fresh.Fingerprint() {
		t.Fatal("near-expiry CA was not regenerated")
	}
}

func TestHostPermitted(t *testing.T) {
	cases := map[string]bool{
		"api.anthropic.com": true, "anthropic.com": true, "claude.ai": true, "x.claude.com": true,
		"API.Anthropic.COM.": true, "evil.com": false, "anthropic.com.evil.com": false, "xclaude.ai": false, "": false,
	}
	for host, want := range cases {
		if got := HostPermitted(host); got != want {
			t.Errorf("HostPermitted(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestLeafCacheIsBounded(t *testing.T) {
	ca := newTestCA(t)
	for i := 0; i < maxCachedLeaves+50; i++ {
		if _, err := ca.LeafFor(fmt.Sprintf("host%d.anthropic.com", i)); err != nil {
			t.Fatal(err)
		}
	}
	ca.mu.Lock()
	defer ca.mu.Unlock()
	if len(ca.leaves) > maxCachedLeaves {
		t.Fatalf("leaf cache holds %d entries, cap is %d", len(ca.leaves), maxCachedLeaves)
	}
	if _, ok := ca.leaves[fmt.Sprintf("host%d.anthropic.com", maxCachedLeaves+49)]; !ok {
		t.Fatal("the newest leaf must stay cached")
	}
}

func TestLeafCachePrunesExpiredLeavesFirst(t *testing.T) {
	ca := newTestCA(t)
	now := time.Now()
	ca.now = func() time.Time { return now }
	for i := 0; i < maxCachedLeaves; i++ {
		if _, err := ca.LeafFor(fmt.Sprintf("old%d.claude.ai", i)); err != nil {
			t.Fatal(err)
		}
	}
	now = now.Add(leafLifetime + time.Hour)
	if _, err := ca.LeafFor("fresh.claude.ai"); err != nil {
		t.Fatal(err)
	}
	ca.mu.Lock()
	defer ca.mu.Unlock()
	if len(ca.leaves) != 1 {
		t.Fatalf("leaf cache holds %d entries, want only the fresh one", len(ca.leaves))
	}
}

func TestLoadedCAServesOnlyItsCertificate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "mitm")
	first, err := LoadOrCreateCA(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	certPEM, err := os.ReadFile(filepath.Join(dir, caCertFile))
	if err != nil {
		t.Fatal(err)
	}
	keyPEM, err := os.ReadFile(filepath.Join(dir, caKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	bundle := append(append([]byte(nil), certPEM...), keyPEM...) // certificate, then private key
	if err := os.WriteFile(filepath.Join(dir, caCertFile), bundle, 0o644); err != nil {
		t.Fatal(err)
	}
	ca, err := LoadOrCreateCA(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ca.Fingerprint() != first.Fingerprint() {
		t.Fatal("the CA was regenerated instead of loaded")
	}
	if bytes.Contains(ca.CertPEM(), []byte("PRIVATE KEY")) {
		t.Fatal("CertPEM served key material from a bundled ca.pem")
	}
	if !bytes.Equal(ca.CertPEM(), certPEM) {
		t.Fatalf("CertPEM = %q, want the single certificate block", ca.CertPEM())
	}
}
