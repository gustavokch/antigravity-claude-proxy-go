package api

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

// CCRSender sends an HTTP request upstream with the given JSON payload.
type CCRSender func(ctx context.Context, body []byte) (*http.Response, error)

// CCRProxyOptions configures the CCR hydration proxy behavior.
type CCRProxyOptions struct {
	IsCCREnabled   func() bool
	GetChunk       func(chunkID string) (payload string, isError bool)
	RecordHeadroom func(count int)
	Sender         CCRSender
	MaxHydrations  int
}

// ProxyAnthropicStreamWithCCR handles streaming Anthropic messages requests with CCR hydration loop.
func ProxyAnthropicStreamWithCCR(ctx context.Context, writer http.ResponseWriter, reqMap map[string]any, opts CCRProxyOptions) error {
	maxHydrations := opts.MaxHydrations
	if maxHydrations <= 0 {
		maxHydrations = maxCCRHydrations
	}

	flusher, _ := writer.(http.Flusher)

	var (
		totalInputTokens         int
		totalOutputTokens        int
		totalCacheReadTokens     int
		totalCacheCreationTokens int
		totalCCRRetrievals       int
		baseBlockIndex           int
		messageStartEmitted      bool
	)

	writeSSEEvent := func(eventName string, data []byte) {
		if eventName != "" {
			fmt.Fprintf(writer, "event: %s\n", eventName)
		}
		fmt.Fprintf(writer, "data: %s\n\n", string(data))
		if flusher != nil {
			flusher.Flush()
		}
	}

	for iter := 0; iter <= maxHydrations; iter++ {
		reqBytes, err := json.Marshal(reqMap)
		if err != nil {
			return fmt.Errorf("failed to marshal request for iteration %d: %w", iter, err)
		}

		resp, err := opts.Sender(ctx, reqBytes)
		if err != nil {
			if !messageStartEmitted {
				http.Error(writer, fmt.Sprintf("Upstream request failed: %v", err), http.StatusBadGateway)
				return err
			}
			errPayload, _ := json.Marshal(map[string]any{
				"type": "error",
				"error": map[string]any{
					"type":    "api_error",
					"message": fmt.Sprintf("Upstream request failed: %v", err),
				},
			})
			writeSSEEvent("error", errPayload)
			return err
		}

		if resp.StatusCode != http.StatusOK {
			defer resp.Body.Close()
			if !messageStartEmitted {
				for k, vv := range resp.Header {
					for _, v := range vv {
						writer.Header().Add(k, v)
					}
				}
				writer.WriteHeader(resp.StatusCode)
				_, _ = io.Copy(writer, resp.Body)
				return nil
			}
			bodyBytes, _ := io.ReadAll(resp.Body)
			errPayload, _ := json.Marshal(map[string]any{
				"type": "error",
				"error": map[string]any{
					"type":    "api_error",
					"message": fmt.Sprintf("Upstream error (%d): %s", resp.StatusCode, string(bodyBytes)),
				},
			})
			writeSSEEvent("error", errPayload)
			return nil
		}

		// Ensure SSE headers on client writer on first successful stream
		if !messageStartEmitted {
			writer.Header().Set("Content-Type", "text/event-stream")
			writer.Header().Set("Cache-Control", "no-cache")
			writer.Header().Set("Connection", "keep-alive")
		}

		state := newCCRStreamState(baseBlockIndex)
		var pendingTerminalEvents [][2]string // pairs of [eventName, dataStr]

		scanner := bufio.NewScanner(resp.Body)
		// Support large lines in SSE
		scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)

		var curEvent string
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "event:") {
				curEvent = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
				continue
			}
			if !strings.HasPrefix(line, "data:") {
				continue
			}

			dataStr := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if dataStr == "[DONE]" {
				continue
			}

			var event map[string]any
			if err := json.Unmarshal([]byte(dataStr), &event); err != nil {
				continue
			}

			eType, _ := event["type"].(string)
			switch eType {
			case "message_start":
				if msg, ok := event["message"].(map[string]any); ok {
					if usage, ok := msg["usage"].(map[string]any); ok {
						if inTok, ok := usage["input_tokens"].(float64); ok && iter == 0 {
							totalInputTokens = int(inTok)
						}
						if readTok, ok := usage["cache_read_input_tokens"].(float64); ok {
							totalCacheReadTokens += int(readTok)
						}
						if crTok, ok := usage["cache_creation_input_tokens"].(float64); ok {
							totalCacheCreationTokens += int(crTok)
						}
					}
				}
				if iter == 0 {
					writeSSEEvent(curEvent, []byte(dataStr))
					messageStartEmitted = true
				}

			case "content_block_start":
				idx := int(event["index"].(float64))
				rawBlock, _ := event["content_block"].(map[string]any)
				if downstreamIdx, emit := state.StartBlock(idx, rawBlock); emit {
					event["index"] = downstreamIdx
					modData, _ := json.Marshal(event)
					writeSSEEvent(curEvent, modData)
				}

			case "content_block_delta":
				idx := int(event["index"].(float64))
				if delta, ok := event["delta"].(map[string]any); ok {
					dType, _ := delta["type"].(string)
					if dType == "text_delta" {
						newText, _ := delta["text"].(string)
						state.AppendText(idx, newText)
					} else if dType == "input_json_delta" {
						if pj, ok := delta["partial_json"].(string); ok {
							state.AppendJSON(idx, pj)
						}
					}
				}
				if downstreamIdx, emit := state.MapIndex(idx); emit {
					event["index"] = downstreamIdx
					modData, _ := json.Marshal(event)
					writeSSEEvent(curEvent, modData)
				}

			case "content_block_stop":
				idx := int(event["index"].(float64))
				if downstreamIdx, emit := state.MapIndex(idx); emit {
					event["index"] = downstreamIdx
					modData, _ := json.Marshal(event)
					writeSSEEvent(curEvent, modData)
				}

			case "message_delta":
				if usage, ok := event["usage"].(map[string]any); ok {
					if outTok, ok := usage["output_tokens"].(float64); ok {
						totalOutputTokens += int(outTok)
					}
				}
				pendingTerminalEvents = append(pendingTerminalEvents, [2]string{curEvent, dataStr})

			case "message_stop":
				pendingTerminalEvents = append(pendingTerminalEvents, [2]string{curEvent, dataStr})

			default:
				// Forward any other events (ping, etc.)
				writeSSEEvent(curEvent, []byte(dataStr))
			}
		}
		_ = resp.Body.Close()

		// Finalize blocks and identify retrieveCalls
		retrieveCalls := state.Finalize()

		ccrEnabled := opts.IsCCREnabled == nil || opts.IsCCREnabled()
		needsHydration := len(retrieveCalls) > 0 && iter < maxHydrations && ccrEnabled

		if needsHydration {
			totalCCRRetrievals += len(retrieveCalls)
			var toolResults []any
			for _, call := range retrieveCalls {
				inputMap, _ := call["input"].(map[string]any)
				chunkID, _ := inputMap["chunk_id"].(string)

				var payload string
				var isErr bool
				if opts.GetChunk != nil {
					payload, isErr = opts.GetChunk(chunkID)
				} else {
					payload = fmt.Sprintf("Error: Chunk %s not found (no resolver)", chunkID)
					isErr = true
				}

				resBlock := map[string]any{
					"type":        "tool_result",
					"tool_use_id": call["id"],
					"content":     payload,
				}
				if isErr {
					resBlock["is_error"] = true
				}
				toolResults = append(toolResults, resBlock)
			}

			// Reconstruct messages array
			existingMsgs, _ := reqMap["messages"].([]any)
			existingMsgs = append(existingMsgs,
				map[string]any{"role": "assistant", "content": state.AssistantBlocks()},
				map[string]any{"role": "user", "content": toolResults},
			)
			reqMap["messages"] = existingMsgs
			baseBlockIndex += state.VisibleCount()
			continue
		}

		// Terminal iteration - flush usage and finish events
		if opts.RecordHeadroom != nil && totalCCRRetrievals > 0 {
			opts.RecordHeadroom(totalCCRRetrievals)
		}

		for _, ev := range pendingTerminalEvents {
			evName := ev[0]
			evData := ev[1]

			var parsed map[string]any
			if err := json.Unmarshal([]byte(evData), &parsed); err == nil {
				if pType, _ := parsed["type"].(string); pType == "message_delta" {
					// Suppressing the retrieve call can leave the turn with no
					// tool_use block at all; stop_reason must follow.
					reconcileStopReasonEvent(parsed, state.HasVisibleToolUse())
					usage, ok := parsed["usage"].(map[string]any)
					if !ok {
						usage = make(map[string]any)
						parsed["usage"] = usage
					}
					usage["output_tokens"] = totalOutputTokens
					if totalInputTokens > 0 {
						usage["input_tokens"] = totalInputTokens
					}
					if totalCacheReadTokens > 0 {
						usage["cache_read_input_tokens"] = totalCacheReadTokens
					}
					if totalCacheCreationTokens > 0 {
						usage["cache_creation_input_tokens"] = totalCacheCreationTokens
					}
					patchedBytes, _ := json.Marshal(parsed)
					writeSSEEvent(evName, patchedBytes)
					continue
				}
			}
			writeSSEEvent(evName, []byte(evData))
		}
		return nil
	}

	return nil
}

