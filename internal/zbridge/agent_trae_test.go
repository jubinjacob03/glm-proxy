package zbridge

// Regression tests for the two things an agentic IDE (TRAE) leans on hardest over
// the OpenAI protocol: image attachments and tool calls.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Vision: image parts must survive the agent-mode fold.

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

func TestExtractImagePartsLastTurnOnly(t *testing.T) {
	// Two user turns, each with an image. Only the current (last) turn's image is
	// attached; the earlier one is history, kept as text context in <recent>.
	msgs := `[
	  {"role":"user","content":[{"type":"image_url","image_url":{"url":"one"}}]},
	  {"role":"assistant","content":"ok"},
	  {"role":"user","content":[{"type":"text","text":"and"},{"type":"image_url","image_url":{"url":"two"}}]}
	]`
	got := extractImageParts(json.RawMessage(msgs))
	if len(got) != 1 {
		t.Fatalf("expected only the current turn's image, got %d", len(got))
	}
	if !strings.Contains(string(got[0]), `"two"`) {
		t.Errorf("expected the current-turn image, got %s", got[0])
	}
}

// The reported bug: an image sent in an earlier turn, then a plain "hi". No image
// may ride the new turn, or the model answers the stale image.
func TestExtractImagePartsIgnoresHistoryImage(t *testing.T) {
	msgs := `[
	  {"role":"user","content":[{"type":"text","text":"what is this?"},{"type":"image_url","image_url":{"url":"shot"}}]},
	  {"role":"assistant","content":"a screenshot"},
	  {"role":"user","content":"hi"}
	]`
	if got := extractImageParts(json.RawMessage(msgs)); len(got) != 0 {
		t.Fatalf("a text-only current turn must carry no image, got %d: %s", len(got), got)
	}
}

// End to end through the modern shim: a "hi" after an image turn must fold to a
// plain string message, not an image-carrying content array.
func TestAgentModernDropsHistoryImageOnNewTurn(t *testing.T) {
	msgs := `[
	  {"role":"user","content":[{"type":"text","text":"what is this?"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]},
	  {"role":"assistant","content":"a screenshot"},
	  {"role":"user","content":"hi"}
	]`
	out, err := transformMessagesForAgentModern(json.RawMessage(msgs), nil)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	var got []map[string]interface{}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, isString := got[len(got)-1]["content"].(string); !isString {
		t.Errorf("text-only current turn must fold to string content (no image), got %T", got[len(got)-1]["content"])
	}
}

// Tool calls: the streamed shape an OpenAI client assembles.

// TestToolCallStreamShape drives the real handler against a mock upstream emitting
// a modern-shim block, then asserts the chunks carry everything an OpenAI client
// needs: an opening role, then tool_call deltas with index, id, type and
// valid-JSON arguments, closed by finish_reason=tool_calls.
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
	defer func() { DrainSessionGC(); BASE_URL = oldBase }()

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

// Native mode: the default. Roles are preserved so the model answers only the
// latest turn (no re-answering history), while tool calls still round-trip.

func TestVisionNativePreservesImage(t *testing.T) {
	out, err := transformMessagesForAgentNative(json.RawMessage(visionMessages), nil)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	assertCarriesImage(t, out)
}

// The reported bug, under the native default: an image in an earlier turn then a
// plain "hi". The current turn must stay a plain string and the turns must not
// collapse into one folded message.
func TestAgentNativeDropsHistoryImageOnNewTurn(t *testing.T) {
	msgs := `[
	  {"role":"user","content":[{"type":"text","text":"what is this?"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]},
	  {"role":"assistant","content":"a screenshot"},
	  {"role":"user","content":"hi"}
	]`
	out, err := transformMessagesForAgentNative(json.RawMessage(msgs), nil)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	var got []map[string]interface{}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("native transform must preserve turns, got %d: %s", len(got), out)
	}
	for i, want := range []string{"user", "assistant", "user"} {
		if fmt.Sprint(got[i]["role"]) != want {
			t.Errorf("message %d role = %v, want %s", i, got[i]["role"], want)
		}
	}
	if got[2]["content"] != "hi" {
		t.Errorf("current turn content = %v, want hi", got[2]["content"])
	}
	if _, isString := got[0]["content"].(string); !isString {
		t.Errorf("history image turn must fold to text, got %T", got[0]["content"])
	}
}

// A full agent-loop transcript must survive with roles intact: the client
// persona in a system message, the assistant's prior tool call replayed in wire
// format, the tool result as a tagged user turn, and the fresh request last with
// the tool contract attached to it (not to the system message).
func TestAgentNativePreservesRolesAndTools(t *testing.T) {
	msgs := `[
	  {"role":"system","content":"You are Trae."},
	  {"role":"user","content":"read main.go"},
	  {"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"main.go\"}"}}]},
	  {"role":"tool","tool_call_id":"call_1","content":"package main"},
	  {"role":"user","content":"now summarise it"}
	]`
	tools := `[{"type":"function","function":{"name":"read_file","description":"Read a file","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}]`
	out, err := transformMessagesForAgentNative(json.RawMessage(msgs), json.RawMessage(tools))
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	var got []map[string]interface{}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) == 0 || fmt.Sprint(got[0]["role"]) != "system" {
		t.Fatalf("first message must be the client system message: %s", out)
	}
	sysText, _ := got[0]["content"].(string)
	if !strings.Contains(sysText, "You are Trae.") {
		t.Errorf("system message should carry the client persona: %q", sysText)
	}
	if strings.Contains(sysText, "<<<TOOL_CALL>>>") {
		t.Errorf("tool contract must not sit in the system message (backend ignores it there): %q", sysText)
	}
	roles := make([]string, 0, len(got)-1)
	for _, m := range got[1:] {
		roles = append(roles, fmt.Sprint(m["role"]))
	}
	if strings.Join(roles, ",") != "user,assistant,user,user" {
		t.Fatalf("roles after system = %v, want [user assistant user user]\n%s", roles, out)
	}
	if asst, _ := got[2]["content"].(string); !strings.Contains(asst, "<<<TOOL_CALL>>>") || !strings.Contains(asst, "read_file") {
		t.Errorf("assistant turn must replay the tool call: %q", got[2]["content"])
	}
	if tr, _ := got[3]["content"].(string); !strings.Contains(tr, "tool_result") || !strings.Contains(tr, "package main") {
		t.Errorf("tool result turn malformed: %q", got[3]["content"])
	}
	// The latest user turn carries the current task plus the tool contract.
	lastContent, _ := got[len(got)-1]["content"].(string)
	for _, want := range []string{"now summarise it", "read_file", "<<<TOOL_CALL>>>"} {
		if !strings.Contains(lastContent, want) {
			t.Errorf("latest user turn missing %q:\n%s", want, lastContent)
		}
	}
}

