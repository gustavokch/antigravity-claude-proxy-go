package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

// chatCompletions serves POST /v1/chat/completions by translating the OpenAI
// Chat Completions body to the Anthropic Messages shape, delegating to the
// standard /v1/messages pipeline, and translating the response back. The
// dispatch pipeline (model mapping, headroom, provider routing, retries, cost
// tracking) is reused untouched; translation lives at the edge only.
func (server *Server) chatCompletions(writer http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(writer, request.Body, maxRequestBody)
	body, err := io.ReadAll(request.Body)
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeOpenAIError(writer, http.StatusRequestEntityTooLarge, "invalid_request_error", "Request body too large")
			return
		}
		writeOpenAIError(writer, http.StatusBadRequest, "invalid_request_error", "Failed to read request body: "+err.Error())
		return
	}
	var openaiRequest map[string]any
	if err := json.Unmarshal(body, &openaiRequest); err != nil {
		writeOpenAIError(writer, http.StatusBadRequest, "invalid_request_error", "Invalid JSON request body: "+err.Error())
		return
	}
	anthropicRequest, err := translateOpenAIRequest(openaiRequest)
	if err != nil {
		writeOpenAIError(writer, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	requestModel := stringFrom(openaiRequest["model"])

	forwarded := request.Clone(request.Context())
	reqBody, err := json.Marshal(anthropicRequest)
	if err != nil {
		writeOpenAIError(writer, http.StatusBadRequest, "invalid_request_error", "Failed to marshal translated request: "+err.Error())
		return
	}
	forwarded.Body = io.NopCloser(bytes.NewReader(reqBody))
	forwarded.ContentLength = int64(len(reqBody))
	forwarded.Header = request.Header.Clone()
	forwarded.Header.Set("Content-Type", "application/json")

	translator := newOpenAIResponseWriter(writer, requestModel, openAIUsageRequested(openaiRequest))
	server.messages(translator, forwarded)
	translator.finish()
}

// openAIUsageRequested reports whether the OpenAI client asked for token
// usage in the stream (stream_options.include_usage).
func openAIUsageRequested(openaiRequest map[string]any) bool {
	options, ok := openaiRequest["stream_options"].(map[string]any)
	if !ok {
		return false
	}
	requested, _ := options["include_usage"].(bool)
	return requested
}

// writeOpenAIError writes an OpenAI-shaped error envelope.
func writeOpenAIError(writer http.ResponseWriter, status int, kind, message string) {
	writeJSON(writer, status, map[string]any{
		"error": map[string]any{"message": message, "type": kind, "code": kind},
	})
}

// openAIResponseWriter wraps the ResponseWriter handed to server.messages()
// and rewrites the Anthropic response it produces into the OpenAI shape. The
// mode is decided lazily from the Content-Type the pipeline sets: JSON
// responses are buffered and translated as a whole (including Anthropic error
// envelopes), SSE responses are translated event by event so per-event
// flushing is preserved. Headers are only written downstream once the mode is
// known, so failures before stream start still surface as plain JSON errors.
type openAIResponseWriter struct {
	inner          http.ResponseWriter
	model          string
	mode           responseMode
	statusCode     int
	sent           bool // headers+body started on inner
	buf            bytes.Buffer
	stream         *openAIStreamState
	parser         sseLineParser
	doneSent       bool
	usageRequested bool
}

type responseMode int

const (
	responseModeAuto responseMode = iota
	responseModeJSON
	responseModeSSE
)

func newOpenAIResponseWriter(inner http.ResponseWriter, model string, usageRequested bool) *openAIResponseWriter {
	return &openAIResponseWriter{inner: inner, model: model, usageRequested: usageRequested}
}

func (w *openAIResponseWriter) Header() http.Header { return w.inner.Header() }

func (w *openAIResponseWriter) WriteHeader(statusCode int) {
	if w.mode == responseModeAuto {
		w.detectMode()
	}
	w.statusCode = statusCode
	if w.mode == responseModeSSE {
		w.beginStream(statusCode)
	}
	// JSON mode defers the header write until the translated body is ready.
}

func (w *openAIResponseWriter) Write(p []byte) (int, error) {
	if w.mode == responseModeAuto {
		w.detectMode()
		if w.mode == responseModeSSE {
			w.beginStream(200)
		}
	}
	if w.mode == responseModeSSE {
		w.parser.feed(p, w.handleStreamEvent)
		return len(p), nil
	}
	w.buf.Write(p)
	return len(p), nil
}

// Flush preserves streaming semantics: in JSON mode it triggers the deferred
// translate-and-write, in SSE mode it flushes the translated bytes.
func (w *openAIResponseWriter) Flush() {
	if flusher, ok := w.inner.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *openAIResponseWriter) detectMode() {
	if strings.Contains(w.inner.Header().Get("Content-Type"), "text/event-stream") {
		w.mode = responseModeSSE
	} else {
		w.mode = responseModeJSON
	}
}

// beginStream hands the (already translated-then-rewritten) SSE response
// through. Upstream Content-Length would be wrong for the rewritten body.
func (w *openAIResponseWriter) beginStream(statusCode int) {
	if w.sent {
		return
	}
	w.sent = true
	w.stream = newOpenAIStreamState(w.model)
	w.inner.Header().Del("Content-Length")
	w.inner.WriteHeader(statusCode)
}

// writeSSEFrame frames and writes one SSE payload as "data: <payload>\n\n".
func (w *openAIResponseWriter) writeSSEFrame(payload []byte) {
	_, _ = w.inner.Write(append(append([]byte("data: "), payload...), '\n', '\n'))
}

// writeChunk marshals and emits one OpenAI chunk.
func (w *openAIResponseWriter) writeSSEChunk(chunk map[string]any) {
	encoded, err := json.Marshal(chunk)
	if err != nil {
		return
	}
	w.writeSSEFrame(encoded)
}

// handleStreamError emits an OpenAI error payload for a mid-stream error.
func (w *openAIResponseWriter) handleStreamError(message, kind string) {
	encoded, err := json.Marshal(map[string]any{
		"error": map[string]any{"message": message, "type": kind, "code": kind},
	})
	if err != nil {
		return
	}
	w.writeSSEFrame(encoded)
}

// writeStreamEnd emits the optional usage chunk (when the client requested
// stream_options.include_usage and any usage was observed) followed by the
// data: [DONE] sentinel. Called exactly once per stream.
func (w *openAIResponseWriter) writeStreamEnd() {
	if w.usageRequested && w.stream != nil && (w.stream.promptTokens > 0 || w.stream.completionTokens > 0) {
		w.writeSSEChunk(map[string]any{
			"id":      w.stream.id,
			"object":  "chat.completion.chunk",
			"created": w.stream.created,
			"model":   w.stream.model,
			"choices": []any{},
			"usage": map[string]any{
				"prompt_tokens":     w.stream.promptTokens,
				"completion_tokens": w.stream.completionTokens,
				"total_tokens":      w.stream.promptTokens + w.stream.completionTokens,
			},
		})
	}
	w.writeSSEFrame([]byte("[DONE]"))
}

// handleStreamEvent translates one parsed Anthropic SSE event into OpenAI
// chunks.
func (w *openAIResponseWriter) handleStreamEvent(eventType string, data map[string]any) {
	if w.stream == nil {
		return
	}
	if eventType == "error" {
		errObj, _ := data["error"].(map[string]any)
		w.handleStreamError(stringFrom(errObj["message"]), stringFrom(errObj["type"]))
		return
	}
	for _, chunk := range w.stream.HandleEvent(eventType, data) {
		w.writeSSEChunk(chunk)
	}
	if w.stream.done && !w.doneSent {
		w.doneSent = true
		w.writeStreamEnd()
	}
}

// finish runs after server.messages() returns: JSON responses are translated
// and written now, and an SSE stream that never sent message_stop still gets
// its [DONE] sentinel.
func (w *openAIResponseWriter) finish() {
	if w.mode == responseModeJSON && !w.sent {
		w.finishJSON()
		return
	}
	if w.mode == responseModeSSE && w.sent && !w.doneSent {
		w.doneSent = true
		w.writeStreamEnd()
	}
	if flusher, ok := w.inner.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *openAIResponseWriter) finishJSON() {
	w.sent = true
	if w.statusCode == 0 {
		w.statusCode = http.StatusOK
	}
	translated := w.translateJSONBody(w.buf.Bytes())
	w.inner.Header().Del("Content-Length")
	w.inner.WriteHeader(w.statusCode)
	_, _ = w.inner.Write(translated)
}

// translateJSONBody converts a buffered Anthropic JSON body (a Messages
// response or an error envelope) into the OpenAI shape.
func (w *openAIResponseWriter) translateJSONBody(raw []byte) []byte {
	var body map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(raw), &body); err != nil {
		return raw // not Anthropic JSON; pass through untouched
	}
	if body["type"] == "error" || body["error"] != nil {
		out, err := json.Marshal(translateAnthropicErrorToOpenAI(body))
		if err != nil {
			return raw
		}
		return out
	}
	out, err := json.Marshal(translateAnthropicMessageToOpenAI(body, w.model, time.Now().Unix()))
	if err != nil {
		return raw
	}
	return out
}

