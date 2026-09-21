package session

import (
	"context"
	"fmt"

	"overseer-judge/internal/jev"
)

// stateInstructions is the instruction text for the state question.
const stateInstructions = "`transcript_tail` is the most recent " +
	"terminal output of a coding agent (`agent_kind`) running in a " +
	"terminal pane. Classify what the pane is doing right now, " +
	"judged by the last few screens of output, giving most weight " +
	"to the final lines."

// coherentInstructions is the instruction text for the coherent
// question.
const coherentInstructions = "Is the most recent output in " +
	"`transcript_tail` coherent, on-task natural language or code, " +
	"as opposed to repetition loops, mixed-language gibberish, or " +
	"random tokens?"

// Verdict is the session judgment.
type Verdict struct {
	State         string             `json:"state"`
	Confidence    float64            `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities"`
	Coherent      float64            `json:"coherent"`
	Model         string             `json:"model"`
	Usage         jev.Usage          `json:"usage"`
}

// stateCriteria returns the six labels the state question chooses
// between, each with the description the model judges against.
func stateCriteria() map[string]string {
	return map[string]string{
		"working": "The agent is actively producing coherent, " +
			"on-task output: tool calls, code, file edits, test " +
			"runs, or prose that advances a task. A spinner or " +
			"'thinking' indicator with recent coherent output " +
			"counts as working.",
		"idle": "The agent has finished and is waiting at its " +
			"input prompt with nothing typed and no dialog. A " +
			"completed answer followed by an empty prompt line is " +
			"idle.",
		"dialog": "A modal question, approval prompt, folder-trust " +
			"prompt, update notice, usage-limit or rate-limit " +
			"notice, or any UI that waits for a keypress or choice " +
			"before the agent can continue.",
		"unsubmitted": "Text has been typed into the agent's input " +
			"line or composer but not submitted; the agent is not " +
			"working on it.",
		"error": "The agent's work ended with an API error, crash, " +
			"stack trace, connection failure, or process exit, and " +
			"it is not continuing.",
		"degraded": "The output has become incoherent: the same " +
			"line or phrase repeating many times, mixed-language " +
			"token salad, random characters, or text that no " +
			"longer relates to any task, while the agent appears " +
			"to keep producing it.",
	}
}

// coherentCriteria returns the two outcomes the coherent question is
// judged against.
func coherentCriteria() map[string]string {
	return map[string]string{
		"true": "The latest output reads as purposeful text or code " +
			"a competent engineer would write.",
		"false": "The latest output is repetitive, garbled, " +
			"multilingual salad, or otherwise meaningless.",
	}
}

// Questions returns the two questions sent for every session
// judgment.
func Questions() map[string]jev.Question {
	return map[string]jev.Question{
		"state": {
			Type:         "choice",
			Instructions: stateInstructions,
			Criteria:     stateCriteria(),
		},
		"coherent": {
			Type:         "noul",
			Instructions: coherentInstructions,
			Criteria:     coherentCriteria(),
		},
	}
}

// State builds the request state for a cleaned tail.
func State(agentKind, tail string) map[string]any {
	return map[string]any{
		"agent_kind":      agentKind,
		"transcript_tail": tail,
	}
}

// Request returns the Jev request Judge sends for rawTail, with the
// tail cleaned using the defaults. Model is left empty so the client
// or the caller fills it.
func Request(agentKind, rawTail string) jev.Request {
	return jev.Request{
		State:     State(agentKind, Clean(rawTail, MaxLines, MaxBytes)),
		Questions: Questions(),
	}
}

// Judge cleans rawTail, asks the two questions, and maps the answers
// to a Verdict. Every contract violation, including a chosen label
// outside the six, is returned as a *jev.ResponseError.
func Judge(
	ctx context.Context,
	c *jev.Client,
	agentKind, rawTail string,
) (Verdict, error) {
	resp, err := c.Ask(ctx, Request(agentKind, rawTail))
	if err != nil {
		return Verdict{}, err
	}

	state, confidence, probs, err := resp.Choice("state")
	if err != nil {
		return Verdict{}, err
	}
	if _, ok := stateCriteria()[state]; !ok {
		return Verdict{}, &jev.ResponseError{Message: fmt.Sprintf(
			`answer "state" chose %q, not one of the six labels`,
			state,
		)}
	}

	coherent, err := resp.Noul("coherent")
	if err != nil {
		return Verdict{}, err
	}

	return Verdict{
		State:         state,
		Confidence:    confidence,
		Probabilities: probs,
		Coherent:      coherent,
		Model:         resp.Model,
		Usage:         resp.Usage,
	}, nil
}