// Request-body + signature tracing: capture exactly what the proxy POSTs upstream
// and assert the wire contract — roles preserved, signature_prompt is the latest
// user turn only (not the folded conversation), and a signature is attached.
func TestNativeRequestBodySignatureTrace(t *testing.T) {
	var (
		mu      sync.Mutex
		gotBody []byte
		gotSig  string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		gotBody = b
		gotSig = r.Header.Get("x-signature")
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		for _, ev := range []string{
			`{"data":{"delta_content":"hello"}}`,
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
	defer func() { DrainSessionGC(); BASE_URL = oldBase }()
	defer OverrideSessionState("test-token", "test-user", true)()

	cfg := GetConfig()
	oldAgent, oldAuth, oldVariant := cfg.AgentMode, cfg.Auth.Enabled, cfg.AgentModeVariant
	cfg.AgentMode, cfg.Auth.Enabled, cfg.AgentModeVariant = true, false, "native"
	defer func() {
		cfg.AgentMode, cfg.Auth.Enabled, cfg.AgentModeVariant = oldAgent, oldAuth, oldVariant
	}()
	SeedCaptchaParam("test-captcha")

	body := `{"model":"glm-4.7","stream":true,` +
		`"tools":[{"type":"function","function":{"name":"read_file","description":"Read","parameters":{"type":"object"}}}],` +
		`"messages":[` +
		`{"role":"system","content":"You are Trae."},` +
		`{"role":"user","content":"what is 2+2?"},` +
		`{"role":"assistant","content":"4"},` +
		`{"role":"user","content":"hi"}` +
		`]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	NewHandler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	mu.Lock()
	defer mu.Unlock()
	if gotBody == nil {
		t.Fatal("upstream never received a request body")
	}

	var up struct {
		Messages        []map[string]interface{} `json:"messages"`
		SignaturePrompt string                   `json:"signature_prompt"`
	}
	if err := json.Unmarshal(gotBody, &up); err != nil {
		t.Fatalf("upstream body not JSON: %v\n%s", err, gotBody)
	}

	// signature_prompt is the latest user turn (which carries the tool contract),
	// not the folded history: it holds the current task, never earlier turns or
	// the system persona.
	if !strings.Contains(up.SignaturePrompt, "hi") {
		t.Errorf("signature_prompt should contain the latest task: %q", up.SignaturePrompt)
	}
	for _, leak := range []string{"You are Trae.", "2+2"} {
		if strings.Contains(up.SignaturePrompt, leak) {
			t.Errorf("signature_prompt leaked history/system %q: %q", leak, up.SignaturePrompt)
		}
	}

	// messages keep their roles instead of folding into one user message.
	if len(up.Messages) < 3 {
		t.Fatalf("messages collapsed, got %d: %s", len(up.Messages), gotBody)
	}
	if up.Messages[0]["role"] != "system" {
		t.Errorf("first upstream message role = %v, want system", up.Messages[0]["role"])
	}
	// The client persona stays in the system message; the tool contract does NOT
	// (chat.z.ai ignores tool instructions placed there).
	sysContent, _ := up.Messages[0]["content"].(string)
	if !strings.Contains(sysContent, "You are Trae.") {
		t.Errorf("system message should carry the client persona: %q", sysContent)
	}
	if strings.Contains(sysContent, "<<<TOOL_CALL>>>") {
		t.Errorf("tool contract must not sit in the system message: %q", sysContent)
	}
	// The tool contract rides the latest user turn, where the backend honors it.
	last := up.Messages[len(up.Messages)-1]
	if last["role"] != "user" {
		t.Errorf("last upstream message role = %v, want user", last["role"])
	}
	lastContent, _ := last["content"].(string)
	for _, want := range []string{"hi", "read_file", "<<<TOOL_CALL>>>"} {
		if !strings.Contains(lastContent, want) {
			t.Errorf("latest user turn missing %q:\n%s", want, lastContent)
		}
	}

	// A signature (hex HMAC-SHA256 = 64 chars) was attached over signature_prompt.
	if len(gotSig) != 64 {
		t.Errorf("x-signature = %q (len %d), want 64 hex chars", gotSig, len(gotSig))
	}
}

// Repeated, table-driven simulation of many Trae request shapes. Asserts the
// native transform always yields a valid messages array using only the roles the
// upstream accepts, and is byte-for-byte deterministic across repeated runs.
func TestAgentNativeTransformSimulations(t *testing.T) {
	cases := []struct {
		name     string
		messages string
		tools    string
	}{
		{"plain single user", `[{"role":"user","content":"hi"}]`, ``},
		{"multi turn chat", `[{"role":"user","content":"a"},{"role":"assistant","content":"b"},{"role":"user","content":"c"}]`, ``},
		{"system plus user", `[{"role":"system","content":"be nice"},{"role":"user","content":"hi"}]`, ``},
		{"image turn with tools", `[{"role":"user","content":[{"type":"text","text":"see"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]`, `[{"type":"function","function":{"name":"noop"}}]`},
		{"full tool loop", `[{"role":"system","content":"sys"},{"role":"user","content":"read"},{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"read_file","arguments":"{\"p\":\"x\"}"}}]},{"role":"tool","tool_call_id":"c1","content":"data"},{"role":"user","content":"go on"}]`, `[{"type":"function","function":{"name":"read_file"}}]`},
		{"consecutive tools", `[{"role":"user","content":"do"},{"role":"assistant","content":"","tool_calls":[{"id":"a","type":"function","function":{"name":"t","arguments":"{}"}},{"id":"b","type":"function","function":{"name":"t2","arguments":"{}"}}]},{"role":"tool","tool_call_id":"a","content":"r1"},{"role":"tool","tool_call_id":"b","content":"r2"},{"role":"user","content":"next"}]`, `[{"type":"function","function":{"name":"t"}}]`},
		{"assistant last no tools", `[{"role":"user","content":"hi"},{"role":"assistant","content":"hello"}]`, ``},
	}

	validRoles := map[string]bool{"system": true, "user": true, "assistant": true}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var toolsRaw json.RawMessage
			if c.tools != "" {
				toolsRaw = json.RawMessage(c.tools)
			}
			out, err := transformMessagesForAgentNative(json.RawMessage(c.messages), toolsRaw)
			if err != nil {
				t.Fatalf("transform: %v", err)
			}
			var msgs []map[string]interface{}
			if err := json.Unmarshal(out, &msgs); err != nil {
				t.Fatalf("output not a messages array: %v\n%s", err, out)
			}
			if len(msgs) == 0 {
				t.Fatal("empty output")
			}
			for i, m := range msgs {
				role, _ := m["role"].(string)
				if !validRoles[role] {
					t.Errorf("message %d role %q not in system/user/assistant", i, role)
				}
				if _, ok := m["content"]; !ok {
					t.Errorf("message %d missing content", i)
				}
			}
			for i := 0; i < 5; i++ {
				again, err := transformMessagesForAgentNative(json.RawMessage(c.messages), toolsRaw)
				if err != nil {
					t.Fatalf("transform repeat: %v", err)
				}
				if !bytes.Equal(out, again) {
					t.Fatalf("non-deterministic output on repeat %d:\n%s\n---\n%s", i, out, again)
				}
			}
		})
	}
}

// Trae drives tools in Anthropic's <function_calls><invoke> text format; the
// proxy must parse it into native OpenAI tool_calls and never leak it as text.

func TestParseFunctionCallsBasic(t *testing.T) {
	text := "Let me read that.\n<function_calls>\n<invoke name=\"Read\">\n<parameter name=\"file_path\">internal/zbridge/types.go</parameter>\n<parameter name=\"limit\">30</parameter>\n</invoke>\n</function_calls>"
	tools := json.RawMessage(`[{"type":"function","function":{"name":"Read","parameters":{"type":"object","properties":{"file_path":{"type":"string"},"limit":{"type":"integer"}}}}}]`)
	calls := ParseAgentToolCalls(text, tools)
	if len(calls) != 1 {
		t.Fatalf("want 1 call, got %d", len(calls))
	}
	fn := calls[0]["function"].(map[string]interface{})
	if fn["name"] != "Read" {
		t.Errorf("name=%v want Read", fn["name"])
	}
	args, _ := fn["arguments"].(string)
	if !json.Valid([]byte(args)) {
		t.Fatalf("arguments not valid JSON: %s", args)
	}
	var a map[string]interface{}
	_ = json.Unmarshal([]byte(args), &a)
	if a["file_path"] != "internal/zbridge/types.go" {
		t.Errorf("file_path=%v", a["file_path"])
	}
	if a["limit"] != float64(30) {
		t.Errorf("limit=%v want number 30", a["limit"])
	}
}

func TestParseFunctionCallsJSONArrayParam(t *testing.T) {
	text := `<function_calls><invoke name="TodoWrite"><parameter name="todos">[{"content":"x","id":"1","status":"pending"}]</parameter><parameter name="merge">false</parameter></invoke></function_calls>`
	tools := json.RawMessage(`[{"type":"function","function":{"name":"TodoWrite","parameters":{"type":"object","properties":{"todos":{"type":"array"},"merge":{"type":"boolean"}}}}}]`)
	calls := ParseAgentToolCalls(text, tools)
	if len(calls) != 1 {
		t.Fatalf("want 1, got %d", len(calls))
	}
	fn := calls[0]["function"].(map[string]interface{})
	if fn["name"] != "TodoWrite" {
		t.Errorf("name=%v", fn["name"])
	}
	var a map[string]interface{}
	_ = json.Unmarshal([]byte(fn["arguments"].(string)), &a)
	if todos, ok := a["todos"].([]interface{}); !ok || len(todos) != 1 {
		t.Errorf("todos not preserved as array: %v", a["todos"])
	}
	if a["merge"] != false {
		t.Errorf("merge=%v want bool false", a["merge"])
	}
}

func TestParseFunctionCallsMultiple(t *testing.T) {
	text := `<function_calls><invoke name="LS"><parameter name="path">.</parameter></invoke><invoke name="Grep"><parameter name="pattern">SendOptions</parameter></invoke></function_calls>`
	if calls := ParseAgentToolCalls(text, nil); len(calls) != 2 {
		t.Fatalf("want 2 calls, got %d", len(calls))
	}
}

func TestStripFunctionCalls(t *testing.T) {
	text := "before\n<function_calls><invoke name=\"Read\"><parameter name=\"file_path\">x</parameter></invoke></function_calls>\nafter"
	got := StripAgentToolCalls(text, nil)
	if strings.Contains(got, "function_calls") || strings.Contains(got, "invoke") {
		t.Errorf("residual tool markup: %q", got)
	}
	if !strings.Contains(got, "before") || !strings.Contains(got, "after") {
		t.Errorf("surrounding content lost: %q", got)
	}
}

// A <function_calls> block streamed in small chunks must not leak as content,
// and must be recoverable as a tool_call from the full text.
func TestInterceptorHoldsBackFunctionCalls(t *testing.T) {
	block := "Reading now.\n<function_calls>\n<invoke name=\"Read\">\n<parameter name=\"file_path\">main.go</parameter>\n</invoke>\n</function_calls>"
	in := &AgentStreamInterceptor{}
	var content strings.Builder
	for i := 0; i < len(block); i += 7 {
		end := i + 7
		if end > len(block) {
			end = len(block)
		}
		content.WriteString(in.Feed(block[i:end]).Content)
	}
	content.WriteString(in.Finish().Content)
	got := content.String()
	if strings.Contains(got, "<function_calls>") || strings.Contains(got, "<invoke") || strings.Contains(got, "<parameter") {
		t.Errorf("function_calls leaked as content: %q", got)
	}
	if !strings.Contains(got, "Reading now.") {
		t.Errorf("leading content dropped: %q", got)
	}
	if calls := ParseAgentToolCalls(block, nil); len(calls) != 1 {
		t.Fatalf("want 1 tool call from full text, got %d", len(calls))
	}
}

// Plain prose that merely mentions the words must stream through untouched.
func TestInterceptorPassesPlainProse(t *testing.T) {
	text := "Here we discuss function calls and how to invoke a parameter in general."
	in := &AgentStreamInterceptor{}
	var content strings.Builder
	for i := 0; i < len(text); i += 5 {
		end := i + 5
		if end > len(text) {
			end = len(text)
		}
		content.WriteString(in.Feed(text[i:end]).Content)
	}
	content.WriteString(in.Finish().Content)
	if content.String() != text {
		t.Errorf("plain prose altered:\n got=%q\nwant=%q", content.String(), text)
	}
}

func TestCoerceParamValueStringPreservedVerbatim(t *testing.T) {
	for _, v := range []string{"hello", `{"a":1}`, "[1,2]", "30", "true", "line1\nline2", `a"b`, ""} {
		var got string
		if err := json.Unmarshal(coerceParamValue(v, "string"), &got); err != nil {
			t.Fatalf("string %q => not a JSON string: %v", v, err)
		}
		if got != v {
			t.Errorf("string %q not preserved verbatim, got %q", v, got)
		}
	}
}

func TestCoerceParamValueTypedAndUnknown(t *testing.T) {
	typed := []struct{ v, typ, want string }{
		{"30", "integer", "30"},
		{"3.5", "number", "3.5"},
		{"true", "boolean", "true"},
		{"false", "boolean", "false"},
		{`[1,2]`, "array", `[1,2]`},
		{`{"a":1}`, "object", `{"a":1}`},
		{"notnum", "integer", `"notnum"`},
		{"30", "", `"30"`},
		{"true", "", `"true"`},
		{`[1]`, "", `[1]`},
		{`{"a":1}`, "", `{"a":1}`},
	}
	for _, c := range typed {
		if got := string(coerceParamValue(c.v, c.typ)); got != c.want {
			t.Errorf("coerce(%q,%q) = %s, want %s", c.v, c.typ, got, c.want)
		}
	}
}

func TestParseFunctionCallsWriteContentPreserved(t *testing.T) {
	content := "{\n  \"key\": \"value\",\n  \"n\": 30\n}"
	text := "<function_calls><invoke name=\"Write\"><parameter name=\"file_path\">a.json</parameter><parameter name=\"content\">" + content + "</parameter></invoke></function_calls>"
	tools := json.RawMessage(`[{"type":"function","function":{"name":"Write","parameters":{"type":"object","properties":{"file_path":{"type":"string"},"content":{"type":"string"}}}}}]`)
	calls := ParseAgentToolCalls(text, tools)
	if len(calls) != 1 {
		t.Fatalf("want 1 call, got %d", len(calls))
	}
	fn := calls[0]["function"].(map[string]interface{})
	var a map[string]interface{}
	if err := json.Unmarshal([]byte(fn["arguments"].(string)), &a); err != nil {
		t.Fatalf("arguments not valid JSON: %v", err)
	}
	if a["content"] != content {
		t.Errorf("content not preserved verbatim:\n got %q\nwant %q", a["content"], content)
	}
	if a["file_path"] != "a.json" {
		t.Errorf("file_path=%v want a.json", a["file_path"])
	}
}

func TestInterceptorFunctionCallsNoContentLoss(t *testing.T) {
	pre := "Let me check the file. "
	block := "<function_calls>\n<invoke name=\"Read\">\n<parameter name=\"file_path\">main.go</parameter>\n</invoke>\n</function_calls>"
	post := " Done looking."
	full := pre + block + post

	in := &AgentStreamInterceptor{}
	var content strings.Builder
	var toolCalls []map[string]interface{}
	for i := 0; i < len(full); i += 3 {
		end := i + 3
		if end > len(full) {
			end = len(full)
		}
		p := in.Feed(full[i:end])
		content.WriteString(p.Content)
		toolCalls = append(toolCalls, p.ToolCalls...)
	}
	f := in.Finish()
	content.WriteString(f.Content)
	toolCalls = append(toolCalls, f.ToolCalls...)

	got := content.String()
	if strings.Contains(got, "<function_calls>") || strings.Contains(got, "<invoke") || strings.Contains(got, "<parameter") {
		t.Errorf("tool markup leaked as content: %q", got)
	}
	if !strings.Contains(got, "Let me check the file.") {
		t.Errorf("leading prose lost: %q", got)
	}
	if !strings.Contains(got, "Done looking.") {
		t.Errorf("trailing prose lost: %q", got)
	}
	if len(toolCalls) != 1 {
		t.Fatalf("interceptor produced %d tool calls, want 1", len(toolCalls))
	}
	if fn := toolCalls[0]["function"].(map[string]interface{}); fn["name"] != "Read" {
		t.Errorf("tool name=%v want Read", fn["name"])
	}
}

func TestNativeContractRidesToolTurnWhenLast(t *testing.T) {
	msgs := `[{"role":"system","content":"You are Trae."},{"role":"user","content":"read the file"},{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"Read","arguments":"{\"file_path\":\"main.go\"}"}}]},{"role":"tool","tool_call_id":"c1","content":"package main"}]`
	tools := json.RawMessage(`[{"type":"function","function":{"name":"Read","parameters":{"type":"object","properties":{"file_path":{"type":"string"}}}}}]`)
	out, err := transformMessagesForAgentNative(json.RawMessage(msgs), tools)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	var arr []map[string]interface{}
	if err := json.Unmarshal(out, &arr); err != nil {
		t.Fatalf("bad output: %v", err)
	}
	last := arr[len(arr)-1]
	if last["role"] != "user" {
		t.Fatalf("last role=%v want user (tool result rendered as user turn)", last["role"])
	}
	c, _ := last["content"].(string)
	for _, want := range []string{"<tools>", "<current_task>", "Read", "package main"} {
		if !strings.Contains(c, want) {
			t.Errorf("tool turn missing %q:\n%s", want, c)
		}
	}
	for i := 0; i < len(arr)-1; i++ {
		if ci, _ := arr[i]["content"].(string); strings.Contains(ci, "<current_task>") {
			t.Errorf("contract leaked onto earlier message %d: %s", i, ci)
		}
	}
}

func TestNativeRoleNormalization(t *testing.T) {
	msgs := `[{"role":" System ","content":"You are Trae."},{"role":"USER","content":"hi"}]`
	tools := json.RawMessage(`[{"type":"function","function":{"name":"noop"}}]`)
	out, err := transformMessagesForAgentNative(json.RawMessage(msgs), tools)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	var arr []map[string]interface{}
	if err := json.Unmarshal(out, &arr); err != nil {
		t.Fatalf("bad output: %v", err)
	}
	if arr[0]["role"] != "system" {
		t.Fatalf("first role=%v want system (from ' System ')", arr[0]["role"])
	}
	sys, _ := arr[0]["content"].(string)
	if !strings.Contains(sys, "You are Trae.") {
		t.Errorf("system persona lost: %s", sys)
	}
	if strings.Contains(sys, "<current_task>") {
		t.Errorf("contract must not ride the system message: %s", sys)
	}
	last := arr[len(arr)-1]
	if last["role"] != "user" {
		t.Fatalf("last role=%v want user (from 'USER')", last["role"])
	}
	if lc, _ := last["content"].(string); !strings.Contains(lc, "<current_task>") || !strings.Contains(lc, "hi") {
		t.Errorf("contract should ride the normalized user turn: %s", lc)
	}
}

func TestParseFunctionCallsNameGating(t *testing.T) {
	prose := `To read a file, emit <invoke name="Reader"><parameter name="p">x</parameter></invoke> in your reply.`
	tools := json.RawMessage(`[{"type":"function","function":{"name":"Read","parameters":{"type":"object","properties":{"file_path":{"type":"string"}}}}}]`)
	if calls := ParseAgentToolCalls(prose, tools); len(calls) != 0 {
		t.Errorf("prose invoke naming an unoffered tool became %d calls, want 0", len(calls))
	}
	real := `<function_calls><invoke name="Read"><parameter name="file_path">main.go</parameter></invoke></function_calls>`
	if calls := ParseAgentToolCalls(real, tools); len(calls) != 1 {
		t.Fatalf("offered-tool invoke => %d calls, want 1", len(calls))
	}
	if calls := ParseAgentToolCalls(prose, nil); len(calls) != 1 {
		t.Errorf("no-tools invoke => %d calls, want 1 (gating disabled)", len(calls))
	}
}

func TestParseFunctionCallsIgnoresInvokeInsideToolCallSpan(t *testing.T) {
	text := "<<<TOOL_CALL>>>\n<invoke name=\"Read\"><parameter name=\"file_path\">main.go</parameter></invoke>\n<<<END_TOOL_CALL>>>"
	tools := json.RawMessage(`[{"type":"function","function":{"name":"Read","parameters":{"type":"object","properties":{"file_path":{"type":"string"}}}}}]`)
	if calls := ParseAgentToolCalls(text, tools); len(calls) != 0 {
		t.Fatalf("invoke inside a TOOL_CALL span produced %d calls, want 0", len(calls))
	}
}

func TestInterceptorFunctionCallsBeforeToolCallMarker(t *testing.T) {
	full := "<function_calls><invoke name=\"Read\"><parameter name=\"file_path\">a.go</parameter></invoke></function_calls>\n" +
		"<<<TOOL_CALL>>>{\"name\":\"bash\",\"arguments\":{\"command\":\"ls\"}}<<<END_TOOL_CALL>>>"
	in := &AgentStreamInterceptor{toolsRaw: json.RawMessage(`[{"type":"function","function":{"name":"Read","parameters":{"type":"object","properties":{"file_path":{"type":"string"}}}}},{"type":"function","function":{"name":"bash"}}]`)}
	var content strings.Builder
	var toolCalls []map[string]interface{}
	for i := 0; i < len(full); i += 4 {
		end := i + 4
		if end > len(full) {
			end = len(full)
		}
		p := in.Feed(full[i:end])
		content.WriteString(p.Content)
		toolCalls = append(toolCalls, p.ToolCalls...)
	}
	f := in.Finish()
	content.WriteString(f.Content)
	toolCalls = append(toolCalls, f.ToolCalls...)

	if got := content.String(); strings.Contains(got, "<function_calls>") || strings.Contains(got, "<invoke") || strings.Contains(got, "TOOL_CALL") {
		t.Errorf("markup leaked as content: %q", got)
	}
	if len(toolCalls) != 2 {
		t.Fatalf("want 2 tool calls (Read + bash), got %d", len(toolCalls))
	}
}

func TestSchemaTypeStringUnions(t *testing.T) {
	cases := []struct{ raw, want string }{
		{`{"anyOf":[{"type":"null"},{"type":"integer"}]}`, "integer"},
		{`{"oneOf":[{"type":"boolean"}]}`, "boolean"},
		{`{"type":["string","null"]}`, "string"},
		{`{"type":"number"}`, "number"},
		{`{"enum":["a","b"]}`, ""},
	}
	for _, c := range cases {
		if got := schemaTypeString(json.RawMessage(c.raw)); got != c.want {
			t.Errorf("schemaTypeString(%s) = %q, want %q", c.raw, got, c.want)
		}
	}
	text := `<function_calls><invoke name="X"><parameter name="n">30</parameter></invoke></function_calls>`
	tools := json.RawMessage(`[{"type":"function","function":{"name":"X","parameters":{"type":"object","properties":{"n":{"anyOf":[{"type":"integer"},{"type":"null"}]}}}}}]`)
	calls := ParseAgentToolCalls(text, tools)
	if len(calls) != 1 {
		t.Fatalf("want 1 call, got %d", len(calls))
	}
	var a map[string]interface{}
	_ = json.Unmarshal([]byte(calls[0]["function"].(map[string]interface{})["arguments"].(string)), &a)
	if a["n"] != float64(30) {
		t.Errorf("anyOf integer param coerced to %v (%T), want number 30", a["n"], a["n"])
	}
}

func TestRecoverGoroutineContainsPanic(t *testing.T) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer recoverGoroutine("test worker")
		panic("boom")
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("goroutine did not complete; panic was not contained")
	}
}

