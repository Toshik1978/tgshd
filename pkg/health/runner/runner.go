// Package runner is the health worker: one 30 s tick that writes the
// heartbeat, honours quiet windows, runs due checks (network first), feeds the
// alert engine, delivers messages and checkpoints state (research R2).
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
	// MaxRunTime is the default tick budget: the network run and the batch
	// together finish within it; a check's shorter interval caps its run further.
	MaxRunTime = 45 * time.Second

	// KeyRouter and KeyInternet are the network conditions that, while bad,
	// mean the outside is known down.
	KeyRouter   = "network/router"
	KeyInternet = "network/internet"

	keyHeartbeat   = "health/heartbeat/write"
	keyMaintenance = "health/maintenance/probe"
	keyUptime      = "health/uptime/read"
	keyLoad        = "health/store/load"
	keySave        = "health/store/write"
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
	RunBudget   time.Duration
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
	Logger *zap.Logger
	// Now defaults to time.Now.
	Now func() time.Time
	// Location is the host's zone for "since" times; defaults to time.Local.
	Location *time.Location
	// Daily runs once per host-local day on a non-quiet tick (e.g. store
	// retention); nil disables it. Weekly runs once per WeeklySlot on a
	// non-quiet tick; it needs both Weekly and WeeklySlot. A job counts as done
	// when attempted, so a failing hook is not retried until its next day or
	// week; the error is reported as a checker error.
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
	// LastCycle is the end of the last judged cycle (zero before the first).
	LastCycle time.Time
	// Quiet is the active quiet-window reason, empty when none.
	Quiet         string
	StoreDegraded bool
}

// Runner is the health app.Worker.
type Runner struct {
	opts     Options
	engine   *health.Engine
	marks    *health.RateMarks
	notifier *notify.Notifier
	checker  *notify.CheckerErrors
	network  int // index of the network check, -1 when absent

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

	loadMu sync.Mutex
	loaded bool
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
	samples []store.SampleRow
	fixes   []Fix
	errs    [][2]string // checker errors as key, text
	// before are the conditions at the start of the tick's runs; kept only
	// when there are fix sources (see saveInterruptedFixes).
	before []health.Condition
}

// New creates a Runner and its notifier and checker errors.
func New(opts Options) *Runner {
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

	r := &Runner{
		opts:    opts,
		engine:  health.NewEngine(),
		marks:   health.NewRateMarks(),
		network: -1,
		lastRun: make([]time.Time, len(opts.Checks)),
		running: make([]atomic.Bool, len(opts.Checks)),
	}
	for i := range opts.Checks {
		if opts.Checks[i].Name == notify.CheckNetwork {
			r.network = i

			break
		}
	}
	r.notifier = notify.New(notify.Options{
		Host:           opts.Host,
		Mode:           opts.Mode,
		Telegram:       opts.Telegram,
		SMS:            opts.SMS,
		OnCheckerError: func(key, text string) { r.checker.CheckerError(key, text) },
		OnDrop:         r.logDrop,
		Now:            opts.Now,
	})
	r.checker = notify.NewCheckerErrors(notify.CheckerOptions{
		Host:     opts.Host,
		Notifier: r.notifier,
		Mailer:   opts.Mailer,
		Marks:    r.marks,
		Logger:   opts.Logger,
		Now:      opts.Now,
	})

	return r
}

// Name implements app.Worker.
func (*Runner) Name() string { return WorkerName }

// Duration implements app.Worker.
func (*Runner) Duration() time.Duration { return Tick }

