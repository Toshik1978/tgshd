// Package runner is the health worker: one 30 s tick that writes the
// heartbeat, runs due checks (network first; in a quiet window only those not
// set to pause), feeds the alert engine, delivers messages and checkpoints
// state (research R2, R15).
package runner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/Toshik1978/tgshd/pkg/health"
	"github.com/Toshik1978/tgshd/pkg/health/notify"
	"github.com/Toshik1978/tgshd/pkg/health/store"
)

const (
	// WorkerName is the app.Worker name of the runner.
	WorkerName = "health"
	// Tick is the period of the runner.
	Tick = 30 * time.Second
	// DefaultConcurrency bounds parallel checks when Options.Concurrency is unset.
	DefaultConcurrency = 4
	// MaxRunTime is the default run budget of a tick: the network run and the
	// batch together finish within it; a check's shorter interval caps its run
	// further. With MaxTickTime it bounds a tick to 70 s plus store work.
	MaxRunTime = 40 * time.Second
	// MaxTickTime is the default delivery budget of a tick, counted from the
	// end of its runs: every Telegram, SMS and email send of the tick (checker
	// errors, flush, dispatch, fixes, weekly push) ends by it, also when a
	// channel hangs. Telegram and SMS get its first 27 s even when a run used
	// the whole MaxRunTime; the last 3 s (a tenth) are kept for email. Load, the daily job and the checkpoint are
	// not bounded; they are local store work. The worst tick is thus 40 s of
	// runs + 30 s of deliveries = 70 s plus store work, which leaves 20 s for
	// the store under the 90 s of three periods. gocron (singleton,
	// reschedule) runs ticks on fixed 30 s slots and skips a slot while a tick
	// is still running, so a tick of duration D makes the next heartbeat
	// 30·ceil(D/30) old: 90 s whenever a tick exceeds 60 s, which is one cycle
	// plus one minute (SC-006). A watchdog must therefore alarm only on an age
	// strictly greater than 90 s.
	MaxTickTime = 30 * time.Second
	// DefaultTelegramFailingAfter is how long Telegram delivery fails before
	// its one email notice when Options.TelegramFailingAfter is unset
	// (general.telegram_failing_after).
	DefaultTelegramFailingAfter = 15 * time.Minute

	// KeyNetwork is the network condition that, while bad, means the outside
	// is known down.
	KeyNetwork = "network/connectivity"

	keyHeartbeat   = "health/heartbeat/write"
	keyMaintenance = "health/maintenance/probe"
	keyUptime      = "health/uptime/read"
	keyLoad        = "health/store/load"
	keySave        = "health/store/write"
	keyOverrun     = "health/tick/overrun"
)

// Heartbeat writes the heartbeat file; lockfile.WriteTick bound to a path.
type Heartbeat interface {
	WriteTick(now time.Time) error
}

// Maintenance probes the maintenance lock; lockfile.MaintenanceHeld bound to a path.
type Maintenance interface {
	MaintenanceHeld() (bool, error)
}

// Uptime reads the host uptime; sysfs.Reader satisfies it.
type Uptime interface {
	Uptime() (time.Duration, error)
}

// Store persists the runner state; *store.Store satisfies it.
type Store interface {
	Load(ctx context.Context) (store.State, error)
	Checkpoint(ctx context.Context, dirty store.State, newAlerts []store.Alert, newSamples []store.SampleRow) error
	Degraded() bool
}

// Options configures a Runner. Nil Heartbeat, Maintenance, Uptime or Store
// mean the facility is absent.
type Options struct {
	Checks      []health.Check
	BootGrace   time.Duration
	Concurrency int
	// RunBudget bounds all check runs of one tick; defaults to MaxRunTime.
	// RunBudget + TickBudget plus local store work must stay under 90 s (three
	// periods) to keep the heartbeat within one cycle plus one minute.
	RunBudget time.Duration
	// TickBudget is the tick's delivery budget, counted from the end of its
	// runs, so a run that uses all of RunBudget never eats it: Telegram and
	// SMS sends end by it less a tenth kept for email, emails by all of it.
	// Defaults to MaxTickTime. A tick
	// lasts at most RunBudget + TickBudget plus local store work: 70 s plus
	// store work by default (see MaxTickTime). Undelivered messages stay queued.
	TickBudget  time.Duration
	Heartbeat   Heartbeat
	Maintenance Maintenance
	Uptime      Uptime
	Store       Store
	// Drives is the drive book /retire works on; nil means no drives are known.
	Drives DriveBook
	// Persisters keep the checks' own state in the store.
	Persisters []Persister
	// Fixes hand over the fix actions the checks took; they are taken after
	// every tick's runs.
	Fixes []FixSource
	// Host prefixes every pushed message.
	Host string
	// Mode is the notifier's default delivery mode.
	Mode     notify.Mode
	Telegram notify.Sender
	// SMS and Mailer may be nil.
	SMS    notify.Sender
	Mailer notify.Mailer
	// TelegramFailingAfter is how long Telegram delivery fails before the one
	// email notice (FR-029); defaults to DefaultTelegramFailingAfter.
	TelegramFailingAfter time.Duration
	// MaintenanceMaxHold is how long the maintenance lock may be held before
	// its checker error (FR-027); defaults to DefaultMaintenanceMaxHold.
	MaintenanceMaxHold time.Duration
	Logger             *zap.Logger
	// Now defaults to time.Now.
	Now func() time.Time
	// Location is the host's zone for "since" times; defaults to time.Local.
	Location *time.Location
	// Daily runs once per host-local day, also in a quiet window (e.g. queue
	// expiry and store retention); nil disables it. Weekly runs once per
	// WeeklySlot on a non-quiet tick; it needs both Weekly and WeeklySlot. A
	// job counts as done when attempted, so a failing hook is not retried
	// until its next day or week; the error is reported as a checker error.
	Daily      func(ctx context.Context, now time.Time) error
	Weekly     func(ctx context.Context, now time.Time) error
	WeeklySlot *Slot
}

