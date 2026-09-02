package zbridge

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestShouldShimGLMPromptOnlyForFoldedPayloads(t *testing.T) {
	if !shouldShimGLMPrompt(json.RawMessage(`[{"role":"user","content":"hi"},{"role":"system","content":"sys"}]`)) {
		t.Fatal("expected folded payload to be shimmed")
	}
	if shouldShimGLMPrompt(json.RawMessage(`[{"role":"system","content":"sys"},{"role":"user","content":"hi"}]`)) {
		t.Fatal("native payload with leading system should not be shimmed")
	}
	if shouldShimGLMPrompt(json.RawMessage(`[{"role":"user","content":"hi"}]`)) {
		t.Fatal("payload without system/developer should not be shimmed")
	}
}

func TestShimGLMFoldsSystemIntoLastUser(t *testing.T) {
	input := `{"model":"GLM-5v-Turbo","messages":[{"role":"system","content":"You are a coding agent."},{"role":"user","content":"hi"}]}`
	out := shimGLMPrompt([]byte(input))

	msgs := extractShimMessages(t, out)
	if len(msgs) != 1 {
		t.Fatalf("want 1 message, got %d", len(msgs))
	}
	if msgs[0].Role != "user" {
		t.Fatalf("want role=user, got %s", msgs[0].Role)
	}
	text := shimContentString(msgs[0].Content)
	if !strings.Contains(text, "hi") {
		t.Error("original user text missing")
	}
	if !strings.Contains(text, "You are a coding agent.") {
		t.Error("system text not folded into user message")
	}
	if !strings.Contains(text, "[SYSTEM INSTRUCTIONS]") {
		t.Error("separator missing")
	}
}

func TestShimGLMNoSystemPassthrough(t *testing.T) {
	input := `{"model":"GLM-5v-Turbo","messages":[{"role":"user","content":"hi"}]}`
	out := shimGLMPrompt([]byte(input))

	if string(out) != input {
		t.Errorf("expected passthrough, got:\n%s", out)
	}
}

func TestShimGLMMultiTurnFoldsIntoLastUser(t *testing.T) {
	input := `{"model":"GLM-5v-Turbo","messages":[` +
		`{"role":"system","content":"Be helpful."},` +
		`{"role":"user","content":"first question"},` +
		`{"role":"assistant","content":"answer"},` +
		`{"role":"user","content":"follow up"}` +
		`]}`
	out := shimGLMPrompt([]byte(input))

	msgs := extractShimMessages(t, out)
	if len(msgs) != 3 {
		t.Fatalf("want 3 messages (2 user + 1 assistant), got %d", len(msgs))
	}

	firstUser := shimContentString(msgs[0].Content)
	if strings.Contains(firstUser, "Be helpful") {
		t.Error("system was folded into the FIRST user message instead of the last")
	}

	lastUser := shimContentString(msgs[2].Content)
	if !strings.Contains(lastUser, "follow up") {
		t.Error("last user original text missing")
	}
	if !strings.Contains(lastUser, "Be helpful") {
		t.Error("system text not in last user message")
	}
}

func TestShimGLMArrayContentSystem(t *testing.T) {
	input := `{"model":"GLM-5v-Turbo","messages":[` +
		`{"role":"system","content":[{"type":"text","text":"agent instructions"}]},` +
		`{"role":"user","content":"do it"}` +
		`]}`
	out := shimGLMPrompt([]byte(input))

	msgs := extractShimMessages(t, out)
	if len(msgs) != 1 {
		t.Fatalf("want 1 message, got %d", len(msgs))
	}
	text := shimContentString(msgs[0].Content)
	if !strings.Contains(text, "agent instructions") {
		t.Error("array-content system text not extracted")
	}
	if !strings.Contains(text, "do it") {
		t.Error("original user text missing")
	}
}

