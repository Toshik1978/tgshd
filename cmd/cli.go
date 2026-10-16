package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.uber.org/fx"
	"go.uber.org/zap"

	"github.com/Toshik1978/tgshd/pkg/health"
	hcfg "github.com/Toshik1978/tgshd/pkg/health/config"
	"github.com/Toshik1978/tgshd/pkg/health/report"
	"github.com/Toshik1978/tgshd/pkg/health/runner"
	"github.com/Toshik1978/tgshd/pkg/health/store"
	"github.com/Toshik1978/tgshd/pkg/host/lockfile"
)

// Subcommands (contracts/cli.md). Any other first argument runs the daemon.
const (
	subcommandCheck     = "check"
	subcommandHeartbeat = "heartbeat"

	exitOK    = 0
	exitError = 1

	checkUsage     = "usage: tgshd check --once"
	heartbeatUsage = "usage: tgshd heartbeat --planned-reboot <seconds>"

	// checkStartTimeout bounds building the checks and starting their adapters.
	checkStartTimeout = 15 * time.Second
	// checkStopTimeout bounds stopping the adapters after the run.
	checkStopTimeout = 5 * time.Second

	// Printed verdicts.
	labelGood    = "GOOD"
	labelBad     = "BAD"
	labelUnknown = "UNKNOWN"

	// schemaV1Notice is printed when the daemon has not migrated the state
	// database since the upgrade; the run goes on without stored state.
	schemaV1Notice = "state database is at schema version 1; run the daemon once to migrate"
)

var (
	// errUsage marks a malformed command line.
	errUsage = errors.New("bad arguments")
	// errInterrupted replaces the result of a check cut short by a signal.
	errInterrupted = errors.New("interrupted")
)

// cliIO is what a subcommand reads from and writes to.
type cliIO struct {
	getenv func(string) string
	stdout io.Writer
	stderr io.Writer
	now    func() time.Time
	// checkProviders builds the checks of check --once: healthCheckProviders()
	// in production, so the CLI runs exactly the daemon's checks.
	checkProviders fx.Option
}

// tally counts what a one-shot run printed.
type tally struct {
	checks, good, bad, unknown, samples, errors int
}

// String renders the summary line.
func (t tally) String() string {
	return fmt.Sprintf("SUMMARY %d checks, %d readings (%d good, %d bad, %d unknown), %d samples, %d errors",
		t.checks, t.good+t.bad+t.unknown, t.good, t.bad, t.unknown, t.samples, t.errors)
}

// runSubcommand runs the subcommand named by args[0] and returns its exit
// code; ok is false when args name no subcommand and the daemon should run.
func runSubcommand(args []string, cio cliIO) (code int, ok bool) {
	if len(args) == 0 {
		return exitOK, false
	}
	switch args[0] {
	case subcommandCheck:
		return runCheck(args[1:], cio), true
	case subcommandHeartbeat:
		return runHeartbeat(args[1:], cio), true
	default:
		return exitOK, false
	}
}

