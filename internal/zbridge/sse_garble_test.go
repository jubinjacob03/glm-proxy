// Regression tests for issue #23 (输出乱码的问题): garbled characters interspersed
// in streamed output. Three causes, each checked against Z.AI's own frontend:
//
//  1. edit_index is a UTF-16 code-unit offset, because the frontend applies
//     content.substring(0, edit_index) + edit_content. Treating it as a rune count
//     spliced at the wrong position whenever non-BMP characters appeared.
//  2. content is a full replacement, not an append.
//  3. Diffing by byte length and slicing content[sentLen:] landed inside a
//     multi-byte rune after any edit that changed the byte length, emitting
//     invalid UTF-8 that renders as U+FFFD. Partial "<det" fragments leaked the
//     same way.
//
// These feed synthetic streams shaped like the real protocol through the actual
// parser, replay them through the handler logic, and assert what an
// OpenAI-compatible client receives.

package zbridge

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// sseEvent wraps one payload as an SSE `data:` line.
func sseEvent(payload string) string {
	return "data: " + payload + "\n\n"
}

// runSSEParser drives the real streamSSEResponse over a synthetic body and
// collects every ZAIResult.
func runSSEParser(t *testing.T, body string) []ZAIResult {
	t.Helper()
	ch := make(chan ZAIResult, 4096)
	errCh := make(chan error, 1)
	go func() {
		// sendToZAI closes the channel once sendToZAIStream returns; mirror that
		// so the drain loop terminates.
		errCh <- streamSSEResponse(context.Background(), strings.NewReader(body), ch)
		close(ch)
	}()
	var results []ZAIResult
	for r := range ch {
		results = append(results, r)
	}
	if err := <-errCh; err != nil {
		t.Fatalf("streamSSEResponse returned error: %v", err)
	}
	return results
}

// replayClientView mirrors the handlers' accumulation exactly: the parser owns
// diffing, the handler forwards result.Chunk and tracks the FullText snapshot.
func replayClientView(results []ZAIResult) (clientText string, deltas []string) {
	fullContent := ""
	for _, result := range results {
		if result.Reasoning != "" {
			continue // reasoning travels on its own channel
		}
		if result.FullText != "" {
			fullContent = result.FullText
		} else {
			fullContent += result.Chunk
		}
		delta := result.Chunk
		if delta == "" {
			continue
		}
		deltas = append(deltas, delta)
	}
	return strings.Join(deltas, ""), deltas
}

// assertValidUTF8 fails if any client-visible delta holds invalid UTF-8, which
// json.Marshal would turn into the U+FFFD garble from issue #23.
func assertValidUTF8(t *testing.T, deltas []string) {
	t.Helper()
	for i, d := range deltas {
		if !utf8.ValidString(d) {
			t.Errorf("client delta #%d is INVALID UTF-8 (renders as replacement-char garble): %q", i, d)
		}
	}
}

// withHoldback runs fn under a temporary StreamHoldback.
func withHoldback(t *testing.T, n int, fn func()) {
	t.Helper()
	old := config.StreamHoldback
	config.StreamHoldback = n
	defer func() { config.StreamHoldback = old }()
	fn()
}

// Baseline: a pure delta_content append must stay clean.

func TestSSEBaselinePureDeltaAppend(t *testing.T) {
	body := sseEvent(`{"data":{"delta_content":"你好，"}}`) +
		sseEvent(`{"data":{"delta_content":"世界 🙂"}}`) +
		sseEvent(`{"data":{"phase":"done"}}`) +
		"data: [DONE]\n\n"

	for _, hb := range []int{0, 24} {
		withHoldback(t, hb, func() {
			results := runSSEParser(t, body)
			clientText, deltas := replayClientView(results)
			assertValidUTF8(t, deltas)
			if clientText != "你好，世界 🙂" {
				t.Errorf("holdback=%d: client text = %q, want %q", hb, clientText, "你好，世界 🙂")
			}
		})
	}
}

// The core repro. Upstream streams "Hello 你好", then revises from UTF-16 index 6
// to "世界！". With hold-back disabled — worst case, the rewrite touches text
// already forwarded — the client must still receive only valid UTF-8. The old code
// sliced mid-rune here and emitted raw continuation bytes.

