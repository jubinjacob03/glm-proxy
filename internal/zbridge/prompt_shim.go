package zbridge

import (
	"encoding/json"
	"strings"
)

const shimSeparator = "\n\n---\n[SYSTEM INSTRUCTIONS]\n"

func shouldShimGLMPrompt(messagesRaw json.RawMessage) bool {
	if len(messagesRaw) == 0 {
		return false
	}
	var msgs []struct {
		Role string `json:"role"`
	}
	if err := json.Unmarshal(messagesRaw, &msgs); err != nil {
		return false
	}
	if len(msgs) == 0 {
		return false
	}
	if msgs[0].Role == "system" {
		return false
	}
	for _, msg := range msgs {
		if msg.Role == "system" || msg.Role == "developer" {
			return true
		}
	}
	return false
}

func shimGLMPrompt(body []byte) []byte {
	var req struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil || len(req.Messages) == 0 {
		return body
	}
	var full map[string]json.RawMessage
	if err := json.Unmarshal(body, &full); err != nil {
		return body
	}

	var systemTexts []string
	var kept []json.RawMessage
	systemFound := false
	for _, raw := range req.Messages {
		var probe struct {
			Role string `json:"role"`
		}
		if json.Unmarshal(raw, &probe) != nil {
			kept = append(kept, raw)
			continue
		}
		if probe.Role == "system" || probe.Role == "developer" {
			if t := extractMessageText(raw); t != "" {
				systemTexts = append(systemTexts, t)
			}
			systemFound = true
			continue
		}
		kept = append(kept, raw)
	}

	if !systemFound {
		return body
	}

	if len(systemTexts) == 0 {
		b, err := json.Marshal(kept)
		if err != nil {
			return body
		}
		full["messages"] = b
		out, err := json.Marshal(full)
		if err != nil {
			return body
		}
		return out
	}

	var sb strings.Builder
	for i, t := range systemTexts {
		if i > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString(t)
	}
	systemBlock := sb.String()

	lastUserIdx := -1
	for i := len(kept) - 1; i >= 0; i-- {
		var probe struct {
			Role string `json:"role"`
		}
		if json.Unmarshal(kept[i], &probe) == nil && probe.Role == "user" {
			lastUserIdx = i
			break
		}
	}

	if lastUserIdx < 0 {
		synth := map[string]string{"role": "user", "content": systemBlock}
		b, _ := json.Marshal(synth)
		kept = append(kept, b)
	} else {
		kept[lastUserIdx] = appendToUserMessage(kept[lastUserIdx], shimSeparator+systemBlock)
	}

	b, err := json.Marshal(kept)
	if err != nil {
		return body
	}
	full["messages"] = b
	out, err := json.Marshal(full)
	if err != nil {
		return body
	}
	return out
}

func extractMessageText(raw json.RawMessage) string {
	var msg struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &msg) != nil || len(msg.Content) == 0 {
		return ""
	}

	var s string
	if json.Unmarshal(msg.Content, &s) == nil {
		return s
	}

	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(msg.Content, &parts) == nil {
		var b strings.Builder
		wrote := false
		for _, p := range parts {
			if p.Type == "text" || p.Text != "" {
				if wrote {
					b.WriteByte('\n')
				}
				b.WriteString(p.Text)
				wrote = true
			}
		}
		return b.String()
	}
	return ""
}

func appendToUserMessage(raw json.RawMessage, suffix string) json.RawMessage {
	var msg map[string]json.RawMessage
	if json.Unmarshal(raw, &msg) != nil {
		return raw
	}

	contentRaw, ok := msg["content"]
	if !ok || len(contentRaw) == 0 {
		b, _ := json.Marshal(suffix)
		msg["content"] = b
		out, _ := json.Marshal(msg)
		return out
	}

	var s string
	if json.Unmarshal(contentRaw, &s) == nil {
		b, _ := json.Marshal(s + suffix)
		msg["content"] = b
		return mustMarshalRawMessage(msg, raw)
	}

	var parts []map[string]json.RawMessage
	if json.Unmarshal(contentRaw, &parts) == nil && len(parts) > 0 {
		lastTextIdx := -1
		for i := len(parts) - 1; i >= 0; i-- {
			var t string
			typeRaw, hasType := parts[i]["type"]
			if hasType && json.Unmarshal(typeRaw, &t) == nil && t == "text" {
				lastTextIdx = i
				break
			}
		}
		if lastTextIdx >= 0 {
			var existing string
			if json.Unmarshal(parts[lastTextIdx]["text"], &existing) == nil {
				b, _ := json.Marshal(existing + suffix)
				parts[lastTextIdx]["text"] = b
			}
		} else {
			newPart := map[string]json.RawMessage{
				"type": json.RawMessage(`"text"`),
			}
			tb, _ := json.Marshal(suffix)
			newPart["text"] = tb
			parts = append(parts, newPart)
		}
		b, _ := json.Marshal(parts)
		msg["content"] = b
		return mustMarshalRawMessage(msg, raw)
	}

	return raw
}

func mustMarshalRawMessage(v interface{}, fallback json.RawMessage) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return fallback
	}
	return b
}
