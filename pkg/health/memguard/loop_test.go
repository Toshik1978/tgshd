package memguard

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/Toshik1978/tgshd/pkg/health"
)

// failsafe bounds a wait that only a blocked loop could exceed; it is never a
// timing assumption of the guard itself.
const failsafe = 5 * time.Second

// fakeClock is a manual clock: time moves only on Advance, which fires every
// timer that falls due.
type fakeClock struct {
	mu     sync.Mutex
	cond   *sync.Cond
	now    time.Time
	timers []fakeTimer
}

type fakeTimer struct {
	at time.Time
	d  time.Duration
	ch chan time.Time
}

func newFakeClock(now time.Time) *fakeClock {
	c := &fakeClock{now: now}
	c.cond = sync.NewCond(&c.mu)

	return c
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- c.now

		return ch
	}
	c.timers = append(c.timers, fakeTimer{at: c.now.Add(d), d: d, ch: ch})
	c.cond.Broadcast()

	return ch
}

// Advance moves the clock by d and returns how many timers fired.
func (c *fakeClock) Advance(d time.Duration) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	fired := 0
	kept := c.timers[:0]
	for _, tm := range c.timers {
		if tm.at.After(c.now) {
			kept = append(kept, tm)

			continue
		}

		tm.ch <- c.now
		fired++
	}
	c.timers = kept

	return fired
}

// waitTimer waits until a timer of duration d is pending; a loop that never
// arms its next tick is blocked.
func (c *fakeClock) waitTimer(t *testing.T, d time.Duration) {
	t.Helper()
	deadline := time.AfterFunc(failsafe, func() {
		c.mu.Lock()
		c.cond.Broadcast()
		c.mu.Unlock()
	})
	defer deadline.Stop()
	start := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	for {
		for _, tm := range c.timers {
			if tm.d == d {
				return
			}
		}
		if time.Since(start) >= failsafe {
			t.Fatalf("no %v timer armed: the loop is blocked", d)
		}
		c.cond.Wait()
	}
}

// fakeHost is the host the fake sampler reads. With total set, available
// memory follows the services' usage and stall follows available memory;
// otherwise avail and stall are what the test sets.
type fakeHost struct {
	mu           sync.Mutex
	avail, stall float64
	total, base  uint64
	svcs         []Sample
	start        map[string]uint64
	rates        map[string]uint64
	top          Top
	hostErr      error
	topErr       error
	hostReads    int
	serviceReads int
	topReads     int
	firstDanger  time.Time
	clock        *fakeClock
}

func newFakeHost(clock *fakeClock, svcs ...Sample) *fakeHost {
	h := &fakeHost{clock: clock, svcs: svcs, start: map[string]uint64{}, avail: 40}
	for _, s := range svcs {
		h.start[s.Name] = s.Anon
	}

	return h
}

func (h *fakeHost) Host() (Host, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.hostReads++
	if h.hostErr != nil {
		return Host{}, h.hostErr
	}
	avail, stall := h.avail, h.stall
	if h.total > 0 {
		used := h.base
		for _, s := range h.svcs {
			used += s.Anon
		}
		avail = float64(h.total-min(used, h.total)) / float64(h.total) * 100
		stall = 0
		if avail < 6 {
			stall = 35
		}
	}
	if avail < 5 && stall >= 20 && h.firstDanger.IsZero() {
		h.firstDanger = h.clock.Now()
	}

	return Host{AvailablePct: avail, FullAvg10: stall}, nil
}

func (h *fakeHost) Services(dst []Sample) ([]Sample, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.serviceReads++

	return append(dst, h.svcs...), nil
}

func (h *fakeHost) TopProcess() (Top, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.topReads++

	return h.top, h.topErr
}

// set makes the host avail % available and stall % stalled while the named
// services grow by rates bytes a second.
func (h *fakeHost) set(avail, stall float64, rates map[string]uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.avail, h.stall, h.rates = avail, stall, rates
}

func (h *fakeHost) grow() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := range h.svcs {
		h.svcs[i].Anon += h.rates[h.svcs[i].Name]
	}
}

// restart resets the service at path to its start size; stop removes it.
func (h *fakeHost) act(path string, remove bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := range h.svcs {
		if h.svcs[i].CgroupPath != path {
			continue
		}
		if remove {
			h.svcs = append(h.svcs[:i], h.svcs[i+1:]...)
		} else {
			h.svcs[i].Anon = h.start[h.svcs[i].Name]
		}

		return
	}
}

