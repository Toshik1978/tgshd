package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"go.uber.org/fx"
	"go.uber.org/zap"

	"github.com/Toshik1978/tgshd/cmd/app"
	"github.com/Toshik1978/tgshd/pkg/command"
	"github.com/Toshik1978/tgshd/pkg/health"
	hcfg "github.com/Toshik1978/tgshd/pkg/health/config"
	"github.com/Toshik1978/tgshd/pkg/health/notify"
	"github.com/Toshik1978/tgshd/pkg/health/runner"
	"github.com/Toshik1978/tgshd/pkg/health/store"
	"github.com/Toshik1978/tgshd/pkg/host/lockfile"
	"github.com/Toshik1978/tgshd/pkg/host/sysfs"
	"github.com/Toshik1978/tgshd/pkg/telegram"
)

const (
	// healthCheckGroup is the fx value group of checkSet values: every check
	// provider in healthProviders contributes one.
	healthCheckGroup = `group:"health_checks"`

	// instanceLockName is the single-instance lock inside the state directory.
	instanceLockName = "tgshd.lock"
	stateDirMode     = 0o700
	// storeOpenTimeout bounds opening and migrating the state database.
	storeOpenTimeout = 30 * time.Second
	// startLoadTimeout bounds reading the stored state at start, well inside
	// fx's 15 s start timeout.
	startLoadTimeout = 5 * time.Second
)

// startError is a checker error raised once, right after the daemon started
// and the stored rate marks were loaded.
type startError struct {
	Key  string
	Text string
}

// checkSet is what one check provider contributes: the checks to run, the
// checker errors to raise at start (e.g. a section that cannot run), the
// persisters of the checks' own state, the drive book for /retire and the
// background jobs the daemon runs alongside (never check --once).
type checkSet struct {
	Checks     []health.Check
	Errors     []startError
	Persisters []runner.Persister
	// Drives is set by the disks check only; nil otherwise.
	Drives runner.DriveBook
	// Temps is set by the temperatures check only; nil otherwise.
	Temps      tempSensors
	Background []background
	// Fixes hand the fix actions of the command checks to the runner (under
	// check --once: what would have been fixed).
	Fixes []runner.FixSource
	// Stamps (the freshness checks) and Probe (the modem probe) feed the
	// weekly summary's services block; nil without those checks.
	Stamps freshnessStamps
	Probe  probeStatus
}

// healthSetup is the outcome of loading the health configuration and claiming
// the state directory.
type healthSetup struct {
	// Config is nil when health monitoring is disabled: the file cannot be
	// parsed, or another instance holds the state directory.
	Config *hcfg.Config
	// Host prefixes every pushed health message.
	Host string
	// Store is the daemon's state database, opened on first use and retried
	// on each later use while it cannot be opened; nil without the instance
	// lock (memory only). Under check --once it is the read-only view, nil
	// when there is none.
	Store stateStore
	// Errors are raised as checker errors at start.
	Errors []startError
	// OneShot is set by check --once: checks only report what they would
	// fix and never act.
	OneShot bool
}

// stateStore is the state database as the runner and the reports use it;
// *store.Deferred (the daemon) and *store.Store (check --once) satisfy it.
type stateStore interface {
	runner.Store
	reportStore
}

// healthParams are the dependencies of the health runner.
type healthParams struct {
	fx.In

	Lifecycle fx.Lifecycle
	Logger    *zap.Logger
	Config    *config
	Setup     *healthSetup
	Publisher command.Publisher
	// SMS is provided only when BOT_GAMMU_DSN is set.
	SMS    command.SmsSender `optional:"true"`
	Uptime *sysfs.Reader
	Sets   []checkSet `group:"health_checks"`
	// Open is the open state the temperatures check was built with, bound
	// here to the runner.
	Open *openState
}

// healthOut registers the runner and its commands; both are empty while
// health monitoring is disabled.
type healthOut struct {
	fx.Out

	Workers  []app.Worker       `group:"worker,flatten"`
	Handlers []telegram.Handler `group:"command_handler,flatten"`
}

// healthProviders provides host health monitoring (BOT_HEALTH_CONFIG) for fx.
func healthProviders() fx.Option {
	return fx.Options(
		fx.Provide(
			newHealthSetup,
			newHealth,
		),
		healthCheckProviders(),
	)
}

