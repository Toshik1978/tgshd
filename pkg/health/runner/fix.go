package runner

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/Toshik1978/tgshd/pkg/health"
	"github.com/Toshik1978/tgshd/pkg/health/notify"
	"github.com/Toshik1978/tgshd/pkg/health/store"
)

// InterruptedSaveTimeout bounds a checkpoint once shutdown has cancelled the
// tick: the one of a tick cut short (its fixes), the tick's final one and the
// stop's (Checkpoint). The daemon's stop timeout accounts for it.
const InterruptedSaveTimeout = 5 * time.Second

// Fix is one fix action a check took (e.g. the command check restarting a
// container). Every fix is recorded in the alert log as kind "fix"; it is
// pushed only when Send is set, on Mode (the notifier's default when empty).
type Fix struct {
	At    time.Time
	Check string
	Key   string
	Text  string
	Send  bool
	Mode  health.Mode
}

// FixSource hands over the fix actions a check took since the last call.
type FixSource interface {
	TakeFixes() []Fix
}

// takeFixes collects the fixes made by this tick's runs (and by runs that
// finished late, after their deadline) into the cycle. Every fix is logged,
// so one that is not pushed (report: journal) still reaches the journal; at
// Warn, the level the daemon logs at in production.
func (r *Runner) takeFixes(c *cycle) {
	for _, s := range r.opts.Fixes {
		for _, f := range s.TakeFixes() {
			r.opts.Logger.Warn("health fix action",
				zap.String("check", f.Check), zap.String("key", f.Key),
				zap.String("outcome", f.Text), zap.Bool("reported", f.Send))
			c.fixes = append(c.fixes, f)
		}
	}
}

// dispatchFixes pushes the fixes that ask for it on their mode.
func (r *Runner) dispatchFixes(ctx context.Context, c *cycle) {
	for _, f := range c.fixes {
		if !f.Send {
			continue
		}
		// Notify queues a failed delivery and retries it; its error is always nil.
		_ = r.notifier.Notify(ctx, notify.Message{
			Kind: notify.KindEvent, Check: f.Check, Key: f.Key, Text: f.Text, Mode: notify.Mode(f.Mode),
		})
	}
}

// saveInterruptedFixes records the fixes of a tick cut short by shutdown: a
// fix has happened, so it must reach the alert log although the rest of the
// tick is dropped. Nothing is sent. The best-effort checkpoint keeps the
// conditions as they were before the tick's runs (none of its results was
// dispatched) and leaves the checks' own state as last saved, so no result of
// the tick is lost unseen; the runner's own state is saved: the delivery
// state goes with the queue it describes, and the maintenance-lock
// observation with it.
func (r *Runner) saveInterruptedFixes(ctx context.Context, c *cycle, loaded bool) {
	r.takeFixes(c)
	if len(c.fixes) == 0 || !r.persistent() {
		return
	}
	r.addPending(fixRecords(c), nil)
	if !loaded {
		return
	}
	alerts, samples := r.takePending()

	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), InterruptedSaveTimeout)
	defer cancel()

	st := store.State{Conditions: c.before, RateMarks: r.marks.Export(), Queue: r.notifier.Export()}
	r.save(r.own, &st)
	if err := r.opts.Store.Checkpoint(saveCtx, st, alerts, samples); err != nil {
		r.opts.Logger.Warn("Cannot record the fixes of an interrupted health tick", zap.Error(err))
	}
}

// fixRecords returns the alert-log records of the cycle's fixes.
func fixRecords(c *cycle) []store.Alert {
	out := make([]store.Alert, 0, len(c.fixes))
	for _, f := range c.fixes {
		out = append(out, store.Alert{At: f.At, Kind: store.KindFix, Check: f.Check, Key: f.Key, Text: f.Text})
	}

	return out
}