func (h *fakeHost) reads() (host, services, top int) {
	h.mu.Lock()
	defer h.mu.Unlock()

	return h.hostReads, h.serviceReads, h.topReads
}

type call struct {
	verb string
	svc  Service
	path string
	at   time.Time
}

// fakeActuator runs restart and stop on the fake host. With hang set they
// block until release is closed; err makes them fail; killErr makes Kill
// fail.
type fakeActuator struct {
	mu      sync.Mutex
	host    *fakeHost
	clock   *fakeClock
	calls   []call
	hang    bool
	err     error
	killErr error
	blocked chan struct{}
	release chan struct{}
}

func newFakeActuator(host *fakeHost, clock *fakeClock) *fakeActuator {
	return &fakeActuator{
		host: host, clock: clock, blocked: make(chan struct{}, 16), release: make(chan struct{}),
	}
}

func (a *fakeActuator) Restart(ctx context.Context, s Service) error {
	return a.do(ctx, "restart", s)
}

func (a *fakeActuator) Stop(ctx context.Context, s Service) error {
	return a.do(ctx, "stop", s)
}

func (a *fakeActuator) do(ctx context.Context, verb string, s Service) error {
	a.mu.Lock()
	a.calls = append(a.calls, call{verb: verb, svc: s, path: s.CgroupPath, at: a.clock.Now()})
	hang, err := a.hang, a.err
	a.mu.Unlock()
	if hang {
		a.blocked <- struct{}{}
		select {
		case <-a.release:
		case <-ctx.Done():
			// A real hang ignores its context; wait for the test to end.
			<-a.release
		}

		return errors.New("released")
	}
	if err != nil {
		return err
	}
	a.host.act(s.CgroupPath, verb == "stop")

	return nil
}

func (a *fakeActuator) Kill(path string) error {
	a.mu.Lock()
	a.calls = append(a.calls, call{verb: "kill", path: path, at: a.clock.Now()})
	err := a.killErr
	a.mu.Unlock()
	if err != nil {
		return err
	}
	a.host.act(path, false)

	return nil
}

func (a *fakeActuator) log() []call {
	a.mu.Lock()
	defer a.mu.Unlock()

	return append([]call(nil), a.calls...)
}

// loopSim drives a loop's step once a fake second, the way Run does, waiting
// for each action goroutine to settle so replays are deterministic.
type loopSim struct {
	t     *testing.T
	l     *Loop
	clock *fakeClock
	host  *fakeHost
	act   *fakeActuator
}

func loopConfig(mode Mode) Config {
	cfg := episodeConfig(mode)
	cfg.ActionTimeout = time.Minute
	cfg.Channel, cfg.FailedChannel = health.ModeTelegram, health.ModeSMS

	return cfg
}

func newLoopSim(t *testing.T, cfg Config, svcs ...Sample) *loopSim {
	t.Helper()
	clock := newFakeClock(t0())
	host := newFakeHost(clock, svcs...)
	act := newFakeActuator(host, clock)
	t.Cleanup(func() { close(act.release) })
	s := &loopSim{t: t, clock: clock, host: host, act: act}
	s.l = NewLoop(cfg, clock, host, act, nopReporter{})

	return s
}

// run advances d one second at a time.
func (s *loopSim) run(d time.Duration) {
	s.t.Helper()
	for range int(d / time.Second) {
		s.host.grow()
		busy := s.l.busy
		s.l.step(s.t.Context(), s.clock.Now())
		if !busy && s.l.busy {
			s.settle()
		}
		if s.clock.Advance(time.Second) > 0 {
			s.l.actions.Wait()
		}
	}
}

// settle waits until the action just dispatched finished or hangs in the
// actuator.
func (s *loopSim) settle() {
	s.t.Helper()
	done := make(chan struct{})
	go func() {
		s.l.actions.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-s.act.blocked:
	case <-time.After(failsafe):
		s.t.Fatal("action neither finished nor blocked")
	}
}

// outcomes drains the outcomes the loop queued for the reporter, after
// collecting an action that finished since the last step.
func (s *loopSim) outcomes() []Outcome {
	s.l.collect(s.clock.Now())
	var got []Outcome
	for {
		select {
		case it := <-s.l.outcomes:
			if it.err == nil {
				got = append(got, it.out)
			}
		default:
			return got
		}
	}
}

func (s *loopSim) state() ([]Record, bool) {
	select {
	case r := <-s.l.states:
		return r, true
	default:
		return nil, false
	}
}

func oneOutcome(t *testing.T, got []Outcome) Outcome {
	t.Helper()
	if len(got) != 1 {
		t.Fatalf("outcomes %+v, want exactly one", got)
	}

	return got[0]
}

