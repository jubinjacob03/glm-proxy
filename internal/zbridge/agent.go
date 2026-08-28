// agent.go
// Agent mode (modern): an XML-sectioned prompt shim for Z.AI compatibility.
//
// Z.AI's completions endpoint accepts neither non-user roles nor OpenAI tool
// definitions, so agent mode rewrites the conversation into a single structured
// prompt and converts the model's textual tool-call protocol back into OpenAI
// tool_calls on the way out. No native tool calling is involved.
//
// The prompt uses explicit section tags (<system>, <tools>, <history_summary>,
// <recent>, <current_task>, <output_rules>), summarises older tool exchanges,
// anchors the latest user message as the current task, and repeats the output
// contract last to exploit recency bias. Parsing is deliberately tolerant:
// markers match with 2..4 angle brackets per side, adjacent ```json fences are
// stripped, several payload shapes are accepted, and the streaming interceptor
// holds back a trailing window so a marker split across upstream chunks cannot
// leak as content.
//
// The legacy [ROLE: ...] shim remains available via AGENT_MODE_VARIANT=legacy.

package zbridge

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

const agentToolStart = "<<<TOOL_CALL>>>"
const agentToolEnd = "<<<END_TOOL_CALL>>>"

// Models sometimes miscount the angle brackets framing a marker, e.g. emitting
// "<<TOOL_CALL>>>" alongside a well-formed "<<<END_TOOL_CALL>>>". An exact
// matcher misses those blocks and the whole tool call leaks to the client as
// plain content, so both markers accept a bracket run of 2..4 per side.
// Emission stays canonical.
const (
	agentStartWord   = "TOOL_CALL"
	agentEndWord     = "END_TOOL_CALL"
	agentMinBrackets = 2
	agentMaxBrackets = 4
)

// agentWorstMarkerLen is the longest accepted spelling ("<<<<TOOL_CALL>>>>").
const agentWorstMarkerLen = 2*agentMaxBrackets + len(agentStartWord)

// bracketRunBack counts the run of b bytes ending immediately before s[i].
func bracketRunBack(s string, i int, b byte) int {
	n := 0
	for i-n-1 >= 0 && s[i-n-1] == b {
		n++
	}
	return n
}

// bracketRunForward counts the run of b bytes starting at s[0].
func bracketRunForward(s string, b byte) int {
	n := 0
	for n < len(s) && s[n] == b {
		n++
	}
	return n
}

// Sentinel results for findAgentMarker.
const (
	markerNone       = -1 // no framed occurrence of word in s
	markerIncomplete = -2 // a candidate needs more bytes before it can match
)

// findAgentMarker locates the first occurrence of word framed by 2..4 '<' before
// and 2..4 '>' after, returning the index of the first bracket and the full
// marker length, or markerNone / markerIncomplete. Occurrences that are not so
// framed (the TOOL_CALL inside an END marker, prose, code) are skipped.
//
// A trailing '>' run reaching the end of s has no terminating byte yet and may
// still grow, so with final=false it reports markerIncomplete rather than
// matching short, which would leak the missing brackets as content. With
// final=true the run is taken as is.
func findAgentMarker(s, word string, final bool) (int, int) {
	for from := 0; ; {
		j := strings.Index(s[from:], word)
		if j < 0 {
			return markerNone, 0
		}
		w := from + j
		lead := bracketRunBack(s, w, '<')
		if lead < agentMinBrackets || lead > agentMaxBrackets {
			from = w + len(word)
			continue
		}
		after := s[w+len(word):]
		trail := bracketRunForward(after, '>')
		switch {
		case trail > agentMaxBrackets:
			// Definitively over-long; more bytes cannot shrink the run.
		case trail == len(after) && !final:
			// The run touches the end of the available data and may still grow
			// past min/max, so wait for a terminating byte.
			return markerIncomplete, 0
		case trail >= agentMinBrackets:
			return w - lead, lead + len(word) + trail
		}
		from = w + len(word)
	}
}

// agentSpan marks one complete tool-call block in finished text:
// [start,end) covers both markers, [bodyStart,bodyEnd) the JSON between them.
type agentSpan struct {
	start, bodyStart, bodyEnd, end int
}

// findAgentSpans walks every complete tool-call block in text. An unterminated
// opening marker is ignored.
func findAgentSpans(text string) []agentSpan {
	var spans []agentSpan
	for pos := 0; ; {
		s, slen := findAgentMarker(text[pos:], agentStartWord, true)
		if s < 0 {
			return spans
		}
		bodyStart := pos + s + slen
		e, elen := findAgentMarker(text[bodyStart:], agentEndWord, true)
		if e < 0 {
			return spans
		}
		spans = append(spans, agentSpan{
			start:     pos + s,
			bodyStart: bodyStart,
			bodyEnd:   bodyStart + e,
			end:       bodyStart + e + elen,
		})
		pos = bodyStart + e + elen
	}
}