// OpenCondition is an open problem as /status shows it.
type OpenCondition struct {
	Key   string
	Text  string
	Since time.Time
}

// Snapshot is the runner state for /status.
type Snapshot struct {
	Open []OpenCondition
	// Checks is the number of configured checks.
	Checks int
	// LastCycle is the time of the last completed tick, quiet or not; a quiet
	// tick runs the checks not set to pause (zero before the first).
	LastCycle time.Time
	// Quiet is the active quiet-window reason, empty when none.
	Quiet string
	// HeldSince is since when the maintenance lock is held, zero while it
	// is free.
	HeldSince time.Time
	// Paused are the configured checks set to pause in quiet windows
	// (Check.PauseInQuiet), in configuration order.
	Paused        []string
	StoreDegraded bool
}

// Runner is the health app.Worker.
type Runner struct {
	opts     Options
	engine   *health.Engine
	marks    *health.RateMarks
	notifier *notify.Notifier
	checker  *notify.CheckerErrors
	// own persists the runner's own state: the notifier's delivery state and
	// the maintenance-lock observation.
	own     []Persister
	network int // index of the network check, -1 when absent
	// networkUnknown tells that the network check has not run yet or its
	// last run was unreadable (an error or no judged reading): checks that
	// need the internet are not run (FR-036). Only touched by Do.
	networkUnknown bool
	// sends bounds every delivery by the running tick's deadline.
	sends deliveries

	// lastRun is indexed like opts.Checks and only touched by Do.
	lastRun []time.Time
	// dailyLast and weeklyLast are the last attempts of the hooks, and
	// scheduleRestored tells that stored times were merged; only touched by Do.
	dailyLast, weeklyLast time.Time
	scheduleRestored      bool
	// running is indexed like opts.Checks: a run is still in flight, possibly
	// abandoned after its deadline.
	running []atomic.Bool
	// pendingAlerts and pendingSamples wait for the first successful Load;
	// only touched by Do.
	pendingAlerts  []store.Alert
	pendingSamples []store.SampleRow
	// pendingDropped tells that pending records were dropped since they were
	// last handed to the store; only touched by Do.
	pendingDropped bool
	// saveReported tells that a state-write failure went out to the operator
	// and its recovery notice is due; only touched by Do.
	saveReported bool

	// ticking is held by Do and by Checkpoint, so the stop's checkpoint never
	// runs inside a tick.
	ticking chan struct{}

	loadMu sync.Mutex
	loaded bool
	// memoryOnly tells that the store holds a newer build's database: the
	// runner runs as without a store. Set by Load; Do reads it after its Load.
	memoryOnly bool
	// loadErr is the last failed Load, not yet raised by Do.
	loadErr error
	// restoreErrs are the persister failures of the successful Load, not yet
	// raised by Do.
	restoreErrs []error
	// loadedKV is the stored kv of the successful Load.
	loadedKV map[string]string

	mu        sync.Mutex
	quiet     string
	lastCycle time.Time
	// maint is the maintenance-lock observation; maintObserved tells that a
	// tick has probed the lock since start.
	maint         MaintenanceLock
	maintObserved bool
	// logged are alert-log records made outside a tick's dispatch (clears,
	// retires, pushes); the next checkpoint takes them.
	logged []store.Alert
}

// outcome is the result of one check in a batch; started is false when the
// tick deadline passed before the check got a slot.
type outcome struct {
	res     health.CheckResult
	started bool
}

// cycle collects what one tick produces.
type cycle struct {
	now     time.Time
	alerts  []health.Alert
	cleared []store.Alert // silent clears (CheckResult.Forget), logged after the alerts
	samples []store.SampleRow
	fixes   []Fix
	errs    []checkerErr
	// overruns are the checks that ran out of the tick deadline, as check
	// name and error text; raiseOverruns reports them.
	overruns [][2]string
	// before are the conditions at the start of the tick's runs; kept only
	// when there are fix sources (see saveInterruptedFixes).
	before []health.Condition
}

