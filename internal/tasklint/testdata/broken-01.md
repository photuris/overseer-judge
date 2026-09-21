# 014 retry-metrics
Status: ready

## Objective
Every retried request is counted, and the counts are readable from
the client. `jev.Client.Stats()` returns a `Stats` value holding
`Attempts`, `Retries`, and `RateLimited`, each an `int`, summed over
the client's lifetime. The counters are incremented inside `Ask`, so
a caller that shares one client across goroutines sees a consistent
total.

## Out of scope
Exporting the counters as Prometheus metrics. Adding a `--stats`
flag to any verb. Counting tokens; `Usage` already carries those.
Persisting the counters across processes.

## Files
Allowed: internal/jev/jev.go, internal/jev/stats.go,
internal/jev/jev_test.go
Read-only: internal/cli/cli.go, .overseer/PLAN.md

## Spec
```go
// Stats counts what one client has done since New returned it.
type Stats struct {
	Attempts    int
	Retries     int
	RateLimited int
}

// Stats returns a snapshot of the client's counters.
func (c *Client) Stats() Stats
```

The counters are guarded by a `sync.Mutex` on `Client`. `Attempts`
counts every HTTP attempt including the first; `Retries` counts the
ones that followed a retryable failure; `RateLimited` counts the 429s
seen, whether or not the retry then succeeded.

## Budget
20 turns. Stop and write Result if exceeded.

## Result

## Verification
