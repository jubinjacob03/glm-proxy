package zbridge

// Anthropic Messages API at /v1/messages, driven by the same ZAIResult stream
// as the OpenAI handler and converted to Anthropic SSE events on the fly.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// formatAnthropicError builds an Anthropic-style error envelope.
func formatAnthropicError(errType, message string) interface{} {
	return map[string]interface{}{
		"type": "error",
		"error": map[string]interface{}{
			"type":    errType,
			"message": message,
		},
	}
}

// extractAnthropicContent coerces an Anthropic content field (string or
// array of content blocks) into a plain string.
func extractAnthropicContent(content interface{}) string {
	if content == nil {
		return ""
	}
	if s, ok := content.(string); ok {
		return s
	}
	if arr, ok := content.([]interface{}); ok {
		var parts []string
		for _, item := range arr {
			if m, ok := item.(map[string]interface{}); ok {
				if t, _ := m["type"].(string); t == "text" {
					if txt, ok := m["text"].(string); ok {
						parts = append(parts, txt)
					}
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	b, _ := json.Marshal(content)
	return string(b)
}

// anthropicToOpenAIRequest rewrites an Anthropic /v1/messages body into the
// OpenAI shape the sendToZAI pipeline expects.
func anthropicToOpenAIRequest(bodyBytes []byte) ([]byte, error) {
	var req map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &req); err != nil {
		return nil, err
	}

	out := make(map[string]interface{})
	if m, ok := req["model"]; ok {
		out["model"] = m
	}
	if s, ok := req["stream"]; ok {
		out["stream"] = s
	}
	if mt, ok := req["max_tokens"]; ok {
		out["max_tokens"] = mt
	}
	if temp, ok := req["temperature"]; ok {
		out["temperature"] = temp
	}
	if tp, ok := req["top_p"]; ok {
		out["top_p"] = tp
	}
	if ss, ok := req["stop_sequences"]; ok {
		out["stop"] = ss
	}

	var messages []map[string]interface{}

	if sys, ok := req["system"]; ok {
		sysStr := extractAnthropicContent(sys)
		if sysStr != "" {
			messages = append(messages, map[string]interface{}{
				"role":    "system",
				"content": sysStr,
			})
		}
	}

	if msgs, ok := req["messages"].([]interface{}); ok {
		for _, m := range msgs {
			mm, ok := m.(map[string]interface{})
			if !ok {
				continue
			}
			role, _ := mm["role"].(string)
			content := mm["content"]

			// tool_result blocks become separate OpenAI tool messages.
			if arr, ok := content.([]interface{}); ok {
				hasToolResult := false
				for _, item := range arr {
					if mp, ok := item.(map[string]interface{}); ok {
						if t, _ := mp["type"].(string); t == "tool_result" {
							hasToolResult = true
							toolUseID, _ := mp["tool_use_id"].(string)
							resultContent := extractAnthropicContent(mp["content"])
							messages = append(messages, map[string]interface{}{
								"role":         "tool",
								"tool_call_id": toolUseID,
								"content":      resultContent,
							})
						}
					}
				}
				if hasToolResult {
					continue
				}
			}

			// Assistant tool_use blocks become OpenAI tool_calls.
			if role == "assistant" {
				if arr, ok := content.([]interface{}); ok {
					var textParts []string
					var toolCalls []map[string]interface{}
					for _, item := range arr {
						if mp, ok := item.(map[string]interface{}); ok {
							switch t, _ := mp["type"].(string); t {
							case "text":
								if txt, ok := mp["text"].(string); ok {
									textParts = append(textParts, txt)
								}
							case "tool_use":
								id, _ := mp["id"].(string)
								name, _ := mp["name"].(string)
								args := mp["input"]
								if args == nil {
									args = map[string]interface{}{}
								}
								argsJSON, _ := json.Marshal(args)
								toolCalls = append(toolCalls, map[string]interface{}{
									"id":   id,
									"type": "function",
									"function": map[string]interface{}{
										"name":      name,
										"arguments": string(argsJSON),
									},
								})
							}
						}
					}
					msg := map[string]interface{}{
						"role":    "assistant",
						"content": strings.Join(textParts, "\n"),
					}
					if len(toolCalls) > 0 {
						msg["tool_calls"] = toolCalls
					}
					messages = append(messages, msg)
					continue
				}
			}

			messages = append(messages, map[string]interface{}{
				"role":    role,
				"content": extractAnthropicContent(content),
			})
		}
	}
	out["messages"] = messages

	// Tools: input_schema becomes parameters, wrapped in a function object.
	if tools, ok := req["tools"].([]interface{}); ok && len(tools) > 0 {
		var openaiTools []map[string]interface{}
		for _, t := range tools {
			tm, ok := t.(map[string]interface{})
			if !ok {
				continue
			}
			name, _ := tm["name"].(string)
			desc, _ := tm["description"].(string)
			fn := map[string]interface{}{
				"name":        name,
				"description": desc,
			}
			if is, ok := tm["input_schema"]; ok {
				fn["parameters"] = is
			}
			openaiTools = append(openaiTools, map[string]interface{}{
				"type":     "function",
				"function": fn,
			})
		}
		out["tools"] = openaiTools
	}

	if thinking, ok := req["thinking"].(map[string]interface{}); ok {
		out["thinking"] = thinking
		if t, _ := thinking["type"].(string); t == "enabled" {
			out["reasoning"] = true
		}
	}

	return json.Marshal(out)
}

func thinkingEnabled(thinkingCfg json.RawMessage) bool {
	if len(thinkingCfg) == 0 {
		return false
	}
	var tc struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(thinkingCfg, &tc); err != nil {
		return false
	}
	return tc.Type == "enabled"
}

func anthropicMessagesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeJSON(w, 405, formatAnthropicError("invalid_request_error", "Method not allowed"))
		return
	}

	access := newAccessRecord(r.Method, r.URL.Path)
	defer access.done()

	limit := config.MaxRequestBytes
	if limit <= 0 {
		limit = 32 << 20
	}
	bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		metrics.requestsRejected.Add(1)
		access.fail(400, "unreadable body")
		writeJSON(w, 400, formatAnthropicError("invalid_request_error", "Failed to read body"))
		return
	}
	if int64(len(bodyBytes)) > limit {
		metrics.requestsRejected.Add(1)
		access.fail(413, "body over MAX_REQUEST_BYTES")
		writeJSON(w, 413, formatAnthropicError("invalid_request_error", "Request body too large"))
		return
	}

	openaiBody, err := anthropicToOpenAIRequest(bodyBytes)
	if err != nil {
		access.fail(400, err.Error())
		writeJSON(w, 400, formatAnthropicError("invalid_request_error", "Invalid JSON: "+err.Error()))
		return
	}

	var body struct {
		Model           string          `json:"model"`
		Messages        json.RawMessage `json:"messages"`
		Stream          *bool           `json:"stream"`
		Reasoning       *bool           `json:"reasoning"`
		Thinking        json.RawMessage `json:"thinking"`
		Tools           json.RawMessage `json:"tools"`
		ReasoningEffort string          `json:"reasoning_effort"`
	}
	if err := json.Unmarshal(openaiBody, &body); err != nil {
		access.fail(400, "conversion: "+err.Error())
		writeJSON(w, 400, formatAnthropicError("invalid_request_error", "Conversion error: "+err.Error()))
		return
	}

	var anthReq struct {
		Stream   bool            `json:"stream"`
		Thinking json.RawMessage `json:"thinking"`
	}
	json.Unmarshal(bodyBytes, &anthReq)

	model := body.Model
	if model == "" {
		model = "glm-4.7"
	}
	access.model = model

	var messages []Message
	if err := json.Unmarshal(body.Messages, &messages); err != nil || len(messages) == 0 {
		access.fail(400, "messages missing or not an array")
		writeJSON(w, 400, formatAnthropicError("invalid_request_error", "messages is required and must be an array"))
		return
	}

	stream := anthReq.Stream
	access.stream = stream

	// Throwaway chat session, deleted on Z.AI once the response is processed.
	chatID, pooled, err := AcquireStatelessSession(r.Context())
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			access.fail(499, "client gone before start")
			return
		}
		access.fail(503, err.Error())
		writeJSON(w, 503, formatAnthropicError("api_error", err.Error()))
		return
	}
	defer ReleaseStatelessSession(chatID, pooled)
	requestId := "msg_" + generateID()

	var transformedMessages json.RawMessage = body.Messages
	if config.AgentMode {
		if tm, err := agentTransformMessages(body.Messages, body.Tools); err == nil {
			transformedMessages = tm
			var localMsgs []Message
			if err := json.Unmarshal(tm, &localMsgs); err == nil {
				messages = localMsgs
			}
		}
	}

	prompt := messagesToPrompt(messages)

	opts := SendOptions{
		Model:             model,
		ChatID:            chatID,
		ClientMessagesRaw: transformedMessages,
		ReasoningEffort:   body.ReasoningEffort,
	}

	if body.Reasoning != nil {
		opts.Thinking = body.Reasoning
	} else if len(body.Thinking) > 0 {
		enabled := thinkingEnabled(body.Thinking)
		opts.Thinking = &enabled
	}

	// Cancelling this tears down the upstream stream when the exchange is
	// abandoned or fails, instead of running it to completion.
	upstreamCtx, cancelUpstream := context.WithCancel(r.Context())
	defer cancelUpstream()

	metrics.requestsTotal.Add(1)
	if stream {
		anthropicStreamResponse(upstreamCtx, w, r, prompt, opts, model, requestId, access)
	} else {
		anthropicNonStreamResponse(upstreamCtx, w, prompt, opts, model, requestId, access)
	}
}

