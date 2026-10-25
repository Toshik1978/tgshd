package memguard

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// HostInterval is how often the loop reads the host's memory and pressure.
const HostInterval = time.Second

// defaultActionTimeout bounds an action when the configuration gives no
// positive action_timeout; config validation refuses one, so this only guards
// a Loop built without it.
const defaultActionTimeout = time.Minute

// stopReportTimeout bounds the delivery of the outcomes reported after the
// guard was told to stop; the daemon's stop is bounded on its own as well.
const stopReportTimeout = 5 * time.Second

// Buffer sizes of the channels from the loop to the reporter goroutine.
const (
	outcomeBuffer = 32
	stateBuffer   = 1
	lateBuffer    = 1
)

// ErrUnreachable is returned by an Actuator when the service's runtime
// (dockerd, systemd) cannot be reached; the loop then kills the cgroup at once
// instead of waiting for ActionTimeout.
var ErrUnreachable = errors.New("runtime unreachable")

var errActionTimeout = errors.New("action timed out")

// Clock tells the time and wakes the loop.
type Clock interface {
	Now() time.Time
	// After returns a channel that receives the time once d has passed.
	After(d time.Duration) <-chan time.Time
}

// Sampler reads the kernel's memory files. Its methods run on the loop's
// goroutine and must not allocate in steady state.
type Sampler interface {
	// Host reads the available memory and full avg10 pressure; the loop sets
	// At.
	Host() (Host, error)
	// Services appends one sample per service cgroup to dst and returns it.
	Services(dst []Sample) ([]Sample, error)
	// TopProcess returns the process with the largest resident set and its
	// cgroup.
	TopProcess() (Top, error)
}

// Actuator restarts and stops services through their runtime, by Kind, and
// kills a cgroup when the runtime does not answer.
type Actuator interface {
	Restart(ctx context.Context, s Service) error
	Stop(ctx context.Context, s Service) error
	// Kill writes to the cgroup.kill of cgroupPath, relative to the cgroup
	// root.
	Kill(cgroupPath string) error
}

// Reporter delivers what the loop produces, on its own goroutine.
type Reporter interface {
	// Report delivers one outcome, rendered as m.
	Report(ctx context.Context, o Outcome, m Message)
	// Save keeps the intervention records for the checkpoint (kv
	// memory.guard).
	Save(ctx context.Context, records []Record)
}

// item is one entry of the reporter queue: an outcome, or a sampler error to
// log.
type item struct {
	out Outcome
	err error
}

// result is how an action ended, sent back to the loop.
type result struct {
	out Outcome
	rec Record
	ok  bool
}

// Loop is the fast memory guard (research R3, R6, R10): it reads the host
// every HostInterval and the services every ServiceInterval, judges them with
// the Guard and runs actions in their own goroutine, bounded by
// ActionTimeout. Neither an action nor the reporter ever blocks it: results
// come back through a channel it polls, outcomes and state go out through
// buffered channels that drop their oldest entry when full.
type Loop struct {
	cfg      Config
	guard    *Guard
	clock    Clock
	sampler  Sampler
	actuator Actuator
	reporter Reporter
	log      *zap.Logger

	services     []Sample
	nextServices time.Time
	hostFailing  bool
	svcFailing   bool

	results  chan result
	late     chan []Record
	outcomes chan item
	states   chan []Record
	dropped  atomic.Uint64
	actions  sync.WaitGroup
}

// LoopOption configures a Loop.
type LoopOption func(*Loop)

// WithLogger logs sampler errors and dropped outcomes.
func WithLogger(log *zap.Logger) LoopOption {
	return func(l *Loop) { l.log = log }
}