type nopReporter struct{}

func (nopReporter) Report(context.Context, Outcome, Message) {}
func (nopReporter) Save(context.Context, []Record)           {}

// TestSC001SC002LeakReplay replays a container leaking 1 GB a minute on a
// 64 GB host: it is restarted within 60 s of danger and before memory runs out
// (SC-001), with exactly one outcome, on mode and none on failed_mode
// (SC-002).
func TestSC001SC002LeakReplay(t *testing.T) {
	s := newLoopSim(t, loopConfig(ModeAct),
		container("web-app", 4*gib), container("cache", 20*gib), unit("worker.service", 2*gib))
	s.host.total, s.host.base = 64*gib, 18*gib
	s.host.set(0, 0, map[string]uint64{"web-app": gib / 60})

	s.run(25 * time.Minute)

	calls := s.act.log()
	if len(calls) != 1 || calls[0].verb != "restart" || calls[0].svc.ID != "container:web-app" {
		t.Fatalf("actuator calls %+v, want one restart of web-app", calls)
	}
	if s.host.firstDanger.IsZero() {
		t.Fatal("the replay never reached danger")
	}
	if lag := calls[0].at.Sub(s.host.firstDanger); lag > time.Minute {
		t.Fatalf("restarted %v after danger, want ≤ 60 s", lag)
	}
	o := oneOutcome(t, s.outcomes())
	m := s.l.cfg.Message(o)
	if o.Kind != OutcomeRestarted || m.Mode != health.ModeTelegram || m.Key != "memory/guard/web-app" {
		t.Fatalf("outcome %+v as %+v, want restarted on tlg", o, m)
	}
	if o.AvailablePct <= 0 || o.Size < 20*gib {
		t.Fatalf("outcome %+v, want the restart before memory ran out", o)
	}
	if recs, ok := s.state(); !ok || len(recs) != 1 || recs[0].Action != ActionRestart || recs[0].Mode != ModeAct {
		t.Fatalf("state %+v, want the restart record", recs)
	}
}

// TestSC006WeekOfNormalBehaviour replays a week of a healthy host: stable large
// services, a cache that fills and is flushed, page cache using most of the
// memory and short pressure spikes. Nothing is acted on or reported.
func TestSC006WeekOfNormalBehaviour(t *testing.T) {
	s := newLoopSim(t, loopConfig(ModeAct),
		container("db", 24*gib), container("cache", 2*gib), container("web-app", 3*gib), unit("worker.service", gib))
	for minute := range 7 * 24 * 60 {
		// The cache grows 3 GB over 20 minutes and is flushed.
		cacheRate := uint64(0)
		if minute%30 < 20 {
			cacheRate = 3 * gib / (20 * 60)
		} else if minute%30 == 20 {
			s.host.act("system.slice/docker-cacheid.scope", false)
		}
		avail, stall := 30.0, 0.0
		switch {
		case minute%240 < 30: // nightly-style page cache: low available, no stalls
			avail = 3
		case minute%97 == 0: // a short stall burst on a busy host
			avail, stall = 4, 40
		}
		s.host.set(avail, stall, map[string]uint64{"cache": cacheRate})
		if stall > 0 {
			s.run(10 * time.Second)
			s.host.set(30, 0, map[string]uint64{"cache": cacheRate})
			s.run(50 * time.Second)

			continue
		}
		s.run(time.Minute)
	}
	if calls := s.act.log(); len(calls) != 0 {
		t.Fatalf("actuator calls %+v, want none", calls)
	}
	if got := s.outcomes(); len(got) != 0 {
		t.Fatalf("outcomes %+v, want none", got)
	}
}

// TestUS1_9UnitRestart: a leaking systemd service is restarted by the same
// rules and the message names its unit.
func TestUS1_9UnitRestart(t *testing.T) {
	s := newLoopSim(t, loopConfig(ModeAct), standardServices()...)
	s.host.set(40, 0, map[string]uint64{"worker.service": leak})
	s.run(5 * time.Minute)
	s.host.set(3, 35, map[string]uint64{"worker.service": leak})
	s.run(time.Minute)

	calls := s.act.log()
	want := Service{
		ID: "unit:worker.service", Kind: KindUnit, Name: "worker.service",
		CgroupPath: "system.slice/worker.service", Eligible: true,
	}
	if len(calls) != 1 || calls[0].verb != "restart" || calls[0].svc != want {
		t.Fatalf("actuator calls %+v, want a restart of %+v", calls, want)
	}
	o := oneOutcome(t, s.outcomes())
	if m := s.l.cfg.Message(o); !strings.HasPrefix(m.Text, "memory: restarted service worker.service (") ||
		m.Mode != health.ModeTelegram {
		t.Fatalf("message %+v, want the unit restart on tlg", m)
	}
}