// New creates a Runner and its notifier and checker errors.
func New(opts Options) *Runner {
	opts = withDefaults(opts)

	r := &Runner{
		opts:    opts,
		engine:  health.NewEngine(),
		marks:   health.NewRateMarks(),
		network: -1,
		lastRun: make([]time.Time, len(opts.Checks)),
		running: make([]atomic.Bool, len(opts.Checks)),
		ticking: make(chan struct{}, 1),
	}
	for i := range opts.Checks {
		if opts.Checks[i].Name == notify.CheckNetwork {
			r.network = i
			// Not read yet: at boot the network check may pause in the
			// boot quiet window before its first run.
			r.networkUnknown = true

			break
		}
	}
	r.sends.bind(&r.opts)
	r.notifier = notify.New(notify.Options{
		Host:           opts.Host,
		Mode:           opts.Mode,
		Telegram:       r.opts.Telegram,
		SMS:            r.opts.SMS,
		OnCheckerError: func(key, text string) { r.checker.CheckerError(key, text) },
		OnDrop:         r.dropped,
		Now:            opts.Now,
		Location:       opts.Location,
	})
	r.checker = notify.NewCheckerErrors(notify.CheckerOptions{
		Host:     opts.Host,
		Notifier: r.notifier,
		Mailer:   r.opts.Mailer,
		Marks:    r.marks,
		Logger:   opts.Logger,
		Now:      opts.Now,
		Location: opts.Location,
	})
	r.own = ownPersisters(r)

	return r
}

// withDefaults fills the unset options with their defaults.
func withDefaults(opts Options) Options {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Location == nil {
		opts.Location = time.Local //nolint:gosmopolitan // "since" times are shown in the host's wall-clock time
	}
	if opts.Logger == nil {
		opts.Logger = zap.NewNop()
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = DefaultConcurrency
	}
	if opts.RunBudget <= 0 {
		opts.RunBudget = MaxRunTime
	}
	if opts.TickBudget <= 0 {
		opts.TickBudget = MaxTickTime
	}
	if opts.TelegramFailingAfter <= 0 {
		opts.TelegramFailingAfter = DefaultTelegramFailingAfter
	}
	if opts.MaintenanceMaxHold <= 0 {
		opts.MaintenanceMaxHold = DefaultMaintenanceMaxHold
	}

	return opts
}

// Name implements app.Worker.
func (*Runner) Name() string { return WorkerName }

// Duration implements app.Worker.
func (*Runner) Duration() time.Duration { return Tick }

// Do runs one tick: heartbeat first, then network first, the other due
// checks, flush, dispatch, checkpoint. Inside a quiet window (maintenance or
// boot grace) the checks set to pause (Check.PauseInQuiet) are not run and
// the weekly job waits; every other step is the same, the daily job included
// (FR-026, R15).
// The tick sends nothing before its runs end; its deliveries (not Load, the
// daily job or the checkpoint) then end by its TickBudget, so a hung run never
// leaves them without a window and a slow channel never delays the next
// tick's heartbeat; what is not delivered stays queued.
func (r *Runner) Do(ctx context.Context) error {
	r.ticking <- struct{}{}
	defer func() { <-r.ticking }()

	return r.tick(ctx)
}

// Checkpoint writes the state and the pending records once more, outside a
// tick. The daemon calls it on stop, after everything that pushes has
// stopped: an event pushed or a message queued after the last tick (by the
// memory guard, a background job or a checker error) is otherwise lost at
// exit. It waits for a running tick, at most until ctx ends, and writes
// nothing before state was loaded.
func (r *Runner) Checkpoint(ctx context.Context) error {
	select {
	case r.ticking <- struct{}{}:
	case <-ctx.Done():
		return fmt.Errorf("checkpoint health state: %w", ctx.Err())
	}
	defer func() { <-r.ticking }()

	r.loadMu.Lock()
	loaded := r.loaded
	r.loadMu.Unlock()

	return r.checkpoint(ctx, &cycle{now: r.opts.Now()}, loaded)
}

// Load restores the engine, rate marks and retry queue from the store. Once it
// has succeeded it does nothing; Do calls it at the top of every tick, so
// calling it at start is optional. Load never sends anything, so it is only
// as slow as the store read: a failure is recorded, raised by the next Do as a
// (rate-limited) checker error, and retried next tick. State loaded late is
// merged into what memory built meanwhile: memory wins per condition key,
// stored queue items go first, and the later rate mark wins. A database of a
// newer schema (store.ErrSchemaTooNew) never loads: Load then gives up on the
// store and runs from memory, as without one, raising that once.
func (r *Runner) Load(ctx context.Context) error {
	r.loadMu.Lock()
	defer r.loadMu.Unlock()

	if r.loaded {
		return nil
	}
	if r.opts.Store == nil {
		r.loaded = true
		r.restore(r.opts.Persisters, store.State{})

		return nil
	}

	st, err := r.opts.Store.Load(ctx)
	if errors.Is(err, store.ErrSchemaTooNew) {
		// A newer build's database never loads: run from memory as without a
		// store, so the gated checks run, and never write over it.
		r.loadErr = fmt.Errorf("%w; running from memory", err)
		r.memoryOnly = true
		r.loaded = true
		r.restore(r.own, store.State{})
		r.restore(r.opts.Persisters, store.State{})

		return nil
	}
	if err != nil {
		r.loadErr = err

		return fmt.Errorf("load health state: %w", err)
	}
	r.loaded = true
	r.loadedKV = st.KV
	// The runner's own state goes first: the SMS fallback must never see the
	// restored queue without the SMS-sent set.
	r.restore(r.own, st)
	r.engine.Merge(st.Conditions)
	r.marks.Merge(st.RateMarks)
	r.notifier.Merge(st.Queue)
	r.restore(r.opts.Persisters, st)

	return nil
}

