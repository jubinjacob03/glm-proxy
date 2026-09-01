package zbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// errBodyTooLarge lets callers answer 413 instead of blaming the client's JSON.
var errBodyTooLarge = errors.New("request body too large")

// decodeRequestBody parses a JSON body into dst under the configured size cap.
func decodeRequestBody(r *http.Request, dst interface{}) error {
	limit := config.MaxRequestBytes
	if limit <= 0 {
		limit = 32 << 20
	}
	if r.ContentLength > limit {
		return errBodyTooLarge
	}
	// One byte past the limit: an exhausted reader proves oversize, not truncation.
	lr := &io.LimitedReader{R: r.Body, N: limit + 1}
	if err := json.NewDecoder(lr).Decode(dst); err != nil {
		if lr.N <= 0 {
			return errBodyTooLarge
		}
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return errors.New("request body is empty or truncated")
		}
		return errors.New("invalid JSON")
	}
	return nil
}

// statusForDecodeError maps a decode failure to its HTTP status.
func statusForDecodeError(err error) int {
	if errors.Is(err, errBodyTooLarge) {
		return http.StatusRequestEntityTooLarge
	}
	return http.StatusBadRequest
}

func chatCompletionsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	access := newAccessRecord(r.Method, r.URL.Path)
	defer access.done()

	var body struct {
		Model           string          `json:"model"`
		Messages        json.RawMessage `json:"messages"`
		Stream          *bool           `json:"stream"`
		Reasoning       *bool           `json:"reasoning"`
		Thinking        json.RawMessage `json:"thinking"`
		WebSearch       *bool           `json:"webSearch"`
		Search          *bool           `json:"search"`
		Tools           json.RawMessage `json:"tools"`
		ToolChoice      json.RawMessage `json:"tool_choice"`
		ReasoningEffort string          `json:"reasoning_effort"`
	}
	if err := decodeRequestBody(r, &body); err != nil {
		status := statusForDecodeError(err)
		metrics.requestsRejected.Add(1)
		access.fail(status, err.Error())
		writeJSON(w, status, formatOpenAIError(err.Error(), "invalid_request_error", nil))
		return
	}

	metrics.requestsTotal.Add(1)

	model := body.Model
	if model == "" {
		model = "glm-4.7"
	}
	access.model = model

	var messages []Message
	if err := json.Unmarshal(body.Messages, &messages); err != nil || len(messages) == 0 {
		access.fail(400, "messages missing or not an array")
		writeJSON(w, 400, formatOpenAIError("messages is required and must be an array", "invalid_request_error", nil))
		return
	}

	stream := true
	if body.Stream != nil {
		stream = *body.Stream
	}
	access.stream = stream

	imageParts := extractImageParts(body.Messages)
	if len(imageParts) > maxImagesPerRequest {
		msg := fmt.Sprintf("too many images: %d provided, limit is %d per request",
			len(imageParts), maxImagesPerRequest)
		access.fail(400, msg)
		writeJSON(w, 400, formatOpenAIError(msg, "invalid_request_error", nil))
		return
	}

	// Throwaway chat, deleted upstream once the response is processed.
	chatID, pooled, err := AcquireStatelessSession(r.Context())
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			access.fail(499, "client gone before start")
			return
		}
		access.fail(503, err.Error())
		writeJSON(w, 503, formatOpenAIError(err.Error(), "server_error", "shutting_down"))
		return
	}
	defer ReleaseStatelessSession(chatID, pooled)
	requestId := generateID()

	// Rewrites tools and non-user roles into a form Z.AI accepts.
	var transformedMessages json.RawMessage = body.Messages
	if config.AgentMode {
		if tm, err := agentTransformMessages(body.Messages, body.Tools); err == nil {
			transformedMessages = tm
			var localMsgs []Message
			if err := json.Unmarshal(tm, &localMsgs); err == nil {
				messages = localMsgs
			}
		} else {
			logError("agent transform failed: " + err.Error())
		}
	}

	prompt := messagesToPrompt(messages)

	// signature_prompt = the last user message of what is actually sent.
	signaturePrompt := lastUserPromptText(messages)

	upstreamModel := model
	if len(imageParts) > 0 && !modelSupportsVision(model) {
		if vm := resolveVisionModel(model); vm != "" {
			logConsolef("[Vision] %s cannot accept images; routing this request to %s", printableASCII(model), vm)
			upstreamModel = vm
		} else {
			logErrorf("[Vision] %s cannot accept images and no vision model is available", printableASCII(model))
		}
	}

	// Features resolve per-model inside sendToZAI; only explicit body fields
	// override per request.
	opts := SendOptions{
		Model:             upstreamModel,
		ChatID:            chatID,
		ClientMessagesRaw: transformedMessages,
		ReasoningEffort:   body.ReasoningEffort,
		SignaturePrompt:   signaturePrompt,
	}

	// Both `reasoning: bool` and `thinking: {type: ...}` map onto enable_thinking.
	if body.Reasoning != nil {
		opts.Thinking = body.Reasoning
	} else if len(body.Thinking) > 0 {
		var thinkCfg struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(body.Thinking, &thinkCfg); err == nil {
			enabled := thinkCfg.Type == "enabled"
			opts.Thinking = &enabled
		}
	}

	if body.WebSearch != nil {
		opts.WebSearch = body.WebSearch
	} else if body.Search != nil {
		opts.WebSearch = body.Search
	}

	// Cancelling tears down the upstream request and its producer goroutine, so
	// no exit path leaves either running.
	upstreamCtx, cancelUpstream := context.WithCancel(r.Context())
	defer cancelUpstream()

	if stream {
		metrics.requestsStreaming.Add(1)
		metrics.activeStreams.Add(1)
		defer metrics.activeStreams.Add(-1)

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")

		sse := newSSEWriter(w)
		defer func() {
			access.bytesOut, access.chunks = sse.written()
		}()
		sse.data(oaRoleInit(model, requestId))

		var contentBuf strings.Builder
		var reasoningBuf strings.Builder
		contentSeeded := false
		fullContent := ""

		var interceptor agentInterceptor
		if config.AgentMode {
			interceptor = newAgentInterceptor(body.Tools)
		}
		toolCallEmitted := false
		toolCallSeq := 0

		// Every streamed tool_call needs index, id and type "function" or
		// assembling clients choke. The interceptors set these; the
		// end-of-stream fallback extractor does not.
		emitToolCallDelta := func(tc map[string]interface{}) {
			if tc == nil {
				return
			}
			if _, ok := tc["index"]; !ok {
				tc["index"] = toolCallSeq
				toolCallSeq++
			}
			if id, _ := tc["id"].(string); id == "" {
				tc["id"] = "call_" + agentRandomHex(12)
			}
			if _, ok := tc["type"]; !ok {
				tc["type"] = "function"
			}
			sse.data(oaToolCallDelta(model, requestId, tc))
		}

		keepAliveStop := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer recoverGoroutine("openai keep-alive")
			ticker := time.NewTicker(5 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					sse.data(oaContentDelta(model, requestId, ""))
				case <-keepAliveStop:
					return
				}
			}
		}()
		var keepAliveOnce sync.Once
		stopKeepAlive := func() {
			keepAliveOnce.Do(func() {
				close(keepAliveStop)
				wg.Wait()
			})
		}

		errored := false
		ch, err := sendToZAI(upstreamCtx, prompt, opts)
		if err != nil {
			logErrorf("[Stream] %s", printableASCII(err.Error()))
			metrics.requestsFailed.Add(1)
			access.fail(statusFromError(err.Error()), err.Error())
			stopKeepAlive()
			sse.data(formatOpenAIError(err.Error(), "api_error", statusFromError(err.Error())))
			sse.raw("[DONE]")
			errored = true
		} else {
			for result := range ch {
				if r.Context().Err() != nil {
					metrics.clientAborts.Add(1)
					logInfof("[Stream] client disconnected, abandoning request %s", requestId)
					access.fail(499, "client disconnected")
					cancelUpstream()
					errored = true
					break
				}
				if result.Err != nil {
					logErrorf("[Stream] %s", printableASCII(result.Err.Error()))
					metrics.requestsFailed.Add(1)
					access.fail(statusFromError(result.Err.Error()), result.Err.Error())
					stopKeepAlive()
					sse.data(formatOpenAIError(result.Err.Error(), "api_error", statusFromError(result.Err.Error())))
					sse.raw("[DONE]")
					errored = true
					break
				}

				if result.Reasoning != "" {
					reasoningBuf.WriteString(result.Reasoning)
					sse.data(oaReasoningDelta(model, requestId, result.Reasoning))
					continue
				}
				if result.FullText != "" && !strings.HasPrefix(result.FullText, fullContent) {
					// A deep edit_content rewrite rewound already-forwarded text,
					// so the interceptor's view is stale; reset it (issue #23).
					if interceptor != nil {
						interceptor = newAgentInterceptor(body.Tools)
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

				// The parser already emitted a rune-safe delta.
				delta := result.Chunk
				if delta == "" {
					continue
				}

				if interceptor != nil {
					contentDelta, toolCalls := interceptor.feed(delta)
					if contentDelta != "" {
						sse.data(oaContentDelta(model, requestId, contentDelta))
					}
					for _, tc := range toolCalls {
						emitToolCallDelta(tc)
						toolCallEmitted = true
					}
				} else {
					sse.data(oaContentDelta(model, requestId, delta))
				}
			}
		}

		if !errored {
			if interceptor != nil {
				// Drain the tail, then strip again: a raw block can never leak if
				// the safety net below is what parses it, and prose is preserved.
				rem, tailCalls := interceptor.finish()
				if rem != "" {
					if clean := agentStripToolCalls(rem, body.Tools); clean != "" {
						sse.data(oaContentDelta(model, requestId, clean))
					}
				}
				for _, tc := range tailCalls {
					emitToolCallDelta(tc)
					toolCallEmitted = true
				}

				// Safety net: re-scan the whole text so a held block becomes a call.
				if !toolCallEmitted {
					fallbackCalls := agentExtractToolCalls(fullContent, body.Tools)
					if len(fallbackCalls) > 0 {
						for _, tc := range fallbackCalls {
							emitToolCallDelta(tc)
						}
						toolCallEmitted = true
					}
				}
			}

			stopKeepAlive()
			if toolCallEmitted {
				sse.data(oaToolCallsStopChunk(model, requestId))
			} else {
				sse.data(oaStopChunk(model, requestId))
			}
			sse.raw("[DONE]")
		}

		stopKeepAlive()

	} else {
		ch, err := sendToZAI(upstreamCtx, prompt, opts)
		if err != nil {
			logErrorf("[API] %s", err.Error())
			metrics.requestsFailed.Add(1)
			access.fail(statusFromError(err.Error()), err.Error())
			writeJSON(w, statusFromError(err.Error()), formatOpenAIError(err.Error(), "api_error", nil))
			return
		}

		var contentBuf, reasoningBuf strings.Builder
		contentSeeded := false
		fullContent := ""
		for result := range ch {
			if result.Err != nil {
				logErrorf("[API] %s", result.Err.Error())
				metrics.requestsFailed.Add(1)
				access.fail(statusFromError(result.Err.Error()), result.Err.Error())
				writeJSON(w, statusFromError(result.Err.Error()), formatOpenAIError(result.Err.Error(), "api_error", nil))
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

		// Agent mode: lift tool-call blocks out of the finished text.
		if config.AgentMode {
			if toolCalls := agentExtractToolCalls(fullContent, body.Tools); len(toolCalls) > 0 {
				writeJSON(w, 200, formatOpenAIToolCallResponse(
					model, requestId, agentStripToolCalls(fullContent, body.Tools),
					fullReasoning, prompt, toolCalls))
				return
			}
		}

		writeJSON(w, 200, formatOpenAIResponse(
			ResponseResult{Content: fullContent, Reasoning: fullReasoning, Prompt: prompt},
			model, requestId, false))
	}
}
func featuresHandler(w http.ResponseWriter, r *http.Request) {
	// GET: resolved features for one model.
	if r.Method == "GET" {
		model := r.URL.Query().Get("model")
		if model != "" {
			resolved := resolveFeaturesForModel(model)
			state := snapshotModelFeatureState(model)
			caps := getModelCapabilities(model)
			writeJSON(w, 200, map[string]interface{}{
				"model":        model,
				"features":     resolved,
				"includeAll":   state.IncludeAll,
				"overrides":    state.Overrides,
				"capabilities": caps,
			})
			return
		}
		// No model given: return every per-model state.
		// Copied, not aliased: these maps are encoded after the unlock below.
		modelFeatureStatesMu.Lock()
		states := make(map[string]interface{}, len(modelFeatureStates))
		for k, v := range modelFeatureStates {
			snap := copyFeatureStateLocked(v)
			states[k] = map[string]interface{}{
				"includeAll": snap.IncludeAll,
				"overrides":  snap.Overrides,
			}
		}
		modelFeatureStatesMu.Unlock()
		writeJSON(w, 200, map[string]interface{}{
			"states": states,
		})
		return
	}

	if r.Method != "POST" {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// POST: update per-model feature state.

	// A raw map, to capture arbitrary capability keys. The shared decoder applies
	// MAX_REQUEST_BYTES; a bare io.ReadAll here was an OOM per request.
	var body map[string]interface{}
	if err := decodeRequestBody(r, &body); err != nil {
		status := statusForDecodeError(err)
		metrics.requestsRejected.Add(1)
		writeJSON(w, status, map[string]interface{}{"error": err.Error()})
		return
	}

	model, _ := body["model"].(string)
	if model == "" {
		writeJSON(w, 400, map[string]interface{}{"error": "model is required"})
		return
	}

	// Include-All-Features opts into forwarding every server capability.
	includeAllHeader := strings.EqualFold(r.Header.Get("Include-All-Features"), "true")

	// Scoped so the lock is always released — even on a panic in
	// getModelCapabilities — before the session.mu section below.
	resolved, includeAll, overrides := func() (map[string]interface{}, bool, map[string]interface{}) {
		modelFeatureStatesMu.Lock()
		defer modelFeatureStatesMu.Unlock()

		state, ok := modelFeatureStates[model]
		if !ok {
			state = &ModelFeatureState{
				IncludeAll: false,
				Overrides:  make(map[string]interface{}),
			}
			modelFeatureStates[model] = state
		}

		// Sticky: once set it stays until overwritten.
		if includeAllHeader {
			state.IncludeAll = true
		}

		// Every key but "model" is a feature override; reasoning and thinking are
		// aliased onto enable_thinking.
		for k, v := range body {
			if k == "model" {
				continue
			}

			// reasoning: bool maps to enable_thinking.
			if k == "reasoning" {
				if b, ok := v.(bool); ok {
					state.Overrides["enable_thinking"] = b
				}
				continue
			}

			// thinking: bool, or {"type":"enabled"|"disabled"}, same target.
			if k == "thinking" {
				if b, ok := v.(bool); ok {
					state.Overrides["enable_thinking"] = b
					continue
				}
				if m, ok := v.(map[string]interface{}); ok {
					if t, ok := m["type"].(string); ok {
						state.Overrides["enable_thinking"] = (t == "enabled")
					}
					continue
				}
				continue
			}

			// Everything else: camelCase to snake_case, no alias mapping.
			snakeKey := normalizeFeatureKey(k)
			// Always forced false on this endpoint.
			if snakeKey == "image_generation" {
				continue
			}
			// Not accepted; use enable_thinking, reasoning or thinking.
			if snakeKey == "think" {
				continue
			}
			// Per-request only, validated against model capabilities, so it is never
			// stored as a persistent override.
			if snakeKey == "reasoning_effort" {
				continue
			}
			state.Overrides[snakeKey] = v
		}

		// Resolve what the response will report.
		resolved := resolveFeaturesWithState(getModelCapabilities(model), state)
		overrides := make(map[string]interface{})
		for k, v := range state.Overrides {
			overrides[k] = v
		}
		return resolved, state.IncludeAll, overrides
	}()

	// Kept in sync for the dashboard's benefit.
	session.mu.Lock()
	if v, ok := resolved["auto_web_search"].(bool); ok {
		session.Features.WebSearch = v
		session.Features.AutoWebSearch = v
	}
	if v, ok := resolved["enable_thinking"].(bool); ok {
		session.Features.Thinking = v
	}
	if v, ok := resolved["preview_mode"].(bool); ok {
		session.Features.PreviewMode = v
	}
	session.Features.ImageGen = false
	session.mu.Unlock()

	logInfof("[Features] model=%s includeAll=%v overrides=%+v resolved=%+v",
		model, includeAll, overrides, resolved)

	writeJSON(w, 200, map[string]interface{}{
		"success":    true,
		"model":      model,
		"includeAll": includeAll,
		"overrides":  overrides,
		"features":   resolved,
	})
}

func statsHandler(w http.ResponseWriter, r *http.Request) {
	session.mu.Lock()
	initialized := session.Initialized
	session.mu.Unlock()

	totalClients := 0
	if initialized {
		totalClients = 1
	}

	writeJSON(w, 200, map[string]interface{}{
		"mode":         "direct",
		"totalClients": totalClients,
		"stats":        metrics.snapshot(),
	})
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	session.mu.Lock()
	sessionReady := session.Initialized
	session.mu.Unlock()

	// Device tokens are the real consumable: with none left no captcha parameter
	// can be produced and every completion fails.
	tokens := getTokenCount()
	tokensLow := tokens < config.TokenMonitor.MinTokens
	healthy := sessionReady && tokens > 0

	status := 200
	if !healthy {
		status = 503
	}
	writeJSON(w, status, map[string]interface{}{
		"healthy":         healthy,
		"mode":            "direct",
		"session":         sessionReady,
		"deviceTokens":    tokens,
		"deviceTokensLow": tokensLow,
		"activeStreams":   metrics.activeStreams.Load(),
		"uptimeSeconds":   int64(time.Since(metrics.startedAt).Seconds()),
	})
}

func clientsHandler(w http.ResponseWriter, r *http.Request) {
	session.mu.Lock()
	initialized := session.Initialized
	session.mu.Unlock()

	var clients []map[string]interface{}
	if initialized {
		clients = []map[string]interface{}{
			{"id": "session", "status": "idle"},
		}
	} else {
		clients = []map[string]interface{}{}
	}
	writeJSON(w, 200, map[string]interface{}{"clients": clients})
}

func injectHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"message":"Direct mode"}`))
}

func stopHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]interface{}{
		"success": true,
		"message": "Stop acknowledged",
	})
}
