package tasklint

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/photuris/overseer-judge/internal/jev"
)

// goodResponse answers all three questions.
const goodResponse = `{"model":"jev-1","answers":{` +
	`"needs_interpretation":{"type":"noul","noul":0.12},` +
	`"acceptance_strength":{"type":"score","score":0.32,` +
	`"confidence":0.81},` +
	`"scope_generic":{"type":"noul","noul":0.05}},` +
	`"usage":{"input_tokens":1400,"output_tokens":9}}`

// fixture reads a task file from testdata.
func fixture(t *testing.T, name string) string {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}

	return string(raw)
}

// ── Parse ───────────────────────────────────────────────────────────────────

func TestParseIgnoresFencedHeadings(t *testing.T) {
	d := Parse(fixture(t, "good-02.md"))

	if d.Title != "007 template-doc" {
		t.Errorf("Title = %q", d.Title)
	}
	if d.Status != "ready" {
		t.Errorf("Status = %q, want ready", d.Status)
	}

	objectives := 0
	for _, sec := range d.Order {
		if sec == "Objective" {
			objectives++
		}
	}
	if objectives != 1 {
		t.Errorf("Order has %d Objective entries, want 1: %v",
			objectives, d.Order)
	}

	if body := d.Section("Objective"); !strings.Contains(
		body, "docs/task-template.md",
	) || strings.Contains(body, "NNN slug") {
		t.Errorf("Objective body is not the real one:\n%s", body)
	}

	// The fenced template holds a "## Acceptance" heading and a
	// "Command: false" line before the real Acceptance section.
	if body := d.Section("Acceptance"); !strings.Contains(
		body, "go test ./internal/tasklint/",
	) || strings.Contains(body, "Command: false") {
		t.Errorf("Acceptance body is not the real one:\n%s", body)
	}
}

func TestParseKeepsTheFirstOfDuplicateHeadings(t *testing.T) {
	d := Parse(`# 020 example
Status: done

## Files
Allowed: a.go
## Out of scope
nothing
## Files
Allowed: b.go
`)

	if want := "Allowed: a.go\n"; d.Section("Files") != want {
		t.Errorf("Files body = %q, want %q", d.Section("Files"), want)
	}
	if want := "nothing\n## Files\nAllowed: b.go\n"; d.Section(
		"Out of scope",
	) != want {
		t.Errorf("Out of scope body = %q, want %q",
			d.Section("Out of scope"), want)
	}
	if len(d.Order) != 2 {
		t.Errorf("Order = %v, want two entries", d.Order)
	}
}

// TestStatusIsOnlyReadFromTheLineBelowTheTitle pins R4-01: the first
// non-empty line after the H1 is the only status candidate, so a
// header without one cannot borrow a Status: line out of a section
// body and pass status_line with it.
func TestStatusIsOnlyReadFromTheLineBelowTheTitle(t *testing.T) {
	tests := []struct {
		name, text, section, body string
	}{
		{
			name:    "heading first",
			text:    "# test\n## Objective\nStatus: ready\n",
			section: "Objective",
			body:    "Status: ready\n",
		},
		{
			name:    "files heading first",
			text:    "# test\n## Files\nStatus: done\n",
			section: "Files",
			body:    "Status: done\n",
		},
		{
			name: "fence first",
			text: "# test\n```\nStatus: ready\n```\n" +
				"## Objective\nwork\n",
			section: "Objective",
			body:    "work\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := Parse(tt.text)

			if d.Title != "test" {
				t.Errorf("Title = %q, want test", d.Title)
			}
			if d.Status != "" {
				t.Errorf("Status = %q, want empty: the line below "+
					"the H1 is not a status line", d.Status)
			}
			if got := d.Section(tt.section); got != tt.body {
				t.Errorf("%s body = %q, want %q",
					tt.section, got, tt.body)
			}

			finding := Static(d)[0]
			if finding.Check != "status_line" || finding.OK {
				t.Errorf("first finding = %+v, want status_line "+
					"failing", finding)
			}
		})
	}
}

// TestStatusIsReadFromTheLineBelowTheTitle is the other half of
// R4-01: a real status line is still found, blank lines and all.
func TestStatusIsReadFromTheLineBelowTheTitle(t *testing.T) {
	for _, text := range []string{
		"# test\nStatus: ready\n## Objective\nwork\n",
		"# test\n\n\nStatus: ready\n\n## Objective\nwork\n",
	} {
		d := Parse(text)
		if d.Status != "ready" {
			t.Errorf("Status = %q, want ready, from %q",
				d.Status, text)
		}
		if finding := Static(d)[0]; !finding.OK {
			t.Errorf("status_line = %+v, want OK", finding)
		}
	}
}

