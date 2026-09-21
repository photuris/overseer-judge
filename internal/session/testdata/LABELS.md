# Session fixture labels

The filename prefix is the expected label. Provenance and the one
fixture that carries no label:

- **Real captures** (Herdr `--format ansi`, byte for byte):
  `idle-03` (Claude Code ghost suggestion), `idle-04`, `idle-06`,
  `unsubmitted-03`, `unsubmitted-05`, `working-03`, `working-05`,
  `idle-08` (opencode 1.18.31); `idle-05`, `idle-07`,
  `unsubmitted-04`, `working-04` (pi 0.86.0). `dialog-01` is a real
  Codex folder-trust dialog, plain text.
- **Synthetic**, modelled on real UIs: everything else. Both
  `degraded` fixtures are synthetic; no real degraded transcript has
  been captured yet.

## ambiguous-01.txt (was unsubmitted-01.txt)

Seeded from a plain-text read of a real Claude Code pane whose
composer showed `go ahead and stub resources/tmux.md`. It was
labelled `unsubmitted`. A later ANSI read of the same pane showed that
text was Claude Code's dim ghost suggestion: the pane was idle. In
plain text the two cases are indistinguishable, so this input has no
knowable label. It scored 0.62–0.72 in every run, the weakest fixture
in the set, and flipped label at ~0.43 once the criteria were
tightened in task 007.

The live test therefore asserts calibration, not a label: for any
`ambiguous-*` fixture the confidence must be below the 0.9 gate. A
confident answer here, in either direction, is the failure.

## working-05.txt and idle-08.txt

Captured 2 s after a prompt and 17.5 s later, just after completion,
in one real opencode session. `working-05` shows only the user's
message and the progress bar `⬝⬝⬝⬝⬝⬝⬝⬝ esc interrupt`; before task
007 it classified as `idle` at 0.96, the first confident wrong answer
found in this project. Found by a fresh live check after task 006,
not by any fixture.