// NewLoop creates the guard loop over a new Guard for cfg. An ActionTimeout
// that is not positive falls back to defaultActionTimeout.
func NewLoop(cfg Config, clock Clock, sampler Sampler, actuator Actuator, reporter Reporter, opts ...LoopOption) *Loop {
	if cfg.ActionTimeout <= 0 {
		cfg.ActionTimeout = defaultActionTimeout
	}
	l := &Loop{
		cfg: cfg, guard: New(cfg), clock: clock, sampler: sampler, actuator: actuator, reporter: reporter,
		log:      zap.NewNop(),
		results:  make(chan result, 1),
		late:     make(chan []Record, lateBuffer),
		outcomes: make(chan item, outcomeBuffer),
		states:   make(chan []Record, stateBuffer),
	}
	for _, o := range opts {
		o(l)
	}

	return l
}

// Restore loads the intervention records saved by a previous run. It must be
// called before Run.
func (l *Loop) Restore(records []Record) {
	l.guard.Restore(l.clock.Now(), records)
}

// RestoreLate hands the loop intervention records restored after it started,
// when the store could not be read at start. It never blocks: the loop merges
// them into its own on its next beat (Guard.Merge) and saves the result. A
// newer call replaces records not taken yet, so each call must pass all the
// records restored late.
func (l *Loop) RestoreLate(records []Record) {
	offer(l.late, records)
}

// Run samples, decides and acts until ctx is done. With Mode off it returns at
// once. On its way out it waits for the running action, takes in its result
// (an action that succeeded as ctx ended is still recorded) and has the
// reporter deliver everything still queued (see deliver).
func (l *Loop) Run(ctx context.Context) {
	if l.cfg.Mode == ModeOff {
		return
	}
	stopped := make(chan struct{})
	var deliver sync.WaitGroup
	deliver.Go(func() { l.deliver(ctx, stopped) })
	defer func() {
		l.actions.Wait()
		select {
		case r := <-l.results:
			l.finish(l.clock.Now(), r)
		default:
		}
		close(stopped)
		deliver.Wait()
	}()
	for {
		l.step(ctx, l.clock.Now())
		select {
		case <-ctx.Done():
			return
		case <-l.clock.After(HostInterval):
		}
	}
}

// step is one beat of the loop: collect a finished action, sample, decide.
func (l *Loop) step(ctx context.Context, now time.Time) {
	l.collect(now)
	if !now.Before(l.nextServices) {
		l.nextServices = now.Add(ServiceInterval)
		svcs, err := l.sampler.Services(l.services[:0])
		l.svcFailing = l.note(l.svcFailing, err, "read services")
		if err == nil {
			l.services = svcs
			l.guard.ObserveServices(now, svcs)
		}
	}
	h, err := l.sampler.Host()
	l.hostFailing = l.note(l.hostFailing, err, "read host memory")
	if err != nil {
		return
	}
	h.At = now
	if dec, ok := l.guard.ObserveHost(h); ok {
		l.handle(ctx, now, dec)
	}
}

// collect takes in the result of a finished action or the records restored
// late, if any. It takes one item per beat: when both are ready, the other
// waits for the next beat, at most HostInterval later, which is harmless.
func (l *Loop) collect(now time.Time) {
	select {
	case r := <-l.results:
		l.finish(now, r)
	case records := <-l.late:
		l.guard.Merge(now, records)
		l.save(now)
	default:
	}
}

// finish tells the guard how an action ended, saves the records it keeps and
// queues the outcome.
func (l *Loop) finish(now time.Time, r result) {
	l.guard.ActionDone(now, r.rec, r.ok)
	if r.ok {
		l.save(now)
	}
	l.emit(r.out)
}

// note queues a sampler error to log when a read starts failing, and returns
// whether it is failing now.
func (l *Loop) note(failing bool, err error, what string) bool {
	if err != nil && !failing {
		l.offer(item{err: fmt.Errorf("memory guard: %s: %w", what, err)})
	}

	return err != nil
}

