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

	"overseer-judge/internal/jev"
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

	if got := InputLine(Clean(raw, 0, 0)); got != "" {
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
	if line := InputLine(got); line != "" {
		t.Errorf("InputLine = %q, want empty", line)
	}
}

func TestInputLine(t *testing.T) {
	tests := []struct {
		name, cleaned, want string
	}{
		{"claude composer", "❯ go ahead", "go ahead"},
		{"bare marker", "❯\u00a0", ""},
		{"codex placeholder", "› Ask Codex to do anything",
			"Ask Codex to do anything"},
		{"menu option", "› 1. Yes, continue", ""},
		{
			name:    "the last marker line wins",
			cleaned: "❯ first\nsome output\n❯ second",
			want:    "second",
		},
		{
			name:    "a boxed marker is not the input line",
			cleaned: "│ ❯ 1. Yes, proceed │",
			want:    "",
		},
		{"no marker", "just output\nmore output", ""},
		{"empty", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := InputLine(tt.cleaned); got != tt.want {
				t.Errorf("InputLine(%q) = %q, want %q",
					tt.cleaned, got, tt.want)
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
		"Classify what the pane is doing right now, judged by the last few " +
		"screens of output, giving most weight to the final lines."
	wantCoherentInstructions = "Is the most recent output in " +
		"`transcript_tail` coherent, on-task natural language or code, as " +
		"opposed to repetition loops, mixed-language gibberish, or random " +
		"tokens?"
	wantWorking = "The agent is actively producing coherent, on-task output: " +
		"tool calls, code, file edits, test runs, or prose that advances a " +
		"task. A spinner, elapsed-time counter, or 'thinking' indicator on " +
		"screen counts as working. Text in `input_line` does not, by itself, " +
		"mean the agent is working."
	wantIdle = "The agent has finished and is waiting for input. `input_line` " +
		"is empty or holds only the tool's placeholder hint (for example 'Ask " +
		"Codex to do anything'), and there is no dialog."
	wantDialog = "A modal question, approval prompt, folder-trust prompt, " +
		"update notice, usage-limit or rate-limit notice, or any UI that waits " +
		"for a keypress or choice before the agent can continue."
	wantUnsubmitted = "`input_line` holds text a person typed (an " +
		"instruction, question, or partial message, not the tool's placeholder " +
		"hint), and no spinner, progress line, or tool call shows the agent " +
		"acting on it. The text has been typed but not sent."
	wantError = "The agent's work ended with an API error, crash, stack " +
		"trace, connection failure, or process exit, and it is not continuing."
	wantDegraded = "The output has become incoherent: the same line or phrase " +
		"repeating many times, mixed-language token salad, random characters, " +
		"or text that no longer relates to any task, while the agent appears to " +
		"keep producing it."
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
