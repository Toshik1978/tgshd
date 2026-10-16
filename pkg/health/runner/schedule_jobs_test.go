package runner

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Toshik1978/tgshd/pkg/health"
)

func TestDailyDue(t *testing.T) {
	loc := time.FixedZone("TST", 2*60*60)
	at := func(d, h, m int) time.Time { return time.Date(2026, 3, d, h, m, 0, 0, loc) }
	tests := []struct {
		name      string
		last, now time.Time
		want      bool
	}{
		{"never run", time.Time{}, at(5, 10, 0), true},
		{"same day", at(5, 0, 1), at(5, 23, 59), false},
		{"next day just after midnight", at(5, 23, 59), at(6, 0, 0), true},
		{"a day later", at(5, 10, 0), at(6, 10, 0), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := dailyDue(tt.last, tt.now, loc); got != tt.want {
				t.Fatalf("dailyDue = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestWeeklyDue(t *testing.T) {
	loc := time.FixedZone("TST", 2*60*60)
	slot := Slot{Weekday: time.Monday, Hour: 9, Minute: 0}
	// 2026-03-02 is a Monday.
	at := func(d, h, m int) time.Time { return time.Date(2026, 3, d, h, m, 0, 0, loc) }
	prevWeek := at(2, 9, 0).AddDate(0, 0, -7)
	tests := []struct {
		name      string
		last, now time.Time
		want      bool
	}{
		{"before the slot, ran last week's", prevWeek, at(2, 8, 59), false},
		{"at the slot", prevWeek, at(2, 9, 0), true},
		{"ran at the slot, same instant", at(2, 9, 0), at(2, 9, 0), false},
		{"ran, later the same day", at(2, 9, 1), at(2, 23, 0), false},
		{"ran, mid week", at(2, 9, 1), at(5, 12, 0), false},
		{"ran, next slot arrives", at(2, 9, 1), at(9, 9, 0), true},
		{"restart after slot, not run yet", prevWeek, at(2, 9, 30), true},
		{"restart after slot, already run", at(2, 9, 1), at(2, 9, 30), false},
		{"down for weeks", at(2, 9, 1), at(30, 12, 0), true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := weeklyDue(tt.last, tt.now, loc, slot); got != tt.want {
				t.Fatalf("weeklyDue = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLastSlotAcrossDST(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skipf("no tzdata: %v", err)
	}
	slot := Slot{Weekday: time.Monday, Hour: 9, Minute: 0}
	// DST began Sunday 2026-03-29; the Monday slot keeps its wall time.
	now := time.Date(2026, 3, 30, 10, 0, 0, 0, loc)

	got := lastSlot(now, loc, slot)

	if want := time.Date(2026, 3, 30, 9, 0, 0, 0, loc); !got.Equal(want) {
		t.Fatalf("lastSlot = %v, want %v", got, want)
	}
	prev := lastSlot(got.Add(-time.Second), loc, slot)
	if want := time.Date(2026, 3, 23, 9, 0, 0, 0, loc); !prev.Equal(want) {
		t.Fatalf("previous slot = %v, want %v", prev, want)
	}
	if d := got.Sub(prev); d != 7*24*time.Hour-time.Hour {
		t.Fatalf("slots %v apart, want 167h across the spring-forward", d)
	}
	// A slot inside the skipped hour still resolves to a real time that day.
	noon := time.Date(2026, 3, 29, 12, 0, 0, 0, loc)
	gap := lastSlot(noon, loc, Slot{Weekday: time.Sunday, Hour: 2, Minute: 30})
	if gap.Day() != 29 || gap.After(noon) {
		t.Fatalf("slot in the DST gap = %v", gap)
	}
}

func TestDailyDueAcrossDST(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Skipf("no tzdata: %v", err)
	}
	if !dailyDue(time.Date(2026, 3, 28, 23, 30, 0, 0, loc), time.Date(2026, 3, 29, 0, 30, 0, 0, loc), loc) {
		t.Fatal("not due on the next local day")
	}
	if dailyDue(time.Date(2026, 3, 29, 0, 30, 0, 0, loc), time.Date(2026, 3, 29, 23, 59, 0, 0, loc), loc) {
		t.Fatal("due twice on the 23-hour day")
	}
}

func TestStoredTimeCodec(t *testing.T) {
	ts := time.Unix(1_800_000_000, 0)
	if got := decodeTime(encodeTime(ts)); !got.Equal(ts) {
		t.Fatalf("round trip = %v, want %v", got, ts)
	}
	for _, bad := range []string{"", "x", "-5", "0"} {
		if got := decodeTime(bad); !got.IsZero() {
			t.Errorf("decodeTime(%q) = %v, want zero", bad, got)
		}
	}
}

type jobs struct {
	daily, weekly []time.Time
	dailyErr      error
}

func (j *jobs) options(o *Options) {
	o.Daily = func(_ context.Context, now time.Time) error {
		j.daily = append(j.daily, now)

		return j.dailyErr
	}
	o.Weekly = func(_ context.Context, now time.Time) error {
		j.weekly = append(j.weekly, now)

		return nil
	}
	o.WeeklySlot = &Slot{Weekday: time.Friday, Hour: 6, Minute: 0}
}

// jobFixture builds a runner over st at now with the hooks wired.
func jobFixture(st *fakeStore, j *jobs, now time.Time, checks ...health.Check) *fixture {
	f := &fixture{now: now, tlg: &fakeSender{}, up: &fakeUptime{up: time.Hour}, maint: &fakeMaintenance{}, st: st}
	o := Options{
		Checks: checks, Concurrency: 2, Maintenance: f.maint, Uptime: f.up, Store: st, Host: host,
		Telegram: f.tlg, Now: func() time.Time { return f.now }, Location: zone(),
	}
	j.options(&o)
	f.r = New(o)

	return f
}

// 2026-01-02 is a Friday; the zone is UTC+2, so 03:00 UTC is 05:00 local and
// the weekly slot (Friday 06:00 local) is 04:00 UTC.
func TestDailyRunsOncePerLocalDayAndSurvivesRestart(t *testing.T) {
	st := &fakeStore{}
	j := &jobs{}
	f := jobFixture(st, j, t0())
	f.tick(t)
	f.next(t)

	if len(j.daily) != 1 {
		t.Fatalf("daily ran %d times in one day, want 1", len(j.daily))
	}

	// Restart the same local day: no repeat.
	f2 := jobFixture(st, j, t0().Add(time.Hour))
	f2.tick(t)

	if len(j.daily) != 1 {
		t.Fatalf("daily repeated after a restart: %d", len(j.daily))
	}

	// Local midnight (22:00 UTC) starts a new day.
	f2.now = time.Date(2026, 1, 2, 22, 0, 0, 0, time.UTC)
	f2.tick(t)
	f2.next(t)

	if len(j.daily) != 2 {
		t.Fatalf("daily ran %d times, want 2 after local midnight", len(j.daily))
	}
}

func TestWeeklySlotRestartBeforeAndAfter(t *testing.T) {
	st := &fakeStore{}
	j := &jobs{}
	f := jobFixture(st, j, t0()) // Friday 05:00 local, before the slot.
	f.tick(t)

	if len(j.weekly) != 0 {
		t.Fatalf("weekly ran at first start: %v", j.weekly)
	}

	// Down across the slot; the restart at 06:30 local runs it once, late.
	f2 := jobFixture(st, j, time.Date(2026, 1, 2, 6, 30, 0, 0, zone()))
	f2.tick(t)
	f2.next(t)

	if len(j.weekly) != 1 {
		t.Fatalf("weekly ran %d times after restart past the slot, want 1", len(j.weekly))
	}

	// A restart after it ran does not repeat it.
	f3 := jobFixture(st, j, f2.now.Add(time.Hour))
	f3.tick(t)

	if len(j.weekly) != 1 {
		t.Fatalf("weekly repeated after a restart: %d", len(j.weekly))
	}

	// The next Friday slot runs it again.
	f3.now = time.Date(2026, 1, 9, 6, 0, 0, 0, zone())
	f3.tick(t)

	if len(j.weekly) != 2 {
		t.Fatalf("weekly ran %d times by next week, want 2", len(j.weekly))
	}
}

// TestDailyRunsInQuietWindowWeeklyWaits: the daily job (queue expiry and
// retention, local store work) runs in a quiet tick too, so a long window
// cannot deliver queued messages past their maximum age; the weekly summary
// waits for the window to end.
func TestDailyRunsInQuietWindowWeeklyWaits(t *testing.T) {
	st := &fakeStore{}
	j := &jobs{}
	f := jobFixture(st, j, t0()) // Friday 05:00 local, before the weekly slot.
	f.tick(t)

	f.maint.held = true
	f.now = time.Date(2026, 1, 3, 5, 0, 0, 0, zone()) // Next day, past the slot.
	f.tick(t)

	if len(j.daily) != 2 || len(j.weekly) != 0 {
		t.Fatalf("daily %d (want 2), weekly %d (want 0) inside the window", len(j.daily), len(j.weekly))
	}

	f.maint.held = false
	f.next(t)

	if len(j.daily) != 2 || len(j.weekly) != 1 {
		t.Fatalf("daily %d (want 2), weekly %d (want 1) after the window", len(j.daily), len(j.weekly))
	}
}

func TestJobErrorIsCheckerErrorAndCountsAsDone(t *testing.T) {
	st := &fakeStore{}
	j := &jobs{dailyErr: errors.New("db locked")}
	f := jobFixture(st, j, t0())
	f.tick(t)
	f.next(t)

	if len(j.daily) != 1 {
		t.Fatalf("failing daily job ran %d times, want 1 (done when attempted)", len(j.daily))
	}
	if got := countContaining(f.tlg.texts(), "daily job failed: db locked"); got != 1 {
		t.Fatalf("checker error sent %d times, want 1: %q", got, f.tlg.texts())
	}
}

func TestPerCheckLastRunSurvivesRestart(t *testing.T) {
	st := &fakeStore{}
	c := newFakeCheck(nil, "disk", reading("disk/root", health.Good, ""))
	chk := checkOf(c)
	chk.Interval = time.Hour
	f := jobFixture(st, &jobs{}, t0(), chk)
	f.tick(t)

	if c.count() != 1 {
		t.Fatalf("first run count = %d", c.count())
	}
	if _, ok := st.state.KV["schedule.check.disk.last"]; !ok {
		t.Fatalf("kv = %v, want the check's last run", st.state.KV)
	}

	f2 := jobFixture(st, &jobs{}, t0().Add(10*time.Minute), chk)
	f2.tick(t)

	if c.count() != 1 {
		t.Fatalf("check re-ran %d times right after a restart, want 1", c.count())
	}

	f2.now = t0().Add(time.Hour)
	f2.tick(t)

	if c.count() != 2 {
		t.Fatalf("check ran %d times after its interval, want 2", c.count())
	}
}

func TestWeeklyFirstStartAfterSlotWaitsForNextSlot(t *testing.T) {
	st := &fakeStore{}
	j := &jobs{}
	// Friday 06:30 local, past the slot, no history: nothing runs now.
	f := jobFixture(st, j, time.Date(2026, 1, 2, 6, 30, 0, 0, zone()))
	f.tick(t)
	f.next(t)

	if len(j.weekly) != 0 {
		t.Fatalf("weekly ran on install: %v", j.weekly)
	}

	// A lost key behaves the same way and the next slot fires.
	delete(st.state.KV, keyWeeklyLast)
	f2 := jobFixture(st, j, time.Date(2026, 1, 9, 6, 0, 0, 0, zone()))
	f2.tick(t)

	if len(j.weekly) != 0 {
		t.Fatalf("weekly ran with the key lost: %v", j.weekly)
	}
}

func TestLateLoadKeepsNewerMemoryAndGatesJobs(t *testing.T) {
	st := &fakeStore{loadErr: errors.New("db busy")}
	st.state.KV = map[string]string{"schedule.check.disk.last": encodeTime(t0().Add(-2 * time.Hour))}
	c := newFakeCheck(nil, "disk", reading("disk/root", health.Good, ""))
	chk := checkOf(c)
	chk.Interval = time.Hour
	j := &jobs{}
	f := jobFixture(st, j, t0(), chk)
	f.tick(t)

	if c.count() != 1 || len(j.daily) != 0 {
		t.Fatalf("while load fails: check runs %d (want 1), daily %d (want 0)", c.count(), len(j.daily))
	}

	st.loadErr = nil
	f.next(t)

	if c.count() != 1 {
		t.Fatalf("check ran %d times, want 1: the older stored time must not win", c.count())
	}
	if len(j.daily) != 1 {
		t.Fatalf("daily ran %d times once loaded, want 1", len(j.daily))
	}
}

func TestJobCancelledByShutdownIsRetried(t *testing.T) {
	st := &fakeStore{}
	j := &jobs{}
	f := jobFixture(st, j, t0())
	ctx, cancel := context.WithCancel(context.Background())
	f.r.opts.Daily = func(context.Context, time.Time) error {
		cancel()

		return ctx.Err()
	}
	c := &cycle{now: f.now}
	f.r.runDaily(ctx, c)

	if len(c.errs) != 0 || !f.r.dailyLast.IsZero() {
		t.Fatalf("cancelled job: errs %v, dailyLast %v; want neither", c.errs, f.r.dailyLast)
	}

	f.r.opts.Daily = func(context.Context, time.Time) error { j.daily = append(j.daily, f.now); return nil }
	f.r.runDaily(context.Background(), c)

	if len(j.daily) != 1 {
		t.Fatalf("daily retried %d times, want 1", len(j.daily))
	}
}
