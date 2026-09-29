package mitm

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
)

// The relay forwards HTTP/1.1 messages byte for byte. Go's net/http cannot do
// that: it canonicalizes header names (X-Stainless-OS becomes X-Stainless-Os)
// and writes headers in sorted order. So heads are read and forwarded raw, and
// parsed only to learn message framing.

const (
	maxHeadBytes = 64 << 10
	maxLineBytes = 8 << 10
)

var (
	errHeadTooLarge = errors.New("mitm: message head exceeds 64 KiB")
	errBadFraming   = errors.New("mitm: ambiguous or invalid message framing")
)

type framing int

const (
	framingNone framing = iota
	framingLength
	framingChunked
	framingUntilClose
)

// msgHead is a parsed message head. Raw is what arrived on the wire and is
// what gets forwarded; the other fields are read-only views of it.
type msgHead struct {
	Raw           []byte
	Header        http.Header // canonicalized copy, for lookups only
	Method        string
	Target        string
	Status        int
	Proto         string
	Framing       framing
	ContentLength int64
	Close         bool
	Upgrade       bool
	Expect100     bool
}

// readHead reads one message head up to and including the blank line,
// returning the exact bytes read. Leading empty lines are skipped.
func readHead(br *bufio.Reader) ([]byte, error) {
	var head []byte
	for {
		line, err := br.ReadSlice('\n')
		head = append(head, line...)
		if len(head) > maxHeadBytes {
			return nil, errHeadTooLarge
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil {
			if err == io.EOF && len(head) > 0 {
				return nil, io.ErrUnexpectedEOF
			}
			return nil, err
		}
		if isBlank(line) {
			if len(head) == len(line) {
				head = head[:0]
				continue
			}
			return head, nil
		}
	}
}

func isBlank(line []byte) bool {
	return bytes.Equal(line, []byte("\r\n")) || bytes.Equal(line, []byte("\n"))
}

func splitHead(raw []byte) (start string, header http.Header, err error) {
	tp := textproto.NewReader(bufio.NewReader(bytes.NewReader(raw)))
	start, err = tp.ReadLine()
	if err != nil {
		return "", nil, err
	}
	mime, err := tp.ReadMIMEHeader()
	if err != nil {
		return "", nil, err
	}
	return start, http.Header(mime), nil
}

func parseRequestHead(raw []byte) (*msgHead, error) {
	start, header, err := splitHead(raw)
	if err != nil {
		return nil, err
	}
	parts := strings.SplitN(start, " ", 3)
	if len(parts) != 3 || !strings.HasPrefix(parts[2], "HTTP/1.") {
		return nil, fmt.Errorf("mitm: malformed request line %q", start)
	}
	head := &msgHead{Raw: raw, Header: header, Method: parts[0], Target: parts[1], Proto: parts[2]}
	head.Framing, head.ContentLength, err = bodyFraming(header, false, head.Method, 0)
	if err != nil {
		return nil, err
	}
	head.Close, head.Upgrade = connectionFlags(header, head.Proto)
	head.Expect100 = strings.EqualFold(header.Get("Expect"), "100-continue")
	return head, nil
}

func parseResponseHead(raw []byte, requestMethod string) (*msgHead, error) {
	start, header, err := splitHead(raw)
	if err != nil {
		return nil, err
	}
	parts := strings.SplitN(start, " ", 3)
	if len(parts) < 2 || !strings.HasPrefix(parts[0], "HTTP/1.") {
		return nil, fmt.Errorf("mitm: malformed status line %q", start)
	}
	status, err := strconv.Atoi(parts[1])
	if err != nil {
		return nil, fmt.Errorf("mitm: malformed status code %q", parts[1])
	}
	head := &msgHead{Raw: raw, Header: header, Method: requestMethod, Status: status, Proto: parts[0]}
	head.Framing, head.ContentLength, err = bodyFraming(header, true, requestMethod, status)
	if err != nil {
		return nil, err
	}
	head.Close, head.Upgrade = connectionFlags(header, head.Proto)
	return head, nil
}

// connectionFlags reports whether the sender asked to close the connection
// after this message, and whether it asked to upgrade the protocol.
func connectionFlags(h http.Header, proto string) (closeAfter, upgrade bool) {
	tokens := tokenSet(h["Connection"])
	closeAfter = tokens["close"] || (proto == "HTTP/1.0" && !tokens["keep-alive"])
	upgrade = tokens["upgrade"] && h.Get("Upgrade") != ""
	return closeAfter, upgrade
}

func tokenSet(values []string) map[string]bool {
	set := map[string]bool{}
	for _, value := range values {
		for _, token := range strings.Split(value, ",") {
			set[strings.ToLower(strings.TrimSpace(token))] = true
		}
	}
	return set
}

// bodyFraming decides how the body is delimited. Requests with both
// Transfer-Encoding and Content-Length, or conflicting Content-Length values,
// are rejected: that is the request-smuggling shape.
func bodyFraming(h http.Header, isResponse bool, method string, status int) (framing, int64, error) {
	if isResponse && (method == http.MethodHead || status/100 == 1 || status == 204 || status == 304) {
		return framingNone, 0, nil
	}
	te := strings.ToLower(strings.Join(h["Transfer-Encoding"], ","))
	lengths := h["Content-Length"]
	if te != "" {
		if !isResponse && len(lengths) > 0 {
			return 0, 0, errBadFraming
		}
		if strings.HasSuffix(strings.TrimSpace(te), "chunked") {
			return framingChunked, 0, nil
		}
		if !isResponse {
			return 0, 0, errBadFraming
		}
		return framingUntilClose, 0, nil
	}
	if len(lengths) > 0 {
		var length int64 = -1
		for _, value := range lengths {
			for _, part := range strings.Split(value, ",") {
				n, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
				if err != nil || n < 0 || (length >= 0 && n != length) {
					return 0, 0, errBadFraming
				}
				length = n
			}
		}
		if length == 0 {
			return framingNone, 0, nil
		}
		return framingLength, length, nil
	}
	if isResponse {
		return framingUntilClose, 0, nil
	}
	return framingNone, 0, nil
}

// copyBody copies one message body from br to dst as it arrived, including
// chunk framing. sink, when non-nil, also receives the decoded payload.
func copyBody(dst io.Writer, br *bufio.Reader, f framing, n int64, sink io.Writer) error {
	switch f {
	case framingNone:
		return nil
	case framingLength:
		return copyN(dst, br, n, sink)
	case framingChunked:
		return copyChunked(dst, br, sink)
	case framingUntilClose:
		w := dst
		if sink != nil {
			w = io.MultiWriter(dst, sink)
		}
		_, err := io.Copy(w, br)
		return err
	}
	return nil
}

func copyN(dst io.Writer, br *bufio.Reader, n int64, sink io.Writer) error {
	w := dst
	if sink != nil {
		w = io.MultiWriter(dst, sink)
	}
	if _, err := io.CopyN(w, br, n); err != nil {
		if err == io.EOF {
			return io.ErrUnexpectedEOF
		}
		return err
	}
	return nil
}

func copyChunked(dst io.Writer, br *bufio.Reader, sink io.Writer) error {
	for {
		line, err := readLine(br)
		if err != nil {
			return err
		}
		if _, err := dst.Write(line); err != nil {
			return err
		}
		size, err := parseChunkSize(line)
		if err != nil {
			return err
		}
		if size == 0 {
			for { // optional trailers, then the blank line
				trailer, err := readLine(br)
				if err != nil {
					return err
				}
				if _, err := dst.Write(trailer); err != nil {
					return err
				}
				if isBlank(trailer) {
					return nil
				}
			}
		}
		if err := copyN(dst, br, size, sink); err != nil {
			return err
		}
		crlf := make([]byte, 2)
		if _, err := io.ReadFull(br, crlf); err != nil {
			return io.ErrUnexpectedEOF
		}
		if crlf[0] != '\r' || crlf[1] != '\n' {
			return errBadFraming
		}
		if _, err := dst.Write(crlf); err != nil {
			return err
		}
	}
}

// readLine returns one line including its terminator. The slice is only valid
// until the next read; callers write it out immediately.
func readLine(br *bufio.Reader) ([]byte, error) {
	line, err := br.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) {
		return nil, errBadFraming
	}
	if err != nil {
		if err == io.EOF {
			return nil, io.ErrUnexpectedEOF
		}
		return nil, err
	}
	if len(line) > maxLineBytes {
		return nil, errBadFraming
	}
	return line, nil
}

func parseChunkSize(line []byte) (int64, error) {
	text := strings.TrimSpace(string(line))
	if i := strings.IndexByte(text, ';'); i >= 0 {
		text = strings.TrimSpace(text[:i])
	}
	size, err := strconv.ParseInt(text, 16, 64)
	if err != nil || size < 0 {
		return 0, errBadFraming
	}
	return size, nil
}

// capBuffer keeps up to limit bytes and silently drops everything once the
// limit is exceeded, so a partial body is never parsed.
type capBuffer struct {
	limit    int
	buf      bytes.Buffer
	overflow bool
}

func (c *capBuffer) Write(p []byte) (int, error) {
	if c.overflow {
		return len(p), nil
	}
	if c.buf.Len()+len(p) > c.limit {
		c.overflow = true
		c.buf.Reset()
		return len(p), nil
	}
	c.buf.Write(p)
	return len(p), nil
}

// Bytes returns the captured body, or nil if it overflowed.
func (c *capBuffer) Bytes() []byte {
	if c.overflow {
		return nil
	}
	return c.buf.Bytes()
}
