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
	rule *config.Rule,
	backend *config.TargetBackend,
	rawBody []byte,
	model string,
	streamRequested bool,
) (responded bool, skipDetect bool) {
	start := time.Now()
	record := func(status, detail string) {
		server.classifierAudit.Add(classifier.Event{
			Timestamp: time.Now(),
			RuleID:    rule.ID,
			RuleName:  rule.Name,
			Action:    string(rule.Action),
			Backend:   rule.TargetBackend,
			Status:    status,
			LatencyMs: time.Since(start).Milliseconds(),
			Model:     model,
			Detail:    detail,
		})
	}

	switch rule.Action {
	case config.RuleActionPassthrough:
		record("passthrough", "")
		return false, true

	case config.RuleActionStub:
		stub, err := classifier.StubWithText(model, rule.VerdictTemplate)
		if err != nil {
			server.classifierLogger().Warn("[Server] classifier rule stub failed; falling through", "rule", rule.ID, "error", err)
			record("error", err.Error())
			return false, false
		}
		if err := writeClassifierResponse(writer, stub, streamRequested); err != nil {
			record("error", err.Error())
			return true, false
		}
		record("stubbed", "")
		return true, false

	case config.RuleActionReroute:
		if backend == nil {
			record("error", "target backend not found")
			return false, false
		}
		message, err := server.callClassifierBackend(request.Context(), backend, rawBody, model)
		if err != nil {
			server.classifierLogger().Warn("[Server] classifier reroute failed; falling back to built-in handling",
				"rule", rule.ID, "backend", rule.TargetBackend, "error", err)
			record("error", err.Error())
			return false, false
		}
		if err := writeClassifierResponse(writer, message, streamRequested); err != nil {
			record("error", err.Error())
			return true, false
		}
		record("rerouted", "")
		return true, false
	}

	return false, false
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

	payload := rawBody
	if backend.Format == config.BackendFormatOpenAI {
		translated, err := classifier.TranslateAnthropicToOpenAI(rawBody, backend.Model, backend.MaxTokens)
		if err != nil {
			return nil, err
		}
		payload = translated
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, backend.URL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if backend.APIKey != "" {
		if backend.Format == config.BackendFormatOpenAI {
			req.Header.Set("Authorization", "Bearer "+backend.APIKey)
		} else {
			req.Header.Set("x-api-key", backend.APIKey)
			req.Header.Set("anthropic-version", "2023-06-01")
		}
	}

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

	if backend.Format == config.BackendFormatOpenAI {
		return classifier.TranslateOpenAIToAnthropic(body, model)
	}
	return body, nil
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
