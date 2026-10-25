package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	dockerclient "github.com/moby/moby/client"
	"go.uber.org/fx"
	"go.uber.org/zap"

	"github.com/Toshik1978/tgshd/pkg/health"
	hcfg "github.com/Toshik1978/tgshd/pkg/health/config"
	"github.com/Toshik1978/tgshd/pkg/health/memguard"
	"github.com/Toshik1978/tgshd/pkg/health/report"
	"github.com/Toshik1978/tgshd/pkg/health/runner"
	"github.com/Toshik1978/tgshd/pkg/host/cgroupmem"
	"github.com/Toshik1978/tgshd/pkg/host/procmem"
	"github.com/Toshik1978/tgshd/pkg/host/systemctl"
)

const (
	// memoryCheckName names the memory section's events and checker errors.
	memoryCheckName = "memory"
	// memoryGuardKey is the kv key of the guard's intervention records.
	memoryGuardKey = "memory.guard"
	// memoryGrowthKey is the kv key of the memory check's growth history.
	memoryGrowthKey = "memory.growth"
	// memoryTopServices is how many services the /status memory block lists.
	memoryTopServices = 3
	// guardNotRunning is the /status guard mode of a guard that could not start.
	guardNotRunning = "not running"

	// Checker errors of the guard's start.
	guardShieldKey = memoryCheckName + "/guard/shield"
	guardCgroupKey = memoryCheckName + "/guard/cgroup"
	guardHostKey   = memoryCheckName + "/guard/host"
	guardSelfKey   = memoryCheckName + "/guard/self"

	// systemdBusError is what systemctl prints when systemd cannot be reached.
	systemdBusError = "Failed to connect to"
)

// errDockerUnavailable is wrapped by lazyDocker when it cannot build the
// Docker client.
var errDockerUnavailable = errors.New("docker client unavailable")

// guardMemory is what the guard reads of pkg/host/procmem.
type guardMemory interface {
	ReadMemInfo() (procmem.MemInfo, error)
	ReadPressure() (procmem.Pressure, error)
	TopProcesses(n int) ([]procmem.Process, error)
}

// guardCgroups is what the guard uses of pkg/host/cgroupmem.
type guardCgroups interface {
	Services() ([]cgroupmem.Service, error)
	Self() (string, error)
	Kill(cgroupPath string) error
}

// containerLister lists the containers as an id→name map.
type containerLister interface {
	ContainerNames(ctx context.Context) (map[string]string, error)
}

// guardContainers is what the guard uses of Docker: the id→name map and the
// actions.
type guardContainers interface {
	containerLister
	Restart(ctx context.Context, name string) error
	Stop(ctx context.Context, name string) error
}

// guardUnits is what the guard uses of pkg/host/systemctl.
type guardUnits interface {
	Restart(ctx context.Context, unit string) error
	Stop(ctx context.Context, unit string) error
}

// guardShield is the starvation shielding of pkg/host/procmem.
type guardShield interface {
	Apply() error
}

// guardHost holds the host adapters the memory guard runs over.
type guardHost struct {
	mem        guardMemory
	cgroups    guardCgroups
	containers guardContainers
	units      guardUnits
	shield     guardShield
	clock      memguard.Clock
	// names is the container id→name map the memory check refreshes.
	names *containerNames
}

// newGuardHost provides the real host adapters of the memory guard. It reads
// and touches nothing until the guard starts.
func newGuardHost(d *lazyDocker, names *containerNames) *guardHost {
	return &guardHost{
		names:      names,
		mem:        procmem.NewDefault(),
		cgroups:    cgroupmem.NewDefault(),
		containers: d,
		units:      systemctl.NewDefault(),
		shield:     procmem.NewShield(),
		clock:      realClock{},
	}
}

// realClock is the wall clock.
type realClock struct{}

// Now implements memguard.Clock.
func (realClock) Now() time.Time { return time.Now() }

// After implements memguard.Clock.
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// guardRunner is what the guard uses of the health runner.
type guardRunner interface {
	PushMode(ctx context.Context, check, key, text, record string, mode health.Mode)
	CheckerError(key, text string)
}

