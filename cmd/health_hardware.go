package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/fx"
	"go.uber.org/zap"

	"github.com/Toshik1978/tgshd/pkg/command"
	"github.com/Toshik1978/tgshd/pkg/health"
	"github.com/Toshik1978/tgshd/pkg/health/check/disks"
	"github.com/Toshik1978/tgshd/pkg/health/check/kernel"
	"github.com/Toshik1978/tgshd/pkg/health/check/temps"
	hcfg "github.com/Toshik1978/tgshd/pkg/health/config"
	"github.com/Toshik1978/tgshd/pkg/health/runner"
	"github.com/Toshik1978/tgshd/pkg/health/store"
	"github.com/Toshik1978/tgshd/pkg/host/edac"
	"github.com/Toshik1978/tgshd/pkg/host/hwmon"
	"github.com/Toshik1978/tgshd/pkg/host/journal"
	"github.com/Toshik1978/tgshd/pkg/host/kmsg"
	"github.com/Toshik1978/tgshd/pkg/host/nvidia"
	"github.com/Toshik1978/tgshd/pkg/host/smartctl"
)

const (
	// Hardware check names; each is the first segment of its condition keys.
	disksCheckName  = "disks"
	tempsCheckName  = "temps"
	kernelCheckName = "kernel"

	// kv keys of the checks' persisted state.
	kernelStateKey = "kernel.state"
	tempsSeenKey   = "temps.seen"

	// kmsgReadKey is the checker error raised when the kernel log reader fails.
	kmsgReadKey = kernelCheckName + "/kmsg/read"
	// backgroundRetry is the pause before a failed background job restarts.
	backgroundRetry = time.Minute

	// noDrivesReply answers /retire when no disks check runs.
	noDrivesReply = "No drives are monitored."
)

// errKernelStateNotLoaded fails kernel check runs until its stored state has
// been restored, mirroring disks.ErrBaselinesNotLoaded.
var errKernelStateNotLoaded = errors.New("kernel check state not loaded")

// background is a job that runs for the daemon's life, e.g. the kernel log
// reader. The daemon starts it after the stored state is loaded and restarts
// it after backgroundRetry when it fails, raising Key as a checker error.
// check --once never starts it.
type background struct {
	Key string
	Run func(ctx context.Context) error
}

// disksChecks provides the disks check (smartctl over the drives found in
// sysfs) when its section is present. Drive temperatures are judged against
// the temperatures section's drive limits and threshold when that is present.
func disksChecks(hs *healthSetup, logger *zap.Logger) checkSet {
	if hs.Config == nil || hs.Config.Disks == nil {
		return checkSet{}
	}

	src := smartctlSource{runner: smartctl.NewDefault()}

	return buildDisksCheck(hs.Config.Disks, hs.Config.Temperatures, src, time.Now, logger, hs.OneShot)
}

// smartctlRunner is the part of *smartctl.Runner smartctlSource uses.
type smartctlRunner interface {
	Drives() ([]smartctl.Node, error)
	Run(ctx context.Context, node string, wake bool) ([]byte, int, error)
}

// smartctlSource adapts pkg/host/smartctl to disks.Source.
type smartctlSource struct {
	runner smartctlRunner
}

// Drives implements disks.Source.
func (s smartctlSource) Drives() ([]disks.Node, error) {
	nodes, err := s.runner.Drives()
	if err != nil {
		return nil, err //nolint:wrapcheck // the adapter adds no context
	}
	out := make([]disks.Node, len(nodes))
	for i, n := range nodes {
		out[i] = disks.Node{Name: n.Name, Path: n.Path}
	}

	return out, nil
}

// Run implements disks.Source.
func (s smartctlSource) Run(ctx context.Context, node string, wake bool) ([]byte, int, error) {
	return s.runner.Run(ctx, node, wake) //nolint:wrapcheck // the adapter adds no context
}

// buildDisksCheck builds the disks check. Its baselines are persisted through
// the runner hook, and it is the drive book /retire works on. oneShot
// (check --once) builds it without the forced read, so it never wakes a drive.
func buildDisksCheck(
	d *hcfg.Disks,
	t *hcfg.Temperatures,
	src disks.Source,
	now func() time.Time,
	logger *zap.Logger,
	oneShot bool,
) checkSet {
	opts := []disks.Option{disks.WithLogger(logger)}
	if oneShot {
		opts = append(opts, disks.WithoutForcedRead())
	}
	chk := disks.New(src, *d, t, now, opts...)

	return checkSet{
		Checks:     []health.Check{newCheck(disksCheckName, d.Common, chk.Run)},
		Persisters: []runner.Persister{disksPersister{check: chk}},
		Drives:     chk,
	}
}