// TestActuatorHangFallsBackToKill (US1-8, FR-010, R6): a restart that never
// answers is given action_timeout, then the cgroup is killed and a killed
// outcome goes out on failed_mode. The loop keeps sampling meanwhile, and the
// kill counts toward the repeat window.
func TestActuatorHangFallsBackToKill(t *testing.T) {
	s := newLoopSim(t, loopConfig(ModeAct), standardServices()...)
	s.host.set(40, 0, webLeak())
	s.run(5 * time.Minute)
	s.act.hang = true
	s.host.set(3, 35, webLeak())
	s.run(31 * time.Second)
	restartAt := s.act.log()[0].at
	reads, _, _ := s.host.reads()

	s.run(time.Minute)
	calls := s.act.log()
	if len(calls) != 2 || calls[1].verb != "kill" || calls[1].path != "system.slice/docker-web-appid.scope" {
		t.Fatalf("actuator calls %+v, want the restart then a kill of web-app's cgroup", calls)
	}
	if got := calls[1].at.Sub(restartAt); got != time.Minute {
		t.Fatalf("killed %v after the restart, want action_timeout", got)
	}
	if after, _, _ := s.host.reads(); after-reads != 60 {
		t.Fatalf("host read %d times during the hang, want once a second", after-reads)
	}
	o := oneOutcome(t, s.outcomes())
	m := s.l.cfg.Message(o)
	if o.Kind != OutcomeKilled || o.Timeout != time.Minute || m.Mode != health.ModeSMS ||
		!strings.Contains(m.Text, "docker did not answer within 60s") {
		t.Fatalf("outcome %+v as %+v, want killed on sms", o, m)
	}
	if recs, ok := s.state(); !ok || len(recs) != 1 || recs[0].Action != ActionKill {
		t.Fatalf("state %+v, want the kill record", recs)
	}

	// It leaks again: the kill counted, so this time it is stopped.
	s.act.hang = false
	s.host.set(40, 0, nil)
	s.run(time.Hour)
	s.host.set(40, 0, webLeak())
	s.run(5 * time.Minute)
	s.host.set(3, 35, webLeak())
	s.run(time.Minute)
	if calls := s.act.log(); len(calls) != 3 || calls[2].verb != "stop" {
		t.Fatalf("actuator calls %+v, want a stop after the kill", calls)
	}
}

// TestUnreachableRuntimeFallsBackToKill: an unreachable runtime is killed at
// once, without waiting for action_timeout.
func TestUnreachableRuntimeFallsBackToKill(t *testing.T) {
	s := newLoopSim(t, loopConfig(ModeAct), standardServices()...)
	s.act.err = fmt.Errorf("dial docker: %w", ErrUnreachable)
	s.host.set(40, 0, webLeak())
	s.run(5 * time.Minute)
	s.host.set(3, 35, webLeak())
	s.run(31 * time.Second)

	calls := s.act.log()
	if len(calls) != 2 || calls[1].verb != "kill" || !calls[1].at.Equal(calls[0].at) {
		t.Fatalf("actuator calls %+v, want an immediate kill", calls)
	}
	o := oneOutcome(t, s.outcomes())
	m := s.l.cfg.Message(o)
	if o.Kind != OutcomeKilled || !o.Unreachable ||
		!strings.Contains(m.Text, "docker could not be reached") || strings.Contains(m.Text, "within") {
		t.Fatalf("outcome %+v as %+v, want killed with a could-not-be-reached text", o, m)
	}
}