// Snapshot returns the state /status reports.
func (r *Runner) Snapshot() Snapshot {
	open := r.engine.OpenConditions()
	degraded := r.opts.Store != nil && r.opts.Store.Degraded()

	r.mu.Lock()
	defer r.mu.Unlock()

	s := Snapshot{
		Checks:        len(r.opts.Checks),
		Quiet:         r.quiet,
		StoreDegraded: degraded,
	}
	if !r.maint.HeldSince.IsZero() {
		s.HeldSince = r.maint.HeldSince.In(r.opts.Location)
	}
	for i := range r.opts.Checks {
		if r.opts.Checks[i].PauseInQuiet {
			s.Paused = append(s.Paused, r.opts.Checks[i].Name)
		}
	}
	if !r.lastCycle.IsZero() {
		s.LastCycle = r.lastCycle.In(r.opts.Location)
	}
	for i := range open {
		s.Open = append(s.Open, OpenCondition{
			Key: open[i].Key, Text: open[i].LastText, Since: open[i].StreakStart.In(r.opts.Location),
		})
	}

	return s
}

// IsOpen reports whether key has an open problem: one whose PROBLEM was sent
// and not yet resolved. A condition still in its streak is not open.
func (r *Runner) IsOpen(key string) bool {
	cond, ok := r.engine.Condition(key)

	return ok && cond.Open
}

// Clear returns a tracked condition to healthy without any message and
// records it in the alert log as cleared. It reports whether key was tracked.
func (r *Runner) Clear(key string) bool {
	cond, ok := r.engine.Condition(key)
	if !ok || !r.engine.Clear(key) {
		return false
	}
	r.notifier.ForgetEnded(func(k string) bool { return k == key })

	r.mu.Lock()
	defer r.mu.Unlock()

	r.logged = append(r.logged, store.Alert{
		At: r.opts.Now(), Kind: store.KindCleared, Check: cond.Check, Key: key, Text: cond.LastText,
	})

	return true
}

// CheckerError reports a problem of the monitoring itself, e.g. an invalid
// configuration entry found at start.
func (r *Runner) CheckerError(key, text string) {
	r.checker.CheckerError(key, text)
}

// Push sends text as a pushed event of check through the notifier, like the
// tick's own events: host prefix, default mode, and a failed delivery queued
// and retried (silently while the outside is known down). The alert log gets
// an event keyed by check with the one-line record instead of text, at the
// next checkpoint.
func (r *Runner) Push(ctx context.Context, check, text, record string) {
	r.PushMode(ctx, check, check, text, record, health.ModeDefault)
}

// PushMode is Push for an event with its own key, distinct from the check's,
// and a delivery mode (health.ModeDefault uses the notifier's default).
func (r *Runner) PushMode(ctx context.Context, check, key, text, record string, mode health.Mode) {
	// Notify queues a failed delivery and retries it; its error is always nil.
	_ = r.notifier.Notify(ctx, notify.Message{
		Kind: notify.KindEvent, Check: check, Key: key, Text: text, Mode: notify.Mode(mode),
	})

	r.mu.Lock()
	r.logged = append(r.logged, store.Alert{
		At: r.opts.Now(), Kind: store.KindEvent, Check: check, Key: key, Text: record,
	})
	r.mu.Unlock()
}

// ExpireQueue drops the queued messages created more than maxAge before now;
// each is logged as dropped. It returns them. The next checkpoint stores the
// queue without them.
func (r *Runner) ExpireQueue(now time.Time, maxAge time.Duration) []notify.QueuedMessage {
	return r.notifier.Expire(now, maxAge)
}

// tick is Do once no Checkpoint runs.
func (r *Runner) tick(ctx context.Context) error {
	c := &cycle{now: r.opts.Now()}
	r.writeHeartbeat(c)

	// A failure is reported as a checker error once the tick's sends begin;
	// the tick runs from memory and does not checkpoint, so stored state is
	// never overwritten unseen.
	loaded := r.Load(ctx) == nil

	reason := r.quietReason(c)
	quiet := reason != ""
	r.mu.Lock()
	r.quiet = reason
	r.mu.Unlock()

	if loaded {
		r.restoreSchedule()
	}
	if len(r.opts.Fixes) > 0 {
		c.before = r.engine.Conditions()
	}
	if !r.runChecks(ctx, c, quiet) {
		_, endSends := r.beginSends(ctx)
		defer endSends()
		r.saveInterruptedFixes(ctx, c, loaded)

		return fmt.Errorf("health tick interrupted: %w", ctx.Err())
	}
	r.takeFixes(c)
	// The daily job runs before the flush, so a queued message it expires is
	// never sent. It is local store work and judges nothing, so it runs in a
	// quiet window too: a long window never delivers a message past its
	// maximum age.
	if loaded {
		r.runDaily(ctx, c)
	}
	// The delivery budget starts only now, when the runs have ended.
	sendCtx, endSends := r.beginSends(ctx)
	defer endSends()
	// Queued messages (e.g. an outage's PROBLEMs) go before this tick's news.
	r.notifier.Flush(sendCtx)
	r.dispatch(sendCtx, c)
	r.dispatchFixes(sendCtx, c)
	// The weekly job pushes the summary after the older news; cut off by the
	// tick deadline, it is retried next tick. It waits for a non-quiet tick.
	if loaded && !quiet {
		r.runWeekly(sendCtx, c)
	}
	r.raise(c)
	r.noticeTelegramFailing()
	// After this tick's RESOLVEDs went out, a key whose condition ended
	// without one (good again after a reopen), or a stored key of such a
	// condition, leaves the SMS-sent set before it is saved.
	r.notifier.ForgetEnded(func(key string) bool { return !r.bad(key) })
	err := r.checkpoint(ctx, c, loaded)

	r.mu.Lock()
	r.lastCycle = c.now
	r.mu.Unlock()

	return err
}

