package zen

import (
	"context"
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestBunSpecFromCapture: the embedded ClientHello must fingerprint cleanly
// into a utls spec with the shape of the genuine capture (17 ciphers,
// 13 extensions).
func TestBunSpecFromCapture(t *testing.T) {
	spec, err := bunHelloSpec()
	if err != nil {
		t.Fatalf("fingerprint captured hello: %v", err)
	}
	if spec == nil {
		t.Fatal("nil spec")
	}
	if len(spec.CipherSuites) != 17 {
		t.Errorf("ciphers = %d, want 17", len(spec.CipherSuites))
	}
	if len(spec.Extensions) != 13 {
		t.Errorf("extensions = %d, want 13", len(spec.Extensions))
	}
}

// TestTLSClientOffByDefault: no opt-in, no utls.
func TestTLSClientOffByDefault(t *testing.T) {
	SetTLSConfig(ZenTLSConfig{})
	t.Cleanup(func() { SetTLSConfig(ZenTLSConfig{}) })

	if TLSClient() != http.DefaultClient {
		t.Error("disabled TLSClient should be http.DefaultClient")
	}
	if GetTLSConfig().Transport() != nil {
		t.Error("disabled Transport should be nil")
	}
	SetTLSConfig(ZenTLSConfig{Enabled: true})
	if TLSClient() == http.DefaultClient {
		t.Error("enabled TLSClient should not be http.DefaultClient")
	}
}

// TestUTLSHelloMatchesCaptureJA3 dials through the utls transport against a
// local listener that sniffs the raw ClientHello, then compares the JA3
// (Wireshark string format) against both the embedded capture and the
// committed baseline. The handshake itself is expected to fail — the sniffer
// never answers — only the bytes on the wire matter.
func TestUTLSHelloMatchesCaptureJA3(t *testing.T) {
	SetTLSConfig(ZenTLSConfig{Enabled: true})
	t.Cleanup(func() { SetTLSConfig(ZenTLSConfig{}) })

	helloCh := make(chan []byte, 1)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		record, err := readTLSRecord(conn)
		if err != nil {
			return
		}
		select {
		case helloCh <- record:
		default:
		}
		// Never answer: the client handshake fails after the hello is out.
	}()

	spec, err := bunHelloSpec()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	// Dial via "localhost" (not the IP) so the SNI assertion is meaningful:
	// utls strips IP literals from SNI.
	_, _ = dialOpencodeTLS(ctx, "tcp", net.JoinHostPort("localhost", port), spec)

	var sent []byte
	select {
	case sent = <-helloCh:
	case <-time.After(10 * time.Second):
		t.Fatal("no ClientHello sniffed")
	}

	want := ja3OfHello(t, opencodeClientHello)
	if want != "1523504b38f0fae0d881d4b6554aac1b" {
		t.Fatalf("capture JA3 = %s, want baseline 1523504b38f0fae0d881d4b6554aac1b", want)
	}
	if got := ja3OfHello(t, sent); got != want {
		t.Errorf("sent JA3 = %s, want capture JA3 %s", got, want)
	}
	if sni := sniOfHello(t, sent); sni != "localhost" {
		t.Errorf("SNI = %q, want dial target host", sni)
	}
	if alpn := alpnOfHello(t, sent); alpn != "http/1.1" {
		t.Errorf("ALPN = %q, want http/1.1", alpn)
	}
}

// readTLSRecord reads one complete TLS record (header + payload).
func readTLSRecord(r io.Reader) ([]byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	length := binary.BigEndian.Uint16(hdr[3:5])
	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, err
	}
	return append(hdr[:], payload...), nil
}

func isGREASE(v uint16) bool {
	return v&0x0f0f == 0x0a0a && v>>8 == uint16(v&0xff)
}

