# overseer-judge: "no implementer responses" on review files

Written by the overseer of run fix-1, 2026-10-01. Tool:
`overseer-judge 0.2.0`, source checked at
`~/Projects/personal/overseer-judge-rs` commit `4767c71`.

## Summary

The judge is not broken. `overseer-judge review` returned
`"responses": []` for all three items of `round-1-122.md`, although
each item had a reply. The cause is a format mismatch: the parser
recognizes one exact layout, and nothing in the overseer skill tells
the reviewer or the implementer to write it. So it will happen on
every round file until the briefs or the skill change. It is
deterministic and local: `--dry-run` shows the same result with no
network call.

## What the parser accepts

From the doc comment on `parse` in `src/review.rs` (lines 130 to 142),
confirmed with dry runs:

```markdown
### R1-01: title

- file: web/src/test/setup.ts
- severity: blocking
- status: open

Body of the finding.

- response: fixed in abc1234. The stub now
  honors the signal.
- response: evidence: see setup.ts:74.
```

- An item starts at `### R<n>-<nn>: title`, three hashes, outside a
  code fence.
- Metadata is a contiguous block of `- file:`, `- severity:` and
  `- status:` list lines right after the header.
- A response starts at a line beginning `- response:`. It continues
  through lines indented two spaces. A blank line continues it only if
  the next line is indented.

## What the agents wrote instead

| Written in this repository | Parsed as |
| --- | --- |
| `Severity: blocking` and `Status: open` on bare lines | body text; `severity` is sent as `""` |
| `Response (fixed, 3dfc1be): ...` (run fix-1) | body text; no response |
| `Implementer response: fixed in <sha>. ...` (run judge-1) | body text; no response |
| `## R1-01` headers, two hashes, no colon (run judge-1) | no items at all; the output is empty |

Other spellings I probed that are also not recognized:
`Response: ...`, `**Response (fixed, sha):** ...`,
`Rebuttal (evidence): ...`, a `#### Response` sub-header,
`- Response (fixed): ...` (capital R), `**Severity:** blocking`,
`- Severity: blocking` (capital S did not parse either), and a
severity in the title.

## Why it is easy to miss

1. A reply that is not recognized is folded into the finding's body
   and sent to the model as part of the finding. The `style_only`
   score is then judged on finding plus reply. Nothing warns.
2. `"responses": []` is the documented signal for "the implementer
   did not respond" (`resources/judge.md`, section 3). A format
   mismatch and a real missing reply look the same.
3. A file with no parseable item prints nothing and exits 0. The
   judge-1 round files (`## R1-01`) do this, so the earlier run very
   likely got empty output from this verb and treated it as nothing
   to act on.

## Evidence

- `.overseer/judge/122-review-responses-present-vs-none.md`: the round
  file as judged.
- `.overseer/judge/122-review-responses-present-vs-none.json`: the
  three output lines, each `"responses":[]`, model `jev-1.13.0`.
- Reproduce without the network:

```bash
overseer-judge review .overseer/review/round-1-122.md --dry-run \
  | jq -c '{id: .body.state.finding.id,
            sev: .body.state.finding.severity,
            n: (.body.state.responses | length)}'
```

  Each line shows `"sev":""` and `"n":0`, and the reply text is inside
  `.body.state.finding.body`.

- A probe in the accepted layout parses correctly: severity
  `blocking`, file set, two responses joined to one line each.

## Options, cheapest first

1. **Document the layout in the skill.** Put the block above into
   `SKILL.md` (Filesystem protocol) and `resources/judge.md`, and
   have the reviewer and implementer briefs quote it. No code change.
2. **Make the mismatch loud.** In `review`, warn on stderr (or add a
   field) when an item has no metadata block, or when its body holds a
   line that looks like a reply (`response`, `rebuttal`, `fixed in`)
   but no `- response:` was parsed. Exit non-zero, or print a
   diagnostic, when a non-empty file yields zero items.
3. **Loosen the parser.** Accept `Severity:` and `Status:` without the
   list dash and in any case, `##` headers, and reply openers such as
   `Response (...)`, `Implementer response:` and `Rebuttal (...)`.
   This is the most forgiving and the most likely to misparse prose.

I would do 1 and 2. Option 3 trades a silent miss for a silent
misread.

## What this run does meanwhile

From the next brief on, the overseer's reviewer and implementer briefs
quote the accepted layout, so the judge can type the replies. Rounds
already written in the other layout (113, 115 and 122 round 1) are
read by the overseer directly, as the skill's fail-open rule says.
