// Package memguard is the pure model of the fast memory guard: which services
// it may touch, their recent memory history and which one is the culprit when
// the host runs out of memory. It performs no I/O; the sampler and the actuator
// that feed and act on it live behind interfaces.
package memguard

import (
	"slices"
	"strings"
	"time"

	"github.com/Toshik1978/tgshd/pkg/health"
)

// ServiceInterval is how often the sampler reads per-service memory. The
// per-service history is sized from it.
const ServiceInterval = 5 * time.Second

// Kind tells a docker container from a systemd unit.
type Kind string

const (
	// KindContainer is a docker container (docker-<id>.scope).
	KindContainer Kind = "container"
	// KindUnit is a systemd service (*.service).
	KindUnit Kind = "unit"
)

// Config is what the guard model needs from the memory configuration.
type Config struct {
	// CulpritWindow is how far back the guard looks for a service's minimum.
	CulpritWindow time.Duration
	// CulpritMinGrowth is the smallest growth, in bytes, that makes a culprit.
	CulpritMinGrowth uint64
	// NeverTouch lists container names or unit names the guard must not act
	// on; such services are Protected.
	NeverTouch []string
	// SelfCgroup is tgshd's own cgroup path relative to the cgroup root; the
	// service that holds it is never eligible.
	SelfCgroup string
	// Mode is act, report or off; off never starts the loop, anything but
	// act reports only.
	Mode Mode
	// DangerAvailablePercent and DangerPressurePercent define danger:
	// available memory below the floor and full avg10 pressure at or above the
	// level, both continuously for DangerFor.
	DangerAvailablePercent float64
	DangerPressurePercent  float64
	DangerFor              time.Duration
	// RepeatWindow turns a restart into a stop when tgshd acted on the same
	// service within it; 0 disables the stop rule.
	RepeatWindow time.Duration
	// RecoveryWindow is how long the guard waits after acting before judging
	// whether memory came back, and how long danger must be clear to end an
	// episode that could not be fixed.
	RecoveryWindow time.Duration
	// ActionTimeout bounds a restart or stop; past it the loop kills the
	// service's cgroup instead.
	ActionTimeout time.Duration
	// Channel delivers restarted and the would_* previews (the memory check's
	// mode); FailedChannel delivers every outcome where tgshd did not fix the
	// problem (guard.failed_mode).
	Channel, FailedChannel health.Mode
}

// Service is one service the guard watches.
type Service struct {
	// ID is the stable key: container:<name> or unit:<name.service>.
	ID   string
	Kind Kind
	// Name is the container name or the unit name.
	Name string
	// CgroupPath is relative to the cgroup root.
	CgroupPath string
	// Eligible is true when the guard may act on the service.
	Eligible bool
	// Protected is true when the service is excluded by never_touch; it is
	// reported as protected instead of being skipped silently.
	Protected bool
}

// Sample is one service's memory at a sample, as the sampler reads it.
type Sample struct {
	Kind       Kind
	Name       string
	CgroupPath string
	// Anon is memory.stat anon + shmem, in bytes.
	Anon uint64
	// Current is memory.current, in bytes.
	Current uint64
}

// Service builds the service for a cgroup and judges its eligibility
// (research R5): only *.service and *.scope cgroups under system.slice are
// services; tgshd's own, the core units and systemd-* are never eligible;
// never_touch names are protected.
func (c Config) Service(kind Kind, name, cgroupPath string) Service {
	path := strings.Trim(cgroupPath, "/")
	s := Service{ID: string(kind) + ":" + name, Kind: kind, Name: name, CgroupPath: cgroupPath}
	if !isServiceCgroup(path) || c.holdsSelf(path) || (kind == KindUnit && isCoreUnit(name)) {
		return s
	}
	if slices.Contains(c.NeverTouch, name) {
		s.Protected = true

		return s
	}
	s.Eligible = true

	return s
}