// beginSends starts the tick's delivery budget (boundSends), then raises the
// load failures within it.
func (r *Runner) beginSends(ctx context.Context) (context.Context, func()) {
	sendCtx, endSends := r.boundSends(ctx)
	r.raiseLoadFailure()

	return sendCtx, endSends
}

// raiseLoadFailure reports the last failed Load and the persister restore
// failures, if any, as checker errors, once the load lock is released.
func (r *Runner) raiseLoadFailure() {
	r.loadMu.Lock()
	err, restoreErrs := r.loadErr, r.restoreErrs
	r.loadErr, r.restoreErrs = nil, nil
	r.loadMu.Unlock()

	if err != nil {
		r.CheckerError(keyLoad, "cannot load state: "+err.Error())
	}
	if len(restoreErrs) > 0 {
		// One message: the key is rate-limited, so a second one would be lost.
		texts := make([]string, 0, len(restoreErrs))
		for _, e := range restoreErrs {
			texts = append(texts, e.Error())
		}
		r.CheckerError(KeyRestore, "cannot restore state: "+strings.Join(texts, "; "))
	}
}

func (r *Runner) writeHeartbeat(c *cycle) {
	if r.opts.Heartbeat == nil {
		return
	}
	if err := r.opts.Heartbeat.WriteTick(c.now); err != nil {
		c.fail(keyHeartbeat, "cannot write heartbeat: "+err.Error())
	}
}

// quietReason probes the maintenance lock and the uptime, and observes the
// lock for the stuck limit. A probe that fails is reported and does not open a
// quiet window.
func (r *Runner) quietReason(c *cycle) string {
	held := false
	if r.opts.Maintenance != nil {
		var err error
		if held, err = r.opts.Maintenance.MaintenanceHeld(); err != nil {
			// A failed probe never opens the window, whatever it reported.
			held = false
			c.fail(keyMaintenance, "cannot probe maintenance lock: "+err.Error())
		} else {
			r.observeMaintenance(c, held)
		}
	}
	grace := r.opts.BootGrace
	var uptime time.Duration
	if r.opts.Uptime == nil {
		grace = 0
	} else if up, err := r.opts.Uptime.Uptime(); err != nil {
		c.fail(keyUptime, "cannot read uptime: "+err.Error())
		grace = 0
	} else {
		uptime = up
	}
	_, reason := health.Quiet(held, uptime, grace)

	return reason
}

// runChecks runs the network check first when due, sets the notifier's
// offline flag from its conditions, then runs the other eligible checks, all
// within one tick deadline of RunBudget. In a quiet tick the checks set to
// pause are skipped and stay due. It reports false, having applied nothing
// more, when ctx ends (shutdown).
func (r *Runner) runChecks(ctx context.Context, c *cycle, quiet bool) bool {
	tickCtx, cancel := context.WithTimeout(ctx, r.opts.RunBudget)
	defer cancel()

	if n := r.network; n >= 0 && !r.paused(n, quiet) && !r.running[n].Load() &&
		due(r.lastRun[n], c.now, r.opts.Checks[n].Interval) {
		r.lastRun[n] = c.now
		res := r.runOne(tickCtx, n)
		if ctx.Err() != nil {
			return false
		}
		r.networkUnknown = unjudged(res)
		r.apply(c, r.opts.Checks[n], res)
	}
	// While the network check is paused by a quiet window, its conditions
	// (and so the offline flag) stay as they were before the window.
	offline := r.bad(KeyNetwork)
	r.notifier.SetOffline(offline)

	// Telegram is still attempted while the network is unknown; only the
	// checks that need the internet wait for a readable network run.
	batch := r.eligible(c.now, offline || r.networkUnknown, quiet)
	results := r.runBatch(tickCtx, batch)
	if ctx.Err() != nil {
		return false
	}
	for i, idx := range batch {
		if results[i].started {
			r.lastRun[idx] = c.now // A check never started stays due.
		}
		r.apply(c, r.opts.Checks[idx], results[i].res)
	}
	r.raiseOverruns(c)

	return true
}

