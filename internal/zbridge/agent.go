// Agent mode gives Trae real OpenAI tool_calls over a backend with no native
// function calling.
//
// The default NATIVE transform keeps the real system/user/assistant conversation
// (the /api/v2 endpoint accepts those roles) so the model answers only the latest
// turn. The tool contract rides the latest USER turn — chat.z.ai ignores tool
// instructions in a system message. MODERN (AGENT_MODE_VARIANT=modern) folds
// everything into one user message; LEGACY (=legacy) is the [ROLE: ...] shim.
// Either way the model's textual tool calls — <<<TOOL_CALL>>> or Anthropic
// <function_calls> — are converted back into OpenAI tool_calls on the way out.
//
// Parsing is tolerant: markers match 2..4 angle brackets per side, ```json fences
// are stripped, several payload shapes are accepted, and the streaming
// interceptor holds back a trailing window so a marker split across chunks never
// leaks as content.

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

// Models miscount the angle brackets, e.g. "<<TOOL_CALL>>>" next to a correct
// "<<<END_TOOL_CALL>>>". An exact matcher misses those and the whole call leaks
// as plain content, so both markers accept 2..4 brackets per side. Emission
// stays canonical.
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

// findAgentMarker locates word framed by 2..4 '<' and 2..4 '>', returning the
// first bracket's index and the marker length, or markerNone/markerIncomplete.
// Unframed occurrences (the TOOL_CALL inside an END marker, prose, code) skip.
//
// A '>' run touching the end of s may still grow, so with final=false it reports
// markerIncomplete rather than matching short and leaking the missing brackets.
func lowerASCII(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + ('a' - 'A')
	}
	return b
}

// hasPrefixFoldASCII is strings.HasPrefix with ASCII case folding.
func hasPrefixFoldASCII(s, prefix string) bool {
	if len(s) < len(prefix) {
		return false
	}
	for i := 0; i < len(prefix); i++ {
		if lowerASCII(s[i]) != lowerASCII(prefix[i]) {
			return false
		}
	}
	return true
}

// agentSkipSpaces counts leading spaces and tabs.
func agentSkipSpaces(s string) int {
	n := 0
	for n < len(s) && (s[n] == ' ' || s[n] == '\t') {
		n++
	}
	return n
}

// It scans bracket runs rather than searching for word, so matching is linear,
// allocation-free, and tolerates case and interior spaces alike.
func findAgentMarker(s, word string, final bool) (int, int) {
	for i := 0; i < len(s); {
		j := strings.IndexByte(s[i:], '<')
		if j < 0 {
			break
		}
		start := i + j
		lead := bracketRunForward(s[start:], '<')
		i = start + lead
		if lead < agentMinBrackets || lead > agentMaxBrackets {
			continue
		}
		p := start + lead
		p += agentSkipSpaces(s[p:])
		if !hasPrefixFoldASCII(s[p:], word) {
			// A word still arriving must not be mistaken for absent.
			if !final && hasPrefixFoldASCII(word, s[p:]) {
				return markerIncomplete, 0
			}
			continue
		}
		p += len(word)
		gap := agentSkipSpaces(s[p:])
		trail := bracketRunForward(s[p+gap:], '>')
		switch {
		case trail > agentMaxBrackets:
			// Over-long for good: more bytes cannot shrink the run.
		case p+gap+trail == len(s) && !final:
			// Touches the end and may still grow past max, so wait for a
			// terminating byte.
			return markerIncomplete, 0
		case trail >= agentMinBrackets:
			return start, (p + gap + trail) - start
		}
	}
	return markerNone, 0
}

// findAgentEndMarker accepts <<<END_TOOL_CALL>>> and the <<</TOOL_CALL>>> closer
// models substitute for it.
func findAgentEndMarker(s string, final bool) (int, int) {
	e, elen := findAgentMarker(s, agentEndWord, final)
	a, alen := findAgentMarker(s, "/"+agentStartWord, final)
	if e >= 0 && (a < 0 || e <= a) {
		return e, elen
	}
	if a >= 0 {
		return a, alen
	}
	return e, elen
}

