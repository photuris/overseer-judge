# 001 scaffold
Status: accepted

## Objective
The module builds and `overseer-judge --version` prints the version.
`internal/config` resolves the API key, base URL, and model;
`internal/jev` posts one System One request and decodes the response
into typed answers and typed errors; `internal/cli` parses flags,
dispatches verbs, and maps every failure onto an exit code. The `raw`
verb sends an arbitrary request body through the authenticated
client.

## Files
Allowed: go.mod, cmd/overseer-judge/main.go,
internal/config/config.go, internal/config/config_test.go,
internal/jev/jev.go, internal/jev/errors.go, internal/jev/jev_test.go,
internal/cli/cli.go, internal/cli/raw.go, internal/cli/exit.go,
internal/cli/commands.go, internal/cli/cli_test.go,
internal/cli/testdata/help.txt, README.md, AGENTS.md, CLAUDE.md
Read-only: .overseer/PLAN.md

## Out of scope
The `session`, `task`, and `review` verbs and their packages: they
belong to tasks 002 to 004. An LLM fallback backend. An MCP server. A
`--env` flag; there is no production/test split for a read-only
judgment API. Adding cobra, testify, or go-cmp.

## Spec
`config.Load` returns the base URL and model from the environment,
defaulting to `https://api.typesafe.ai` and `jev-latest`.
`config.Key` reads `TYPESAFE_API_KEY`, else the bare token in
`~/.config/jev`, and returns `ErrNoKey` when both are empty.
`jev.Client.Ask` posts to `/v1/systemone` with a bearer token,
retries 429 and 5xx twice with waits of 500ms then 1s, and returns
one typed error per failure class: `AuthError`, `RequestError`,
`RateLimitError`, `ServerError`, `ResponseError`, `NetworkError`.
`cli.Run` returns the exit code and never calls `os.Exit`.

## Acceptance
Command: go test ./... -count=1
Expect: exit 0; every package line starts with `ok`, none with `FAIL`

Command: go run ./cmd/overseer-judge --version
Expect: exit 0; prints exactly `dev`

Command: TYPESAFE_API_KEY= go run ./cmd/overseer-judge raw --input - < /dev/null; echo "exit=$?"
Expect: prints `exit=2`; the last stderr line is a JSON object whose
`.error.type` is `usage`

## Budget
30 turns. Stop and write Result if exceeded.

## Rules
Record `BASE=$(git rev-parse HEAD)` at step 0. Commit before writing
Result; the subject is one lower-case imperative line. No stubs or
TODO bodies. Stdlib only. Do not touch `.overseer/` except this
file's Result section.

## Result

## Verification
