package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
)

// rawExample is the example shown in `raw --help`.
const rawExample = `printf '%s' '{"state":"Payouts have failed for 3 days.",
  "questions":{"urgent":{"type":"noul",
  "instructions":"Does this convey urgency?"}}}' |
  overseer-judge raw --input - --pretty`

// rawCommand returns the raw verb: an escape hatch that sends an
// arbitrary Jev request through the authenticated client.
func rawCommand() command {
	var input string

	return command{
		name:    "raw",
		summary: "send an arbitrary Jev request read from --input",
		args:    "--input <path|-> [flags]",
		output:  "the System One response as one JSON object.",
		example: rawExample,
		flags: func(fs *flag.FlagSet) {
			fs.StringVar(&input, "input", "",
				"request JSON to send; - reads stdin (required)")
		},
		run: func(
			ctx context.Context,
			g *globals,
			args []string,
			stdin io.Reader,
			stdout io.Writer,
		) error {
			return runRaw(ctx, g, input, args, stdin, stdout)
		},
	}
}

// dryRunRecord is what --dry-run prints instead of sending.
type dryRunRecord struct {
	Method string          `json:"method"`
	Path   string          `json:"path"`
	Body   json.RawMessage `json:"body"`
}

// runRaw reads a Jev request, fills in the model if absent, and
// either prints it (--dry-run) or sends it.
func runRaw(
	ctx context.Context,
	g *globals,
	input string,
	args []string,
	stdin io.Reader,
	stdout io.Writer,
) error {
	if len(args) > 0 {
		return usagef(
			"raw takes no positional arguments, got %q", args[0],
		)
	}
	if input == "" {
		return usagef("raw requires --input <path|->")
	}

	r, closeFn, err := openInput(input, stdin)
	if err != nil {
		return err
	}
	defer closeFn()

	body, err := rawBody(r, g.effectiveModel())
	if err != nil {
		return err
	}

	if g.dryRun {
		return writeJSON(stdout, g.pretty, dryRunRecord{
			Method: "POST", Path: "/v1/systemone", Body: body,
		})
	}

	client, err := g.client()
	if err != nil {
		return err
	}

	g.log.Debug("sending request", "bytes", len(body))

	resp, err := client.AskRaw(ctx, body)
	if err != nil {
		return err
	}

	return writeJSON(stdout, g.pretty, resp)
}

// rawBody reads one JSON object from r, inserts model when the object
// has none, and re-marshals it. Anything that is not a lone JSON
// object carrying state and questions is a usage error.
func rawBody(r io.Reader, model string) (json.RawMessage, error) {
	var obj map[string]json.RawMessage

	dec := json.NewDecoder(r)
	if err := dec.Decode(&obj); err != nil {
		return nil, usagef("--input must be one JSON object: %v", err)
	}
	if obj == nil {
		return nil, usagef("--input must be a JSON object, got null")
	}
	if dec.More() {
		return nil, usagef(
			"--input must hold exactly one JSON object",
		)
	}

	for _, key := range []string{"state", "questions"} {
		if _, ok := obj[key]; !ok {
			return nil, usagef("--input is missing %q", key)
		}
	}

	if _, ok := obj["model"]; !ok {
		encoded, err := json.Marshal(model)
		if err != nil {
			return nil, fmt.Errorf("encode model: %w", err)
		}
		obj["model"] = encoded
	}

	body, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}

	return body, nil
}

// openInput returns a reader for path, where "-" means stdin, plus a
// close function.
func openInput(
	path string, stdin io.Reader,
) (io.Reader, func(), error) {
	if path == "-" {
		return stdin, func() {}, nil
	}

	f, err := os.Open(path)
	if err != nil {
		return nil, nil, usagef("open --input: %v", err)
	}

	return f, func() { _ = f.Close() }, nil
}

// writeJSON writes v to w as one JSON document plus a newline,
// indented when pretty.
func writeJSON(w io.Writer, pretty bool, v any) error {
	enc := json.NewEncoder(w)
	if pretty {
		enc.SetIndent("", "  ")
	}
	if err := enc.Encode(v); err != nil {
		return fmt.Errorf("write output: %w", err)
	}

	return nil
}