// agentIdentByte reports whether b can appear in a tool name inside a marker.
func agentIdentByte(b byte) bool {
	return b == '_' || b == '-' || b == '.' ||
		(b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// findAgentNamedMarker matches an opener carrying the tool name itself —
// <<<TodoWrite>>> — which models emit regularly. Only offered names match, so
// bracketed prose is never taken for a call; case folds, canonical is returned.
func findAgentNamedMarker(s string, names map[string]string) (int, int, string) {
	if len(names) == 0 {
		return markerNone, 0, ""
	}
	for i := 0; i < len(s); {
		j := strings.IndexByte(s[i:], '<')
		if j < 0 {
			break
		}
		start := i + j
		lead := bracketRunForward(s[start:], '<')
		i = start + lead
		if lead < agentMinBrackets || lead > agentMaxBrackets {
			continue
		}
		w := start + lead
		n := 0
		for w+n < len(s) && agentIdentByte(s[w+n]) {
			n++
		}
		if n == 0 {
			continue
		}
		word := s[w : w+n]
		end := w + n
		canon, ok := names[strings.ToLower(word)]
		if !ok {
			// "<<<TOOL_CALL: LS>>>" names the tool after a colon instead.
			if !strings.EqualFold(word, agentStartWord) {
				continue
			}
			k := end + agentSkipSpaces(s[end:])
			if k >= len(s) || s[k] != ':' {
				continue
			}
			k++
			k += agentSkipSpaces(s[k:])
			m := 0
			for k+m < len(s) && agentIdentByte(s[k+m]) {
				m++
			}
			if m == 0 {
				continue
			}
			if canon, ok = names[strings.ToLower(s[k:k+m])]; !ok {
				continue
			}
			end = k + m
		}
		gap := agentSkipSpaces(s[end:])
		trail := bracketRunForward(s[end+gap:], '>')
		if trail >= agentMinBrackets && trail <= agentMaxBrackets {
			return start, (end + gap + trail) - start, canon
		}
	}
	return markerNone, 0, ""
}

// agentMarkerSafeLen returns the bytes preceding the earliest position that could
// still grow into any opener — canonical, name-carrying or colon form — so a
// marker split across chunks is never emitted as content. Sole hold-back
// authority for markers, which is why it is deliberately conservative.
func agentMarkerSafeLen(s string, names map[string]string) int {
	for i := 0; i < len(s); {
		j := strings.IndexByte(s[i:], '<')
		if j < 0 {
			break
		}
		start := i + j
		lead := bracketRunForward(s[start:], '<')
		i = start + lead
		// A bracket run reaching the end may still grow into a marker.
		if start+lead == len(s) {
			if lead <= agentMaxBrackets {
				return start
			}
			continue
		}
		if lead < agentMinBrackets || lead > agentMaxBrackets {
			continue
		}
		p := start + lead
		p += agentSkipSpaces(s[p:])
		if p == len(s) {
			return start // only spaces so far
		}
		n := 0
		for p+n < len(s) && agentIdentByte(s[p+n]) {
			n++
		}
		if n == 0 {
			continue
		}
		if p+n == len(s) {
			return start // the word is still arriving
		}
		word := s[p : p+n]
		if _, ok := names[strings.ToLower(word)]; ok {
			return start
		}
		// The canonical word may still grow into "<<<TOOL_CALL: Name>>>".
		if strings.EqualFold(word, agentStartWord) {
			return start
		}
	}
	return len(s)
}

// findAgentStart returns the earliest opener, canonical or name-carrying. An
// empty name means canonical, whose body names the tool.
func findAgentStart(s string, names map[string]string, final bool) (int, int, string) {
	c, clen := findAgentMarker(s, agentStartWord, final)
	n, nlen, nname := findAgentNamedMarker(s, names)
	if c >= 0 && (n < 0 || c <= n) {
		return c, clen, ""
	}
	if n >= 0 {
		return n, nlen, nname
	}
	return c, clen, ""
}

// agentNamedCall treats the body as the named tool's arguments, unless it nests
// the canonical {"name":...,"arguments":...} shape, which then wins.
func agentNamedCall(name, body string) (string, json.RawMessage, bool) {
	raw := strings.TrimSpace(body)
	raw = agentFenceLead.ReplaceAllString(raw, "")
	raw = agentFenceTail.ReplaceAllString(raw, "")
	if raw == "" {
		return name, json.RawMessage("{}"), true
	}
	obj, repaired, ok := agentUnmarshalBody(raw)
	if !ok {
		return "", nil, false
	}
	raw = repaired
	hasName, hasArgs := false, false
	for _, k := range agentNameKeys {
		if _, ok := obj[k]; ok {
			hasName = true
			break
		}
	}
	for _, k := range agentArgKeys {
		if _, ok := obj[k]; ok {
			hasArgs = true
			break
		}
	}
	if hasName && hasArgs {
		if n, args, ok := agentExtractCall(obj); ok && n != "" {
			return n, args, true
		}
	}
	return name, json.RawMessage(raw), true
}

// agentSkipClosingTag returns the length of a stray </toolname> models append
// after the end marker.
func agentSkipClosingTag(s string) int {
	if !strings.HasPrefix(s, "</") {
		return 0
	}
	i := 2
	for i < len(s) && agentIdentByte(s[i]) {
		i++
	}
	if i == 2 || i >= len(s) || s[i] != '>' {
		return 0
	}
	return i + 1
}

// agentSpan marks one complete block: [start,end) covers both markers,
// [bodyStart,bodyEnd) the JSON between them. name is set when the opening marker
// carried the tool name instead of the canonical word.
type agentSpan struct {
	start, bodyStart, bodyEnd, end int
	name                           string
	unterminated                   bool
}

// findAgentSpans walks every block in finished text. An opener with no closer
// still yields a span to the end, so a truncated call is recovered when its
// payload is complete; callers drop it when the body will not parse.
func findAgentSpans(text string, names map[string]string) []agentSpan {
	var spans []agentSpan
	for pos := 0; ; {
		s, slen, name := findAgentStart(text[pos:], names, true)
		if s < 0 {
			return spans
		}
		bodyStart := pos + s + slen
		e, elen := findAgentEndMarker(text[bodyStart:], true)
		if e < 0 {
			return append(spans, agentSpan{
				start:        pos + s,
				bodyStart:    bodyStart,
				bodyEnd:      len(text),
				end:          len(text),
				name:         name,
				unterminated: true,
			})
		}
		end := bodyStart + e + elen
		if adv := agentSkipClosingTag(text[end:]); adv > 0 {
			end += adv
		}
		spans = append(spans, agentSpan{
			start:     pos + s,
			bodyStart: bodyStart,
			bodyEnd:   bodyStart + e,
			end:       end,
			name:      name,
		})
		pos = end
	}
}

// agentSpanCall resolves one span to a call, canonicalizing the tool name.
func agentSpanCall(span agentSpan, text string, names map[string]string) (string, json.RawMessage, bool) {
	body := text[span.bodyStart:span.bodyEnd]
	var name string
	var args json.RawMessage
	var ok bool
	if span.name != "" {
		name, args, ok = agentNamedCall(span.name, body)
	} else {
		name, args, ok = agentLooseParse(body)
	}
	return canonicalToolName(name, names), args, ok
}

// agentSplitToolCalls returns the calls plus the text stripped of exactly the
// blocks that parsed. One pass drives both, so an unparseable block stays
// visible instead of being deleted as though it had been handled.
func agentSplitToolCalls(text string, toolsRaw json.RawMessage) ([]map[string]interface{}, string) {
	text = NormalizeAgentFences(text)
	names, _ := toolCatalog(toolsRaw)

	var calls []map[string]interface{}
	// kept drops parsed blocks only, so an unparseable one stays visible; outside
	// drops every block, so markup quoted inside one is not re-read as a call.
	var kept, outside strings.Builder
	prevKept, prevOutside := 0, 0
	for _, span := range findAgentSpans(text, names) {
		name, args, ok := agentSpanCall(span, text, names)
		if !ok || name == "" {
			// A stray opener with no closer must not hide the trailing text from
			// the <invoke> scan; a real block's interior still stays hidden.
			if !span.unterminated {
				outside.WriteString(text[prevOutside:span.start])
				prevOutside = span.end
			}
			continue
		}
		outside.WriteString(text[prevOutside:span.start])
		prevOutside = span.end
		kept.WriteString(text[prevKept:span.start])
		prevKept = span.end
		calls = append(calls, map[string]interface{}{
			"id":   "call_" + agentRandomHex(12),
			"type": "function",
			"function": map[string]interface{}{
				"name":      name,
				"arguments": agentParseArguments(args),
			},
		})
	}
	outside.WriteString(text[prevOutside:])
	kept.WriteString(text[prevKept:])

	calls = append(calls, parseFunctionInvokes(outside.String(), toolsRaw)...)
	return calls, strings.TrimSpace(stripFunctionCalls(kept.String()))
}

// Prompt section order, exploiting recency bias — the contract appears first and
// last, and the current task sits near the end:
//
//	<system>       compact output contract
//	<tools>        available tool definitions
//	<history>      older turns, summarised when long
//	<recent>       recent turns with grouped tool exchanges
//	<current_task> the latest user message, as a recency anchor
//	<output_rules> final reminder, carrying the heaviest weight

// agentCallSchema appears verbatim in the prompt and again in the final reminder.
// A bare "{JSON}" placeholder let models invent flat payloads like
// {"tool":"bash","command":...} that cannot map back to tool_calls.
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
	"- The opening marker is the literal text <<<TOOL_CALL>>>. Never put the tool name in it (never <<<TodoWrite>>>) and never add a closing tag such as </TodoWrite>.\n" +
	"</system>"

// agentFinalReminder closes the prompt: models weight the end most heavily, so
// the contract is the last thing they see.
const agentFinalReminder = `<output_rules>
Answer <current_task> only. <recent> and any earlier turns are prior context — do
not re-answer old messages or re-describe images from earlier turns.
RESPOND WITH EXACTLY ONE OF:
1. <<<TOOL_CALL>>>{"name":"<tool_name>","arguments":{...}}<<<END_TOOL_CALL>>> (no fences, no other text)
2. Plain text final answer (only if no tool applies to this step)
The tool-call JSON uses EXACTLY the keys "name" and "arguments" — never a "tool" key, never bare top-level parameters.
</output_rules>`

// agentNativeReminder closes the tool contract that wraps the latest user turn
// in native mode.
const agentNativeReminder = `<output_rules>
Reply to the LAST message only. Earlier turns are context — do not re-answer them or re-describe earlier images.
Emit EXACTLY ONE of:
1. <<<TOOL_CALL>>>{"name":"<tool_name>","arguments":{...}}<<<END_TOOL_CALL>>>  (no fences, nothing else)
2. A plain-text answer (only when no tool applies to this step)
The tool-call JSON uses EXACTLY the keys "name" and "arguments" — never a "tool" key, never bare top-level parameters.
The opening marker is the literal text <<<TOOL_CALL>>>: never put the tool name in it (never <<<TodoWrite>>>), and never append a closing tag such as </TodoWrite>.
</output_rules>`

// agentMessage is one incoming OpenAI-style message. Content stays raw so both
// strings and typed-part arrays are accepted, and unlike the minimal Message
// type it carries the tool fields needed to replay prior exchanges.
type agentMessage struct {
	Role       string              `json:"role"`
	Content    json.RawMessage     `json:"content"`
	ToolCallID string              `json:"tool_call_id,omitempty"`
	ToolCalls  []assistantToolCall `json:"tool_calls,omitempty"`
	Name       string              `json:"name,omitempty"`
}

// openAITool is one tools-array entry. Both the nested
// {type:"function",function:{...}} form and flat definitions are accepted.
type openAITool struct {
	Type        string          `json:"type"`
	Function    *openAIFnSpec   `json:"function,omitempty"`
	Name        string          `json:"name,omitempty"`
	Descr       string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
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
	if len(t.Parameters) > 0 {
		return t.Parameters
	}
	return t.InputSchema
}

// assistantToolCall is a call inside an incoming assistant message, i.e. the
// client replaying earlier calls.
type assistantToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"` // JSON-encoded string per spec
	} `json:"function"`
}

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

