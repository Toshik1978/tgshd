package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/Toshik1978/tgshd/pkg/command"
	"github.com/Toshik1978/tgshd/pkg/health/check/disks"
	"github.com/Toshik1978/tgshd/pkg/health/check/memory"
	"github.com/Toshik1978/tgshd/pkg/health/check/modemprobe"
	"github.com/Toshik1978/tgshd/pkg/health/check/temps"
	hcfg "github.com/Toshik1978/tgshd/pkg/health/config"
	"github.com/Toshik1978/tgshd/pkg/health/notify"
	"github.com/Toshik1978/tgshd/pkg/health/report"
	"github.com/Toshik1978/tgshd/pkg/health/runner"
	"github.com/Toshik1978/tgshd/pkg/health/store"
	"github.com/Toshik1978/tgshd/pkg/host/sysfs"
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
	missingSuffix  = "/missing"
	driveMetricsOf = "drive."
)

// errNotBound means the weekly job ran before the runner was bound.
var errNotBound = errors.New("health reports not bound to the runner")

// reportStore is the state store as the reports read and prune it;
// *store.Store satisfies it.
type reportStore interface {
	command.HistorySource
	command.MetricsSource
	Retention(ctx context.Context, now time.Time) (store.RetentionResult, error)
}

// tempSensors is what the reports read of the temperatures check;
// *temps.Check satisfies it.
type tempSensors interface {
	Sensors() []temps.SensorStatus
}

var _ tempSensors = (*temps.Check)(nil)

// driveStatuses is what the reports read of the disks check; *disks.Check
// satisfies it.
type driveStatuses interface {
	Status() []disks.DriveStatus
}

var _ driveStatuses = (*disks.Check)(nil)

// topologyReader is what the summary reads of the host's mounts and block
// devices at report time; *sysfs.Reader satisfies it.
type topologyReader interface {
	MountInfo() ([]sysfs.MountEntry, error)
	BlockTopology() ([]sysfs.BlockDevice, error)
}

var _ topologyReader = (*sysfs.Reader)(nil)

// freshnessStamps is the freshness checks' state as the summary reads it;
// *freshnessState satisfies it.
type freshnessStamps interface {
	Stamp(name string) (stamp time.Time, present, readOK bool)
}

// probeStatus is the modem probe as the summary reads it; *probeState
// satisfies it.
type probeStatus interface {
	Status() modemprobe.State
}

// memoryFigures is what the reports read of the memory check; *memory.Check
// satisfies it.
type memoryFigures interface {
	Figures(ctx context.Context, n int) (memory.Figures, error)
}

var _ memoryFigures = (*memory.Check)(nil)

// guardStatus is the memory guard as /status shows it; *memoryGuard
// satisfies it.
type guardStatus interface {
	status() report.Guard
}

// reportRunner is the runner as the reports use it; *runner.Runner satisfies it.
type reportRunner interface {
	Snapshot() runner.Snapshot
	Push(ctx context.Context, check, text, record string)
	ExpireQueue(now time.Time, maxAge time.Duration) []notify.QueuedMessage
}