// ============================================================================
// PROMPT ARCHITECTURE
// ============================================================================
//
//	<system>       compact output contract
//	<tools>        available tool definitions
//	<history>      older turns, summarised when long
//	<recent>       recent turns with grouped tool exchanges
//	<current_task> the latest user message, as a recency anchor
//	<output_rules> final reminder, carrying the heaviest weight

// agentCallSchema is stated verbatim in the prompt and repeated in the final
// reminder. A bare "{JSON}" placeholder let models invent flat payloads such as
// {"tool":"bash","command":...} that cannot be mapped back to tool_calls.
const agentCallSchema = `{"name":"<tool_name>","arguments":{<parameter JSON>}}`

const agentSystemPrefix = "<system>\n" +
	"You are a helpful assistant with access to tools. Follow these rules strictly:\n" +
	"\n" +
	"REPLY FORMAT \u2014 exactly ONE of:\n" +
	"(A) TOOL CALL: <<<TOOL_CALL>>>" + agentCallSchema + "<<<END_TOOL_CALL>>> \u2014 nothing before or after.\n" +
	"    The JSON object has EXACTLY two keys: \"name\" (the tool to call, spelled exactly as in <tools>) and \"arguments\" (an object with ONLY that tool's parameters).\n" +
	"(B) FINAL ANSWER: plain text, only when no tool applies.\n" +
	"\n" +
	"RULES:\n" +
	"- Never announce plans (\u201cI\u2019ll...\u201d, \u201cLet me...\u201d). Emit the block \u2014 that IS the action.\n" +
	"- Never print code fences (" + "```bash" + ", " + "```json" + "). Only the runtime executes tools.\n" +
	"- Never wrap tool-call markers in code fences.\n" +
	"- Never invent results. Stop at <<<END_TOOL_CALL>>> and wait for tool output.\n" +
	"- Never call a tool not listed in <tools>.\n" +
	"</system>"

// agentFinalReminder closes the prompt. Models weight the end most heavily, so
// the output contract is repeated as the last thing they see.
const agentFinalReminder = `<output_rules>
RESPOND WITH EXACTLY ONE OF:
1. <<<TOOL_CALL>>>{"name":"<tool_name>","arguments":{...}}<<<END_TOOL_CALL>>> (no fences, no other text)
2. Plain text final answer (only if no tool applies to this step)
The tool-call JSON uses EXACTLY the keys "name" and "arguments" — never a "tool" key, never bare top-level parameters.
</output_rules>`

// ============================================================================
// OPENAI WIRE TYPES
// ============================================================================

// agentMessage is one incoming OpenAI-style message. Content stays raw so both
// strings and typed-part arrays are accepted. Unlike the minimal Message type
// used for the Z.AI wire, it carries the tool fields needed to replay prior
// exchanges in the prompt.
type agentMessage struct {
	Role       string              `json:"role"`
	Content    json.RawMessage     `json:"content"`
	ToolCallID string              `json:"tool_call_id,omitempty"`
	ToolCalls  []assistantToolCall `json:"tool_calls,omitempty"`
	Name       string              `json:"name,omitempty"`
}

// openAITool is one entry of the OpenAI tools array. Both the nested form
// ({type:"function",function:{...}}) and flat definitions are accepted.
type openAITool struct {
	Type       string          `json:"type"`
	Function   *openAIFnSpec   `json:"function,omitempty"`
	Name       string          `json:"name,omitempty"`
	Descr      string          `json:"description,omitempty"`
	Parameters json.RawMessage `json:"parameters,omitempty"`
}

type openAIFnSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

func (t *openAITool) fnName() string {
	if t.Function != nil && t.Function.Name != "" {
		return t.Function.Name
	}
	return t.Name
}

func (t *openAITool) fnDescription() string {
	if t.Function != nil && t.Function.Description != "" {
		return t.Function.Description
	}
	return t.Descr
}

func (t *openAITool) fnParameters() json.RawMessage {
	if t.Function != nil && len(t.Function.Parameters) > 0 {
		return t.Function.Parameters
	}
	return t.Parameters
}

// assistantToolCall is a tool call inside an assistant message of the
// incoming request (the client replaying previous calls).
type assistantToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"` // JSON-encoded string per spec
	} `json:"function"`
}

// ============================================================================
// PROMPT BUILDING
// ============================================================================

// contentToText flattens OpenAI message content (string or typed parts) to text.
func contentToText(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return ""
	}
	var s string
	if json.Unmarshal(trimmed, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(trimmed, &parts) == nil {
		texts := make([]string, 0, len(parts))
		for _, p := range parts {
			if p.Text != "" {
				texts = append(texts, p.Text)
			}
		}
		return strings.Join(texts, "\n")
	}
	return string(trimmed)
}