// runCheck is `tgshd check --once`: it loads the environment and runs every
// configured check once, sending and writing nothing.
func runCheck(args []string, cio cliIO) int {
	if err := parseCheckArgs(args); err != nil {
		fmt.Fprintf(cio.stderr, "tgshd check: %v\n%s\n", err, checkUsage)

		return exitError
	}
	cfg, err := newConfig()
	if err != nil {
		fmt.Fprintf(cio.stderr, "tgshd check: %v\n", err)

		return exitError
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return runCheckOnce(ctx, cfg, cio)
}

// runCheckOnce builds the configured checks with real adapters and runs each
// once, printing every reading, sample and error, then a summary line. It
// takes no fix action (it prints what it would fix), writes no state, no
// heartbeat and takes no lock; the stored state is opened
// read-only when it exists. Exit 1 means a configuration or setup error (the
// checks that could be built still ran); bad verdicts and failed runs exit 0.
func runCheckOnce(ctx context.Context, cfg *config, cio cliIO) int {
	if cfg.HealthConfig == "" {
		fmt.Fprintln(cio.stderr, "tgshd check: BOT_HEALTH_CONFIG is not set")

		return exitError
	}
	hc, entryErrs := hcfg.Load(cfg.HealthConfig)
	if hc == nil {
		fmt.Fprintf(cio.stderr, "tgshd check: %v\n", entryErrs[0])

		return exitError
	}
	logger, err := newOneShotLogger(cfg)
	if err != nil {
		fmt.Fprintf(cio.stderr, "tgshd check: %v\n", err)

		return exitError
	}

	hs := &healthSetup{
		Config: hc, Host: hostName(cfg.HostName, os.Hostname), Errors: configErrors(entryErrs), OneShot: true,
	}
	closeState := attachState(ctx, hs, cio.stderr)
	defer closeState()

	sets, stopAdapters, err := collectCheckSets(ctx, cfg, hs, logger, cio.checkProviders)
	if err != nil {
		fmt.Fprintf(cio.stderr, "tgshd check: %v\n", err)

		return exitError
	}
	defer stopAdapters()

	set := mergeCheckSets(sets)
	restoreErrs := restoreChecks(ctx, hs.Store, set.Persisters, cio.stderr)
	setupErrs := slices.Concat(hs.Errors, set.Errors, restoreErrs)

	var t tally
	for _, e := range setupErrs {
		writeError(cio.stdout, e.Key, e.Text, &t)
	}
	interrupted := runChecks(ctx, cio.stdout, set.Checks, set.Fixes, &t, runner.MaxRunTime)
	if !interrupted {
		writeAsleep(cio.stdout, set.Drives, cio.now())
	}
	fmt.Fprintln(cio.stdout, t)

	switch {
	case interrupted:
		fmt.Fprintln(cio.stderr, "tgshd check: interrupted")

		return exitError
	case len(setupErrs) > 0:
		return exitError
	default:
		return exitOK
	}
}

// parseCheckArgs accepts exactly `--once`.
func parseCheckArgs(args []string) error {
	fs := flag.NewFlagSet(subcommandCheck, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	once := fs.Bool("once", false, "run every configured check once")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("%w: unexpected argument %q", errUsage, fs.Arg(0))
	}
	if !*once {
		return fmt.Errorf("%w: --once is required", errUsage)
	}

	return nil
}

// attachState sets hs.Store to the read-only stored state when there is one
// and returns the function that closes it. A state that exists but cannot be
// opened is a warning: the run goes on without it.
func attachState(ctx context.Context, hs *healthSetup, stderr io.Writer) (closeState func()) {
	st, err := openStateReadOnly(ctx, hs.Config.General.StateDir)
	if err != nil {
		var old *store.OldSchemaError
		if errors.As(err, &old) && old.Version == 1 {
			fmt.Fprintln(stderr, schemaV1Notice)
		} else {
			fmt.Fprintf(stderr, "tgshd check: stored state not used: %v\n", err)
		}
	}
	if st == nil {
		return func() {}
	}
	hs.Store = st

	return func() { _ = st.Close() }
}

// openStateReadOnly opens the stored state read-only; nil without an error
// when there is none. It never creates the directory or the database.
func openStateReadOnly(ctx context.Context, stateDir string) (*store.Store, error) {
	_, err := os.Stat(filepath.Join(stateDir, store.FileName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil //nolint:nilnil // No stored state is not an error for a one-shot run.
	}
	st, err := store.OpenReadOnly(ctx, stateDir)
	if err != nil {
		return nil, fmt.Errorf("open stored state: %w", err)
	}

	return st, nil
}

// restoreChecks hands the stored state to the checks' persisters, as the
// daemon's first load does, so the run judges against the stored baselines,
// cursors and counters. st is the read-only store (nil when there is none:
// an empty state is restored, like a first start). Nothing is ever saved back
// and no background job runs. A store that cannot be read is a warning; a
// persister that cannot restore is a setup error.
func restoreChecks(ctx context.Context, st stateStore, ps []runner.Persister, stderr io.Writer) []startError {
	var state store.State
	if st != nil && len(ps) > 0 {
		loaded, err := st.Load(ctx)
		if err != nil {
			fmt.Fprintf(stderr, "tgshd check: stored state not used: %v\n", err)
		} else {
			state = loaded
		}
	}

	var errs []startError
	for _, p := range ps {
		if err := p.Restore(state); err != nil {
			errs = append(errs, startError{Key: runner.KeyRestore, Text: "cannot restore state: " + err.Error()})
		}
	}

	return errs
}

// checkGraph is the fx graph of `check --once`: the daemon's check providers
// fed with the one-shot setup, collecting the check sets into sets. It holds
// no Telegram, SMS, mail or runner, so nothing can be sent.
func checkGraph(
	cfg *config, hs *healthSetup, logger *zap.Logger, providers fx.Option, sets *[]checkSet,
) []fx.Option {
	type in struct {
		fx.In

		Sets []checkSet `group:"health_checks"`
	}

	return []fx.Option{
		fx.NopLogger,
		fx.StartTimeout(checkStartTimeout),
		fx.StopTimeout(checkStopTimeout),
		fx.Supply(cfg, hs, logger),
		providers,
		fx.Invoke(func(p in) { *sets = p.Sets }),
	}
}

// collectCheckSets builds the checks through the providers
// (healthCheckProviders in production) and starts their adapters; stop
// releases them.
func collectCheckSets(
	ctx context.Context, cfg *config, hs *healthSetup, logger *zap.Logger, providers fx.Option,
) (sets []checkSet, stop func(), err error) {
	app := fx.New(checkGraph(cfg, hs, logger, providers, &sets)...)
	if err = app.Err(); err != nil {
		return nil, nil, fmt.Errorf("build checks: %w", err)
	}
	startCtx, cancel := context.WithTimeout(ctx, checkStartTimeout)
	defer cancel()
	if err = app.Start(startCtx); err != nil {
		return nil, nil, fmt.Errorf("start check adapters: %w", err)
	}

	stop = func() {
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), checkStopTimeout)
		defer cancel()
		_ = app.Stop(stopCtx)
	}

	return sets, stop, nil
}

