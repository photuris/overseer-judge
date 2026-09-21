// Package cli parses overseer-judge's arguments, dispatches verbs,
// and maps failures onto exit codes.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"overseer-judge/internal/config"
	"overseer-judge/internal/jev"
)

// Version is the tool's version, set at build time.
var Version = "dev"

// keyFile is where the API key is read from when TYPESAFE_API_KEY is
// empty. Tests point it at a temporary path.
var keyFile = config.DefaultKeyFile()

// logLevels maps --log-level values onto slog levels.
var logLevels = map[string]slog.Level{
	"debug": slog.LevelDebug,
	"info":  slog.LevelInfo,
	"warn":  slog.LevelWarn,
	"error": slog.LevelError,
}

// globals carries the parsed global flags, the non-secret config, and
// a lazily built Jev client.
type globals struct {
	pretty   bool
	dryRun   bool
	model    string
	timeout  time.Duration
	logLevel string

	cfg    config.Config
	log    *slog.Logger
	client func() (*jev.Client, error)
}

// effectiveModel returns --model if set, else the configured default.
func (g *globals) effectiveModel() string {
	if g.model != "" {
		return g.model
	}

	return g.cfg.Model
}

// addGlobals registers the flags every verb accepts.
func addGlobals(fs *flag.FlagSet, g *globals) {
	fs.BoolVar(&g.pretty, "pretty", false,
		"indent JSON output")
	fs.BoolVar(&g.dryRun, "dry-run", false,
		"print the request that would be sent and exit 0")
	fs.StringVar(&g.model, "model", "",
		"Jev model (default from TYPESAFE_DEFAULT_MODEL)")
	fs.DurationVar(&g.timeout, "timeout", 10*time.Second,
		"timeout for each HTTP attempt")
	fs.StringVar(&g.logLevel, "log-level", "warn",
		"stderr diagnostics: debug, info, warn, error")
}

// Run parses args, dispatches the verb, and returns the exit code.
// ctx is the process context (cancelled by SIGINT in main). Run never
// calls os.Exit and reads the environment only through the config
// package.
func Run(
	ctx context.Context,
	args []string,
	stdin io.Reader,
	stdout, stderr io.Writer,
) int {
	if len(args) == 0 {
		writeTopHelp(stderr)

		return reportError(stderr, usagef("no verb given"))
	}

	switch args[0] {
	case "--help", "-h":
		writeTopHelp(stdout)

		return exitOK
	case "--version":
		_, _ = fmt.Fprintln(stdout, Version)

		return exitOK
	}

	cmd := lookup(args[0])
	if cmd == nil {
		writeTopHelp(stderr)

		return reportError(stderr, usagef("unknown verb %q", args[0]))
	}

	if err := runCommand(
		ctx, *cmd, args[1:], stdin, stdout, stderr,
	); err != nil {
		return reportError(stderr, err)
	}

	return exitOK
}

// runCommand builds the verb's flag set, parses its args, and runs
// it.
func runCommand(
	ctx context.Context,
	cmd command,
	args []string,
	stdin io.Reader,
	stdout, stderr io.Writer,
) error {
	var g globals

	fs := flag.NewFlagSet(
		"overseer-judge "+cmd.name, flag.ContinueOnError,
	)
	// Silence flag's own reporting: help goes to stdout on -h, and a
	// parse error's message travels in the error record instead.
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	addGlobals(fs, &g)
	cmd.flags(fs)

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			writeVerbHelp(stdout, cmd, fs)

			return nil
		}
		writeVerbHelp(stderr, cmd, fs)

		return usagef("%s: %v", cmd.name, err)
	}

	level, ok := logLevels[g.logLevel]
	if !ok {
		return usagef("invalid --log-level %q", g.logLevel)
	}

	g.cfg = config.Load()
	g.log = slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{
		Level: level,
	}))
	g.client = func() (*jev.Client, error) {
		key, err := config.Key(keyFile)
		if err != nil {
			return nil, err
		}

		return jev.New(
			&http.Client{Timeout: g.timeout},
			g.cfg.BaseURL, key, g.effectiveModel(),
		), nil
	}

	return cmd.run(ctx, &g, fs.Args(), stdin, stdout)
}

// ── Help ────────────────────────────────────────────────────────────────────

// topHelp is the top-level help text, less the generated verb list.
const topHelp = `overseer-judge <verb> [flags] [args]
overseer-judge --help | --version

Turns overseer judgments into typed JSON verdicts
via TypeSafe's System One (Jev) API.

Verbs:
%s
All flags follow the verb. Every verb accepts these
global flags:
  --pretty              indent JSON output
  --dry-run             print the request, exit 0
  --model <name>        Jev model to ask
  --timeout <duration>  per HTTP attempt (10s)
  --log-level <level>   debug|info|warn|error (warn)
  -h, --help            help for the verb, exit 0

Input:
  --input <path|->      read JSON from a file, or -
                        for stdin.

stdout is data only: one JSON document, compact
unless --pretty. Failures print one JSON object as
the last stderr line.

Exit codes:
  0    success
  1    unexpected failure
  2    usage: bad flags, missing argument, bad input
  3    authentication failed or no API key
  5    request rejected (422 or other 4xx)
  6    rate limited after retries
  7    upstream error (5xx after retries, or an invalid response)
  8    network failure or timeout
  130  interrupted (SIGINT)
`

// writeTopHelp prints the top-level help to w.
func writeTopHelp(w io.Writer) {
	var verbs strings.Builder
	for _, c := range summaries() {
		fmt.Fprintf(&verbs, "  %-8s %s\n", c.name, c.summary)
	}

	_, _ = fmt.Fprintf(w, topHelp, verbs.String())
}

// writeVerbHelp prints one verb's help to w.
func writeVerbHelp(w io.Writer, cmd command, fs *flag.FlagSet) {
	var b strings.Builder

	fmt.Fprintf(&b, "overseer-judge %s %s\n\n", cmd.name, cmd.args)
	fmt.Fprintf(&b, "%s\n\n", cmd.summary)
	fmt.Fprintf(&b, "Output: %s\n\nFlags:\n", cmd.output)

	fs.SetOutput(&b)
	fs.PrintDefaults()
	fs.SetOutput(io.Discard)

	b.WriteString("\nExample:\n")
	for line := range strings.SplitSeq(
		strings.TrimRight(cmd.example, "\n"), "\n",
	) {
		fmt.Fprintf(&b, "  %s\n", line)
	}

	_, _ = io.WriteString(w, b.String())
}
