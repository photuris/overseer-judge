// Command overseer-judge turns overseer judgments into typed JSON
// verdicts via TypeSafe's System One (Jev) API.
package main

import (
	"context"
	"os"
	"os/signal"

	"overseer-judge/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	os.Exit(cli.Run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