func TestParseFunctionCallsAnthropicInputSchema(t *testing.T) {
	text := `<function_calls><invoke name="Read"><parameter name="limit">30</parameter><parameter name="file_path">a.go</parameter></invoke></function_calls>`
	tools := json.RawMessage(`[{"name":"Read","description":"read a file","input_schema":{"type":"object","properties":{"limit":{"type":"integer"},"file_path":{"type":"string"}}}}]`)
	calls := ParseAgentToolCalls(text, tools)
	if len(calls) != 1 {
		t.Fatalf("want 1 call, got %d", len(calls))
	}
	var a map[string]interface{}
	_ = json.Unmarshal([]byte(calls[0]["function"].(map[string]interface{})["arguments"].(string)), &a)
	if a["limit"] != float64(30) {
		t.Errorf("input_schema integer coerced to %v (%T), want number 30", a["limit"], a["limit"])
	}
	if a["file_path"] != "a.go" {
		t.Errorf("file_path=%v want a.go", a["file_path"])
	}
}

func TestNativeTransformPreservesConversationMemory(t *testing.T) {
	msgs := `[
		{"role":"system","content":"You are Trae."},
		{"role":"user","content":"write a bubble sort in Go called BubbleSortXYZ"},
		{"role":"assistant","content":"Here is the BubbleSortXYZ implementation ABC123."},
		{"role":"user","content":"fix the above issue"}
	]`
	tools := json.RawMessage(`[{"type":"function","function":{"name":"Edit","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}]`)
	out, err := transformMessagesForAgentNative(json.RawMessage(msgs), tools)
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	var arr []map[string]interface{}
	if err := json.Unmarshal(out, &arr); err != nil {
		t.Fatalf("bad output: %v", err)
	}

	var joined string
	for _, m := range arr {
		c, _ := m["content"].(string)
		joined += "\n" + c
	}
	for _, want := range []string{"You are Trae.", "BubbleSortXYZ", "ABC123", "fix the above issue"} {
		if !strings.Contains(joined, want) {
			t.Errorf("conversation memory lost %q:\n%s", want, joined)
		}
	}

	var sawAssistant bool
	for _, m := range arr {
		if m["role"] == "assistant" {
			if c, _ := m["content"].(string); strings.Contains(c, "ABC123") {
				sawAssistant = true
			}
		}
	}
	if !sawAssistant {
		t.Error("prior assistant answer not preserved on an assistant-role turn")
	}

	last := arr[len(arr)-1]
	lc, _ := last["content"].(string)
	if !strings.Contains(lc, "fix the above issue") || !strings.Contains(lc, "<current_task>") {
		t.Errorf("final turn should carry the new instruction plus the tool contract: %s", lc)
	}
}

