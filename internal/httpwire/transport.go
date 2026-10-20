// Package httpwire is an HTTP/1.1 client transport that writes a request the way
// one particular client stack does.
//
// net/http cannot. It writes Host and User-Agent first, sorts the remaining
// headers, injects its own Accept-Encoding, and gives a caller no way to change
// any of it, so the byte layout of a request is the same whoever sends it. Where
// an upstream tells clients apart by that layout (Claude Code's two stacks write
// their headers in different orders and with different tails), this transport
// writes the layout of the client being imitated.
//
// It is deliberately small: HTTP/1.1 only, no redirects (http.Client follows
// them), no 1xx handling, no request streaming. A request body is buffered so a
// request that fails on a connection the server had closed can be sent again.
package httpwire

import (
	"bufio"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
	"golang.org/x/net/http/httpguts"
)

// Header names the transport writes itself. A stack lists them in its Tail in
// the order it writes them, and any caller-supplied header of the same name is
// dropped: the transport owns the framing, the host and the encoding.
const (
	Connection     = "Connection"
	Host           = "Host"
	AcceptEncoding = "Accept-Encoding"
	ContentLength  = "Content-Length"
)

// owned are the names a caller cannot set, compared case-insensitively.
var owned = map[string]bool{
	"connection": true, "host": true, "accept-encoding": true, "content-length": true,
	"transfer-encoding": true, "keep-alive": true, "proxy-connection": true,
	"te": true, "trailer": true, "upgrade": true,
}

// Stack describes how one client stack lays out an HTTP/1.1 request.
type Stack struct {
	// Order sorts the caller's header names, in place, into the order the stack
	// writes them. They are written before Tail.
	Order func(names []string)
	// Tail lists the headers the stack's HTTP layer writes after the caller's,
	// in order: any of Connection, Host, AcceptEncoding and ContentLength. A
	// ContentLength entry is written only for a request with a body (or a
	// method that carries one).
	Tail []string
	// AcceptEncoding is the Accept-Encoding value. The transport decodes what the
	// server answers with: gzip, deflate, br and zstd.
	AcceptEncoding string
	// KeepAlive selects "Connection: keep-alive" and connection reuse; false
	// selects "Connection: close" and one connection per request.
	KeepAlive bool
}

// Transport is an http.RoundTripper for one Stack.
type Transport struct {
	Stack Stack

	// DialTLS opens a TLS connection for an https URL. Required for https.
	DialTLS func(ctx context.Context, network, addr string) (net.Conn, error)
	// Dial opens a plain connection for an http URL. Nil uses a net.Dialer.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// TLSClientConfig is for the DialTLS function to read; the transport itself
	// does not use it.
	TLSClientConfig *tls.Config

	// ResponseHeaderTimeout bounds the wait for the response headers after the
	// request is written. Zero means no bound.
	ResponseHeaderTimeout time.Duration
	// IdleConnTimeout closes an idle connection after this long. Zero keeps it
	// until the server closes it.
	IdleConnTimeout time.Duration
	// MaxIdleConnsPerHost bounds the idle connections kept per host. Zero
	// means 2, as net/http's default does.
	MaxIdleConnsPerHost int

	mu   sync.Mutex
	idle map[string][]*conn
}

// conn is one connection and the reader that owns its buffered input.
type conn struct {
	net.Conn
	br    *bufio.Reader
	key   string
	timer *time.Timer // closes the connection when it has idled too long
}