func (l *Loop) handle(ctx context.Context, now time.Time, dec Decision) {
	switch dec.Outcome.Kind {
	case OutcomeNoCulprit:
		dec.Outcome.Top = l.topProcess()
	case OutcomeNotRecovered:
		dec.Outcome.Top = l.top()
	case OutcomeWouldRestart, OutcomeWouldStop:
		l.save(now)
	case OutcomeRestarted, OutcomeStopped, OutcomeKilled, OutcomeProtected, OutcomeActionFailed:
	}
	if !dec.Act {
		l.emit(dec.Outcome)

		return
	}
	// The guard decides no new action before ActionDone, so at most one runs.
	l.actions.Add(1)
	go l.act(ctx, dec)
}

// top is the largest memory holder: the largest service, or the largest
// process when it holds more.
func (l *Loop) top() Top {
	var top Top
	for i := range l.services {
		if s := &l.services[i]; s.Anon > top.Size {
			top = Top{Name: s.Name, Size: s.Anon}
		}
	}
	if p := l.topProcess(); p.Size > top.Size {
		return p
	}

	return top
}

func (l *Loop) topProcess() Top {
	p, err := l.sampler.TopProcess()
	if err != nil {
		l.offer(item{err: fmt.Errorf("memory guard: read top process: %w", err)})

		return Top{}
	}

	return p
}

// act runs the decided action on its own goroutine and sends how it ended
// back to the loop. A timeout or an unreachable runtime falls back to killing
// the service's cgroup. On shutdown a failed action is dropped, never killed;
// one that succeeded is still sent, so it is recorded.
func (l *Loop) act(ctx context.Context, dec Decision) {
	defer l.actions.Done()
	out, rec := dec.Outcome, dec.Record
	err := l.run(ctx, out.Action, out.Service)
	if err != nil && ctx.Err() != nil {
		return // shutting down: never fall back to a kill
	}
	ok := err == nil
	if errors.Is(err, errActionTimeout) || errors.Is(err, ErrUnreachable) ||
		errors.Is(err, context.DeadlineExceeded) {
		cause := err
		if kerr := l.kill(out.Service); kerr != nil {
			err = fmt.Errorf("%w; kill: %w", err, kerr)
		} else {
			ok, err = true, nil
			out.Kind, rec.Action = OutcomeKilled, ActionKill
			out.Unreachable = errors.Is(cause, ErrUnreachable)
			if !out.Unreachable {
				out.Timeout = l.cfg.ActionTimeout
			}
		}
	}
	if !ok {
		out.Kind, out.Err = OutcomeActionFailed, err
	}
	// The channel holds one result and only one action runs at a time.
	l.results <- result{out: out, rec: rec, ok: ok}
}

// run calls the actuator and waits for it at most ActionTimeout. An actuator
// that ignores its context is left behind.
func (l *Loop) run(ctx context.Context, action Action, s Service) error {
	actx, cancel := context.WithCancel(ctx)
	defer cancel()
	timeout := l.clock.After(l.cfg.ActionTimeout)
	done := make(chan error, 1)
	go func() {
		if action == ActionStop {
			done <- l.actuator.Stop(actx, s)
		} else {
			done <- l.actuator.Restart(actx, s)
		}
	}()
	select {
	case err := <-done:
		return err
	case <-timeout:
		return fmt.Errorf("%s %s: %w", action, s.Name, errActionTimeout)
	case <-ctx.Done():
		return fmt.Errorf("%s %s: %w", action, s.Name, ctx.Err())
	}
}

// kill kills the service's cgroup. Only a cgroup whose last element is a
// .service or a .scope is ever passed on: killing a slice would kill every
// service inside it.
func (l *Loop) kill(s Service) error {
	p := path.Clean(strings.Trim(s.CgroupPath, "/"))
	last := path.Base(p)
	if p == "." || p == ".." || strings.HasPrefix(p, "../") ||
		!strings.HasSuffix(last, ".service") && !strings.HasSuffix(last, ".scope") {
		return fmt.Errorf("refusing to kill cgroup %q: not a service or a scope", s.CgroupPath)
	}
	if err := l.actuator.Kill(p); err != nil {
		return fmt.Errorf("kill cgroup %s: %w", p, err)
	}

	return nil
}

