package cli

import (
	"context"
	"flag"
	"io"
)

// command is one verb.
type command struct {
	name, summary string
	// args is the positional-argument sketch shown in the usage line.
	args string
	// output describes the verb's stdout in one sentence.
	output string
	// example is one runnable invocation.
	example string
	// flags registers the verb's own flags.
	flags func(fs *flag.FlagSet)
	run   func(
		ctx context.Context,
		g *globals,
		args []string,
		stdin io.Reader,
		stdout io.Writer,
	) error
}

// commands is the dispatch table. Each entry is a constructor so a
// verb's flag targets are fresh per invocation. Later tasks append
// one line each.
var commands = []func() command{
	rawCommand, sessionCommand, taskCommand, reviewCommand,
}

// lookup returns the command named name, or nil.
func lookup(name string) *command {
	for _, build := range commands {
		if cmd := build(); cmd.name == name {
			return &cmd
		}
	}

	return nil
}

// summaries returns each verb's name and summary, for help.
func summaries() []command {
	out := make([]command, 0, len(commands))
	for _, build := range commands {
		out = append(out, build())
	}

	return out
}