// tickDeadlineError fails a run that did not finish, or was not started,
// before the tick deadline.
type tickDeadlineError struct {
	started bool
	budget  time.Duration
}

func (e tickDeadlineError) Error() string {
	if e.started {
		return fmt.Sprintf("run did not finish before the tick deadline (%s)", e.budget)
	}

	return fmt.Sprintf("run not started before the tick deadline (%s)", e.budget)
}

// raiseOverruns reports the checks that ran out of the tick deadline. One is
// reported as its own checker error; several are one symptom (a host too
// overloaded to run its checks), so one checker error names them all.
func (r *Runner) raiseOverruns(c *cycle) {
	switch len(c.overruns) {
	case 0:
	case 1:
		check, text := c.overruns[0][0], c.overruns[0][1]
		c.failCheck(check, check+"/run/error", text)
	default:
		names := make([]string, len(c.overruns))
		for i, o := range c.overruns {
			names[i] = o[0]
		}
		c.fail(keyOverrun, fmt.Sprintf(
			"%d checks did not finish before the tick deadline (%s), the host may be overloaded: %s",
			len(names), r.opts.RunBudget, strings.Join(names, ", "),
		))
	}
}

// unjudged reports whether a network run left connectivity unknown: it
// failed, or returned no Good or Bad reading of KeyNetwork.
func unjudged(res health.CheckResult) bool {
	if res.Err != nil {
		return true
	}
	for _, rd := range res.Readings {
		if rd.Key == KeyNetwork && rd.Kind == health.State && rd.Verdict != health.Unknown {
			return false
		}
	}

	return true
}

// eligible returns the indexes of the non-network checks that are due, not
// paused by a quiet window, not still in flight, and neither need the
// internet while the outside is down or unknown (noInternet) nor depend on a
// bad condition.
func (r *Runner) eligible(now time.Time, noInternet, quiet bool) []int {
	var out []int
	for i := range r.opts.Checks {
		chk := &r.opts.Checks[i]
		switch {
		case i == r.network, r.paused(i, quiet), r.running[i].Load(), !due(r.lastRun[i], now, chk.Interval):
		case chk.NeedsInternet && noInternet:
		case chk.DependsOn != "" && r.bad(dependsOnKey(chk.DependsOn)):
		default:
			out = append(out, i)
		}
	}

	return out
}

// dependsOnKey resolves a depends_on condition key. The 001 network keys
// (network/router, network/internet) name the one network/connectivity
// condition that replaced them, so a 001 config keeps working. The literals
// mirror network.formerKeyRouter and network.formerKeyInternet; keep them in
// sync.
func dependsOnKey(key string) string {
	switch key {
	case "network/router", "network/internet":
		return KeyNetwork
	default:
		return key
	}
}

// paused reports whether the check at idx is held back by a quiet tick.
func (r *Runner) paused(idx int, quiet bool) bool {
	return quiet && r.opts.Checks[idx].PauseInQuiet
}

// runBatch starts the checks at the given indexes in order, at most
// Concurrency at a time, and returns their outcomes in the same order. Checks
// still waiting for a slot when tickCtx ends are not started and fail.
func (r *Runner) runBatch(tickCtx context.Context, batch []int) []outcome {
	out := make([]outcome, len(batch))
	sem := make(chan struct{}, r.opts.Concurrency)
	notStarted := health.CheckResult{Err: tickDeadlineError{budget: r.opts.RunBudget}}

	var wg sync.WaitGroup
	for i, idx := range batch {
		select {
		case sem <- struct{}{}:
		case <-tickCtx.Done():
		}
		if tickCtx.Err() != nil {
			for j := i; j < len(batch); j++ {
				out[j] = outcome{res: notStarted}
			}

			break
		}
		wg.Go(func() {
			defer func() { <-sem }()

			out[i] = outcome{res: r.runOne(tickCtx, idx), started: true}
		})
	}
	wg.Wait()

	return out
}

// runOne runs a check until the tick deadline or its interval, whichever
// comes first, and waits no longer than that. A run that overruns is
// abandoned: it fails with a timeout, its late result is dropped, and it
// stays marked running (so it is not launched again) until it returns.
func (r *Runner) runOne(tickCtx context.Context, idx int) health.CheckResult {
	chk := r.opts.Checks[idx]
	r.running[idx].Store(true)

	res := RunBounded(tickCtx, chk, chk.Interval, func() { r.running[idx].Store(false) })
	if errors.Is(res.Err, ErrContextDone) {
		return health.CheckResult{Err: tickDeadlineError{started: true, budget: r.opts.RunBudget}}
	}

	return res
}

