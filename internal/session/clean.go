// Package session classifies the transcript tail of a coding agent's
// terminal pane into one of six states.
package session

import (
	"regexp"
	"slices"
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
// turning faint on and ends at the next one turning it off, or at end
// of line, whichever comes first. See sgrFaint for how a parameter
// list is read. Claude Code renders its greyed-out prompt suggestion
// this way; it is not typed input and must not reach the model. This
// is a no-op on input without escapes.
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
		if ok && final == 'm' && sgrFaint(params) == faintOn {
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
		if final == 'm' && sgrFaint(params) == faintOff {
			return i + n
		}
		i += n
	}

	return i
}

// The faint transitions an SGR parameter list can leave behind.
const (
	faintOff       = -1
	faintUnchanged = 0
	faintOn        = 1
)

// sgrFaint walks the semicolon-separated parameters of one CSI … m
// sequence, left to right, and reports the faint state it leaves
// behind. An extended-colour selector consumes its own arguments, so
// the 2 in 38;5;2 or 38;2;r;g;b is a colour value and never turns
// faint on, and the 22 in 48;5;22 never turns it off. A standalone 2
// turns faint on; a standalone 0, a 22, or an empty parameter --
// including the empty list of a bare CSI m -- turns it off.
func sgrFaint(params string) int {
	if params == "" {
		return faintOff
	}

	fields := strings.Split(params, ";")
	state := faintUnchanged

	for i := 0; i < len(fields); i++ {
		// A parameter carrying colon sub-parameters, such as
		// 38:2::10:20:30, is one whole extended-colour operation.
		if strings.Contains(fields[i], ":") {
			continue
		}

		if n, ok := colourArgs(fields, i); ok {
			i += n

			continue
		}

		switch fields[i] {
		case "2":
			state = faintOn
		case "0", "22", "":
			state = faintOff
		}
	}

	return state
}