// anthropicStreamResponse converts a ZAIResult stream to Anthropic SSE events.
func anthropicStreamResponse(ctx context.Context, w http.ResponseWriter, r *http.Request, prompt string, opts SendOptions, model, requestId string, access *accessRecord) {
	metrics.requestsStreaming.Add(1)
	metrics.activeStreams.Add(1)
	defer metrics.activeStreams.Add(-1)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	sse := newSSEWriter(w)
	writeEvent := sse.event
	defer func() {
		access.bytesOut, access.chunks = sse.written()
	}()

	inputTokens := estimateTokens(prompt)

	writeEvent("message_start", map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id":            requestId,
			"type":          "message",
			"role":          "assistant",
			"content":       []interface{}{},
			"model":         model,
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]interface{}{
				"input_tokens":  inputTokens,
				"output_tokens": 0,
			},
		},
	})

	writeEvent("ping", map[string]interface{}{"type": "ping"})

	keepAliveStop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				writeEvent("ping", map[string]interface{}{"type": "ping"})
			case <-keepAliveStop:
				return
			}
		}
	}()

	blockIndex := -1
	currentBlockType := ""
	outputTokens := 0

	startBlock := func(bType string, extra map[string]interface{}) {
		blockIndex++
		currentBlockType = bType
		cb := map[string]interface{}{"type": bType}
		for k, v := range extra {
			cb[k] = v
		}
		writeEvent("content_block_start", map[string]interface{}{
			"type":          "content_block_start",
			"index":         blockIndex,
			"content_block": cb,
		})
	}

	stopBlock := func() {
		if currentBlockType != "" {
			writeEvent("content_block_stop", map[string]interface{}{
				"type":  "content_block_stop",
				"index": blockIndex,
			})
			currentBlockType = ""
		}
	}

	emitBlockDelta := func(deltaType, field, text string) {
		writeEvent("content_block_delta", map[string]interface{}{
			"type":  "content_block_delta",
			"index": blockIndex,
			"delta": map[string]interface{}{"type": deltaType, field: text},
		})
		outputTokens += estimateTokens(text)
	}

	emitText := func(text string) {
		if text == "" {
			return
		}
		if currentBlockType != "text" {
			stopBlock()
			startBlock("text", map[string]interface{}{"text": ""})
		}
		emitBlockDelta("text_delta", "text", text)
	}

	emitThinking := func(text string) {
		if currentBlockType != "thinking" {
			stopBlock()
			startBlock("thinking", map[string]interface{}{"thinking": ""})
		}
		emitBlockDelta("thinking_delta", "thinking", text)
	}

	toolCallEmitted := false

	// A delta carrying an id starts a new tool_use block; an id-less delta only
	// appends partial arguments JSON. The modern shim emits one complete call
	// per delta, the legacy shim streams a header then argument fragments.
	emitToolCallEvent := func(tc map[string]interface{}) {
		fn, _ := tc["function"].(map[string]interface{})
		argsStr, _ := fn["arguments"].(string)

		emitArgs := func() {
			if argsStr == "" {
				return
			}
			writeEvent("content_block_delta", map[string]interface{}{
				"type":  "content_block_delta",
				"index": blockIndex,
				"delta": map[string]interface{}{
					"type":         "input_json_delta",
					"partial_json": argsStr,
				},
			})
		}

		if id, ok := tc["id"].(string); ok && id != "" {
			stopBlock()
			name, _ := fn["name"].(string)
			startBlock("tool_use", map[string]interface{}{
				"id":    "toolu_" + strings.TrimPrefix(id, "call_"),
				"name":  name,
				"input": map[string]interface{}{},
			})
			toolCallEmitted = true
		}
		emitArgs()
	}

	var interceptor agentInterceptor
	if config.AgentMode {
		interceptor = newAgentInterceptor()
	}

	var contentBuf strings.Builder
	contentSeeded := false
	fullContent := ""

	ch, err := sendToZAI(ctx, prompt, opts)
	if err != nil {
		close(keepAliveStop)
		wg.Wait()
		metrics.requestsFailed.Add(1)
		access.fail(statusFromError(err.Error()), err.Error())
		writeEvent("error", formatAnthropicError("api_error", err.Error()))
		writeEvent("message_stop", map[string]interface{}{"type": "message_stop"})
		return
	}

	for result := range ch {
		if r != nil && r.Context().Err() != nil {
			metrics.clientAborts.Add(1)
			logInfof("[Anthropic] client disconnected, abandoning request %s", requestId)
			access.fail(499, "client disconnected")
			stopBlock()
			close(keepAliveStop)
			wg.Wait()
			return
		}
		if result.Err != nil {
			stopBlock()
			close(keepAliveStop)
			wg.Wait()
			metrics.requestsFailed.Add(1)
			access.fail(statusFromError(result.Err.Error()), result.Err.Error())
			writeEvent("error", formatAnthropicError("api_error", result.Err.Error()))
			writeEvent("message_stop", map[string]interface{}{"type": "message_stop"})
			return
		}

		if result.Reasoning != "" {
			emitThinking(result.Reasoning)
			continue
		}

		if result.FullText != "" && !strings.HasPrefix(result.FullText, fullContent) {
			// A deep edit_content rewrite rewound text that was already
			// forwarded: reset the stale agent interceptor (issue #23).
			if interceptor != nil {
				interceptor = newAgentInterceptor()
			}
		}
		if result.FullText != "" {
			fullContent = result.FullText
			contentSeeded = false
		} else if result.Chunk != "" {
			if !contentSeeded {
				contentBuf.Reset()
				contentBuf.WriteString(fullContent)
				contentSeeded = true
			}
			contentBuf.WriteString(result.Chunk)
			fullContent = contentBuf.String()
		}

		// The parser emits the exact rune-safe delta to forward.
		delta := result.Chunk
		if delta == "" {
			continue
		}

		if interceptor != nil {
			contentDelta, toolCalls := interceptor.feed(delta)
			emitText(contentDelta)
			for _, tc := range toolCalls {
				emitToolCallEvent(tc)
			}
		} else {
			emitText(delta)
		}
	}

	// Drain the interceptor tail: trailing text plus any tool call whose
	// block only completed at end of stream (modern shim hold-back window).
	if interceptor != nil {
		rem, tailCalls := interceptor.finish()
		if !toolCallEmitted {
			emitText(rem)
		}
		for _, tc := range tailCalls {
			emitToolCallEvent(tc)
		}

		// Safety net: fallback tool call extraction at stream end
		if !toolCallEmitted {
			for _, tc := range agentExtractToolCalls(fullContent) {
				emitToolCallEvent(tc)
			}
		}
	}

	stopBlock()

	close(keepAliveStop)
	wg.Wait()

	stopReason := "end_turn"
	if toolCallEmitted {
		stopReason = "tool_use"
	}

	writeEvent("message_delta", map[string]interface{}{
		"type": "message_delta",
		"delta": map[string]interface{}{
			"stop_reason":   stopReason,
			"stop_sequence": nil,
		},
		"usage": map[string]interface{}{
			"output_tokens": outputTokens,
		},
	})

	writeEvent("message_stop", map[string]interface{}{"type": "message_stop"})
}