// ProxyAnthropicJSONWithCCR handles non-streaming Anthropic messages requests with CCR hydration loop.
func ProxyAnthropicJSONWithCCR(ctx context.Context, writer http.ResponseWriter, reqMap map[string]any, opts CCRProxyOptions) error {
	maxHydrations := opts.MaxHydrations
	if maxHydrations <= 0 {
		maxHydrations = maxCCRHydrations
	}

	var (
		totalInputTokens         int
		totalOutputTokens        int
		totalCacheReadTokens     int
		totalCacheCreationTokens int
		totalCCRRetrievals       int
	)

	for iter := 0; iter <= maxHydrations; iter++ {
		reqBytes, err := json.Marshal(reqMap)
		if err != nil {
			return fmt.Errorf("failed to marshal request for iteration %d: %w", iter, err)
		}

		resp, err := opts.Sender(ctx, reqBytes)
		if err != nil {
			http.Error(writer, fmt.Sprintf("Upstream request failed: %v", err), http.StatusBadGateway)
			return err
		}

		bodyBytes, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			http.Error(writer, fmt.Sprintf("Failed to read upstream response: %v", err), http.StatusBadGateway)
			return err
		}

		if resp.StatusCode != http.StatusOK {
			for k, vv := range resp.Header {
				for _, v := range vv {
					writer.Header().Add(k, v)
				}
			}
			writer.WriteHeader(resp.StatusCode)
			_, _ = writer.Write(bodyBytes)
			return nil
		}

		var respMap map[string]any
		if err := json.Unmarshal(bodyBytes, &respMap); err != nil {
			// Not valid JSON, proxy verbatim
			for k, vv := range resp.Header {
				for _, v := range vv {
					writer.Header().Add(k, v)
				}
			}
			writer.WriteHeader(resp.StatusCode)
			_, _ = writer.Write(bodyBytes)
			return nil
		}

		// Accumulate usage
		if usage, ok := respMap["usage"].(map[string]any); ok {
			if inTok, ok := usage["input_tokens"].(float64); ok && iter == 0 {
				totalInputTokens = int(inTok)
			}
			if outTok, ok := usage["output_tokens"].(float64); ok {
				totalOutputTokens += int(outTok)
			}
			if readTok, ok := usage["cache_read_input_tokens"].(float64); ok {
				totalCacheReadTokens += int(readTok)
			}
			if crTok, ok := usage["cache_creation_input_tokens"].(float64); ok {
				totalCacheCreationTokens += int(crTok)
			}
		}

		retrieveCalls := findRetrieveToolUsesFromResponse(respMap)
		ccrEnabled := opts.IsCCREnabled == nil || opts.IsCCREnabled()
		needsHydration := len(retrieveCalls) > 0 && iter < maxHydrations && ccrEnabled

		if needsHydration {
			totalCCRRetrievals += len(retrieveCalls)
			var toolResults []any
			for _, call := range retrieveCalls {
				inputMap, _ := call["input"].(map[string]any)
				chunkID, _ := inputMap["chunk_id"].(string)

				var payload string
				var isErr bool
				if opts.GetChunk != nil {
					payload, isErr = opts.GetChunk(chunkID)
				} else {
					payload = fmt.Sprintf("Error: Chunk %s not found (no resolver)", chunkID)
					isErr = true
				}

				resBlock := map[string]any{
					"type":        "tool_result",
					"tool_use_id": call["id"],
					"content":     payload,
				}
				if isErr {
					resBlock["is_error"] = true
				}
				toolResults = append(toolResults, resBlock)
			}

			existingMsgs, _ := reqMap["messages"].([]any)
			existingMsgs = append(existingMsgs,
				map[string]any{"role": "assistant", "content": respMap["content"]},
				map[string]any{"role": "user", "content": toolResults},
			)
			reqMap["messages"] = existingMsgs
			continue
		}

		// Final response: filter out any headroom_retrieve blocks from final content
		stripRetrieveBlocks(respMap)

		// Patch usage
		usage, ok := respMap["usage"].(map[string]any)
		if !ok {
			usage = make(map[string]any)
			respMap["usage"] = usage
		}
		usage["output_tokens"] = totalOutputTokens
		if totalInputTokens > 0 {
			usage["input_tokens"] = totalInputTokens
		}
		if totalCacheReadTokens > 0 {
			usage["cache_read_input_tokens"] = totalCacheReadTokens
		}
		if totalCacheCreationTokens > 0 {
			usage["cache_creation_input_tokens"] = totalCacheCreationTokens
		}

		if opts.RecordHeadroom != nil && totalCCRRetrievals > 0 {
			opts.RecordHeadroom(totalCCRRetrievals)
		}

		finalBytes, err := json.Marshal(respMap)
		if err != nil {
			slog.Error("Failed to marshal final CCR response", "error", err)
			writer.WriteHeader(http.StatusOK)
			_, _ = writer.Write(bodyBytes)
			return nil
		}

		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write(finalBytes)
		return nil
	}

	return nil
}