// Captured live from Trae: the model puts the tool name in the opening marker
// instead of the canonical <<<TOOL_CALL>>>{"name":...}.
const namedMarkerTools = `[{"type":"function","function":{"name":"TodoWrite","parameters":{"type":"object","properties":{"todos":{"type":"array"},"merge":{"type":"boolean"}}}}}]`

func TestParseNamedMarkerToolCall(t *testing.T) {
	text := "Now let me create a simple agent todo list: <<<TodoWrite>>>\n" +
		`{"todos": [{"id": "1", "content": "Identify screen", "status": "completed", "priority": "high"}], "merge": false}` +
		"<<<END_TOOL_CALL>>>"
	tools := json.RawMessage(namedMarkerTools)

	calls := ParseAgentToolCalls(text, tools)
	if len(calls) != 1 {
		t.Fatalf("named marker produced %d calls, want 1", len(calls))
	}
	fn := calls[0]["function"].(map[string]interface{})
	if fn["name"] != "TodoWrite" {
		t.Errorf("name=%v want TodoWrite", fn["name"])
	}
	var a map[string]interface{}
	if err := json.Unmarshal([]byte(fn["arguments"].(string)), &a); err != nil {
		t.Fatalf("arguments not valid JSON: %v", err)
	}
	if todos, ok := a["todos"].([]interface{}); !ok || len(todos) != 1 {
		t.Errorf("todos not carried through: %v", a["todos"])
	}
	if a["merge"] != false {
		t.Errorf("merge=%v want false", a["merge"])
	}
	stripped := StripAgentToolCalls(text, tools)
	if strings.Contains(stripped, "TodoWrite") || strings.Contains(stripped, "TOOL_CALL") || strings.Contains(stripped, "todos") {
		t.Errorf("marker block leaked into visible text: %q", stripped)
	}
	if !strings.Contains(stripped, "todo list") {
		t.Errorf("surrounding prose lost: %q", stripped)
	}
}