// renderAgentTools renders the tools array as the [TOOL CONTRACT] block.
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

// agentCallPayload is what goes inside a tool-call block. A struct, not a map, so
// the documented name-first key order survives.
type agentCallPayload struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// renderToolCallBlock emits one block in wire format, used for prompt history and
// for response parsing alike.
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

// renderAssistantTurn wraps text and any tool calls in an XML-like tag.
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

// renderToolResult wraps a result in a tag carrying call_id, so the pairing is
// unambiguous.
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
		// Unknown role: render as user, annotated.
		text := contentToText(m.Content)
		return fmt.Sprintf("<user role=%s>\n%s\n</user>", role, text)
	}
}

// Replaying every tool exchange in a long conversation costs the model its focus
// on the current task, so older turns collapse into a compact block while the
// most recent stay verbatim.

// maxRecentToolExchanges is how many recent exchange pairs stay verbatim.
const maxRecentToolExchanges = 6

// toolExchange is one assistant-to-tool round, kept for summarising.
type toolExchange struct {
	toolName string
	summary  string // truncated tool result
}

// summarizeOldHistory collapses older tool exchanges into a compact
// <history_summary> block, or "" when there is nothing to summarise.
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

// extractToolExchanges splits messages into exchanges past the recent window and
// the messages to render verbatim.
func extractToolExchanges(messages []agentMessage) (old []toolExchange, recent []agentMessage) {
	// An exchange is an assistant with tool_calls plus the tool results after it.
	type exchange struct{ start, end int } // indices into messages
	var exchanges []exchange
	i := 0
	for i < len(messages) {
		if messages[i].Role == "assistant" && len(messages[i].ToolCalls) > 0 {
			ex := exchange{start: i}
			i++
			// Consume the results belonging to this call.
			for i < len(messages) && messages[i].Role == "tool" {
				i++
			}
			ex.end = i
			exchanges = append(exchanges, ex)
		} else {
			i++
		}
	}

	// Too few to be worth summarising; keep them all.
	if len(exchanges) <= maxRecentToolExchanges {
		return nil, messages
	}

	// Everything before the recent window collapses.
	splitIdx := exchanges[len(exchanges)-maxRecentToolExchanges].start
	for _, ex := range exchanges[:len(exchanges)-maxRecentToolExchanges] {
		// Names plus a truncated result are enough to keep the thread.
		assistant := messages[ex.start]
		names := make([]string, 0, len(assistant.ToolCalls))
		for _, tc := range assistant.ToolCalls {
			names = append(names, tc.Function.Name)
		}
		toolName := strings.Join(names, ", ")
		// The first result stands in for the rest.
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

// buildAgentPrompt assembles the prompt in the section order listed above.
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

	// Anchored separately, so the model knows which message it is answering.
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

// renderRecentConversation groups each call with its result in a
// <tool_exchange> tag so the pairing is unambiguous to the model.
func renderRecentConversation(b *strings.Builder, messages []agentMessage) {
	i := 0
	for i < len(messages) {
		m := messages[i]

		// Skipped here: the last user message goes in <current_task>.
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

		// Group a call with the results that follow it.
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

// wrapAgentPromptAsMessages wraps the folded prompt as one Z.AI user message.
// Image parts are appended after the text as an OpenAI content array so vision
// survives the fold; a text-only request keeps its plain string content.
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

// extractImageParts returns every image content part verbatim, in order. The
// shim folds text and tools into one prompt string, which structurally cannot
// carry an image, so these are pulled out and re-attached as a content array.
//
// Only the LAST user turn's images count. Clients like Trae replay the full
// history every request, so an image sent earlier is always present; attaching it
// again made the model answer that stale image when the new turn (say "hi")
// carried none. Earlier images belong to earlier turns, now context in <recent>.
func extractImageParts(rawMessages json.RawMessage) []json.RawMessage {
	var msgs []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(rawMessages, &msgs) != nil {
		return nil
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return imagePartsOfContent(msgs[i].Content)
		}
	}
	return nil
}

// imagePartsOfContent returns the image_url/input_image parts of one message's
// content array, in order; string or partless content yields nothing.
func imagePartsOfContent(content json.RawMessage) []json.RawMessage {
	trimmed := bytes.TrimSpace(content)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return nil
	}
	var parts []json.RawMessage
	if json.Unmarshal(trimmed, &parts) != nil {
		return nil
	}
	var out []json.RawMessage
	for _, p := range parts {
		var probe struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(p, &probe) == nil &&
			(probe.Type == "image_url" || probe.Type == "input_image") {
			out = append(out, p)
		}
	}
	return out
}

var (
	agentFenceLead = regexp.MustCompile(`(?i)^` + "```" + `(?:json)?\s*`)
	agentFenceTail = regexp.MustCompile(`(?i)\s*` + "```" + `$`)
)

// Models wrap blocks in ```json fences even when told not to. These strip only
// fence lines touching a marker, never ordinary code blocks. Bracket runs are
// tolerant for the same reason as findAgentMarker.
const agentMarkerPat = "(?:<{2,4})TOOL_CALL(?:>{2,4})"
const agentEndMarkerPat = "(?:<{2,4})END_TOOL_CALL(?:>{2,4})"

var (
	// Fence line immediately before an opening marker.
	agentFenceBeforeCallRe = regexp.MustCompile("(?:\\A|\r?\n)[ \t]*```(?:json)?[ \t]*\r?\n(" + agentMarkerPat + ")")
	// Fence line right after a closing marker; keeps the newline after it.
	agentFenceAfterEndRe = regexp.MustCompile("(" + agentEndMarkerPat + ")[ \t]*\r?\n[ \t]*```(?:json)?[ \t]*((?:\r?\n)?)")
	// Bare fence line left hanging at the end of a streamed piece.
	agentTrailFenceRe = regexp.MustCompile("(?:\\A|\r?\n)[ \t]*```(?:json)?[ \t]*(?:\r?\n)?\\z")
)

const agentFenceJSON = "```json"

// agentStreamKeep is how many trailing bytes the interceptor holds while no
// marker has matched: enough for a fence line plus a partial marker at its worst
// tolerated spelling. The cut is pulled back to a rune boundary, so up to 3 more
// bytes may be held.
const agentStreamKeep = agentWorstMarkerLen + len("```json\n") + 5

// NormalizeAgentFences strips marker-adjacent fences from finished text.
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

// TrimTrailingAgentFence drops one fence line hanging at the end of s, the one a
// model puts just before <<<TOOL_CALL>>>.
func TrimTrailingAgentFence(s string) string {
	return agentTrailFenceRe.ReplaceAllString(s, "")
}

// agentPossibleFencePrefix reports whether s is empty or could still grow into a
// bare fence line, i.e. too early to call the bytes after a block content.
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

// SkipLeadingAgentFence returns the length of a bare fence line starting s — the
// ``` models put right after <<<END_TOOL_CALL>>> — or 0 if there is none.
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

// The contract asks for {"name":"<tool>","arguments":{...}}, but models invent
// shapes, most often flat {"tool":"bash","command":"..."} with the name under
// "tool" and parameters as the remaining keys. A strict unmarshal accepts that
// with Name == "", so the block leaks as content and the tool never runs. Hence
// every shape that unambiguously names a tool and its parameters is taken.

// agentNameKeys are accepted spellings of the "which tool" key, in priority
// order. tool-* outranks "name" because in a flat payload a "name" entry is more
// likely a parameter than the tool.
var agentNameKeys = []string{"tool", "tool_name", "function", "function_name", "name"}

// agentArgKeys are accepted spellings of the explicit parameters key.
var agentArgKeys = []string{"arguments", "parameters", "args", "params", "input"}

// agentExtractCall resolves (name, arguments) from one decoded payload, taking
// the canonical shape, alternate spellings, or a flat object.
func agentExtractCall(obj map[string]json.RawMessage) (name string, args json.RawMessage, ok bool) {
	// Find the name under any accepted spelling.
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

	// An explicit arguments object beats the flat fallback.
	for _, k := range agentArgKeys {
		if raw, present := obj[k]; present && !isJSONNull(raw) {
			return name, raw, true
		}
	}

	// Flat payload: every remaining key is a parameter.
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

// isJSONNull reports whether raw is empty, whitespace or JSON null.
func isJSONNull(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) == 0 || bytes.Equal(t, []byte("null"))
}

// agentRepairJSON fixes the deviations models produce in tool-call bodies so the
// payload parses instead of vanishing: emphasis or backticks around the object,
// a trailing comma before a closer, and raw newlines or tabs inside strings
// (illegal JSON, common in file content). Only reached after a strict parse
// fails, so valid JSON is never touched.
func agentRepairJSON(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "*`")
	s = strings.TrimSpace(s)

	var b strings.Builder
	b.Grow(len(s) + 16)
	inStr, esc := false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			case c == '\n':
				b.WriteString(`\n`)
				continue
			case c == '\r':
				b.WriteString(`\r`)
				continue
			case c == '\t':
				b.WriteString(`\t`)
				continue
			}
			b.WriteByte(c)
			continue
		}
		if c == '"' {
			inStr = true
			b.WriteByte(c)
			continue
		}
		if c == ',' {
			j := i + 1
			for j < len(s) && isASCIISpace(s[j]) {
				j++
			}
			if j < len(s) && (s[j] == '}' || s[j] == ']') {
				continue // a comma that only precedes a closer
			}
		}
		b.WriteByte(c)
	}
	return b.String()
}

