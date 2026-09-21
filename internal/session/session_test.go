package session

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/photuris/overseer-judge/internal/jev"
)

// goodResponse is a complete, well-formed answer pair.
const goodResponse = `{"model":"jev-1","answers":{` +
	`"state":{"type":"choice","choice":"idle","confidence":0.91,` +
	`"probabilities":{"idle":0.91,"working":0.09}},` +
	`"coherent":{"type":"noul","noul":0.97}},` +
	`"usage":{"input_tokens":1200,"output_tokens":8}}`

// ── Clean ───────────────────────────────────────────────────────────────────

func TestCleanFaintSpans(t *testing.T) {
	tests := []struct {
		name, raw, want string
	}{
		{"whole line", "\x1b[2mghost\x1b[0m", ""},
		{"reset then faint", "\x1b[0;2mghost\x1b[22mkept", "kept"},
		{"text before", "typed \x1b[2mghost\x1b[0m", "typed"},
		{"unterminated ends at the line", "a\x1b[2mghost\nkept",
			"a\nkept"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Clean(tt.raw, 0, 0); got != tt.want {
				t.Errorf("Clean(%q) = %q, want %q",
					tt.raw, got, tt.want)
			}
		})
	}
}

// TestCleanFaintColourParameters covers R3-01: a 2 or 22 that is a
// colour argument must not start or end a faint span.
func TestCleanFaintColourParameters(t *testing.T) {
	tests := []struct {
		name, raw, want string
	}{
		{"indexed colour 2", "\x1b[38;5;2mvisible\x1b[0m", "visible"},
		{
			name: "rgb colour with a 2 channel",
			raw:  "\x1b[38;2;100;150;200mvisible\x1b[0m",
			want: "visible",
		},
		{
			name: "indexed background colour 22",
			raw:  "\x1b[48;5;22mvisible\x1b[0m",
			want: "visible",
		},
		{
			name: "colon sub-parameters",
			raw:  "\x1b[38:2::10:20:30mvisible\x1b[0m",
			want: "visible",
		},
		{
			name: "indexed colour 22 does not end a faint span",
			raw:  "\x1b[2mghost\x1b[38;5;22m still ghost\x1b[0mkept",
			want: "kept",
		},
		{
			name: "a zero rgb channel does not end a faint span",
			raw:  "\x1b[2mghost\x1b[38;2;0;120;130m still\x1b[0mkept",
			want: "kept",
		},
		{
			name: "reset then faint stays on",
			raw:  "\x1b[0;2mghost\x1b[22mkept",
			want: "kept",
		},
		{
			name: "faint then reset never starts",
			raw:  "\x1b[2;22mkept",
			want: "kept",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Clean(tt.raw, 0, 0); got != tt.want {
				t.Errorf("Clean(%q) = %q, want %q",
					tt.raw, got, tt.want)
			}
		})
	}
}

// TestInputLineIgnoresColouredGhost is R3-01's input-side repro: a
// colour change inside the ghost suggestion must not end the span and
// leak the tail of it as typed input.
func TestInputLineIgnoresColouredGhost(t *testing.T) {
	const raw = "❯ \x1b[2mghost\x1b[38;5;22m suggestion\x1b[0m"

	if got := InputLine("claude", Clean(raw, 0, 0)); got != "" {
		t.Errorf("InputLine = %q, want empty", got)
	}
}

func TestCleanGhostSuggestionFixture(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "idle-03.txt"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	got := Clean(string(raw), MaxLines, MaxBytes)
	if strings.Contains(got, "truncated bodies") {
		t.Errorf("the ghost suggestion survived Clean:\n%s", got)
	}
	if !strings.Contains(got, "\n❯\n") {
		t.Errorf("the composer line is not a bare prompt:\n%s", got)
	}
	if line := InputLine("claude", got); line != "" {
		t.Errorf("InputLine = %q, want empty", line)
	}
}

// composer builds an opencode box: bar lines followed by a closer
// whose cap reaches past the longest of them.
func composer(lines ...string) string {
	width := 0
	for _, line := range lines {
		if n := utf8.RuneCountInString(line); n > width {
			width = n
		}
	}

	return boxedAt(width+2, lines...)
}

// boxInner is how many runes of a boxedAt line fall inside a box
// whose cap is boxInner+2 runes wide.
const boxInner = 30

