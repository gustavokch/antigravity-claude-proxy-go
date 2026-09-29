package mitm

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Options configures a Server.
type Options struct {
	CA       *CA
	Registry *Registry
	Logger   *slog.Logger
	// Dial opens outbound connections (tests redirect it). Nil uses net.Dialer.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	// UpstreamTLS is for tests only (custom roots). Production leaves it nil,
	// which means an empty tls.Config{}: the standard Go client handshake.
	UpstreamTLS *tls.Config
	// IdleTimeout bounds a keep-alive connection waiting for the next request.
	IdleTimeout time.Duration
	Now         func() time.Time
}

// Stats are process-lifetime counters.
type Stats struct {
	Terminated        int64 `json:"terminated"`
	Tunnelled         int64 `json:"tunnelled"`
	HandshakeFailures int64 `json:"handshakeFailures"`
	UpstreamErrors    int64 `json:"upstreamErrors"`
	Requests          int64 `json:"requests"`
	Observed          int64 `json:"observed"`
}

// Server is the CONNECT listener.
type Server struct {
	opts   Options
	logger *slog.Logger

	mu       sync.Mutex
	listener net.Listener
	conns    map[net.Conn]struct{}
	closing  bool
	wg       sync.WaitGroup

	terminated, tunnelled, handshakeFailures, upstreamErrors, requests, observed atomic.Int64
	warnedHosts                                                                  sync.Map
}

// New validates options and returns a Server.
func New(opts Options) (*Server, error) {
	if opts.CA == nil {
		return nil, errors.New("mitm: CA is required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.IdleTimeout <= 0 {
		opts.IdleTimeout = 2 * time.Minute
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Dial == nil {
		dialer := &net.Dialer{Timeout: 10 * time.Second}
		opts.Dial = dialer.DialContext
	}
	return &Server{opts: opts, logger: opts.Logger, conns: map[net.Conn]struct{}{}}, nil
}

// Stats returns a snapshot of the counters.
func (s *Server) Stats() Stats {
	return Stats{
		Terminated: s.terminated.Load(), Tunnelled: s.tunnelled.Load(),
		HandshakeFailures: s.handshakeFailures.Load(), UpstreamErrors: s.upstreamErrors.Load(),
		Requests: s.requests.Load(), Observed: s.observed.Load(),
	}
}

// Serve accepts connections on l until it is closed or Shutdown is called.
func (s *Server) Serve(l net.Listener) error {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return http.ErrServerClosed
	}
	s.listener = l
	s.mu.Unlock()
	var delay time.Duration
	for {
		conn, err := l.Accept()
		if err != nil {
			s.mu.Lock()
			closing := s.closing
			s.mu.Unlock()
			if closing {
				return http.ErrServerClosed
			}
			if retryableAccept(err) {
				delay = min(max(2*delay, 5*time.Millisecond), time.Second)
				s.logger.Warn("mitm: accept failed; retrying", "error", err, "delay", delay)
				time.Sleep(delay)
				continue
			}
			return err
		}
		delay = 0
		if !s.track(conn) {
			conn.Close()
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer s.untrack(conn)
			s.handleConn(conn)
		}()
	}
}

// retryableAccept reports whether an Accept error is transient: a timeout, or
// a temporary condition such as running out of file descriptors.
func retryableAccept(err error) bool {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	var temp interface{ Temporary() bool }
	return errors.As(err, &temp) && temp.Temporary()
}

// Shutdown stops accepting, closes every live connection and waits for the
// handlers to return or ctx to end.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.closing = true
	if s.listener != nil {
		s.listener.Close()
	}
	for conn := range s.conns {
		conn.Close()
	}
	s.mu.Unlock()
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Server) track(conn net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return false
	}
	s.conns[conn] = struct{}{}
	return true
}

func (s *Server) untrack(conn net.Conn) {
	conn.Close()
	s.mu.Lock()
	delete(s.conns, conn)
	s.mu.Unlock()
}

// bufferedConn serves reads from a bufio.Reader that already holds bytes the
// client sent right behind the CONNECT head.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