// disksPersister keeps the disks check's baselines, retired flags included,
// in the store's baselines table.
type disksPersister struct {
	check *disks.Check
}

// Restore implements runner.Persister. It always loads, even nothing: the
// disks check refuses to run until it has.
func (p disksPersister) Restore(st store.State) error {
	p.check.LoadBaselines(st.Baselines)

	return nil
}

// Save implements runner.Persister.
func (p disksPersister) Save(st *store.State) error {
	st.Baselines = append(st.Baselines, p.check.Baselines()...)

	return nil
}

// tempsChecks provides the temperatures check (hwmon and nvidia-smi) when its
// section is present.
func tempsChecks(hs *healthSetup, open *openState) checkSet {
	if hs.Config == nil || hs.Config.Temperatures == nil {
		return checkSet{}
	}

	return buildTempsCheck(hs.Config.Temperatures,
		hwmonSource{reader: hwmon.NewDefault()}, nvidiaSource{reader: nvidia.NewDefault()}, open)
}

// buildTempsCheck builds the temperatures check; its set of known sensors is
// persisted under tempsSeenKey. The check bounds the nvidia-smi run itself;
// the hwmon read is bounded by the check's run budget. open answers whether a
// sensor's condition is open.
func buildTempsCheck(t *hcfg.Temperatures, sensors temps.Hwmon, gpus temps.Nvidia, open temps.OpenState) checkSet {
	chk := temps.New(sensors, gpus, *t, open)

	return checkSet{
		Checks:     []health.Check{newCheck(tempsCheckName, t.Common, chk.Run)},
		Persisters: []runner.Persister{runner.KVPersister{Key: tempsSeenKey, State: chk}},
		Temps:      chk,
	}
}

// openSource answers whether a condition is open; the runner is one.
type openSource interface {
	IsOpen(key string) bool
}

// openState is the temps.OpenState of the temperatures check. fx builds the
// checks before the runner, so it answers false until newHealth binds it to
// the runner.
type openState struct {
	mu     sync.Mutex
	source openSource
}

// newOpenState returns an unbound open state.
func newOpenState() *openState {
	return &openState{}
}

// IsOpen implements temps.OpenState.
func (o *openState) IsOpen(key string) bool {
	o.mu.Lock()
	src := o.source
	o.mu.Unlock()

	return src != nil && src.IsOpen(key)
}

// bind hands over the source the open state answers from. A nil open state
// (one no check was built with) has nothing to bind.
func (o *openState) bind(src openSource) {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()

	o.source = src
}

// hwmonReader is the sensor read of pkg/host/hwmon.
type hwmonReader interface {
	Sensors() ([]hwmon.Sensor, error)
}

// hwmonSource adapts pkg/host/hwmon to temps.Hwmon.
type hwmonSource struct {
	reader hwmonReader
}

// Sensors implements temps.Hwmon.
func (s hwmonSource) Sensors() ([]temps.Sensor, error) {
	in, err := s.reader.Sensors()
	if err != nil {
		return nil, fmt.Errorf("hwmon: %w", err)
	}
	out := make([]temps.Sensor, len(in))
	for i := range in {
		out[i] = temps.Sensor(in[i])
	}

	return out, nil
}

// gpuReader is the GPU read of pkg/host/nvidia.
type gpuReader interface {
	GPUs(ctx context.Context) ([]nvidia.GPU, error)
}

// nvidiaSource adapts pkg/host/nvidia to temps.Nvidia.
type nvidiaSource struct {
	reader gpuReader
}

// GPUs implements temps.Nvidia.
func (s nvidiaSource) GPUs(ctx context.Context) ([]temps.GPU, error) {
	in, err := s.reader.GPUs(ctx)
	if errors.Is(err, nvidia.ErrNoDevices) {
		return nil, temps.ErrNoGPUs
	}
	if err != nil {
		return nil, fmt.Errorf("nvidia-smi: %w", err)
	}
	out := make([]temps.GPU, len(in))
	for i := range in {
		out[i] = temps.GPU(in[i])
	}

	return out, nil
}

// kernelChecks provides the kernel check (/dev/kmsg and EDAC) when its
// section is present. Only the daemon check reads the previous boot's journal tail.
func kernelChecks(hs *healthSetup, logger *zap.Logger) checkSet {
	if hs.Config == nil || hs.Config.Kernel == nil {
		return checkSet{}
	}

	if hs.OneShot {
		return buildKernelOneShot(hs.Config.Kernel, kmsg.NewDefault(), edac.NewDefault(), time.Now)
	}

	return buildKernelCheck(hs.Config.Kernel, kmsg.NewDefault(), edac.NewDefault(), time.Now,
		journal.NewDefault(), logger)
}

