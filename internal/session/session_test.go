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
		t, goodResponse, "claude", "done\r\n\x1b[2mghost\x1b[0m",
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
	if state["transcript_tail"] != "done" {
		t.Errorf("transcript_tail = %q, want the cleaned tail",
			state["transcript_tail"])
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
			if v.State != "" || v.Probabilities != nil {
				t.Errorf("verdict = %+v, want the zero value", v)
			}
		})
	}
}
