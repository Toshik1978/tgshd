package runner

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Toshik1978/tgshd/pkg/health"
	"github.com/Toshik1978/tgshd/pkg/health/check/disks"
	"github.com/Toshik1978/tgshd/pkg/health/config"
)

// TestCheckTimeoutCeilingFitsTheRunBudget: a configurable check timeout, plus
// the margin a bounded step keeps, ends inside the run budget.
func TestCheckTimeoutCeilingFitsTheRunBudget(t *testing.T) {
	if health.MaxCheckTimeout+health.RunMargin >= MaxRunTime {
		t.Fatalf("MaxCheckTimeout %s + RunMargin %s must end inside MaxRunTime %s",
			health.MaxCheckTimeout, health.RunMargin, MaxRunTime)
	}
}

// TestCheckHangingPastTheRunBudget documents the budget model: a check whose
// own timeout is longer than the run budget, and which bounds its step with
// health.Bound, is cut before the budget ends and judges the hang itself (a
// PROBLEM, here with threshold 0); a check that ignores its context is
// abandoned at the budget and only reported as a checker error.
func TestCheckHangingPastTheRunBudget(t *testing.T) {
	const ownTimeout = time.Hour

	release := make(chan struct{})
	defer close(release)

	bounded := health.Check{
		Name:     "bounded",
		Interval: 5 * time.Minute,
		Run: func(ctx context.Context) health.CheckResult {
			step, cancel, _ := health.Bound(ctx, ownTimeout)
			defer cancel()
			<-step.Done()
			if ctx.Err() != nil {
				return health.CheckResult{Err: ctx.Err()}
			}

			return reading("bounded/probe", health.Bad, "probe did not finish before the run deadline")
		},
	}
	ignoring := health.Check{
		Name:     "ignoring",
		Interval: 5 * time.Minute,
		Run: func(context.Context) health.CheckResult {
			<-release // Ignores its context on purpose.

			return reading("ignoring/probe", health.Bad, "late")
		},
	}
	tlg := &fakeSender{}
	r := New(Options{
		Checks: []health.Check{bounded, ignoring}, RunBudget: 300 * time.Millisecond,
		Telegram: tlg, Host: host, Now: t0,
	})

	start := time.Now()
	if err := r.Do(context.Background()); err != nil {
		t.Fatal(err)
	}

	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("tick took %v, want it bounded by the run budget", elapsed)
	}
	texts := tlg.texts()
	if countContaining(texts, "PROBLEM: probe did not finish before the run deadline") != 1 {
		t.Fatalf("telegram = %q, want the bounded check's PROBLEM", texts)
	}
	if countContaining(texts, "CHECKER ERROR: bounded") != 0 {
		t.Fatalf("telegram = %q, the bounded check must not be a checker error", texts)
	}
	if countContaining(texts, "CHECKER ERROR: ignoring: run did not finish before the tick deadline") != 1 ||
		countContaining(texts, "late") != 0 {
		t.Fatalf("telegram = %q, want only a checker error for the check that ignores its context", texts)
	}
}

// TestQuietTickCheckpoints: what a quiet tick raises (a checker error, its
// rate mark, a message queued while Telegram fails) is checkpointed, so a crash
// during a long maintenance window loses none of it.
func TestQuietTickCheckpoints(t *testing.T) {
	f := newFixture(t, &fakeStore{})
	f.maint.held = true
	f.hb.err = errors.New("disk full")
	f.tlg.setFail(true)

	f.tick(t)

	if f.r.Snapshot().Quiet != health.ReasonMaintenance {
		t.Fatal("the tick was not quiet")
	}
	if f.st.checkpoints != 1 {
		t.Fatalf("quiet tick checkpointed %d times, want 1", f.st.checkpoints)
	}
	f.st.mu.Lock()
	defer f.st.mu.Unlock()
	if len(f.st.state.Queue) != 1 || len(f.st.state.RateMarks) == 0 {
		t.Fatalf("queue = %+v, marks = %+v, want the queued checker error and its mark",
			f.st.state.Queue, f.st.state.RateMarks)
	}
	if kinds := f.st.alerts; len(kinds) != 1 || kinds[0].Key != keyHeartbeat {
		t.Fatalf("alert log = %+v, want the heartbeat checker error", kinds)
	}
}

// hangingDrives is a disks.Source whose drive "sdb" hangs until its context
// ends and then, like a killed smartctl whose output is still being drained,
// takes a little longer to return; every other drive is in standby.
type hangingDrives struct{}

func (hangingDrives) Drives() ([]string, error) { return []string{"sda", "sdb"}, nil }

func (hangingDrives) Run(ctx context.Context, node string) ([]byte, int, error) {
	if node != "sdb" {
		return nil, disks.StandbyStatus, nil
	}
	<-ctx.Done()
	time.Sleep(20 * time.Millisecond)

	return nil, 0, ctx.Err()
}

// TestDisksReadsEndBeforeTheRunDeadline: a disks run whose drive read hangs
// past the run budget ends the read before the run's deadline, so the run
// reports the unreadable drive itself instead of being abandoned (which
// would lose every drive's readings).
func TestDisksReadsEndBeforeTheRunDeadline(t *testing.T) {
	chk := disks.New(hangingDrives{}, config.Disks{}, nil, time.Now)
	chk.LoadBaselines(nil)

	res := RunBounded(t.Context(), health.Check{Name: "disks", Run: chk.Run}, 3*time.Second, nil)
	if !errors.Is(res.Err, disks.ErrUnreadable) || !strings.Contains(res.Err.Error(), "/dev/sdb") {
		t.Fatalf("Err = %v, want the check's own unreadable /dev/sdb", res.Err)
	}
}