// RoundTrip implements http.RoundTripper.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL == nil {
		return nil, errors.New("httpwire: nil request URL")
	}
	if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
		return nil, fmt.Errorf("httpwire: unsupported scheme %q", req.URL.Scheme)
	}
	if req.URL.Host == "" {
		return nil, errors.New("httpwire: request URL has no host")
	}
	body, err := readBody(req)
	if err != nil {
		return nil, err
	}
	if err := t.validate(req); err != nil {
		return nil, err
	}

	addr := canonicalAddr(req.URL)
	key := req.URL.Scheme + "://" + addr
	ctx := req.Context()
	for attempt := 0; ; attempt++ {
		c, reused, err := t.conn(ctx, req.URL.Scheme, addr, key)
		if err != nil {
			return nil, err
		}
		resp, err := t.roundTrip(ctx, c, req, body)
		if err == nil {
			return resp, nil
		}
		_ = c.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// A reused connection may have been closed by the server while it idled.
		// That surfaces as a failed write or a response that never started, and
		// the request has not been processed, so it is sent once more on a new
		// connection.
		if reused && attempt == 0 && isStale(err) {
			continue
		}
		return nil, err
	}
}

// readBody buffers and closes req.Body.
func readBody(req *http.Request) ([]byte, error) {
	if req.Body == nil || req.Body == http.NoBody {
		return nil, nil
	}
	defer req.Body.Close()
	return io.ReadAll(req.Body)
}

func (t *Transport) validate(req *http.Request) error {
	for name, values := range req.Header {
		if owned[strings.ToLower(name)] {
			continue
		}
		if !httpguts.ValidHeaderFieldName(name) {
			return fmt.Errorf("httpwire: invalid header name %q", name)
		}
		for _, value := range values {
			if !httpguts.ValidHeaderFieldValue(value) {
				return fmt.Errorf("httpwire: invalid value for header %q", name)
			}
		}
	}
	return nil
}

// canonicalAddr returns host:port with the scheme's default port filled in.
func canonicalAddr(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[u.Scheme]
	}
	return net.JoinHostPort(u.Hostname(), port)
}

func (t *Transport) conn(ctx context.Context, scheme, addr, key string) (*conn, bool, error) {
	if t.Stack.KeepAlive {
		if c := t.takeIdle(key); c != nil {
			return c, true, nil
		}
	}
	dial := t.Dial
	if scheme == "https" {
		dial = t.DialTLS
		if dial == nil {
			return nil, false, errors.New("httpwire: https request but no DialTLS")
		}
	}
	if dial == nil {
		dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
		dial = dialer.DialContext
	}
	raw, err := dial(ctx, "tcp", addr)
	if err != nil {
		return nil, false, err
	}
	return &conn{Conn: raw, br: bufio.NewReader(raw), key: key}, false, nil
}

func (t *Transport) takeIdle(key string) *conn {
	t.mu.Lock()
	defer t.mu.Unlock()
	list := t.idle[key]
	for len(list) > 0 {
		c := list[len(list)-1]
		list = list[:len(list)-1]
		// A stopped timer that had already fired has closed the connection.
		if c.timer == nil || c.timer.Stop() {
			t.idle[key] = list
			return c
		}
	}
	delete(t.idle, key)
	return nil
}

func (t *Transport) putIdle(c *conn) {
	limit := t.MaxIdleConnsPerHost
	if limit <= 0 {
		limit = 2
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.idle == nil {
		t.idle = make(map[string][]*conn)
	}
	if len(t.idle[c.key]) >= limit {
		_ = c.Close()
		return
	}
	if t.IdleConnTimeout > 0 {
		c.timer = time.AfterFunc(t.IdleConnTimeout, func() {
			t.removeIdle(c)
			_ = c.Close()
		})
	}
	t.idle[c.key] = append(t.idle[c.key], c)
}

func (t *Transport) removeIdle(c *conn) {
	t.mu.Lock()
	defer t.mu.Unlock()
	list := t.idle[c.key]
	for i, other := range list {
		if other == c {
			t.idle[c.key] = append(list[:i:i], list[i+1:]...)
			return
		}
	}
}

// CloseIdleConnections closes every idle connection. http.Client calls it.
func (t *Transport) CloseIdleConnections() {
	t.mu.Lock()
	idle := t.idle
	t.idle = nil
	t.mu.Unlock()
	for _, list := range idle {
		for _, c := range list {
			if c.timer != nil {
				c.timer.Stop()
			}
			_ = c.Close()
		}
	}
}

func isStale(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, net.ErrClosed)
}