// colourArgs reports whether fields[i] selects an extended colour
// and, if so, how many parameters after it the selector consumes. A
// list that ends before its arguments consumes what is left.
func colourArgs(fields []string, i int) (int, bool) {
	switch fields[i] {
	case "38", "48", "58":
	default:
		return 0, false
	}

	if i+1 >= len(fields) {
		return 0, true
	}

	switch fields[i+1] {
	case "5":
		return 2, true
	case "2":
		return 4, true
	}

	return 0, true
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

// inputMarkers are the runes agents draw the left edge of their input
// box with.
const inputMarkers = "❯›"

// The runes opencode draws its composer box with: a bar down the left
// edge of every line, a foot under the last one, and a cap that runs
// from the foot to the right edge of the box.
const (
	boxBar  = '┃'
	boxFoot = '╹'
	boxCap  = '▀'
)

// ruleRune is the glyph pi draws its horizontal rules with, minRule
// is how many of them a line must end with to count as one, and
// maxLabel is how many other runes a labelled rule may carry.
const (
	ruleRune = '─'
	minRule  = 20
	maxLabel = 24
)

// menuOption matches a dialog menu option sitting under the cursor,
// such as "1. Yes, continue".
var menuOption = regexp.MustCompile(`^\d+\.\s`)

// placeholderHints are the prompts agents draw in an empty input box.
// Text starting with one of them was not typed by a person.
//
// ponytail: literal prefixes; agents reword these across versions.
// Add entries here; the idle criterion's placeholder clause is the
// backstop.
var placeholderHints = []string{
	"Ask Codex to do anything",
	"Ask anything…",
}

// strategy finds an agent's input box in the cleaned lines of a pane
// and returns the text in it. found reports whether the structure the
// strategy looks for was there at all, which is not the same as the
// text being non-empty: an empty box is found and empty.
type strategy func(lines []string) (text string, found bool)

// InputLine returns the text sitting in the agent's input box, or "".
// cleaned is post-Clean text. It runs the strategies for agentKind in
// order and returns the result of the FIRST strategy that finds its
// structure, even when the text it finds is empty. Known placeholder
// hints are then dropped.
//
//	claude, codex: marker
//	opencode:      box
//	pi:            rules
//	unknown (or anything else): marker, then box, then rules
func InputLine(agentKind, cleaned string) string {
	lines := strings.Split(cleaned, "\n")

	for _, find := range strategiesFor(agentKind) {
		if text, found := find(lines); found {
			return dropPlaceholder(text)
		}
	}

	return ""
}

// strategiesFor returns the strategies to try for an agent kind, in
// order. An unrecognised kind tries all three.
func strategiesFor(agentKind string) []strategy {
	switch agentKind {
	case "claude", "codex":
		return []strategy{markerInput}
	case "opencode":
		return []strategy{boxInput}
	case "pi":
		return []strategy{rulesInput}
	default:
		return []strategy{markerInput, boxInput, rulesInput}
	}
}

// dropPlaceholder returns "" when text is one of the placeholder
// hints, and text unchanged otherwise.
func dropPlaceholder(text string) string {
	for _, hint := range placeholderHints {
		if strings.HasPrefix(text, hint) {
			return ""
		}
	}

	return text
}

// markerInput finds the last line whose first rune is ❯ or › and
// returns it with the marker and any following spaces or U+00A0
// removed, then trimmed. A menu option under a dialog cursor is not
// typed input and yields "", as does a bare marker.
func markerInput(lines []string) (string, bool) {
	for i := len(lines) - 1; i >= 0; i-- {
		marker, size := utf8.DecodeRuneInString(lines[i])
		if !strings.ContainsRune(inputMarkers, marker) {
			continue
		}

		text := strings.TrimSpace(
			strings.TrimLeft(lines[i][size:], "  "),
		)
		if menuOption.MatchString(text) {
			return "", true
		}

		return text, true
	}

	return "", false
}

// boxInput finds opencode's composer: the lowest run of lines whose
// first non-space rune is a bar, sitting directly above a line whose
// first non-space rune is the foot. The last non-empty line inside
// the box is the status line, not input, so it is dropped.
func boxInput(lines []string) (string, bool) {
	for i := len(lines) - 1; i >= 0; i-- {
		if firstRune(lines[i]) != boxFoot {
			continue
		}

		top := i
		for top > 0 && firstRune(lines[top-1]) == boxBar {
			top--
		}
		if top == i {
			continue
		}

		lo, hi := boxEdges(lines[i])

		return boxText(lines[top:i], lo, hi), true
	}

	return "", false
}

// boxEdges returns the rune index of the foot in a closer line and
// the rune index of the last cap rune after it. Those are the left
// and right edges of the box; anything further right on a box line
// belongs to whatever the agent drew beside it. A closer carrying no
// cap runes clips to its own last rune.
func boxEdges(closer string) (lo, hi int) {
	runes := []rune(closer)
	hi = len(runes) - 1

	for i, r := range runes {
		switch r {
		case boxFoot:
			lo = i
		case boxCap:
			hi = i
		}
	}

	return lo, hi
}

// boxText takes the runes between the box's edges out of every line
// of a composer box, drops the blank ones and the status line that
// follows them, and joins what is left with single spaces.
func boxText(box []string, lo, hi int) string {
	content := make([]string, 0, len(box))

	for _, line := range box {
		// ponytail: rune indices, not display columns; double-width
		// input would shift the clip. Switch to a width-aware walk if
		// that bites.
		runes := []rune(line)
		if len(runes) > hi+1 {
			runes = runes[:hi+1]
		}
		if lo+1 >= len(runes) {
			continue
		}
		if text := strings.TrimSpace(
			string(runes[lo+1:]),
		); text != "" {
			content = append(content, text)
		}
	}
	if len(content) == 0 {
		return ""
	}

	return strings.Join(content[:len(content)-1], " ")
}

// rulesInput finds pi's composer: the non-empty lines between the
// last two horizontal rules. found reports that two rules exist.
func rulesInput(lines []string) (string, bool) {
	last, prev := -1, -1

	for i := len(lines) - 1; i >= 0 && prev < 0; i-- {
		if !isRule(lines[i]) {
			continue
		}
		if last < 0 {
			last = i

			continue
		}
		prev = i
	}
	if prev < 0 {
		return "", false
	}

	content := make([]string, 0, last-prev)
	for _, line := range lines[prev+1 : last] {
		if line = strings.TrimSpace(line); line != "" {
			content = append(content, line)
		}
	}

	return strings.Join(content, " "), true
}

// isRule reports whether a line is one of pi's horizontal rules: a
// trimmed line that opens with at least two rule runes and closes
// with at least minRule of them. Between the two runs pi may draw a
// short label, as in "── Working ───…", so up to maxLabel further
// runes are allowed, all of them letters, digits, or spaces. A line
// of nothing but rule runes still qualifies.
func isRule(line string) bool {
	runes := []rune(strings.TrimSpace(line))
	if len(runes) < minRule ||
		runes[0] != ruleRune || runes[1] != ruleRune {
		return false
	}

	tail := 0
	for i := len(runes) - 1; i >= 0 && runes[i] == ruleRune; i-- {
		tail++
	}
	if tail < minRule {
		return false
	}

	label := 0
	for _, r := range runes {
		if r == ruleRune {
			continue
		}
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != ' ' {
			return false
		}
		label++
	}

	return label <= maxLabel
}

// firstRune returns a line's first non-space rune, or 0 when the line
// holds nothing else.
func firstRune(line string) rune {
	for _, r := range line {
		if !unicode.IsSpace(r) {
			return r
		}
	}

	return 0
}

// hintLines is how many of a pane's last non-empty lines a busy
// indicator may hide in, and maxHint is how many runes of the line
// that carries it reach the model.
const (
	hintLines = 12
	maxHint   = 100
)

// interruptPhrases are the phrases an agent draws beside the key that
// interrupts it while it works, lower-cased for matching. pi's static
// "escape interrupt" key hint is deliberately not one of them: it is
// on screen whether or not the agent is busy.
var interruptPhrases = []string{"esc to interrupt", "esc interrupt"}

// hint finds an agent's busy indicator among the last lines of a pane
// and returns the text of it. found reports whether the shape the
// strategy looks for was there at all, which is not the same as the
// text being non-empty: a rule carrying only spaces is found and
// empty.
type hint func(lines []string) (text string, found bool)

// ActivityHint returns the agent's own busy-indicator line as found
// in cleaned (post-Clean) text, or "". It looks only at the last
// hintLines non-empty lines, runs the strategies for agentKind in
// order, and returns the result of the FIRST one that finds its
// shape:
//
//	claude, codex, opencode: interrupt
//	pi:                      label
//	unknown (or anything else): interrupt, then label
//
// ponytail: a pane whose visible text merely quotes "esc to
// interrupt" in its last 12 lines reads as busy. Tighten to per-agent
// line shapes if that bites.
func ActivityHint(agentKind, cleaned string) string {
	lines := lastNonEmpty(strings.Split(cleaned, "\n"), hintLines)

	for _, find := range hintsFor(agentKind) {
		if text, found := find(lines); found {
			return text
		}
	}

	return ""
}

// hintsFor returns the busy-indicator strategies to try for an agent
// kind, in order. An unrecognised kind tries both.
func hintsFor(agentKind string) []hint {
	switch agentKind {
	case "claude", "codex", "opencode":
		return []hint{interruptHint}
	case "pi":
		return []hint{labelHint}
	default:
		return []hint{interruptHint, labelHint}
	}
}

// lastNonEmpty returns the last n lines holding more than whitespace,
// in the order they appear.
func lastNonEmpty(lines []string, n int) []string {
	out := make([]string, 0, n)

	for i := len(lines) - 1; i >= 0 && len(out) < n; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			out = append(out, lines[i])
		}
	}
	slices.Reverse(out)

	return out
}