func TestParseNamedMarkerLowercaseAndClosingTag(t *testing.T) {
	text := "<<<todowrite>>>" + `{"todos": [], "merge": true}` + "<<<END_TOOL_CALL>>></todowrite>"
	tools := json.RawMessage(namedMarkerTools)
	calls := ParseAgentToolCalls(text, tools)
	if len(calls) != 1 {
		t.Fatalf("lowercase named marker produced %d calls, want 1", len(calls))
	}
	if fn := calls[0]["function"].(map[string]interface{}); fn["name"] != "TodoWrite" {
		t.Errorf("name=%v want canonical TodoWrite", fn["name"])
	}
	if stripped := StripAgentToolCalls(text, tools); strings.TrimSpace(stripped) != "" {
		t.Errorf("residue left after strip: %q", stripped)
	}
}

func TestNamedMarkerIgnoredWhenNotAnOfferedTool(t *testing.T) {
	text := "Use <<<SomeRandomThing>>> in your reply.<<<END_TOOL_CALL>>>"
	tools := json.RawMessage(namedMarkerTools)
	if calls := ParseAgentToolCalls(text, tools); len(calls) != 0 {
		t.Errorf("bracketed non-tool word became %d calls, want 0", len(calls))
	}
}

func TestNamedMarkerNestedCanonicalPayload(t *testing.T) {
	text := "<<<TodoWrite>>>" + `{"name":"TodoWrite","arguments":{"merge":true}}` + "<<<END_TOOL_CALL>>>"
	calls := ParseAgentToolCalls(text, json.RawMessage(namedMarkerTools))
	if len(calls) != 1 {
		t.Fatalf("want 1 call, got %d", len(calls))
	}
	fn := calls[0]["function"].(map[string]interface{})
	var a map[string]interface{}
	_ = json.Unmarshal([]byte(fn["arguments"].(string)), &a)
	if a["merge"] != true || len(a) != 1 {
		t.Errorf("nested canonical payload not unwrapped: %v", a)
	}
}