// Do runs one tick: heartbeat first, then nothing more inside a quiet window;
// otherwise network first, the other due checks, flush, dispatch, checkpoint.
func (r *Runner) Do(ctx context.Context) error {
	c := &cycle{now: r.opts.Now()}
	r.writeHeartbeat(c)
	// A failure is reported as a checker error; the tick runs from memory and
	// does not checkpoint, so stored state is never overwritten unseen.
	loaded := r.Load(ctx) == nil
	r.raiseLoadFailure()

	reason := r.quietReason(c)
	r.mu.Lock()
	r.quiet = reason
	r.mu.Unlock()
	if reason != "" {
		r.raise(c)

		return nil
	}

	if loaded {
		r.restoreSchedule()
	}
	if len(r.opts.Fixes) > 0 {
		c.before = r.engine.Conditions()
	}
	if !r.runChecks(ctx, c) {
		r.saveInterruptedFixes(ctx, c, loaded)

		return fmt.Errorf("health tick interrupted: %w", ctx.Err())
	}
	r.takeFixes(c)
	// The daily job runs before the flush, so a queued message it expires is
	// never sent.
	if loaded {
		r.runDaily(ctx, c)
	}
	// Queued messages (e.g. an outage's PROBLEMs) go before this tick's news.
	r.notifier.Flush(ctx)
	r.dispatch(ctx, c)
	r.dispatchFixes(ctx, c)
	// The weekly job pushes the summary after the older news.
	if loaded {
		r.runWeekly(ctx, c)
	}
	r.raise(c)
	err := r.checkpoint(ctx, c, loaded)

	r.mu.Lock()
	r.lastCycle = c.now
	r.mu.Unlock()

	return err
}