func jsonIndent(raw json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Indent(&buf, bytes.TrimSpace(raw), "", "  "); err != nil {
		return string(bytes.TrimSpace(raw))
	}
	return buf.String()
}

// renderAgentTools renders the OpenAI tools array as the [TOOL CONTRACT] block.
func renderAgentTools(tools []openAITool) string {
	if len(tools) == 0 {
		return "(no tools provided)"
	}
	var b strings.Builder
	for i, tool := range tools {
		name := tool.fnName()
		if name == "" {
			continue
		}
		b.WriteString(fmt.Sprintf("### Tool %d: %s", i+1, name))
		if desc := tool.fnDescription(); desc != "" {
			b.WriteString("\nDescription: " + desc)
		}
		if params := tool.fnParameters(); len(params) > 0 && !bytes.Equal(bytes.TrimSpace(params), []byte("null")) {
			b.WriteString("\nParameters JSON Schema:\n" + jsonIndent(params))
		}
		b.WriteString("\n")
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// agentCallPayload is the JSON object emitted inside a tool-call block.
// A struct (not a map) keeps the documented name-first key order.
type agentCallPayload struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// renderToolCallBlock renders one assistant tool-call block in the wire protocol
// format, used both in prompt history and in response parsing.
func renderToolCallBlock(call assistantToolCall) string {
	payload, err := json.Marshal(agentCallPayload{
		Name:      call.Function.Name,
		Arguments: json.RawMessage(agentParseArguments(call.Function.Arguments)),
	})
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%s\n%s\n%s", agentToolStart, payload, agentToolEnd)
}

// renderAssistantTurn renders an assistant message with optional text and tool
// calls inside an XML-like tag.
func renderAssistantTurn(m agentMessage) string {
	text := contentToText(m.Content)
	var blocks []string
	if text != "" {
		blocks = append(blocks, text)
	}
	for _, call := range m.ToolCalls {
		if block := renderToolCallBlock(call); block != "" {
			blocks = append(blocks, block)
		}
	}
	content := strings.Join(blocks, "\n")
	return fmt.Sprintf("<assistant>\n%s\n</assistant>", content)
}

// renderUserTurn renders a user message inside an XML-like tag.
func renderUserTurn(m agentMessage) string {
	text := contentToText(m.Content)
	if text == "" {
		return ""
	}
	return fmt.Sprintf("<user>\n%s\n</user>", text)
}

// renderSystemTurn renders a system message inside an XML-like tag.
func renderSystemTurn(m agentMessage) string {
	text := contentToText(m.Content)
	if text == "" {
		return ""
	}
	return fmt.Sprintf("<system_message>\n%s\n</system_message>", text)
}

// renderToolResult renders a tool result inside an XML-like tag with the
// call_id attribute for unambiguous matching.
func renderToolResult(m agentMessage) string {
	text := contentToText(m.Content)
	attr := ""
	if m.ToolCallID != "" {
		attr = fmt.Sprintf(` call_id="%s"`, m.ToolCallID)
	}
	return fmt.Sprintf("<tool_result%s>\n%s\n</tool_result>", attr, text)
}

// renderAgentMessage renders one OpenAI message as a delimited section.
func renderAgentMessage(m agentMessage) string {
	role := strings.TrimSpace(m.Role)
	if role == "" {
		role = "user"
	}
	switch role {
	case "system":
		return renderSystemTurn(m)
	case "user":
		return renderUserTurn(m)
	case "assistant":
		return renderAssistantTurn(m)
	case "tool":
		return renderToolResult(m)
	default:
		// Unknown role: render as user with role annotation.
		text := contentToText(m.Content)
		return fmt.Sprintf("<user role=%s>\n%s\n</user>", role, text)
	}
}

// ============================================================================
// HISTORY SUMMARISATION
// ============================================================================
//
// Replaying every tool exchange in a long conversation costs the model its
// focus on the current task, so older turns collapse into a compact block while
// the most recent stay verbatim.

// maxRecentToolExchanges is how many recent exchange pairs stay verbatim.
const maxRecentToolExchanges = 6

// toolExchange records one assistant→tool exchange for summarization.
type toolExchange struct {
	toolName string
	summary  string // truncated tool result
}

// summarizeOldHistory extracts tool-exchange summaries from older messages and
// returns a compact <history_summary> block. Returns empty string if there's
// nothing to summarize.
func summarizeOldHistory(exchanges []toolExchange) string {
	if len(exchanges) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("<history_summary>\nPreviously completed tool calls:\n")
	for i, ex := range exchanges {
		b.WriteString(fmt.Sprintf("%d. %s → %s\n", i+1, ex.toolName, ex.summary))
	}
	b.WriteString("</history_summary>")
	return b.String()
}

// extractToolExchanges scans messages and returns (old exchanges beyond the
// recent window, messages to render verbatim).
func extractToolExchanges(messages []agentMessage) (old []toolExchange, recent []agentMessage) {
	// First pass: identify tool-exchange boundaries.
	// A tool exchange = assistant with tool_calls followed by 1+ tool results.
	type exchange struct{ start, end int } // indices into messages
	var exchanges []exchange
	i := 0
	for i < len(messages) {
		if messages[i].Role == "assistant" && len(messages[i].ToolCalls) > 0 {
			ex := exchange{start: i}
			i++
			// skip tool results
			for i < len(messages) && messages[i].Role == "tool" {
				i++
			}
			ex.end = i
			exchanges = append(exchanges, ex)
		} else {
			i++
		}
	}

	// If there aren't enough exchanges to summarize, keep everything.
	if len(exchanges) <= maxRecentToolExchanges {
		return nil, messages
	}

	// Summarize exchanges before the recent window.
	splitIdx := exchanges[len(exchanges)-maxRecentToolExchanges].start
	for _, ex := range exchanges[:len(exchanges)-maxRecentToolExchanges] {
		// Collect tool names and truncated results from this exchange.
		assistant := messages[ex.start]
		names := make([]string, 0, len(assistant.ToolCalls))
		for _, tc := range assistant.ToolCalls {
			names = append(names, tc.Function.Name)
		}
		toolName := strings.Join(names, ", ")
		// Grab first tool result as summary.
		summary := "ok"
		if ex.end > ex.start+1 {
			result := contentToText(messages[ex.start+1].Content)
			if len(result) > 80 {
				result = result[:77] + "..."
			}
			summary = result
		}
		old = append(old, toolExchange{toolName: toolName, summary: summary})
	}
	recent = messages[splitIdx:]
	return old, recent
}

// buildAgentPrompt assembles the prompt sent to Z.AI, in the section order
// documented under PROMPT ARCHITECTURE above.
func buildAgentPrompt(messages []agentMessage, tools []openAITool) string {
	var b strings.Builder

	b.WriteString(agentSystemPrefix)
	b.WriteString("\n\n")

	b.WriteString("<tools>\n")
	b.WriteString(renderAgentTools(tools))
	b.WriteString("\n</tools>\n\n")

	oldExchanges, recentMessages := extractToolExchanges(messages)

	if summary := summarizeOldHistory(oldExchanges); summary != "" {
		b.WriteString(summary)
		b.WriteString("\n\n")
	}

	if len(recentMessages) > 0 {
		b.WriteString("<recent>\n")
		renderRecentConversation(&b, recentMessages)
		b.WriteString("</recent>\n\n")
	}

	// The last user message is anchored separately so the model knows exactly
	// which message it is answering.
	lastUserIdx := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			lastUserIdx = i
			break
		}
	}
	if lastUserIdx >= 0 {
		text := contentToText(messages[lastUserIdx].Content)
		if text != "" {
			b.WriteString("<current_task>\n")
			b.WriteString(text)
			b.WriteString("\n</current_task>\n\n")
		}
	}

	b.WriteString(agentFinalReminder)

	return b.String()
}