// kmsgSnapshotter reads the kernel buffer once, without waiting for more.
type kmsgSnapshotter interface {
	BootID() (string, error)
	Snapshot(ctx context.Context, fn func(kmsg.Record)) error
}

// buildKernelOneShot builds the kernel check for check --once: each run reads
// what the kernel ring buffer holds right now (never blocking, no cursor kept)
// and prints what the class table matches, whatever the stored cursor says;
// EDAC is judged against the stored counters. Nothing is saved and no
// background reader runs. An unreadable /dev/kmsg is reported as the run's
// error next to the EDAC readings.
func buildKernelOneShot(k *hcfg.Kernel, log kmsgSnapshotter, counters edacReader, now func() time.Time) checkSet {
	chk := kernel.New(log, edacSource{reader: counters}, health.NewRateMarks(), now, kernel.WithReplay())
	gate := &kernelGate{persister: runner.KVPersister{Key: kernelStateKey, State: chk}}
	run := func(ctx context.Context) health.CheckResult {
		readErr := log.Snapshot(ctx, func(r kmsg.Record) { chk.Consume(kernelRecord(r)) })
		res := chk.Run(ctx)
		if readErr != nil {
			res.Err = errors.Join(res.Err, fmt.Errorf("read the kernel log: %w", readErr))
		}

		return res
	}

	return checkSet{
		Checks:     []health.Check{newCheck(kernelCheckName, k.Common, gate.run(run))},
		Persisters: []runner.Persister{gate},
	}
}

// kmsgReader is the kernel log of pkg/host/kmsg.
type kmsgReader interface {
	BootID() (string, error)
	Run(ctx context.Context, fn func(kmsg.Record)) error
}

// edacReader is the EDAC counter read of pkg/host/edac.
type edacReader interface {
	Read() ([]edac.Counters, error)
}

// buildKernelCheck builds the kernel check. Its cursor, episodes and EDAC
// values are persisted under kernelStateKey; the kernel log reader is a
// background job, so it starts only after the stored state was restored and
// never under check --once (which judges EDAC and what is already buffered).
// A non-nil jr lets the first run after a reboot read the previous boot's
// journal tail; a missing journalctl is reported by the check, not fatal.
func buildKernelCheck(
	k *hcfg.Kernel, log kmsgReader, counters edacReader, now func() time.Time,
	jr journalReader, logger *zap.Logger,
) checkSet {
	// The check's own marks only back up Episode.LastStillAt, which is
	// persisted in kernel.state and already spaces "still happening" messages
	// across restarts; the runner's stored marks are not needed.
	opts := []kernel.Option{kernel.WithLogger(logger)}
	if jr != nil {
		opts = append(opts, kernel.WithJournal(journalSource{reader: jr}))
	}
	chk := kernel.New(log, edacSource{reader: counters}, health.NewRateMarks(), now, opts...)
	gate := &kernelGate{persister: runner.KVPersister{Key: kernelStateKey, State: chk}}
	reader := func(ctx context.Context) error {
		if err := log.Run(ctx, func(r kmsg.Record) { chk.Consume(kernelRecord(r)) }); err != nil {
			return fmt.Errorf("read the kernel log: %w", err)
		}

		return nil
	}

	return checkSet{
		Checks:     []health.Check{newCheck(kernelCheckName, k.Common, gate.run(chk.Run))},
		Persisters: []runner.Persister{gate},
		Background: []background{{Key: kmsgReadKey, Run: reader}},
	}
}

// kernelGate holds the kernel check back until its stored state has been
// restored. Without it a run while the store cannot be loaded would take the
// fresh-install path (commit the cursor, drain the buffered records), and the
// stored cursor restored later would lose those records or repeat "started"
// messages. The kernel log reader may start earlier: Consume only buffers.
type kernelGate struct {
	persister runner.Persister
	restored  atomic.Bool
}

// Restore implements runner.Persister. The gate opens even when the delegate
// fails: that failure is reported, and the check then starts afresh.
func (g *kernelGate) Restore(st store.State) error {
	err := g.persister.Restore(st)
	g.restored.Store(true)
	if err != nil {
		return fmt.Errorf("kernel check: %w", err)
	}

	return nil
}

// Save implements runner.Persister.
func (g *kernelGate) Save(st *store.State) error {
	if err := g.persister.Save(st); err != nil {
		return fmt.Errorf("kernel check: %w", err)
	}

	return nil
}