// memoryGuard is the fast memory guard of the daemon (research R3–R11): it
// runs beside the runner, never inside its tick, so neither a slow tick nor a
// quiet window holds it back.
type memoryGuard struct {
	cfg   memguard.Config
	host  *guardHost
	state *guardState
	log   *zap.Logger
	// mode is the mode /status shows: the configured one until the guard
	// starts, then the one it runs in, or guardNotRunning.
	mode atomic.Pointer[string]
}

// newMemoryGuard builds the guard of the memory section; nil without the
// section, with intervene off or without host adapters.
func newMemoryGuard(m *hcfg.Memory, host *guardHost, logger *zap.Logger) *memoryGuard {
	if m == nil || m.Guard.Intervene == hcfg.InterveneOff || host == nil {
		return nil
	}
	g := m.Guard

	guard := &memoryGuard{
		cfg: memguard.Config{
			CulpritWindow:          g.CulpritWindow.Duration(),
			CulpritMinGrowth:       g.CulpritMinGrowth.Bytes(),
			NeverTouch:             g.NeverTouch,
			Mode:                   memguard.Mode(g.Intervene),
			DangerAvailablePercent: float64(g.DangerAvailablePercent),
			DangerPressurePercent:  float64(g.DangerPressurePercent),
			DangerFor:              g.DangerFor.Duration(),
			RepeatWindow:           g.RepeatWindow.Duration(),
			RecoveryWindow:         g.RecoveryWindow.Duration(),
			ActionTimeout:          g.ActionTimeout.Duration(),
			Channel:                health.Mode(m.Mode),
			FailedChannel:          health.Mode(g.FailedMode),
		},
		host:  host,
		state: &guardState{log: logger},
		log:   logger,
	}
	guard.setMode(string(guard.cfg.Mode))

	return guard
}

func (g *memoryGuard) setMode(mode string) { g.mode.Store(&mode) }

// status is the guard line of /status: the mode and the latest intervention.
func (g *memoryGuard) status() report.Guard {
	s := report.Guard{Mode: *g.mode.Load()}
	if rec, ok := g.state.last(); ok {
		s.Last = &report.GuardAction{
			Action: string(rec.Action), Service: rec.Name, Report: rec.Mode != memguard.ModeAct, At: rec.At,
		}
	}

	return s
}

// persister keeps the intervention records in the checkpoint as kv
// memory.guard.
//
//nolint:ireturn // the runner takes its persisters as runner.Persister.
func (g *memoryGuard) persister() runner.Persister {
	return runner.KVPersister{Key: memoryGuardKey, State: g.state}
}

