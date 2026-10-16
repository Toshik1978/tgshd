package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/Toshik1978/tgshd/pkg/command"
	"github.com/Toshik1978/tgshd/pkg/health/notify"
	"github.com/Toshik1978/tgshd/pkg/health/report"
	"github.com/Toshik1978/tgshd/pkg/health/runner"
	"github.com/Toshik1978/tgshd/pkg/health/store"
	"github.com/Toshik1978/tgshd/pkg/telegram"
)

const (
	// noHistoryReply answers /history, /temp and /disks without a state database.
	noHistoryReply = "History is unavailable: the state database is not open."
	// summaryCheck is the check name of the pushed weekly summary, and
	// summaryRecord its alert-log text.
	summaryCheck  = "summary"
	summaryRecord = "Weekly summary sent"
	// queueMaxAge is how long an undelivered message is retried; the store's
	// retention drops the same age.
	queueMaxAge = 7 * 24 * time.Hour

	diskKeyPrefix  = disksCheckName + "/"
	healthSuffix   = "/health"
	missingSuffix  = "/missing"
	driveMetricsOf = "drive."
	spareSuffix    = ".spare"
	selfTestSuffix = ".selftest_age_h"
	// driveReadTimeout bounds the metrics read of /disks' drive source, which
	// gets no context.
	driveReadTimeout = 10 * time.Second
)

var (
	// errNoHistory makes the summary's drives section read "unavailable" when
	// there is no state database to read the drive metrics from.
	errNoHistory = errors.New("no state database")
	// errNotBound means the weekly job ran before the runner was bound.
	errNotBound = errors.New("health reports not bound to the runner")
)

// reportStore is the state store as the reports read and prune it;
// *store.Store satisfies it.
type reportStore interface {
	command.HistorySource
	command.MetricsSource
	Retention(ctx context.Context, now time.Time) (store.RetentionResult, error)
}

// reportRunner is the runner as the reports use it; *runner.Runner satisfies it.
type reportRunner interface {
	Snapshot() runner.Snapshot
	Push(ctx context.Context, check, text, record string)
	ExpireQueue(now time.Time, maxAge time.Duration) []notify.QueuedMessage
}

// healthReports serves /history, /temp and /disks, and the runner's daily
// retention and weekly summary jobs. It is also the drive source of /disks
// and the summary: the disks check's baselines joined with the runner's open
// conditions and the stored drive metrics.
type healthReports struct {
	// store is nil without a state database (memory only).
	store reportStore
	// book is nil without the disks check.
	book   runner.DriveBook
	checks []string
	host   string
	// loc is the host's zone retention takes local days in.
	loc    *time.Location
	logger *zap.Logger
	now    func() time.Time

	// mu guards runner, set by bind.
	mu     sync.Mutex
	runner runnerHooks
}

// runnerHooks are the runner's methods the reports call; all nil before bind.
type runnerHooks struct {
	snapshot func() runner.Snapshot
	push     func(ctx context.Context, check, text, record string)
	expire   func(now time.Time, maxAge time.Duration) []notify.QueuedMessage
}

// newHealthReports builds the reports over the setup's store and the checks.
func newHealthReports(hs *healthSetup, set checkSet, logger *zap.Logger) *healthReports {
	h := &healthReports{
		book:   set.Drives,
		host:   hs.Host,
		loc:    time.Local, //nolint:gosmopolitan // retention rolls samples up by the host's local day
		logger: logger,
		now:    time.Now,
	}
	if hs.Store != nil {
		h.store = hs.Store
	}
	for i := range set.Checks {
		h.checks = append(h.checks, set.Checks[i].Name)
	}

	return h
}

// Drives implements command.DriveSource: the active drives with their health
// from the open conditions, and spare and self-test age from the latest
// stored samples (unknown without them).
func (h *healthReports) Drives() []command.Drive {
	if h.book == nil {
		return nil
	}
	var stats []store.MetricStats
	if h.store != nil {
		ctx, cancel := context.WithTimeout(context.Background(), driveReadTimeout)
		defer cancel()

		var err error
		if stats, err = h.store.Metrics(ctx, driveMetricsOf, h.now()); err != nil {
			h.logger.Warn("cannot read drive metrics; spare and self-test age unknown", zap.Error(err))
		}
	}

	return driveList(h.book.Baselines(), h.open(), stats)
}

// hook sets the runner's daily retention (with a store only) and weekly
// summary jobs.
func (h *healthReports) hook(opts *runner.Options) {
	if h.store != nil {
		opts.Daily = h.daily
	}
	opts.Weekly = h.weekly
}

// bind hands over the runner: its snapshot gives the open conditions and the
// summary is pushed through its notifier.
func (h *healthReports) bind(r reportRunner) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.runner = runnerHooks{snapshot: r.Snapshot, push: r.Push, expire: r.ExpireQueue}
}

// handlers returns /history, /temp and /disks; without a state database each
// replies that history is unavailable.
func (h *healthReports) handlers(logger *zap.Logger, publisher command.Publisher) []telegram.Handler {
	if h.store == nil {
		out := make([]telegram.Handler, 0, 3)
		for _, name := range []string{"history", "temp", "disks"} {
			out = append(out, replyCommand{name: name, reply: noHistoryReply, publisher: publisher})
		}

		return out
	}

	return []telegram.Handler{
		command.NewHistoryCommand(logger, publisher, h.store),
		command.NewTempCommand(logger, publisher, h.store),
		command.NewDisksCommand(logger, publisher, h, h.store),
	}
}