// agentUnmarshalBody decodes a body, retrying once via agentRepairJSON.
func agentUnmarshalBody(raw string) (map[string]json.RawMessage, string, bool) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &obj); err == nil && len(obj) > 0 {
		return obj, raw, true
	}
	fixed := agentRepairJSON(raw)
	if fixed == raw {
		return nil, raw, false
	}
	obj = nil
	if err := json.Unmarshal([]byte(fixed), &obj); err == nil && len(obj) > 0 {
		return obj, fixed, true
	}
	return nil, raw, false
}

// canonicalToolName maps the model's spelling onto the exact name from the tools
// array, so the client recognizes the call.
func canonicalToolName(name string, names map[string]string) string {
	if name == "" || len(names) == 0 {
		return name
	}
	if canon, ok := names[strings.ToLower(name)]; ok {
		return canon
	}
	return name
}

// agentLooseParse parses one body, tolerating fences and the shape deviations
// listed at agentNameKeys and agentArgKeys.
func agentLooseParse(body string) (name string, args json.RawMessage, ok bool) {
	raw := strings.TrimSpace(body)
	raw = agentFenceLead.ReplaceAllString(raw, "")
	raw = agentFenceTail.ReplaceAllString(raw, "")
	obj, _, ok := agentUnmarshalBody(raw)
	if !ok {
		return "", nil, false
	}
	return agentExtractCall(obj)
}