// beside pads text out to the box's inner width and draws side text
// past its right edge, the way opencode draws its file sidebar.
func beside(text, side string) string {
	pad := boxInner - utf8.RuneCountInString(text)

	return text + strings.Repeat(" ", pad) + side
}

// boxedAt builds an opencode box whose cap stops capWidth runes past
// the foot, so a caller can draw a sidebar past the box's right edge.
func boxedAt(capWidth int, lines ...string) string {
	out := make([]string, 0, len(lines)+1)
	for _, line := range lines {
		out = append(out, "  ┃  "+line)
	}
	closer := "  ╹" + strings.Repeat("▀", capWidth)

	return strings.Join(append(out, closer), "\n")
}

// rule is a pi composer rule, and shortRule is one rune too short to
// count as one.
var (
	rule      = strings.Repeat("─", minRule)
	shortRule = strings.Repeat("─", minRule-1)
)

// labelled builds one of pi's labelled rules, as in "── Working ──…".
func labelled(label string) string {
	return "── " + label + " " + rule
}

func TestInputLine(t *testing.T) {
	tests := []struct {
		name, kind, cleaned, want string
	}{
		// marker
		{"claude composer", "claude", "❯ go ahead", "go ahead"},
		{"bare marker", "claude", "❯\u00a0", ""},
		{"codex hint", "codex", "› Ask Codex to do anything", ""},
		{"menu option", "codex", "› 1. Yes, continue", ""},
		{
			name:    "the last marker line wins",
			kind:    "claude",
			cleaned: "❯ first\nsome output\n❯ second",
			want:    "second",
		},
		{
			name:    "a boxed marker is not the input line",
			kind:    "claude",
			cleaned: "│ ❯ 1. Yes, proceed │",
			want:    "",
		},
		{"no marker", "claude", "just output\nmore output", ""},
		{"empty", "claude", "", ""},

		// box
		{
			name:    "the box joins its input lines",
			kind:    "opencode",
			cleaned: composer("first line", "second line", "status"),
			want:    "first line second line",
		},
		{
			name:    "a box holding only a status line is empty",
			kind:    "opencode",
			cleaned: composer("", "status", ""),
			want:    "",
		},
		{
			name: "the bottom box wins",
			kind: "opencode",
			cleaned: composer("upper", "status") + "\nreply\n" +
				composer("lower", "status"),
			want: "lower",
		},
		{
			name:    "a bar run without a foot is not a box",
			kind:    "opencode",
			cleaned: "  ┃  quoted reply\n  ┃  more reply",
			want:    "",
		},
		{
			name: "a sidebar past the cap is not input",
			kind: "opencode",
			cleaned: boxedAt(boxInner+2,
				beside("typed here", "/tmp/claude-1000/-home-"),
				beside("", "joshua-Projects-agent-skills/"),
				beside("Build · medium", "23edc16b-74db-4b4f"),
			),
			want: "typed here",
		},
		{
			name:    "a box line shorter than the cap keeps what it has",
			kind:    "opencode",
			cleaned: boxedAt(60, "short", "status"),
			want:    "short",
		},
		{
			name: "a closer with no cap clips to its own length",
			kind: "opencode",
			cleaned: "  ┃  typed here is long\n  ┃  status\n  ╹" +
				strings.Repeat("▔", 10),
			want: "typed he",
		},

		// rules
		{
			name:    "the text between the last two rules",
			kind:    "pi",
			cleaned: rule + "\nnotice\n" + rule + "\ntyped\n" + rule,
			want:    "typed",
		},
		{
			name:    "an empty composer between two rules",
			kind:    "pi",
			cleaned: rule + "\ntyped\n" + rule + "\n\n" + rule,
			want:    "",
		},
		{
			name:    "one rule is not a composer",
			kind:    "pi",
			cleaned: "typed\n" + rule,
			want:    "",
		},
		{
			name:    "a short rule is not a rule",
			kind:    "pi",
			cleaned: shortRule + "\ntyped\n" + rule,
			want:    "",
		},
		{
			name:    "rules survive CRLF through Clean",
			kind:    "pi",
			cleaned: Clean(rule+"\r\ntyped\r\n"+rule, 0, 0),
			want:    "typed",
		},
		{
			name:    "a labelled rule is a rule",
			kind:    "pi",
			cleaned: labelled(" Working") + "\ntyped\n" + rule,
			want:    "typed",
		},
		{
			name: "an empty composer under a labelled rule",
			kind: "pi",
			cleaned: rule + "\nstreamed output\n" +
				labelled(" Working") + "\n" + rule,
			want: "",
		},

		// strategy selection
		{
			name:    "pi ignores a marker line",
			kind:    "pi",
			cleaned: rule + "\ntyped\n" + rule + "\n❯ ignored",
			want:    "typed",
		},
		{
			name:    "claude ignores rules",
			kind:    "claude",
			cleaned: rule + "\ntyped\n" + rule,
			want:    "",
		},
		{
			name:    "unknown prefers the marker over rules",
			kind:    "unknown",
			cleaned: rule + "\ntyped\n" + rule + "\n❯ marked",
			want:    "marked",
		},
		{
			name:    "unknown keeps an empty marker over rules",
			kind:    "unknown",
			cleaned: rule + "\ntyped\n" + rule + "\n❯",
			want:    "",
		},
		{
			name: "unknown prefers the box over rules",
			kind: "unknown",
			cleaned: rule + "\ntyped\n" + rule + "\n" +
				composer("", "status"),
			want: "",
		},
		{
			name:    "unknown falls through a bar run to the rules",
			kind:    "unknown",
			cleaned: rule + "\ntyped\n" + rule + "\n  ┃  reply",
			want:    "typed",
		},

		// placeholders
		{
			name:    "a bare placeholder is dropped",
			kind:    "opencode",
			cleaned: composer("Ask anything…", "status"),
			want:    "",
		},
		{
			name: "a placeholder with an example is dropped",
			kind: "opencode",
			cleaned: composer(
				`Ask anything… "What is the tech stack?"`, "status",
			),
			want: "",
		},
		{
			name:    "a typed question that starts alike is kept",
			kind:    "opencode",
			cleaned: composer("Ask anything about X", "status"),
			want:    "Ask anything about X",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := InputLine(tt.kind, tt.cleaned)
			if got != tt.want {
				t.Errorf("InputLine(%q, %q) = %q, want %q",
					tt.kind, tt.cleaned, got, tt.want)
			}
		})
	}
}

