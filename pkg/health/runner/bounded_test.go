package runner

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Toshik1978/tgshd/pkg/health"
)

func TestRunBoundedReturnsTheResult(t *testing.T) {
	t.Parallel()

	var done atomic.Int32
	chk := health.Check{Name: "ok", Run: func(ctx context.Context) health.CheckResult {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("a positive budget must bound the run's context")
		}

		return health.CheckResult{Readings: []health.Reading{{Key: "ok/x", Verdict: health.Good}}}
	}}

	res := RunBounded(t.Context(), chk, time.Second, func() { done.Add(1) })
	if res.Err != nil || len(res.Readings) != 1 {
		t.Fatalf("res = %+v", res)
	}
	if done.Load() != 1 {
		t.Fatalf("onDone ran %d times, want 1", done.Load())
	}
}

func TestRunBoundedWithoutBudgetOrOnDone(t *testing.T) {
	t.Parallel()

	chk := health.Check{Name: "free", Run: func(ctx context.Context) health.CheckResult {
		if _, ok := ctx.Deadline(); ok {
			t.Error("no budget must add no deadline")
		}

		return health.CheckResult{}
	}}
	if res := RunBounded(t.Context(), chk, 0, nil); res.Err != nil {
		t.Fatalf("res = %+v", res)
	}
}

func TestRunBoundedTurnsAPanicIntoAnError(t *testing.T) {
	t.Parallel()

	var done atomic.Int32
	chk := health.Check{Name: "boom", Run: func(context.Context) health.CheckResult { panic("nil map") }}

	res := RunBounded(t.Context(), chk, time.Second, func() { done.Add(1) })
	if res.Err == nil || res.Err.Error() != "panic: nil map" {
		t.Fatalf("err = %v", res.Err)
	}
	if done.Load() != 1 {
		t.Fatalf("onDone ran %d times, want 1", done.Load())
	}
}

func TestRunBoundedAbandonsAnOverrun(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	finished := make(chan struct{})
	chk := health.Check{Name: "hung", Run: func(context.Context) health.CheckResult {
		<-release // Ignores its context.

		return health.CheckResult{Readings: []health.Reading{{Key: "hung/late"}}}
	}}

	res := RunBounded(t.Context(), chk, 20*time.Millisecond, func() { close(finished) })
	if res.Err == nil || res.Err.Error() != "run did not finish within 20ms" || len(res.Readings) != 0 {
		t.Fatalf("res = %+v", res)
	}
	if errors.Is(res.Err, ErrContextDone) {
		t.Fatal("an overrun of the budget is not an ended parent context")
	}
	select {
	case <-finished:
		t.Fatal("onDone must wait for the abandoned run to return")
	default:
	}

	close(release)
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("onDone never ran after the abandoned run returned")
	}
}

func TestRunBoundedReportsAnEndedParentContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	release := make(chan struct{})
	defer close(release)
	chk := health.Check{Name: "slow", Run: func(context.Context) health.CheckResult {
		cancel()
		<-release

		return health.CheckResult{}
	}}

	res := RunBounded(ctx, chk, time.Minute, nil)
	if !errors.Is(res.Err, ErrContextDone) || !errors.Is(res.Err, context.Canceled) {
		t.Fatalf("err = %v, want ErrContextDone wrapping context.Canceled", res.Err)
	}
}