// register starts the guard in OnStart and stops it in OnStop. Register it
// after registerStart, so the stored records are restored before the loop
// runs.
func (g *memoryGuard) register(lc fx.Lifecycle, r guardRunner) {
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup

	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			wg.Go(func() { g.run(ctx, r) })

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

// run shields the daemon, checks that the host can be guarded and runs the
// guard loop until ctx is done. Every start failure is a checker error. The
// failures of a guard that still runs (shielding, its own cgroup) are raised
// on their own goroutine: a checker error goes out over Telegram and email,
// and an unreachable network must never hold the guard back (FR-008).
func (g *memoryGuard) run(ctx context.Context, r guardRunner) {
	errs := g.applyShield()

	sampler := guardSampler{mem: g.host.mem, cgroups: g.host.cgroups, names: g.host.names}
	if _, err := g.host.cgroups.Services(); err != nil {
		errs = append(errs, startError{
			guardCgroupKey, "no cgroup v2 service cgroups, the memory guard is not started: " + err.Error(),
		})
		g.setMode(guardNotRunning)
		raiseAll(r, errs)

		return
	}
	if _, err := sampler.Host(); err != nil {
		errs = append(errs, startError{
			guardHostKey, "cannot read host memory, the memory guard is not started: " + err.Error(),
		})
		g.setMode(guardNotRunning)
		raiseAll(r, errs)

		return
	}
	cfg, selfErr := g.withSelf()
	if selfErr != nil {
		errs = append(errs, *selfErr)
	}
	g.setMode(string(cfg.Mode))

	var wg sync.WaitGroup
	defer wg.Wait()
	wg.Go(func() { raiseAll(r, errs) })
	wg.Go(func() { g.refreshNames(ctx) })

	loop := memguard.NewLoop(cfg, g.host.clock, sampler,
		guardActuator{containers: g.host.containers, units: g.host.units, cgroups: g.host.cgroups},
		guardReporter{runner: r, state: g.state}, memguard.WithLogger(g.log))
	loop.Restore(g.state.start())
	g.log.Info("Memory guard started", zap.String("intervene", string(cfg.Mode)))
	loop.Run(ctx)
}

// raiseAll raises errs as checker errors, in order.
func raiseAll(r guardRunner, errs []startError) {
	for _, e := range errs {
		r.CheckerError(e.Key, e.Text)
	}
}

// applyShield protects the daemon from the OOM killer and from paging and
// returns each failure as its own checker error.
func (g *memoryGuard) applyShield() []startError {
	err := g.host.shield.Apply()
	if err == nil {
		return nil
	}
	errs := []error{err}
	var joined interface{ Unwrap() []error }
	if errors.As(err, &joined) {
		errs = joined.Unwrap()
	}
	out := make([]startError, 0, len(errs))
	for i, e := range errs {
		out = append(out, startError{
			fmt.Sprintf("%s/%d", guardShieldKey, i+1), "cannot shield tgshd from memory starvation: " + e.Error(),
		})
	}

	return out
}

// withSelf returns the guard configuration with tgshd's own cgroup. When it
// is unknown (unreadable, or the root cgroup), the guard could restart the
// service tgshd itself runs in, so act falls back to report, with the
// checker error that says so.
func (g *memoryGuard) withSelf() (memguard.Config, *startError) {
	cfg := g.cfg
	self, err := g.host.cgroups.Self()
	if err == nil && strings.Trim(self, "/") != "" {
		cfg.SelfCgroup = self

		return cfg, nil
	}
	reason := "it runs in the root cgroup"
	if err != nil {
		reason = err.Error()
	}
	g.log.Warn("Memory guard cannot tell its own cgroup", zap.String("reason", reason))
	if cfg.Mode != memguard.ModeAct {
		return cfg, nil
	}
	cfg.Mode = memguard.ModeReport

	return cfg, &startError{
		guardSelfKey,
		"cannot tell tgshd's own cgroup (" + reason + "); the memory guard only reports",
	}
}

// refreshNames refreshes the container id→name map from the Docker list at
// once and then every runner tick, until ctx is done. It runs whatever the
// memory check's schedule: a quiet window pauses the check, never the guard,
// and containers redeployed in maintenance must be guarded too.
func (g *memoryGuard) refreshNames(ctx context.Context) {
	names := &nameRefresher{names: g.host.names, lister: g.host.containers, log: g.log, who: "Memory guard"}
	for {
		names.refresh(ctx)
		select {
		case <-ctx.Done():
			return
		case <-g.host.clock.After(runner.Tick):
		}
	}
}

// nameRefresher refreshes the container id→name map from the Docker list,
// off the guard's decision path, and warns once while the list keeps
// failing. The guard runs one every tick; the memory check runs one on each
// of its runs too.
type nameRefresher struct {
	names  *containerNames
	lister containerLister
	log    *zap.Logger
	who    string

	mu      sync.Mutex
	failing bool
}

// refresh lists the containers now; a failed list keeps the last map.
func (r *nameRefresher) refresh(ctx context.Context) {
	err := r.names.refresh(ctx, r.lister)
	if ctx.Err() != nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if err != nil && !r.failing {
		r.log.Warn(r.who+" cannot list containers; new containers are not named (nor guarded) until it can",
			zap.Error(err))
	}
	r.failing = err != nil
}

// containerNames maps container ids to names. It is replaced whole and read
// lock-free by the guard loop.
type containerNames struct {
	ids atomic.Pointer[map[string]string]
}

// newContainerNames provides the container id→name map the memory check
// refreshes and the guard reads.
func newContainerNames() *containerNames { return &containerNames{} }

// refresh replaces the map with the containers src lists now; a failed list
// keeps the last one.
func (c *containerNames) refresh(ctx context.Context, src containerLister) error {
	names, err := src.ContainerNames(ctx)
	if err != nil {
		return fmt.Errorf("refresh container names: %w", err)
	}
	c.ids.Store(&names)

	return nil
}

// name returns the name of the container with id.
func (c *containerNames) name(id string) (string, bool) {
	m := c.ids.Load()
	if m == nil {
		return "", false
	}
	name, ok := (*m)[id]

	return name, ok
}

// guardSampler reads the host for the guard loop over procmem and cgroupmem.
// Every string it returns is owned by its sample, never a view of a reused
// buffer.
type guardSampler struct {
	mem     guardMemory
	cgroups guardCgroups
	names   *containerNames
}

// Host implements memguard.Sampler.
func (s guardSampler) Host() (memguard.Host, error) {
	info, err := s.mem.ReadMemInfo()
	if err != nil {
		return memguard.Host{}, fmt.Errorf("read host memory: %w", err)
	}
	if info.Total == 0 {
		return memguard.Host{}, errors.New("read host memory: MemTotal is 0")
	}
	psi, err := s.mem.ReadPressure()
	if err != nil {
		return memguard.Host{}, fmt.Errorf("read memory pressure: %w", err)
	}

	return memguard.Host{
		AvailablePct: float64(info.Available) * 100 / float64(info.Total),
		FullAvg10:    psi.FullAvg10,
	}, nil
}

// Services implements memguard.Sampler. A container whose name Docker has not
// told yet is left out: never_touch names containers, so it could not be
// judged.
func (s guardSampler) Services(dst []memguard.Sample) ([]memguard.Sample, error) {
	svcs, err := s.cgroups.Services()
	if err != nil {
		return dst, fmt.Errorf("read service cgroups: %w", err)
	}
	for i := range svcs {
		svc := &svcs[i]
		sample := memguard.Sample{
			Kind: memguard.KindUnit, Name: svc.Name, CgroupPath: svc.Path, Anon: svc.Anon, Current: svc.Current,
		}
		if svc.Kind == cgroupmem.KindContainer {
			name, ok := s.names.name(svc.Name)
			if !ok {
				continue
			}
			sample.Kind, sample.Name = memguard.KindContainer, name
		}
		dst = append(dst, sample)
	}

	return dst, nil
}

// TopProcess implements memguard.Sampler; it keeps only the largest process.
func (s guardSampler) TopProcess() (memguard.Top, error) {
	procs, err := s.mem.TopProcesses(1)
	if err != nil {
		return memguard.Top{}, fmt.Errorf("read top process: %w", err)
	}
	if len(procs) == 0 {
		return memguard.Top{}, nil
	}
	p := procs[0]

	return memguard.Top{Name: p.Name, PID: p.PID, Size: p.RSS, Slice: p.Cgroup}, nil
}

// guardActuator restarts and stops services through Docker or systemctl and
// kills their cgroup when the runtime does not answer.
type guardActuator struct {
	containers guardContainers
	units      guardUnits
	cgroups    guardCgroups
}

// Restart implements memguard.Actuator.
func (a guardActuator) Restart(ctx context.Context, s memguard.Service) error {
	if s.Kind == memguard.KindContainer {
		return containerError(a.containers.Restart(ctx, s.Name))
	}

	return unitError(a.units.Restart(ctx, s.Name))
}

// Stop implements memguard.Actuator.
func (a guardActuator) Stop(ctx context.Context, s memguard.Service) error {
	if s.Kind == memguard.KindContainer {
		return containerError(a.containers.Stop(ctx, s.Name))
	}

	return unitError(a.units.Stop(ctx, s.Name))
}

// Kill implements memguard.Actuator. Only a cgroup whose last element is a
// .service or a .scope is killed: a slice holds many services.
func (a guardActuator) Kill(cgroupPath string) error {
	p := path.Clean(strings.Trim(cgroupPath, "/"))
	last := path.Base(p)
	if cgroupPath == "" || p == "." || p == ".." || strings.HasPrefix(p, "../") ||
		!strings.HasSuffix(last, ".service") && !strings.HasSuffix(last, ".scope") {
		return fmt.Errorf("refusing to kill cgroup %q: not a service or a scope", cgroupPath)
	}
	if err := a.cgroups.Kill(p); err != nil {
		return fmt.Errorf("kill cgroup: %w", err)
	}

	return nil
}

// containerError marks a Docker daemon that cannot be reached as
// memguard.ErrUnreachable.
func containerError(err error) error {
	if err != nil && (errors.Is(err, errDockerUnavailable) || dockerclient.IsErrConnectionFailed(err)) {
		return fmt.Errorf("%w: %w", memguard.ErrUnreachable, err)
	}

	return err
}

// unitError marks a missing systemctl or an unreachable systemd as
// memguard.ErrUnreachable.
func unitError(err error) error {
	if err != nil && (errors.Is(err, exec.ErrNotFound) || strings.Contains(err.Error(), systemdBusError)) {
		return fmt.Errorf("%w: %w", memguard.ErrUnreachable, err)
	}

	return err
}

// guardReporter pushes the guard's outcomes as events of the memory check,
// logged in the alert log, and keeps its records for the checkpoint.
type guardReporter struct {
	runner guardRunner
	state  *guardState
}

// Report implements memguard.Reporter.
func (r guardReporter) Report(ctx context.Context, _ memguard.Outcome, m memguard.Message) {
	r.runner.PushMode(ctx, memoryCheckName, m.Key, m.Text, m.Text, m.Mode)
}

// Save implements memguard.Reporter.
func (r guardReporter) Save(ctx context.Context, records []memguard.Record) {
	r.state.Save(ctx, records)
}

// guardState holds the intervention records between the guard loop and the
// checkpoint (runner.KVState). The loop takes the restored records once, when
// it starts; records restored later (the store could not be read at start)
// are kept for the checkpoint until the loop saves its own, but the running
// loop goes on without them.
type guardState struct {
	log *zap.Logger

	mu       sync.Mutex
	records  []memguard.Record
	started  bool
	loopSave bool
}

// Save keeps the loop's records; ctx is not needed for an in-memory copy.
func (s *guardState) Save(_ context.Context, records []memguard.Record) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.records, s.loopSave = records, true
}

