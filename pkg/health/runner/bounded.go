package runner

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Toshik1978/tgshd/pkg/health"
)

// ErrContextDone is wrapped by the error of a run abandoned because the
// caller's context ended before the run's own budget; the error also wraps
// the context's cause.
var ErrContextDone = errors.New("run abandoned: context ended")

// RunBounded runs chk once and waits no longer than budget (no limit when
// budget is 0) or until ctx ends. The run's context carries the budget. A
// panic in the check is a failed run. A run that overruns is abandoned: its
// late result is dropped and the error says the budget was exceeded, or wraps
// ErrContextDone when ctx ended first. onDone (may be nil) is called when the
// run actually returns, also after it was abandoned.
func RunBounded(
	ctx context.Context, chk health.Check, budget time.Duration, onDone func(),
) health.CheckResult {
	runCtx, cancel := ctx, context.CancelFunc(func() {})
	if budget > 0 {
		runCtx, cancel = context.WithTimeout(ctx, budget)
	}
	defer cancel()

	done := make(chan health.CheckResult, 1)
	go func() {
		res := runSafely(runCtx, chk)
		if onDone != nil {
			onDone()
		}

		done <- res
	}()

	select {
	case res := <-done:
		return res
	case <-runCtx.Done():
		select {
		case res := <-done:
			return res
		default:
		}

		if ctx.Err() != nil {
			return health.CheckResult{Err: fmt.Errorf("%w: %w", ErrContextDone, context.Cause(ctx))}
		}

		return health.CheckResult{Err: fmt.Errorf("run did not finish within %s", budget)}
	}
}

// runSafely runs a check, turning a panic into a failed run.
func runSafely(ctx context.Context, chk health.Check) (res health.CheckResult) {
	defer func() {
		if p := recover(); p != nil {
			res = health.CheckResult{Err: fmt.Errorf("panic: %v", p)}
		}
	}()

	return chk.Run(ctx)
}
