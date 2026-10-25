// Package memory implements the memory early-warning check: available memory,
// swap use and memory pressure judged against configured levels, with the
// largest memory holder named in every problem. Reading the host is behind
// Source; the package performs no I/O of its own.
package memory

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Toshik1978/tgshd/pkg/health"
)

// Condition keys.
const (
	keyAvailable = "memory/available"
	keySwap      = "memory/swap"
	keyPressure  = "memory/pressure"
)

// Sample metric names.
const (
	metricAvailable = "memory.available_pct"
	metricSwap      = "memory.swap_pct"
	metricPressure  = "memory.pressure_some_avg60"
)

// kindUnreadable is the CheckError kind of a failed source read.
const kindUnreadable = "unreadable"

const gib = float64(1 << 30)

// ErrNoPressure marks a host without memory pressure information (PSI off).
var ErrNoPressure = errors.New("memory pressure information is unavailable")

// Host is one snapshot of host memory, in bytes, with the some avg60 and
// full avg10 memory pressure in percent. HasPressure is false when the host
// has no PSI. FullAvg10 (the share of time every task stalled) is shown by
// /status; the warning judges SomeAvg60.
type Host struct {
	Total, Available, SwapTotal, SwapFree uint64
	SomeAvg60, FullAvg10                  float64
	HasPressure                           bool
}

// Service is a container or systemd unit with its anonymous memory in bytes.
// Name is the display name (container name or unit name).
type Service struct {
	Name string
	Anon uint64
}

// Process is one host process with its resident set size in bytes.
type Process struct {
	Name string
	PID  int
	RSS  uint64
}

// Container is a Docker container as the limit audit sees it. Name is the
// container name; HasLimit is true when it runs under a memory limit.
type Container struct {
	Name     string
	Running  bool
	HasLimit bool
}

// ErrNoContainers is returned by Source.Containers on a host without a
// reachable container runtime; the audit then stays silent.
var ErrNoContainers = errors.New("no container runtime")

// Source reads the host. A zero Process (empty Name) means none was found.
type Source interface {
	Containers(ctx context.Context) ([]Container, error)
	Host(ctx context.Context) (Host, error)
	Services(ctx context.Context) ([]Service, error)
	TopProcess(ctx context.Context) (Process, error)
}

// Settings holds the warning levels, in percent.
type Settings struct {
	WarnAvailablePercent int
	WarnSwapPercent      int
	WarnPressurePercent  int

	// GrowthWindow and GrowthWarn drive the per-service growth warning; the
	// warning is off when either is zero. GrowthWarn is in bytes.
	GrowthWindow time.Duration
	GrowthWarn   uint64

	// AuditExclude lists containers allowed to run without a memory limit.
	AuditExclude []string

	// Now is the clock; time.Now when nil.
	Now func() time.Time
}

func (s Settings) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}

	return time.Now()
}

// Check is the memory check.
type Check struct {
	source   Source
	settings Settings
	growth   growth
	audit    audit
}

// New builds the check.
func New(source Source, settings Settings) *Check {
	return &Check{source: source, settings: settings}
}

// Run reads the host and judges available memory, swap and pressure. Each
// judgement is independent: a missing PSI file is one checker error and the
// others still run. A failed host read is Unknown for the whole run.
func (c *Check) Run(ctx context.Context) health.CheckResult {
	host, err := c.source.Host(ctx)
	if err != nil {
		return health.CheckResult{Err: health.CheckError{Subject: "host", Kind: kindUnreadable, Err: err}}
	}

	var res health.CheckResult
	top := &topLookup{ctx: ctx, source: c.source, host: host}

	c.judgeAvailable(&res, host, top)
	if host.SwapTotal > 0 {
		c.judgeSwap(&res, host, top)
	}
	if host.HasPressure {
		c.judgePressure(&res, host, top)
	} else {
		res.Err = errors.Join(res.Err, health.CheckError{Subject: "pressure", Kind: "unavailable", Err: ErrNoPressure})
	}
	c.judgeGrowth(ctx, &res, top)
	c.judgeLimits(ctx, &res, top)
	res.Err = errors.Join(res.Err, top.err())

	return res
}

