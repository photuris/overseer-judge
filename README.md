# overseer-judge

A CLI that turns overseer judgments into typed JSON verdicts. It sends
each judgment to TypeSafe's System One (Jev) model and prints the
answer as one JSON object, so a shell script or an agent can branch on
it without reading prose.

stdout carries data only. Every failure prints one JSON object as the
last stderr line and exits with a code from the table below.

## Install

### Prebuilt binary

Linux and macOS:

```sh
curl --proto '=https' --tlsv1.2 -LsSf https://github.com/photuris/overseer-judge/releases/latest/download/overseer-judge-installer.sh | sh
```

Windows (PowerShell):

```powershell
powershell -ExecutionPolicy Bypass -c "irm https://github.com/photuris/overseer-judge/releases/latest/download/overseer-judge-installer.ps1 | iex"
```

Both put the binary in `~/.local/bin` (`%USERPROFILE%\.local\bin` on
Windows) and add that directory to `PATH` if needed. If the installer
added it, open a new terminal, or run the reload command the installer
prints, before the next step. Check with `overseer-judge --version`.
Linux and Windows binaries are statically linked. macOS binaries
depend only on the system libraries. Archives named
`overseer-judge-<target>.tar.xz` (`.zip` on Windows), each with a
`.sha256` file, are on the
[releases page](https://github.com/photuris/overseer-judge/releases/latest).

### From source

Building needs Rust 1.98 or later.

```sh
cargo install --git https://github.com/photuris/overseer-judge --locked
```

Or clone and build; the binary lands at
`target/release/overseer-judge`:

```sh
git clone https://github.com/photuris/overseer-judge overseer-judge
cd overseer-judge
cargo build --release --locked
```

## Quickstart

```sh
printf '%s' "$TYPESAFE_API_KEY" > ~/.config/jev   # bare token
overseer-judge --help
printf '{"state":"hi","questions":{"q":{"type":"noul","instructions":"Is this a greeting?"}}}' \
  | overseer-judge raw --dry-run --input -
printf '{"state":"hi","questions":{"q":{"type":"noul","instructions":"Is this a greeting?"}}}' \
  | overseer-judge raw --input - | jq '.answers.q.noul'
```

## Verbs

```
overseer-judge <verb> [flags] [args]
overseer-judge --help | --version

  raw      send an arbitrary Jev request read from --input
  session  classify an agent pane's transcript tail read from --input
  task     lint an overseer task file for spec defects
  review   type each item and response in an overseer review round file
```

All flags follow the verb. Every verb accepts `--pretty`, `--dry-run`,
`--model`, `--timeout`, `--log-level`, and `-h`. `review` is the one
exception: it rejects `--pretty`, because its output is JSON Lines and
a record stays on one line. `--version` prints
`overseer-judge <version>`.

`raw` is the escape hatch. It passes any request body through the
authenticated client, so a new question set needs no release. Run
`overseer-judge <verb> --help` for the flags, the output shape, and one
runnable example.

`--dry-run` prints the request the tool would have sent and exits 0
without a network call. This is how you read the question wording:

```sh
overseer-judge task --dry-run tests/fixtures/tasklint/good-01.md \
  | jq .body.questions
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

tmux capture-pane -p -e -J -S -60 -t <pane> \
  | overseer-judge session --input - --agent pi
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
  "input_line": "",
  "activity_hint": "",
  "model": "jev-latest",
  "usage": {"input_tokens": 1183, "output_tokens": 9}
}
```

`--agent` takes `claude`, `codex`, `pi`, `opencode`, or `unknown` (the
default). It is passed to the model as context, and it picks how the
input box is found.

`input_line` is the text the tool found sitting in the agent's input
box, extracted in code rather than left to the model. Each agent draws
that box differently, so each kind gets its own strategy:

- `claude`, `codex` — the last line starting with `❯` or `›`, minus
  the marker.
- `opencode` — the `┃` box sitting above the `╹` foot, minus the
  status line at its bottom.
- `pi` — the lines between the last two `─` rules.
- `unknown` — marker, then box, then rules. The first structure found
  wins, even when the text inside it is empty.

The result is empty when the box is empty, when it holds a dialog menu
option, or when it holds a placeholder hint such as `Ask anything…`.
It is sent as part of the state and echoed in the verdict, so a caller
can see what the classifier was told.

`activity_hint` is the agent's own busy-indicator line, extracted the
same way: the line carrying `esc to interrupt` or `esc interrupt` for
`claude`, `codex`, and `opencode`; the label on pi's rule, as in
`──  Working ───…`; and for `unknown`, the interrupt line, then the
label. Only the last 12 non-empty lines are searched and the result is
cut to 100 characters. It is empty when nothing on screen says the
agent is busy. This is what keeps a pane that has just started work —
a progress bar and no output yet — out of `idle`.

Act on a verdict only when its `confidence` clears `session::GATE`
(0.9). Below that, escalate to a person or to a reasoning model: no
wrong answer in this project's live runs has ever scored above it.

**Feed it ANSI, not plain text.** Only ANSI input lets the tool drop an
agent's greyed-out prompt suggestion, which Claude Code renders as
faint text on the composer line. In plain text that suggestion is
indistinguishable from input the user typed and has not submitted, so
an idle pane reads as `unsubmitted`. Before sending, the tool removes
faint spans, strips the remaining escapes and spinner glyphs, collapses
blank runs, and caps the tail at the last 200 lines and 12000 bytes.

### `task`

The overseer writes a task file per unit of work, and an implementer
executes it literally. `task` reads one and reports the defects that
would send the implementer guessing.

```sh
overseer-judge task --pretty tests/fixtures/tasklint/good-01.md
```

The path is positional and comes after the flags. `-` reads the file
from stdin.

Six structural checks run in code, always in the same order and always
all six: `status_line`, `sections_present`, `sections_ordered`,
`allowed_nonempty`, `acceptance_command`, `budget_numeric`. They are
fence-aware, so a template quoted inside a fenced code block cannot
satisfy or break a check.

Three judgments come from the model. Two are probabilities in
`judgments`: `needs_interpretation` (the spec leaves design decisions
to the implementer) and `scope_generic` (`Out of scope` names no
concrete adjacent work). The third, `acceptance`, is graded rather
than yes/no, because acceptance quality is: `score` runs from 0, where
the `Expect:` lines check only that a command runs, to 3, where every
one of them names output a wrong implementation would fail. Read it
against a cut, not a coin flip; `tasklint::SOUND_CUT` is 2.2, fitted
to five fixtures and provisional.

```json
{
  "file": "tests/fixtures/tasklint/good-01.md",
  "static": [
    {"check": "status_line", "ok": true},
    {"check": "sections_present", "ok": true},
    {"check": "sections_ordered", "ok": true},
    {"check": "allowed_nonempty", "ok": true},
    {"check": "acceptance_command", "ok": true},
    {"check": "budget_numeric", "ok": true}
  ],
  "judgments": {
    "needs_interpretation": 0.07,
    "scope_generic": 0.04
  },
  "acceptance": {"score": 2.54, "confidence": 0.83},
  "model": "jev-latest",
  "usage": {"input_tokens": 1412, "output_tokens": 12}
}
```

A file missing its `## Objective` or `## Acceptance` section gives the
model nothing to judge, so no request is made: the report carries the
static findings alone, `judgments`, `acceptance`, and `model` are
absent, and
`--dry-run` prints `{"method":"","path":"","body":null,"static":[…]}`.

### `review`

A review round is a markdown file of findings, each with the
implementer's replies under it. `review` reads one and types every
part of it: whether the finding is style only, and what each reply
does about it.

```sh
overseer-judge review tests/fixtures/review/round-01.md |
  jq -c 'select(.style_only > 0.5)'
```

The path is positional and comes after the flags. `-` reads the file
from stdin. Output is JSON Lines, one object per item, written as each
item's answer arrives. `--pretty` is a usage error here.

A file with content and no items is a usage error: exit 2, with a
message that names the expected `### R<n>-<nn>: title` header. Empty
or all-whitespace input prints nothing and exits 0.

This is the layout the parser accepts, taken from the first item of
`tests/fixtures/review/round-01.md` and shortened:

```markdown
### R6-01: Pending re-run can start a batch while the editor is open
- file: src/shell/controller.cpp:1055
- severity: medium
- status: resolved
`rerunPending()` only rejects busy, resolving, and recording states.
- response: fixed in `rerunPending`, which now refuses with
  `close the mapping editor first` before any other check.
```

The parser is strict. A header is `###`, then the id, a colon, and the
title. The metadata lines start with `- ` and use `: ` after the key.
A reply starts with `- response:`, and each continuation line is
indented two spaces.

The parser is fence-aware. A `### R9-99:` header or a `---` line
inside a fenced code block is body text, not a new item, so a finding
that quotes a review round does not split into two. The `- file:`,
`- severity:`, and `- status:` lines may sit a blank line below the
header, as reviewers write them; the block itself is contiguous, and
the first blank line after it starts the body.

`style_only` is a probability: at 1.0 the finding is taste and nothing
the program does would change. Each response gets one of five kinds,
classified by what the reply *does*, not by whether it is right, which
stays the overseer's call:

| Kind       | The reply                                         |
|------------|---------------------------------------------------|
| `fixed`    | claims a change and names where or how            |
| `evidence` | disputes the finding, citing something checkable  |
| `concern`  | disputes it by argument alone                     |
| `question` | asks the reviewer for more information            |
| `agree`    | accepts it without claiming a fix yet             |

```json
{"id":"R6-01","severity":"medium","style_only":0.04,
 "responses":[{"kind":"fixed","confidence":0.91,
 "probabilities":{"fixed":0.91,"evidence":0.06}}],
 "model":"jev-latest","usage":{"input_tokens":880,"output_tokens":7}}
```

When part of an item does not match the layout, the record carries a
`warnings` array that names what did not parse:

| Warning               | Condition                                                                       |
|-----------------------|---------------------------------------------------------------------------------|
| `missing_severity`    | The item has no `- severity:` line.                                             |
| `missing_status`      | The item has no `- status:` line.                                               |
| `unparsed_response`   | A body line outside a fence looks like a reply opener but is not `- response:`. |
| `text_after_response` | A non-blank, unindented line ended a reply and was dropped.                     |

The strings appear in table order, each at most once per item.
`warnings` is the last field of the record, beside `body` in a
`--dry-run` record, and is omitted when empty. Each warning is also
logged once at `warn` level on stderr. Warnings never change the exit
code: an item with warnings is still typed and printed. The JSON
example above is a well-formed item, so it has no `warnings` field.

One request goes out per item, sequentially; the first failure aborts
the run, so a partial round can reach stdout before the error record
reaches stderr.

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

See [AGENTS.md](AGENTS.md) for the layout, the rules, and the test
commands.
