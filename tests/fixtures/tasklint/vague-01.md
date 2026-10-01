# 011 client-hardening
Status: ready

## Objective
Make the client more robust and improve error handling across the
package. The retry behaviour should be cleaned up so that transient
upstream problems are handled properly instead of surfacing to the
caller, and the diagnostics should be more useful when something goes
wrong. Make sure the package behaves sensibly under load and that timeouts are
dealt with in a reasonable way.

## Files
Allowed: internal/jev/jev.go, internal/jev/errors.go,
internal/jev/jev_test.go
Read-only: internal/cli/exit.go, internal/config/config.go,
.overseer/PLAN.md

## Out of scope
Changing the exit-code map in `internal/cli/exit.go`; the CLI's
contract with callers stays as it is. Adding a circuit breaker or a
request cache. Introducing `golang.org/x/time/rate` or any other
dependency. Touching the `session` verb's question wording.

## Acceptance
Command: go test ./internal/jev/ -count=1
Expect: exit 0; the package line starts with `ok`, none say `FAIL`

Command: go test ./internal/jev/ -run Retry -count=1 -v 2>&1 | grep -c '^=== RUN'
Expect: exit 0; prints a count of at least `4`

Command: go vet ./internal/jev/
Expect: exit 0; prints nothing

## Budget
25 turns. Stop and write Result if exceeded.

## Rules
Record `BASE=$(git rev-parse HEAD)` at step 0. Commit before writing
Result; the subject is one lower-case imperative line. No stubs or
TODO bodies. Do not edit or skip tests to make Acceptance pass;
report instead. Stdlib only. Do not touch `.overseer/` except this
file's Result section.

## Result

## Verification
