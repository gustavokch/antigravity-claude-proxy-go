package mitm

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func br(s string) *bufio.Reader { return bufio.NewReader(strings.NewReader(s)) }

func TestReadHeadReturnsExactBytes(t *testing.T) {
	in := "POST /v1/messages?beta=true HTTP/1.1\r\nHost: api.anthropic.com\r\nX-Stainless-OS: MacOS\r\nx-app: cli\r\nContent-Length: 2\r\n\r\nhi"
	reader := br(in)
	head, err := readHead(reader)
	if err != nil {
		t.Fatal(err)
	}
	want := in[:strings.Index(in, "hi")]
	if string(head) != want {
		t.Fatalf("head = %q, want %q", head, want)
	}
	rest, _ := io.ReadAll(reader)
	if string(rest) != "hi" {
		t.Fatalf("body remainder = %q", rest)
	}
}

func TestReadHeadRejectsOversizedHead(t *testing.T) {
	in := "GET / HTTP/1.1\r\nX: " + strings.Repeat("a", maxHeadBytes+10) + "\r\n\r\n"
	if _, err := readHead(br(in)); !errors.Is(err, errHeadTooLarge) {
		t.Fatalf("err = %v, want errHeadTooLarge", err)
	}
}

func TestRequestFraming(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want framing
		n    int64
		err  bool
	}{
		{"none", "GET / HTTP/1.1\r\nHost: a\r\n\r\n", framingNone, 0, false},
		{"length", "POST / HTTP/1.1\r\nContent-Length: 5\r\n\r\n", framingLength, 5, false},
		{"chunked", "POST / HTTP/1.1\r\nTransfer-Encoding: chunked\r\n\r\n", framingChunked, 0, false},
		{"both is smuggling", "POST / HTTP/1.1\r\nContent-Length: 5\r\nTransfer-Encoding: chunked\r\n\r\n", 0, 0, true},
		{"conflicting lengths", "POST / HTTP/1.1\r\nContent-Length: 5\r\nContent-Length: 6\r\n\r\n", 0, 0, true},
		{"repeated same length ok", "POST / HTTP/1.1\r\nContent-Length: 5\r\nContent-Length: 5\r\n\r\n", framingLength, 5, false},
		{"bad length", "POST / HTTP/1.1\r\nContent-Length: x\r\n\r\n", 0, 0, true},
	}
	for _, c := range cases {
		head, err := parseRequestHead([]byte(c.raw))
		if c.err {
			if err == nil {
				t.Errorf("%s: expected error", c.name)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if head.Framing != c.want || head.ContentLength != c.n {
			t.Errorf("%s: framing=%v n=%d", c.name, head.Framing, head.ContentLength)
		}
	}
}

func TestResponseFraming(t *testing.T) {
	head, err := parseResponseHead([]byte("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\n\r\n"), http.MethodGet)
	if err != nil || head.Framing != framingUntilClose {
		t.Fatalf("no length response: %v %v", head, err)
	}
	head, _ = parseResponseHead([]byte("HTTP/1.1 200 OK\r\nContent-Length: 3\r\n\r\n"), http.MethodHead)
	if head.Framing != framingNone {
		t.Fatal("HEAD response must have no body")
	}
	head, _ = parseResponseHead([]byte("HTTP/1.1 204 No Content\r\n\r\n"), http.MethodGet)
	if head.Framing != framingNone {
		t.Fatal("204 must have no body")
	}
}

func TestConnectionFlags(t *testing.T) {
	head, _ := parseRequestHead([]byte("GET / HTTP/1.1\r\nConnection: close\r\n\r\n"))
	if !head.Close {
		t.Error("Connection: close not detected")
	}
	head, _ = parseRequestHead([]byte("GET / HTTP/1.0\r\n\r\n"))
	if !head.Close {
		t.Error("HTTP/1.0 without keep-alive should close")
	}
	head, _ = parseRequestHead([]byte("GET /x HTTP/1.1\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n"))
	if !head.Upgrade {
		t.Error("upgrade not detected")
	}
}

func TestCopyChunkedIsByteExactAndSinkGetsPayload(t *testing.T) {
	wire := "5;ext=1\r\nhello\r\n6\r\n world\r\n0\r\nX-Trailer: t\r\n\r\n"
	var dst, sink bytes.Buffer
	if err := copyBody(&dst, br(wire+"NEXT"), framingChunked, 0, &sink); err != nil {
		t.Fatal(err)
	}
	if dst.String() != wire {
		t.Fatalf("dst = %q, want %q", dst.String(), wire)
	}
	if sink.String() != "hello world" {
		t.Fatalf("sink = %q", sink.String())
	}
}

func TestCopyChunkedTruncated(t *testing.T) {
	var dst bytes.Buffer
	err := copyBody(&dst, br("5\r\nhel"), framingChunked, 0, nil)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v, want unexpected EOF", err)
	}
}

func TestCopyLengthTruncated(t *testing.T) {
	var dst bytes.Buffer
	err := copyBody(&dst, br("abc"), framingLength, 10, nil)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v, want unexpected EOF", err)
	}
}

func TestCapBufferDropsOnOverflow(t *testing.T) {
	c := &capBuffer{limit: 4}
	c.Write([]byte("abc"))
	if string(c.Bytes()) != "abc" {
		t.Fatal("within limit should be kept")
	}
	c.Write([]byte("de"))
	if c.Bytes() != nil {
		t.Fatal("overflow must drop the body entirely")
	}
}
