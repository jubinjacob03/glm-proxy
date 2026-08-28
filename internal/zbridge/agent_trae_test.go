package zbridge

// Regression tests for the two capabilities an agentic IDE (TRAE) exercises
// hardest through the OpenAI protocol: image attachments and tool calls.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ============================================================================
// VISION — image parts must survive the agent-mode fold
// ============================================================================

const visionMessages = `[
  {"role":"system","content":"You are a coding assistant."},
  {"role":"user","content":[
    {"type":"text","text":"What is in this screenshot?"},
    {"type":"image_url","image_url":{"url":"data:image/png;base64,AAAABBBBCCCC"}}
  ]}
]`

const textOnlyMessages = `[
  {"role":"user","content":"just text, no image"}
]`

func assertCarriesImage(t *testing.T, payload []byte) {
	t.Helper()
	var msgs []map[string]interface{}
	if err := json.Unmarshal(payload, &msgs); err != nil {
		t.Fatalf("payload is not a messages array: %v\n%s", err, payload)
	}
	if len(msgs) == 0 {
		t.Fatal("transformed payload has no messages")
	}
	content := msgs[len(msgs)-1]["content"] // the image rides the last user message
	arr, ok := content.([]interface{})
	if !ok {
		t.Fatalf("expected content array carrying the image, got %T: %s", content, payload)
	}
	sawText, sawImage := false, false
	for _, p := range arr {
		m, ok := p.(map[string]interface{})
		if !ok {
			continue
		}
		switch m["type"] {
		case "text":
			sawText = true
		case "image_url":
			sawImage = true
			iu, _ := m["image_url"].(map[string]interface{})
			if url, _ := iu["url"].(string); !strings.HasPrefix(url, "data:image/png;base64,") {
				t.Errorf("image url not preserved verbatim: %v", iu["url"])
			}
		}
	}
	if !sawText || !sawImage {
		t.Errorf("content array missing text=%v image=%v: %s", sawText, sawImage, payload)
	}
}

func TestVisionModernShimPreservesImage(t *testing.T) {
	out, err := transformMessagesForAgentModern(json.RawMessage(visionMessages), nil)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	assertCarriesImage(t, out)
}

func TestVisionLegacyShimPreservesImage(t *testing.T) {
	var tools []interface{}
	out, err := transformMessagesForAgent(json.RawMessage(visionMessages), tools)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	assertCarriesImage(t, out)
}

func TestVisionTextOnlyStaysString(t *testing.T) {
	out, err := transformMessagesForAgentModern(json.RawMessage(textOnlyMessages), nil)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	var msgs []map[string]interface{}
	if err := json.Unmarshal(out, &msgs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, isString := msgs[0]["content"].(string); !isString {
		t.Errorf("text-only request must keep string content, got %T", msgs[0]["content"])
	}
}

func TestExtractImagePartsOrder(t *testing.T) {
	msgs := `[
	  {"role":"user","content":[{"type":"image_url","image_url":{"url":"one"}}]},
	  {"role":"assistant","content":"ok"},
	  {"role":"user","content":[{"type":"text","text":"and"},{"type":"image_url","image_url":{"url":"two"}}]}
	]`
	got := extractImageParts(json.RawMessage(msgs))
	if len(got) != 2 {
		t.Fatalf("expected 2 image parts, got %d", len(got))
	}
	if !strings.Contains(string(got[0]), `"one"`) || !strings.Contains(string(got[1]), `"two"`) {
		t.Errorf("image parts out of order: %s | %s", got[0], got[1])
	}
}

// ============================================================================
// TOOL CALLS — streamed shape an OpenAI client assembles
// ============================================================================

// TestToolCallStreamShape drives the real OpenAI handler against a mock Z.AI
// upstream that emits a modern-shim tool-call block, then asserts the streamed
// chunks carry everything an OpenAI-compatible agent client needs: an opening
// role, and tool_call deltas with index, id, type, and valid-JSON arguments,
// closed by finish_reason=tool_calls.
func TestToolCallStreamShape(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		block := "<<<TOOL_CALL>>>\n" +
			`{"name":"read_file","arguments":{"path":"main.go"}}` +
			"\n<<<END_TOOL_CALL>>>"
		for _, ev := range []string{
			`{"data":{"delta_content":"Let me read that file."}}`,
			fmt.Sprintf(`{"data":{"delta_content":%s}}`, mustJSONStr(block)),
			`{"data":{"phase":"done"}}`,
		} {
			fmt.Fprintf(w, "data: %s\n\n", ev)
			if fl != nil {
				fl.Flush()
			}
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	oldBase := BASE_URL
	BASE_URL = upstream.URL
	defer func() { BASE_URL = oldBase }()

	defer OverrideSessionState("test-token", "test-user", true)()

	cfg := GetConfig()
	oldAgent, oldAuth := cfg.AgentMode, cfg.Auth.Enabled
	cfg.AgentMode, cfg.Auth.Enabled = true, false
	defer func() { cfg.AgentMode, cfg.Auth.Enabled = oldAgent, oldAuth }()
	SeedCaptchaParam("test-captcha")

	body := `{"model":"glm-4.7","stream":true,` +
		`"tools":[{"type":"function","function":{"name":"read_file","description":"Read a file","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}],` +
		`"messages":[{"role":"user","content":"read main.go"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	NewHandler().ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status %d\n%s", rec.Code, rec.Body.String())
	}

	var (
		firstDeltaRole string
		toolName       string
		toolArgs       string
		toolID         string
		toolType       string
		sawIndex       bool
		finish         string
		firstSet       bool
	)
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") || line == "data: [DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Role      string `json:"role"`
					ToolCalls []struct {
						Index    *int   `json:"index"`
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(line[6:]), &chunk) != nil || len(chunk.Choices) == 0 {
			continue
		}
		ch := chunk.Choices[0]
		if !firstSet {
			firstDeltaRole = ch.Delta.Role
			firstSet = true
		}
		for _, tc := range ch.Delta.ToolCalls {
			if tc.Index != nil {
				sawIndex = true
			}
			if tc.Function.Name != "" {
				toolName = tc.Function.Name
			}
			if tc.Function.Arguments != "" {
				toolArgs = tc.Function.Arguments
			}
			if tc.ID != "" {
				toolID = tc.ID
			}
			if tc.Type != "" {
				toolType = tc.Type
			}
		}
		if ch.FinishReason != "" {
			finish = ch.FinishReason
		}
	}

	if firstDeltaRole != "assistant" {
		t.Errorf("first delta role = %q, want assistant", firstDeltaRole)
	}
	if toolName != "read_file" {
		t.Errorf("tool name = %q, want read_file", toolName)
	}
	if !sawIndex {
		t.Error("no tool_call carried an index (OpenAI clients need it to assemble)")
	}
	if toolID == "" {
		t.Error("tool_call missing id")
	}
	if toolType != "function" {
		t.Errorf("tool_call type = %q, want function", toolType)
	}
	if !json.Valid([]byte(toolArgs)) {
		t.Errorf("tool arguments not valid JSON: %q", toolArgs)
	}
	if !strings.Contains(toolArgs, "main.go") {
		t.Errorf("tool arguments lost the path: %q", toolArgs)
	}
	if finish != "tool_calls" {
		t.Errorf("finish_reason = %q, want tool_calls", finish)
	}
}

func mustJSONStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
