package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"antigravity-go-proxy/internal/classifier"
	"antigravity-go-proxy/internal/classifier/corpus"
	"antigravity-go-proxy/internal/config"
)

// defaultClassifierBackendTimeout bounds a rerouted call. A classifier
// request sits in front of the user's permission prompt, so a hung sidecar
// must surface as a fast failure, not an indefinite stall.
const defaultClassifierBackendTimeout = 20 * time.Second

// maxClassifierBackendResponse caps what is read from a rerouted backend. A
// verdict is a few dozen bytes; anything near this bound is a misconfigured
// endpoint, not a verdict.
const maxClassifierBackendResponse = 1 << 20

// applyClassifierConfig rebuilds the rule matcher. Regexes are compiled here
// rather than per request, and a config that fails to compile leaves the
// previous rule set active instead of silently disabling interception.
func (server *Server) applyClassifierConfig(cfg config.ClassifierConfig) {
	// The recorder is rebuilt on every config application so a settings save
	// takes effect without a restart.
	if cfg.Capture.Enabled {
		resolved := cfg.Capture.Resolved()
		server.classifierCorpus = corpus.New(resolved.Dir, corpus.Options{
			MaxFiles:     resolved.MaxFiles,
			MaxFileBytes: resolved.MaxFileBytes,
			RedactPaths:  resolved.RedactPathsEnabled(),
		})
	} else {
		server.classifierCorpus = nil
	}

	if server.classifierMatcher == nil {
		matcher, err := classifier.NewConfigurableMatcher(cfg.Rules, cfg.Backends)
		if err != nil {
			server.classifierLogger().Warn("[Server] classifier rules rejected at startup; rule routing is inactive", "error", err)
			matcher, _ = classifier.NewConfigurableMatcher(nil, nil)
		}
		server.classifierMatcher = matcher
		return
	}
	if err := server.classifierMatcher.UpdateRules(cfg.Rules, cfg.Backends); err != nil {
		server.classifierLogger().Warn("[Server] classifier rules rejected; keeping the previous rule set", "error", err)
	}
}

func (server *Server) classifierLogger() *slog.Logger {
	if server.logger != nil {
		return server.logger
	}
	return slog.Default()
}

// classifierRequest bundles the inputs needed to evaluate and apply a rule.
type classifierRequest struct {
	rule            *config.Rule
	backend         *config.TargetBackend
	rawBody         []byte
	model           string
	streamRequested bool
	// captureSource is the corpus source for this request. A rule that
	// answers assigns through it so the single deferred recorder in messages
	// writes one row with the right provenance.
	captureSource *corpus.Source
}

// setCaptureSource records which path answered, when capture is on.
func (req classifierRequest) setCaptureSource(source corpus.Source) {
	if req.captureSource != nil {
		*req.captureSource = source
	}
}

// applyClassifierRule carries out a matched rule.
//
// responded is true when the rule already wrote the HTTP response and the
// caller must return immediately. skipDetect is true only when the rule
// deliberately declines to act (passthrough), meaning the built-in
// classifier.Detect handling must be bypassed as well. A reroute that fails
// returns (false, false): the request falls through to today's behavior.
func (server *Server) applyClassifierRule(
	writer http.ResponseWriter,
	request *http.Request,
	req classifierRequest,
) (responded bool, skipDetect bool) {
	start := time.Now()
	rule := req.rule
	record := func(status classifier.EventStatus, detail string) {
		server.classifierAudit.Add(classifier.Event{
			Timestamp: time.Now(),
			RuleID:    rule.ID,
			RuleName:  rule.Name,
			Action:    rule.Action,
			Backend:   rule.TargetBackend,
			Status:    status,
			LatencyMs: time.Since(start).Milliseconds(),
			Model:     req.model,
			Detail:    detail,
		})
	}

	switch rule.Action {
	case config.RuleActionPassthrough:
		record(classifier.EventStatusPassthrough, "")
		return false, true

	case config.RuleActionStub:
		stub, err := classifier.StubWithText(req.model, rule.VerdictTemplate)
		if err != nil {
			server.classifierLogger().Warn("[Server] classifier rule stub failed; falling through", "rule", rule.ID, "error", err)
			record(classifier.EventStatusError, err.Error())
			return false, false
		}
		// Set before the write: the source names the path that produced the
		// verdict, not the delivery. A stub whose write failed is still a
		// stub, and labeling it upstream would claim a teacher model graded
		// an action the proxy graded itself.
		req.setCaptureSource(corpus.SourceStub)
		if err := writeClassifierResponse(writer, stub, req.streamRequested); err != nil {
			record(classifier.EventStatusError, err.Error())
			return true, false
		}
		record(classifier.EventStatusStubbed, "")
		return true, false

	case config.RuleActionReroute:
		if req.backend == nil {
			record(classifier.EventStatusError, "target backend not found")
			return false, false
		}
		message, err := server.callClassifierBackend(request.Context(), req.backend, req.rawBody, req.model)
		if err != nil {
			server.classifierLogger().Warn("[Server] classifier reroute failed; falling back to built-in handling",
				"rule", rule.ID, "backend", rule.TargetBackend, "error", err)
			record(classifier.EventStatusError, err.Error())
			return false, false
		}
		// Set once the backend has answered, before the write, for the same
		// reason as the stub branch above. A backend call that failed returns
		// earlier and leaves the source alone, so a fall-through to built-in
		// handling is still labeled by whichever path answers.
		req.setCaptureSource(corpus.SourceRule)
		if err := writeClassifierResponse(writer, message, req.streamRequested); err != nil {
			record(classifier.EventStatusError, err.Error())
			return true, false
		}
		record(classifier.EventStatusRerouted, "")
		return true, false
	}

	return false, false
}

