package session

import (
	"context"
	"fmt"

	"github.com/photuris/overseer-judge/internal/jev"
)

// stateInstructions is the instruction text for the state question.
const stateInstructions = "`transcript_tail` is the most recent terminal " +
	"output of a coding agent (`agent_kind`) running in a terminal pane. " +
	"`input_line` is the text currently sitting in the agent's input box, " +
	"extracted by code (empty string if the box is empty or was not found). " +
	"`activity_hint` is the agent's own busy-indicator line as found on " +
	"screen by code, for example a spinner or progress bar beside 'esc to " +
	"interrupt'; it is an empty string when no busy indicator is on screen. " +
	"Classify what the pane is doing right now, judged by the last few " +
	"screens of output, giving most weight to the final lines."

// coherentInstructions is the instruction text for the coherent
// question.
const coherentInstructions = "Is the most recent output in " +
	"`transcript_tail` coherent, on-task natural language or code, " +
	"as opposed to repetition loops, mixed-language gibberish, or " +
	"random tokens?"

// Gate is the confidence a session verdict must reach before a
// caller acts on it unattended: below this, callers should escalate
// to a person or a reasoning model. Nothing here enforces it; it is
// exported for the live test and for callers to compare against.
const Gate = 0.9

// Verdict is the session judgment.
type Verdict struct {
	State         string             `json:"state"`
	Confidence    float64            `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities"`
	Coherent      float64            `json:"coherent"`
	InputLine     string             `json:"input_line"`
	ActivityHint  string             `json:"activity_hint"`
	Model         string             `json:"model"`
	Usage         jev.Usage          `json:"usage"`
}

// stateCriteria returns the six labels the state question chooses
// between, each with the description the model judges against.
func stateCriteria() map[string]string {
	return map[string]string{
		"working": "The agent is busy right now and its output is coherent. " +
			"`activity_hint` is non-empty, showing the agent's own busy " +
			"indicator; a busy indicator with no output yet still counts. When " +
			"`activity_hint` is empty, choose this only if the final lines show a " +
			"tool call or command still in progress. Finished output above an " +
			"input box is not working, and text in `input_line` does not mean the " +
			"agent is working.",
		"idle": "The agent has finished and is waiting for input. " +
			"`activity_hint` is empty, `input_line` is empty or holds only the " +
			"tool's placeholder hint (for example 'Ask Codex to do anything'), " +
			"and there is no dialog.",
		"dialog": "A modal UI that blocks the agent until a person " +
			"responds: an approval or permission prompt, a " +
			"folder-trust prompt, a numbered or yes/no choice " +
			"under a cursor, or a usage-limit or rate-limit screen " +
			"that must be dismissed before work can continue. " +
			"Informational banners, warnings, update notices, and " +
			"tips that do not wait for a keypress are not dialogs.",
		"unsubmitted": "`input_line` holds text a person typed (an instruction, " +
			"question, or partial message, not the tool's placeholder hint) and " +
			"`activity_hint` is empty, so the agent is not acting on it. The text " +
			"has been typed but not sent, however much finished output sits above " +
			"it.",
		"error": "The agent's work ended with an API error, crash, " +
			"stack trace, connection failure, or process exit, and " +
			"it is not continuing.",
		"degraded": "The output has become incoherent: the same line or phrase " +
			"repeating many times, mixed-language token salad, random characters, " +
			"or text that no longer relates to any task. This holds even when " +
			"`activity_hint` shows a busy indicator, because a degraded agent " +
			"keeps producing.",
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

// State builds the request state for a cleaned tail, including the
// input line extracted from it.
func State(agentKind, tail string) map[string]any {
	return map[string]any{
		"agent_kind":      agentKind,
		"transcript_tail": tail,
		"input_line":      InputLine(agentKind, tail),
		"activity_hint":   ActivityHint(agentKind, tail),
	}
}

// Request returns the Jev request Judge sends for rawTail, with the
// tail cleaned using the defaults. Model is left empty so the client
// or the caller fills it.
func Request(agentKind, rawTail string) jev.Request {
	return requestFor(agentKind, Clean(rawTail, MaxLines, MaxBytes))
}

// requestFor builds the request from an already-cleaned tail, so
// Judge cleans once and reports what the cleaning extracted.
func requestFor(agentKind, cleaned string) jev.Request {
	return jev.Request{
		State:     State(agentKind, cleaned),
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
	cleaned := Clean(rawTail, MaxLines, MaxBytes)

	resp, err := c.Ask(ctx, requestFor(agentKind, cleaned))
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
		InputLine:     InputLine(agentKind, cleaned),
		ActivityHint:  ActivityHint(agentKind, cleaned),
		Model:         resp.Model,
		Usage:         resp.Usage,
	}, nil
}
