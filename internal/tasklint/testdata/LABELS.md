# Label rationale

> **Superseded in part, same day.** The yes/no `acceptance_vacuous`
> judgment described below was replaced by the graded Score
> `acceptance_strength` (0 vacuous to 3 fully specific) after
> `good-01.md` flipped across 0.5 on jitter alone (0.47, 0.49, 0.51).
> `expect.json` now carries `acceptance_sound`, true when the score is
> expected to be at least 2.2. Observed scores over two runs:
> vacuous-01 0.32, vague-01 1.70, generic-scope-01 2.49–2.54, good-01
> 2.54–2.62, good-02 2.89–2.91. The 2.2 cut is provisional: it is
> fitted to these five fixtures. The history below is kept because the
> reasoning about coupling still holds: vague-01 is labelled unsound.

`expect.json` holds the expected boolean per judgment at threshold
0.5. Labels are the overseer's, not the model's. One was corrected
after the first live run, on the merits, and the change is recorded
here so the score is not read as better than it is.

## vague-01.md: `acceptance_vacuous` false → true (2026-09-21)

First live run: 14/15 as originally labelled; this was the miss (0.62
against `false`, stable to ±0.02 over three runs).

The original label assumed a fixture could have a vague objective and
an otherwise sound acceptance. It cannot. The objective is "improve
error handling and make the client more robust"; the Expect lines
check that `go test` passes, that at least four tests match
`-run Retry`, and that `go vet` is clean. All three can pass on an
implementation that improves nothing. An acceptance cannot assert the
specific result of an objective that names none, so the two
properties are coupled in fact, not only in the model. The model was
right; the label was wrong.

Consequence for callers: a high `needs_interpretation` drags
`acceptance_vacuous` up. Read them together.

## `acceptance_vacuous` is graded, not binary

Observed: 0.21 (good-02), 0.47–0.49 (good-01), 0.62 (vague-01), 0.92
(vacuous-01). good-01 sits near 0.5 honestly: its first check is
"`go test` passes", which is weak on its own, and its other two are
specific. Treat this judgment as a scale (below ~0.35 sound, ~0.35 to
~0.75 worth a look, above ~0.75 vacuous) rather than a yes/no at 0.5.
Jev's Score primitive is the better fit; that redesign is out of this
POC's scope.

## Known weaknesses of this set

Five fixtures, all written by one implementer to a spec that asked for
one flipped flag each. No file has two independent defects. No real
task file from a past overseer run is included.