func (s *Server) handleConn(conn net.Conn) {
	br := bufio.NewReaderSize(conn, 32<<10)
	conn.SetReadDeadline(s.opts.Now().Add(10 * time.Second))
	raw, err := readHead(br)
	if err != nil {
		return
	}
	req, err := parseRequestHead(raw)
	if err != nil {
		writeError(conn, http.StatusBadRequest, "malformed request")
		return
	}
	if req.Method != http.MethodConnect {
		conn.Write([]byte("HTTP/1.1 405 Method Not Allowed\r\nAllow: CONNECT\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"))
		return
	}
	host, port, err := net.SplitHostPort(req.Target)
	if err != nil {
		writeError(conn, http.StatusBadRequest, "CONNECT target must be host:port")
		return
	}
	conn.SetReadDeadline(time.Time{})
	if port == "443" && HostPermitted(host) {
		s.terminate(conn, br, strings.ToLower(host))
		return
	}
	s.tunnel(conn, br, host, port)
}

// tunnel is a blind byte relay: the proxy sees the destination and nothing else.
func (s *Server) tunnel(conn net.Conn, br *bufio.Reader, host, port string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	up, err := s.opts.Dial(ctx, "tcp", net.JoinHostPort(host, port))
	if err != nil {
		s.upstreamErrors.Add(1)
		writeError(conn, http.StatusBadGateway, "cannot reach "+host)
		return
	}
	defer up.Close()
	if _, err := conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}
	s.tunnelled.Add(1)
	splice(conn, br, up, bufio.NewReader(up))
}

// splice copies both directions until both end. Each direction half-closes
// its destination when its source finishes.
func splice(a net.Conn, aBuf *bufio.Reader, b net.Conn, bBuf *bufio.Reader) {
	var wg sync.WaitGroup
	wg.Add(2)
	pipe := func(dst net.Conn, src *bufio.Reader) {
		defer wg.Done()
		io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			cw.CloseWrite()
		} else {
			dst.Close()
		}
	}
	go pipe(b, aBuf)
	go pipe(a, bBuf)
	wg.Wait()
}

func (s *Server) terminate(conn net.Conn, br *bufio.Reader, host string) {
	if _, err := conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}
	// Server side only: this config faces the local CLI. The upstream
	// handshake toward Anthropic uses a separate, empty tls.Config.
	tlsConn := tls.Server(&bufferedConn{Conn: conn, r: br}, &tls.Config{
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return s.opts.CA.LeafFor(host) },
		NextProtos:     []string{"http/1.1"},
		MinVersion:     tls.VersionTLS12,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	err := tlsConn.HandshakeContext(ctx)
	cancel()
	if err != nil {
		s.handshakeFailures.Add(1)
		if _, seen := s.warnedHosts.LoadOrStore(host, true); !seen {
			s.logger.Warn("mitm: client TLS handshake failed; the CLI probably does not trust the proxy CA (set NODE_EXTRA_CA_CERTS)", "host", host, "error", err)
		}
		return
	}
	s.terminated.Add(1)
	s.relay(tlsConn, host)
}

type upstream struct {
	conn net.Conn
	br   *bufio.Reader
}