// healthCheckProviders provides every check type and the host sources the
// checks read. The daemon and `tgshd check --once` both build their checks
// from it, so a check added here is run by both.
//
// Each check type has a provider returning a checkSet into the health_checks
// group; a new check type adds one fx.Annotate(xxxChecks,
// fx.ResultTags(healthCheckGroup)) line here, and the adapters it needs go
// here too. A check provider may depend only on *config, *healthSetup,
// *zap.Logger, fx.Lifecycle and what this option provides (the check --once
// graph supplies nothing else; TestCheckGraphIsComplete guards it). It returns
// an empty checkSet when setup.Config is nil or its section is absent, must
// not need Docker or Postgres to construct (FR-019), and must not send or
// write anything while constructed: check --once only reads. State a check
// keeps across restarts goes through checkSet.Persisters (check --once
// restores it from the read-only store and never saves), and long-running
// work (a log reader) through checkSet.Background, never a lifecycle hook.
func healthCheckProviders() fx.Option {
	return fx.Provide(
		sysfs.NewDefault,
		newOpenState,
		fx.Annotate(networkChecks, fx.ResultTags(healthCheckGroup)),
		fx.Annotate(upsChecks, fx.ResultTags(healthCheckGroup)),
		fx.Annotate(disksChecks, fx.ResultTags(healthCheckGroup)),
		fx.Annotate(tempsChecks, fx.ResultTags(healthCheckGroup)),
		fx.Annotate(kernelChecks, fx.ResultTags(healthCheckGroup)),
		newLazyDocker,
		fx.Annotate(containersChecks, fx.ResultTags(healthCheckGroup)),
		fx.Annotate(httpChecks, fx.ResultTags(healthCheckGroup)),
		fx.Annotate(freshnessChecks, fx.ResultTags(healthCheckGroup)),
		fx.Annotate(commandChecks, fx.ResultTags(healthCheckGroup)),
		modemProbeChecks,
	)
}

// newHealthSetup loads the configuration, takes the instance lock and opens
// the state database. Only an unparsable file or a second instance disable
// health monitoring; every other failure is a checker error and the monitor
// runs with what it has.
func newHealthSetup(lc fx.Lifecycle, logger *zap.Logger, cfg *config) *healthSetup {
	hs := &healthSetup{Host: hostName(cfg.HostName, os.Hostname)}

	hc, errs := hcfg.Load(cfg.HealthConfig)
	hs.Errors = configErrors(errs)
	if hc == nil {
		logger.Error("Health configuration cannot be parsed; health monitoring disabled",
			zap.String("path", cfg.HealthConfig), zap.String("error", hs.Errors[0].Text))

		return hs
	}

	if !claimState(lc, logger, hs, hc.General.StateDir) {
		return &healthSetup{Host: hs.Host}
	}
	hs.Config = hc

	return hs
}

// claimState creates the state directory, takes the instance lock and opens
// the store, releasing both on stop. It reports false when another instance
// holds the lock.
func claimState(lc fx.Lifecycle, logger *zap.Logger, hs *healthSetup, dir string) bool {
	if err := os.MkdirAll(dir, stateDirMode); err != nil {
		hs.Errors = append(
			hs.Errors,
			startError{"health/state/create", "cannot create state directory: " + err.Error()},
		)
	}

	inst, err := lockfile.AcquireInstance(filepath.Join(dir, instanceLockName))
	switch {
	case errors.Is(err, lockfile.ErrAlreadyLocked):
		logger.Error("Another tgshd instance holds the health state; health monitoring disabled",
			zap.String("state_dir", dir))

		return false
	case err != nil:
		// Without the lock another daemon may own the database: never share
		// it, run from memory instead.
		hs.Errors = append(hs.Errors, startError{
			"health/lock/acquire", "cannot take the instance lock, running from memory: " + err.Error(),
		})

		return true
	}

	// The store's *sql.DB keeps its single connection for the daemon's life;
	// OpenReadOnly (the CLI) relies on that, so no connection lifetime is set.
	open := func(ctx context.Context) (*store.Store, error) {
		ctx, cancel := context.WithTimeout(ctx, storeOpenTimeout)
		defer cancel()

		return store.Open(ctx, dir)
	}
	st, err := open(context.Background())
	if err != nil {
		// The runner retries the open with its Load each tick and raises the
		// failure as a checker error; it runs from memory meanwhile.
		logger.Warn("Cannot open the state database yet; retrying every tick",
			zap.String("state_dir", dir), zap.Error(err))
	}
	deferred := store.NewDeferred(st, open)
	hs.Store = deferred

	lc.Append(fx.Hook{
		OnStop: func(context.Context) error {
			return closeState(deferred, inst)
		},
	})

	return true
}