// save queues the intervention records for the reporter, replacing any not
// yet saved.
func (l *Loop) save(now time.Time) {
	offer(l.states, l.guard.Export(now))
}

// emit queues an outcome for the reporter.
func (l *Loop) emit(o Outcome) {
	l.offer(item{out: o})
}

func (l *Loop) offer(it item) {
	if offer(l.outcomes, it) {
		l.dropped.Add(1)
	}
}

// offer sends v on ch without blocking, dropping the oldest entries to make
// room. ch must have one sender. It reports whether it dropped one.
func offer[T any](ch chan T, v T) bool {
	dropped := false
	for {
		select {
		case ch <- v:
			return dropped
		default:
		}
		select {
		case <-ch:
			dropped = true
		default:
		}
	}
}

// deliver hands outcomes and state to the reporter until stopped is closed,
// which Run does once nothing more can be queued, then delivers what is left.
// State is always saved with a ctx that is never done: records taken in as
// the guard stops must still be kept. An outcome reported once ctx is done
// gets a live ctx bounded by stopReportTimeout, one budget shared by every
// such report, so its delivery is really attempted (and queued by the
// notifier when it fails, for the daemon's final checkpoint) without holding
// the stop for long.
func (l *Loop) deliver(ctx context.Context, stopped <-chan struct{}) {
	d := delivery{loop: l, ctx: ctx}
	defer d.close()
	for {
		select {
		case <-stopped:
			d.drain()

			return
		case recs := <-l.states:
			l.reporter.Save(context.WithoutCancel(ctx), recs)
		case it := <-l.outcomes:
			d.item(it)
		}
		d.warnDropped()
	}
}

// delivery is the state of the deliver goroutine.
type delivery struct {
	loop *Loop
	ctx  context.Context //nolint:containedctx // the deliver goroutine's ctx, held for its lifetime only
	// logged is the dropped count already warned about.
	logged uint64
	// late is the ctx of the reports made once ctx is done; nil until then.
	late       context.Context //nolint:containedctx // see ctx
	cancelLate context.CancelFunc
}

// drain delivers the outcomes and the state still queued when the loop has
// stopped, and warns about outcomes dropped before.
func (d *delivery) drain() {
	l := d.loop
	for {
		select {
		case it := <-l.outcomes:
			d.item(it)
		default:
			select {
			case recs := <-l.states:
				l.reporter.Save(context.WithoutCancel(d.ctx), recs)
			default:
			}
			d.warnDropped()

			return
		}
	}
}

// item reports an outcome or logs a sampler error.
func (d *delivery) item(it item) {
	l := d.loop
	if it.err != nil {
		l.log.Warn("memory guard sampler failed", zap.Error(it.err))

		return
	}
	l.reporter.Report(d.reportCtx(), it.out, l.cfg.Message(it.out))
}

// reportCtx is ctx while it is live, then the shared bounded stop ctx.
func (d *delivery) reportCtx() context.Context {
	if d.ctx.Err() == nil {
		return d.ctx
	}
	if d.late == nil {
		d.late, d.cancelLate = context.WithTimeout(context.WithoutCancel(d.ctx), stopReportTimeout)
	}

	return d.late
}

// warnDropped logs the outcomes dropped since the last warning.
func (d *delivery) warnDropped() {
	if n := d.loop.dropped.Load(); n != d.logged {
		d.loop.log.Warn("memory guard dropped outcomes: the reporter fell behind", zap.Uint64("dropped", n-d.logged))
		d.logged = n
	}
}

func (d *delivery) close() {
	if d.cancelLate != nil {
		d.cancelLate()
	}
}