// run wraps the check's Run so it fails until Restore was called.
func (g *kernelGate) run(check func(context.Context) health.CheckResult) func(context.Context) health.CheckResult {
	return func(ctx context.Context) health.CheckResult {
		if !g.restored.Load() {
			return health.CheckResult{Err: errKernelStateNotLoaded}
		}

		return check(ctx)
	}
}

// noDrivesCommand is /retire while no disks check runs.
type noDrivesCommand struct {
	logger    *zap.Logger
	publisher command.Publisher
}

// Name implements telegram.Handler.
func (noDrivesCommand) Name() string { return "retire" }

// Enabled implements telegram.Handler.
func (noDrivesCommand) Enabled() bool { return true }

// Handle implements telegram.Handler.
func (c noDrivesCommand) Handle(ctx context.Context, senderID int64, _ string) error {
	if err := c.publisher.Publish(ctx, senderID, noDrivesReply); err != nil {
		return fmt.Errorf("failed to publish reply: %w", err)
	}

	return nil
}

// kernelRecord converts a kernel log record for the kernel check.
func kernelRecord(r kmsg.Record) kernel.Record {
	return kernel.Record(r)
}

// journalReader is the previous-boot journal read of pkg/host/journal.
type journalReader interface {
	Read(ctx context.Context, bootID string) ([]journal.Record, error)
}

// journalSource adapts pkg/host/journal to kernel.JournalSource, mapping its
// sentinels onto the kernel package's so the check never imports the host layer.
type journalSource struct {
	reader journalReader
}

// Read implements kernel.JournalSource. The records read are returned with
// kernel.ErrJournalCapped too.
func (s journalSource) Read(ctx context.Context, bootID string) ([]kernel.Record, error) {
	in, err := s.reader.Read(ctx, bootID)
	out := make([]kernel.Record, len(in))
	for i, r := range in {
		out[i] = kernel.Record{
			Facility: r.Facility,
			Level:    r.Priority,
			Time:     usecDuration(r.Usec),
			Message:  r.Message,
		}
	}

	switch {
	case errors.Is(err, journal.ErrUnavailable):
		return out, fmt.Errorf("%w: %w", kernel.ErrNoJournal, err)
	case errors.Is(err, journal.ErrCapped):
		return out, fmt.Errorf("%w: %w", kernel.ErrJournalCapped, err)
	case err != nil:
		return out, fmt.Errorf("read the journal: %w", err)
	}

	return out, nil
}

// usecDuration converts microseconds to a Duration, saturating instead of overflowing.
func usecDuration(usec uint64) time.Duration {
	const maxUsec = uint64(math.MaxInt64 / int64(time.Microsecond))
	if usec > maxUsec {
		usec = maxUsec
	}

	return time.Duration(usec) * time.Microsecond
}

// edacSource adapts pkg/host/edac to kernel.EDACSource.
type edacSource struct {
	reader edacReader
}

// Read implements kernel.EDACSource; no EDAC support stays nil, nil.
func (s edacSource) Read() ([]kernel.Counters, error) {
	in, err := s.reader.Read()
	if err != nil {
		return nil, fmt.Errorf("edac: %w", err)
	}
	if in == nil {
		return nil, nil
	}
	out := make([]kernel.Counters, len(in))
	for i := range in {
		out[i] = kernel.Counters(in[i])
	}

	return out, nil
}

// registerBackground starts the background jobs in OnStart and stops them in
// OnStop. Register it after registerStart so the stored state is restored
// before any job starts.
func registerBackground(lc fx.Lifecycle, jobs []background, raise func(key, text string), retry time.Duration) {
	if len(jobs) == 0 {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup

	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			for _, j := range jobs {
				wg.Go(func() { runBackground(ctx, j, raise, retry) })
			}

			return nil
		},
		OnStop: func(stopCtx context.Context) error {
			cancel()
			done := make(chan struct{})
			go func() {
				wg.Wait()
				close(done)
			}()
			select {
			case <-done:
			case <-stopCtx.Done():
			}

			return nil
		},
	})
}

// runBackground runs a job until ctx ends, restarting it after retry when it
// returns early; a failure is raised as a checker error first.
func runBackground(ctx context.Context, j background, raise func(key, text string), retry time.Duration) {
	for {
		err := j.Run(ctx)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			err = errors.New("stopped unexpectedly")
		}
		raise(j.Key, fmt.Sprintf("%v; retrying in %s", err, retry))

		timer := time.NewTimer(retry)
		select {
		case <-ctx.Done():
			timer.Stop()

			return
		case <-timer.C:
		}
	}
}
