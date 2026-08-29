// Package ansi holds the small set of SGR colours the console output uses.
//
// The log writer strips these before a line reaches log.txt, so grep never has to
// step over escape codes. NO_COLOR or LOG_COLOR=false turns colour off.
package ansi

import (
	"os"
	"strings"
	"unicode/utf8"
)

var Enabled = detect()

// Refresh re-reads the environment: package init runs before .env is loaded.
func Refresh() { Enabled = detect() }

func detect() bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LOG_COLOR"))) {
	case "0", "false", "no", "off":
		return false
	}
	return true
}

const (
	reset  = "\x1b[0m"
	escape = '\x1b'
)

func wrap(code, s string) string {
	if !Enabled || s == "" {
		return s
	}
	return code + s + reset
}

func Cyan(s string) string   { return wrap("\x1b[36m", s) }
func Violet(s string) string { return wrap("\x1b[95m", s) }
func Green(s string) string  { return wrap("\x1b[32m", s) }
func Yellow(s string) string { return wrap("\x1b[33m", s) }
func Red(s string) string    { return wrap("\x1b[31m", s) }
func Blue(s string) string   { return wrap("\x1b[94m", s) }
func Grey(s string) string   { return wrap("\x1b[90m", s) }
func Bold(s string) string   { return wrap("\x1b[1m", s) }

// escapeEnd returns the index past the CSI sequence at i. A malformed one costs only
// the ESC byte; scanning ahead for a terminator would eat the rest of the line.
func escapeEnd(p []byte, i int) int {
	if i+1 >= len(p) || p[i+1] != '[' {
		return i + 1
	}
	// Parameter and intermediate bytes, then a final byte, per ECMA-48.
	j := i + 2
	for j < len(p) && p[j] >= 0x20 && p[j] <= 0x3f {
		j++
	}
	if j < len(p) && p[j] >= 0x40 && p[j] <= 0x7e {
		return j + 1
	}
	return i + 1
}

// Strip removes every escape sequence, for the file writer and for width maths.
func Strip(s string) string {
	if !strings.ContainsRune(s, escape) {
		return s
	}
	return string(StripBytes([]byte(s)))
}

// StripBytes is Strip over bytes, returning p untouched when it holds no escape.
func StripBytes(p []byte) []byte {
	if !hasEscape(p) {
		return p
	}
	out := make([]byte, 0, len(p))
	for i := 0; i < len(p); {
		if p[i] == escape {
			i = escapeEnd(p, i)
			continue
		}
		out = append(out, p[i])
		i++
	}
	return out
}

// TruncateVisible appends p to dst until limit printable bytes have been copied,
// keeping escape sequences intact and closing any colour still open at the cut, so
// a shortened line keeps its colour instead of bleeding into the next one.
func TruncateVisible(dst, p []byte, limit int) []byte {
	visible, open, i := 0, false, 0
	for i < len(p) && visible < limit {
		if p[i] == escape {
			j := escapeEnd(p, i)
			dst = append(dst, p[i:j]...)
			open = string(p[i:j]) != reset
			i = j
			continue
		}
		// Whole runes only: bodies carry CJK and emoji.
		size := 1
		if p[i] >= utf8.RuneSelf {
			_, size = utf8.DecodeRune(p[i:])
		}
		if visible+size > limit {
			break
		}
		dst = append(dst, p[i:i+size]...)
		visible += size
		i += size
	}
	if open {
		dst = append(dst, reset...)
	}
	return dst
}

// VisibleLen counts the printable bytes, ignoring colour.
func VisibleLen(p []byte) int {
	if !hasEscape(p) {
		return len(p)
	}
	return len(StripBytes(p))
}

func hasEscape(p []byte) bool {
	for _, b := range p {
		if b == escape {
			return true
		}
	}
	return false
}
