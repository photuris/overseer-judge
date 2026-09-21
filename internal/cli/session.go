package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"slices"

	"overseer-judge/internal/jev"
	"overseer-judge/internal/session"
)

// agentKinds are the accepted --agent values.
var agentKinds = []string{"claude", "codex", "unknown"}

// sessionExample is the example shown in `session --help`. The note
// is part of the contract: only ANSI input lets Clean drop an agent's
// greyed-out prompt suggestion, which in plain text cannot be told
// apart from unsubmitted input.
const sessionExample = `# ANSI input is preferred. Only ANSI lets the tool
# drop an agent's greyed-out prompt suggestion,
# which in plain text is indistinguishable from
# text the user typed and has not submitted.
herdr agent read <pane> --lines 60 \
  --source recent-unwrapped --format ansi |
  overseer-judge session --input - --agent claude`

// sessionCommand returns the session verb: it classifies what an
// agent's terminal pane is doing from the tail of its transcript.
func sessionCommand() command {
	var input, agent string

	return command{
		name:    "session",
		summary: "classify an agent pane's transcript tail read from --input",
		args:    "--input <path|-> [flags]",
		output: "the verdict as one JSON object: state, " +
			"confidence, probabilities, coherent, model, usage.",
		example: sessionExample,
		flags: func(fs *flag.FlagSet) {
			fs.StringVar(&input, "input", "",
				"transcript tail; - reads stdin (required)")
			fs.StringVar(&agent, "agent", "unknown",
				"agent kind: claude, codex, or unknown")
		},
		run: func(
			ctx context.Context,
			g *globals,
			args []string,
			stdin io.Reader,
			stdout io.Writer,
		) error {
			return runSession(
				ctx, g, input, agent, args, stdin, stdout,
			)
		},
	}
}

// runSession reads a transcript tail and either prints the request
// (--dry-run) or prints the verdict.
func runSession(
	ctx context.Context,
	g *globals,
	input, agent string,
	args []string,
	stdin io.Reader,
	stdout io.Writer,
) error {
	if len(args) > 0 {
		return usagef(
			"session takes no positional arguments, got %q", args[0],
		)
	}
	if input == "" {
		return usagef("session requires --input <path|->")
	}
	if !slices.Contains(agentKinds, agent) {
		return usagef(
			"invalid --agent %q: want claude, codex, or unknown",
			agent,
		)
	}

	r, closeFn, err := openInput(input, stdin)
	if err != nil {
		return err
	}
	defer closeFn()

	raw, err := io.ReadAll(r)
	if err != nil {
		return usagef("read --input: %v", err)
	}

	if g.dryRun {
		return writeDryRun(stdout, g, agent, string(raw))
	}

	client, err := g.client()
	if err != nil {
		return err
	}

	g.log.Debug("classifying tail", "bytes", len(raw), "agent", agent)

	verdict, err := session.Judge(ctx, client, agent, string(raw))
	if err != nil {
		return err
	}

	return writeJSON(stdout, g.pretty, verdict)
}

// writeDryRun prints the request Judge would have sent.
func writeDryRun(
	stdout io.Writer, g *globals, agent, raw string,
) error {
	req := session.Request(agent, raw)
	req.Model = g.effectiveModel()

	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}

	return writeJSON(stdout, g.pretty, dryRunRecord{
		Method: "POST", Path: jev.Path, Body: body,
	})
}
