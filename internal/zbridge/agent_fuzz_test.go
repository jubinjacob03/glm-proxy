package zbridge

import (
	"encoding/json"
	"strings"
	"testing"
)

var fuzzTools = json.RawMessage(`[
 {"type":"function","function":{"name":"TodoWrite","parameters":{"type":"object","properties":{"todos":{"type":"array"},"merge":{"type":"boolean"}}}}},
 {"type":"function","function":{"name":"Write","parameters":{"type":"object","properties":{"file_path":{"type":"string"},"content":{"type":"string"}}}}},
 {"type":"function","function":{"name":"LS","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}
]`)

var fuzzSeeds = []string{
	"",
	"plain prose with no markers at all",
	`<<<TOOL_CALL>>>{"name":"LS","arguments":{"path":"."}}<<<END_TOOL_CALL>>>`,
	`<<<TodoWrite>>>{"todos":[],"merge":false}<<<END_TOOL_CALL>>>`,
	`<<<TOOL_CALL: LS>>>{"path":"."}<<<END_TOOL_CALL>>>`,
	`<<< TOOL_CALL >>>{"name":"LS","arguments":{"path":"."}}<<< END_TOOL_CALL >>>`,
	`<<<TOOL_CALL>>>{"name":"LS"}<<</TOOL_CALL>>>`,
	`<<<TOOL_CALL>>>{"name":"LS","arguments":{"path":".",}}`,
	"<<<TOOL_CALL>>>{\"name\":\"Write\",\"arguments\":{\"content\":\"a\nb\"}}<<<END_TOOL_CALL>>>",
	"<<<",
	"<<<<<<<<<<TOOL_CALL",
	"<<<TOOL_CALL>>>",
	"<<<END_TOOL_CALL>>>",
	"</LS>",
	"<<<LS>>>",
	"<invoke name=\"LS\">",
	`<function_calls><invoke name="LS"><parameter name="path">.</parameter></invoke></function_calls>`,
	"```json\n<<<TOOL_CALL>>>{}<<<END_TOOL_CALL>>>\n```",
	"\xff\xfe invalid utf8 \x00 bytes",
	"<<<TOOL_CALL>>>" + strings.Repeat("{", 200),
	"<<<TOOL_CALL: >>>{}<<<END_TOOL_CALL>>>",
	"<<<TOOL_CALL" + strings.Repeat(" ", 80) + ">>>{}<<<END_TOOL_CALL>>>",
}

// FuzzAgentToolCallParsing asserts the finished-text path never panics and every
// emitted call carries a name and valid JSON arguments.
func FuzzAgentToolCallParsing(f *testing.F) {
	for _, s := range fuzzSeeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, text string) {
		calls := ParseAgentToolCalls(text, fuzzTools)
		stripped := StripAgentToolCalls(text, fuzzTools)

		for _, c := range calls {
			fn, ok := c["function"].(map[string]interface{})
			if !ok {
				t.Fatalf("call without function object: %#v", c)
			}
			name, _ := fn["name"].(string)
			if name == "" {
				t.Fatalf("call with empty name: %#v", c)
			}
			args, _ := fn["arguments"].(string)
			if !json.Valid([]byte(args)) {
				t.Fatalf("call arguments are not valid JSON: %q", args)
			}
		}
		if len(stripped) > len(text)+8 {
			t.Fatalf("strip grew the text: %d -> %d", len(text), len(stripped))
		}
	})
}

// FuzzAgentInterceptorChunking is the core invariant: streaming in arbitrary
// chunks must yield exactly what one whole feed yields.
func FuzzAgentInterceptorChunking(f *testing.F) {
	for _, s := range fuzzSeeds {
		f.Add(s, 3)
	}
	f.Fuzz(func(t *testing.T, text string, step int) {
		if step < 1 || step > 512 {
			step = 3
		}

		drive := func(size int) (string, []string) {
			in := &AgentStreamInterceptor{toolsRaw: fuzzTools}
			var content strings.Builder
			var names []string
			collect := func(p AgentParsedChunk) {
				content.WriteString(p.Content)
				for _, c := range p.ToolCalls {
					fn, ok := c["function"].(map[string]interface{})
					if !ok {
						t.Fatalf("call without function object: %#v", c)
					}
					n, _ := fn["name"].(string)
					if n == "" {
						t.Fatalf("streamed call with empty name: %#v", c)
					}
					names = append(names, n)
				}
			}
			for i := 0; i < len(text); i += size {
				e := i + size
				if e > len(text) {
					e = len(text)
				}
				collect(in.Feed(text[i:e]))
			}
			collect(in.Finish())
			return content.String(), names
		}

		gotContent, gotNames := drive(step)
		wantContent, wantNames := drive(len(text) + 1)

		if gotContent != wantContent {
			t.Fatalf("chunked content differs from whole feed\nstep=%d\n chunked=%q\n  whole=%q",
				step, gotContent, wantContent)
		}
		if strings.Join(gotNames, ",") != strings.Join(wantNames, ",") {
			t.Fatalf("chunked calls differ from whole feed: %v vs %v", gotNames, wantNames)
		}
	})
}