// TestKillOnlyServiceOrScopeCgroups: the kill fallback never passes a slice,
// the root or an empty path to the actuator (it would kill every service
// inside).
func TestKillOnlyServiceOrScopeCgroups(t *testing.T) {
	cases := []struct {
		path string
		kill bool
	}{
		{"system.slice/docker-abc.scope", true},
		{"system.slice/backup.service", true},
		{"system.slice/app.slice/worker.service/", true},
		{"system.slice/app.slice", false},
		{"system.slice", false},
		{"system.slice/", false},
		{"", false},
		{"/", false},
		{"system.slice/web.service/..", false},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			s := newLoopSim(t, loopConfig(ModeAct))
			s.act.err = ErrUnreachable
			dec := Decision{Act: true, Outcome: Outcome{
				Culprit: Culprit{Service: Service{Kind: KindUnit, Name: "x", CgroupPath: tc.path}},
				Kind:    OutcomeRestarted, Action: ActionRestart,
			}}
			s.l.busy = true
			s.l.actions.Add(1)
			s.l.act(t.Context(), dec)
			r := <-s.l.results
			kills := 0
			for _, c := range s.act.log() {
				if c.verb == "kill" {
					kills++
				}
			}
			if tc.kill != (kills == 1) || tc.kill != r.ok {
				t.Fatalf("kills %d, result %+v; want kill %v", kills, r, tc.kill)
			}
			if !tc.kill && (r.out.Kind != OutcomeActionFailed || r.out.Err == nil) {
				t.Fatalf("outcome %+v, want action_failed", r.out)
			}
		})
	}
}

// TestFailedActionDoesNotCountTowardRepeat: a restart that fails is reported
// as action_failed on failed_mode, is not kept as an intervention record, no
// not_recovered follows it, and the next episode restarts again instead of
// stopping.
func TestFailedActionDoesNotCountTowardRepeat(t *testing.T) {
	s := newLoopSim(t, loopConfig(ModeAct), standardServices()...)
	s.act.err = errors.New("no such container")
	s.host.set(40, 0, webLeak())
	s.run(5 * time.Minute)
	s.host.set(3, 35, webLeak())
	s.run(10 * time.Minute)

	o := oneOutcome(t, s.outcomes())
	m := s.l.cfg.Message(o)
	if o.Kind != OutcomeActionFailed || m.Mode != health.ModeSMS ||
		m.Text != "memory: could not restart web-app (8.5 GB): no such container; host 3% available" {
		t.Fatalf("outcome %+v as %+v, want action_failed on sms", o, m)
	}
	if recs, ok := s.state(); ok {
		t.Fatalf("state exported %+v after a failed action", recs)
	}
	if recs := s.l.guard.Export(s.clock.Now()); len(recs) != 0 {
		t.Fatalf("records %+v, want none", recs)
	}

	s.act.err = nil
	s.host.set(40, 0, nil)
	s.run(time.Hour)
	s.host.set(40, 0, webLeak())
	s.run(5 * time.Minute)
	s.host.set(3, 35, webLeak())
	s.run(time.Minute)
	calls := s.act.log()
	if len(calls) != 2 || calls[1].verb != "restart" {
		t.Fatalf("actuator calls %+v, want a second restart, not a stop", calls)
	}
	if o := oneOutcome(t, s.outcomes()); o.Kind != OutcomeRestarted || !o.LastAction.IsZero() {
		t.Fatalf("outcome %+v, want a plain restart", o)
	}
}

// TestKillFailureIsActionFailed: when the kill fallback fails too, the outcome
// is action_failed with both errors and nothing is recorded.
func TestKillFailureIsActionFailed(t *testing.T) {
	s := newLoopSim(t, loopConfig(ModeAct), standardServices()...)
	s.act.err = ErrUnreachable
	s.act.killErr = errors.New("permission denied")
	s.host.set(40, 0, webLeak())
	s.run(5 * time.Minute)
	s.host.set(3, 35, webLeak())
	s.run(31 * time.Second)

	o := oneOutcome(t, s.outcomes())
	if o.Kind != OutcomeActionFailed || !errors.Is(o.Err, ErrUnreachable) ||
		!strings.Contains(o.Err.Error(), "permission denied") {
		t.Fatalf("outcome %+v, want action_failed naming both errors", o)
	}
	if recs := s.l.guard.Export(s.clock.Now()); len(recs) != 0 {
		t.Fatalf("records %+v, want none", recs)
	}
}

// TestNoCulpritReportsTopProcess (FR-011, R9): danger with no growing service
// reports the largest process and its cgroup once, on failed_mode.
func TestNoCulpritReportsTopProcess(t *testing.T) {
	s := newLoopSim(t, loopConfig(ModeAct), standardServices()...)
	s.host.top = Top{Name: "python3", PID: 4242, Size: 40 * gib, Slice: "user.slice/user-1000.slice/session-1.scope"}
	s.host.set(40, 0, nil)
	s.run(5 * time.Minute)
	s.host.set(3, 45, nil)
	s.run(10 * time.Minute)

	o := oneOutcome(t, s.outcomes())
	m := s.l.cfg.Message(o)
	want := "memory: host 3% available, stalled 45%; no service is growing; " +
		"top process: python3 (pid 4242) 40 GB in user.slice/user-1000.slice/session-1.scope"
	if o.Kind != OutcomeNoCulprit || m.Text != want || m.Mode != health.ModeSMS {
		t.Fatalf("message %+v, want %q on sms", m, want)
	}
	if calls := s.act.log(); len(calls) != 0 {
		t.Fatalf("actuator calls %+v, want none", calls)
	}
	if _, _, top := s.host.reads(); top != 1 {
		t.Fatalf("top process read %d times, want once per episode", top)
	}
}

