// Package tasklint reads an overseer task file, checks its structure
// in code, and asks Jev the three questions that need judgment.
package tasklint

import (
	"context"

	"github.com/photuris/overseer-judge/internal/jev"
)

// SoundCut is the acceptance score at or above which a task's
// Expect lines are read as sound. It is provisional: it was fitted
// to five fixtures, so re-measure before trusting it on new ones.
const SoundCut = 2.2

// interpretationInstructions is the instruction text for the
// needs_interpretation question.
const interpretationInstructions = "The state is one task " +
	"specification written for a less capable coding agent that " +
	"executes specs literally. `objective` says what must exist; " +
	"`spec` (possibly empty) gives interfaces and behavior. Does " +
	"completing the task require the agent to make design " +
	"decisions, choose between approaches, or guess intent, rather " +
	"than execute steps and interfaces the spec states?"

// strengthInstructions is the instruction text for the
// acceptance_strength question.
const strengthInstructions = "`acceptance` lists shell commands, " +
	"each with an `Expect:` line, used to decide whether a coding " +
	"task described by `objective` was completed correctly. Rate " +
	"how well the Expect lines would catch a wrong or incomplete " +
	"implementation."

// scopeInstructions is the instruction text for the scope_generic
// question.
const scopeInstructions = "`out_of_scope` should name the specific " +
	"tempting adjacent work an agent might do while completing " +
	"`objective`. Is it generic instead, saying only 'anything " +
	"else' or restating that unlisted files are off limits, without " +
	"naming concrete adjacent work?"

// nouls are the questions whose answers land in Report.Judgments.
var nouls = []string{"needs_interpretation", "scope_generic"}

// Strength is the graded acceptance judgment: Score runs 0 (vacuous)
// to 3 (every Expect is specific).
type Strength struct {
	Score      float64 `json:"score"`
	Confidence float64 `json:"confidence"`
}

// Report is the full lint output for one task file.
type Report struct {
	File      string             `json:"file"`
	Static    []Finding          `json:"static"`
	Judgments map[string]float64 `json:"judgments,omitempty"`
	// Acceptance is nil when the model call is skipped.
	Acceptance *Strength `json:"acceptance,omitempty"`
	Model      string    `json:"model,omitempty"`
	Usage      jev.Usage `json:"usage"`
}

// interpretationCriteria returns the two outcomes the
// needs_interpretation question is judged against.
func interpretationCriteria() map[string]string {
	return map[string]string{
		"true": "The objective or spec uses words like improve, " +
			"clean up, make robust, handle properly, or describes " +
			"an outcome without saying what to build; a capable " +
			"engineer would need to decide the design before " +
			"starting.",
		"false": "The task names the concrete things that will " +
			"exist (files, functions, types, behaviors) precisely " +
			"enough that two engineers would build the same thing.",
	}
}

// strengthCriteria returns the four levels the acceptance_strength
// question scores against, worst first: the index of the level the
// model picks is the score.
func strengthCriteria() []string {
	return []string{
		"The Expect lines check only that something runs, prints " +
			"anything, or exits 0. A wrong or empty implementation " +
			"would pass.",
		"The Expect lines mostly check that commands or a test " +
			"suite pass, with little or nothing tied to the " +
			"specific behavior the objective describes.",
		"Some Expect lines name specific output, strings, counts, " +
			"or exit codes tied to the objective, but at least one " +
			"only checks that a command or test suite passes.",
		"Every Expect line names specific output, strings, counts, " +
			"or exit codes tied to the behavior the objective " +
			"describes. A wrong implementation would fail.",
	}
}

// scopeCriteria returns the two outcomes the scope_generic question
// is judged against.
func scopeCriteria() map[string]string {
	return map[string]string{
		"true": "Out of scope is boilerplate; it names no specific " +
			"feature, file, refactor, or dependency to avoid.",
		"false": "Out of scope names at least one concrete thing an " +
			"agent might plausibly do and forbids it.",
	}
}

// Questions returns the three questions sent for every task
// judgment.
func Questions() map[string]jev.Question {
	return map[string]jev.Question{
		"needs_interpretation": {
			Type:         "noul",
			Instructions: interpretationInstructions,
			Criteria:     interpretationCriteria(),
		},
		"acceptance_strength": {
			Type:         "score",
			Instructions: strengthInstructions,
			Criteria:     strengthCriteria(),
		},
		"scope_generic": {
			Type:         "noul",
			Instructions: scopeInstructions,
			Criteria:     scopeCriteria(),
		},
	}
}

// State builds the request state from a Doc. A section the document
// lacks contributes the empty string.
func State(d Doc) map[string]any {
	return map[string]any{
		"objective":    d.Section("Objective"),
		"files":        d.Section("Files"),
		"out_of_scope": d.Section("Out of scope"),
		"acceptance":   d.Section("Acceptance"),
		"spec":         d.Section("Spec"),
	}
}

// Request returns the Jev request Judge sends for d. Model is left
// empty so the client or the caller fills it.
func Request(d Doc) jev.Request {
	return jev.Request{State: State(d), Questions: Questions()}
}

// Judge parses text, runs the static checks, and, unless the
// document is missing the sections the questions read, asks the two
// Nouls and the acceptance Score. When the model call is skipped the
// report carries the static findings alone and no request is made.
func Judge(
	ctx context.Context,
	c *jev.Client,
	file, text string,
) (Report, error) {
	d := Parse(text)
	report := Report{File: file, Static: Static(d)}

	if !d.Judgeable() {
		return report, nil
	}

	resp, err := c.Ask(ctx, Request(d))
	if err != nil {
		return Report{}, err
	}

	report.Judgments = make(map[string]float64, len(nouls))
	for _, id := range nouls {
		p, err := resp.Noul(id)
		if err != nil {
			return Report{}, err
		}
		report.Judgments[id] = p
	}

	score, confidence, err := resp.Score("acceptance_strength")
	if err != nil {
		return Report{}, err
	}
	report.Acceptance = &Strength{Score: score, Confidence: confidence}

	report.Model = resp.Model
	report.Usage = resp.Usage

	return report, nil
}