func TestSSEEditContentRevisionNeverEmitsGarble(t *testing.T) {
	body := sseEvent(`{"data":{"delta_content":"Hello 你好"}}`) +
		sseEvent(`{"data":{"edit_content":"世界！","edit_index":6}}`) +
		sseEvent(`{"data":{"phase":"done"}}`) +
		"data: [DONE]\n\n"

	withHoldback(t, 0, func() {
		results := runSSEParser(t, body)
		clientText, deltas := replayClientView(results)
		assertValidUTF8(t, deltas)
		// The stale "你好" cannot be taken back from an append-only client, but the
		// corrected tail must follow it intact.
		if !strings.HasSuffix(clientText, "世界！") {
			t.Errorf("client text = %q, must end with the corrected tail %q", clientText, "世界！")
		}
	})
}

// With the default window the revision lands inside the pending region, so the
// client converges exactly on the upstream's final text: no stale fragment.
func TestSSEEditContentRevisionAbsorbedByHoldback(t *testing.T) {
	body := sseEvent(`{"data":{"delta_content":"Hello 你好"}}`) +
		sseEvent(`{"data":{"edit_content":"世界！","edit_index":6}}`) +
		sseEvent(`{"data":{"phase":"done"}}`) +
		"data: [DONE]\n\n"

	withHoldback(t, 24, func() {
		results := runSSEParser(t, body)
		clientText, deltas := replayClientView(results)
		assertValidUTF8(t, deltas)
		want := "Hello 世界！"
		if clientText != want {
			t.Errorf("client text = %q, want %q", clientText, want)
		}
	})
}

// A deep edit far beyond the window must still stay readable.

func TestSSEDeepEditStaysValidUTF8(t *testing.T) {
	// 30 Chinese runes (90 bytes), then a backtrack to rune 2 that retypes a
	// longer ASCII/Chinese passage, as real output mixes markdown and spaces. The
	// old byte-length diff sliced the new content at the old offset, mid-rune.
	longPrefix := strings.Repeat("长", 30)
	body := sseEvent(fmt.Sprintf(`{"data":{"delta_content":"%s"}}`, longPrefix)) +
		sseEvent(`{"data":{"edit_content":"短文abc`+longPrefix+`","edit_index":2}}`) +
		sseEvent(`{"data":{"phase":"done"}}`) +
		"data: [DONE]\n\n"

	withHoldback(t, 0, func() {
		results := runSSEParser(t, body)
		_, deltas := replayClientView(results)
		assertValidUTF8(t, deltas)
	})
}

// A <details> tag split across events. Upstream streams the opener a fragment at a
// time; the old parser emitted the partial "<det" as content, then sliced mid-rune
// when the tag completed and content shrank back — exactly the
// "穿插着一些无法查看的乱码" pattern from the report.

func TestSSEDetailsTagSplitAcrossEvents(t *testing.T) {
	body := sseEvent(`{"data":{"delta_content":"答案"}}`) +
		sseEvent(`{"data":{"delta_content":"<det"}}`) +
		sseEvent(`{"data":{"delta_content":"ails>思考过程</details>"}}`) +
		sseEvent(`{"data":{"delta_content":"正文内容"}}`) +
		sseEvent(`{"data":{"phase":"done"}}`) +
		"data: [DONE]\n\n"

	for _, hb := range []int{0, 24} {
		withHoldback(t, hb, func() {
			results := runSSEParser(t, body)
			clientText, deltas := replayClientView(results)

			assertValidUTF8(t, deltas)
			if strings.Contains(clientText, "<det") || strings.Contains(clientText, "<details") {
				t.Errorf("holdback=%d: <details> tag fragment leaked into client content: %q", hb, clientText)
			}
			want := "答案正文内容"
			if clientText != want {
				t.Errorf("holdback=%d: client text = %q, want %q", hb, clientText, want)
			}

			// Reasoning must still arrive intact on its own channel.
			reasoning := ""
			for _, r := range results {
				reasoning += r.Reasoning
			}
			if reasoning != "思考过程" {
				t.Errorf("holdback=%d: reasoning = %q, want %q", hb, reasoning, "思考过程")
			}
		})
	}
}

