package runner

import (
	"context"
	"strconv"
	"time"
)

// dueSlack absorbs tick jitter: a check whose interval elapses up to half a
// tick after this tick runs now rather than a whole tick late.
const dueSlack = Tick / 2

const (
	keyCheckPrefix = "schedule.check."
	keyCheckSuffix = ".last"
	keyDailyLast   = "schedule.daily.last"
	keyWeeklyLast  = "schedule.weekly.last"
	daysPerWeek    = 7
)

// Slot is a weekly "weekday + HH:MM" time in the host's zone.
type Slot struct {
	Weekday time.Weekday
	Hour    int
	Minute  int
}

// due reports whether a check last run at last is due at now. A check that
// never ran is due, and so is one whose last run lies in the future (the clock
// went backwards).
func due(last, now time.Time, interval time.Duration) bool {
	if last.IsZero() {
		return true
	}
	elapsed := now.Sub(last)

	return elapsed < 0 || elapsed+dueSlack >= interval
}

// dailyDue reports whether the daily job is due: it never ran, or last ran on
// an earlier host-local calendar day than now.
func dailyDue(last, now time.Time, loc *time.Location) bool {
	if last.IsZero() {
		return true
	}
	ly, lm, ld := last.In(loc).Date()
	ny, nm, nd := now.In(loc).Date()

	return ly != ny || lm != nm || ld != nd
}

// lastSlot returns the latest occurrence of the slot that is not after now. It
// is built with time.Date per calendar day, so it stays at the same wall-clock
// time across DST changes (a wall time that does not exist shifts forward).
func lastSlot(now time.Time, loc *time.Location, s Slot) time.Time {
	n := now.In(loc)
	back := (int(n.Weekday()) - int(s.Weekday) + daysPerWeek) % daysPerWeek
	slot := time.Date(n.Year(), n.Month(), n.Day()-back, s.Hour, s.Minute, 0, 0, loc)
	if slot.After(now) {
		slot = time.Date(n.Year(), n.Month(), n.Day()-back-daysPerWeek, s.Hour, s.Minute, 0, 0, loc)
	}

	return slot
}

// weeklyDue reports whether the weekly slot has passed since the job last ran.
// A job that has run since the latest slot is not due again until the next
// one; a slot missed while the daemon was down runs once, late. The caller
// handles a job that never ran (zero last).
func weeklyDue(last, now time.Time, loc *time.Location, s Slot) bool {
	return last.Before(lastSlot(now, loc, s))
}

func checkKey(name string) string { return keyCheckPrefix + name + keyCheckSuffix }

func encodeTime(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }

// decodeTime parses Unix seconds; a missing or malformed value is the zero time.
func decodeTime(s string) time.Time {
	sec, err := strconv.ParseInt(s, 10, 64)
	if err != nil || sec <= 0 {
		return time.Time{}
	}

	return time.Unix(sec, 0)
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}

	return a
}

// restoreSchedule merges the stored last-run times into memory once, the
// later time winning, so a restart neither re-runs every check nor repeats the
// daily or weekly job.
func (r *Runner) restoreSchedule() {
	if r.scheduleRestored {
		return
	}
	r.scheduleRestored = true

	r.loadMu.Lock()
	kv := r.loadedKV
	r.loadMu.Unlock()

	for i := range r.opts.Checks {
		r.lastRun[i] = later(r.lastRun[i], decodeTime(kv[checkKey(r.opts.Checks[i].Name)]))
	}
	r.dailyLast = later(r.dailyLast, decodeTime(kv[keyDailyLast]))
	r.weeklyLast = later(r.weeklyLast, decodeTime(kv[keyWeeklyLast]))
}

// scheduleKV returns the last-run times to persist as Unix seconds.
func (r *Runner) scheduleKV() map[string]string {
	kv := map[string]string{}
	for i := range r.opts.Checks {
		if !r.lastRun[i].IsZero() {
			kv[checkKey(r.opts.Checks[i].Name)] = encodeTime(r.lastRun[i])
		}
	}
	if !r.dailyLast.IsZero() {
		kv[keyDailyLast] = encodeTime(r.dailyLast)
	}
	if !r.weeklyLast.IsZero() {
		kv[keyWeeklyLast] = encodeTime(r.weeklyLast)
	}

	return kv
}

// runJobs runs the daily and weekly hooks when due. A hook counts as done once
// attempted (no hammering a failing job); its error is a checker error. A
// weekly job with no history is seeded with the latest slot, so nothing runs
// on install and the next slot fires. A hook that fails only because ctx ended
// is not counted as done and is retried next tick.
func (r *Runner) runJobs(ctx context.Context, c *cycle) {
	if r.opts.Daily != nil && dailyDue(r.dailyLast, c.now, r.opts.Location) {
		attempt(ctx, c, &r.dailyLast, r.opts.Daily, "health/daily/error", "daily job failed: ")
	}
	if r.opts.Weekly == nil || r.opts.WeeklySlot == nil {
		return
	}
	if r.weeklyLast.IsZero() {
		r.weeklyLast = lastSlot(c.now, r.opts.Location, *r.opts.WeeklySlot)
	}
	if weeklyDue(r.weeklyLast, c.now, r.opts.Location, *r.opts.WeeklySlot) {
		attempt(ctx, c, &r.weeklyLast, r.opts.Weekly, "health/weekly/error", "weekly job failed: ")
	}
}

// attempt marks a job done at c.now, runs it and reports a failure; a failure
// caused by ctx ending undoes the mark so the job is retried next tick.
func attempt(ctx context.Context, c *cycle, last *time.Time, job func(context.Context, time.Time) error,
	key, text string,
) {
	prev := *last
	*last = c.now
	if err := job(ctx, c.now); err != nil {
		if ctx.Err() != nil {
			*last = prev

			return
		}
		c.fail(key, text+err.Error())
	}
}
