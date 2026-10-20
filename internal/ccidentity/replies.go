package ccidentity

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"hash"
	"io"
	"regexp"
	"strings"
	"sync"
)

// replyLogCapacity bounds the keys a ReplyLog holds. A reply leaves one key per
// tool call, thinking signature and text block, so this keeps the latest replies
// of far more conversations than one proxy serves at once, in a few megabytes.
const replyLogCapacity = 1 << 16

// requestIDPattern is the guard the real client puts on cc_prev_req: 2.1.304
// writes the field only when the value matches it.
var requestIDPattern = regexp.MustCompile(`^req_[A-Za-z0-9_-]{1,36}$`)

// Key kinds. Each names the piece of a reply a client echoes back.
const (
	kindID        = "id"
	kindText      = "text"
	kindSignature = "signature"
	kindRedacted  = "redacted_thinking"
)

// ReplyLog remembers which upstream request produced each assistant reply, so a
// later request can name that request in cc_prev_req.
//
// The real client takes cc_prev_req from its own transcript. Before each
// messages request, 2.1.304 walks the messages back from the end and takes the
// requestId of the latest assistant message that has one; the billing builder
// then checks it against requestIDPattern. The value belongs to a conversation,
// not to a session: a subagent's requests name the subagent's own replies, and
// the first request of a conversation names nothing. The captures agree: each
// follow-up names the request-id response header of the request before it, and
// the first request of each process names nothing
// (.reference/claude-code-headers-20261020*.jsonl).
//
// A proxy client's messages carry no request ids, so the log keeps the link.
// When a reply comes back, it is keyed by what a client has to echo to continue
// the conversation: each tool call id, each thinking signature, and the text of
// each text block. When a later request's latest assistant message holds one of
// those, the log knows the reply and the request that produced it.
//
// Keys are scoped by upstream credential and client session (see ReplyScope).
// The credential keeps one account from being sent another account's request
// id; the session keeps two conversations that say the same text apart.
//
// The log is bounded and drops its oldest key first. A reply it dropped or never
// saw (the proxy restarted, another account served it) sends no cc_prev_req.
type ReplyLog struct {
	mu       sync.Mutex
	ids      map[replyKey]string
	order    []replyKey // insertion order; a ring once it reaches capacity
	next     int        // the oldest slot of the full ring
	capacity int
}

// replyKey identifies one echoed piece of a reply within one scope.
type replyKey [16]byte

// NewReplyLog returns an empty log.
func NewReplyLog() *ReplyLog {
	return newReplyLog(replyLogCapacity)
}

func newReplyLog(capacity int) *ReplyLog {
	return &ReplyLog{ids: make(map[replyKey]string), capacity: capacity}
}

// ReplyScope names the space a reply's keys live in: one upstream credential and
// one client session. upstream names the credential and must not collide across
// kinds of upstream.
//
// A request without a session key gets an empty scope, which records and finds
// nothing. Such a request is sent under a fresh random session (NewSessionKey),
// so it is the first request of its session, and that names no previous request.
func ReplyScope(upstream, sessionKey string) string {
	if strings.TrimSpace(sessionKey) == "" {
		return ""
	}
	return upstream + "\x00" + sessionKey
}

// PrevRequestID returns the upstream request id of the reply that the turn's
// latest assistant message echoes, or "" when the log does not know that reply.
//
// Only the latest assistant message is looked up. The real client walks further
// back when the latest one has no request id, but on its side that happens only
// for a message it made up itself. Here a miss means the log dropped or never
// saw the reply, and an older reply's id would name a request that is not the
// one before this. Omitting the field claims nothing.
func (l *ReplyLog) PrevRequestID(scope string, turn Turn) string {
	if l == nil || scope == "" || len(turn.latestReply) == 0 {
		return ""
	}
	keys := replyKeys(scope, turn.latestReply)
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := len(keys) - 1; i >= 0; i-- {
		if id, ok := l.ids[keys[i]]; ok {
			return id
		}
	}
	return ""
}

// RecordMessage records the reply in a complete, non-streaming Messages API
// response body under scope.
func (l *ReplyLog) RecordMessage(scope, requestID string, body []byte) {
	if l == nil || scope == "" || !requestIDPattern.MatchString(requestID) {
		return
	}
	var message struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(body, &message) != nil {
		return
	}
	l.record(requestID, replyKeys(scope, message.Content)...)
}

// Watch returns body wrapped so the reply it streams is recorded under scope as
// the caller reads it. body must be a Messages API event stream; the bytes pass
// through unchanged.
//
// A block is recorded once its key is whole: a tool call when it starts, text
// and thinking when they stop. A stream cut short keeps the blocks that
// completed before the cut.
func (l *ReplyLog) Watch(scope, requestID string, body io.ReadCloser) io.ReadCloser {
	if l == nil || scope == "" || !requestIDPattern.MatchString(requestID) {
		return body
	}
	return &replyStream{ReadCloser: body, log: l, scope: scope, requestID: requestID}
}

func (l *ReplyLog) record(requestID string, keys ...replyKey) {
	if len(keys) == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, key := range keys {
		if _, known := l.ids[key]; !known {
			if len(l.order) < l.capacity {
				l.order = append(l.order, key)
			} else {
				delete(l.ids, l.order[l.next])
				l.order[l.next] = key
				l.next = (l.next + 1) % l.capacity
			}
		}
		l.ids[key] = requestID
	}
}

// replyBlock is the part of a content block the log keys on. One shape decodes
// a block of a request's assistant message and the content_block of a streamed
// content_block_start event.
type replyBlock struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	Text      string `json:"text"`
	Signature string `json:"signature"`
	Data      string `json:"data"`
}