// runChecks runs every check once, in order, bounded like the runner bounds
// it: by its interval or runBudget (the daemon's run budget), whichever is
// shorter. It prints its result into t, followed by the fixes it would
// have taken (one-shot checks never fix). Quiet windows, the internet gate and
// dependencies do not apply to a one-shot run. When ctx ends (Ctrl-C) the
// check in flight is reported as interrupted, no further check starts, and
// interrupted is true.
func runChecks(
	ctx context.Context, w io.Writer, checks []health.Check, fixes []runner.FixSource, t *tally,
	runBudget time.Duration,
) (interrupted bool) {
	for _, chk := range checks {
		if ctx.Err() != nil {
			return true
		}
		res := runner.RunBounded(ctx, chk, min(chk.Interval, runBudget), nil)
		if ctx.Err() != nil {
			// The result may be cut short by the interruption: do not judge it.
			res = health.CheckResult{Err: errInterrupted}
		}
		writeResult(w, chk.Name, res, t)
		writeFixes(w, fixes)
	}

	return ctx.Err() != nil
}

// writeResult prints a check's readings, then its samples, then its error.
func writeResult(w io.Writer, name string, res health.CheckResult, t *tally) {
	t.checks++
	for _, r := range res.Readings {
		label := verdictLabel(r.Verdict)
		switch label {
		case labelGood:
			t.good++
		case labelBad:
			t.bad++
		default:
			t.unknown++
		}
		fmt.Fprintf(w, "%s %s: %s\n", label, r.Key, oneLine(r.Text))
	}
	for _, s := range res.Samples {
		t.samples++
		fmt.Fprintf(w, "SAMPLE %s: %s\n", s.Metric, strconv.FormatFloat(s.Value, 'g', -1, 64))
	}
	writeCheckErrors(w, name, res.Err, t)
}

// writeCheckErrors prints one ERROR line per distinct key of err, keyed as the
// runner keys them: a health.CheckError under its own key, everything else
// joined under "<check>/run/error".
func writeCheckErrors(w io.Writer, check string, err error, t *tally) {
	if err == nil {
		return
	}
	checks, plain := health.SplitErrors(err)
	seen := make(map[string]bool, len(checks))
	for _, ce := range checks {
		key := ce.Key(check)
		if seen[key] {
			continue
		}
		seen[key] = true
		writeError(w, key, ce.Error(), t)
	}
	if len(plain) > 0 {
		writeError(w, check+"/run/error", errors.Join(plain...).Error(), t)
	}
}

// writeFixes prints the fixes a one-shot check decided on but did not take.
// Checks run one at a time, so these are the last check's.
func writeFixes(w io.Writer, fixes []runner.FixSource) {
	for _, src := range fixes {
		for _, f := range src.TakeFixes() {
			fmt.Fprintf(w, "WOULD FIX %s: %s\n", f.Key, oneLine(f.Text))
		}
	}
}