// apply feeds one result to the engine. Err is a checker error; readings
// returned alongside it (a partial failure) are still applied, and conditions
// absent from them are untouched. An Unknown reading is a checker error and
// changes no state.
func (r *Runner) apply(c *cycle, chk health.Check, res health.CheckResult) {
	res.Readings = r.dropRetired(chk.Name, res.Readings)
	r.failErr(c, chk.Name, res.Err)
	for _, rd := range res.Readings {
		if rd.Kind != health.State || rd.Verdict != health.Unknown {
			continue
		}
		text := rd.Text
		if text == "" {
			text = rd.Key + " could not be read"
		}
		c.failCheck(chk.Name, rd.Key+"/unreadable", text)
	}
	for _, a := range r.engine.Apply(c.now, chk.Name, chk.Interval, chk.Threshold, res.Readings) {
		if a.Mode == health.ModeDefault {
			a.Mode = chk.Mode
		}
		c.alerts = append(c.alerts, a)
	}
	r.forget(c, chk.Name, res.Forget)
	for _, s := range res.Samples {
		c.samples = append(c.samples, store.SampleRow{At: c.now, Metric: s.Metric, Value: s.Value})
	}
}

// failErr raises one checker error per distinct key: each health.CheckError in
// err under its own key, anything else under the 001 key "<check>/run/error"
// so a plain error keeps its old key. Rate limiting is per key (RateMarks), so
// a new key is delivered while another key of the check is inside its window.
func (r *Runner) failErr(c *cycle, check string, err error) {
	if err == nil {
		return
	}
	if de := (tickDeadlineError{}); errors.As(err, &de) {
		c.overruns = append(c.overruns, [2]string{check, err.Error()})

		return
	}
	checks, plain := health.SplitErrors(err)
	for _, ce := range checks {
		c.failCheck(check, ce.Key(check), ce.Error())
	}
	if len(plain) > 0 {
		c.failCheck(check, check+"/run/error", errors.Join(plain...).Error())
	}
}

// forget silently clears the check's own conditions named by keys and queues
// their cleared records; another check's condition is left alone. The record
// reads "no longer judged", which fits a sensor that is gone or no longer
// judged and a 001 network key alike.
func (r *Runner) forget(c *cycle, check string, keys []string) {
	for _, key := range keys {
		cond, ok := r.engine.Condition(key)
		if !ok {
			continue
		}
		if cond.Check != check {
			r.opts.Logger.Debug("ignored a forget of another check's condition",
				zap.String("check", check), zap.String("key", key))

			continue
		}
		if r.engine.Clear(key) {
			r.notifier.ForgetEnded(func(k string) bool { return k == key })
			c.cleared = append(c.cleared, store.Alert{
				At: c.now, Kind: store.KindCleared, Check: check, Key: key, Text: "no longer judged",
			})
		}
	}
}

func (r *Runner) dispatch(ctx context.Context, c *cycle) {
	for i := range c.alerts {
		a := &c.alerts[i]
		// Notify queues a failed delivery and retries it; its error is always nil.
		_ = r.notifier.Notify(ctx, notify.Message{
			Kind:  messageKind(a.Kind),
			Check: a.Check,
			Key:   a.Key,
			Text:  a.Text,
			Since: a.StreakStart.In(r.opts.Location),
			Mode:  notify.Mode(a.Mode),
		})
	}
}

func (r *Runner) raise(c *cycle) {
	for _, e := range c.errs {
		r.checker.ReportCheck(e.check, e.key, e.text)
	}
}

// checkpoint hands the full snapshots and the pending records to the store.
// Until state has been loaded nothing is written (a snapshot would replace the
// unseen stored state) and the records stay pending. A write failure is
// reported (the store returns it once per outage) and the runner carries on
// from memory; the first write that succeeds afterwards sends the recovery
// notice (saveRecovered). The tick's results have been dispatched, so a
// shutdown arriving now must not lose them: the write outlives the
// cancellation of ctx by up to interruptedSaveTimeout, and only a write cut
// off by that is returned.
func (r *Runner) checkpoint(ctx context.Context, c *cycle, loaded bool) error {
	r.addPending(r.records(c), c.samples)
	if !r.persistent() {
		r.pendingAlerts, r.pendingSamples = nil, nil

		return nil
	}
	if !loaded {
		return nil
	}
	alerts, samples := r.takePending()

	st := store.State{
		Conditions: r.engine.Conditions(), RateMarks: r.marks.Export(), Queue: r.notifier.Export(),
		KV: r.scheduleKV(),
	}
	r.persist(&st)
	saveCtx, cancel := saveContext(ctx)
	defer cancel()
	err := r.opts.Store.Checkpoint(saveCtx, st, alerts, samples)
	switch {
	case err == nil:
		// A store still degraded returns nil for every failure after the first.
		if !r.opts.Store.Degraded() {
			r.saveRecovered()
		}
	case saveCtx.Err() != nil:
		return fmt.Errorf("checkpoint health state: %w", err)
	default:
		if r.checker.Report(keySave, "cannot save state: "+err.Error()) {
			r.saveReported = true
		}
	}

	return nil
}

// persistent reports whether the runner writes its state to a store.
func (r *Runner) persistent() bool {
	return r.opts.Store != nil && !r.memoryOnly
}

// saveContext returns the context of a checkpoint: it is not cancelled with
// ctx, but once ctx is done it ends after interruptedSaveTimeout, so a
// shutdown neither cuts the write off nor waits on a hung store for long.
func saveContext(ctx context.Context) (context.Context, context.CancelFunc) {
	saveCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stop := context.AfterFunc(ctx, func() { time.AfterFunc(interruptedSaveTimeout, cancel) })

	return saveCtx, func() {
		stop()
		cancel()
	}
}