// helloBody returns the handshake body (after record + handshake headers).
func helloBody(t *testing.T, record []byte) []byte {
	t.Helper()
	if len(record) < 9 || record[0] != 0x16 || record[5] != 0x01 {
		t.Fatalf("not a ClientHello record: %x", record[:min(9, len(record))])
	}
	hsLen := int(binary.BigEndian.Uint32([]byte{0, record[6], record[7], record[8]}))
	if len(record) < 9+hsLen {
		t.Fatalf("truncated handshake: have %d, want %d", len(record), 9+hsLen)
	}
	return record[9 : 9+hsLen]
}

// helloFields parses a ClientHello into the JA3 components, stripping GREASE
// the way every JA3 implementation does.
func helloFields(t *testing.T, record []byte) (version uint16, ciphers, exts, groups, formats []uint16) {
	t.Helper()
	p := helloBody(t, record)
	u16 := func() uint16 {
		v := binary.BigEndian.Uint16(p[:2])
		p = p[2:]
		return v
	}
	skip := func(n int) { p = p[n:] }

	version = u16()
	skip(32) // random
	sidLen := int(p[0])
	skip(1 + sidLen)
	csLen := int(u16())
	for i := 0; i < csLen; i += 2 {
		ciphers = append(ciphers, u16())
	}
	compLen := int(p[0])
	skip(1 + compLen)
	remaining := int(u16())
	for remaining > 0 {
		if remaining < 4 {
			t.Fatalf("truncated extension list (%d left)", remaining)
		}
		et := u16()
		el := int(u16())
		body := p[:el]
		skip(el)
		remaining -= 4 + el
		exts = append(exts, et)
		switch et {
		case 10: // supported_groups
			n := int(binary.BigEndian.Uint16(body[:2]))
			for i := 0; i < n; i += 2 {
				groups = append(groups, binary.BigEndian.Uint16(body[2+i:4+i]))
			}
		case 11: // ec_point_formats
			n := int(body[0])
			for i := 0; i < n; i++ {
				formats = append(formats, uint16(body[1+i]))
			}
		}
	}
	return version, ciphers, exts, groups, formats
}

// ja3OfHello computes the Wireshark JA3 hash: values inside a group are
// hyphen separated, groups comma separated, then md5.
func ja3OfHello(t *testing.T, record []byte) string {
	t.Helper()
	version, ciphers, exts, groups, formats := helloFields(t, record)
	join := func(vs []uint16) string {
		parts := make([]string, 0, len(vs))
		for _, v := range vs {
			if isGREASE(v) {
				continue
			}
			parts = append(parts, fmt.Sprintf("%d", v))
		}
		return strings.Join(parts, "-")
	}
	full := fmt.Sprintf("%d,%s,%s,%s,%s", version, join(ciphers), join(exts), join(groups), join(formats))
	sum := md5.Sum([]byte(full))
	return hex.EncodeToString(sum[:])
}

// extensionBody finds one extension's body in a ClientHello record.
func extensionBody(t *testing.T, record []byte, want uint16) []byte {
	t.Helper()
	p := helloBody(t, record)
	u16 := func() uint16 { v := binary.BigEndian.Uint16(p[:2]); p = p[2:]; return v }
	skip := func(n int) { p = p[n:] }
	u16() // version
	skip(32)
	sidLen := int(p[0])
	skip(1 + sidLen)
	csLen := int(u16())
	skip(csLen)
	compLen := int(p[0])
	skip(1 + compLen)
	remaining := int(u16())
	for remaining > 0 {
		et := u16()
		el := int(u16())
		body := p[:el]
		skip(el)
		remaining -= 4 + el
		if et == want {
			return body
		}
	}
	return nil
}

func sniOfHello(t *testing.T, record []byte) string {
	t.Helper()
	body := extensionBody(t, record, 0)
	if len(body) < 5 {
		return ""
	}
	nameLen := int(binary.BigEndian.Uint16(body[3:5]))
	if len(body) < 5+nameLen {
		return ""
	}
	return string(body[5 : 5+nameLen])
}

func alpnOfHello(t *testing.T, record []byte) string {
	t.Helper()
	body := extensionBody(t, record, 16)
	if len(body) < 3 {
		return ""
	}
	return string(body[3 : 3+int(body[2])])
}
