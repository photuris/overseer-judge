package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"

	"overseer-judge/internal/jev"
	"overseer-judge/internal/review"
)

// reviewExample is the example shown in `review --help`. Flags
// precede the path: flag parsing stops at the first positional
// argument.
const reviewExample = `overseer-judge review .overseer/reviews/` +
	`round-04.md |
  jq -c 'select(.style_only > 0.5)'`

// reviewCommand returns the review verb: it types each finding in an
// overseer review round and each reply to it.
func reviewCommand() command {
	return command{
		name: "review",
		summary: "type each item and response in an overseer " +
			"review round file",
		args: "[flags] <path|->",
		output: "JSON Lines, one object per item: id, severity, " +
			"style_only, responses, model, usage. --pretty is " +
			"rejected: a record stays on one line.",
		example: reviewExample,
		flags:   func(_ *flag.FlagSet) {},
		run: func(
			ctx context.Context,
			g *globals,
			args []string,
			stdin io.Reader,
			stdout io.Writer,
		) error {
			return runReview(ctx, g, args, stdin, stdout)
		},
	}
}

// runReview reads a review round and prints one line per item: the
// request with --dry-run, otherwise the judgment.
func runReview(
	ctx context.Context,
	g *globals,
	args []string,
	stdin io.Reader,
	stdout io.Writer,
) error {
	if len(args) == 0 {
		return usagef("review requires <path|->")
	}
	if len(args) > 1 {
		return usagef(
			"review takes one positional argument, got %q", args[1],
		)
	}
	// JSONL stays one record per line, so there is nothing to indent.
	if g.pretty {
		return usagef("review does not accept --pretty: its output " +
			"is JSON Lines, one item per line")
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

	items := review.Parse(string(raw))
	g.log.Debug("parsed review round", "file", args[0],
		"items", len(items))

	if g.dryRun {
		return writeReviewDryRun(stdout, g, items)
	}

	client, err := g.client()
	if err != nil {
		return err
	}

	// One item at a time, so a long round streams rather than
	// arriving all at once when the last request returns.
	for _, it := range items {
		typed, err := review.Judge(
			ctx, client, []review.Item{it},
		)
		if err != nil {
			return err
		}
		if err := writeJSON(stdout, false, typed[0]); err != nil {
			return err
		}
	}

	return nil
}

// writeReviewDryRun prints the request Judge would send for each
// item, one per line, in order.
func writeReviewDryRun(
	stdout io.Writer, g *globals, items []review.Item,
) error {
	for _, it := range items {
		req := review.Request(it)
		req.Model = g.effectiveModel()

		body, err := json.Marshal(req)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}

		if err := writeJSON(stdout, false, dryRunRecord{
			Method: "POST", Path: jev.Path, Body: body,
		}); err != nil {
			return err
		}
	}

	return nil
}