// TestIsRule pins the shape of pi's horizontal rules, labelled ones
// included: pi draws its composer's top rule as "── Working ───…"
// while it works.
func TestIsRule(t *testing.T) {
	tests := []struct {
		name, line string
		want       bool
	}{
		{"a plain rule", rule, true},
		{"a labelled rule", labelled(" Working"), true},
		{
			name: "a label that is too long",
			line: labelled(
				"a label that is far too long to be a rule label",
			),
			want: false,
		},
		{"a label with punctuation", labelled(" Work/ing"), false},
		{"a short tail", "──  Working " + shortRule, false},
		{"a short rule", shortRule, false},
		{"not a rule at all", "typed text", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isRule(tt.line); got != tt.want {
				t.Errorf("isRule(%q) = %v, want %v",
					tt.line, got, tt.want)
			}
		})
	}
}

// fixtureInputLines is what every fixture's input box holds. Each is
// listed under the agent that drew it and under unknown, which must
// reach the same answer without being told.
var fixtureInputLines = map[string]string{
	"ambiguous-01.txt": "go ahead and stub resources/tmux.md",
	"degraded-01.txt":  "",
	"degraded-02.txt":  "",
	"dialog-01.txt":    "",
	"dialog-02.txt":    "",
	"error-01.txt":     "",
	"error-02.txt":     "",
	"idle-01.txt":      "",
	"idle-02.txt":      "",
	"idle-03.txt":      "",
	"idle-04.txt":      "",
	"idle-05.txt":      "",
	"idle-06.txt":      "",
	"idle-07.txt":      "",
	"idle-08.txt":      "",
	"unsubmitted-02.txt": "take option 2, and add a test that the " +
		"cooked path has no escapes",
	"unsubmitted-03.txt": "refactor the parser to use a state table",
	"unsubmitted-04.txt": "add a regression test for the empty case",
	"unsubmitted-05.txt": "now add a unit test for greet",
	"working-01.txt":     "",
	"working-02.txt":     "",
	"working-03.txt":     "",
	"working-04.txt":     "",
	"working-05.txt":     "",
}

