# overseer-judge

A CLI that turns overseer judgments into typed JSON verdicts. It sends
each judgment to TypeSafe's System One (Jev) model and prints the
answer as one JSON object, so a shell script or an agent can branch on
it without reading prose.

stdout carries data only. Every failure prints one JSON object as the
last stderr line and exits with a code from the table below.

## Quickstart

```sh
go build -o overseer-judge ./cmd/overseer-judge
printf '%s' "$TYPESAFE_API_KEY" > ~/.config/jev   # bare token
./overseer-judge --help
printf '{"state":"hi","questions":{"q":{"type":"noul","instructions":"Is this a greeting?"}}}' \
  | ./overseer-judge raw --dry-run --input -
printf '{"state":"hi","questions":{"q":{"type":"noul","instructions":"Is this a greeting?"}}}' \
  | ./overseer-judge raw --input - | jq '.answers.q.noul'
```

## Verbs

```
overseer-judge <verb> [flags] [args]
overseer-judge --help | --version

  raw      send an arbitrary Jev request read from --input
  session  classify an agent pane's transcript tail read from --input
```

All flags follow the verb. Every verb accepts `--pretty`, `--dry-run`,
`--model`, `--timeout`, `--log-level`, and `-h`.

`raw` is the escape hatch. It passes any request body through the
authenticated client, so a new question set needs no release. Run
`overseer-judge <verb> --help` for the flags, the output shape, and one
runnable example.

`--dry-run` prints the request the tool would have sent and exits 0
without a network call. This is how you read the question wording:

```sh
overseer-judge raw --dry-run --input req.json | jq .body.questions
```

### `session`

`session` reads the tail of a coding agent's terminal pane and says
what the pane is doing: `working`, `idle`, `dialog`, `unsubmitted`,
`error`, or `degraded`. The verdict carries the label, its confidence,
the full probability distribution, and a separate `coherent`
probability that a watcher can threshold on its own.

```sh
herdr agent read <pane> --lines 60 --source recent-unwrapped \
  --format ansi | overseer-judge session --input - --agent claude
```

stdout is one compact JSON object; `--pretty` indents it:

```json
{
  "state": "idle",
  "confidence": 0.88,
  "probabilities": {
    "idle": 0.88,
    "unsubmitted": 0.07,
    "working": 0.03,
    "dialog": 0.01,
    "error": 0.01,
    "degraded": 0.0
  },
  "coherent": 0.97,
  "model": "jev-latest",
  "usage": {"input_tokens": 1183, "output_tokens": 9}
}
```

`--agent` takes `claude`, `codex`, or `unknown` (the default) and is
passed to the model as context.

**Feed it ANSI, not plain text.** Only ANSI input lets the tool drop an
agent's greyed-out prompt suggestion, which Claude Code renders as
faint text on the composer line. In plain text that suggestion is
indistinguishable from input the user typed and has not submitted, so
an idle pane reads as `unsubmitted`. Before sending, the tool removes
faint spans, strips the remaining escapes and spinner glyphs, collapses
blank runs, and caps the tail at the last 200 lines and 12000 bytes.

## Configuration

Precedence: flag, then process environment, then `~/.config/jev`.

| Setting  | Env var                  | File / flag                                                           | Default                   |
|----------|--------------------------|-----------------------------------------------------------------------|---------------------------|
| API key  | `TYPESAFE_API_KEY`       | `~/.config/jev` (bare token, trimmed; `$XDG_CONFIG_HOME/jev` honored) | required                  |
| Base URL | `TYPESAFE_BASE_URL`      |                                                                       | `https://api.typesafe.ai` |
| Model    | `TYPESAFE_DEFAULT_MODEL` | `--model`                                                             | `jev-latest`              |

No flag carries the key. Argv is visible in the process list and in
shell history.

There is no `--env` flag. A read-only judgment API has no
production/test split, so this deviates from the standard CLI flag set
on purpose.

A missing key is fatal before any request: exit 3, with both the
variable name and the file path in the error message.

## Exit codes

```
0    success
1    unexpected failure
2    usage: bad flags, missing argument, bad input
3    authentication failed or no API key
5    request rejected (422 or other 4xx)
6    rate limited after retries
7    upstream error (5xx after retries, or an invalid response)
8    network failure or timeout
130  interrupted (SIGINT)
```

Codes 4 and 9 are unused. The tool has no resources to not-find and no
bulk writes.

The client retries a 429 and any 5xx: three attempts in total, with
waits of 500ms and then 1s. An integer `Retry-After` header, in
seconds, replaces the wait. `--timeout` bounds each attempt, not the
whole command. SIGINT cancels the command, including a wait between
attempts, and exits 130.

## Pipelines

The same pipeline in bash:

```sh
if printf '%s' "$req" | overseer-judge raw --input - \
     | jq -e '.answers.q.noul > 0.5' > /dev/null; then
  echo yes
fi
```

And in PowerShell. Check `$LASTEXITCODE`, not `$?`:

```powershell
$req | overseer-judge raw --input - | ConvertFrom-Json |
  ForEach-Object { $_.answers.q.noul }
if ($LASTEXITCODE -ne 0) { throw "judge failed: $LASTEXITCODE" }
```

## Development

See [AGENTS.md](AGENTS.md) for the layout, the test commands, and the
standard-library-only rule. `.overseer/` holds the run ledger for the
overseer workflow that builds this tool.