// Load restores the engine, rate marks and retry queue from the store. Once it
// has succeeded it does nothing; Do calls it at the top of every tick, so
// calling it at start is optional. Load never sends anything, so it is only
// as slow as the store read: a failure is recorded, raised by the next Do as a
// (rate-limited) checker error, and retried next tick. State loaded late is
// merged into what memory built meanwhile: memory wins per condition key,
// stored queue items go first, and the later rate mark wins.
func (r *Runner) Load(ctx context.Context) error {
	r.loadMu.Lock()
	defer r.loadMu.Unlock()

	if r.loaded {
		return nil
	}
	if r.opts.Store == nil {
		r.loaded = true
		r.restore(store.State{})

		return nil
	}

	st, err := r.opts.Store.Load(ctx)
	if err != nil {
		r.loadErr = err

		return fmt.Errorf("load health state: %w", err)
	}
	r.loaded = true
	r.loadedKV = st.KV
	r.engine.Merge(st.Conditions)
	r.marks.Merge(st.RateMarks)
	r.notifier.Merge(st.Queue)
	r.restore(st)

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

// Clear returns a tracked condition to healthy without any message and
// records it in the alert log as cleared. It reports whether key was tracked.
func (r *Runner) Clear(key string) bool {
	cond, ok := r.engine.Condition(key)
	if !ok || !r.engine.Clear(key) {
		return false
	}

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
	// Notify queues a failed delivery and retries it; its error is always nil.
	_ = r.notifier.Notify(ctx, notify.Message{Kind: notify.KindEvent, Check: check, Key: check, Text: text})

	r.mu.Lock()
	r.logged = append(r.logged, store.Alert{
		At: r.opts.Now(), Kind: store.KindEvent, Check: check, Key: check, Text: record,
	})
	r.mu.Unlock()
}

// ExpireQueue drops the queued messages created more than maxAge before now;
// each is logged as dropped. It returns them. The next checkpoint stores the
// queue without them.
func (r *Runner) ExpireQueue(now time.Time, maxAge time.Duration) []notify.QueuedMessage {
	return r.notifier.Expire(now, maxAge)
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

// quietReason probes the maintenance lock and the uptime. A probe that fails
// is reported and does not open a quiet window.
func (r *Runner) quietReason(c *cycle) string {
	held := false
	if r.opts.Maintenance != nil {
		var err error
		if held, err = r.opts.Maintenance.MaintenanceHeld(); err != nil {
			c.fail(keyMaintenance, "cannot probe maintenance lock: "+err.Error())
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
// within one tick deadline of RunBudget. It reports false, having applied
// nothing more, when ctx ends (shutdown).
func (r *Runner) runChecks(ctx context.Context, c *cycle) bool {
	tickCtx, cancel := context.WithTimeout(ctx, r.opts.RunBudget)
	defer cancel()

	if n := r.network; n >= 0 && !r.running[n].Load() && due(r.lastRun[n], c.now, r.opts.Checks[n].Interval) {
		r.lastRun[n] = c.now
		res := r.runOne(tickCtx, n)
		if ctx.Err() != nil {
			return false
		}
		r.apply(c, r.opts.Checks[n], res)
	}
	offline := r.bad(KeyRouter) || r.bad(KeyInternet)
	r.notifier.SetOffline(offline)

	batch := r.eligible(c.now, offline)
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

	return true
}

// eligible returns the indexes of the non-network checks that are due, not
// still in flight, and neither need the internet while offline nor depend on
// a bad condition.
func (r *Runner) eligible(now time.Time, offline bool) []int {
	var out []int
	for i := range r.opts.Checks {
		chk := &r.opts.Checks[i]
		switch {
		case i == r.network, r.running[i].Load(), !due(r.lastRun[i], now, chk.Interval):
		case chk.NeedsInternet && offline:
		case chk.DependsOn != "" && r.bad(chk.DependsOn):
		default:
			out = append(out, i)
		}
	}

	return out
}

// runBatch starts the checks at the given indexes in order, at most
// Concurrency at a time, and returns their outcomes in the same order. Checks
// still waiting for a slot when tickCtx ends are not started and fail.
func (r *Runner) runBatch(tickCtx context.Context, batch []int) []outcome {
	out := make([]outcome, len(batch))
	sem := make(chan struct{}, r.opts.Concurrency)
	notStarted := health.CheckResult{
		Err: fmt.Errorf("run not started before the tick deadline (%s)", r.opts.RunBudget),
	}

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
		return health.CheckResult{
			Err: fmt.Errorf("run did not finish before the tick deadline (%s)", r.opts.RunBudget),
		}
	}

	return res
}

// apply feeds one result to the engine. Err is a checker error; readings
// returned alongside it (a partial failure) are still applied, and conditions
// absent from them are untouched. An Unknown reading is a checker error and
// changes no state.
func (r *Runner) apply(c *cycle, chk health.Check, res health.CheckResult) {
	res.Readings = r.dropRetired(chk.Name, res.Readings)
	if res.Err != nil {
		c.fail(chk.Name+"/run/error", res.Err.Error())
	}
	for _, rd := range res.Readings {
		if rd.Kind != health.State || rd.Verdict != health.Unknown {
			continue
		}
		text := rd.Text
		if text == "" {
			text = rd.Key + " could not be read"
		}
		c.fail(rd.Key+"/unreadable", text)
	}
	for _, a := range r.engine.Apply(c.now, chk.Name, chk.Interval, chk.Threshold, res.Readings) {
		if a.Mode == health.ModeDefault {
			a.Mode = chk.Mode
		}
		c.alerts = append(c.alerts, a)
	}
	for _, s := range res.Samples {
		c.samples = append(c.samples, store.SampleRow{At: c.now, Metric: s.Metric, Value: s.Value})
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
		r.CheckerError(e[0], e[1])
	}
}

// checkpoint hands the full snapshots and the pending records to the store.
// Until state has been loaded nothing is written (a snapshot would replace the
// unseen stored state) and the records stay pending. A write failure is
// reported (the store returns it once per outage) and the runner carries on
// from memory; only a cancelled context is returned.
func (r *Runner) checkpoint(ctx context.Context, c *cycle, loaded bool) error {
	r.pendingAlerts = append(r.pendingAlerts, r.records(c)...)
	r.pendingSamples = append(r.pendingSamples, c.samples...)
	if r.opts.Store == nil {
		r.pendingAlerts, r.pendingSamples = nil, nil

		return nil
	}
	if !loaded {
		return nil
	}
	alerts, samples := r.pendingAlerts, r.pendingSamples
	r.pendingAlerts, r.pendingSamples = nil, nil

	st := store.State{
		Conditions: r.engine.Conditions(), RateMarks: r.marks.Export(), Queue: r.notifier.Export(),
		KV: r.scheduleKV(),
	}
	r.persist(&st)
	err := r.opts.Store.Checkpoint(ctx, st, alerts, samples)
	switch {
	case err == nil:
	case ctx.Err() != nil:
		return fmt.Errorf("checkpoint health state: %w", err)
	default:
		r.CheckerError(keySave, "cannot save state: "+err.Error())
	}

	return nil
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

func (r *Runner) logDrop(m notify.QueuedMessage) {
	r.opts.Logger.Warn("health message dropped from the retry queue",
		zap.Int64("id", m.ID), zap.String("channel", m.Channel),
		zap.Time("created_at", m.CreatedAt), zap.Int("attempts", m.Attempts))
}

func (c *cycle) fail(key, text string) {
	c.errs = append(c.errs, [2]string{key, text})
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