// anthropicNonStreamResponse produces a single Anthropic message object.
func anthropicNonStreamResponse(ctx context.Context, w http.ResponseWriter, prompt string, opts SendOptions, model, requestId string, access *accessRecord) {
	ch, err := sendToZAI(ctx, prompt, opts)
	if err != nil {
		metrics.requestsFailed.Add(1)
		access.fail(statusFromError(err.Error()), err.Error())
		writeJSON(w, statusFromError(err.Error()), formatAnthropicError("api_error", err.Error()))
		return
	}

	var contentBuf, reasoningBuf strings.Builder
	contentSeeded := false
	fullContent := ""
	for result := range ch {
		if result.Err != nil {
			metrics.requestsFailed.Add(1)
			access.fail(statusFromError(result.Err.Error()), result.Err.Error())
			writeJSON(w, statusFromError(result.Err.Error()), formatAnthropicError("api_error", result.Err.Error()))
			return
		}
		if result.Reasoning != "" {
			reasoningBuf.WriteString(result.Reasoning)
			continue
		}
		if result.FullText != "" {
			fullContent = result.FullText
			contentSeeded = false
		} else if result.Chunk != "" {
			if !contentSeeded {
				contentBuf.Reset()
				contentBuf.WriteString(fullContent)
				contentSeeded = true
			}
			contentBuf.WriteString(result.Chunk)
			fullContent = contentBuf.String()
		}
	}
	fullReasoning := reasoningBuf.String()

	content := []interface{}{}
	stopReason := "end_turn"

	if fullReasoning != "" {
		content = append(content, map[string]interface{}{
			"type":     "thinking",
			"thinking": fullReasoning,
		})
	}

	if config.AgentMode {
		toolCalls := agentExtractToolCalls(fullContent)
		if len(toolCalls) > 0 {
			stripped := agentStripToolCalls(fullContent)
			if stripped != "" {
				content = append(content, map[string]interface{}{
					"type": "text",
					"text": stripped,
				})
			}
			for _, tc := range toolCalls {
				fn, _ := tc["function"].(map[string]interface{})
				name, _ := fn["name"].(string)
				argsStr, _ := fn["arguments"].(string)
				id, _ := tc["id"].(string)
				tooluID := "toolu_" + strings.TrimPrefix(id, "call_")
				var args interface{}
				json.Unmarshal([]byte(argsStr), &args)
				if args == nil {
					args = map[string]interface{}{}
				}
				content = append(content, map[string]interface{}{
					"type":  "tool_use",
					"id":    tooluID,
					"name":  name,
					"input": args,
				})
			}
			stopReason = "tool_use"
		} else {
			content = append(content, map[string]interface{}{
				"type": "text",
				"text": fullContent,
			})
		}
	} else {
		content = append(content, map[string]interface{}{
			"type": "text",
			"text": fullContent,
		})
	}

	writeJSON(w, 200, map[string]interface{}{
		"id":            requestId,
		"type":          "message",
		"role":          "assistant",
		"content":       content,
		"model":         model,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage": map[string]interface{}{
			"input_tokens":  estimateTokens(prompt),
			"output_tokens": estimateTokens(fullContent),
		},
	})
}