// renderRecentConversation renders recent messages with tool exchanges grouped.
// Tool calls and their results are wrapped in <tool_exchange> tags so the
// model can clearly see the call→result pairing.
func renderRecentConversation(b *strings.Builder, messages []agentMessage) {
	i := 0
	for i < len(messages) {
		m := messages[i]

		// The last user message is skipped here; it goes in <current_task>.
		isLastUser := false
		if m.Role == "user" {
			isLastUser = true
			for j := i + 1; j < len(messages); j++ {
				if messages[j].Role == "user" {
					isLastUser = false
					break
				}
			}
		}

		if isLastUser {
			i++
			continue
		}

		// Assistant tool calls are grouped with the results that follow them.
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			b.WriteString("<tool_exchange>\n")
			b.WriteString(renderAssistantTurn(m))
			b.WriteString("\n")
			i++
			for i < len(messages) && messages[i].Role == "tool" {
				b.WriteString(renderToolResult(messages[i]))
				b.WriteString("\n")
				i++
			}
			b.WriteString("</tool_exchange>\n")
			continue
		}

		if rendered := renderAgentMessage(m); rendered != "" {
			b.WriteString(rendered)
			b.WriteString("\n")
		}
		i++
	}
}

// wrapAgentPromptAsMessages wraps the folded prompt as a single Z.AI user
// message. When the original conversation carried image parts they are appended
// after the text as an OpenAI content array, so vision survives the fold; a
// text-only request keeps the plain string content it always had.
func wrapAgentPromptAsMessages(prompt string, images []json.RawMessage) ([]byte, error) {
	if len(images) == 0 {
		return json.Marshal([]map[string]interface{}{
			{"role": "user", "content": prompt},
		})
	}
	content := make([]interface{}, 0, len(images)+1)
	content = append(content, map[string]interface{}{"type": "text", "text": prompt})
	for _, img := range images {
		content = append(content, img)
	}
	return json.Marshal([]map[string]interface{}{
		{"role": "user", "content": content},
	})
}