// interruptHint returns the last line carrying an interrupt phrase,
// its whitespace collapsed to single spaces and cut to maxHint runes.
// Matching folds ASCII A-Z only: strings.ToLower would fold runes
// such as U+0130 into a phrase that the line does not carry.
func interruptHint(lines []string) (string, bool) {
	for i := len(lines) - 1; i >= 0; i-- {
		lower := strings.Map(asciiLower, lines[i])
		for _, phrase := range interruptPhrases {
			if strings.Contains(lower, phrase) {
				return cutRunes(collapseSpaces(lines[i])), true
			}
		}
	}

	return "", false
}

// labelHint returns the label on the last labelled rule, which is how
// pi says it is working: "──  Working ────…" yields "Working". A rule
// of nothing but rule runes carries no label, so it is not found.
// The label comes off the trimmed line, as isRule tests it, so that
// an indented plain rule stays unlabelled.
func labelHint(lines []string) (string, bool) {
	for i := len(lines) - 1; i >= 0; i-- {
		if !isRule(lines[i]) {
			continue
		}

		label := strings.Map(dropRuleRune, strings.TrimSpace(lines[i]))
		if label == "" {
			continue
		}

		return collapseSpaces(label), true
	}

	return "", false
}

// asciiLower lower-cases A-Z and leaves every other rune alone.
func asciiLower(r rune) rune {
	if 'A' <= r && r <= 'Z' {
		return r - 'A' + 'a'
	}

	return r
}

// dropRuleRune deletes pi's rule rune and keeps everything else.
func dropRuleRune(r rune) rune {
	if r == ruleRune {
		return -1
	}

	return r
}

// collapseSpaces trims a line and collapses every run of whitespace
// in it to one space.
func collapseSpaces(line string) string {
	return strings.Join(strings.Fields(line), " ")
}

// cutRunes cuts a line to maxHint runes.
func cutRunes(line string) string {
	runes := []rune(line)
	if len(runes) <= maxHint {
		return line
	}

	return string(runes[:maxHint])
}