// healthReports serves /history, /temp and /disks, and the runner's daily
// retention and weekly summary jobs. It is also the drive source of /disks
// and of the summary: the disks check's status with the runner's open missing
// conditions (and, for the summary, the mounts and the stored wear).
type healthReports struct {
	// store is nil without a state database (memory only).
	store reportStore
	// sensors is the temperatures check's sensors; nil without it.
	sensors tempSensors
	// statuses is the disks check's status; nil without it.
	statuses driveStatuses
	// topology reads the mounts for the summary; nil shows them as unknown.
	topology topologyReader
	// checks are the configured check names and groups their summary_group.
	checks []string
	groups map[string]string
	// stamps and probe feed the summary's freshness and modem probe lines;
	// nil without those checks.
	stamps freshnessStamps
	probe  probeStatus
	// memory feeds the /status memory block and the summary's memory line;
	// nil without the memory section. guard is the memory guard; nil shows
	// it off.
	memory memoryFigures
	guard  guardStatus
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

// newHealthReports builds the reports over the setup's store, the checks and
// the host's mount and block topology.
func newHealthReports(hs *healthSetup, set checkSet, topology topologyReader, logger *zap.Logger) *healthReports {
	h := &healthReports{
		topology: topology,
		groups:   summaryGroups(hs.Config),
		stamps:   set.Stamps,
		probe:    set.Probe,
		memory:   set.Memory,
		host:     hs.Host,
		loc:      time.Local, //nolint:gosmopolitan // retention rolls samples up by the host's local day
		logger:   logger,
		now:      time.Now,
	}
	if hs.Store != nil {
		h.store = hs.Store
	}
	h.sensors = set.Temps
	if st, ok := set.Drives.(driveStatuses); ok {
		h.statuses = st
	}
	for i := range set.Checks {
		h.checks = append(h.checks, set.Checks[i].Name)
	}

	return h
}

// Drives implements command.DriveSource: the active drives as the disks
// check last saw them, with "missing" from the open missing condition.
func (h *healthReports) Drives() []command.Drive {
	return h.drives(h.open())
}

// TempSensors implements command.TempSensorSource: the temperature sensors
// the check last saw, with their limits and unreadable times.
func (h *healthReports) TempSensors() []report.Sensor {
	if h.sensors == nil {
		return nil
	}
	in := h.sensors.Sensors()
	out := make([]report.Sensor, len(in))
	for i, s := range in {
		out[i] = report.Sensor{Metric: s.Metric, Label: s.Label, Limit: s.Limit, Since: s.Since}
	}

	return out
}

// MemoryStatus implements command.MemoryStatusSource: the host's memory
// figures now with the guard line; nil without the memory section.
func (h *healthReports) MemoryStatus(ctx context.Context) *report.Memory {
	m := h.memoryFigures(ctx)
	if m == nil {
		return nil
	}
	m.Guard = &report.Guard{Mode: string(hcfg.InterveneOff)}
	if h.guard != nil {
		g := h.guard.status()
		m.Guard = &g
	}

	return m
}

// drives maps the disks check's status to drives, missing while their
// disks/<serial>/missing condition is open; nil without the disks check.
func (h *healthReports) drives(open []runner.OpenCondition) []command.Drive {
	if h.statuses == nil {
		return nil
	}
	missing := map[string]bool{}
	for _, c := range open {
		if s, ok := strings.CutPrefix(c.Key, diskKeyPrefix); ok {
			if serial, ok := strings.CutSuffix(s, missingSuffix); ok {
				missing[serial] = true
			}
		}
	}
	now := h.now()
	statuses := h.statuses.Status()
	out := make([]command.Drive, 0, len(statuses))
	for i := range statuses {
		out = append(out, statusDrive(&statuses[i], missing[statuses[i].Serial], now))
	}

	return out
}

// statusDrive maps one drive status to the /disks drive.
func statusDrive(s *disks.DriveStatus, missing bool, now time.Time) command.Drive {
	d := command.Drive{
		Node: s.Node, Bus: string(s.Bus), Model: s.Model, Serial: s.Serial, Transport: s.Transport,
		Health: s.Health, Missing: missing,
		Temp: s.Temperature, Wear: s.Wear, Spare: s.Spare, SpareThreshold: s.SpareThreshold,
		SpareFloor: s.SpareFloor,
		SelfTest: report.SelfTest{
			Format: string(s.SelfTestFormat), Age: s.SelfTestPassAge,
			NewestAge: s.SelfTestNewestAge, Unobserved: !s.SelfTestPassObserved,
		},
		AsleepSince: s.AsleepSince, UnreadableSince: s.UnreadableSince,
	}
	if d.Health == "" {
		d.Health = "unknown"
	}
	if s.SelfTestNewest != nil {
		d.SelfTest.Result = s.SelfTestNewest.Result
	}
	if !s.LastReadAt.IsZero() {
		ago := max(now.Sub(s.LastReadAt), 0)
		d.LastReadAge = &ago
	}

	return d
}

// hook sets the runner's daily (queue expiry, and retention with a store)
// and weekly summary jobs.
func (h *healthReports) hook(opts *runner.Options) {
	opts.Daily = h.daily
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
		command.NewTempCommand(logger, publisher, h.store, h),
		command.NewDisksCommand(logger, publisher, h, h.store),
	}
}