// edit_index counts UTF-16 units, not runes. "A🙂" is 2 runes but 3 units, so an
// edit right after the emoji arrives as edit_index=3. The old rune-based conversion
// spliced one character late and silently corrupted the text.

func TestSSEEditIndexIsUTF16Units(t *testing.T) {
	body := sseEvent(`{"data":{"delta_content":"A🙂B"}}`) +
		// Index 3 lands after "A" (1 unit) plus "🙂" (2 units).
		sseEvent(`{"data":{"edit_content":"C","edit_index":3}}`) +
		sseEvent(`{"data":{"phase":"done"}}`) +
		"data: [DONE]\n\n"

	// Hold-back absorbs the rewrite, so the client converges exactly. The old
	// conversion put index 3 after the "B" (3 runes) and appended instead of
	// replacing, so the client saw "A🙂BC".
	withHoldback(t, 24, func() {
		results := runSSEParser(t, body)
		clientText, deltas := replayClientView(results)
		assertValidUTF8(t, deltas)
		want := "A🙂C"
		if clientText != want {
			t.Errorf("client text = %q, want %q (edit_index must be interpreted as UTF-16 units)", clientText, want)
		}
	})
}

// Pure BMP text, where units and runes coincide, must keep working.
func TestSSEEditIndexBMPText(t *testing.T) {
	body := sseEvent(`{"data":{"delta_content":"你好世界"}}`) +
		sseEvent(`{"data":{"edit_content":"地球","edit_index":2}}`) +
		sseEvent(`{"data":{"phase":"done"}}`) +
		"data: [DONE]\n\n"

	// Hold-back absorbs the rewrite, so the client converges exactly.
	withHoldback(t, 24, func() {
		results := runSSEParser(t, body)
		clientText, deltas := replayClientView(results)
		assertValidUTF8(t, deltas)
		want := "你好地球"
		if clientText != want {
			t.Errorf("client text = %q, want %q", clientText, want)
		}
	})
}

// content events are full replacements, not appends: the frontend does
// lt.content = rr ("全量更新"). The old parser appended them, duplicating the whole
// message whenever upstream sent a snapshot.

func TestSSEContentFieldIsFullReplacement(t *testing.T) {
	body := sseEvent(`{"data":{"delta_content":"partial text"}}`) +
		sseEvent(`{"data":{"content":"完整的最终文本"}}`) +
		sseEvent(`{"data":{"phase":"done"}}`) +
		"data: [DONE]\n\n"

	// Hold-back keeps "partial text" pending, so the replacement lands before
	// anything reaches the client: it must see only the snapshot.
	withHoldback(t, 24, func() {
		results := runSSEParser(t, body)
		clientText, deltas := replayClientView(results)
		assertValidUTF8(t, deltas)
		if strings.Contains(clientText, "partial text") {
			t.Errorf("content snapshot must REPLACE, client text = %q", clientText)
		}
		if !strings.HasSuffix(clientText, "完整的最终文本") {
			t.Errorf("client text = %q, must end with the snapshot", clientText)
		}
	})
}

// A realistic-length stream, well past the window, mixing Chinese, ASCII and emoji
// with periodic tail edits. Every chunk must be valid UTF-8, every FullText must
// equal the running concatenation, and the final text must match upstream.