// --- Incremental SSE frame parser ---

// sseLineParser reassembles SSE frames from arbitrary Write boundaries and
// dispatches complete events. It mirrors the parsing rules of
// parseSSEStream (event:/data: lines, blank line = event boundary) but works
// incrementally on written chunks.
type sseLineParser struct {
	event   string
	data    bytes.Buffer
	pending []byte
}

func (p *sseLineParser) feed(chunk []byte, handle func(eventType string, data map[string]any)) {
	p.pending = append(p.pending, chunk...)
	for {
		idx := bytes.IndexByte(p.pending, '\n')
		if idx < 0 {
			return
		}
		line := string(bytes.TrimRight(p.pending[:idx], "\r"))
		p.pending = p.pending[idx+1:]
		if line == "" {
			p.dispatch(handle)
		} else if strings.HasPrefix(line, ":") {
			// comment / keepalive
		} else if strings.HasPrefix(line, "event: ") {
			p.event = strings.TrimPrefix(line, "event: ")
		} else if strings.HasPrefix(line, "data: ") {
			if p.data.Len() > 0 {
				p.data.WriteByte('\n')
			}
			p.data.WriteString(strings.TrimPrefix(line, "data: "))
		}
	}
}

func (p *sseLineParser) dispatch(handle func(eventType string, data map[string]any)) {
	if p.data.Len() == 0 && p.event == "" {
		return
	}
	var dataObj map[string]any
	if err := json.Unmarshal(p.data.Bytes(), &dataObj); err != nil {
		dataObj = nil
	}
	eventType := p.event
	if eventType == "" && dataObj != nil {
		eventType, _ = dataObj["type"].(string)
	}
	p.event = ""
	p.data.Reset()
	handle(eventType, dataObj)
}