func (c *Check) judgeAvailable(res *health.CheckResult, host Host, top *topLookup) {
	pct := percentOf(host.Available, host.Total)
	res.Samples = append(res.Samples, health.Sample{Metric: metricAvailable, Value: pct})

	reading := health.Reading{Key: keyAvailable, Verdict: health.Good, Text: percent(pct) + " available"}
	if pct < float64(c.settings.WarnAvailablePercent) {
		reading.Verdict = health.Bad
		reading.Text = fmt.Sprintf("%s available (%s of %s GB)%s",
			percent(pct), number(float64(host.Available)/gib), number(float64(host.Total)/gib), top.suffix())
	}
	res.Readings = append(res.Readings, reading)
}

func (c *Check) judgeSwap(res *health.CheckResult, host Host, top *topLookup) {
	used := host.SwapTotal - min(host.SwapFree, host.SwapTotal)
	pct := percentOf(used, host.SwapTotal)
	res.Samples = append(res.Samples, health.Sample{Metric: metricSwap, Value: pct})

	reading := health.Reading{Key: keySwap, Verdict: health.Good, Text: "swap " + percent(pct) + " used"}
	if pct > float64(c.settings.WarnSwapPercent) {
		reading.Verdict = health.Bad
		reading.Text = fmt.Sprintf("swap %s used (%s of %s GB)%s",
			percent(pct), number(float64(used)/gib), number(float64(host.SwapTotal)/gib), top.suffix())
	}
	res.Readings = append(res.Readings, reading)
}

func (c *Check) judgePressure(res *health.CheckResult, host Host, top *topLookup) {
	res.Samples = append(res.Samples, health.Sample{Metric: metricPressure, Value: host.SomeAvg60})

	reading := health.Reading{Key: keyPressure, Verdict: health.Good, Text: "pressure " + percent(host.SomeAvg60)}
	if host.SomeAvg60 > float64(c.settings.WarnPressurePercent) {
		reading.Verdict = health.Bad
		reading.Text = "pressure " + percent(host.SomeAvg60) + " (stalled, 1 min avg)" + top.suffix()
	}
	res.Readings = append(res.Readings, reading)
}

// topLookup finds the top consumer once per run, and only when a problem
// needs it.
type topLookup struct {
	ctx    context.Context //nolint:containedctx // scoped to one Run call
	source Source
	host   Host

	done bool
	name string
	errs []error
}

// suffix is "; top: <consumer>", or empty when no consumer is known.
func (t *topLookup) suffix() string {
	if !t.done {
		t.done = true
		t.name = t.find()
	}
	if t.name == "" {
		return ""
	}

	return "; top: " + t.name
}

func (t *topLookup) err() error { return errors.Join(t.errs...) }

// find returns "web-app 31 GB" for a service holding most of the used memory,
// otherwise "python3 (pid 4242) 40 GB" for the largest process.
func (t *topLookup) find() string {
	services, err := t.source.Services(t.ctx)
	if err != nil {
		t.errs = append(t.errs, health.CheckError{Subject: "services", Kind: kindUnreadable, Err: err})
	}

	var best Service
	for _, s := range services {
		if s.Anon > best.Anon {
			best = s
		}
	}
	used := t.host.Total - min(t.host.Available, t.host.Total)
	if best.Name != "" && best.Anon*2 > used {
		return best.Name + " " + size(best.Anon)
	}

	proc, err := t.source.TopProcess(t.ctx)
	if err != nil {
		t.errs = append(t.errs, health.CheckError{Subject: "processes", Kind: kindUnreadable, Err: err})
	}
	if proc.Name != "" {
		return proc.Name + " (pid " + strconv.Itoa(proc.PID) + ") " + size(proc.RSS)
	}
	if best.Name != "" {
		return best.Name + " " + size(best.Anon)
	}

	return ""
}

func percentOf(part, whole uint64) float64 {
	if whole == 0 {
		return 0
	}

	return float64(part) * 100 / float64(whole)
}

func percent(v float64) string { return strconv.FormatFloat(v, 'f', 0, 64) + "%" }

// size is "31 GB" or "5.6 GB".
func size(b uint64) string { return number(float64(b)/gib) + " GB" }

// number shows one decimal below 10 and none above; a whole value has no decimal.
func number(v float64) string {
	if one := strconv.FormatFloat(v, 'f', 1, 64); v < 10 && one != "10.0" {
		return strings.TrimSuffix(one, ".0")
	}

	return strconv.FormatFloat(v, 'f', 0, 64)
}