// alive reports whether the upstream connection is still open and idle. A
// pending read timeout is the healthy state; EOF, an error, or unexpected
// buffered data all mean the connection cannot be reused.
func (u *upstream) alive() bool {
	u.conn.SetReadDeadline(time.Now().Add(time.Millisecond))
	_, err := u.br.Peek(1)
	u.conn.SetReadDeadline(time.Time{})
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func (s *Server) dialUpstream(host string) (*upstream, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	raw, err := s.opts.Dial(ctx, "tcp", net.JoinHostPort(host, "443"))
	if err != nil {
		return nil, err
	}
	cfg := &tls.Config{}
	if s.opts.UpstreamTLS != nil {
		cfg = s.opts.UpstreamTLS.Clone()
	}
	if cfg.ServerName == "" {
		cfg.ServerName = host
	}
	tc := tls.Client(raw, cfg)
	if err := tc.HandshakeContext(ctx); err != nil {
		raw.Close()
		return nil, err
	}
	return &upstream{conn: tc, br: bufio.NewReaderSize(tc, 32<<10)}, nil
}

// relay serves one terminated client connection: an HTTP/1.1 keep-alive loop
// that forwards each message byte for byte.
func (s *Server) relay(client net.Conn, host string) {
	cbr := bufio.NewReaderSize(client, 32<<10)
	var up *upstream
	defer func() {
		if up != nil {
			up.conn.Close()
		}
	}()
	for {
		client.SetReadDeadline(s.opts.Now().Add(s.opts.IdleTimeout))
		raw, err := readHead(cbr)
		if err != nil {
			if errors.Is(err, errHeadTooLarge) {
				writeError(client, http.StatusRequestHeaderFieldsTooLarge, "request head too large")
			}
			return
		}
		client.SetReadDeadline(time.Time{})
		req, err := parseRequestHead(raw)
		if err != nil {
			writeError(client, http.StatusBadRequest, "malformed or ambiguous request")
			return
		}
		if req.Expect100 {
			writeError(client, http.StatusExpectationFailed, "Expect: 100-continue is not supported by the proxy")
			return
		}
		if up != nil && !up.alive() {
			up.conn.Close()
			up = nil
		}
		if up == nil {
			if up, err = s.dialUpstream(host); err != nil {
				s.upstreamErrors.Add(1)
				s.logger.Warn("mitm: upstream dial failed", "host", host, "error", err)
				writeError(client, http.StatusBadGateway, "cannot reach "+host)
				return
			}
		}
		s.requests.Add(1)
		if keep := s.exchange(client, cbr, up, req); !keep {
			return
		}
	}
}

// exchange forwards one request and its response. It returns true when both
// connections can carry another request.
func (s *Server) exchange(client net.Conn, cbr *bufio.Reader, up *upstream, req *msgHead) bool {
	route, rawID := classifyRoute(req.Method, req.Target)
	s.logger.Debug("mitm: request", "method", req.Method, "route", route)

	if _, err := up.conn.Write(req.Raw); err != nil {
		s.upstreamFailed(client, err)
		return false
	}
	if err := copyBody(up.conn, cbr, req.Framing, req.ContentLength, nil); err != nil {
		s.upstreamFailed(client, err)
		return false
	}

	var resp *msgHead
	for {
		rawResp, err := readHead(up.br)
		if err != nil {
			s.upstreamFailed(client, err)
			return false
		}
		if resp, err = parseResponseHead(rawResp, req.Method); err != nil {
			s.upstreamFailed(client, err)
			return false
		}
		if resp.Status/100 == 1 && resp.Status != http.StatusSwitchingProtocols {
			if _, err := client.Write(resp.Raw); err != nil {
				return false
			}
			continue
		}
		break
	}
	if _, err := client.Write(resp.Raw); err != nil {
		return false
	}
	if resp.Status == http.StatusSwitchingProtocols {
		s.observe(Observation{Route: route, RawID: rawID, Status: resp.Status})
		splice(client, cbr, up.conn, up.br)
		return false
	}

	var sink io.Writer
	var captured *capBuffer
	if parsesBody(route) && resp.Status/100 == 2 && strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "json") {
		captured = &capBuffer{limit: maxObservedBody}
		sink = captured
	}
	if err := copyBody(client, up.br, resp.Framing, resp.ContentLength, sink); err != nil {
		s.logger.Debug("mitm: response copy ended", "error", err)
		return false
	}
	if route != "" {
		obs := Observation{Route: route, RawID: rawID, Status: resp.Status}
		if captured != nil && captured.Bytes() != nil {
			id, fields := summarizeBody(captured.Bytes(), resp.Header.Get("Content-Encoding"))
			if obs.RawID == "" {
				obs.RawID = id
			}
			obs.Fields = fields
		}
		s.observe(obs)
	}
	return !req.Close && !resp.Close && resp.Framing != framingUntilClose
}

func (s *Server) upstreamFailed(client net.Conn, err error) {
	s.upstreamErrors.Add(1)
	s.logger.Warn("mitm: upstream exchange failed", "error", err)
	writeError(client, http.StatusBadGateway, "upstream error")
}

// observe reports to the registry. Any panic is swallowed: observation must
// never affect forwarding.
func (s *Server) observe(o Observation) {
	if s.opts.Registry == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			s.logger.Warn("mitm: observer panic recovered", "panic", fmt.Sprint(r))
		}
	}()
	s.opts.Registry.Observe(o)
	s.observed.Add(1)
}

func writeError(conn net.Conn, status int, message string) {
	body := fmt.Sprintf(`{"type":"error","error":{"type":"api_error","message":%q}}`, message)
	fmt.Fprintf(conn, "HTTP/1.1 %d %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		status, http.StatusText(status), len(body), body)
}
