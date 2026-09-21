# Review round 6

Round 6 covers the batch-control work in task 011: the pending-run
entry points, the scrub worker's cancellation path, and the mapping
editor's interaction with both. Findings are ordered by file.

### R6-01: Pending re-run can start a batch while the mapping editor is open
- file: src/shell/controller.cpp:1055
- severity: medium
- status: resolved
`rerunPending()` only rejects busy, resolving, and recording states. The main window remains usable beside the editor window, so a user can open a pending-transcript popup and press “Re-run” while `editorOpen` is true; this starts scrubbing and leaves the editor able to write mappings during that batch. Make this entry point reject `m_editorOpen` with the same “close the mapping editor first” status, and cover it alongside the drop and recording guards.
- response: fixed in `rerunPending`, which now refuses with
  `close the mapping editor first` before any other check, and in
  `deletePending`, which had no guards at all and is the other way into
  the same working directories (the task spec names both). Test:
  `rerun_pending_refused_while_editor_open`, which also checks the run
  list is untouched and that both calls work again after
  `closeEditor()`.

### R6-02: `n` is a poor name for the pending-batch counter
- file: src/shell/controller.cpp:1180
- severity: low
- status: open
The counter holding how many pending transcripts a batch has left to
scrub is called `n`, and the two loops beside it use `i` and `k`, so
the block reads as arithmetic rather than as batch bookkeeping.
`remaining` would say what it holds. Nothing the program does changes
either way; the task's acceptance does not mention naming.
- response: agreed, will rename it in the next round.

### R6-03: Scrub worker writes mappings after the batch is cancelled
- file: src/shell/scrub.cpp:212
- severity: high
- status: disputed
`cancelBatch()` sets `m_cancelled` and returns. The worker thread
tests that flag between transcripts but not between the mapping
writes inside one transcript, so a cancelled batch can still write
every remaining mapping of the transcript it was on:

```cpp
for (const auto &m : mappings) {
    // ### R9-99: not an item
    // ---
    writeMapping(m);
}
```

The inner loop needs the same check the outer one has, or cancellation
does not mean what the status line says it means.
- response: the loop already exits: `writeMapping` returns false once
  `m_cancelled` is set, and the caller breaks on the first false
  (`src/shell/scrub.cpp:198`). This is covered by
  `scrub_cancel_stops_mid_transcript` in
  `tests/shell/scrub_test.cpp`, which cancels between two mappings of
  one transcript and asserts only the first landed on disk.

### R6-04: Batch progress counts skipped transcripts as done
- file: src/shell/progress.cpp:74
- severity: medium
- status: disputed
`advance()` increments the completed count for every transcript the
worker finishes with, including the ones it skipped because their
audio was missing. The progress bar therefore reaches 100% on a batch
where nothing was scrubbed, and the summary line reports a count that
does not match the run list. Count skips separately and report them.
- response: I don't think this matters in practice. Anyone running a
  batch is watching the run list, which already shows the skips, and a
  second counter is more UI for a case that almost never comes up.

### R6-05: Retry backoff is not bounded
- file: src/shell/scrub.cpp:301
- severity: medium
- status: open
The retry wait doubles on every failure with no ceiling, so a
transcript that keeps failing pushes the wait past any useful bound
and the batch appears to hang. Cap it.
- response: what should the cap be? The task spec names no number, and
  the existing config has nothing I can read a ceiling from.

### R6-06: Mapping editor writes to the batch working directory
- file: src/shell/editor.cpp:412
- severity: high
- status: resolved
`saveMappings()` writes into the directory the running batch is
scrubbing, so an editor save during a batch can replace a file the
worker is about to read. Write to a staging path and move it into
place once the batch is idle.
- response: the editor cannot be open during a batch, so the two never
  overlap and staging would be dead code.
- response: fixed after R6-01 showed the guard was missing on two
  entry points: `saveMappings` now writes `mappings.staged` and moves
  it in `onBatchIdle`, in `src/shell/editor.cpp:430`. Test:
  `editor_save_during_batch_stages`.
overseer note: this item stays resolved; the first response was
withdrawn by the implementer.
