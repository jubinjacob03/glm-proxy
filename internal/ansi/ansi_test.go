package ansi

import "testing"

// A stray ESC used to make Strip discard everything up to the next 'm' anywhere in
// the line, so one bad byte from an upstream payload could erase a whole log entry.
func TestStripHandlesMalformedEscapes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "hello", "hello"},
		{"sgr colour", "\x1b[36mhello\x1b[0m", "hello"},
		{"bare esc then m later", "a\x1bbcdmefg", "abcdmefg"},
		{"bare esc at end", "abc\x1b", "abc"},
		{"esc bracket unterminated", "abc\x1b[36", "abc[36"},
		{"non sgr csi", "a\x1b[2Jb", "ab"},
		{"cursor move", "a\x1b[10;20Hb", "ab"},
		{"two colours", "\x1b[1m\x1b[95mPOST\x1b[0m\x1b[0m /v1", "POST /v1"},
		{"m inside text after esc", "x\x1b[0mmodel=glm", "xmodel=glm"},
	}
	for _, c := range cases {
		if got := Strip(c.in); got != c.want {
			t.Errorf("%s: Strip(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
		if got := VisibleLen([]byte(c.in)); got != len(c.want) {
			t.Errorf("%s: VisibleLen(%q) = %d, want %d", c.name, c.in, got, len(c.want))
		}
	}
}

func TestTruncateVisibleKeepsColourAndCloses(t *testing.T) {
	// Limit lands inside the coloured run, so the reset must be synthesised.
	got := string(TruncateVisible(nil, []byte("\x1b[36mabcdefghij\x1b[0m"), 4))
	if got != "\x1b[36mabcd\x1b[0m" {
		t.Errorf("got %q, want %q", got, "\x1b[36mabcd\x1b[0m")
	}

	// Already closed before the cut: no second reset.
	got = string(TruncateVisible(nil, []byte("\x1b[36mab\x1b[0mcdefgh"), 4))
	if got != "\x1b[36mab\x1b[0mcd" {
		t.Errorf("got %q, want %q", got, "\x1b[36mab\x1b[0mcd")
	}

	// The budget is in bytes, matching LOG_CONSOLE_WIDTH, and a rune is never
	// split: 4 bytes admits one 3-byte rune, not one and two thirds.
	if got = string(TruncateVisible(nil, []byte("你好世界"), 4)); got != "你" {
		t.Errorf("got %q, want %q", got, "你")
	}
	if got = string(TruncateVisible(nil, []byte("你好世界"), 6)); got != "你好" {
		t.Errorf("got %q, want %q", got, "你好")
	}

	// Escapes do not consume the visible budget.
	in := []byte("\x1b[1m\x1b[95mPOST\x1b[0m\x1b[0m /v1/chat")
	if got := string(TruncateVisible(nil, in, 4)); Strip(got) != "POST" {
		t.Errorf("visible content = %q, want %q", Strip(got), "POST")
	}
}

func TestTruncateVisibleNoTrailingResetWhenNoColour(t *testing.T) {
	if got := string(TruncateVisible(nil, []byte("abcdefgh"), 3)); got != "abc" {
		t.Errorf("got %q, want %q", got, "abc")
	}
}