type backendFormatAdapter struct {
	preparePayload func(rawBody []byte, backend *config.TargetBackend) ([]byte, error)
	setHeaders     func(req *http.Request, apiKey string)
	parseResponse  func(respBody []byte, clientModel string) ([]byte, error)
}

var openAIFormatAdapter = backendFormatAdapter{
	preparePayload: func(rawBody []byte, backend *config.TargetBackend) ([]byte, error) {
		return classifier.TranslateAnthropicToOpenAI(rawBody, backend.Model, backend.MaxTokens)
	},
	setHeaders: func(req *http.Request, apiKey string) {
		req.Header.Set("Content-Type", "application/json")
		if apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
	},
	parseResponse: func(respBody []byte, clientModel string) ([]byte, error) {
		return classifier.TranslateOpenAIToAnthropic(respBody, clientModel)
	},
}

var anthropicFormatAdapter = backendFormatAdapter{
	preparePayload: func(rawBody []byte, backend *config.TargetBackend) ([]byte, error) {
		return rawBody, nil
	},
	setHeaders: func(req *http.Request, apiKey string) {
		req.Header.Set("Content-Type", "application/json")
		if apiKey != "" {
			req.Header.Set("x-api-key", apiKey)
			req.Header.Set("anthropic-version", "2023-06-01")
		}
	},
	parseResponse: func(respBody []byte, clientModel string) ([]byte, error) {
		return respBody, nil
	},
}

func getBackendFormatAdapter(format config.BackendFormat) backendFormatAdapter {
	if format == config.BackendFormatOpenAI {
		return openAIFormatAdapter
	}
	return anthropicFormatAdapter
}

// callClassifierBackend posts the request to backend and returns an Anthropic
// Messages response body, translating in both directions when the backend
// speaks OpenAI.
func (server *Server) callClassifierBackend(
	ctx context.Context,
	backend *config.TargetBackend,
	rawBody []byte,
	model string,
) ([]byte, error) {
	timeout := time.Duration(backend.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = defaultClassifierBackendTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	adapter := getBackendFormatAdapter(backend.Format)

	payload, err := adapter.preparePayload(rawBody, backend)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, backend.URL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	adapter.setHeaders(req, backend.APIKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxClassifierBackendResponse))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("classifier backend returned %d", resp.StatusCode)
	}

	return adapter.parseResponse(body, model)
}

// writeClassifierResponse sends a completed Anthropic message, re-emitting it
// as SSE frames when the client asked to stream.
func writeClassifierResponse(writer http.ResponseWriter, message []byte, streamRequested bool) error {
	if !streamRequested {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusOK)
		_, err := writer.Write(message)
		return err
	}

	flusher, ok := writer.(http.Flusher)
	if !ok {
		return errors.New("classifier: response writer does not support streaming")
	}
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache")
	writer.Header().Set("Connection", "keep-alive")
	writer.Header().Set("X-Accel-Buffering", "no")
	writer.WriteHeader(http.StatusOK)
	return classifier.WriteSyntheticStream(writer, flusher.Flush, message)
}