func TestSSELongStreamWithTailEditsContract(t *testing.T) {
	var b strings.Builder
	// 40 events of mixed text, then a tail edit inside the window.
	for i := 0; i < 40; i++ {
		b.WriteString(sseEvent(fmt.Sprintf(`{"data":{"delta_content":"第%d段。"}}`, i)))
	}
	// Replace the last two events' text, inside the 24-rune window. Segments 0-9
	// are 4 runes and 10-37 are 5, so 10×4 + 28×5 = 180 units precede 38-39.
	b.WriteString(sseEvent(`{"data":{"edit_content":"最终段。","edit_index":180}}`))
	b.WriteString(sseEvent(`{"data":{"delta_content":"收尾 🙂"}}`))
	b.WriteString(sseEvent(`{"data":{"phase":"done"}}`))
	b.WriteString("data: [DONE]\n\n")

	withHoldback(t, 24, func() {
		results := runSSEParser(t, b.String())
		clientText, deltas := replayClientView(results)

		assertValidUTF8(t, deltas)

		// The contract: every chunk is a suffix of its FullText snapshot, never a
		// mid-string slice, and the last snapshot equals upstream's final content.
		for i, r := range results {
			if r.Reasoning != "" || r.Chunk == "" {
				continue
			}
			if !strings.HasSuffix(r.FullText, r.Chunk) {
				t.Fatalf("result #%d: chunk %q is not a suffix of snapshot %q", i, r.Chunk, r.FullText)
			}
		}
		last := ""
		for _, r := range results {
			if r.FullText != "" {
				last = r.FullText
			}
		}

		want := strings.Builder{}
		for i := 0; i < 38; i++ {
			want.WriteString(fmt.Sprintf("第%d段。", i))
		}
		want.WriteString("最终段。收尾 🙂")
		if last != want.String() {
			t.Errorf("final snapshot = %q, want %q", last, want.String())
		}
		if clientText != want.String() {
			t.Errorf("client text = %q, want %q", clientText, want.String())
		}
	})
}

// Unit tests for the helpers.

func TestUTF16IndexToByteIndex(t *testing.T) {
	cases := []struct {
		s    string
		idx  int
		want int
	}{
		{"", 5, 0},       // empty string
		{"abc", 0, 0},    // zero
		{"abc", -3, 0},   // negative
		{"abc", 2, 2},    // ASCII
		{"abc", 99, 3},   // beyond end -> clamp
		{"你好", 1, 3},     // BMP Chinese: 1 unit = 1 rune = 3 bytes
		{"你好", 2, 6},     // end
		{"A🙂B", 0, 0},    // before emoji
		{"A🙂B", 1, 1},    // after 'A'
		{"A🙂B", 2, 1},    // inside the surrogate pair -> clamp to rune start
		{"A🙂B", 3, 5},    // after the emoji (1 + 4 bytes)
		{"A🙂B", 4, 6},    // after 'B'
		{"A🙂B", 100, 6},  // beyond end
		{"你好🙂世界", 4, 10}, // 2+2 units then emoji: bytes 6+4=10
	}
	for _, c := range cases {
		if got := utf16IndexToByteIndex(c.s, c.idx); got != c.want {
			t.Errorf("utf16IndexToByteIndex(%q, %d) = %d, want %d", c.s, c.idx, got, c.want)
		}
		// The result must always land on a rune boundary.
		got := utf16IndexToByteIndex(c.s, c.idx)
		if got < len(c.s) && !utf8.RuneStart(c.s[got]) {
			t.Errorf("utf16IndexToByteIndex(%q, %d) = %d lands mid-rune", c.s, c.idx, got)
		}
	}
}