// daily expires the runner's undelivered messages older than queueMaxAge
// (the runner logs each, and the tick's flush no longer sends them), then
// applies the store's retention rules in the host's zone. A stored message the
// runner did not hold is logged here (never its text). A state database not
// open (yet) or closed skips the retention quietly, and without a state
// database (memory only) there is none.
func (h *healthReports) daily(ctx context.Context, now time.Time) error {
	expired := map[int64]bool{}
	if rh := h.bound(); rh.expire != nil {
		dropped := rh.expire(now, queueMaxAge)
		for i := range dropped {
			m := &dropped[i]
			expired[m.ID] = true
		}
	}
	if h.store == nil {
		return nil
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
	for i := range res.QueueDropped {
		m := &res.QueueDropped[i]
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
	in := report.SummaryInput{Host: h.host, Services: h.services(ctx), Now: now, Open: open}
	in.Drives = h.summaryDrives(open)
	if len(in.Drives) > 0 {
		in.DriveStats = h.driveStats(ctx, now)
	}
	rh.push(ctx, summaryCheck, report.Summary(in), summaryRecord)

	return nil
}

// summaryDrives is the drives with their mount points, read at report time;
// a failed read shows them as unknown ("?"), as it does for a missing drive.
func (h *healthReports) summaryDrives(open []runner.OpenCondition) []command.Drive {
	drives := h.drives(open)
	if len(drives) == 0 || h.topology == nil {
		return drives
	}
	mounts, mountErr := h.topology.MountInfo()
	blocks, blockErr := h.topology.BlockTopology()
	readErr := errors.Join(mountErr, blockErr)
	if readErr != nil {
		h.logger.Warn("cannot read the mounts for the weekly summary", zap.Error(readErr))
	}
	entries, topology := reportMounts(mounts), reportTopology(blocks)
	for i := range drives {
		// A missing drive's node is stale: another drive may hold it now.
		// Its Mounts stay empty, which the summary shows as "?".
		if drives[i].Missing {
			continue
		}
		drives[i].Mounts = report.DriveMounts(drives[i].Node, entries, topology, readErr)
	}

	return drives
}

// reportMounts converts the mount entries to the report's, dropping those
// without a device number.
func reportMounts(in []sysfs.MountEntry) []report.MountEntry {
	out := make([]report.MountEntry, 0, len(in))
	for _, e := range in {
		if isZeroDev(e.Dev) {
			continue
		}
		out = append(out, report.MountEntry{Dev: e.Dev.String(), MountPoint: e.MountPoint})
	}

	return out
}

// reportTopology converts the block topology to the report's. A device,
// partition or holder with a zero device number ("0:0", its dev file was not
// readable) is dropped so it never matches a mount entry.
func reportTopology(in []sysfs.BlockDevice) []report.BlockDevice {
	out := make([]report.BlockDevice, 0, len(in))
	for i := range in {
		d := &in[i]
		if isZeroDev(d.Dev) {
			continue
		}
		bd := report.BlockDevice{Name: d.Name, Dev: d.Dev.String(), Holders: reportHolders(d.Holders)}
		for _, p := range d.Partitions {
			if isZeroDev(p.Dev) {
				continue
			}
			bd.Partitions = append(bd.Partitions, report.BlockPartition{
				Name: p.Name, Dev: p.Dev.String(), Holders: reportHolders(p.Holders),
			})
		}
		out = append(out, bd)
	}

	return out
}

func reportHolders(in []sysfs.Holder) []report.BlockHolder {
	var out []report.BlockHolder
	for _, h := range in {
		if !isZeroDev(h.Dev) {
			out = append(out, report.BlockHolder{Name: h.Name, Dev: h.Dev.String()})
		}
	}

	return out
}

func isZeroDev(d sysfs.DevNum) bool { return d == sysfs.DevNum{} }

// services maps the configured checks onto the summary's services block,
// which leaves the hardware checks out itself: each with its summary_group,
// a freshness check with its stamp, the modem probe with its state and the
// memory check with the host's figures now.
func (h *healthReports) services(ctx context.Context) []report.ServiceCheck {
	out := make([]report.ServiceCheck, len(h.checks))
	for i, name := range h.checks {
		c := report.ServiceCheck{Name: name, Group: h.groups[name]}
		if entry, ok := strings.CutPrefix(name, freshnessCheckPrefix); ok && h.stamps != nil {
			at, present, readOK := h.stamps.Stamp(entry)
			c.Freshness = &report.FreshnessStamp{At: at, Present: present, ReadOK: readOK}
		}
		if name == modemProbeCheckName && h.probe != nil {
			st := h.probe.Status()
			c.Modem = &report.ModemState{LastOK: st.LastOKAt, Outstanding: st.Outstanding()}
		}
		if name == memoryCheckName {
			c.Memory = h.memoryFigures(ctx)
		}
		out[i] = c
	}

	return out
}

// memoryFigures reads the memory check's figures now; nil without the memory
// section. An unreadable host shows as unreadable, unreadable services leave
// the top consumers out.
func (h *healthReports) memoryFigures(ctx context.Context) *report.Memory {
	if h.memory == nil {
		return nil
	}
	fig, err := h.memory.Figures(ctx, memoryTopServices)
	if err != nil {
		h.logger.Warn("cannot read the memory figures", zap.Error(err))
	}
	host := fig.Host
	if host.Total == 0 {
		return &report.Memory{Unreadable: true}
	}
	m := &report.Memory{
		Total: host.Total, Available: host.Available,
		SwapTotal: host.SwapTotal, SwapUsed: host.SwapTotal - min(host.SwapFree, host.SwapTotal),
		Stalled: host.FullAvg10, HasPressure: host.HasPressure,
	}
	for _, s := range fig.Top {
		m.Top = append(m.Top, report.Consumer{Name: s.Name, Size: s.Anon})
	}

	return m
}

// driveStats reads the drive.* metrics, whose wear stands in for a drive not
// read since the start; none without a state database or when it cannot be
// read.
func (h *healthReports) driveStats(ctx context.Context, now time.Time) []store.MetricStats {
	if h.store == nil {
		return nil
	}
	stats, err := h.store.Metrics(ctx, driveMetricsOf, now)
	if err != nil {
		h.logger.Warn("cannot read drive metrics for the weekly summary", zap.Error(err))

		return nil
	}

	return stats
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
