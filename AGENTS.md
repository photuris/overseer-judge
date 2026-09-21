# overseer-judge — agent instructions

A Go CLI that turns overseer judgments into typed JSON verdicts. It
asks TypeSafe's System One (Jev) model, so the overseer skill can move
those judgments off an expensive reasoning model.

## Layout

```
cmd/overseer-judge/main.go   signal context, os.Exit(cli.Run(...))
internal/cli/                Run, global flags, verb dispatch, help,
                             exit codes
internal/config/             base URL, model, API key
internal/jev/                HTTP client, typed request/response,
                             typed errors, retries
internal/session/            verb: transcript-tail classification
internal/tasklint/           verb: task-file lint
internal/review/             verb: review-item typing
```

Tests sit beside the code as `*_test.go`. Fixtures live in
`internal/<pkg>/testdata/`.

## Rules

- **Standard library only.** No cobra, no godotenv, no testify, no
  go-cmp. Four verbs fit inside `flag`.
- No `pkg/` directory. `internal/` is the library.
- Format with `gofmt`. Wrap at 79 columns, counting each tab as 2.
  `.golangci.yml` enforces this through `lll`.
- stdout carries data only. Diagnostics go to stderr through `slog`.
- Every package, type, and function gets a doc comment.

## Question wording

Each judgment's exact question text lives in the package that sends
it: `internal/session`, `internal/tasklint`, `internal/review`. The
wording is versioned with the code that sends it, so read the package
when you need the current text.

Use `--dry-run` to read a request without sending it:

```sh
overseer-judge session --dry-run --input tail.txt | jq .body.questions
```

## Tests

```sh
gofmt -l .
go vet ./...
go test ./... -count=1
```

The live tests call the real API and record the measurement this POC
exists to produce. They need a key and they cost tokens:

```sh
go test -tags live -count=1 -v \
  ./internal/session/ ./internal/tasklint/ ./internal/review/
```

A live test skips only when `config.Key` returns `config.ErrNoKey`.
Any other configuration failure fails the test.

## Credentials

The key comes from `TYPESAFE_API_KEY`, else from `~/.config/jev`
(`$XDG_CONFIG_HOME/jev` when that variable is set). The file holds the
bare token. No flag carries the key: argv is visible in the process
list.

## `.overseer/`

`.overseer/` is the local run ledger for the overseer workflow:
`PLAN.md`, `STATE.md`, the task files, and the review rounds. Git
ignores it, so a fresh clone has none. When one exists, do not edit it
except the `Result` section of the task you were given.