// writeAsleep prints the drives found in standby, which a one-shot run never
// wakes, with the time since their last read. book is the disks check's drive
// book, nil without one.
func writeAsleep(w io.Writer, book runner.DriveBook, now time.Time) {
	st, ok := book.(driveStatuses)
	if !ok {
		return
	}
	drives := st.Status()
	for i := range drives {
		d := &drives[i]
		if d.AsleepSince.IsZero() {
			continue
		}
		fmt.Fprintf(w, "ASLEEP %s/%s: last read %s\n", disksCheckName, d.Serial, report.Ago(now.Sub(d.LastReadAt)))
	}
}

// writeError prints one checker or setup error.
func writeError(w io.Writer, key, text string, t *tally) {
	t.errors++
	fmt.Fprintf(w, "ERROR %s: %s\n", key, oneLine(text))
}

// verdictLabel is the printed form of a verdict.
func verdictLabel(v health.Verdict) string {
	switch v {
	case health.Good:
		return labelGood
	case health.Bad:
		return labelBad
	case health.Unknown:
	}

	// Unknown, and any verdict this build does not know.
	return labelUnknown
}

// oneLine keeps a printed text on its line.
func oneLine(s string) string {
	return strings.NewReplacer("\r\n", " ", "\n", " ", "\r", " ").Replace(s)
}

// runHeartbeat is `tgshd heartbeat --planned-reboot <seconds>`: it sets the
// planned-reboot window in the heartbeat file (0 clears it) under the
// heartbeat lock and runs no checks.
func runHeartbeat(args []string, cio cliIO) int {
	seconds, err := parseHeartbeatArgs(args)
	if err != nil {
		fmt.Fprintf(cio.stderr, "tgshd heartbeat: %v\n%s\n", err, heartbeatUsage)

		return exitError
	}
	path, err := heartbeatPath(cio.getenv("BOT_HEALTH_CONFIG"))
	if err != nil {
		fmt.Fprintf(cio.stderr, "tgshd heartbeat: %v\n", err)

		return exitError
	}
	now := cio.now()
	if err = lockfile.SetPlanned(path, now, seconds); err != nil {
		fmt.Fprintf(cio.stderr, "tgshd heartbeat: %s: %v\n", path, err)

		return exitError
	}

	if seconds == 0 {
		fmt.Fprintf(cio.stdout, "%s: planned_until=0 (cleared)\n", path)
	} else {
		fmt.Fprintf(cio.stdout, "%s: planned_until=%d\n", path, now.Unix()+int64(seconds))
	}

	return exitOK
}

// parseHeartbeatArgs accepts exactly `--planned-reboot <seconds>`, a decimal
// number of seconds within 0..lockfile.MaxPlannedSeconds.
func parseHeartbeatArgs(args []string) (int, error) {
	seconds, set := 0, false

	fs := flag.NewFlagSet(subcommandHeartbeat, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Func("planned-reboot", "planned reboot window in seconds, 0 clears", func(v string) error {
		n, err := strconv.Atoi(v)
		if err != nil {
			return errors.New("not a whole number of seconds")
		}
		if n < 0 || n > lockfile.MaxPlannedSeconds {
			return fmt.Errorf("out of range 0..%d", lockfile.MaxPlannedSeconds)
		}
		seconds, set = n, true

		return nil
	})
	if err := fs.Parse(args); err != nil {
		return 0, fmt.Errorf("%w: %w", errUsage, err)
	}
	if fs.NArg() > 0 {
		return 0, fmt.Errorf("%w: unexpected argument %q", errUsage, fs.Arg(0))
	}
	if !set {
		return 0, fmt.Errorf("%w: --planned-reboot is required", errUsage)
	}

	return seconds, nil
}

// heartbeatPath is general.heartbeat_file of the health configuration, or its
// default when no configuration is given. An entry error elsewhere in the
// file does not matter; an unreadable file does.
func heartbeatPath(configPath string) (string, error) {
	if configPath == "" {
		// Parsing an empty document yields the configuration defaults, so the
		// default path has a single source of truth in the config package.
		// Parse returns nil only for an unparsable document; guard anyway.
		hc, errs := hcfg.Parse(nil, "")
		if hc == nil {
			return "", fmt.Errorf("default health configuration: %v", errs)
		}

		return hc.General.HeartbeatFile, nil
	}
	hc, errs := hcfg.Load(configPath)
	if hc == nil {
		return "", fmt.Errorf("health configuration: %w", errs[0])
	}

	return hc.General.HeartbeatFile, nil
}