func TestShimGLMEmptySystemRemoved(t *testing.T) {
	input := `{"model":"GLM-5v-Turbo","messages":[{"role":"system","content":""},{"role":"user","content":"hi"}]}`
	out := shimGLMPrompt([]byte(input))

	msgs := extractShimMessages(t, out)
	if len(msgs) != 1 {
		t.Fatalf("want 1 message after removing empty system, got %d", len(msgs))
	}
	text := shimContentString(msgs[0].Content)
	if text != "hi" {
		t.Errorf("want unchanged user text 'hi', got %q", text)
	}
}

func TestShimGLMPreservesOtherFields(t *testing.T) {
	input := `{"model":"GLM-5v-Turbo","stream":true,"temperature":0.7,"messages":[{"role":"system","content":"sys"},{"role":"user","content":"hi"}]}`
	out := shimGLMPrompt([]byte(input))

	var full map[string]json.RawMessage
	if err := json.Unmarshal(out, &full); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var stream bool
	json.Unmarshal(full["stream"], &stream)
	if !stream {
		t.Error("stream field lost")
	}
	var temp float64
	json.Unmarshal(full["temperature"], &temp)
	if temp != 0.7 {
		t.Errorf("temperature = %v, want 0.7", temp)
	}
}

func TestShimGLMMultipleSystemMessages(t *testing.T) {
	input := `{"model":"GLM-5v-Turbo","messages":[` +
		`{"role":"system","content":"instruction one"},` +
		`{"role":"developer","content":"instruction two"},` +
		`{"role":"user","content":"go"}` +
		`]}`
	out := shimGLMPrompt([]byte(input))

	msgs := extractShimMessages(t, out)
	if len(msgs) != 1 {
		t.Fatalf("want 1 message, got %d", len(msgs))
	}
	text := shimContentString(msgs[0].Content)
	if !strings.Contains(text, "instruction one") || !strings.Contains(text, "instruction two") {
		t.Errorf("both system messages should be folded, got: %s", text)
	}
}

func TestShimGLMNoUserCreatesOne(t *testing.T) {
	input := `{"model":"GLM-5v-Turbo","messages":[{"role":"system","content":"you are helpful"}]}`
	out := shimGLMPrompt([]byte(input))

	msgs := extractShimMessages(t, out)
	if len(msgs) != 1 {
		t.Fatalf("want 1 synthesized user message, got %d", len(msgs))
	}
	if msgs[0].Role != "user" {
		t.Errorf("role = %s, want user", msgs[0].Role)
	}
	text := shimContentString(msgs[0].Content)
	if !strings.Contains(text, "you are helpful") {
		t.Error("system text not in synthesized user message")
	}
}

func TestShimGLMUserWithArrayContent(t *testing.T) {
	input := `{"model":"GLM-5v-Turbo","messages":[` +
		`{"role":"system","content":"sys prompt"},` +
		`{"role":"user","content":[{"type":"text","text":"user text"},{"type":"image_url","image_url":{"url":"data:image/png;base64,abc"}}]}` +
		`]}`
	out := shimGLMPrompt([]byte(input))

	msgs := extractShimMessages(t, out)
	if len(msgs) != 1 {
		t.Fatalf("want 1 message, got %d", len(msgs))
	}

	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(msgs[0].Content, &parts); err != nil {
		t.Fatalf("content should still be array: %v", err)
	}

	foundText := false
	for _, p := range parts {
		var typ string
		json.Unmarshal(p["type"], &typ)
		if typ == "text" {
			var txt string
			json.Unmarshal(p["text"], &txt)
			if strings.Contains(txt, "sys prompt") && strings.Contains(txt, "user text") {
				foundText = true
			}
		}
	}
	if !foundText {
		t.Error("system text not appended to the text part of array content")
	}
}

type shimMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

func extractShimMessages(t *testing.T, body []byte) []shimMsg {
	t.Helper()
	var req struct {
		Messages []shimMsg `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("unmarshal messages: %v", err)
	}
	return req.Messages
}

func shimContentString(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		out := ""
		for _, p := range parts {
			out += p.Text
		}
		return out
	}
	return string(raw)
}