// fixtureAgents names the agent that drew each capture that is not
// Claude Code's or Codex's. Those two are already covered by the
// unknown pass.
var fixtureAgents = map[string]string{
	"idle-04.txt":        "opencode",
	"idle-06.txt":        "opencode",
	"idle-08.txt":        "opencode",
	"unsubmitted-03.txt": "opencode",
	"unsubmitted-05.txt": "opencode",
	"working-03.txt":     "opencode",
	"working-05.txt":     "opencode",
	"idle-05.txt":        "pi",
	"idle-07.txt":        "pi",
	"unsubmitted-04.txt": "pi",
	"working-04.txt":     "pi",
}

func TestInputLineOnFixtures(t *testing.T) {
	for name, want := range fixtureInputLines {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", name))
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}
			cleaned := Clean(string(raw), MaxLines, MaxBytes)

			kinds := []string{"unknown"}
			if kind, ok := fixtureAgents[name]; ok {
				kinds = append(kinds, kind)
			}

			for _, kind := range kinds {
				got := InputLine(kind, cleaned)
				if got != want {
					t.Errorf("InputLine(%q, %s) = %q, want %q",
						kind, name, got, want)
				}
			}
		})
	}
}

func TestStateCarriesTheInputLine(t *testing.T) {
	got := State("claude", "output\n❯ ship it")
	if got["input_line"] != "ship it" {
		t.Errorf("input_line = %v, want \"ship it\"",
			got["input_line"])
	}
	if got["agent_kind"] != "claude" {
		t.Errorf("agent_kind = %v, want claude", got["agent_kind"])
	}
	if got["transcript_tail"] != "output\n❯ ship it" {
		t.Errorf("transcript_tail = %v", got["transcript_tail"])
	}
}

func TestCleanStripsEscapesAndGlyphs(t *testing.T) {
	tests := []struct {
		name, raw, want string
	}{
		{"csi colour", "\x1b[31mred\x1b[39m text", "red text"},
		{"osc title", "\x1b]0;a title\x07hello", "hello"},
		{"osc string terminator", "\x1b]0;a title\x1b\\hi", "hi"},
		{"two-byte escape", "a\x1bMb", "ab"},
		{"braille spinner", "load⠋⠙ing", "loading"},
		{"carriage returns", "a\r\nb\r", "a\nb"},
		{"no escapes", "plain text", "plain text"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Clean(tt.raw, 0, 0); got != tt.want {
				t.Errorf("Clean(%q) = %q, want %q",
					tt.raw, got, tt.want)
			}
		})
	}
}

func TestCleanLineHandling(t *testing.T) {
	tests := []struct {
		name, raw, want  string
		maxLines, maxLen int
	}{
		{
			name: "collapses a long blank run",
			raw:  "a\n\n\n\n\n\nb", want: "a\n\nb",
		},
		{
			name: "keeps a short blank run",
			raw:  "a\n\nb", want: "a\n\nb",
		},
		{
			name: "trims trailing whitespace",
			raw:  "a   \nb\t", want: "a\nb",
		},
		{
			name: "drops the trailing newline",
			raw:  "a\nb\n\n", want: "a\nb",
		},
		{
			name: "honours maxLines",
			raw:  "1\n2\n3\n4", maxLines: 2, want: "3\n4",
		},
		{
			name: "drops whole lines for maxBytes",
			raw:  "aaa\nbbb\nccc", maxLen: 8, want: "bbb\nccc",
		},
		{
			name: "cuts an oversize line at a rune boundary",
			raw:  "ααααα", maxLen: 5, want: "αα",
		},
		{
			name: "non-positive limits disable",
			raw:  "1\n2\n3\n4", maxLines: 0, maxLen: -1,
			want: "1\n2\n3\n4",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Clean(tt.raw, tt.maxLines, tt.maxLen)
			if got != tt.want {
				t.Errorf("Clean(%q, %d, %d) = %q, want %q",
					tt.raw, tt.maxLines, tt.maxLen, got, tt.want)
			}
		})
	}
}

// ── Questions ───────────────────────────────────────────────────────────────