// daily expires the runner's undelivered messages older than queueMaxAge
// (the runner logs each, and the tick's flush no longer sends them), then
// applies the store's retention rules in the host's zone. A stored message the
// runner did not hold is logged here (never its text). A state database not
// open (yet) or closed skips the retention quietly.
func (h *healthReports) daily(ctx context.Context, now time.Time) error {
	expired := map[int64]bool{}
	if rh := h.bound(); rh.expire != nil {
		for _, m := range rh.expire(now, queueMaxAge) {
			expired[m.ID] = true
		}
	}
	res, err := h.store.Retention(ctx, now.In(h.loc))
	switch {
	case errors.Is(err, store.ErrNotOpen), errors.Is(err, store.ErrClosed):
		// Nothing to prune; the runner's Load already reports a database it
		// cannot open as a checker error.
		h.logger.Debug("retention skipped: the state database is not open", zap.Error(err))

		return nil
	case err != nil:
		return fmt.Errorf("retention: %w", err)
	}
	for _, m := range res.QueueDropped {
		if expired[m.ID] {
			continue
		}
		h.logger.Warn("health message dropped after 7 days undelivered",
			zap.Int64("id", m.ID), zap.String("channel", m.Channel),
			zap.Time("created_at", m.CreatedAt), zap.Int("attempts", m.Attempts))
	}

	return nil
}

// weekly pushes the weekly summary through the runner's notifier, which adds
// the host prefix (FR-010) and queues a failed delivery for retry.
func (h *healthReports) weekly(ctx context.Context, now time.Time) error {
	rh := h.bound()
	if rh.snapshot == nil {
		return errNotBound
	}
	open := rh.snapshot().Open
	in := report.SummaryInput{Host: h.host, Checks: h.checks, Open: open}
	if h.book != nil {
		bases := h.book.Baselines()
		if slices.ContainsFunc(bases, func(b store.Baseline) bool { return !b.Retired }) {
			in.DriveStats, in.DrivesErr = h.driveStats(ctx, now)
		}
		in.Drives = driveList(bases, open, in.DriveStats)
	}
	rh.push(ctx, summaryCheck, report.Summary(in), summaryRecord)

	return nil
}

// driveStats reads the drive.* metrics (wear, data written, spare, self-test age).
func (h *healthReports) driveStats(ctx context.Context, now time.Time) ([]store.MetricStats, error) {
	if h.store == nil {
		return nil, errNoHistory
	}
	stats, err := h.store.Metrics(ctx, driveMetricsOf, now)
	if err != nil {
		h.logger.Warn("cannot read drive metrics for the weekly summary", zap.Error(err))

		return nil, fmt.Errorf("read drive metrics: %w", err)
	}

	return stats, nil
}

// bound returns the runner's methods; nil before bind.
func (h *healthReports) bound() runnerHooks {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.runner
}

// open returns the runner's open conditions; none before it is bound.
func (h *healthReports) open() []runner.OpenCondition {
	rh := h.bound()
	if rh.snapshot == nil {
		return nil
	}

	return rh.snapshot().Open
}

// driveList maps the active (not retired) baselines to drives ordered by
// serial. A drive is healthy unless its disks/<serial>/health condition is
// open, and missing while its disks/<serial>/missing condition is. Spare and self-test age come from the current
// drive.<serial>.spare
// and drive.<serial>.selftest_age_h samples; without one they stay unknown.
func driveList(bases []store.Baseline, open []runner.OpenCondition, stats []store.MetricStats) []command.Drive {
	openKeys := make(map[string]bool, len(open))
	for _, c := range open {
		if strings.HasPrefix(c.Key, diskKeyPrefix) {
			openKeys[c.Key] = true
		}
	}
	latest := make(map[string]float64, len(stats))
	for _, m := range stats {
		if m.HasCurrent {
			latest[m.Metric] = m.Current
		}
	}

	var out []command.Drive
	for i := range bases {
		b := &bases[i]
		if b.Retired {
			continue
		}
		d := command.Drive{
			Bus: b.Bus, Model: b.Model, Serial: b.Serial,
			HealthOK: !openKeys[diskKeyPrefix+b.Serial+healthSuffix],
			Missing:  openKeys[diskKeyPrefix+b.Serial+missingSuffix],
		}
		if v, ok := latest[driveMetricsOf+b.Serial+spareSuffix]; ok {
			d.SparePct = &v
		}
		if v, ok := latest[driveMetricsOf+b.Serial+selfTestSuffix]; ok {
			age := time.Duration(v * float64(time.Hour))
			d.SelfTestAge = &age
		}
		out = append(out, d)
	}
	slices.SortFunc(out, func(a, b command.Drive) int { return strings.Compare(a.Serial, b.Serial) })

	return out
}

// replyCommand answers a command with a fixed reply.
type replyCommand struct {
	name      string
	reply     string
	publisher command.Publisher
}

// Name implements telegram.Handler.
func (c replyCommand) Name() string { return c.name }

// Enabled implements telegram.Handler.
func (replyCommand) Enabled() bool { return true }

// Handle implements telegram.Handler.
func (c replyCommand) Handle(ctx context.Context, senderID int64, _ string) error {
	if err := c.publisher.Publish(ctx, senderID, c.reply); err != nil {
		return fmt.Errorf("failed to publish reply: %w", err)
	}

	return nil
}
