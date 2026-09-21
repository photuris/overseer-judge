# Review round 1

Round 1 of task 004 reads the parser and the two fixtures it ships.

---

### R1-01: Fence flag is never reset between files
- file: internal/review/parse.go:61
- severity: medium
- status: resolved
`Parse` keeps its fence flag in a package-level variable, so a file
that ends inside an unclosed fence leaves the next call starting in
fenced mode and every header in the second file is swallowed. Make it
local to the call.
- response: fixed: the flag is a local in `Parse`
  (`internal/review/parse.go:63`), so each call starts outside a
  fence. Test: `TestParseUnclosedFenceDoesNotLeak`, which parses a
  file ending mid-fence and then a normal file.

### R1-02: `p` reads poorly for the parser receiver
- file: internal/review/parse.go:95
- severity: low
- status: open
Every method on the parser takes `p`, which collides in the reader's
head with the `parser` type itself in the one place both appear. This
is taste; the code works.

### R1-03: Metadata lines after the body are silently dropped
- file: internal/review/parse.go:104
- severity: medium
- status: disputed
A `- severity: high` line placed after the first paragraph of the body
is treated as body text, so a round written with the metadata below
the summary loses its severity.
- response: that is the format, not a defect: the spec says metadata
  lines are the ones "immediately after the header", and
  `.overseer/PLAN.md` fixes the same order. The parser test
  `TestParseMetadataStopsAtFirstOtherLine` pins it.