// agentParseArguments normalises arguments to compact JSON: objects pass through,
// JSON-encoded strings are parsed, unparsable strings stay quoted.
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

// agentStreamArguments is the stream counterpart: non-strings are compacted,
// strings used verbatim.
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

// agentRandomHex returns n random bytes as hex, for call-id suffixes.
func agentRandomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// ParseAgentToolCalls turns every complete block in finished text into
// OpenAI-format tool_calls objects, accepting both the <<<TOOL_CALL>>> markers
// and Trae's Anthropic-style <function_calls><invoke> blocks.
func ParseAgentToolCalls(text string, toolsRaw json.RawMessage) []map[string]interface{} {
	calls, _ := agentSplitToolCalls(text, toolsRaw)
	return calls
}

// StripAgentToolCalls removes every tool-call block from finished text, in both
// supported formats.
func StripAgentToolCalls(text string, toolsRaw json.RawMessage) string {
	_, stripped := agentSplitToolCalls(text, toolsRaw)
	return stripped
}

// Trae drives tools in Anthropic's text format —
// <function_calls><invoke name="X"><parameter name="p">v</parameter></invoke></function_calls>
// — and GLM emits it too, so it is parsed into the same OpenAI tool_calls.

const fnCallsOpen = "<function_calls>"

var (
	fnInvokeRe   = regexp.MustCompile(`(?s)<invoke\s+name=["']([^"']+)["']\s*>(.*?)</invoke>`)
	fnParamRe    = regexp.MustCompile(`(?s)<parameter\s+name=["']([^"']+)["']\s*>(.*?)</parameter>`)
	fnCallsTagRe = regexp.MustCompile(`</?function_calls>`)
)

// fnStartTokens open a Trae tool block; the interceptor holds content back from
// the earliest of them so a block never leaks as text.
var fnStartTokens = []string{fnCallsOpen, "<invoke "}

