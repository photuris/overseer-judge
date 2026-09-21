// Package session classifies the transcript tail of a coding agent's
// terminal pane into one of six states.
package session

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Cleaning limits the session verb applies to every tail.
const (
	MaxLines = 200
	MaxBytes = 12000
)

// esc is the byte that opens every ANSI escape sequence.
const esc = 0x1b

// Clean prepares a raw pane tail for judgment. Steps, in order:
//
// 1. Remove every carriage return.
//
// 1a. Remove faint text: every span that starts at an SGR sequence
// enabling faint (a CSI … m whose parameter list contains 2) and ends
// at the next SGR sequence whose parameters contain 0 or 22 or are
// empty, or at end of line, whichever comes first. Claude Code renders
// its greyed-out prompt suggestion this way; it is not typed input and
// must not reach the model. This is a no-op on input without escapes.
//
// 2. Strip ANSI escapes: CSI sequences, OSC sequences, and any other
// two-byte escape sequence.
//
// 3. Remove braille glyphs U+2800–U+28FF, which spinners are drawn
// with.
//
// 4. Split on newlines, trim trailing whitespace from each line, and
// collapse any run of three or more blank lines to a single blank
// line.
//
// 5. If maxLines is positive, keep only the last maxLines lines.
//
// 6. If maxBytes is positive and the joined text exceeds it, drop
// whole lines from the front until it fits; if one remaining line is
// still too long, keep its last maxBytes bytes, cut back to a rune
// boundary.
//
// A non-positive limit disables its step. Lines are re-joined with
// newlines and the result carries no trailing newline.
func Clean(raw string, maxLines, maxBytes int) string {
	text := strings.ReplaceAll(raw, "\r", "")
	text = removeFaint(text)
	text = stripEscapes(text)
	text = stripBraille(text)

	lines := collapseBlanks(strings.Split(text, "\n"))
	if maxLines > 0 && len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}

	return capBytes(lines, maxBytes)
}

// removeFaint deletes every faint-rendered span, escapes included.
func removeFaint(s string) string {
	var b strings.Builder

	for i := 0; i < len(s); {
		n, final, params, ok := csiAt(s, i)
		if ok && final == 'm' && hasParam(params, "2") {
			i = skipFaint(s, i+n)

			continue
		}
		b.WriteByte(s[i])
		i++
	}

	return b.String()
}

// skipFaint returns the offset just past the end of the faint span
// that starts at i, which is the end of the SGR sequence that turns
// faint off, or the newline that ends the line.
func skipFaint(s string, i int) int {
	for i < len(s) {
		if s[i] == '\n' {
			return i
		}

		n, final, params, ok := csiAt(s, i)
		if !ok {
			i++

			continue
		}
		if final == 'm' && endsFaint(params) {
			return i + n
		}
		i += n
	}

	return i
}

// endsFaint reports whether an SGR parameter list turns faint off.
func endsFaint(params string) bool {
	return params == "" ||
		hasParam(params, "0") || hasParam(params, "22")
}

// hasParam reports whether a semicolon-separated SGR parameter list
// contains want.
func hasParam(params, want string) bool {
	for p := range strings.SplitSeq(params, ";") {
		if p == want {
			return true
		}
	}

	return false
}

// csiAt reports whether a complete CSI sequence starts at s[i],
// returning its length, its final byte, and its parameter bytes.
func csiAt(s string, i int) (n int, final byte, params string, ok bool) {
	if i+1 >= len(s) || s[i] != esc || s[i+1] != '[' {
		return 0, 0, "", false
	}

	for j := i + 2; j < len(s); j++ {
		if s[j] >= 0x40 && s[j] <= 0x7e {
			return j - i + 1, s[j], s[i+2 : j], true
		}
	}

	return 0, 0, "", false
}

// stripEscapes removes every ANSI escape sequence. A sequence left
// unterminated by a truncated tail takes the rest of the text with
// it, so half an escape never reaches the model.
func stripEscapes(s string) string {
	var b strings.Builder

	for i := 0; i < len(s); {
		if s[i] != esc {
			b.WriteByte(s[i])
			i++

			continue
		}

		n, ok := escLen(s, i)
		if !ok {
			break
		}
		i += n
	}

	return b.String()
}

// escLen returns the length of the escape sequence at s[i].
func escLen(s string, i int) (int, bool) {
	if i+1 >= len(s) {
		return 0, false
	}

	switch s[i+1] {
	case '[':
		n, _, _, ok := csiAt(s, i)

		return n, ok
	case ']':
		return oscLen(s, i)
	default:
		return 2, true
	}
}

// oscLen returns the length of the OSC sequence at s[i], which ends
// at a BEL or at a string terminator.
func oscLen(s string, i int) (int, bool) {
	for j := i + 2; j < len(s); j++ {
		if s[j] == 0x07 {
			return j - i + 1, true
		}
		if s[j] == esc && j+1 < len(s) && s[j+1] == '\\' {
			return j - i + 2, true
		}
	}

	return 0, false
}

// stripBraille removes the braille glyphs agents draw spinners with.
func stripBraille(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 0x2800 && r <= 0x28ff {
			return -1
		}

		return r
	}, s)
}

// collapseBlanks trims trailing whitespace from every line, collapses
// any run of three or more blank lines to one, and drops trailing
// blank lines so the joined result carries no trailing newline.
func collapseBlanks(lines []string) []string {
	out := make([]string, 0, len(lines))
	blanks := 0

	for _, line := range lines {
		line = strings.TrimRightFunc(line, unicode.IsSpace)
		if line == "" {
			blanks++

			continue
		}
		out = append(out, blankRun(blanks)...)
		out = append(out, line)
		blanks = 0
	}

	return out
}

// blankRun returns the blank lines that stand in for a run of n.
func blankRun(n int) []string {
	if n >= 3 {
		n = 1
	}

	return make([]string, n)
}

// capBytes joins lines, dropping whole lines from the front, and then
// bytes from the front of the last line, until the result fits in
// maxBytes.
func capBytes(lines []string, maxBytes int) string {
	if maxBytes <= 0 {
		return strings.Join(lines, "\n")
	}

	total := 0
	for _, line := range lines {
		total += len(line) + 1
	}
	if len(lines) > 0 {
		total--
	}

	for len(lines) > 1 && total > maxBytes {
		total -= len(lines[0]) + 1
		lines = lines[1:]
	}

	text := strings.Join(lines, "\n")
	if len(text) <= maxBytes {
		return text
	}

	text = text[len(text)-maxBytes:]
	for len(text) > 0 && !utf8.RuneStart(text[0]) {
		text = text[1:]
	}

	return text
}
