package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"

	"overseer-judge/internal/jev"
	"overseer-judge/internal/tasklint"
)

// taskExample is the example shown in `task --help`. Flags precede
// the path: flag parsing stops at the first positional argument.
const taskExample = `overseer-judge task --pretty ` +
	`.overseer/tasks/003-tasklint.md`

// taskCommand returns the task verb: it lints an overseer task file
// for the spec defects an implementer would have to interpret.
func taskCommand() command {
	return command{
		name:    "task",
		summary: "lint an overseer task file for spec defects",
		args:    "[flags] <path|->",
		output: "the lint report as one JSON object: file, static, " +
			"judgments, model, usage.",
		example: taskExample,
		flags:   func(_ *flag.FlagSet) {},
		run: func(
			ctx context.Context,
			g *globals,
			args []string,
			stdin io.Reader,
			stdout io.Writer,
		) error {
			return runTask(ctx, g, args, stdin, stdout)
		},
	}
}

// taskDryRun is what `task --dry-run` prints when Judge would skip
// the model call: there is no request to show, so the static
// findings are all there is to report.
type taskDryRun struct {
	dryRunRecord
	Static []tasklint.Finding `json:"static"`
}

// runTask reads a task file and either prints the request
// (--dry-run) or prints the lint report.
func runTask(
	ctx context.Context,
	g *globals,
	args []string,
	stdin io.Reader,
	stdout io.Writer,
) error {
	if len(args) == 0 {
		return usagef("task requires <path|->")
	}
	if len(args) > 1 {
		return usagef(
			"task takes one positional argument, got %q", args[1],
		)
	}

	r, closeFn, err := openInput(args[0], stdin)
	if err != nil {
		return err
	}
	defer closeFn()

	raw, err := io.ReadAll(r)
	if err != nil {
		return usagef("read %s: %v", args[0], err)
	}

	if g.dryRun {
		return writeTaskDryRun(stdout, g, string(raw))
	}

	client, err := g.client()
	if err != nil {
		return err
	}

	g.log.Debug("linting task file", "file", args[0],
		"bytes", len(raw))

	report, err := tasklint.Judge(ctx, client, args[0], string(raw))
	if err != nil {
		return err
	}

	return writeJSON(stdout, g.pretty, report)
}

// writeTaskDryRun prints the request Judge would have sent, or the
// static findings alone when Judge would send nothing.
func writeTaskDryRun(stdout io.Writer, g *globals, raw string) error {
	d := tasklint.Parse(raw)
	if !d.Judgeable() {
		return writeJSON(stdout, g.pretty, taskDryRun{
			Static: tasklint.Static(d),
		})
	}

	req := tasklint.Request(d)
	req.Model = g.effectiveModel()

	body, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}

	return writeJSON(stdout, g.pretty, dryRunRecord{
		Method: "POST", Path: jev.Path, Body: body,
	})
}