// toolCatalog reads the offered tools once into folded-to-canonical names and
// per-tool parameter types. The name set gates marker and <invoke> matching.
func toolCatalog(toolsRaw json.RawMessage) (names map[string]string, types map[string]map[string]string) {
	if len(toolsRaw) == 0 {
		return nil, nil
	}
	var tools []openAITool
	if json.Unmarshal(toolsRaw, &tools) != nil {
		return nil, nil
	}
	names = make(map[string]string, len(tools))
	types = make(map[string]map[string]string, len(tools))
	for i := range tools {
		name := tools[i].fnName()
		if name == "" {
			continue
		}
		names[strings.ToLower(name)] = name
		var schema struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if json.Unmarshal(tools[i].fnParameters(), &schema) != nil || len(schema.Properties) == 0 {
			continue
		}
		m := make(map[string]string, len(schema.Properties))
		for pname, praw := range schema.Properties {
			m[pname] = schemaTypeString(praw)
		}
		types[name] = m
	}
	return names, types
}

func schemaTypeString(raw json.RawMessage) string {
	var probe struct {
		Type  json.RawMessage   `json:"type"`
		AnyOf []json.RawMessage `json:"anyOf"`
		OneOf []json.RawMessage `json:"oneOf"`
	}
	if json.Unmarshal(raw, &probe) != nil {
		return ""
	}
	if t := jsonSchemaType(probe.Type); t != "" {
		return t
	}
	for _, alt := range probe.AnyOf {
		if t := schemaTypeString(alt); t != "" {
			return t
		}
	}
	for _, alt := range probe.OneOf {
		if t := schemaTypeString(alt); t != "" {
			return t
		}
	}
	return ""
}

func jsonSchemaType(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if s == "null" {
			return ""
		}
		return s
	}
	var arr []string
	if json.Unmarshal(raw, &arr) == nil {
		for _, t := range arr {
			if t != "" && t != "null" {
				return t
			}
		}
	}
	return ""
}

// parseFunctionInvokes converts every <invoke> to a tool_call, coercing each
// parameter by its schema type. The schema is built only once an invoke is
// present, so a plain-text response never parses the tools array.
func parseFunctionInvokes(text string, toolsRaw json.RawMessage) []map[string]interface{} {
	invokes := fnInvokeRe.FindAllStringSubmatch(text, -1)
	if len(invokes) == 0 {
		return nil
	}
	names, schema := toolCatalog(toolsRaw)
	var calls []map[string]interface{}
	for _, m := range invokes {
		name := strings.TrimSpace(m[1])
		if name == "" {
			continue
		}
		if len(names) > 0 {
			canon, ok := names[strings.ToLower(name)]
			if !ok {
				continue
			}
			name = canon
		}
		types := schema[name]
		args := map[string]json.RawMessage{}
		for _, p := range fnParamRe.FindAllStringSubmatch(m[2], -1) {
			pname := strings.TrimSpace(p[1])
			args[pname] = coerceParamValue(p[2], types[pname])
		}
		argsJSON, err := json.Marshal(args)
		if err != nil {
			argsJSON = []byte("{}")
		}
		calls = append(calls, map[string]interface{}{
			"id":   "call_" + agentRandomHex(12),
			"type": "function",
			"function": map[string]interface{}{
				"name":      name,
				"arguments": string(argsJSON),
			},
		})
	}
	return calls
}

func jsonQuote(v string) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`""`)
	}
	return json.RawMessage(b)
}

// coerceParamValue converts one XML-extracted parameter to JSON by schema type:
// a string stays quoted, so braces, digits and newlines survive verbatim; a
// concrete type becomes native JSON when it parses; an unknown type only when it
// clearly opens an array or object.
func coerceParamValue(v, typ string) json.RawMessage {
	switch typ {
	case "string":
		return jsonQuote(v)
	case "number", "integer", "boolean", "array", "object":
		if t := strings.TrimSpace(v); t != "" {
			if bt := []byte(t); json.Valid(bt) {
				return json.RawMessage(bt)
			}
		}
		return jsonQuote(v)
	default:
		if t := strings.TrimSpace(v); strings.HasPrefix(t, "[") || strings.HasPrefix(t, "{") {
			if bt := []byte(t); json.Valid(bt) {
				return json.RawMessage(bt)
			}
		}
		return jsonQuote(v)
	}
}

// stripFunctionCalls removes <function_calls> wrappers and <invoke> blocks.
func stripFunctionCalls(text string) string {
	text = fnInvokeRe.ReplaceAllString(text, "")
	return strings.TrimSpace(fnCallsTagRe.ReplaceAllString(text, ""))
}

// functionCallsSafeLen returns how many leading bytes of s precede the first
// Trae tool-block start. A start token split across chunks is caught by the
// interceptor's trailing keep-window, which is wider than any of them.
func functionCallsSafeLen(s string) int {
	safe := len(s)
	for _, tok := range fnStartTokens {
		if i := strings.Index(s, tok); i >= 0 && i < safe {
			safe = i
		}
	}
	return safe
}

// AgentStreamInterceptor incrementally separates ordinary text from tool-call
// blocks, retaining a short suffix so a marker split across upstream chunks is
// never leaked to the client.
type AgentStreamInterceptor struct {
	buffer     string
	offset     int
	callIndex  int
	pendingSep bool // a tool-call block just closed: watch for a stray fence
	toolsRaw   json.RawMessage
	toolNames  map[string]string
	catalogSet bool
}

// names resolves the offered tool names once, not per chunk.
func (in *AgentStreamInterceptor) names() map[string]string {
	if !in.catalogSet {
		in.toolNames, _ = toolCatalog(in.toolsRaw)
		in.catalogSet = true
	}
	return in.toolNames
}

type AgentParsedChunk struct {
	Content   string
	ToolCalls []map[string]interface{}
}

// maxAgentHold caps text held back waiting for a marker to close. Generous, so only a
// stream that opened a tool call and never closed it can reach it.
const maxAgentHold = 4 << 20

