# 012 usage-report
Status: ready

## Objective
`overseer-judge usage --since <date>` exists. It reads the JSONL
ledger at `~/.local/state/overseer-judge/usage.jsonl`, sums
`input_tokens` and `output_tokens` per verb over the records whose
`at` field is on or after `--since`, and prints one JSON object:
`{"since": <date>, "verbs": {<verb>: {"calls": n, "input_tokens": n,
"output_tokens": n}}, "total_tokens": n}`. A missing ledger is not an
error: the verb prints zeroes.

## Files
Allowed: internal/usage/usage.go, internal/usage/usage_test.go,
internal/usage/testdata/ledger.jsonl, internal/cli/usage.go,
internal/cli/commands.go
Read-only: internal/jev/jev.go, internal/cli/cli.go,
internal/cli/exit.go, .overseer/PLAN.md

## Out of scope
Writing the ledger: the verbs that would append to it are a later
task. Rotating or truncating the file. A `--until` flag. Per-model
breakdowns. Printing a human table; stdout stays one JSON object.

## Spec
`usage.Sum(r io.Reader, since time.Time) (Report, error)` decodes one
record per line and skips blank lines. A line that is not valid JSON
is a `*usage.LineError` naming the 1-based line number. `Report` is
the object above, with `Verbs` a `map[string]Totals` so an absent
verb simply does not appear.

## Acceptance
Command: go test ./internal/usage/ -count=1
Expect: the command runs and exits 0

Command: go run ./cmd/overseer-judge usage --since 2026-01-01
Expect: prints something

Command: go run ./cmd/overseer-judge usage --help
Expect: exit 0

## Budget
25 turns. Stop and write Result if exceeded.

## Rules
Record `BASE=$(git rev-parse HEAD)` at step 0. Commit before writing
Result; the subject is one lower-case imperative line. No stubs or
TODO bodies. Stdlib only. Do not touch `.overseer/` except this
file's Result section.

## Result

## Verification