// extractImageParts collects every image content part from the incoming OpenAI
// messages, in order, returning each part's JSON verbatim. The agent shim folds
// text and tools into one prompt string, which structurally cannot carry an
// image, so these are pulled out and re-attached as a content array. Verbatim
// passthrough matches the documented Z.AI image_url block, which takes a URL or
// a base64 data URL under image_url.url.
func extractImageParts(rawMessages json.RawMessage) []json.RawMessage {
	var msgs []struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(rawMessages, &msgs) != nil {
		return nil
	}
	var out []json.RawMessage
	for _, m := range msgs {
		trimmed := bytes.TrimSpace(m.Content)
		if len(trimmed) == 0 || trimmed[0] != '[' {
			continue // string content or empty: no parts to inspect
		}
		var parts []json.RawMessage
		if json.Unmarshal(trimmed, &parts) != nil {
			continue
		}
		for _, p := range parts {
			var probe struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(p, &probe) == nil &&
				(probe.Type == "image_url" || probe.Type == "input_image") {
				out = append(out, p)
			}
		}
	}
	return out
}

// ============================================================================
// RESPONSE PARSING
// ============================================================================

var (
	agentFenceLead = regexp.MustCompile(`(?i)^` + "```" + `(?:json)?\s*`)
	agentFenceTail = regexp.MustCompile(`(?i)\s*` + "```" + `$`)
)

// Models often wrap tool-call blocks in ```json fences even when told not to.
// These patterns strip only fence lines sitting directly against a marker,
// never ordinary code blocks elsewhere in the answer. The bracket runs are
// tolerant for the same reason as findAgentMarker.
const agentMarkerPat = "(?:<{2,4})TOOL_CALL(?:>{2,4})"
const agentEndMarkerPat = "(?:<{2,4})END_TOOL_CALL(?:>{2,4})"

var (
	// fence line immediately before a tool-call opening marker
	agentFenceBeforeCallRe = regexp.MustCompile("(?:\\A|\r?\n)[ \t]*```(?:json)?[ \t]*\r?\n(" + agentMarkerPat + ")")
	// fence line right after a tool-call closing marker (keeps the newline that follows)
	agentFenceAfterEndRe = regexp.MustCompile("(" + agentEndMarkerPat + ")[ \t]*\r?\n[ \t]*```(?:json)?[ \t]*((?:\r?\n)?)")
	// bare fence line hanging at the very end of a streamed content piece
	agentTrailFenceRe = regexp.MustCompile("(?:\\A|\r?\n)[ \t]*```(?:json)?[ \t]*(?:\r?\n)?\\z")
)

const agentFenceJSON = "```json"

// agentStreamKeep is the minimum number of trailing bytes the streaming
// interceptor keeps un-flushed while no marker has matched: enough to cover
// a fence line plus a partially received marker at its worst tolerated
// spelling, so neither can ever leak as content. The actual cut is pulled
// back to a rune boundary, so up to 3 extra bytes may be held.
const agentStreamKeep = agentWorstMarkerLen + len("```json\n") + 5

// NormalizeAgentFences removes fence lines adjacent to tool-call markers from
// finished text (non-streaming path).
func NormalizeAgentFences(text string) string {
	for {
		t := agentFenceAfterEndRe.ReplaceAllString(text, "${1}${2}")
		t = agentFenceBeforeCallRe.ReplaceAllString(t, "$1")
		if t == text {
			return t
		}
		text = t
	}
}

// TrimTrailingAgentFence drops one fence line hanging at the end of s
// (the fence the model placed immediately before <<<TOOL_CALL>>>).
func TrimTrailingAgentFence(s string) string {
	return agentTrailFenceRe.ReplaceAllString(s, "")
}

// agentPossibleFencePrefix reports whether s is empty or could still grow into a
// bare fence line, meaning it is too early to treat the bytes after a tool-call
// block as ordinary content.
func agentPossibleFencePrefix(s string) bool {
	if s == "" {
		return true // can't judge yet; wait for more chunks
	}
	for k := 1; k <= len(s) && k <= len(agentFenceJSON)+1; k++ {
		if strings.HasPrefix("```json\n", s[:k]) || strings.HasPrefix("```\n", s[:k]) {
			return true
		}
	}
	return false
}

