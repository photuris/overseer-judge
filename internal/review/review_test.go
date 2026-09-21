package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"overseer-judge/internal/jev"
)

// fixture reads a testdata file.
func fixture(t *testing.T, name string) string {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}

	return string(raw)
}

// ── Parse ───────────────────────────────────────────────────────────────────

func TestParseRoundOne(t *testing.T) {
	items := Parse(fixture(t, "round-01.md"))
	if len(items) != 6 {
		t.Fatalf("Parse returned %d items, want 6: %+v",
			len(items), items)
	}

	wantIDs := []string{
		"R6-01", "R6-02", "R6-03", "R6-04", "R6-05", "R6-06",
	}
	wantCounts := []int{1, 1, 1, 1, 1, 2}
	for i, want := range wantIDs {
		if items[i].ID != want {
			t.Errorf("item %d is %q, want %q", i, items[i].ID, want)
		}
		if got := len(items[i].Responses); got != wantCounts[i] {
			t.Errorf("%s has %d responses, want %d: %q",
				items[i].ID, got, wantCounts[i],
				items[i].Responses)
		}
	}

	first := items[0]
	wantTitle := "Pending re-run can start a batch while the " +
		"mapping editor is open"
	if first.Title != wantTitle {
		t.Errorf("R6-01 title = %q, want %q", first.Title, wantTitle)
	}
	if first.File != "src/shell/controller.cpp:1055" {
		t.Errorf("R6-01 file = %q", first.File)
	}
	if first.Severity != "medium" {
		t.Errorf("R6-01 severity = %q, want medium", first.Severity)
	}
	if first.Status != "resolved" {
		t.Errorf("R6-01 status = %q, want resolved", first.Status)
	}

	if !strings.Contains(first.Responses[0], "closeEditor()") {
		t.Errorf("R6-01 response lost its continuation lines: %q",
			first.Responses[0])
	}
	if strings.Contains(first.Responses[0], "\n") {
		t.Errorf("R6-01 response is not one line: %q",
			first.Responses[0])
	}

	for _, it := range items {
		if strings.Contains(it.Body, "- response:") {
			t.Errorf("%s body swallowed a response: %q",
				it.ID, it.Body)
		}
	}

	if !strings.Contains(items[2].Body, "### R9-99") {
		t.Errorf("R6-03 body lost its fenced fake header: %q",
			items[2].Body)
	}

	if strings.Contains(items[5].Responses[1], "overseer note") {
		t.Errorf("R6-06 second response swallowed the note: %q",
			items[5].Responses[1])
	}
	if !strings.Contains(items[5].Responses[0], "dead code") {
		t.Errorf("R6-06 first response = %q", items[5].Responses[0])
	}
}

func TestParseRoundTwo(t *testing.T) {
	items := Parse(fixture(t, "round-02.md"))
	if len(items) != 3 {
		t.Fatalf("Parse returned %d items, want 3: %+v",
			len(items), items)
	}

	for i, want := range []string{"R1-01", "R1-02", "R1-03"} {
		if items[i].ID != want {
			t.Errorf("item %d is %q, want %q", i, items[i].ID, want)
		}
		if items[i].Responses == nil {
			t.Errorf("%s has a nil Responses", items[i].ID)
		}
	}

	if len(items[1].Responses) != 0 {
		t.Errorf("R1-02 has %d responses, want 0: %q",
			len(items[1].Responses), items[1].Responses)
	}
	for _, it := range items {
		if strings.Contains(it.Body, "Round 1 of task 004") {
			t.Errorf("%s body swallowed the preamble: %q",
				it.ID, it.Body)
		}
	}
}