// TestNotRecoveredNamesTop: a restart that frees nothing is reported once with
// the largest holder: the top service, or the top process when it is larger.
func TestNotRecoveredNamesTop(t *testing.T) {
	cases := []struct {
		name string
		top  Top
		want string
	}{
		{"service", Top{Name: "python3", PID: 4242, Size: gib}, "; top: cache 30 GB"},
		{"process", Top{Name: "python3", PID: 4242, Size: 40 * gib}, "; top: python3 (pid 4242) 40 GB"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newLoopSim(t, loopConfig(ModeAct), standardServices()...)
			s.act.err = nil
			s.host.top = tc.top
			s.host.set(40, 0, webLeak())
			s.run(5 * time.Minute)
			s.host.set(3, 35, nil)
			s.run(31 * time.Second)
			if o := oneOutcome(t, s.outcomes()); o.Kind != OutcomeRestarted {
				t.Fatalf("outcome %+v, want restarted", o)
			}
			s.run(3 * time.Minute)
			o := oneOutcome(t, s.outcomes())
			m := s.l.cfg.Message(o)
			if o.Kind != OutcomeNotRecovered || !strings.HasSuffix(m.Text, tc.want) || m.Mode != health.ModeSMS {
				t.Fatalf("message %+v, want not_recovered ending %q on sms", m, tc.want)
			}
		})
	}
}

// TestRecoveryWindowStartsWhenTheActionEnds: with action_timeout longer than
// recovery_window, recovery is not judged while the restart still hangs; the
// window starts at the kill, so the operator gets the killed outcome and,
// with danger still there a recovery window later, one not_recovered for the
// kill, and no second action.
func TestRecoveryWindowStartsWhenTheActionEnds(t *testing.T) {
	cfg := loopConfig(ModeAct)
	cfg.ActionTimeout = 5 * time.Minute
	cfg.RecoveryWindow = 30 * time.Second
	cfg.DangerFor = 5 * time.Second
	s := newLoopSim(t, cfg, standardServices()...)
	s.act.hang = true
	both := map[string]uint64{"web-app": leak, "worker.service": 2 * leak}
	s.host.set(40, 0, both)
	s.run(5 * time.Minute)
	s.host.set(3, 35, both)
	s.run(6 * time.Second) // acts on worker.service, which hangs
	s.run(time.Minute)
	s.host.set(40, 0, both)
	s.run(31 * time.Second) // a clear spell while the restart still hangs
	s.host.set(3, 35, both)
	s.run(2 * time.Minute)
	if got := s.outcomes(); len(got) != 0 {
		t.Fatalf("outcomes %+v while the restart still hangs, want none", got)
	}

	s.run(90 * time.Second) // the kill at action_timeout
	o := oneOutcome(t, s.outcomes())
	if o.Kind != OutcomeKilled || o.Service.Name != "worker.service" {
		t.Fatalf("outcome %+v, want worker.service killed", o)
	}
	s.run(31 * time.Second) // danger holds a recovery window after the kill
	o = oneOutcome(t, s.outcomes())
	if m := s.l.cfg.Message(o); o.Kind != OutcomeNotRecovered || o.Action != ActionKill ||
		!strings.HasPrefix(m.Text, "memory: killing worker.service (13 GB before) did not free memory") {
		t.Fatalf("outcome %+v as %q, want not_recovered for the kill", o, m.Text)
	}
	calls := s.act.log()
	if len(calls) != 2 || calls[0].verb != "restart" || calls[1].verb != "kill" {
		t.Fatalf("actuator calls %+v, want the hanging restart and its kill only", calls)
	}
}