// SkipLeadingAgentFence returns the length of a bare fence line at the start
// of s (the ``` the model places immediately after <<<END_TOOL_CALL>>>), or 0
// if s does not begin with one.
func SkipLeadingAgentFence(s string) int {
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	if !strings.HasPrefix(s[i:], "```") {
		return 0
	}
	j := i + 3
	if strings.HasPrefix(s[j:], "json") {
		j += len("json")
	}
	for j < len(s) && (s[j] == ' ' || s[j] == '\t') {
		j++
	}
	if j < len(s) && s[j] != '\n' && s[j] != '\r' {
		return 0 // not a bare fence line (e.g. an ordinary ```bash block)
	}
	if j < len(s) { // consume one line terminator
		if s[j] == '\r' {
			j++
		}
		if j < len(s) && s[j] == '\n' {
			j++
		}
	}
	return j
}

// ============================================================================
// PAYLOAD TOLERANCE
// ============================================================================
//
// The contract asks for {"name":"<tool>","arguments":{...}}, but models invent
// their own shapes, most often the flat {"tool":"bash","command":"..."} where
// the name sits under "tool" and the parameters are the remaining top-level
// keys. A strict {name,arguments} unmarshal accepts those with Name == "", so
// the block leaks to the client as plain content and the tool never runs. Every
// shape that unambiguously names a tool and its parameters is therefore taken.

// agentNameKeys are the accepted spellings of the "which tool" key, in priority
// order. Explicit tool-* keys outrank "name", because in a flat payload a "name"
// entry is more likely a parameter than the tool itself.
var agentNameKeys = []string{"tool", "tool_name", "function", "function_name", "name"}

// agentArgKeys are accepted spellings of the explicit "parameters" key.
var agentArgKeys = []string{"arguments", "parameters", "args", "params", "input"}

// agentExtractCall resolves (name, arguments) from one decoded tool-call
// payload object, accepting the canonical shape, alternate key spellings,
// and flat payloads where the parameters are the remaining top-level keys.
func agentExtractCall(obj map[string]json.RawMessage) (name string, args json.RawMessage, ok bool) {
	// Locate the tool name under any accepted key spelling.
	nameKey := ""
	for _, k := range agentNameKeys {
		raw, present := obj[k]
		if !present {
			continue
		}
		var s string
		if json.Unmarshal(raw, &s) == nil && strings.TrimSpace(s) != "" {
			name, nameKey = strings.TrimSpace(s), k
			break
		}
	}
	if nameKey == "" {
		return "", nil, false
	}

	// An explicit arguments object wins over the flat fallback.
	for _, k := range agentArgKeys {
		if raw, present := obj[k]; present && !isJSONNull(raw) {
			return name, raw, true
		}
	}

	// Flat payload: every remaining top-level key is a parameter.
	rest := make(map[string]json.RawMessage, len(obj)-1)
	for k, v := range obj {
		if k != nameKey {
			rest[k] = v
		}
	}
	if len(rest) == 0 {
		return name, json.RawMessage("{}"), true
	}
	marshaled, err := json.Marshal(rest)
	if err != nil {
		return name, json.RawMessage("{}"), true
	}
	return name, marshaled, true
}

// isJSONNull reports whether raw is whitespace, JSON null, or empty.
func isJSONNull(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) == 0 || bytes.Equal(t, []byte("null"))
}

// agentLooseParse parses one tool-call body, tolerating markdown fences and
// the payload shape deviations listed at agentNameKeys / agentArgKeys.
func agentLooseParse(body string) (name string, args json.RawMessage, ok bool) {
	raw := strings.TrimSpace(body)
	raw = agentFenceLead.ReplaceAllString(raw, "")
	raw = agentFenceTail.ReplaceAllString(raw, "")
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &obj); err != nil || len(obj) == 0 {
		return "", nil, false
	}
	return agentExtractCall(obj)
}

// agentParseArguments normalises model-provided arguments to compact JSON:
// objects pass through, JSON-encoded strings are parsed, unparsable strings stay
// quoted.
func agentParseArguments(raw json.RawMessage) string {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || bytes.Equal(t, []byte("null")) {
		return "{}"
	}
	if t[0] == '"' {
		var s string
		if err := json.Unmarshal(t, &s); err == nil {
			var c bytes.Buffer
			if json.Compact(&c, []byte(strings.TrimSpace(s))) == nil && json.Valid(c.Bytes()) {
				return c.String()
			}
			quoted, _ := json.Marshal(s)
			return string(quoted)
		}
	}
	var c bytes.Buffer
	if json.Compact(&c, t) == nil {
		return c.String()
	}
	return "{}"
}

