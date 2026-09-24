Source: internal/deploystate/ (deploystate.go, deploystate_test.go)
Commit: c2423f0
Taken: the one-home rule for "is this deploy still going to happen" — input, answer, exact set
Cut: the status spellings; they become the job states
Cuts belong to: leaf/job (states + these predicates), flow/deploy (reading them)

## The rule

One home because the set was hand-written four times and drifted. Input: one
status string. Output: a bool. No store, no Docker, no clock.

- **live** = `queued`, `running`, `waiting_ci`.
- **terminal** = the exact complement, never its own list — a second list is
  how the copies drifted.
- **cancellable** = `queued` or `waiting_ci` only; a running deploy is
  cancelled through its context before it reaches here.

A parked row counts as live — the bug it fixed: a poll or SSE stream ending
on `waiting_ci` left a dead badge showing for a deploy about to start on its
own. `error` (interrupted or failed) and `cancelled` (a user, or a
supersede) stay two spellings: different events.

```go
func Live(status string) bool {
	switch status {
	case Queued, Running, Waiting:
		return true
	}
	return false
}

func Terminal(status string) bool { return !Live(status) }

func Cancellable(status string) bool { return status == Queued || status == Waiting }
```

## Onto the new design

- No `deployments` table. `waiting_ci` and the plan's `waiting` (unset
  param) are one `waiting`: parked, resumes itself, live; reason on the row.
- `superseded` is new and terminal — outside `Live` for free, out of
  `Cancellable` by hand.
- "Is this tile deploying?" = newest deploy job whose lock set holds the
  tile, through `Live`.
- "What does it run?" = newest deploy job for it with status `done`, and its
  `release_id`; `environments.release_id` is the env-wide answer.

## Notes for the builder

- `leaf/tile.State()` reads Docker first, the row second. The deploy answer
  is neither, and a leaf may not read the jobs table, so it arrives as an
  argument (`deploying bool`). DECIDE: or `State()` returns container truth
  only and the flow layers this on top.
- Keep the complement — every drift came from a second hand-written list.

Size: source 67 lines, extract 56 lines
