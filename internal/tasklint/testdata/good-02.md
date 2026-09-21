# 007 template-doc
Status: ready

## Objective
`docs/task-template.md` exists and holds the task-file template
verbatim, and `internal/tasklint.TemplateHeadings` returns the eight
required heading names in template order. A test reads
`docs/task-template.md`, extracts its `## ` headings, and asserts
they equal `TemplateHeadings()`, so the document and the checker
cannot drift apart.

## Files
Allowed: docs/task-template.md, internal/tasklint/template.go,
internal/tasklint/template_test.go
Read-only: internal/tasklint/static.go, .overseer/PLAN.md

## Out of scope
Changing the required heading list or the order of the checks in
`Static`. Rendering the template into new task files. Adding a
`template` verb to the CLI. Teaching the parser any heading level
other than `## `.

## Spec
`docs/task-template.md` holds exactly this, and nothing else:

```
# NNN slug
Status: ready

## Objective
## Files
Allowed:
Read-only:

## Out of scope

## Acceptance
Command: false
Expect: replace this line with the real expectation

## Budget

## Rules

## Result

## Verification
```

`TemplateHeadings` returns a fresh slice each call, so a caller that
sorts it cannot corrupt the checker:

```go
// TemplateHeadings returns the required headings in template order.
func TemplateHeadings() []string
```

## Acceptance
Command: go test ./internal/tasklint/ -run Template -count=1 -v
Expect: exit 0; output names `TestTemplateMatchesHeadings` and `PASS`

Command: grep -c '^## ' docs/task-template.md
Expect: exit 0; prints `8`

## Budget
20 turns. Stop and write Result if exceeded.

## Rules
Record `BASE=$(git rev-parse HEAD)` at step 0. Commit before writing
Result. Stdlib only. Do not touch `.overseer/` except this file's
Result section.

## Result

## Verification