// isServiceCgroup reports whether path is a service or scope under
// system.slice. A slice is never a service: acting on it would hit every
// service inside.
func isServiceCgroup(path string) bool {
	rest, ok := strings.CutPrefix(path, "system.slice/")
	if !ok {
		return false
	}
	last := rest[strings.LastIndexByte(rest, '/')+1:]

	return strings.HasSuffix(last, ".service") || strings.HasSuffix(last, ".scope")
}

func (c Config) holdsSelf(path string) bool {
	self := strings.Trim(c.SelfCgroup, "/")
	if self == "" {
		return false
	}
	rest, ok := strings.CutPrefix(self, path)

	return ok && (rest == "" || rest[0] == '/')
}

// isCoreUnit reports whether name is a unit the guard never touches, whatever
// the configuration says.
func isCoreUnit(name string) bool {
	switch name {
	case "docker.service", "containerd.service", "dbus.service", "ssh.service", "sshd.service":
		return true
	}

	return strings.HasPrefix(name, "systemd-")
}

type serviceKey struct {
	kind Kind
	name string
}

type point struct {
	at   time.Time
	anon uint64
}

// ring is a preallocated history of one service's samples.
type ring struct {
	points []point
	next   int
	n      int
}

func newRing(depth int) ring {
	return ring{points: make([]point, depth)}
}

func (r *ring) add(p point) {
	r.points[r.next] = p
	r.next = (r.next + 1) % len(r.points)
	if r.n < len(r.points) {
		r.n++
	}
}

func (r *ring) reset() {
	r.next, r.n = 0, 0
}

// latest returns the newest point; the ring must not be empty.
func (r *ring) latest() point {
	return r.points[(r.next-1+len(r.points))%len(r.points)]
}

// entry is one service present at the latest sample. Services that are
// neither eligible nor protected are kept too, so they are not judged again on
// every sample, but they have no history.
type entry struct {
	service Service
	history ring
	seen    uint64
}

func (e *entry) tracked() bool {
	return e.service.Eligible || e.service.Protected
}

// Guard holds the per-service memory history of the eligible and protected
// services. It is not safe for concurrent use.
type Guard struct {
	cfg     Config
	depth   int
	entries map[serviceKey]*entry
	gen     uint64

	ep          Episode
	dangerSince time.Time
	clearSince  time.Time
	records     []Record
}

// New creates the guard model. Each service's history is
// CulpritWindow / ServiceInterval samples deep.
func New(cfg Config) *Guard {
	return &Guard{
		cfg:     cfg,
		depth:   max(1, int(cfg.CulpritWindow/ServiceInterval)),
		entries: make(map[serviceKey]*entry),
		ep:      Episode{State: StateIdle},
	}
}

// ObserveServices records one sample of every service present at at. A
// service missing from samples is dropped with its history; a service whose
// cgroup changed (a recreated container) is judged again and starts a new
// history. Services that are neither eligible nor protected keep no history.
// It must be called at most once per ServiceInterval, as the loop does: the
// history is sized for that spacing, and faster calls would shorten the
// culprit window it covers.
func (g *Guard) ObserveServices(at time.Time, samples []Sample) {
	g.gen++
	for i := range samples {
		s := &samples[i]
		key := serviceKey{kind: s.Kind, name: s.Name}
		e, ok := g.entries[key]
		if !ok || e.service.CgroupPath != s.CgroupPath {
			if !ok {
				e = &entry{}
				g.entries[key] = e
			}
			e.service = g.cfg.Service(s.Kind, s.Name, s.CgroupPath)
			if e.tracked() && e.history.points == nil {
				e.history = newRing(g.depth)
			}
			e.history.reset()
		}
		e.seen = g.gen
		if e.tracked() {
			e.history.add(point{at: at, anon: s.Anon})
		}
	}
	for key, e := range g.entries {
		if e.seen != g.gen {
			delete(g.entries, key)
		}
	}
}