// Export implements runner.KVState.
func (s *guardState) Export() (string, error) {
	s.mu.Lock()
	records := s.records
	s.mu.Unlock()

	if records == nil {
		records = []memguard.Record{}
	}
	data, err := json.Marshal(records)
	if err != nil {
		return "", fmt.Errorf("encode memory guard records: %w", err)
	}

	return string(data), nil
}

// Restore implements runner.KVState.
func (s *guardState) Restore(data string) error {
	var records []memguard.Record
	if data != "" {
		if err := json.Unmarshal([]byte(data), &records); err != nil {
			return fmt.Errorf("decode memory guard records: %w", err)
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.started {
		s.log.Warn("Memory guard records restored after the guard started; it goes on without them",
			zap.Int("records", len(records)))
	}
	if !s.loopSave {
		s.records = records
	}

	return nil
}

// last returns the latest record held.
func (s *guardState) last() (memguard.Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var latest memguard.Record
	for _, r := range s.records {
		if r.At.After(latest.At) {
			latest = r
		}
	}

	return latest, !latest.At.IsZero()
}

// start marks the loop started and returns the records restored so far.
func (s *guardState) start() []memguard.Record {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.started = true

	return s.records
}

// restored returns the records held now.
func (s *guardState) restored() []memguard.Record {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.records
}
