# 013 key-file-permissions
Status: ready

## Objective
`config.Key` refuses a key file that is group- or world-readable.
When `~/.config/jev` exists and its mode has any bit outside `0600`
set, `Key` returns a `*config.PermissionError` naming the path and
the octal mode it found, and reads no bytes from the file. The
environment variable path is unaffected: `TYPESAFE_API_KEY` is used
whenever it is non-empty, whatever the file's mode.

## Files
Allowed: internal/config/config.go, internal/config/config_test.go
Read-only: internal/cli/exit.go, internal/cli/cli.go,
.overseer/PLAN.md

## Out of scope
Anything not listed under Allowed. Anything else is off limits.

## Spec
```go
// PermissionError reports a key file other users can read.
type PermissionError struct {
	Path string
	Mode fs.FileMode
}

// Error implements error.
func (e *PermissionError) Error() string
```

`Error` reads `key file %s is mode %04o, want 0600`. `Key` checks the
mode with `os.Stat` before opening the file. `classify` in
`internal/cli/exit.go` already routes an unrecognised error to exit
1; a later task maps this one to exit 3.

## Acceptance
Command: go test ./internal/config/ -run Permission -count=1 -v
Expect: exit 0; output names `TestKeyRejectsGroupReadableFile` and
ends with `PASS`

Command: install -m 0644 /dev/null /tmp/jev-loose && TYPESAFE_API_KEY= XDG_CONFIG_HOME=/tmp go run ./cmd/overseer-judge raw --input - < /dev/null; echo "exit=$?"
Expect: prints `exit=2`; stderr's last line is a JSON object

## Budget
20 turns. Stop and write Result if exceeded.

## Rules
Record `BASE=$(git rev-parse HEAD)` at step 0. Commit before writing
Result; the subject is one lower-case imperative line. No stubs or
TODO bodies. Stdlib only. Do not touch `.overseer/` except this
file's Result section.

## Result

## Verification