// TestParseRoundThree reads a real review round written by a
// reviewer who left a blank line between each header and its
// metadata. The whole metadata block was lost before the spec
// amendment of 2026-09-21.
func TestParseRoundThree(t *testing.T) {
	items := Parse(fixture(t, "round-03.md"))
	if len(items) != 6 {
		t.Fatalf("Parse returned %d items, want 6: %+v",
			len(items), items)
	}

	wantSeverity := []string{
		"blocking", "blocking", "minor",
		"blocking", "blocking", "blocking",
	}
	for i, it := range items {
		wantID := fmt.Sprintf("R1-%02d", i+1)
		if it.ID != wantID {
			t.Errorf("item %d is %q, want %q", i, it.ID, wantID)
		}
		if it.Severity != wantSeverity[i] {
			t.Errorf("%s severity = %q, want %q",
				it.ID, it.Severity, wantSeverity[i])
		}
		if it.Status != "resolved" {
			t.Errorf("%s status = %q, want resolved",
				it.ID, it.Status)
		}
		if !strings.HasPrefix(it.File, "internal/") {
			t.Errorf("%s file = %q, want an internal/ path",
				it.ID, it.File)
		}
		if strings.Contains(it.Body, "- severity:") {
			t.Errorf("%s body swallowed its metadata: %q",
				it.ID, it.Body)
		}
		if len(it.Responses) != 1 {
			t.Errorf("%s has %d responses, want 1: %q",
				it.ID, len(it.Responses), it.Responses)
		}
	}
}

// TestParseFencedTemplateYieldsNoItem covers the format template near
// the top of round-03.md: it is a fenced block holding a fake header
// and fake metadata lines, and must not become an item.
func TestParseFencedTemplateYieldsNoItem(t *testing.T) {
	for _, it := range Parse(fixture(t, "round-03.md")) {
		if strings.Contains(it.Title, "one-line title") {
			t.Errorf("the fenced template became item %q", it.ID)
		}
	}
}

// TestParseBlankLinesInMetadata pins the two halves of the amended
// rule: blanks before the block are skipped, and the first blank
// after a metadata line closes it.
func TestParseBlankLinesInMetadata(t *testing.T) {
	items := Parse("### R1-01: t\n\n\n- file: a.go:1\n" +
		"- severity: minor\n\n- status: open\nbody\n")
	if len(items) != 1 {
		t.Fatalf("Parse returned %d items, want 1", len(items))
	}

	it := items[0]
	if it.File != "a.go:1" || it.Severity != "minor" {
		t.Errorf("metadata after blank lines was lost: %+v", it)
	}
	if it.Status != "" {
		t.Errorf("status = %q, want empty: the blank line closed "+
			"the block", it.Status)
	}
	if it.Body != "- status: open\nbody" {
		t.Errorf("body = %q, want the closed-off line and the text",
			it.Body)
	}
}

func TestParseEmpty(t *testing.T) {
	items := Parse("")
	if items == nil {
		t.Fatal("Parse(\"\") returned nil, want an empty slice")
	}
	if len(items) != 0 {
		t.Errorf("Parse(\"\") returned %d items", len(items))
	}
}

// ── Questions and state ─────────────────────────────────────────────────────

func TestQuestionsPerResponse(t *testing.T) {
	it := Item{ID: "R1-01", Responses: []string{"a", "b"}}

	qs := Questions(it)
	want := map[string]string{
		"style_only": "noul",
		"response_0": "choice",
		"response_1": "choice",
	}
	if len(qs) != len(want) {
		t.Fatalf("Questions has %d entries, want %d: %v",
			len(qs), len(want), qs)
	}
	for id, kind := range want {
		q, ok := qs[id]
		if !ok {
			t.Fatalf("Questions has no %q", id)
		}
		if q.Type != kind {
			t.Errorf("%q has type %q, want %q", id, q.Type, kind)
		}
	}

	if !strings.Contains(
		qs["response_1"].Instructions.(string), "responses[1]",
	) {
		t.Errorf("response_1 does not name its reply: %q",
			qs["response_1"].Instructions)
	}
}

func TestStateShape(t *testing.T) {
	state := State(Item{
		ID: "R1-01", Title: "t", File: "f.go:1",
		Severity: "low", Status: "open", Body: "b",
	})

	finding, ok := state["finding"].(map[string]any)
	if !ok {
		t.Fatalf("finding is %T", state["finding"])
	}
	for key, want := range map[string]string{
		"id": "R1-01", "title": "t", "file": "f.go:1",
		"severity": "low", "body": "b",
	} {
		if finding[key] != want {
			t.Errorf("finding[%q] = %v, want %q",
				key, finding[key], want)
		}
	}
	if len(finding) != 5 {
		t.Errorf("finding has %d members, want 5: %v",
			len(finding), finding)
	}

	responses, ok := state["responses"].([]string)
	if !ok {
		t.Fatalf("responses is %T", state["responses"])
	}
	if responses == nil || len(responses) != 0 {
		t.Errorf("responses = %v, want an empty slice", responses)
	}
}