func (in *AgentStreamInterceptor) Feed(chunk string) AgentParsedChunk {
	in.buffer += chunk
	parsed := in.drain(false)
	in.compact()

	// An unterminated marker pins offset, so compact cannot advance and the buffer
	// would grow with the whole response. Past the cap, stop waiting and emit it.
	if len(in.buffer)-in.offset > maxAgentHold {
		parsed.Content += in.buffer[in.offset:]
		in.buffer = ""
		in.offset = 0
	}
	return parsed
}

// compact drops the consumed prefix. Without it Feed's string append is quadratic in
// the response length: a 1 MB answer in 2 KB deltas moves gigabytes.
func (in *AgentStreamInterceptor) compact() {
	if in.offset == 0 || in.offset > len(in.buffer) {
		return
	}
	in.buffer = in.buffer[in.offset:]
	in.offset = 0
}

// Finish drains at end of stream, treating the buffered tail as complete: a
// marker whose trailing '>' run touches the end can now match, and anything left
// unparsed is ordinary content. Tool calls found here still need forwarding.
func (in *AgentStreamInterceptor) Finish() AgentParsedChunk {
	parsed := in.drain(true)
	in.offset = len(in.buffer)
	return parsed
}

func (in *AgentStreamInterceptor) drain(final bool) AgentParsedChunk {
	var content []string
	var toolCalls []map[string]interface{}

	for {
		// Straight after a block, swallow blank space and stray fence lines the
		// model appends anyway, possibly split across chunks. Content elsewhere,
		// real code blocks included, is untouched.
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
			if adv := agentSkipClosingTag(in.buffer[in.offset:]); adv > 0 {
				in.offset += adv
				continue
			}
			// Closing tag still arriving: wait rather than leak "</too".
			if tail := in.buffer[in.offset:]; !final &&
				strings.HasPrefix(tail, "</") && !strings.Contains(tail, ">") {
				break
			}
			if agentPossibleFencePrefix(in.buffer[in.offset:]) && !final {
				break // could still become a fence; wait for more chunks
			}
			in.pendingSep = false
		}

		rest := in.buffer[in.offset:]
		names := in.names()
		start, markerLen, callName := findAgentStart(rest, names, final)
		if start < 0 {
			if final {
				// End of stream: parse complete <function_calls> in the tail, emit
				// the rest as content, so nothing is dropped or leaked.
				if calls := parseFunctionInvokes(rest, in.toolsRaw); len(calls) > 0 {
					for _, c := range calls {
						c["index"] = in.callIndex
						in.callIndex++
						toolCalls = append(toolCalls, c)
					}
					if residual := stripFunctionCalls(rest); residual != "" {
						content = append(content, residual)
					}
				} else if rest != "" {
					content = append(content, rest)
				}
				in.offset = len(in.buffer)
				break
			}
			// Mid-stream: hold back any partial marker or <function_calls> block so
			// it never leaks, keeping a window wide enough for a fence line too. The
			// cut backs up to a rune boundary, else it garbles as U+FFFD (issue #23).
			safe := functionCallsSafeLen(rest)
			if n := agentMarkerSafeLen(rest, names); n < safe {
				safe = n
			}
			emitLen := len(rest) - agentStreamKeep
			if safe < emitLen {
				emitLen = safe
			}
			if emitLen > 0 {
				cut := emitLen
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
				// A <function_calls> block ahead of this marker must not leak: parse
				// it and strip the markup first.
				if strings.Contains(piece, fnCallsOpen) || strings.Contains(piece, "<invoke") {
					for _, c := range parseFunctionInvokes(piece, in.toolsRaw) {
						c["index"] = in.callIndex
						in.callIndex++
						toolCalls = append(toolCalls, c)
					}
					piece = stripFunctionCalls(piece)
				}
				if piece != "" {
					content = append(content, piece)
				}
			}
			in.offset += start
		}
		bodyStart := in.offset + markerLen
		idx, endMarkerLen := findAgentEndMarker(in.buffer[bodyStart:], final)
		if idx < 0 {
			if !final {
				break // incomplete block: wait for more chunks
			}
			// No closing marker at end of stream: recover the call if the payload is
			// complete, else show the text rather than drop it.
			tail := strings.TrimSpace(in.buffer[bodyStart:])
			name, args, ok := agentLooseParse(tail)
			if callName != "" {
				name, args, ok = agentNamedCall(callName, tail)
			}
			if name = canonicalToolName(name, names); ok && name != "" {
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
				content = append(content, in.buffer[in.offset:])
			}
			in.offset = len(in.buffer)
			break
		}
		end := bodyStart + idx
		raw := strings.TrimSpace(in.buffer[bodyStart:end])
		var name string
		var args json.RawMessage
		var ok bool
		if callName != "" {
			name, args, ok = agentNamedCall(callName, raw)
		} else {
			name, args, ok = agentLooseParse(raw)
		}
		name = canonicalToolName(name, names)
		if ok && name != "" {
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
			// Unparsable: leave it as visible text.
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

// The modern shim here and the legacy one in agent_legacy.go expose slightly
// different APIs. These adapters present one surface, so the handlers select the
// active shim purely from config.

// agentNativeCurrentTurnText wraps the latest user turn with the tool contract.
// It rides the USER turn, not a system message, because chat.z.ai ignores tool
// instructions placed in a system message. No tools means plain text.
func agentNativeCurrentTurnText(text string, tools []openAITool) string {
	if len(tools) == 0 {
		return text
	}
	var b strings.Builder
	b.WriteString(agentSystemPrefix)
	b.WriteString("\n\n<tools>\n")
	b.WriteString(renderAgentTools(tools))
	b.WriteString("\n</tools>\n\n<current_task>\n")
	b.WriteString(text)
	b.WriteString("\n</current_task>\n\n")
	b.WriteString(agentNativeReminder)
	return b.String()
}

// renderAssistantContent is renderAssistantTurn without the <assistant> wrapper:
// prior text plus any tool_calls re-rendered as <<<TOOL_CALL>>> blocks.
func renderAssistantContent(m agentMessage) string {
	text := contentToText(m.Content)
	if len(m.ToolCalls) == 0 {
		return text
	}
	var blocks []string
	if text != "" {
		blocks = append(blocks, text)
	}
	for _, call := range m.ToolCalls {
		if block := renderToolCallBlock(call); block != "" {
			blocks = append(blocks, block)
		}
	}
	return strings.Join(blocks, "\n")
}

// agentNativeUserMessage renders a user turn. The current turn carries the tool
// contract and keeps its images; earlier turns fold to text so a stale image
// never rides a new turn.
func agentNativeUserMessage(m agentMessage, isCurrent bool, tools []openAITool) map[string]interface{} {
	text := contentToText(m.Content)
	if isCurrent {
		text = agentNativeCurrentTurnText(text, tools)
		if imgs := imagePartsOfContent(m.Content); len(imgs) > 0 {
			content := make([]interface{}, 0, len(imgs)+1)
			content = append(content, map[string]interface{}{"type": "text", "text": text})
			for _, img := range imgs {
				content = append(content, img)
			}
			return map[string]interface{}{"role": "user", "content": content}
		}
	}
	return map[string]interface{}{"role": "user", "content": text}
}

// buildAgentNativeMessages maps the conversation onto the system/user/assistant
// roles Z.AI accepts: client system stays system; assistant tool_calls render as
// <<<TOOL_CALL>>> blocks; tool results become <tool_result> user turns; the tool
// contract rides the latest input turn — the last user turn, or the last tool
// result in a multi-round loop — so the model always sees the callable tools.
func buildAgentNativeMessages(msgs []agentMessage, tools []openAITool) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(msgs)+1)

	roles := make([]string, len(msgs))
	for i := range msgs {
		roles[i] = strings.ToLower(strings.TrimSpace(msgs[i].Role))
	}

	var sys strings.Builder
	for i, m := range msgs {
		if roles[i] != "system" {
			continue
		}
		if t := contentToText(m.Content); t != "" {
			if sys.Len() > 0 {
				sys.WriteString("\n\n")
			}
			sys.WriteString(t)
		}
	}
	if sys.Len() > 0 {
		out = append(out, map[string]interface{}{"role": "system", "content": sys.String()})
	}

	lastInput := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if roles[i] == "user" || roles[i] == "tool" {
			lastInput = i
			break
		}
	}

	for i, m := range msgs {
		switch roles[i] {
		case "system":
			continue
		case "assistant":
			out = append(out, map[string]interface{}{
				"role":    "assistant",
				"content": renderAssistantContent(m),
			})
		case "tool":
			content := renderToolResult(m)
			if i == lastInput {
				content = agentNativeCurrentTurnText(content, tools)
			}
			out = append(out, map[string]interface{}{
				"role":    "user",
				"content": content,
			})
		case "user":
			out = append(out, agentNativeUserMessage(m, i == lastInput, tools))
		default:
			out = append(out, map[string]interface{}{
				"role":    "user",
				"content": contentToText(m.Content),
			})
		}
	}

	if len(out) == 0 {
		out = append(out, map[string]interface{}{"role": "user", "content": ""})
	}
	return out
}