func TestSectionAbsentIsEmpty(t *testing.T) {
	d := Parse(fixture(t, "vague-01.md"))
	if got := d.Section("Spec"); got != "" {
		t.Errorf("Section(Spec) = %q, want empty", got)
	}
	if d.Judgeable() != true {
		t.Error("Judgeable() = false, want true")
	}
}

// ── Static ──────────────────────────────────────────────────────────────────

// checks lists the findings Static returns, in order.
var checks = []string{
	"status_line", "sections_present", "sections_ordered",
	"allowed_nonempty", "acceptance_command", "budget_numeric",
}

func TestStatic(t *testing.T) {
	tests := []struct {
		file string
		want []bool
	}{
		{"good-01.md", []bool{true, true, true, true, true, true}},
		{"good-02.md", []bool{true, true, true, true, true, true}},
		{"vague-01.md", []bool{true, true, true, true, true, true}},
		{"vacuous-01.md", []bool{true, true, true, true, true, true}},
		{"generic-scope-01.md",
			[]bool{true, true, true, true, true, true}},
		{"broken-01.md",
			[]bool{true, false, false, true, false, true}},
	}

	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			got := Static(Parse(fixture(t, tt.file)))
			if len(got) != len(checks) {
				t.Fatalf("Static returned %d findings, want %d",
					len(got), len(checks))
			}

			for i, f := range got {
				if f.Check != checks[i] {
					t.Errorf("finding %d is %q, want %q",
						i, f.Check, checks[i])
				}
				if f.OK != tt.want[i] {
					t.Errorf("%s: OK = %v, want %v (detail %q)",
						f.Check, f.OK, tt.want[i], f.Detail)
				}
			}
		})
	}
}

func TestStaticBrokenDetails(t *testing.T) {
	got := map[string]Finding{}
	for _, f := range Static(Parse(fixture(t, "broken-01.md"))) {
		got[f.Check] = f
	}

	if d := got["sections_present"].Detail; d != "Acceptance, Rules" {
		t.Errorf("sections_present detail = %q, want %q",
			d, "Acceptance, Rules")
	}
	if f := got["sections_ordered"]; f.OK ||
		!strings.Contains(f.Detail, "Files") {
		t.Errorf("sections_ordered = %+v, want a failure naming Files",
			f)
	}
	if d := got["acceptance_command"].Detail; d != "no Command: line" {
		t.Errorf("acceptance_command detail = %q", d)
	}
}

func TestStaticReportsMissingPieces(t *testing.T) {
	d := Parse(`# 021 thin
Status: pending

## Files
Read-only: a.go

## Acceptance
Command: go test ./...

## Budget
a few turns
`)

	got := map[string]Finding{}
	for _, f := range Static(d) {
		got[f.Check] = f
	}

	for check, wantDetail := range map[string]string{
		"status_line": `status "pending" is not one of ` +
			`ready|in-progress|done|accepted`,
		"allowed_nonempty":   "no Allowed: line",
		"acceptance_command": "Command 1 has no Expect: line",
		"budget_numeric":     `no "<n> turns" in the budget`,
	} {
		if got[check].OK {
			t.Errorf("%s: OK = true, want false", check)
		}
		if got[check].Detail != wantDetail {
			t.Errorf("%s detail = %q, want %q",
				check, got[check].Detail, wantDetail)
		}
	}
}

// ── Questions and state ─────────────────────────────────────────────────────

func TestQuestions(t *testing.T) {
	qs := Questions()
	if len(qs) != 3 {
		t.Fatalf("Questions() has %d entries, want 3", len(qs))
	}

	for _, id := range []string{
		"needs_interpretation", "scope_generic",
	} {
		q, ok := qs[id]
		if !ok {
			t.Fatalf("Questions() has no %q", id)
		}
		if q.Type != "noul" {
			t.Errorf("%q has type %q, want noul", id, q.Type)
		}

		criteria, ok := q.Criteria.(map[string]string)
		if !ok {
			t.Fatalf("%q criteria is %T, want map[string]string",
				id, q.Criteria)
		}
		if criteria["true"] == "" || criteria["false"] == "" {
			t.Errorf("%q lacks a true or false criterion", id)
		}
	}

	strength, ok := qs["acceptance_strength"]
	if !ok {
		t.Fatal("Questions() has no acceptance_strength")
	}
	if strength.Type != "score" {
		t.Errorf("acceptance_strength has type %q, want score",
			strength.Type)
	}
	levels, ok := strength.Criteria.([]string)
	if !ok {
		t.Fatalf("acceptance_strength criteria is %T, want []string",
			strength.Criteria)
	}
	if len(levels) != 4 {
		t.Errorf("acceptance_strength has %d levels, want 4",
			len(levels))
	}
}

