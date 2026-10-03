# Lessons from run fix-1 (bespoke-grc, 2026-10-01)

Written by the overseer of that run for whoever folds these into the
judge. The run used `overseer-judge 0.2.0`; this file was checked
against `0.3.1` (commit `6242207`) on 2026-10-03. The run's own
write-up of the review-layout mismatch is at
`docs/2026-10-01-review-layout-mismatch.md`; its recommendations are
reconciled below so nobody implements them twice.

## Already done in 0.3.0

Both fixes the write-up recommended shipped in `fd6da6f`:

- Per-item `warnings` (`missing_severity`, `missing_status`,
  `unparsed_response`, `text_after_response`), also printed on stderr
  through `tracing`.
- Exit 2 with `no review items` for a file with content and no
  parseable header.

Checked against the run's real files, no network:

```sh
cd ~/Projects/personal/bespoke-grc
overseer-judge review .overseer/review/round-1-122.md --dry-run
```

prints `missing_severity`, `missing_status` and `unparsed_response`
for each of the three items. That file is the one 0.2.0 judged
silently as "no responses". The recommendation to loosen the parser
(option 3 in the write-up) stays declined: the warnings make the
mismatch loud, and a forgiving parser would misread prose.

## 1. `status` is parsed but never output

**What happened.** In the later round files (correct layout) two
items had no `- response:` because the overseer resolved them
directly: one by a ruling ("not a defect", written as an overseer
note under the item), one by opening a follow-up task. The judge
reports both as `"responses": []`, and `resources/judge.md` in the
skill says that means "the implementer did not respond, send it
back". The overseer has to remember which items it settled itself.

**What the code does.** `review.rs` parses `- status:` into
`Item.status` (the `missing_status` warning depends on it) and the
`Typed` output record omits it. The doc comment on `state()` says
`status` is never sent to the model, which is right; it should still
reach stdout.

**Proposal.** Add `status` to `Typed`, omitted when empty, next to
`severity`. Then the skill's rule can read: `"responses": []` with
`status: open` and no warnings means send it back; any other status
means the overseer already settled it. One field, one fixture line.

## 2. The stderr warnings and `2>&1`

**What happened.** My first check piped `--dry-run 2>&1 | jq` and got
`jq: parse error: Invalid numeric literal`, because the `WARN` lines
land in the same stream. Correct behaviour (stdout is data), but the
first thing an overseer tries.

**Proposal.** Nothing in the code. One line in `README.md` under
`--dry-run`, and one in the skill's `resources/judge.md`: pipe stdout
only; read stderr for the warnings. Already proposed on the skill
side in `~/Projects/overseer/LESSONS.md`.

## 3. The overseer note is not a reply

**What happened.** Every round file in the run ends each item, or
the file, with an `## Overseer note` paragraph that rules on the
items. The parser is right to ignore it (the fixture
`round-01` already separates the note from the reply). But an
overseer ruling is the third voice in a round file and has no place
in the layout, so it gets improvised.

**Proposal.** Consider, with the skill's author, a third list line
for the overseer, for example `- ruling: resolved. ...` with the same
two-space continuation rule, parsed into `Item.ruling` and output
but not sent to the model. This is the smallest layout change that
lets the judge and the skill distinguish "nobody answered" from
"the overseer closed it". Item 1 above covers the same need with
no layout change, so do item 1 first and only add this if the
status line turns out not to be enough.

## Not proposed

- Anything in `session` or `tasklint`. Neither verb misbehaved in a
  way the run recorded. I did not keep a comparison of the `tasklint`
  scores against the plan critic's findings, so I cannot say whether
  the lint predicts the critic; that would be worth measuring in a
  future run before trusting the lint to replace anything.
