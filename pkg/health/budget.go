package health

import (
	"context"
	"time"
)

// The run budget model. The runner bounds all check runs of a tick by its run
// budget (40 s, runner.MaxRunTime) and abandons a run still going at its end:
// the run's late result is dropped and it becomes a checker error, never a
// PROBLEM. So that a hung probe is judged Bad (raising a PROBLEM and counting
// towards its fix) rather than abandoned:
//
//   - every configurable per-check timeout is at most MaxCheckTimeout, well
//     inside the run budget, which leaves room for the network check that runs
//     first and for a fix action after a hung probe;
//   - a check bounds each timed step with Bound, so the step also ends RunMargin
//     before the run's own deadline and is judged by the check itself, even
//     when the check started late in the tick.
const (
	// MaxCheckTimeout is the ceiling of every configurable check timeout.
	MaxCheckTimeout = 20 * time.Second
	// RunMargin is how long before its run's deadline a step bounded by Bound
	// ends, so the check can still judge it and return; a run with less than
	// ten times this left keeps a tenth of it instead.
	RunMargin = 2 * time.Second
)

// marginShare is the share of the time left that Bound keeps when that is
// less than RunMargin·marginShare.
const marginShare = 10

// Bound returns ctx limited to timeout and to the end of ctx's deadline less
// a margin (RunMargin, or a tenth of the time left when that is shorter), and
// the limit it applies, at most timeout. A step bounded by it that times out
// ends while ctx is still live, so the check can tell its own timeout
// (ctx.Err() == nil) from the run being cut or cancelled.
func Bound(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc, time.Duration) {
	limit := timeout
	if dl, ok := ctx.Deadline(); ok {
		left := time.Until(dl)
		limit = min(limit, left-min(RunMargin, left/marginShare))
	}
	limit = max(limit, 0)
	bounded, cancel := context.WithTimeout(ctx, limit)

	return bounded, cancel, limit
}
