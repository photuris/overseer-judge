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

	"overseer-judge/internal/jev"
)

// goodResponse answers all three questions.
const goodResponse = `{"model":"jev-1","answers":{` +
	`"needs_interpretation":{"type":"noul","noul":0.12},` +
	`"acceptance_vacuous":{"type":"noul","noul":0.88},` +
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
		"needs_interpretation", "acceptance_vacuous", "scope_generic",
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
	for id, want := range map[string]float64{
		"needs_interpretation": 0.12,
		"acceptance_vacuous":   0.88,
		"scope_generic":        0.05,
	} {
		if report.Judgments[id] != want {
			t.Errorf("Judgments[%q] = %v, want %v",
				id, report.Judgments[id], want)
		}
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
	if report.Model != "" || report.Usage != (jev.Usage{}) {
		t.Errorf("Model = %q, Usage = %+v, want zero",
			report.Model, report.Usage)
	}
	if len(report.Static) != len(checks) {
		t.Errorf("Static has %d findings, want %d",
			len(report.Static), len(checks))
	}
}

func TestJudgeRejectsAMissingAnswer(t *testing.T) {
	const body = `{"model":"jev-1","answers":{` +
		`"needs_interpretation":{"type":"noul","noul":0.12},` +
		`"acceptance_vacuous":{"type":"noul","noul":0.88}}}`

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
	wantVacuous = "`acceptance` lists shell commands with an `Expect:` line " +
		"each. Could the expectation be met even if the work were wrong or " +
		"incomplete, because it checks only that a command runs, prints " +
		"something, or exits 0 without asserting the specific result the " +
		"objective requires?"
	wantVacuousTrue = "The Expect lines could pass on an empty or broken " +
		"implementation, or do not name a specific observable outcome."
	wantVacuousFalse = "The Expect lines name specific output, counts, " +
		"strings, or exit codes that a wrong implementation would fail."
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
		"acceptance_vacuous":   wantVacuous,
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
		"acceptance_vacuous": {
			"true":  wantVacuousTrue,
			"false": wantVacuousFalse,
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
}