func TestQuestions(t *testing.T) {
	qs := Questions()
	if len(qs) != 2 {
		t.Fatalf("Questions() has %d entries, want 2", len(qs))
	}

	for id, wantType := range map[string]string{
		"state": "choice", "coherent": "noul",
	} {
		q, ok := qs[id]
		if !ok {
			t.Fatalf("Questions() has no %q", id)
		}
		if q.Type != wantType {
			t.Errorf("%q has type %q, want %q", id, q.Type, wantType)
		}
	}

	criteria, ok := qs["state"].Criteria.(map[string]string)
	if !ok {
		t.Fatalf("state criteria is %T, want map[string]string",
			qs["state"].Criteria)
	}
	if len(criteria) != 6 {
		t.Errorf("state has %d criteria, want 6", len(criteria))
	}
	for _, label := range []string{
		"working", "idle", "dialog", "unsubmitted", "error",
		"degraded",
	} {
		if criteria[label] == "" {
			t.Errorf("state criteria lack %q", label)
		}
	}
}

// ── Judge ───────────────────────────────────────────────────────────────────

// judgeAgainst runs Judge against a server that answers with body,
// and returns the verdict, the request the server received, and the
// error.
func judgeAgainst(
	t *testing.T, body, agentKind, rawTail string,
) (Verdict, map[string]any, error) {
	t.Helper()

	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewDecoder(r.Body).Decode(&got)
			_, _ = io.WriteString(w, body)
		},
	))
	defer srv.Close()

	client := jev.New(srv.Client(), srv.URL, "sekret", "jev-test")
	v, err := Judge(
		context.Background(), client, agentKind, rawTail,
	)

	return v, got, err
}

func TestJudgeMapsEveryField(t *testing.T) {
	v, req, err := judgeAgainst(
		t, goodResponse, "claude",
		"done\r\n❯ ship it\x1b[2m and the ghost\x1b[0m",
	)
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}

	if v.State != "idle" {
		t.Errorf("State = %q, want idle", v.State)
	}
	if v.Confidence != 0.91 {
		t.Errorf("Confidence = %v, want 0.91", v.Confidence)
	}
	if v.Probabilities["working"] != 0.09 {
		t.Errorf("Probabilities = %v", v.Probabilities)
	}
	if v.Coherent != 0.97 {
		t.Errorf("Coherent = %v, want 0.97", v.Coherent)
	}
	if v.InputLine != "ship it" {
		t.Errorf("InputLine = %q, want \"ship it\"", v.InputLine)
	}
	if v.Model != "jev-1" {
		t.Errorf("Model = %q, want jev-1", v.Model)
	}
	if v.Usage.InputTokens != 1200 || v.Usage.OutputTokens != 8 {
		t.Errorf("Usage = %+v", v.Usage)
	}

	state, ok := req["state"].(map[string]any)
	if !ok {
		t.Fatalf("request state is %T", req["state"])
	}
	if state["agent_kind"] != "claude" {
		t.Errorf("agent_kind = %v, want claude", state["agent_kind"])
	}
	if state["transcript_tail"] != "done\n❯ ship it" {
		t.Errorf("transcript_tail = %q, want the cleaned tail",
			state["transcript_tail"])
	}
	if state["input_line"] != "ship it" {
		t.Errorf("input_line = %q, want the extracted line",
			state["input_line"])
	}
	if req["model"] != "jev-test" {
		t.Errorf("model = %v, want jev-test", req["model"])
	}
}

