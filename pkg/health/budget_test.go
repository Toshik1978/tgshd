package health_test

import (
	"context"
	"testing"
	"time"

	"github.com/Toshik1978/tgshd/pkg/health"
)

func TestBoundWithoutDeadlineUsesTheTimeout(t *testing.T) {
	t.Parallel()

	ctx, cancel, limit := health.Bound(context.Background(), time.Minute)
	defer cancel()

	if limit != time.Minute {
		t.Fatalf("limit = %s, want 1m", limit)
	}
	dl, ok := ctx.Deadline()
	if !ok || time.Until(dl) > time.Minute || time.Until(dl) < 59*time.Second {
		t.Fatalf("deadline = %v, %v", dl, ok)
	}
}

func TestBoundKeepsAShortTimeout(t *testing.T) {
	t.Parallel()

	parent, stop := context.WithTimeout(context.Background(), time.Hour)
	defer stop()

	_, cancel, limit := health.Bound(parent, time.Second)
	defer cancel()

	if limit != time.Second {
		t.Fatalf("limit = %s, want 1s", limit)
	}
}

func TestBoundEndsBeforeTheCallersDeadline(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		remaining time.Duration
		margin    time.Duration // the margin Bound must leave at least
		wait      bool
	}{
		// A long run keeps the full RunMargin.
		{"long run", 40 * time.Second, health.RunMargin, false},
		// A short run keeps a tenth of what is left.
		{"short run", 200 * time.Millisecond, 20 * time.Millisecond, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			parent, stop := context.WithTimeout(context.Background(), tt.remaining)
			defer stop()
			parentDL, _ := parent.Deadline()

			ctx, cancel, limit := health.Bound(parent, time.Hour)
			defer cancel()

			dl, ok := ctx.Deadline()
			if !ok {
				t.Fatal("no deadline")
			}
			if got := parentDL.Sub(dl); got < tt.margin-time.Millisecond {
				t.Fatalf("bound ends %s before the caller's deadline, want at least %s", got, tt.margin)
			}
			if limit >= tt.remaining || limit <= 0 {
				t.Fatalf("limit = %s, want within (0, %s)", limit, tt.remaining)
			}
			if !tt.wait {
				return
			}
			<-ctx.Done()
			if parent.Err() != nil {
				t.Fatal("the bound ended after the caller's context")
			}
		})
	}
}

func TestBoundPastTheDeadlineIsDone(t *testing.T) {
	t.Parallel()

	parent, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer stop()

	ctx, cancel, _ := health.Bound(parent, time.Minute)
	defer cancel()

	if ctx.Err() == nil {
		t.Fatal("a bound past the deadline must be done")
	}
}

func TestMaxCheckTimeoutLeavesRoomForTheMargin(t *testing.T) {
	t.Parallel()

	if health.MaxCheckTimeout <= 0 || health.RunMargin <= 0 ||
		health.MaxCheckTimeout+health.RunMargin >= 40*time.Second {
		t.Fatalf("MaxCheckTimeout %s and RunMargin %s must fit a 40 s run", health.MaxCheckTimeout, health.RunMargin)
	}
}