func TestCommonPrefixLenRuneSafe(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"abc", "abd", 2},
		{"你好世界", "你好地球", 6},
		{"A🙂B", "A🙂C", 5},
		{"abc", "abc", 3},
		{"abc", "xabc", 0},
	}
	for _, c := range cases {
		if got := commonPrefixLen(c.a, c.b); got != c.want {
			t.Errorf("commonPrefixLen(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestSSEEmitterDelta(t *testing.T) {
	// Plain growth emits the new suffix.
	e := &sseEmitter{}
	if got := e.delta("abc"); got != "abc" {
		t.Errorf("growth from empty: got %q, want %q", got, "abc")
	}
	if got := e.delta("abcdef"); got != "def" {
		t.Errorf("growth: got %q, want %q", got, "def")
	}
	if got := e.delta("abcdef"); got != "" {
		t.Errorf("no-op: got %q, want empty", got)
	}

	// Multibyte growth stays rune-aligned too.
	e2 := &sseEmitter{}
	if got := e2.delta("你好"); got != "你好" {
		t.Errorf("multibyte growth: got %q", got)
	}
	if got := e2.delta("你好世界"); got != "世界" {
		t.Errorf("multibyte growth delta: got %q, want %q", got, "世界")
	}

	// Deep truncation, where target is a prefix of the view: nothing is sent and
	// the view is not rewound, so later growth does not re-emit what the client
	// already has.
	e3 := &sseEmitter{}
	e3.delta("你好世界天地")
	if got := e3.delta("你好世界"); got != "" {
		t.Errorf("shrink must emit nothing, got %q", got)
	}
	if got := e3.delta("你好世界天地人"); got != "人" {
		t.Errorf("growth after shrink: got %q, want %q", got, "人")
	}

	// A rewrite inside the view re-syncs from the common prefix, still valid UTF-8.
	e4 := &sseEmitter{}
	e4.delta("Hello 你好")
	got := e4.delta("Hello 世界！")
	if got != "世界！" {
		t.Errorf("rewrite re-sync: got %q, want %q", got, "世界！")
	}
	if !utf8.ValidString(got) {
		t.Errorf("rewrite re-sync produced invalid UTF-8: %q", got)
	}

	// A full rewrite shares no prefix at all.
	e5 := &sseEmitter{}
	e5.delta("abc")
	if got := e5.delta("xyz"); got != "xyz" {
		t.Errorf("full rewrite: got %q, want %q", got, "xyz")
	}
}

func TestHoldBackTail(t *testing.T) {
	cases := []struct {
		s    string
		n    int
		want string
	}{
		{"", 5, ""},
		{"abc", 0, "abc"},
		{"abc", 2, "a"},
		{"abc", 99, ""},
		{"你好世界", 2, "你好"},
		{"A🙂B", 1, "A🙂"},
		{"A🙂B", 2, "A"},
	}
	for _, c := range cases {
		got := holdBackTail(c.s, c.n)
		if got != c.want {
			t.Errorf("holdBackTail(%q, %d) = %q, want %q", c.s, c.n, got, c.want)
		}
		if !utf8.ValidString(got) {
			t.Errorf("holdBackTail(%q, %d) produced invalid UTF-8: %q", c.s, c.n, got)
		}
	}
}

func TestHoldBackPartialDetailsTag(t *testing.T) {
	cases := []struct{ s, want string }{
		{"abc", "abc"},
		{"abc<", "abc"},
		{"abc<d", "abc"},
		{"abc<det", "abc"},
		{"abc<details", "abc"}, // complete opener literal held
		{"abc</det", "abc"},
		{"abc</details>", "abc</details>"},   // complete close tag stays (literal text)
		{"abc<details>", "abc<details>"},     // splitDetails owns complete openers; not this layer
		{"abc<div>", "abc<div>"},             // unrelated tag stays
		{"abc<details> x", "abc<details> x"}, // opener already followed by more text stays
	}
	for _, c := range cases {
		if got := holdBackPartialDetailsTag(c.s); got != c.want {
			t.Errorf("holdBackPartialDetailsTag(%q) = %q, want %q", c.s, got, c.want)
		}
	}
}

func TestSplitDetails(t *testing.T) {
	cases := []struct {
		raw         string
		wantReason  string
		wantContent string
	}{
		{"plain text", "", "plain text"},
		{"<details>think</details>answer", "think", "answer"},
		{"before<details>think</details>after", "think", "beforeafter"},
		// A fragment like "<det" passes through splitDetails; the emission layer
		// holds it back while streaming and only releases it as literal text on
		// the final flush, if the stream really ends there.
		{"answer<det", "", "answer<det"},
		// A complete opener with no '>' yet is held pending entirely.
		{"answer<details", "", "answer"},
		{"answer<details sty", "", "answer"},
		// Opener complete, reasoning still streaming.
		{"a<details>thinking so far", "thinking so far", "a"},
		// Several blocks concatenate into one reasoning body.
		{"<details>t1</details>x<details>t2</details>y", "t1t2", "xy"},
		// Attributes on the opener must not break the match.
		{"<details type=\"thinking\">t</details>ans", "t", "ans"},
	}
	for _, c := range cases {
		r, content := splitDetails(c.raw)
		if r != c.wantReason || content != c.wantContent {
			t.Errorf("splitDetails(%q) = (%q, %q), want (%q, %q)",
				c.raw, r, content, c.wantReason, c.wantContent)
		}
	}
}
