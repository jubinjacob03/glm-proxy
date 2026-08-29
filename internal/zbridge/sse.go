package zbridge

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"
)

var sseBufPool = sync.Pool{
	New: func() interface{} { return bytes.NewBuffer(make([]byte, 0, 4096)) },
}

// sseWriter serialises writes to one client and renders each event into a pooled
// buffer, so a chunk costs one syscall and no intermediate string.
type sseWriter struct {
	mu      sync.Mutex
	w       io.Writer
	flusher http.Flusher
	bytes   int64
	events  int
}

func newSSEWriter(w http.ResponseWriter) *sseWriter {
	f, _ := w.(http.Flusher)
	return &sseWriter{w: w, flusher: f}
}

// data writes an unnamed `data:` event carrying v as JSON (the OpenAI form).
func (s *sseWriter) data(v interface{}) {
	buf := sseBufPool.Get().(*bytes.Buffer)
	buf.Reset()
	buf.WriteString("data: ")
	if err := json.NewEncoder(buf).Encode(v); err != nil {
		sseBufPool.Put(buf)
		return
	}
	buf.WriteByte('\n') // Encode wrote the first newline already
	s.flush(buf)
}

// event writes a named event, which is the Anthropic form.
func (s *sseWriter) event(name string, v interface{}) {
	buf := sseBufPool.Get().(*bytes.Buffer)
	buf.Reset()
	buf.WriteString("event: ")
	buf.WriteString(name)
	buf.WriteString("\ndata: ")
	if err := json.NewEncoder(buf).Encode(v); err != nil {
		sseBufPool.Put(buf)
		return
	}
	buf.WriteByte('\n')
	s.flush(buf)
}

// raw writes a pre-rendered payload, such as the [DONE] sentinel.
func (s *sseWriter) raw(payload string) {
	buf := sseBufPool.Get().(*bytes.Buffer)
	buf.Reset()
	buf.WriteString("data: ")
	buf.WriteString(payload)
	buf.WriteString("\n\n")
	s.flush(buf)
}

func (s *sseWriter) flush(buf *bytes.Buffer) {
	s.mu.Lock()
	n, _ := s.w.Write(buf.Bytes())
	if s.flusher != nil {
		s.flusher.Flush()
	}
	s.bytes += int64(n)
	s.events++
	s.mu.Unlock()
	metrics.sseBytesOut.Add(int64(n))
	sseBufPool.Put(buf)
}

// written reports bytes and event count for the access summary line.
func (s *sseWriter) written() (int64, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bytes, s.events
}

// Pointer fields distinguish "absent" from "empty": a content delta must carry
// "content":"" while a reasoning delta must not carry a content key at all.

type oaDelta struct {
	Role             string        `json:"role,omitempty"`
	Content          *string       `json:"content,omitempty"`
	ReasoningContent *string       `json:"reasoning_content,omitempty"`
	ToolCalls        []interface{} `json:"tool_calls,omitempty"`
}

type oaMessage struct {
	Role             string        `json:"role"`
	Content          string        `json:"content"`
	ReasoningContent string        `json:"reasoning_content,omitempty"`
	ToolCalls        []interface{} `json:"tool_calls,omitempty"`
}

type oaUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type oaChoice struct {
	Index        int        `json:"index"`
	Delta        *oaDelta   `json:"delta,omitempty"`
	Message      *oaMessage `json:"message,omitempty"`
	FinishReason *string    `json:"finish_reason"`
}

type oaChunk struct {
	ID      string     `json:"id"`
	Object  string     `json:"object"`
	Created int64      `json:"created"`
	Model   string     `json:"model"`
	Choices []oaChoice `json:"choices"`
	Usage   *oaUsage   `json:"usage,omitempty"`
}

func newOAChunk(model, requestID string, delta *oaDelta, finishReason *string) oaChunk {
	return oaChunk{
		ID:      "chatcmpl-" + requestID,
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []oaChoice{{Index: 0, Delta: delta, FinishReason: finishReason}},
	}
}

func oaContentDelta(model, requestID, content string) oaChunk {
	return newOAChunk(model, requestID, &oaDelta{Content: &content}, nil)
}

func oaReasoningDelta(model, requestID, reasoning string) oaChunk {
	return newOAChunk(model, requestID, &oaDelta{ReasoningContent: &reasoning}, nil)
}

func oaToolCallDelta(model, requestID string, call map[string]interface{}) oaChunk {
	return newOAChunk(model, requestID, &oaDelta{ToolCalls: []interface{}{call}}, nil)
}

// oaRoleInit opens a stream. OpenAI announces the assistant role in the first
// delta, and strict clients (TRAE included) need it before any content arrives.
func oaRoleInit(model, requestID string) oaChunk {
	return newOAChunk(model, requestID, &oaDelta{Role: "assistant"}, nil)
}

func oaStopChunk(model, requestID string) oaChunk {
	empty, reason := "", "stop"
	return newOAChunk(model, requestID, &oaDelta{Content: &empty}, &reason)
}

func oaToolCallsStopChunk(model, requestID string) oaChunk {
	reason := "tool_calls"
	return newOAChunk(model, requestID, &oaDelta{}, &reason)
}