func TestInterceptorStreamsNamedMarker(t *testing.T) {
	full := "Creating the list. <<<TodoWrite>>>" +
		`{"todos": [{"id": "1", "content": "x", "status": "pending"}], "merge": false}` +
		"<<<END_TOOL_CALL>>></TodoWrite> Done."
	in := &AgentStreamInterceptor{toolsRaw: json.RawMessage(namedMarkerTools)}
	var content strings.Builder
	var toolCalls []map[string]interface{}
	for i := 0; i < len(full); i += 3 {
		end := i + 3
		if end > len(full) {
			end = len(full)
		}
		p := in.Feed(full[i:end])
		content.WriteString(p.Content)
		toolCalls = append(toolCalls, p.ToolCalls...)
	}
	f := in.Finish()
	content.WriteString(f.Content)
	toolCalls = append(toolCalls, f.ToolCalls...)

	got := content.String()
	for _, bad := range []string{"TodoWrite", "TOOL_CALL", "todos", "<<<", "</"} {
		if strings.Contains(got, bad) {
			t.Errorf("streamed content leaked %q: %q", bad, got)
		}
	}
	if !strings.Contains(got, "Creating the list.") || !strings.Contains(got, "Done.") {
		t.Errorf("prose around the call lost: %q", got)
	}
	if len(toolCalls) != 1 {
		t.Fatalf("streaming produced %d tool calls, want 1", len(toolCalls))
	}
	fn := toolCalls[0]["function"].(map[string]interface{})
	if fn["name"] != "TodoWrite" {
		t.Errorf("name=%v want TodoWrite", fn["name"])
	}
	if !json.Valid([]byte(fn["arguments"].(string))) {
		t.Errorf("streamed arguments not valid JSON: %s", fn["arguments"])
	}
}