// transformMessagesForAgentNative preserves conversation roles so the model
// answers only the latest turn.
func transformMessagesForAgentNative(rawMessages, toolsRaw json.RawMessage) ([]byte, error) {
	var msgs []agentMessage
	if err := json.Unmarshal(rawMessages, &msgs); err != nil {
		return nil, fmt.Errorf("agent transform (native): parse messages: %w", err)
	}
	var tools []openAITool
	if len(toolsRaw) > 0 {
		_ = json.Unmarshal(toolsRaw, &tools)
	}
	return json.Marshal(buildAgentNativeMessages(msgs, tools))
}

// transformMessagesForAgentModern folds conversation and contract into one
// sectioned prompt, wrapped as a single user message.
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

// agentTransformMessages rewrites the messages array for the active shim.
func agentTransformMessages(rawMessages, toolsRaw json.RawMessage) ([]byte, error) {
	if config.agentModern() {
		if config.agentNative() {
			return transformMessagesForAgentNative(rawMessages, toolsRaw)
		}
		return transformMessagesForAgentModern(rawMessages, toolsRaw)
	}
	var tools []interface{}
	if len(toolsRaw) > 0 {
		_ = json.Unmarshal(toolsRaw, &tools)
	}
	return transformMessagesForAgent(rawMessages, tools)
}

// agentExtractToolCalls lifts tool calls out of finished assistant text.
func agentExtractToolCalls(text string, toolsRaw json.RawMessage) []map[string]interface{} {
	if config.agentModern() {
		return ParseAgentToolCalls(text, toolsRaw)
	}
	return extractAgentToolCalls(text)
}

// agentStripToolCalls removes them, leaving the residual text.
func agentStripToolCalls(text string, toolsRaw json.RawMessage) string {
	if config.agentModern() {
		return StripAgentToolCalls(text, toolsRaw)
	}
	return stripAgentToolCallBlocks(text)
}

// agentInterceptor is the streaming surface both handlers use: feed takes one
// upstream chunk, finish drains the tail.
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
// trailing content only; end-of-stream calls fall to the caller's
// agentExtractToolCalls safety net.
type legacyAgentInterceptor struct{ in *agentStreamInterceptor }

func (l *legacyAgentInterceptor) feed(chunk string) (string, []map[string]interface{}) {
	content, toolCalls, _ := l.in.feed(chunk)
	return content, toolCalls
}

func (l *legacyAgentInterceptor) finish() (string, []map[string]interface{}) {
	return l.in.flushFinal(), nil
}

// newAgentInterceptor builds the interceptor for the active shim.
func newAgentInterceptor(toolsRaw json.RawMessage) agentInterceptor {
	if config.agentModern() {
		return &modernAgentInterceptor{in: &AgentStreamInterceptor{toolsRaw: toolsRaw}}
	}
	return &legacyAgentInterceptor{in: newAgentStreamInterceptor()}
}