// ── Judge ───────────────────────────────────────────────────────────────────

// answer builds a response body whose response_0 carries kind.
func answer(kind string) string {
	return `{"model":"jev-1","answers":{` +
		`"style_only":{"type":"noul","noul":0.12},` +
		`"response_0":{"type":"choice","choice":"` + kind + `",` +
		`"confidence":0.77,"probabilities":{"` + kind + `":0.77,` +
		`"concern":0.23}}},` +
		`"usage":{"input_tokens":900,"output_tokens":6}}`
}

// serve returns a client posting to a server that replies with
// bodies in turn, and the slice the received requests land in.
func serve(t *testing.T, bodies ...string) (*jev.Client, *[]jev.Request) {
	t.Helper()

	got := new([]jev.Request)
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			var req jev.Request
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode request: %v", err)
			}
			*got = append(*got, req)

			i := len(*got) - 1
			if i >= len(bodies) {
				t.Errorf("request %d has no scripted reply", i)

				return
			}
			_, _ = io.WriteString(w, bodies[i])
		},
	))
	t.Cleanup(srv.Close)

	return jev.New(srv.Client(), srv.URL, "k", "jev-1"), got
}

func TestJudgeMapsAndSendsOnePerItem(t *testing.T) {
	client, got := serve(t, answer("fixed"), answer("evidence"))

	items := []Item{
		{ID: "R1-01", Severity: "high", Responses: []string{"a"}},
		{ID: "R1-02", Severity: "low", Responses: []string{"b"}},
	}

	typed, err := Judge(context.Background(), client, items)
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if len(typed) != 2 {
		t.Fatalf("Judge returned %d results, want 2", len(typed))
	}
	if len(*got) != 2 {
		t.Fatalf("server saw %d requests, want 2", len(*got))
	}

	for i, want := range []string{"R1-01", "R1-02"} {
		if typed[i].ID != want {
			t.Errorf("result %d is %q, want %q", i, typed[i].ID, want)
		}

		state, ok := (*got)[i].State.(map[string]any)
		if !ok {
			t.Fatalf("request %d state is %T", i, (*got)[i].State)
		}
		finding, ok := state["finding"].(map[string]any)
		if !ok {
			t.Fatalf("request %d finding is %T", i, state["finding"])
		}
		if finding["id"] != want {
			t.Errorf("request %d is for %v, want %q",
				i, finding["id"], want)
		}
	}

	first := typed[0]
	if first.Severity != "high" {
		t.Errorf("severity = %q, want high", first.Severity)
	}
	if first.StyleOnly != 0.12 {
		t.Errorf("style_only = %v, want 0.12", first.StyleOnly)
	}
	if first.Model != "jev-1" {
		t.Errorf("model = %q, want jev-1", first.Model)
	}
	if first.Usage.InputTokens != 900 {
		t.Errorf("usage = %+v", first.Usage)
	}
	if len(first.Responses) != 1 {
		t.Fatalf("result 0 has %d responses, want 1",
			len(first.Responses))
	}
	if first.Responses[0].Kind != "fixed" {
		t.Errorf("kind = %q, want fixed", first.Responses[0].Kind)
	}
	if first.Responses[0].Confidence != 0.77 {
		t.Errorf("confidence = %v", first.Responses[0].Confidence)
	}
	if first.Responses[0].Probabilities["fixed"] != 0.77 {
		t.Errorf("probabilities = %v",
			first.Responses[0].Probabilities)
	}
	if typed[1].Responses[0].Kind != "evidence" {
		t.Errorf("result 1 kind = %q, want evidence",
			typed[1].Responses[0].Kind)
	}
}

func TestJudgeRejectsUnknownKind(t *testing.T) {
	client, _ := serve(t, answer("maybe"))

	_, err := Judge(context.Background(), client, []Item{
		{ID: "R1-01", Responses: []string{"a"}},
	})
	if err == nil {
		t.Fatal("Judge accepted a kind outside the five labels")
	}
	if !errors.As(err, new(*jev.ResponseError)) {
		t.Errorf("error is %T (%v), want *jev.ResponseError", err, err)
	}
}