// Malformed shapes the model actually produces. Each must yield the call under
// its canonical name and leave no markup visible; a regression here is a lost or
// leaked tool call.
func TestToolCallShapeTolerance(t *testing.T) {
	tools := json.RawMessage(`[
	 {"type":"function","function":{"name":"TodoWrite","parameters":{"type":"object","properties":{"todos":{"type":"array"},"merge":{"type":"boolean"}}}}},
	 {"type":"function","function":{"name":"Write","parameters":{"type":"object","properties":{"file_path":{"type":"string"},"content":{"type":"string"}}}}},
	 {"type":"function","function":{"name":"LS","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}
	]`)

	cases := []struct {
		name     string
		text     string
		wantName string
		wantArg  string
	}{
		{"canonical", `<<<TOOL_CALL>>>{"name":"LS","arguments":{"path":"."}}<<<END_TOOL_CALL>>>`, "LS", `"path":"."`},
		{"name in marker", `<<<TodoWrite>>>{"todos":[],"merge":false}<<<END_TOOL_CALL>>>`, "TodoWrite", `"merge":false`},
		{"lowercase name in body", `<<<TOOL_CALL>>>{"name":"todowrite","arguments":{"todos":[]}}<<<END_TOOL_CALL>>>`, "TodoWrite", `"todos":[]`},
		{"spaces in markers", `<<< TOOL_CALL >>>{"name":"LS","arguments":{"path":"."}}<<< END_TOOL_CALL >>>`, "LS", `"path":"."`},
		{"name after colon", `<<<TOOL_CALL: LS>>>{"path":"."}<<<END_TOOL_CALL>>>`, "LS", `"path":"."`},
		{"no end marker", `<<<TOOL_CALL>>>{"name":"LS","arguments":{"path":"."}}`, "LS", `"path":"."`},
		{"xml style closer", `<<<TOOL_CALL>>>{"name":"LS","arguments":{"path":"."}}<<</TOOL_CALL>>>`, "LS", `"path":"."`},
		{"trailing comma", `<<<TOOL_CALL>>>{"name":"LS","arguments":{"path":".",}}<<<END_TOOL_CALL>>>`, "LS", `"path":"."`},
		{"raw newline in string", "<<<TOOL_CALL>>>{\"name\":\"Write\",\"arguments\":{\"file_path\":\"a.txt\",\"content\":\"line1\nline2\"}}<<<END_TOOL_CALL>>>", "Write", `line1\nline2`},
		{"invoke single quotes", `<function_calls><invoke name='LS'><parameter name='path'>.</parameter></invoke></function_calls>`, "LS", `"path":"."`},
		{"invoke lowercase name", `<function_calls><invoke name="ls"><parameter name="path">.</parameter></invoke></function_calls>`, "LS", `"path":"."`},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			calls := ParseAgentToolCalls(c.text, tools)
			if len(calls) != 1 {
				t.Fatalf("got %d calls, want 1", len(calls))
			}
			fn := calls[0]["function"].(map[string]interface{})
			if fn["name"] != c.wantName {
				t.Errorf("name = %v, want %s", fn["name"], c.wantName)
			}
			args, _ := fn["arguments"].(string)
			if !json.Valid([]byte(args)) {
				t.Fatalf("arguments not valid JSON: %s", args)
			}
			if !strings.Contains(args, c.wantArg) {
				t.Errorf("arguments = %s, want substring %s", args, c.wantArg)
			}
			stripped := StripAgentToolCalls(c.text, tools)
			for _, bad := range []string{"TOOL_CALL", "<<<", "invoke", "parameter"} {
				if strings.Contains(stripped, bad) {
					t.Errorf("visible text leaked %q: %q", bad, stripped)
				}
			}
		})
	}
}