// agentStreamArguments mirrors the stream path: non-string values are
// compacted, string values are used verbatim.
func agentStreamArguments(raw json.RawMessage) string {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || bytes.Equal(t, []byte("null")) {
		return "{}"
	}
	if t[0] == '"' {
		var s string
		if err := json.Unmarshal(t, &s); err == nil {
			return s
		}
	}
	var c bytes.Buffer
	if json.Compact(&c, t) == nil {
		return c.String()
	}
	return "{}"
}

// agentRandomHex returns n random bytes as lowercase hex, for call-id suffixes.
func agentRandomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// ParseAgentToolCalls extracts every complete tool-call block from finished
// text and returns OpenAI-format tool_calls objects.
func ParseAgentToolCalls(text string) []map[string]interface{} {
	text = NormalizeAgentFences(text)
	var calls []map[string]interface{}
	for _, span := range findAgentSpans(text) {
		name, args, ok := agentLooseParse(text[span.bodyStart:span.bodyEnd])
		if !ok || name == "" {
			continue
		}
		calls = append(calls, map[string]interface{}{
			"id":   "call_" + agentRandomHex(12),
			"type": "function",
			"function": map[string]interface{}{
				"name":      name,
				"arguments": agentParseArguments(args),
			},
		})
	}
	return calls
}

// StripAgentToolCalls removes all tool-call blocks from finished text.
func StripAgentToolCalls(text string) string {
	text = NormalizeAgentFences(text)
	var kept strings.Builder
	prev := 0
	for _, span := range findAgentSpans(text) {
		kept.WriteString(text[prev:span.start])
		prev = span.end
	}
	kept.WriteString(text[prev:])
	return strings.TrimSpace(kept.String())
}

// ============================================================================
// STREAMING INTERCEPTOR
// ============================================================================

// AgentStreamInterceptor incrementally separates ordinary text from tool-call
// blocks. It retains a short suffix so a marker split across upstream chunks
// is never leaked to the client.
type AgentStreamInterceptor struct {
	buffer     string
	offset     int
	callIndex  int
	pendingSep bool // a tool-call block just closed: watch for a stray fence
}

type AgentParsedChunk struct {
	Content   string
	ToolCalls []map[string]interface{}
}

func (in *AgentStreamInterceptor) Feed(chunk string) AgentParsedChunk {
	in.buffer += chunk
	return in.drain(false)
}

// Finish drains the interceptor at end of stream, treating the buffered tail as
// complete: a marker whose trailing '>' run touches the very end can now match,
// and whatever remains unparsed is ordinary content. Tool calls discovered here
// must still be forwarded to the client.
func (in *AgentStreamInterceptor) Finish() AgentParsedChunk {
	parsed := in.drain(true)
	in.offset = len(in.buffer)
	return parsed
}

func (in *AgentStreamInterceptor) drain(final bool) AgentParsedChunk {
	var content []string
	var toolCalls []map[string]interface{}

	for {
		// Right after a tool-call block, swallow blank space and stray fence
		// lines the model appends despite instructions, possibly split across
		// chunks. Content elsewhere, including real code blocks, is untouched.
		if in.pendingSep {
			for {
				for in.offset < len(in.buffer) && isASCIISpace(in.buffer[in.offset]) {
					in.offset++
				}
				n := SkipLeadingAgentFence(in.buffer[in.offset:])
				if n == 0 {
					break
				}
				in.offset += n
			}
			if agentPossibleFencePrefix(in.buffer[in.offset:]) && !final {
				break // could still become a fence; wait for more chunks
			}
			in.pendingSep = false
		}

		rest := in.buffer[in.offset:]
		start, markerLen := findAgentMarker(rest, agentStartWord, final)
		if start < 0 {
			if final {
				// End of data: everything left is ordinary content.
				if rest != "" {
					content = append(content, rest)
					in.offset = len(in.buffer)
				}
				break
			}
			// Hold back a window wide enough for a fence line plus a partial
			// marker, so neither leaks as content while split across chunks. A
			// marker reported incomplete keeps its bytes inside this window, so
			// nothing here can belong to a future match. The cut backs up to a
			// rune boundary; splitting a multi-byte character would render as
			// replacement-char garble on the client (issue #23).
			const keep = agentStreamKeep
			if len(rest) > keep {
				cut := len(rest) - keep
				for cut > 0 && !utf8.RuneStart(rest[cut]) {
					cut--
				}
				if cut > 0 {
					content = append(content, rest[:cut])
					in.offset += cut
				}
			}
			break
		}
		if start > 0 {
			piece := TrimTrailingAgentFence(rest[:start])
			if piece != "" {
				content = append(content, piece)
			}
			in.offset += start
		}
		bodyStart := in.offset + markerLen
		idx, endMarkerLen := findAgentMarker(in.buffer[bodyStart:], agentEndWord, final)
		if idx < 0 {
			break // incomplete block: wait for more chunks
		}
		end := bodyStart + idx
		raw := strings.TrimSpace(in.buffer[bodyStart:end])
		if name, args, ok := agentLooseParse(raw); ok && name != "" {
			toolCalls = append(toolCalls, map[string]interface{}{
				"index": in.callIndex,
				"id":    "call_" + agentRandomHex(12),
				"type":  "function",
				"function": map[string]interface{}{
					"name":      name,
					"arguments": agentStreamArguments(args),
				},
			})
			in.callIndex++
		} else {
			// Unparsable block: leave it as visible text.
			content = append(content, in.buffer[in.offset:end+endMarkerLen])
		}
		in.offset = end + endMarkerLen
		in.pendingSep = true
	}
	return AgentParsedChunk{Content: strings.Join(content, ""), ToolCalls: toolCalls}
}

func isASCIISpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\v' || b == '\f'
}

// ============================================================================
// SHIM DISPATCH
// ============================================================================
//
// The modern shim in this file and the legacy one in agent_legacy.go expose
// slightly different APIs. These adapters present one surface, so the request
// handlers select the active shim purely from config.

// transformMessagesForAgentModern folds the conversation and tool contract into
// one sectioned prompt, wrapped as a single Z.AI user message.
func transformMessagesForAgentModern(rawMessages json.RawMessage, toolsRaw json.RawMessage) ([]byte, error) {
	var msgs []agentMessage
	if err := json.Unmarshal(rawMessages, &msgs); err != nil {
		return nil, fmt.Errorf("agent transform (modern): parse messages: %w", err)
	}
	var tools []openAITool
	if len(toolsRaw) > 0 {
		_ = json.Unmarshal(toolsRaw, &tools)
	}
	prompt := buildAgentPrompt(msgs, tools)
	return wrapAgentPromptAsMessages(prompt, extractImageParts(rawMessages))
}

// agentTransformMessages rewrites the incoming OpenAI messages array for the
// active agent shim, returning the JSON-encoded messages to send upstream.
func agentTransformMessages(rawMessages, toolsRaw json.RawMessage) ([]byte, error) {
	if config.agentModern() {
		return transformMessagesForAgentModern(rawMessages, toolsRaw)
	}
	var tools []interface{}
	if len(toolsRaw) > 0 {
		_ = json.Unmarshal(toolsRaw, &tools)
	}
	return transformMessagesForAgent(rawMessages, tools)
}

// agentExtractToolCalls parses tool-call blocks out of finished assistant text.
func agentExtractToolCalls(text string) []map[string]interface{} {
	if config.agentModern() {
		return ParseAgentToolCalls(text)
	}
	return extractAgentToolCalls(text)
}

// agentStripToolCalls removes tool-call blocks from finished assistant text.
func agentStripToolCalls(text string) string {
	if config.agentModern() {
		return StripAgentToolCalls(text)
	}
	return stripAgentToolCallBlocks(text)
}

// agentInterceptor is the streaming surface both protocol handlers use: feed
// processes one upstream chunk, finish drains the tail at end of stream.
type agentInterceptor interface {
	feed(chunk string) (content string, toolCalls []map[string]interface{})
	finish() (content string, toolCalls []map[string]interface{})
}

type modernAgentInterceptor struct{ in *AgentStreamInterceptor }

func (m *modernAgentInterceptor) feed(chunk string) (string, []map[string]interface{}) {
	p := m.in.Feed(chunk)
	return p.Content, p.ToolCalls
}

func (m *modernAgentInterceptor) finish() (string, []map[string]interface{}) {
	p := m.in.Finish()
	return p.Content, p.ToolCalls
}

// legacyAgentInterceptor streams arguments incrementally. Its finish returns
// only trailing content; end-of-stream tool calls are caught by the caller's
// agentExtractToolCalls safety net.
type legacyAgentInterceptor struct{ in *agentStreamInterceptor }

func (l *legacyAgentInterceptor) feed(chunk string) (string, []map[string]interface{}) {
	content, toolCalls, _ := l.in.feed(chunk)
	return content, toolCalls
}

func (l *legacyAgentInterceptor) finish() (string, []map[string]interface{}) {
	return l.in.flushFinal(), nil
}

// newAgentInterceptor constructs the streaming interceptor for the active shim.
func newAgentInterceptor() agentInterceptor {
	if config.agentModern() {
		return &modernAgentInterceptor{in: &AgentStreamInterceptor{}}
	}
	return &legacyAgentInterceptor{in: newAgentStreamInterceptor()}
}