// TestReportFullChannelDropsOldest (R10): outcomes nobody collects never block
// the loop; the oldest are dropped and counted.
func TestReportFullChannelDropsOldest(t *testing.T) {
	s := newLoopSim(t, loopConfig(ModeAct), standardServices()...)
	s.l.outcomes = make(chan item, 2)
	for range 4 {
		s.host.set(3, 45, nil)
		s.run(31 * time.Second)
		s.host.set(40, 0, nil)
		s.run(3 * time.Minute)
	}

	got := s.outcomes()
	if len(got) != 2 || s.l.dropped.Load() != 2 {
		t.Fatalf("outcomes %+v, dropped %d; want the last two and two dropped", got, s.l.dropped.Load())
	}
	if first := t0().Add(30 * time.Second); !got[1].At.After(got[0].At) || !got[0].At.After(first) {
		t.Fatalf("outcomes at %v and %v, want the two newest", got[0].At, got[1].At)
	}
}

// TestReportModeSavesState: a would_* preview is a record (never counted for
// the repeat rule) and is exported.
func TestReportModeSavesState(t *testing.T) {
	s := newLoopSim(t, loopConfig(ModeReport), standardServices()...)
	s.host.set(40, 0, webLeak())
	s.run(5 * time.Minute)
	s.host.set(3, 35, webLeak())
	s.run(time.Minute)

	if o := oneOutcome(t, s.outcomes()); o.Kind != OutcomeWouldRestart {
		t.Fatalf("outcome %+v, want would_restart", o)
	}
	if calls := s.act.log(); len(calls) != 0 {
		t.Fatalf("actuator calls %+v, want none in report mode", calls)
	}
	if recs, ok := s.state(); !ok || len(recs) != 1 || recs[0].Mode != ModeReport {
		t.Fatalf("state %+v, want the report record", recs)
	}
}

// TestStepSteadyStateDoesNotAllocate: sampling and deciding without an outcome
// allocates nothing, services read included.
func TestStepSteadyStateDoesNotAllocate(t *testing.T) {
	s := newLoopSim(t, loopConfig(ModeAct), standardServices()...)
	s.host.set(40, 0, webLeak())
	s.run(5 * time.Minute)
	now := s.clock.Now()
	allocs := testing.AllocsPerRun(100, func() {
		now = now.Add(time.Second)
		s.l.step(s.t.Context(), now)
	})
	if allocs != 0 {
		t.Fatalf("step allocates %v times", allocs)
	}
}

// recordingReporter blocks every Report until release is closed when block is
// set, and hands the outcomes and saved states to the test.
type recordingReporter struct {
	block   bool
	release chan struct{}
	reports chan Message
	saves   chan []Record
}

func (r *recordingReporter) Report(_ context.Context, _ Outcome, m Message) {
	if r.block {
		<-r.release
	}

	r.reports <- m
}

func (r *recordingReporter) Save(_ context.Context, records []Record) {
	r.saves <- records
}

// runLoop runs l in the background until the test ends.
func runLoop(t *testing.T, l *Loop) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		l.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

// TestRunReportsAndSaves: Run samples on the clock and hands outcomes and
// state to the reporter.
func TestRunReportsAndSaves(t *testing.T) {
	clock := newFakeClock(t0())
	host := newFakeHost(clock, standardServices()...)
	rep := &recordingReporter{reports: make(chan Message, 8), saves: make(chan []Record, 8)}
	l := NewLoop(loopConfig(ModeReport), clock, host, newFakeActuator(host, clock), rep)
	runLoop(t, l)

	host.set(40, 0, webLeak())
	for range 300 {
		clock.waitTimer(t, time.Second)
		host.grow()
		clock.Advance(time.Second)
	}
	host.set(3, 35, webLeak())
	for range 40 {
		clock.waitTimer(t, time.Second)
		host.grow()
		clock.Advance(time.Second)
	}
	select {
	case m := <-rep.reports:
		if !strings.HasPrefix(m.Text, "memory: would restart container web-app") || m.Mode != health.ModeTelegram {
			t.Fatalf("report %+v, want would restart on tlg", m)
		}
	case <-time.After(failsafe):
		t.Fatal("no report")
	}
	select {
	case recs := <-rep.saves:
		if len(recs) != 1 || recs[0].Name != "web-app" {
			t.Fatalf("saved %+v, want the web-app record", recs)
		}
	case <-time.After(failsafe):
		t.Fatal("no state saved")
	}
}