func (t *Transport) roundTrip(ctx context.Context, c *conn, req *http.Request, body []byte) (*http.Response, error) {
	// Cancelling the request closes the connection, which fails a blocked read
	// or write and, for a response whose body is still streaming, ends the body.
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })

	if err := t.write(c, req, body); err != nil {
		stop()
		return nil, err
	}
	if t.ResponseHeaderTimeout > 0 {
		_ = c.SetReadDeadline(time.Now().Add(t.ResponseHeaderTimeout))
	}
	resp, err := http.ReadResponse(c.br, req)
	if err != nil {
		stop()
		return nil, err
	}
	_ = c.SetReadDeadline(time.Time{})

	reusable := t.Stack.KeepAlive && !resp.Close && !req.Close
	if resp.Body == http.NoBody {
		if stop() && reusable {
			t.putIdle(c)
		} else {
			_ = c.Close()
		}
		return resp, nil
	}

	b := &respBody{t: t, c: c, stop: stop, src: resp.Body, reusable: reusable}
	b.dec = b.decoder(resp)
	resp.Body = b
	return resp, nil
}

// write sends the request line, the caller's headers in the stack's order, the
// stack's tail, and the body.
func (t *Transport) write(c *conn, req *http.Request, body []byte) error {
	w := bufio.NewWriterSize(c, 4096)
	target := req.URL.RequestURI()
	method := req.Method
	if method == "" {
		method = http.MethodGet
	}
	fmt.Fprintf(w, "%s %s HTTP/1.1\r\n", method, target)

	names := make([]string, 0, len(req.Header))
	for name := range req.Header {
		if !owned[strings.ToLower(name)] {
			names = append(names, name)
		}
	}
	if t.Stack.Order != nil {
		t.Stack.Order(names)
	}
	for _, name := range names {
		for _, value := range req.Header[name] {
			fmt.Fprintf(w, "%s: %s\r\n", name, value)
		}
	}

	hasBody := len(body) > 0 || method == http.MethodPost || method == http.MethodPut || method == http.MethodPatch
	for _, name := range t.Stack.Tail {
		switch name {
		case Connection:
			if t.Stack.KeepAlive && !req.Close {
				w.WriteString("Connection: keep-alive\r\n")
			} else {
				w.WriteString("Connection: close\r\n")
			}
		case Host:
			host := req.Host
			if host == "" {
				host = req.URL.Host
			}
			fmt.Fprintf(w, "Host: %s\r\n", stripDefaultPort(host, req.URL.Scheme))
		case AcceptEncoding:
			if t.Stack.AcceptEncoding != "" {
				fmt.Fprintf(w, "Accept-Encoding: %s\r\n", t.Stack.AcceptEncoding)
			}
		case ContentLength:
			if hasBody {
				fmt.Fprintf(w, "Content-Length: %d\r\n", len(body))
			}
		}
	}
	w.WriteString("\r\n")
	w.Write(body)
	return w.Flush()
}

// stripDefaultPort drops :443 or :80 where it is the scheme's default, as the
// real clients do.
func stripDefaultPort(host, scheme string) string {
	h, port, err := net.SplitHostPort(host)
	if err != nil {
		return host
	}
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		if strings.Contains(h, ":") {
			return "[" + h + "]"
		}
		return h
	}
	return host
}

// respBody hands a response's body to the caller, decodes its Content-Encoding,
// and returns the connection to the pool once the body has been read to the end.
type respBody struct {
	t        *Transport
	c        *conn
	stop     func() bool
	src      io.ReadCloser // the framed body, from http.ReadResponse
	dec      io.Reader     // src, decoded
	closeDec func()        // guarded by mu: the decoder opens on a read, Close runs anywhere
	mu       sync.Mutex
	reusable bool
	once     sync.Once
}