func TestJudgeRejectsBadAnswers(t *testing.T) {
	tests := []struct {
		name, body string
	}{
		{
			name: "coherent missing",
			body: `{"model":"m","answers":{"state":{"type":` +
				`"choice","choice":"idle","confidence":0.9,` +
				`"probabilities":{"idle":0.9}}}}`,
		},
		{
			name: "state lacks confidence",
			body: `{"model":"m","answers":{"state":{"type":` +
				`"choice","choice":"idle","probabilities":` +
				`{"idle":0.9}},"coherent":{"type":"noul",` +
				`"noul":0.9}}}`,
		},
		{
			name: "choice outside the six labels",
			body: `{"model":"m","answers":{"state":{"type":` +
				`"choice","choice":"banana","confidence":0.9,` +
				`"probabilities":{"banana":0.9}},"coherent":` +
				`{"type":"noul","noul":0.9}}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, _, err := judgeAgainst(t, tt.body, "unknown", "tail")
			if err == nil {
				t.Fatalf("Judge returned %+v, want an error", v)
			}
			if !errors.As(err, new(*jev.ResponseError)) {
				t.Errorf("error is %T (%v), want *jev.ResponseError",
					err, err)
			}
			if v.State != "" || v.Probabilities != nil ||
				v.InputLine != "" {
				t.Errorf("verdict = %+v, want the zero value", v)
			}
		})
	}
}

// ── Question wording ────────────────────────────────────────────────────────

// The amended question text, written out here as literals so a
// change to session.go cannot pass unnoticed. These are
// deliberately not derived from Questions().
const (
	wantStateInstructions = "`transcript_tail` is the most recent terminal " +
		"output of a coding agent (`agent_kind`) running in a terminal pane. " +
		"`input_line` is the text currently sitting in the agent's input box, " +
		"extracted by code (empty string if the box is empty or was not found). " +
		"`activity_hint` is the agent's own busy-indicator line as found on " +
		"screen by code, for example a spinner or progress bar beside 'esc to " +
		"interrupt'; it is an empty string when no busy indicator is on screen. " +
		"Classify what the pane is doing right now, judged by the last few " +
		"screens of output, giving most weight to the final lines."
	wantCoherentInstructions = "Is the most recent output in " +
		"`transcript_tail` coherent, on-task natural language or code, as " +
		"opposed to repetition loops, mixed-language gibberish, or random " +
		"tokens?"
	wantWorking = "The agent is busy right now and its output is coherent. " +
		"`activity_hint` is non-empty, showing the agent's own busy indicator; " +
		"a busy indicator with no output yet still counts. When `activity_hint` " +
		"is empty, choose this only if the final lines show a tool call or " +
		"command still in progress. Finished output above an input box is not " +
		"working, and text in `input_line` does not mean the agent is working."
	wantIdle = "The agent has finished and is waiting for input. " +
		"`activity_hint` is empty, `input_line` is empty or holds only the " +
		"tool's placeholder hint (for example 'Ask Codex to do anything'), and " +
		"there is no dialog."
	wantDialog = "A modal UI that blocks the agent until a person responds: " +
		"an approval or permission prompt, a folder-trust prompt, a numbered " +
		"or yes/no choice under a cursor, or a usage-limit or rate-limit " +
		"screen that must be dismissed before work can continue. " +
		"Informational banners, warnings, update notices, and tips that do " +
		"not wait for a keypress are not dialogs."
	wantUnsubmitted = "`input_line` holds text a person typed (an " +
		"instruction, question, or partial message, not the tool's placeholder " +
		"hint) and `activity_hint` is empty, so the agent is not acting on it. " +
		"The text has been typed but not sent, however much finished output " +
		"sits above it."
	wantError = "The agent's work ended with an API error, crash, stack " +
		"trace, connection failure, or process exit, and it is not continuing."
	wantDegraded = "The output has become incoherent: the same line or phrase " +
		"repeating many times, mixed-language token salad, random characters, " +
		"or text that no longer relates to any task. This holds even when " +
		"`activity_hint` shows a busy indicator, because a degraded agent keeps " +
		"producing."
	wantCoherentTrue = "The latest output reads as purposeful text or code a " +
		"competent engineer would write."
	wantCoherentFalse = "The latest output is repetitive, garbled, " +
		"multilingual salad, or otherwise meaningless."
)

func TestQuestionWordingIsVerbatim(t *testing.T) {
	qs := Questions()

	if got := qs["state"].Instructions; got != wantStateInstructions {
		t.Errorf("state instructions drifted:\n got %q\nwant %q",
			got, wantStateInstructions)
	}
	if got := qs["coherent"].Instructions; got !=
		wantCoherentInstructions {
		t.Errorf("coherent instructions drifted:\n got %q\nwant %q",
			got, wantCoherentInstructions)
	}

	state, ok := qs["state"].Criteria.(map[string]string)
	if !ok {
		t.Fatalf("state criteria is %T", qs["state"].Criteria)
	}
	for label, want := range map[string]string{
		"working":     wantWorking,
		"idle":        wantIdle,
		"dialog":      wantDialog,
		"unsubmitted": wantUnsubmitted,
		"error":       wantError,
		"degraded":    wantDegraded,
	} {
		if state[label] != want {
			t.Errorf("state criterion %q drifted:\n got %q\nwant %q",
				label, state[label], want)
		}
	}

	coherent, ok := qs["coherent"].Criteria.(map[string]string)
	if !ok {
		t.Fatalf("coherent criteria is %T", qs["coherent"].Criteria)
	}
	if coherent["true"] != wantCoherentTrue {
		t.Errorf("coherent true drifted:\n got %q\nwant %q",
			coherent["true"], wantCoherentTrue)
	}
	if coherent["false"] != wantCoherentFalse {
		t.Errorf("coherent false drifted:\n got %q\nwant %q",
			coherent["false"], wantCoherentFalse)
	}
}

// ── ActivityHint ────────────────────────────────────────────────────────────

// filler returns n non-empty lines of ordinary output.
func filler(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "some output"
	}

	return out
}

// above puts line above n non-empty lines, so a caller can push it
// out of the window ActivityHint reads.
func above(line string, n int) string {
	return strings.Join(append([]string{line}, filler(n)...), "\n")
}

func TestActivityHint(t *testing.T) {
	const busy = "✶ Wrangling… (esc to interrupt · 1m 14s)"

	tests := []struct {
		name, kind, cleaned, want string
	}{
		{"nothing on screen", "claude", "all done\n❯", ""},
		{"empty", "unknown", "", ""},
		{"the claude footer", "claude", busy, busy},
		{
			name:    "case does not matter",
			kind:    "claude",
			cleaned: "ESC TO INTERRUPT",
			want:    "ESC TO INTERRUPT",
		},
		{
			name:    "uppercase matches without the to as well",
			kind:    "opencode",
			cleaned: "ESC INTERRUPT",
			want:    "ESC INTERRUPT",
		},
		{
			name:    "a dotted capital I does not fold to ASCII i",
			kind:    "opencode",
			cleaned: "ESC \u0130NTERRUPT",
			want:    "",
		},
		{
			name:    "opencode drops the to",
			kind:    "opencode",
			cleaned: "⬝⬝⬝⬝  esc interrupt  tab agents",
			want:    "⬝⬝⬝⬝ esc interrupt tab agents",
		},
		{
			name:    "pi's static key hint is not a busy indicator",
			kind:    "unknown",
			cleaned: "escape interrupt · ctrl+c quit",
			want:    "",
		},
		{
			name:    "the twelfth-from-last line still counts",
			kind:    "claude",
			cleaned: above(busy, 11),
			want:    busy,
		},
		{
			name:    "the thirteenth-from-last line does not",
			kind:    "claude",
			cleaned: above(busy, 12),
			want:    "",
		},
		{
			name:    "blank lines do not fill the window",
			kind:    "claude",
			cleaned: busy + "\n\n\n" + strings.Join(filler(11), "\n"),
			want:    busy,
		},
		{
			name:    "the last matching line wins",
			kind:    "claude",
			cleaned: "first (esc to interrupt)\nlast (esc to interrupt)",
			want:    "last (esc to interrupt)",
		},
		{
			name:    "a long line is cut to 100 runes",
			kind:    "claude",
			cleaned: "esc to interrupt " + strings.Repeat("x", 103),
			want:    "esc to interrupt " + strings.Repeat("x", 83),
		},

		// labelled rules
		{
			name:    "pi reads its labelled rule",
			kind:    "pi",
			cleaned: "streamed output\n" + labelled(" Working"),
			want:    "Working",
		},
		{
			name:    "a plain rule carries no label",
			kind:    "pi",
			cleaned: "all done\n" + rule,
			want:    "",
		},
		{
			name:    "a rule labelled with spaces is found and empty",
			kind:    "pi",
			cleaned: "all done\n── " + rule,
			want:    "",
		},
		{
			name:    "an indented plain rule hides no earlier label",
			kind:    "pi",
			cleaned: labelled(" Working") + "\n  " + rule,
			want:    "Working",
		},
		{
			name:    "an indented plain rule hides none for unknown",
			kind:    "unknown",
			cleaned: labelled(" Working") + "\n  " + rule,
			want:    "Working",
		},
		{
			name:    "the last labelled rule wins",
			kind:    "pi",
			cleaned: labelled(" Thinking") + "\n" + labelled(" Working"),
			want:    "Working",
		},

		// strategy selection
		{
			name:    "pi ignores an interrupt line",
			kind:    "pi",
			cleaned: busy,
			want:    "",
		},
		{
			name:    "claude ignores a labelled rule",
			kind:    "claude",
			cleaned: labelled(" Working"),
			want:    "",
		},
		{
			name:    "opencode ignores a labelled rule",
			kind:    "opencode",
			cleaned: labelled(" Working"),
			want:    "",
		},
		{
			name:    "unknown reads a labelled rule",
			kind:    "unknown",
			cleaned: labelled(" Working"),
			want:    "Working",
		},
		{
			name:    "unknown prefers the interrupt line",
			kind:    "unknown",
			cleaned: labelled(" Working") + "\n" + busy,
			want:    busy,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ActivityHint(tt.kind, tt.cleaned)
			if got != tt.want {
				t.Errorf("ActivityHint(%q, %q) = %q, want %q",
					tt.kind, tt.cleaned, got, tt.want)
			}
		})
	}
}

// fixtureHints is the busy indicator every fixture shows. want is the
// whole hint where the line is short enough to pin; phrase is a
// substring of it where the agent draws a whole status bar into the
// line. Each fixture is checked under the agent that drew it and
// under unknown.
var fixtureHints = map[string]struct{ want, phrase string }{
	"ambiguous-01.txt":   {},
	"degraded-01.txt":    {phrase: "esc to interrupt"},
	"degraded-02.txt":    {phrase: "esc to interrupt"},
	"dialog-01.txt":      {},
	"dialog-02.txt":      {},
	"error-01.txt":       {},
	"error-02.txt":       {},
	"idle-01.txt":        {},
	"idle-02.txt":        {},
	"idle-03.txt":        {},
	"idle-04.txt":        {},
	"idle-05.txt":        {},
	"idle-06.txt":        {},
	"idle-07.txt":        {},
	"idle-08.txt":        {},
	"unsubmitted-02.txt": {},
	"unsubmitted-03.txt": {},
	"unsubmitted-04.txt": {},
	"unsubmitted-05.txt": {},
	"working-01.txt":     {phrase: "esc to interrupt"},
	"working-02.txt":     {phrase: "esc to interrupt"},
	"working-03.txt":     {phrase: "esc interrupt"},
	"working-04.txt":     {want: "Working"},
	"working-05.txt":     {phrase: "esc interrupt"},
}

func TestActivityHintOnFixtures(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("testdata", "*.txt"))
	if err != nil {
		t.Fatalf("list fixtures: %v", err)
	}
	if len(paths) != len(fixtureHints) {
		t.Errorf("testdata holds %d fixtures, fixtureHints has %d",
			len(paths), len(fixtureHints))
	}

	for _, path := range paths {
		name := filepath.Base(path)
		t.Run(name, func(t *testing.T) {
			tt, ok := fixtureHints[name]
			if !ok {
				t.Fatalf("fixtureHints has no entry for %s", name)
			}

			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}
			cleaned := Clean(string(raw), MaxLines, MaxBytes)

			kinds := []string{"unknown"}
			if kind, ok := fixtureAgents[name]; ok {
				kinds = append(kinds, kind)
			}

			for _, kind := range kinds {
				got := ActivityHint(kind, cleaned)
				if tt.phrase == "" {
					if got != tt.want {
						t.Errorf("ActivityHint(%q) = %q, want %q",
							kind, got, tt.want)
					}

					continue
				}
				if !strings.Contains(got, tt.phrase) {
					t.Errorf("ActivityHint(%q) = %q, want a line "+
						"carrying %q", kind, got, tt.phrase)
				}
			}
		})
	}
}

func TestStateCarriesTheActivityHint(t *testing.T) {
	got := State("claude", "working\n✶ Wrangling… (esc to interrupt)")
	if got["activity_hint"] != "✶ Wrangling… (esc to interrupt)" {
		t.Errorf("activity_hint = %v", got["activity_hint"])
	}

	if got := State("claude", "all done\n❯")["activity_hint"]; got != "" {
		t.Errorf("activity_hint = %v, want empty", got)
	}
}