func TestStateCarriesEverySection(t *testing.T) {
	state := State(Parse(fixture(t, "vague-01.md")))

	if len(state) != 5 {
		t.Errorf("state has %d members, want 5: %v", len(state), state)
	}
	if s, _ := state["objective"].(string); !strings.Contains(
		s, "improve error handling",
	) {
		t.Errorf("objective = %q", s)
	}
	if s, _ := state["acceptance"].(string); !strings.Contains(
		s, "Command: go test ./internal/jev/",
	) {
		t.Errorf("acceptance = %q", s)
	}
	if state["spec"] != "" {
		t.Errorf("spec = %q, want empty", state["spec"])
	}
}

// ── Judge ───────────────────────────────────────────────────────────────────

// judgeAgainst runs Judge against a server that answers with body,
// and returns the report, the request the server received, and the
// error.
func judgeAgainst(t *testing.T, body, file string) (
	Report, map[string]any, error,
) {
	t.Helper()

	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Errorf("decode request: %v", err)
			}
			_, _ = w.Write([]byte(body))
		},
	))
	defer srv.Close()

	client := jev.New(srv.Client(), srv.URL, "sekret", "jev-test")
	report, err := Judge(
		context.Background(), client, file, fixture(t, file),
	)

	return report, got, err
}

func TestJudgeMapsEveryField(t *testing.T) {
	report, req, err := judgeAgainst(t, goodResponse, "vacuous-01.md")
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}

	if report.File != "vacuous-01.md" {
		t.Errorf("File = %q", report.File)
	}
	if len(report.Static) != len(checks) {
		t.Errorf("Static has %d findings, want %d",
			len(report.Static), len(checks))
	}
	if len(report.Judgments) != 2 {
		t.Errorf("Judgments = %v, want two entries", report.Judgments)
	}
	for id, want := range map[string]float64{
		"needs_interpretation": 0.12,
		"scope_generic":        0.05,
	} {
		if report.Judgments[id] != want {
			t.Errorf("Judgments[%q] = %v, want %v",
				id, report.Judgments[id], want)
		}
	}
	if report.Acceptance == nil {
		t.Fatal("Acceptance is nil, want the score")
	}
	if report.Acceptance.Score != 0.32 ||
		report.Acceptance.Confidence != 0.81 {
		t.Errorf("Acceptance = %+v, want 0.32 at 0.81",
			*report.Acceptance)
	}
	if report.Model != "jev-1" {
		t.Errorf("Model = %q, want jev-1", report.Model)
	}
	if report.Usage.InputTokens != 1400 ||
		report.Usage.OutputTokens != 9 {
		t.Errorf("Usage = %+v", report.Usage)
	}

	if req["model"] != "jev-test" {
		t.Errorf("model = %v, want jev-test", req["model"])
	}
	state, ok := req["state"].(map[string]any)
	if !ok {
		t.Fatalf("request state is %T", req["state"])
	}
	if s, _ := state["out_of_scope"].(string); !strings.Contains(
		s, "Writing the ledger",
	) {
		t.Errorf("out_of_scope = %q", s)
	}
}

func TestJudgeSkipsTheModelWhenSectionsAreMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(
		func(_ http.ResponseWriter, _ *http.Request) {
			t.Error("Judge sent a request for an unjudgeable file")
		},
	))
	defer srv.Close()

	client := jev.New(srv.Client(), srv.URL, "sekret", "jev-test")
	report, err := Judge(
		context.Background(), client, "broken-01.md",
		fixture(t, "broken-01.md"),
	)
	if err != nil {
		t.Fatalf("Judge: %v", err)
	}

	if report.Judgments != nil {
		t.Errorf("Judgments = %v, want nil", report.Judgments)
	}
	if report.Acceptance != nil {
		t.Errorf("Acceptance = %+v, want nil", *report.Acceptance)
	}
	if report.Model != "" || report.Usage != (jev.Usage{}) {
		t.Errorf("Model = %q, Usage = %+v, want zero",
			report.Model, report.Usage)
	}
	if len(report.Static) != len(checks) {
		t.Errorf("Static has %d findings, want %d",
			len(report.Static), len(checks))
	}
}