func (b *respBody) Read(p []byte) (int, error) {
	n, err := b.dec.Read(p)
	switch {
	case err == io.EOF:
		b.finish(true)
	case err != nil:
		b.finish(false)
	}
	return n, err
}

func (b *respBody) Close() error {
	b.finish(false)
	return nil
}

func (b *respBody) setCloser(fn func()) {
	b.mu.Lock()
	b.closeDec = fn
	b.mu.Unlock()
}

// finish releases the connection. complete means the caller read the body to its
// end, which is the only case where the connection is safe to reuse.
func (b *respBody) finish(complete bool) {
	b.once.Do(func() {
		b.mu.Lock()
		closeDec := b.closeDec
		b.mu.Unlock()
		if closeDec != nil {
			closeDec()
		}
		if complete {
			// A decoder reaches its own end before the framing does: the last
			// chunk marker of a chunked body is still unread.
			_, err := io.Copy(io.Discard, b.src)
			complete = err == nil
		}
		_ = b.src.Close()
		if complete && b.reusable && b.stop() {
			b.t.putIdle(b.c)
			return
		}
		b.stop()
		_ = b.c.Close()
	})
}

// decoder wraps src for the response's Content-Encoding and edits the response to
// say so, as net/http does when it decompresses. An encoding it does not know is
// left alone for the caller.
func (b *respBody) decoder(resp *http.Response) io.Reader {
	if b.t.Stack.AcceptEncoding == "" {
		return b.src
	}
	encoding := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	var open func(io.Reader) (io.Reader, func(), error)
	switch encoding {
	case "gzip", "x-gzip":
		open = func(r io.Reader) (io.Reader, func(), error) {
			g, err := gzip.NewReader(r)
			if err != nil {
				return nil, nil, err
			}
			return g, func() { _ = g.Close() }, nil
		}
	case "deflate":
		open = openDeflate
	case "br":
		open = func(r io.Reader) (io.Reader, func(), error) { return brotli.NewReader(r), nil, nil }
	case "zstd":
		open = func(r io.Reader) (io.Reader, func(), error) {
			z, err := zstd.NewReader(r, zstd.WithDecoderConcurrency(1))
			if err != nil {
				return nil, nil, err
			}
			return z, z.Close, nil
		}
	default:
		return b.src
	}
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("Content-Length")
	resp.ContentLength = -1
	resp.Uncompressed = true
	return &lazyDecoder{src: b.src, open: open, body: b}
}

// openDeflate reads HTTP's "deflate", which is zlib-wrapped in the standard and
// raw in a number of servers; the zlib header is recognisable.
func openDeflate(r io.Reader) (io.Reader, func(), error) {
	br := bufio.NewReader(r)
	head, err := br.Peek(2)
	if err != nil {
		return nil, nil, err
	}
	if head[0]&0x0f == 8 && (uint16(head[0])<<8|uint16(head[1]))%31 == 0 {
		z, err := zlib.NewReader(br)
		if err != nil {
			return nil, nil, err
		}
		return z, func() { _ = z.Close() }, nil
	}
	f := flate.NewReader(br)
	return f, func() { _ = f.Close() }, nil
}

// lazyDecoder opens the decoder on the first read. gzip.NewReader and friends
// read a header at construction, which would block RoundTrip on the body.
type lazyDecoder struct {
	src  io.Reader
	open func(io.Reader) (io.Reader, func(), error)
	body *respBody
	r    io.Reader
	err  error
}

func (d *lazyDecoder) Read(p []byte) (int, error) {
	if d.err != nil {
		return 0, d.err
	}
	if d.r == nil {
		r, closeFn, err := d.open(d.src)
		if err != nil {
			// An empty body under a Content-Encoding is an empty body.
			if errors.Is(err, io.EOF) {
				d.err = io.EOF
			} else {
				d.err = err
			}
			return 0, d.err
		}
		d.r = r
		d.body.setCloser(closeFn)
	}
	n, err := d.r.Read(p)
	if err != nil {
		d.err = err
	}
	return n, err
}