// An unparseable block must stay visible: silent loss is worse than raw text.
func TestUnparseableBlockStaysVisible(t *testing.T) {
	text := `<<<TOOL_CALL>>>this is not json at all<<<END_TOOL_CALL>>>`
	if calls := ParseAgentToolCalls(text, nil); len(calls) != 0 {
		t.Errorf("got %d calls, want 0", len(calls))
	}
	if got := StripAgentToolCalls(text, nil); !strings.Contains(got, "not json") {
		t.Errorf("unparseable block vanished: %q", got)
	}
}

func TestStreamingShapeTolerance(t *testing.T) {
	tools := json.RawMessage(`[{"type":"function","function":{"name":"LS","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}]`)
	cases := []struct{ name, text string }{
		{"canonical", `<<<TOOL_CALL>>>{"name":"LS","arguments":{"path":"."}}<<<END_TOOL_CALL>>>`},
		{"name in marker", `<<<LS>>>{"path":"."}<<<END_TOOL_CALL>>>`},
		{"lowercase body name", `<<<TOOL_CALL>>>{"name":"ls","arguments":{"path":"."}}<<<END_TOOL_CALL>>>`},
		{"no end marker", `<<<TOOL_CALL>>>{"name":"LS","arguments":{"path":"."}}`},
		{"spaces in markers", `<<< TOOL_CALL >>>{"name":"LS","arguments":{"path":"."}}<<< END_TOOL_CALL >>>`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, step := range []int{1, 3, 7, 64} {
				in := &AgentStreamInterceptor{toolsRaw: tools}
				var content strings.Builder
				var got []map[string]interface{}
				for i := 0; i < len(c.text); i += step {
					e := i + step
					if e > len(c.text) {
						e = len(c.text)
					}
					p := in.Feed(c.text[i:e])
					content.WriteString(p.Content)
					got = append(got, p.ToolCalls...)
				}
				f := in.Finish()
				content.WriteString(f.Content)
				got = append(got, f.ToolCalls...)

				if len(got) != 1 {
					t.Fatalf("step %d: got %d calls, want 1", step, len(got))
				}
				if fn := got[0]["function"].(map[string]interface{}); fn["name"] != "LS" {
					t.Errorf("step %d: name = %v, want LS", step, fn["name"])
				}
				for _, bad := range []string{"TOOL_CALL", "<<<", "path"} {
					if strings.Contains(content.String(), bad) {
						t.Errorf("step %d: content leaked %q: %q", step, bad, content.String())
					}
				}
			}
		})
	}
}