// TestJudgeRejectsAMissingAnswer feeds back a response without the
// acceptance_strength answer.
func TestJudgeRejectsAMissingAnswer(t *testing.T) {
	const body = `{"model":"jev-1","answers":{` +
		`"needs_interpretation":{"type":"noul","noul":0.12},` +
		`"scope_generic":{"type":"noul","noul":0.05}}}`

	report, _, err := judgeAgainst(t, body, "good-01.md")
	if err == nil {
		t.Fatalf("Judge returned %+v, want an error", report)
	}
	if !errors.As(err, new(*jev.ResponseError)) {
		t.Errorf("error is %T (%v), want *jev.ResponseError", err, err)
	}
	if report.File != "" || report.Static != nil {
		t.Errorf("report = %+v, want the zero value", report)
	}
}

// ── Question wording ────────────────────────────────────────────────────────

// The question text, written out here as literals so a change to
// tasklint.go cannot pass unnoticed. These are deliberately not
// derived from Questions().
const (
	wantInterpretation = "The state is one task specification written for " +
		"a less capable coding agent that executes specs literally. " +
		"`objective` says what must exist; `spec` (possibly empty) gives " +
		"interfaces and behavior. Does completing the task require the agent " +
		"to make design decisions, choose between approaches, or guess " +
		"intent, rather than execute steps and interfaces the spec states?"
	wantInterpretationTrue = "The objective or spec uses words like " +
		"improve, clean up, make robust, handle properly, or describes an " +
		"outcome without saying what to build; a capable engineer would need " +
		"to decide the design before starting."
	wantInterpretationFalse = "The task names the concrete things that will " +
		"exist (files, functions, types, behaviors) precisely enough that two " +
		"engineers would build the same thing."
	wantStrength = "`acceptance` lists shell commands, each with an " +
		"`Expect:` line, used to decide whether a coding task described by " +
		"`objective` was completed correctly. Rate how well the Expect lines " +
		"would catch a wrong or incomplete implementation."
	wantLevel0 = "The Expect lines check only that something runs, prints " +
		"anything, or exits 0. A wrong or empty implementation would pass."
	wantLevel1 = "The Expect lines mostly check that commands or a test " +
		"suite pass, with little or nothing tied to the specific behavior " +
		"the objective describes."
	wantLevel2 = "Some Expect lines name specific output, strings, counts, " +
		"or exit codes tied to the objective, but at least one only checks " +
		"that a command or test suite passes."
	wantLevel3 = "Every Expect line names specific output, strings, counts, " +
		"or exit codes tied to the behavior the objective describes. A wrong " +
		"implementation would fail."
	wantScope = "`out_of_scope` should name the specific tempting adjacent " +
		"work an agent might do while completing `objective`. Is it generic " +
		"instead, saying only 'anything else' or restating that unlisted " +
		"files are off limits, without naming concrete adjacent work?"
	wantScopeTrue = "Out of scope is boilerplate; it names no specific " +
		"feature, file, refactor, or dependency to avoid."
	wantScopeFalse = "Out of scope names at least one concrete thing an " +
		"agent might plausibly do and forbids it."
)

func TestQuestionWordingIsVerbatim(t *testing.T) {
	qs := Questions()

	for id, want := range map[string]string{
		"needs_interpretation": wantInterpretation,
		"acceptance_strength":  wantStrength,
		"scope_generic":        wantScope,
	} {
		if got := qs[id].Instructions; got != want {
			t.Errorf("%s instructions drifted:\n got %q\nwant %q",
				id, got, want)
		}
	}

	for id, want := range map[string]map[string]string{
		"needs_interpretation": {
			"true":  wantInterpretationTrue,
			"false": wantInterpretationFalse,
		},
		"scope_generic": {
			"true":  wantScopeTrue,
			"false": wantScopeFalse,
		},
	} {
		criteria, ok := qs[id].Criteria.(map[string]string)
		if !ok {
			t.Fatalf("%s criteria is %T", id, qs[id].Criteria)
		}
		for outcome, text := range want {
			if criteria[outcome] != text {
				t.Errorf("%s %s criterion drifted:\n got %q\nwant %q",
					id, outcome, criteria[outcome], text)
			}
		}
	}

	levels, ok := qs["acceptance_strength"].Criteria.([]string)
	if !ok {
		t.Fatalf("acceptance_strength criteria is %T",
			qs["acceptance_strength"].Criteria)
	}
	for i, text := range []string{
		wantLevel0, wantLevel1, wantLevel2, wantLevel3,
	} {
		if i >= len(levels) {
			t.Fatalf("acceptance_strength has %d levels, want 4",
				len(levels))
		}
		if levels[i] != text {
			t.Errorf("acceptance_strength level %d drifted:"+
				"\n got %q\nwant %q", i, levels[i], text)
		}
	}
}
