// Package review reads an overseer review round, splits it into
// findings and the implementer's replies, and asks Jev to type each
// one: whether the finding is style only, and what each reply does.
package review

import (
	"context"
	"fmt"
	"strconv"

	"overseer-judge/internal/jev"
)

// styleInstructions is the instruction text for the style_only
// question.
const styleInstructions = "`finding` is one code-review finding " +
	"written against a task specification. Is it purely a style or " +
	"preference remark (naming, formatting, ordering, wording, " +
	"idiom choice) with no claim about correctness, behavior, the " +
	"task's acceptance criteria, or scope?"

// Typed is one item's judgment, emitted as one JSONL line.
type Typed struct {
	ID        string          `json:"id"`
	Severity  string          `json:"severity,omitempty"`
	StyleOnly float64         `json:"style_only"`
	Responses []TypedResponse `json:"responses"`
	Model     string          `json:"model"`
	Usage     jev.Usage       `json:"usage"`
}

// TypedResponse is one reply's classification.
type TypedResponse struct {
	Kind          string             `json:"kind"`
	Confidence    float64            `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities"`
}

// responseID is the question id for the nth response.
func responseID(n int) string {
	return "response_" + strconv.Itoa(n)
}

// responseInstructions is the instruction text for the nth response
// question, which names the reply it is asked about.
func responseInstructions(n int) string {
	return "`responses[" + strconv.Itoa(n) + "]` is the " +
		"implementer's reply to `finding`. Classify the reply by " +
		"what it does, not by whether it is right."
}

// styleCriteria returns the two outcomes the style_only question is
// judged against.
func styleCriteria() map[string]string {
	return map[string]string{
		"true": "The finding would not change what the program does " +
			"or whether the task's acceptance passes; it is taste.",
		"false": "The finding claims a bug, a missing behavior, a " +
			"test gap, a violated acceptance criterion, or work " +
			"outside the allowed scope.",
	}
}

// kindCriteria returns the five labels a reply is classified into,
// each with the description the model judges against.
func kindCriteria() map[string]string {
	return map[string]string{
		"fixed": "Says a change was made and names where or how (a " +
			"function, file, commit, or test), addressing the " +
			"finding.",
		"evidence": "Disputes the finding by pointing at something " +
			"checkable: a code citation with a path or line, an " +
			"existing test, a command output, or a reproduction.",
		"concern": "Disputes the finding by argument or opinion " +
			"only, with nothing checkable named.",
		"question": "Asks the reviewer for clarification or more " +
			"information instead of fixing or disputing.",
		"agree": "Accepts the finding without claiming a fix yet, " +
			"e.g. 'will do', 'agreed, next round'.",
	}
}

// Questions builds the question set for one item: style_only, plus
// response_N (N from 0) for each response.
func Questions(it Item) map[string]jev.Question {
	qs := map[string]jev.Question{
		"style_only": {
			Type:         "noul",
			Instructions: styleInstructions,
			Criteria:     styleCriteria(),
		},
	}

	for n := range it.Responses {
		qs[responseID(n)] = jev.Question{
			Type:         "choice",
			Instructions: responseInstructions(n),
			Criteria:     kindCriteria(),
		}
	}

	return qs
}

// State builds the request state for one item.
func State(it Item) map[string]any {
	responses := it.Responses
	if responses == nil {
		responses = []string{}
	}

	return map[string]any{
		"finding": map[string]any{
			"id":       it.ID,
			"title":    it.Title,
			"file":     it.File,
			"severity": it.Severity,
			"body":     it.Body,
		},
		"responses": responses,
	}
}

// Request returns the Jev request Judge sends for one item. Model is
// left empty so the client or the caller fills it.
func Request(it Item) jev.Request {
	return jev.Request{State: State(it), Questions: Questions(it)}
}

// Judge sends one request per item, sequentially, and returns the
// results in input order. The first error aborts and is returned.
func Judge(
	ctx context.Context, c *jev.Client, items []Item,
) ([]Typed, error) {
	out := make([]Typed, 0, len(items))

	for _, it := range items {
		typed, err := judgeItem(ctx, c, it)
		if err != nil {
			return nil, err
		}
		out = append(out, typed)
	}

	return out, nil
}

// judgeItem asks the questions for one item and maps the answers.
// Every contract violation, including a kind outside the five
// labels, is returned as a *jev.ResponseError.
func judgeItem(
	ctx context.Context, c *jev.Client, it Item,
) (Typed, error) {
	resp, err := c.Ask(ctx, Request(it))
	if err != nil {
		return Typed{}, err
	}

	style, err := resp.Noul("style_only")
	if err != nil {
		return Typed{}, err
	}

	typed := Typed{
		ID:        it.ID,
		Severity:  it.Severity,
		StyleOnly: style,
		Responses: make([]TypedResponse, 0, len(it.Responses)),
		Model:     resp.Model,
		Usage:     resp.Usage,
	}

	kinds := kindCriteria()
	for n := range it.Responses {
		kind, confidence, probs, err := resp.Choice(responseID(n))
		if err != nil {
			return Typed{}, err
		}
		if _, ok := kinds[kind]; !ok {
			return Typed{}, &jev.ResponseError{Message: fmt.Sprintf(
				"answer %q chose %q, not one of the five labels",
				responseID(n), kind,
			)}
		}

		typed.Responses = append(typed.Responses, TypedResponse{
			Kind:          kind,
			Confidence:    confidence,
			Probabilities: probs,
		})
	}

	return typed, nil
}