// closeState closes the store, then releases the instance lock.
func closeState(st *store.Deferred, inst *lockfile.Instance) error {
	var errs []error
	if st != nil {
		errs = append(errs, st.Close())
	}
	if inst != nil {
		errs = append(errs, inst.Release())
	}

	return errors.Join(errs...)
}

// newHealth builds the runner, its daily and weekly jobs and its /status,
// /clear, /retire, /history, /temp and /disks commands, and runs the checks'
// background jobs. When the configuration could not be parsed it only reports
// that as a checker error.
func newHealth(p healthParams) healthOut {
	d := newDelivery(p.Config, p.Publisher, p.SMS)
	errs := slices.Concat(p.Setup.Errors, d.errors)

	if p.Setup.Config == nil {
		if len(p.Setup.Errors) > 0 {
			checker := notify.NewCheckerErrors(notify.CheckerOptions{
				Host:     p.Setup.Host,
				Notifier: notify.New(notify.Options{Host: p.Setup.Host, Telegram: d.telegram}),
				Mailer:   d.mailer,
				Marks:    health.NewRateMarks(),
				Logger:   p.Logger,
			})
			registerStart(p.Lifecycle, nil, 0, checker.CheckerError, errs)
		}

		return healthOut{}
	}

	set := mergeCheckSets(p.Sets)
	reports := newHealthReports(p.Setup, set, p.Uptime, p.Logger)
	opts := runnerOptions(p.Setup, d, set, p.Uptime, p.Logger)
	reports.hook(&opts)
	r := runner.New(opts)
	reports.bind(r)
	p.Open.bind(r)
	registerStart(p.Lifecycle, r.Load, startLoadTimeout, r.CheckerError, slices.Concat(errs, set.Errors))
	// After registerStart: the stored state is restored before a job (the
	// kernel log reader) starts.
	registerBackground(p.Lifecycle, set.Background, r.CheckerError, backgroundRetry)

	var retire telegram.Handler = noDrivesCommand{logger: p.Logger, publisher: p.Publisher}
	if set.Drives != nil {
		retire = command.NewRetireCommand(p.Logger, p.Publisher, r)
	}

	return healthOut{
		Workers: []app.Worker{r},
		Handlers: append([]telegram.Handler{
			command.NewStatusCommand(p.Logger, p.Publisher, r),
			command.NewClearCommand(p.Logger, p.Publisher, r),
			retire,
		}, reports.handlers(p.Logger, p.Publisher)...),
	}
}

// runnerOptions maps the setup, delivery and checks onto runner options.
// Absent facilities stay nil interfaces, never typed nils.
func runnerOptions(
	hs *healthSetup, d delivery, set checkSet, uptime *sysfs.Reader, logger *zap.Logger,
) runner.Options {
	g := hs.Config.General
	opts := runner.Options{
		Checks:      set.Checks,
		Persisters:  set.Persisters,
		Fixes:       set.Fixes,
		Drives:      set.Drives,
		BootGrace:   g.BootGrace.Duration(),
		Concurrency: g.Concurrency,
		Heartbeat:   heartbeatFile(g.HeartbeatFile),
		Uptime:      uptime,
		Host:        hs.Host,
		Mode:        notify.ModeTelegram,
		Telegram:    d.telegram,
		SMS:         d.sms,
		Mailer:      d.mailer,
		Logger:      logger,
		WeeklySlot:  weeklySlot(hs.Config.Summary, logger),
	}
	if g.MaintenanceLock != "" {
		opts.Maintenance = maintenanceFile(g.MaintenanceLock)
	}
	if hs.Store != nil {
		opts.Store = hs.Store
	}

	return opts
}

