# Review round 1

Scope: task 001, commits `636b3a3..188c8ae`. Task spec:
`.overseer/tasks/001-scaffold.md` (read its Result "Deviations" and
the overseer's Verification note, which accepts deviations 1–5).
Plan: `.overseer/PLAN.md`.

Reviewer: read the diff (`git diff 636b3a3 188c8ae`) against the task
spec and the plan. Do not edit code. You may run `go test ./...` and
`go vet ./...`. Report gaps that affect correctness, the stated
acceptance criteria, or scope (changes outside `Allowed`). Focus, in
order:

1. `internal/jev`: retry logic (attempt count, which statuses, waits,
   `Retry-After`, ctx cancellation during sleep, response body always
   closed and drained), error typing, and the `Noul`/`Choice`
   accessors' failure cases.
2. `internal/cli`: exit-code map vs PLAN.md, the error record as the
   *last* stderr line, stdout purity (nothing but data), dry-run never
   touching the key or network, `raw` input validation (null,
   trailing values, missing keys, unknown keys preserved).
3. Secrets: the API key must never reach stdout, stderr, logs at any
   level, or the dry-run output.
4. Whether `commands.go`'s shape lets tasks 002–004 add a verb by
   appending one line plus their own file, without editing `cli.go`.
5. Test gaps against the task spec's test list.

Do not report style preferences. Do not report the five deviations
already accepted unless one causes a correctness defect.

## Findings

Format per item:

```
### R1-NN: <one-line title>
- file: <path>:<line>
- severity: blocking | minor
- status: open
<what is wrong, and what correct looks like>
```

(Reviewer writes items below. The implementer appends a
`- response:` line under an item, typed `evidence` or `concern` when
rebutting. The overseer sets the final `status`. End the file with a
line `Items: <count>`.)

Reviewed `636b3a3..188c8ae` against task 001 and the plan, including the
accepted deviations. All 20 changed files are within `Allowed`. The
dispatch table supports later verbs without changes to `cli.go`.
`go test ./... -count=1` and `go vet ./...` pass. `gofmt -l .` is empty.
The reviewed code matches the current checkout. Local mock-server and
binary checks reproduced the failures below. No live API call was made.
No code was edited.

### R1-01: Body-read timeouts and interrupts lose their error types

- file: internal/jev/jev.go:266
- severity: blocking
- status: resolved

Every error from decoding a 200 response becomes a `ResponseError`,
with the original error reduced to text. A server that sends headers
and a partial JSON body, then stalls, makes `--timeout 50ms` exit 7
with type `response`. SIGINT during the same read also exits 7.
These must exit 8 (`network`) and 130 (`interrupted`), respectively.
Preserve transport and context errors as `NetworkError` with the
original cause. Reserve `ResponseError` for invalid response content.
Add tests that interrupt or time out while reading the response body.
Existing tests cover a closed server and injected sleep cancellation,
so they do not exercise this failure.

- response: fixed. `decodeResponse` now returns a read failure
  unwrapped, and the new `Client.bodyError` classifies it: if the root
  ctx is done it returns `NetworkError{ctx.Err()}` (exit 130 on
  SIGINT), else `isReadFailure` treats `context.Canceled`,
  `context.DeadlineExceeded`, and any `net.Error` (which covers both
  timeouts and transport errors) as `NetworkError` (exit 8). Only
  content faults stay `ResponseError`. Tests:
  `TestBodyReadFailuresKeepTheirType` in `internal/jev/jev_test.go`
  drives a stub transport whose body stalls mid-JSON and then returns
  a `net.Error` timeout, `context.DeadlineExceeded`, or a reset with
  the ctx cancelled; each asserts `NetworkError` and, for the latter
  two, `errors.Is` on the wrapped cause. Judgment call worth flagging:
  a truncated body that yields `io.ErrUnexpectedEOF` with no net or
  ctx error is classified as `ResponseError`, not `NetworkError` —
  amendment 2 enumerates transport/timeout/ctx causes, and that error
  is indistinguishable from a genuinely truncated document.

### R1-02: Invalid response documents are reported as successful judgments

- file: internal/jev/jev.go:264
- severity: blocking
- status: resolved

A single successful `Decode` does not establish the response contract.
Mock 200 bodies of `null` and `{}` both produce exit 0 and an output
object with `answers:null`. A valid response followed by ` {}` also
exits 0, silently discarding the second document. `raw` never calls the
answer accessors, so their checks do not catch these cases. Require one
complete response object with the required response structure, and
require EOF after it. Invalid responses must return `ResponseError`
and exit 7 without success output. Add cases for null, missing response
fields, and trailing data beside the existing `not json` test.

- response: fixed. `decodeResponse` replaces the single
  `Decode`. It decodes one `json.RawMessage`, requires EOF after it
  (`requireEOF`), then requires the value to be a JSON object with an
  `answers` member that unmarshals to a non-nil map. `null`, `{}`,
  `{"answers":null}`, `{"answers":[]}`, `[1,2]`, a trailing document,
  and `not json` all return `*ResponseError` with no response value.
  Tests: `TestResponseValidity` (9 cases, `internal/jev`) and
  `TestRawRejectsInvalidResponses` (`internal/cli`) which asserts exit
  7, type `response`, and empty stdout end to end. Verified the CLI
  test fails against pre-fix `jev.go`: `null`, `empty_object`,
  `null_answers`, and `trailing_document` all failed before the
  change.

### R1-03: Response bodies are closed without being fully drained

- file: internal/jev/jev.go:262
- severity: minor
- status: resolved

The deferred cleanup only closes the body. `readErrBody` consumes at
most 500 bytes, leaving larger error bodies unread. The success path
also stops after one decoded value. This violates the brief's drain
requirement and can prevent HTTP connection reuse across retries.
Keep the accepted 500-byte diagnostic limit, but drain the remaining
body before closing, within the attempt's timeout and cancellation
boundaries. Add a tracking response-body test that checks both EOF
consumption and Close on success, decode failure, and retry paths.
The missing drain is confirmed by source inspection. A local HTTP/1.1
server with 64 KiB error bodies also observed new connections across
the three attempts.

- response: fixed. `drainClose` replaces the close-only
  deferred cleanup on every path:
  `io.Copy(io.Discard, io.LimitReader(body, 1<<16))` then `Close`.
  `drainLimit` is a named constant beside `maxErrBody`; the 500-byte
  diagnostic cap is unchanged. Test:
  `TestBodiesAreDrainedAndClosed` uses a `trackingBody` that records
  EOF consumption and `Close`, over the success, decode-failure, and
  retry (500 then 200) paths, asserting both flags on every body the
  client consumed. The stub transport is how the body gets injected —
  `httptest` gives no handle on it.

### R1-04: Top-level usage failures omit the final JSON error record

- file: internal/cli/cli.go:100
- severity: blocking
- status: resolved

No arguments and an unknown verb return `exitUsage` directly after
printing help. Both exit 2, but the last stderr line is
`130  interrupted (SIGINT)`, not a JSON error record. This breaks the
documented machine-readable failure contract. After printing help,
route both branches through `reportError` with a `usageError`.
Extend `TestNoArgsAndUnknownVerb` to assert the final record's `usage`
type, as the other CLI failure tests already do.

- response: fixed. Both branches in `Run` now print
  top-level help to stderr and then return
  `reportError(stderr, usagef(...))`, so the JSON record is the final
  stderr line and the code stays 2. Tests:
  `TestNoArgsAndUnknownVerb` gained an `errType` assertion, and the
  new `TestEveryFailureEndsWithOneErrorRecord` asserts the
  file-wide invariant across no-args, unknown verb, bad input, no key,
  and a 401 — exactly one parseable error record in stderr, and it is
  the last line. Verified both fail against pre-fix `cli.go`.

### R1-05: The raw parser accepts malformed trailing delimiters

- file: internal/cli/raw.go:116
- severity: blocking
- status: resolved

`Decoder.More()` is not an EOF check. A valid request followed by `]`,
`}`, or `] {}` passes validation and exits 0 under `--dry-run`. A live
invocation would send the truncated request. Although task 001 names
`More()`, this contradicts its requirement for exactly one JSON object.
Perform a second decode and require `io.EOF`, rejecting every other
result as a usage error. Add these suffixes to the input tests. The
existing `{} trailing` case does not expose this defect.

- response: fixed, per amendment 6. `rawBody` now decodes a
  second `json.RawMessage` and requires `io.EOF`; every other result,
  including a syntax error, is a usage error. Tests: `TestRawUsageErrors`
  gained `]`, `}`, `] {}`, and two-objects cases. Verified the first
  three fail against pre-fix `raw.go` (the two-objects case passed
  before, since `More()` does catch a buffered second object — that is
  why it was not enough).

### R1-06: Echoed credentials reach stderr through error response bodies

- file: internal/jev/jev.go:275
- severity: blocking
- status: resolved

Error bodies flow unchanged into `RequestError` or `ServerError`, then
into the CLI's final stderr record. A local server returning HTTP 422
with the received Authorization header as its body reproduced this:
the stderr message contained `Bearer review-fake-token`, the synthetic
API key used for the check. An upstream diagnostic that echoes a
credential therefore violates the brief's explicit secrecy requirement.
Remove the configured key from externally supplied diagnostics before
they can reach errors or output. Preserve the accepted body truncation
behavior. Add fake-key echo tests for request and server errors,
including debug logging, and assert the key is absent from both streams.

- response: fixed. `Client.redact` replaces every occurrence
  of the configured key with `[redacted]`, and is applied to
  `readErrBody` output before it reaches `RequestError` or the
  `retryable` that becomes `ServerError`, and to `ResponseError`
  messages built from body text. It is a no-op when the key is empty.
  Truncation is unchanged. Tests: `TestEchoedKeyIsRedacted` (422 and
  500, the latter with one body per retry attempt),
  `TestEchoedKeyIsRedactedInResponseErrors`,
  `TestRedactSkipsEmptyKey`, and end to end
  `TestEchoedKeyNeverReachesAnyStream`, which runs with
  `--log-level debug` against a server echoing the Authorization
  header and asserts the fake key is in neither stdout nor stderr and
  that `[redacted]` is present. Verified both end-to-end cases fail
  against pre-fix `jev.go`. Note the scope limit: this redacts the
  configured key only. A body echoing some *other* secret is still
  passed through truncated.


- response (round 2, R1-06 residual): fixed, per amendment 8. The
  client-side redaction is kept, and a second redactor now sits at the
  CLI choke point, which catches upstream text that never passes
  through a response body. `globals` gained an `apiKey` field that
  `client()` sets once `config.Key` resolves it, plus a `redact`
  method. `reportError` redacts the final message string before
  encoding, so the error's type and wrapped cause survive untouched --
  the malformed `Content-Length` case still exits 8 with type
  `network`. The `slog` handler is built with
  `ReplaceAttr: redactAttr(g.redact)`, which rewrites every
  string-valued attribute and the built-in message; because `redact`
  is a method on the same `globals` the closure holds, it sees the key
  even though the logger is built before the key is resolved. Tests:
  `TestMalformedContentLengthDoesNotLeakKey` serves the reviewer's
  `Content-Length: round2-fake-token` response from a raw socket and
  asserts exit 8, type `network`, the key absent from both streams at
  `--log-level debug`, and `[redacted]` present; `TestRedactAttr`
  covers the attribute, message, and non-string cases;
  `TestRedactBeforeKeyIsResolved` covers the pre-resolution no-op.
  Verified the end-to-end test fails against `79ba9dd`, leaking
  `bad Content-Length "round2-fake-token"` into stderr. Residual scope
  limit, unchanged: only the configured key is redacted, not any other
  secret an upstream body might echo.

## Overseer notes (before implementer responses)

All six items are accepted as real; none is a style preference. Three
trace to holes in the overseer's spec, not to implementer error, and
the task file has been amended first (see `## Spec amendments` in
`.overseer/tasks/001-scaffold.md`):

- R1-05: the spec named `dec.More()` as the trailing-data check. Wrong
  tool; amended to a second `Decode` that must return `io.EOF`.
- R1-04: the spec said "help to stderr, exit 2" for no-args/unknown
  verb without restating that the JSON error record is still the last
  stderr line. Amended.
- R1-02: the spec never defined a structurally valid 200 body.
  Amended: one JSON object, non-null `answers` object, then EOF.

R1-01, R1-03, R1-06 are implementation gaps against stated
requirements (exit-code map; reviewer brief's drain and secrecy
points). Implementer: fix all six under the amended spec, append a
`- response:` line under each item naming the change and its test,
commit, and add a "Round 1 fixes" subsection to the task's Result with
the new commit sha. Allowed files are unchanged from task 001.

Items: 6
