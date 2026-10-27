package main

import (
	"context"
	"errors"
	"fmt"

	dockerclient "github.com/moby/moby/client"
	"go.uber.org/zap"

	"github.com/Toshik1978/tgshd/pkg/health"
	"github.com/Toshik1978/tgshd/pkg/health/check/memory"
	hcfg "github.com/Toshik1978/tgshd/pkg/health/config"
	"github.com/Toshik1978/tgshd/pkg/health/runner"
	"github.com/Toshik1978/tgshd/pkg/host/cgroupmem"
	"github.com/Toshik1978/tgshd/pkg/host/procmem"
)

// memoryChecks provides the memory check when the memory section is present.
// Each run also refreshes the container id→name map the guard reads; the
// guard refreshes it on its own as well. open is the engine's condition state
// the limit audit asks whether its PROBLEM is open.
func memoryChecks(
	hs *healthSetup, d *lazyDocker, names *containerNames, open *openState, logger *zap.Logger,
) checkSet {
	if hs.Config == nil || hs.Config.Memory == nil {
		return checkSet{}
	}
	src := memorySource{
		sampler:    guardSampler{mem: procmem.NewDefault(), cgroups: cgroupmem.NewDefault(), names: names},
		containers: d,
	}

	return buildMemoryCheck(hs.Config.Memory, src, d, open, logger)
}

// buildMemoryCheck builds the memory check over src. Its growth history is
// persisted under memoryGrowthKey and its figures feed /status and the
// weekly summary. The limit audit reads open (bound to the runner, or to the
// stored state under check --once) at run time, when it reflects the
// previous tick.
func buildMemoryCheck(
	m *hcfg.Memory, src memorySource, lister containerLister, open *openState, logger *zap.Logger,
) checkSet {
	chk := memory.New(src, memory.Settings{
		WarnAvailablePercent: m.WarnAvailablePercent,
		WarnSwapPercent:      m.WarnSwapPercent,
		WarnPressurePercent:  m.WarnPressurePercent,
		GrowthWindow:         m.GrowthWindow.Duration(),
		GrowthWarn:           m.GrowthWarn.Bytes(),
		GrowthExclude:        m.GrowthExclude,
		AuditExclude:         m.AuditExclude,
		Open:                 open.Problem,
	})
	names := &nameRefresher{names: src.sampler.names, lister: lister, log: logger, who: "Memory check"}
	common := hcfg.Common{
		Interval: m.Interval, Threshold: m.Threshold, Mode: m.Mode, PauseInQuietWindows: m.PauseInQuietWindows,
	}
	run := func(ctx context.Context) health.CheckResult {
		names.refresh(ctx)

		return chk.Run(ctx)
	}

	return checkSet{
		Checks: []health.Check{newCheck(memoryCheckName, common, run)},
		Persisters: []runner.Persister{
			runner.KVPersister{Key: memoryGrowthKey, State: bytesKV{state: chk}},
			runner.KVPersister{Key: memoryAuditKey, State: bytesKV{state: chk.AuditState()}},
		},
		Memory: chk,
	}
}

// containerAuditor lists the containers with their memory limits for the
// limit audit.
type containerAuditor interface {
	AuditContainers(ctx context.Context) ([]memory.Container, error)
}

// memorySource reads the host for the memory check over procmem and
// cgroupmem, naming containers as the guard does. The container list is only
// ever read by the check, never by the guard's decision path.
type memorySource struct {
	sampler    guardSampler
	containers containerAuditor
}

// Containers implements memory.Source. A host without a reachable Docker has
// nothing to audit.
func (s memorySource) Containers(ctx context.Context) ([]memory.Container, error) {
	if s.containers == nil {
		return nil, memory.ErrNoContainers
	}
	list, err := s.containers.AuditContainers(ctx)
	switch {
	case errors.Is(err, errDockerUnavailable) || dockerclient.IsErrConnectionFailed(err):
		return nil, fmt.Errorf("%w: %w", memory.ErrNoContainers, err)
	case err != nil:
		return nil, fmt.Errorf("list containers: %w", err)
	}

	return list, nil
}

// Host implements memory.Source. A host without PSI has no pressure; any
// other failure to read it fails the read.
func (s memorySource) Host(context.Context) (memory.Host, error) {
	info, err := s.sampler.mem.ReadMemInfo()
	if err != nil {
		return memory.Host{}, fmt.Errorf("read host memory: %w", err)
	}
	host := memory.Host{
		Total:     info.Total,
		Available: info.Available,
		SwapTotal: info.SwapTotal,
		SwapFree:  info.SwapFree,
	}
	psi, err := s.sampler.mem.ReadPressure()
	switch {
	case errors.Is(err, procmem.ErrNoPressure):
		return host, nil
	case err != nil:
		return memory.Host{}, fmt.Errorf("read memory pressure: %w", err)
	}
	host.SomeAvg60, host.FullAvg10, host.HasPressure = psi.SomeAvg60, psi.FullAvg10, true

	return host, nil
}

// Services implements memory.Source: the services the guard samples, by
// display name.
func (s memorySource) Services(context.Context) ([]memory.Service, error) {
	samples, err := s.sampler.Services(nil)
	if err != nil {
		return nil, err
	}
	out := make([]memory.Service, len(samples))
	for i, sm := range samples {
		out[i] = memory.Service{Name: sm.Name, Anon: sm.Anon}
	}

	return out, nil
}

// TopProcess implements memory.Source.
func (s memorySource) TopProcess(context.Context) (memory.Process, error) {
	top, err := s.sampler.TopProcess()
	if err != nil {
		return memory.Process{}, err
	}

	return memory.Process{Name: top.Name, PID: top.PID, RSS: top.Size}, nil
}