func (b replyBlock) key(scope string) (replyKey, bool) {
	switch {
	case b.ID != "":
		return keyOf(scope, kindID, b.ID), true
	case b.Type == "text" && b.Text != "":
		return keyOf(scope, kindText, b.Text), true
	case b.Type == "thinking" && b.Signature != "":
		return keyOf(scope, kindSignature, b.Signature), true
	case b.Type == "redacted_thinking" && b.Data != "":
		return keyOf(scope, kindRedacted, b.Data), true
	}
	return replyKey{}, false
}

// replyKeys lists the keys of an assistant message's content in block order. A
// string content is one text block.
func replyKeys(scope string, content json.RawMessage) []replyKey {
	content = bytes.TrimSpace(content)
	if len(content) == 0 {
		return nil
	}
	if content[0] == '"' {
		var text string
		if json.Unmarshal(content, &text) != nil || text == "" {
			return nil
		}
		return []replyKey{keyOf(scope, kindText, text)}
	}
	var blocks []replyBlock
	if json.Unmarshal(content, &blocks) != nil {
		return nil
	}
	keys := make([]replyKey, 0, len(blocks))
	for _, block := range blocks {
		if key, ok := block.key(scope); ok {
			keys = append(keys, key)
		}
	}
	return keys
}

// keyHash starts the hash behind a key. The value is written after it, which
// lets a streamed text block be hashed one delta at a time.
func keyHash(scope, kind string) hash.Hash {
	h := sha256.New()
	_, _ = io.WriteString(h, scope)
	_, _ = h.Write([]byte{0})
	_, _ = io.WriteString(h, kind)
	_, _ = h.Write([]byte{0})
	return h
}

func keyOf(scope, kind, value string) replyKey {
	h := keyHash(scope, kind)
	_, _ = io.WriteString(h, value)
	return sumKey(h)
}

func sumKey(h hash.Hash) replyKey {
	var key replyKey
	copy(key[:], h.Sum(nil))
	return key
}

// contentBlockEvent is in every event that carries reply content, and in no
// other event of the stream.
var contentBlockEvent = []byte(`"content_block_`)

// replyStream records the reply of an event stream as it is read. One reader
// drives it, as with any response body.
type replyStream struct {
	io.ReadCloser
	log       *ReplyLog
	scope     string
	requestID string
	line      []byte             // a partial line carried to the next read
	open      map[int]*openBlock // text and thinking blocks not yet stopped
}

// openBlock is a text or thinking block whose key is whole only when it stops.
type openBlock struct {
	text      hash.Hash // a text block's key, fed delta by delta; nil for thinking
	hasText   bool
	signature strings.Builder // a thinking block's signature
}

func (s *replyStream) Read(p []byte) (int, error) {
	n, err := s.ReadCloser.Read(p)
	s.consume(p[:n])
	return n, err
}

// consume handles every complete line in chunk and keeps the rest for the next
// read.
func (s *replyStream) consume(chunk []byte) {
	for len(chunk) > 0 {
		end := bytes.IndexByte(chunk, '\n')
		if end < 0 {
			s.line = append(s.line, chunk...)
			return
		}
		if len(s.line) > 0 {
			s.line = append(s.line, chunk[:end]...)
			s.handle(s.line)
			s.line = s.line[:0]
		} else {
			s.handle(chunk[:end])
		}
		chunk = chunk[end+1:]
	}
}

func (s *replyStream) handle(line []byte) {
	payload, ok := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:"))
	if !ok || !bytes.Contains(payload, contentBlockEvent) {
		return
	}
	var event struct {
		Type         string     `json:"type"`
		Index        int        `json:"index"`
		ContentBlock replyBlock `json:"content_block"`
		Delta        struct {
			Type      string `json:"type"`
			Text      string `json:"text"`
			Signature string `json:"signature"`
		} `json:"delta"`
	}
	if json.Unmarshal(payload, &event) != nil {
		return
	}
	switch event.Type {
	case "content_block_start":
		s.start(event.Index, event.ContentBlock)
	case "content_block_delta":
		block := s.open[event.Index]
		if block == nil {
			return
		}
		switch event.Delta.Type {
		case "text_delta":
			block.addText(event.Delta.Text)
		case "signature_delta":
			block.signature.WriteString(event.Delta.Signature)
		}
	case "content_block_stop":
		block := s.open[event.Index]
		if block == nil {
			return
		}
		delete(s.open, event.Index)
		if key, ok := block.key(s.scope); ok {
			s.log.record(s.requestID, key)
		}
	}
}

func (s *replyStream) start(index int, block replyBlock) {
	var open *openBlock
	switch block.Type {
	case "text":
		open = &openBlock{text: keyHash(s.scope, kindText)}
		open.addText(block.Text)
	case "thinking":
		open = &openBlock{}
		open.signature.WriteString(block.Signature)
	default:
		// A tool call's id and a redacted block's data arrive whole.
		if key, ok := block.key(s.scope); ok {
			s.log.record(s.requestID, key)
		}
		return
	}
	if s.open == nil {
		s.open = make(map[int]*openBlock)
	}
	s.open[index] = open
}

func (b *openBlock) addText(text string) {
	if b.text == nil || text == "" {
		return
	}
	_, _ = io.WriteString(b.text, text)
	b.hasText = true
}

func (b *openBlock) key(scope string) (replyKey, bool) {
	if b.text != nil {
		return sumKey(b.text), b.hasText
	}
	if b.signature.Len() == 0 {
		return replyKey{}, false
	}
	return keyOf(scope, kindSignature, b.signature.String()), true
}