// registerStart loads the stored state in OnStart, then raises the start
// errors in the background so a slow Telegram or SMTP send never fails fx
// start.
//
// The load runs before the worker scheduler starts (the Application's hook is
// appended after this one), so a stored queue is never merged while the
// notifier flushes. It only reads the store (Runner.Load never sends; the next
// tick raises a failure) and loadTimeout keeps it well inside fx's start
// timeout. The errors are raised after the load so the stored rate marks keep
// them to once per 24 h across restarts.
func registerStart(
	lc fx.Lifecycle, load func(context.Context) error, loadTimeout time.Duration, raise func(key, text string),
	errs []startError,
) {
	done := make(chan struct{})

	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			if load != nil {
				loadCtx, cancel := context.WithTimeout(ctx, loadTimeout)
				_ = load(loadCtx) // A failure is raised and retried by the next tick.
				cancel()
			}

			go func() {
				defer close(done)

				for _, e := range errs {
					raise(e.Key, e.Text)
				}
			}()

			return nil
		},
		OnStop: func(ctx context.Context) error {
			select {
			case <-done:
			case <-ctx.Done():
			}

			return nil
		},
	})
}

// mergeCheckSets joins the check sets, ordering checks by name so the
// runner's order does not depend on fx's value-group order. Drives, Stamps
// and Probe are the first set's that has one (only one check type sets each).
func mergeCheckSets(sets []checkSet) checkSet {
	var out checkSet
	for i := range sets {
		s := &sets[i]
		out.Checks = append(out.Checks, s.Checks...)
		out.Errors = append(out.Errors, s.Errors...)
		out.Persisters = append(out.Persisters, s.Persisters...)
		out.Background = append(out.Background, s.Background...)
		out.Fixes = append(out.Fixes, s.Fixes...)
		if out.Temps == nil && s.Temps != nil {
			out.Temps = s.Temps
		}
		if out.Drives == nil && s.Drives != nil {
			out.Drives = s.Drives
		}
		if out.Stamps == nil && s.Stamps != nil {
			out.Stamps = s.Stamps
		}
		if out.Probe == nil && s.Probe != nil {
			out.Probe = s.Probe
		}
	}
	slices.SortStableFunc(out.Checks, func(a, b health.Check) int { return strings.Compare(a.Name, b.Name) })

	return out
}

// configErrors turns configuration entry errors into start errors keyed
// config/<entry>/invalid, or config/file/unreadable for the whole file.
func configErrors(errs []hcfg.EntryError) []startError {
	out := make([]startError, 0, len(errs))
	for _, e := range errs {
		key := "config/file/unreadable"
		if e.Entry != "" {
			key = "config/" + e.Entry + "/invalid"
		}
		out = append(out, startError{Key: key, Text: e.Error()})
	}

	return out
}

// newCheck wraps a check's Run with its configured common settings.
func newCheck(name string, c hcfg.Common, run func(context.Context) health.CheckResult) health.Check {
	return health.Check{
		Name:          name,
		Interval:      c.Interval.Duration(),
		Threshold:     c.Threshold.Duration(),
		Mode:          health.Mode(c.Mode),
		NeedsInternet: c.NeedsInternet,
		Run:           run,
	}
}

// weeklySlot converts the summary schedule; nil when there is no summary.
// The config package validates the time, so a parse failure is unexpected; it
// is logged and leaves the weekly job unscheduled.
func weeklySlot(s *hcfg.Summary, logger *zap.Logger) *runner.Slot {
	if s == nil {
		return nil
	}
	t, err := time.Parse("15:04", s.Time)
	if err != nil {
		logger.Error("Cannot parse the summary time; the weekly summary is not scheduled",
			zap.String("time", s.Time), zap.Error(err))

		return nil
	}

	return &runner.Slot{Weekday: s.Weekday, Hour: t.Hour(), Minute: t.Minute()}
}

// hostName is BOT_HOST_NAME when set, else the OS host name.
func hostName(override string, hostname func() (string, error)) string {
	if name := strings.TrimSpace(override); name != "" {
		return name
	}
	if name, err := hostname(); err == nil && name != "" {
		return name
	}

	return fallbackHostName
}
