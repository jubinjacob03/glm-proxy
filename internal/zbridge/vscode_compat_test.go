package zbridge

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOpenAIReasoningFallsBackToContentOutsideAgentMode(t *testing.T) {
	oldAgentMode := config.AgentMode
	config.AgentMode = false
	defer func() { config.AgentMode = oldAgentMode }()

	full := "<details>> step one\n> step two</details>Final answer."
	results := runSSEParser(t, editAppendStream(full))

	var clientText strings.Builder
	var reasoning strings.Builder
	for _, r := range results {
		if r.Reasoning != "" {
			clientText.WriteString(r.Reasoning)
			reasoning.WriteString(r.Reasoning)
			continue
		}
		if r.Chunk != "" {
			clientText.WriteString(r.Chunk)
		}
	}

	if got, want := clientText.String(), "step one\nstep twoFinal answer."; got != want {
		t.Fatalf("client text = %q, want %q", got, want)
	}
	if got, want := reasoning.String(), "step one\nstep two"; got != want {
		t.Fatalf("reasoning = %q, want %q", got, want)
	}
}

func TestOpenAIKeepAlivePayloadIsEmptyObject(t *testing.T) {
	payload := "{}"
	var decoded struct {
		Choices []json.RawMessage `json:"choices"`
	}
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		t.Fatalf("unmarshal keep-alive: %v", err)
	}
	if len(decoded.Choices) != 0 {
		t.Fatalf("keep-alive should not carry choices, got %d", len(decoded.Choices))
	}
}
