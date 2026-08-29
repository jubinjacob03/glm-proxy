package zbridge

func formatOpenAIResponse(result ResponseResult, model, requestId string, stream bool) interface{} {
	rawContent := result.Content
	if rawContent == "" {
		rawContent = result.Text
	}

	if stream {
		if result.FinishReason != "stop" {
			return oaContentDelta(model, requestId, rawContent)
		}
		empty, reason := rawContent, "stop"
		return newOAChunk(model, requestId, &oaDelta{Content: &empty}, &reason)
	}

	promptTokens := estimateTokens(result.Prompt)
	completionTokens := estimateTokens(rawContent)
	reason := "stop"

	return oaChunk{
		ID:      "chatcmpl-" + requestId,
		Object:  "chat.completion",
		Created: nowUnix(),
		Model:   model,
		Choices: []oaChoice{{
			Index: 0,
			Message: &oaMessage{
				Role:             "assistant",
				Content:          rawContent,
				ReasoningContent: result.Reasoning,
			},
			FinishReason: &reason,
		}},
		Usage: &oaUsage{
			PromptTokens:     promptTokens,
			CompletionTokens: completionTokens,
			TotalTokens:      promptTokens + completionTokens,
		},
	}
}

// formatOpenAIToolCallResponse is the non-stream reply when the model invoked a
// tool instead of answering.
func formatOpenAIToolCallResponse(model, requestId, content, reasoning, prompt string, toolCalls []map[string]interface{}) interface{} {
	promptTokens := estimateTokens(prompt)
	completionTokens := estimateTokens(content)
	reason := "tool_calls"

	calls := make([]interface{}, len(toolCalls))
	for i, c := range toolCalls {
		calls[i] = c
	}

	return oaChunk{
		ID:      "chatcmpl-" + requestId,
		Object:  "chat.completion",
		Created: nowUnix(),
		Model:   model,
		Choices: []oaChoice{{
			Index: 0,
			Message: &oaMessage{
				Role:             "assistant",
				Content:          content,
				ReasoningContent: reasoning,
				ToolCalls:        calls,
			},
			FinishReason: &reason,
		}},
		Usage: &oaUsage{
			PromptTokens:     promptTokens,
			CompletionTokens: completionTokens,
			TotalTokens:      promptTokens + completionTokens,
		},
	}
}

type oaErrorBody struct {
	Message string      `json:"message"`
	Type    string      `json:"type"`
	Code    interface{} `json:"code"`
	Param   interface{} `json:"param"`
}

func formatOpenAIError(message, errType string, code interface{}) interface{} {
	return struct {
		Error oaErrorBody `json:"error"`
	}{oaErrorBody{Message: message, Type: errType, Code: code}}
}