func TestJudgeNoResponsesAsksOneQuestion(t *testing.T) {
	client, got := serve(t,
		`{"model":"jev-1","answers":{"style_only":`+
			`{"type":"noul","noul":0.88}},`+
			`"usage":{"input_tokens":5,"output_tokens":1}}`)

	typed, err := Judge(context.Background(), client, []Item{
		{ID: "R1-02", Responses: []string{}},
	})
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}
	if typed[0].Responses == nil {
		t.Error("Responses is nil, want an empty slice")
	}
	if len(typed[0].Responses) != 0 {
		t.Errorf("Responses = %+v, want empty", typed[0].Responses)
	}
	if len((*got)[0].Questions) != 1 {
		t.Errorf("sent %d questions, want 1", len((*got)[0].Questions))
	}
}

// ── Question wording ────────────────────────────────────────────────────────

// The question text, written out here as literals so a change to
// review.go cannot pass unnoticed. These are deliberately not derived
// from Questions().
const (
	wantStyle = "`finding` is one code-review finding written against " +
		"a task specification. Is it purely a style or preference remark " +
		"(naming, formatting, ordering, wording, idiom choice) with no " +
		"claim about correctness, behavior, the task's acceptance " +
		"criteria, or scope?"
	wantStyleTrue = "The finding would not change what the program does " +
		"or whether the task's acceptance passes; it is taste."
	wantStyleFalse = "The finding claims a bug, a missing behavior, a " +
		"test gap, a violated acceptance criterion, or work outside the " +
		"allowed scope."
	wantResponse0 = "`responses[0]` is the implementer's reply to " +
		"`finding`. Classify the reply by what it does, not by whether it " +
		"is right."
	wantFixed = "Says a change was made and names where or how (a " +
		"function, file, commit, or test), addressing the finding."
	wantEvidence = "Disputes the finding by pointing at something " +
		"checkable: a code citation with a path or line, an existing " +
		"test, a command output, or a reproduction."
	wantConcern = "Disputes the finding by argument or opinion only, " +
		"with nothing checkable named."
	wantQuestion = "Asks the reviewer for clarification or more " +
		"information instead of fixing or disputing."
	wantAgree = "Accepts the finding without claiming a fix yet, e.g. " +
		"'will do', 'agreed, next round'."
)

func TestQuestionWordingIsVerbatim(t *testing.T) {
	qs := Questions(Item{ID: "R1-01", Responses: []string{"a"}})

	if got := qs["style_only"].Instructions; got != wantStyle {
		t.Errorf("style_only instructions drifted:\n got %q\nwant %q",
			got, wantStyle)
	}
	if got := qs["response_0"].Instructions; got != wantResponse0 {
		t.Errorf("response_0 instructions drifted:\n got %q\nwant %q",
			got, wantResponse0)
	}

	style, ok := qs["style_only"].Criteria.(map[string]string)
	if !ok {
		t.Fatalf("style_only criteria is %T", qs["style_only"].Criteria)
	}
	for outcome, want := range map[string]string{
		"true": wantStyleTrue, "false": wantStyleFalse,
	} {
		if style[outcome] != want {
			t.Errorf("style_only %s criterion drifted:"+
				"\n got %q\nwant %q", outcome, style[outcome], want)
		}
	}

	kinds, ok := qs["response_0"].Criteria.(map[string]string)
	if !ok {
		t.Fatalf("response_0 criteria is %T", qs["response_0"].Criteria)
	}
	want := map[string]string{
		"fixed":    wantFixed,
		"evidence": wantEvidence,
		"concern":  wantConcern,
		"question": wantQuestion,
		"agree":    wantAgree,
	}
	if len(kinds) != len(want) {
		t.Errorf("response_0 has %d labels, want %d: %v",
			len(kinds), len(want), kinds)
	}
	for label, text := range want {
		if kinds[label] != text {
			t.Errorf("%s criterion drifted:\n got %q\nwant %q",
				label, kinds[label], text)
		}
	}
}