// TestUS1_8LoopNotBlockedByActuatorOrReporter: with the reporter stuck and the
// actuator hanging, Run keeps sampling every second (FR-008).
func TestUS1_8LoopNotBlockedByActuatorOrReporter(t *testing.T) {
	clock := newFakeClock(t0())
	host := newFakeHost(clock, standardServices()...)
	act := newFakeActuator(host, clock)
	act.hang = true
	rep := &recordingReporter{
		block: true, release: make(chan struct{}), reports: make(chan Message, 64), saves: make(chan []Record, 64),
	}
	l := NewLoop(loopConfig(ModeAct), clock, host, act, rep)
	runLoop(t, l)
	t.Cleanup(func() {
		close(rep.release)
		close(act.release)
	})

	host.set(40, 0, webLeak())
	for range 300 {
		clock.waitTimer(t, time.Second)
		host.grow()
		clock.Advance(time.Second)
	}
	host.set(3, 35, nil)
	for range 31 {
		clock.waitTimer(t, time.Second)
		clock.Advance(time.Second)
	}
	select {
	case <-act.blocked:
	case <-time.After(failsafe):
		t.Fatal("the restart was never issued")
	}
	clock.waitTimer(t, time.Second) // the beat that issued it is over
	before, _, _ := host.reads()
	// The restart hangs until action_timeout, the kill fallback's outcome and
	// then not_recovered go to the stuck reporter; the loop keeps its
	// one-second beat throughout.
	for range 3 * 60 {
		clock.waitTimer(t, time.Second)
		clock.Advance(time.Second)
	}
	clock.waitTimer(t, time.Second)
	if after, _, _ := host.reads(); after-before != 180 {
		t.Fatalf("host read %d times in 180 s, want 180", after-before)
	}
}

// TestOffIsNotStarted: intervene off runs nothing.
func TestOffIsNotStarted(t *testing.T) {
	clock := newFakeClock(t0())
	host := newFakeHost(clock, standardServices()...)
	l := NewLoop(loopConfig(ModeOff), clock, host, newFakeActuator(host, clock), nopReporter{})
	done := make(chan struct{})
	go func() {
		l.Run(t.Context())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(failsafe):
		t.Fatal("Run did not return with intervene off")
	}
	if h, svc, _ := host.reads(); h != 0 || svc != 0 {
		t.Fatalf("sampled %d host and %d service times, want none", h, svc)
	}
}

// TestRestoredRecordsCountTowardRepeat: records restored from the checkpoint
// make the next runaway of the same service a stop.
func TestRestoredRecordsCountTowardRepeat(t *testing.T) {
	s := newLoopSim(t, loopConfig(ModeAct), standardServices()...)
	s.l.Restore([]Record{{
		ServiceID: "container:web-app", Name: "web-app", Action: ActionRestart,
		At: t0().Add(-time.Hour), Before: 20 * gib, Mode: ModeAct,
	}})
	s.host.set(40, 0, webLeak())
	s.run(5 * time.Minute)
	s.host.set(3, 35, webLeak())
	s.run(time.Minute)

	if calls := s.act.log(); len(calls) != 1 || calls[0].verb != "stop" {
		t.Fatalf("actuator calls %+v, want a stop", calls)
	}
	if o := oneOutcome(t, s.outcomes()); o.Kind != OutcomeStopped || !o.LastAction.Equal(t0().Add(-time.Hour)) {
		t.Fatalf("outcome %+v, want stopped after the restored restart", o)
	}
}

// TestSamplerErrorsAreLoggedOnce: a failing read skips the beat and is logged
// once when it starts failing, not on every beat; a failing top-process read
// still reports the episode.
func TestSamplerErrorsAreLoggedOnce(t *testing.T) {
	clock := newFakeClock(t0())
	host := newFakeHost(clock, standardServices()...)
	host.hostErr = errors.New("meminfo unreadable")
	host.topErr = errors.New("proc unreadable")
	rep := &recordingReporter{reports: make(chan Message, 8), saves: make(chan []Record, 8)}
	core, logs := observer.New(zap.WarnLevel)
	l := NewLoop(loopConfig(ModeAct), clock, host, newFakeActuator(host, clock), rep, WithLogger(zap.New(core)))
	runLoop(t, l)

	for range 60 {
		clock.waitTimer(t, time.Second)
		clock.Advance(time.Second)
	}
	host.mu.Lock()
	host.hostErr = nil
	host.mu.Unlock()
	host.set(3, 45, nil)
	for range 31 {
		clock.waitTimer(t, time.Second)
		clock.Advance(time.Second)
	}
	select {
	case m := <-rep.reports:
		if !strings.HasSuffix(m.Text, "no service is growing") {
			t.Fatalf("report %+v, want no_culprit without a top process", m)
		}
	case <-time.After(failsafe):
		t.Fatal("no report")
	}
	if n := logs.FilterMessage("memory guard sampler failed").Len(); n != 2 {
		t.Fatalf("logged %d sampler failures, want the host read once and the top process once", n)
	}
}