// addPending appends the records waiting for the store, keeping the newest
// within store.MaxPendingAlerts and store.MaxPendingSamples: they grow while
// state cannot be loaded. The first drop of a run of drops is logged.
func (r *Runner) addPending(alerts []store.Alert, samples []store.SampleRow) {
	var da, ds int
	r.pendingAlerts, da = store.KeepNewest(append(r.pendingAlerts, alerts...), store.MaxPendingAlerts)
	r.pendingSamples, ds = store.KeepNewest(append(r.pendingSamples, samples...), store.MaxPendingSamples)
	if da+ds == 0 {
		return
	}
	if !r.pendingDropped {
		r.opts.Logger.Warn("dropped the oldest health records waiting for the state store",
			zap.Int("alerts", da), zap.Int("samples", ds))
	}
	r.pendingDropped = true
}

// takePending hands the pending records over to the store.
func (r *Runner) takePending() ([]store.Alert, []store.SampleRow) {
	alerts, samples := r.pendingAlerts, r.pendingSamples
	r.pendingAlerts, r.pendingSamples, r.pendingDropped = nil, nil, false

	return alerts, samples
}

// saveRecovered sends the one recovery notice of a reported state-write
// failure (FR-032, R20) through CheckerErrors.Recovered: by Telegram, and by
// email when the failure went by email; the failure's rate marks are given
// back, so a later failure is reported at once.
func (r *Runner) saveRecovered() {
	if !r.saveReported {
		return
	}
	r.saveReported = false
	r.checker.Recovered(keySave, "state can be saved again")
}

// records returns the alert-log records of this tick: clears made since the
// last tick, the engine's alerts, the fixes and the checker errors that went
// out.
func (r *Runner) records(c *cycle) []store.Alert {
	r.mu.Lock()
	alerts := r.logged
	r.logged = nil
	r.mu.Unlock()

	for i := range c.alerts {
		a := &c.alerts[i]
		alerts = append(alerts, store.Alert{
			At: c.now, Kind: storeKind(a.Kind), Check: a.Check, Key: a.Key, Text: a.Text,
		})
	}
	alerts = append(alerts, c.cleared...)
	alerts = append(alerts, fixRecords(c)...)
	for _, ce := range r.checker.Drain() {
		alerts = append(alerts, store.Alert{
			At: ce.At, Kind: store.KindCheckerError, Check: ce.Check, Key: ce.Key, Text: ce.Text,
		})
	}

	return alerts
}

// bad reports whether key is currently bad: in a streak or open.
func (r *Runner) bad(key string) bool {
	_, ok := r.engine.Condition(key)

	return ok
}

// dropped handles a message dropped from the retry queue undelivered (R19):
// it is logged, and a PROBLEM is never lost. A PROBLEM of the condition's
// current streak, still open, reopens the condition, so the next Bad reading
// raises it again; otherwise (an earlier streak, a new streak not yet open,
// or resolved and no longer tracked) it is listed ahead of the next message
// delivered to Telegram.
func (r *Runner) dropped(m notify.QueuedMessage) {
	r.logDrop(m)
	if m.Kind != string(notify.KindProblem) {
		return
	}

	// A stored CreatedAt has whole seconds, so the streak start is compared
	// at that precision.
	cond, ok := r.engine.Condition(m.Key)
	current := ok && !m.CreatedAt.Before(cond.StreakStart.Truncate(time.Second))
	if current && r.engine.Reopen(m.Key) {
		r.opts.Logger.Info("dropped PROBLEM reopened to be raised again", zap.String("key", m.Key))

		return
	}
	r.notifier.ListUndelivered(m)
}

func (r *Runner) logDrop(m notify.QueuedMessage) {
	r.opts.Logger.Warn("health message dropped from the retry queue",
		zap.Int64("id", m.ID), zap.String("channel", m.Channel),
		zap.String("kind", m.Kind), zap.String("key", m.Key),
		zap.Time("created_at", m.CreatedAt), zap.Int("attempts", m.Attempts))
}

// checkerErr is a checker error raised in a tick; check labels its message.
type checkerErr struct {
	check, key, text string
}

// fail raises a checker error labelled with the first segment of key.
func (c *cycle) fail(key, text string) {
	check, _, _ := strings.Cut(key, "/")
	c.failCheck(check, key, text)
}

// failCheck raises a checker error of check, labelled with its whole name.
func (c *cycle) failCheck(check, key, text string) {
	c.errs = append(c.errs, checkerErr{check: check, key: key, text: text})
}

func messageKind(k health.AlertKind) notify.Kind {
	switch k {
	case health.AlertProblem:
		return notify.KindProblem
	case health.AlertResolved:
		return notify.KindResolved
	case health.AlertEvent:
		return notify.KindEvent
	}

	return notify.KindEvent
}

func storeKind(k health.AlertKind) store.AlertKind {
	switch k {
	case health.AlertProblem:
		return store.KindProblem
	case health.AlertResolved:
		return store.KindResolved
	case health.AlertEvent:
		return store.KindEvent
	}

	return store.KindEvent
}
