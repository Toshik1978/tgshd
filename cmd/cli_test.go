package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/fx"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/Toshik1978/tgshd/pkg/health"
	hcfg "github.com/Toshik1978/tgshd/pkg/health/config"
	"github.com/Toshik1978/tgshd/pkg/health/runner"
	"github.com/Toshik1978/tgshd/pkg/health/store"
	"github.com/Toshik1978/tgshd/pkg/host/lockfile"
)

// testIO returns a cliIO over buffers with a fixed clock and environment.
func testIO(env map[string]string, now time.Time) (cio cliIO, stdout, stderr *bytes.Buffer) {
	stdout, stderr = &bytes.Buffer{}, &bytes.Buffer{}
	cio = cliIO{
		getenv: func(k string) string { return env[k] },
		stdout: stdout,
		stderr: stderr,
		now:    func() time.Time { return now },
		// The daemon's check providers, as in production.
		checkProviders: healthCheckProviders(),
	}

	return cio, stdout, stderr
}

func TestRunSubcommandDispatch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		args   []string
		wantOK bool
	}{
		{"no arguments runs the daemon", nil, false},
		{"unknown argument runs the daemon", []string{"serve"}, false},
		{"flag runs the daemon", []string{"--once"}, false},
		{"check is a subcommand", []string{"check"}, true},
		{"heartbeat is a subcommand", []string{"heartbeat"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cio, _, _ := testIO(nil, time.Unix(0, 0))
			code, ok := runSubcommand(tt.args, cio)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && code != exitError {
				// Both subcommands fail on a missing argument.
				t.Fatalf("code = %d, want %d", code, exitError)
			}
		})
	}
}

func TestParseCheckArgs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		args    []string
		wantErr bool
	}{
		{"double dash", []string{"--once"}, false},
		{"single dash", []string{"-once"}, false},
		{"missing --once", nil, true},
		{"positional argument", []string{"--once", "extra"}, true},
		{"unknown flag", []string{"--once", "--loop"}, true},
		{"once disabled", []string{"--once=false"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if err := parseCheckArgs(tt.args); (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestParseHeartbeatArgs(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		args    []string
		want    int
		wantErr bool
	}{
		{"seconds", []string{"--planned-reboot", "900"}, 900, false},
		{"equals form", []string{"--planned-reboot=900"}, 900, false},
		{"zero clears", []string{"--planned-reboot", "0"}, 0, false},
		{"maximum", []string{"--planned-reboot", "86400"}, lockfile.MaxPlannedSeconds, false},
		{"leading zero stays decimal", []string{"--planned-reboot", "010"}, 10, false},
		{"above maximum", []string{"--planned-reboot", "86401"}, 0, true},
		{"negative", []string{"--planned-reboot", "-1"}, 0, true},
		{"not a number", []string{"--planned-reboot", "soon"}, 0, true},
		{"hex is rejected", []string{"--planned-reboot", "0x10"}, 0, true},
		{"duration is rejected", []string{"--planned-reboot", "15m"}, 0, true},
		{"missing value", []string{"--planned-reboot"}, 0, true},
		{"missing flag", nil, 0, true},
		{"positional argument", []string{"--planned-reboot", "900", "now"}, 0, true},
		{"unknown flag", []string{"--reboot", "900"}, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseHeartbeatArgs(tt.args)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Fatalf("seconds = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestNewOneShotLoggerLogsOnlyErrors(t *testing.T) {
	for _, env := range []string{"", "production", "development"} {
		logger, err := newOneShotLogger(&config{AppEnv: env})
		if err != nil {
			t.Fatalf("env %q: newOneShotLogger() = %v", env, err)
		}
		if logger.Core().Enabled(zapcore.WarnLevel) {
			t.Errorf("env %q: warnings are enabled, want errors only", env)
		}
		if !logger.Core().Enabled(zapcore.ErrorLevel) {
			t.Errorf("env %q: errors are disabled", env)
		}
	}
}

func TestVerdictLabel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		v    health.Verdict
		want string
	}{
		{health.Good, "GOOD"},
		{health.Bad, "BAD"},
		{health.Unknown, "UNKNOWN"},
		{health.Verdict(42), "UNKNOWN"},
	}
	for _, tt := range tests {
		if got := verdictLabel(tt.v); got != tt.want {
			t.Errorf("verdictLabel(%d) = %q, want %q", tt.v, got, tt.want)
		}
	}
}

func TestWriteResult(t *testing.T) {
	t.Parallel()

	var (
		buf bytes.Buffer
		tl  tally
	)
	writeResult(&buf, "ups", health.CheckResult{
		Readings: []health.Reading{
			{Key: "ups/state", Verdict: health.Good, Text: "online"},
			{Key: "ups/charge", Verdict: health.Bad, Text: "battery\nlow"},
			{Key: "ups/voltage", Verdict: health.Unknown, Text: "not reported"},
		},
		Samples: []health.Sample{{Metric: "ups.charge", Value: 12.5}, {Metric: "ups.voltage", Value: 230}},
		Err:     errors.New("read ups status: driver not connected"),
	}, &tl)
	writeResult(&buf, "network", health.CheckResult{
		Readings: []health.Reading{{Key: "network/router", Verdict: health.Good, Text: "gateway answers"}},
	}, &tl)
	writeError(&buf, "config/http/web-app/invalid", "health.yaml: http/web-app: bad url", &tl)

	want := "GOOD ups/state: online\n" +
		"BAD ups/charge: battery low\n" +
		"UNKNOWN ups/voltage: not reported\n" +
		"SAMPLE ups.charge: 12.5\n" +
		"SAMPLE ups.voltage: 230\n" +
		"ERROR ups/run/error: read ups status: driver not connected\n" +
		"GOOD network/router: gateway answers\n" +
		"ERROR config/http/web-app/invalid: health.yaml: http/web-app: bad url\n"
	if got := buf.String(); got != want {
		t.Fatalf("output:\n%s\nwant:\n%s", got, want)
	}

	wantTally := tally{checks: 2, good: 2, bad: 1, unknown: 1, samples: 2, errors: 2}
	if tl != wantTally {
		t.Fatalf("tally = %+v, want %+v", tl, wantTally)
	}
	if got, want := tl.String(), "SUMMARY 2 checks, 4 readings (2 good, 1 bad, 1 unknown), 2 samples, 2 errors"; got != want {
		t.Fatalf("summary = %q, want %q", got, want)
	}
}

func TestRunChecksRunsEveryCheckOnceInOrder(t *testing.T) {
	t.Parallel()

	var (
		mu  sync.Mutex
		ran []string
	)
	release := make(chan struct{})
	defer close(release)
	mk := func(name string, run func(context.Context) health.CheckResult) health.Check {
		return health.Check{
			Name:     name,
			Interval: 50 * time.Millisecond,
			Run: func(ctx context.Context) health.CheckResult {
				mu.Lock()
				ran = append(ran, name)
				mu.Unlock()

				return run(ctx)
			},
		}
	}
	checks := []health.Check{
		mk("a", func(context.Context) health.CheckResult {
			return health.CheckResult{Readings: []health.Reading{{Key: "a/x", Verdict: health.Good, Text: "fine"}}}
		}),
		mk("b", func(context.Context) health.CheckResult { panic("boom") }),
		mk("c", func(context.Context) health.CheckResult {
			<-release // Hangs, ignoring its context.
			return health.CheckResult{}
		}),
	}
	// Neither dependencies nor the internet gate a one-shot run.
	checks[0].NeedsInternet = true
	checks[2].DependsOn = "a/x"

	var (
		buf bytes.Buffer
		tl  tally
	)
	if interrupted := runChecks(t.Context(), &buf, checks, nil, &tl, runner.MaxRunTime); interrupted {
		t.Fatal("nothing interrupted the run")
	}

	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(ran, []string{"a", "b", "c"}) {
		t.Fatalf("ran = %v", ran)
	}
	out := buf.String()
	for _, want := range []string{
		"GOOD a/x: fine\n",
		"ERROR b/run/error: panic: boom\n",
		// A run that overruns its interval is abandoned.
		"ERROR c/run/error: run did not finish within 50ms\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
	if tl.checks != 3 || tl.good != 1 || tl.errors != 2 {
		t.Fatalf("tally = %+v", tl)
	}
}

// TestRunChecksBoundedByTheRunBudget: like the daemon, a one-shot run is cut
// at the run budget when that is shorter than the check's interval.
func TestRunChecksBoundedByTheRunBudget(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	defer close(release)
	checks := []health.Check{{
		Name:     "slow",
		Interval: time.Hour,
		Run: func(context.Context) health.CheckResult {
			<-release // Hangs, ignoring its context.

			return health.CheckResult{}
		},
	}}

	var (
		buf bytes.Buffer
		tl  tally
	)
	start := time.Now()
	runChecks(t.Context(), &buf, checks, nil, &tl, 50*time.Millisecond)

	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("run took %v, want it bounded by the run budget", elapsed)
	}
	if want := "ERROR slow/run/error: run did not finish within 50ms\n"; !strings.Contains(buf.String(), want) {
		t.Fatalf("output lacks %q:\n%s", want, buf.String())
	}
}

func TestHeartbeatPath(t *testing.T) {
	t.Parallel()

	if got, err := heartbeatPath(""); err != nil || got != "/run/tgshd/heartbeat" {
		t.Fatalf("default = %q, %v", got, err)
	}

	path, _ := writeHealthConfig(t, "")
	want := filepath.Join(filepath.Dir(path), "heartbeat")
	if got, err := heartbeatPath(path); err != nil || got != want {
		t.Fatalf("configured = %q, %v; want %q", got, err, want)
	}

	// Another section's error does not hide the heartbeat path.
	path, _ = writeHealthConfig(t, "bogus: {}\n")
	if got, err := heartbeatPath(path); err != nil || got != filepath.Join(filepath.Dir(path), "heartbeat") {
		t.Fatalf("with an entry error = %q, %v", got, err)
	}

	if _, err := heartbeatPath(filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Fatal("a missing configuration must be an error")
	}
}

func TestRunHeartbeat(t *testing.T) {
	t.Parallel()

	cfgPath, _ := writeHealthConfig(t, "")
	hbPath := filepath.Join(filepath.Dir(cfgPath), "heartbeat")
	now := time.Unix(1_700_000_000, 0)
	cio, stdout, stderr := testIO(map[string]string{"BOT_HEALTH_CONFIG": cfgPath}, now)

	if code := runHeartbeat([]string{"--planned-reboot", "900"}, cio); code != exitOK {
		t.Fatalf("set: code %d, stderr %q", code, stderr)
	}
	hb, err := lockfile.ReadHeartbeat(hbPath)
	if err != nil || hb.PlannedUntil != now.Unix()+900 {
		t.Fatalf("heartbeat = %+v, %v", hb, err)
	}
	if !strings.Contains(stdout.String(), strconv.FormatInt(now.Unix()+900, 10)) {
		t.Fatalf("stdout = %q", stdout)
	}

	if code := runHeartbeat([]string{"--planned-reboot", "0"}, cio); code != exitOK {
		t.Fatalf("clear: code %d, stderr %q", code, stderr)
	}
	if hb, err = lockfile.ReadHeartbeat(hbPath); err != nil || hb.PlannedUntil != 0 {
		t.Fatalf("cleared heartbeat = %+v, %v", hb, err)
	}

	before, err := os.ReadFile(hbPath)
	if err != nil {
		t.Fatal(err)
	}
	stderr.Reset()
	if code := runHeartbeat([]string{"--planned-reboot", "86401"}, cio); code != exitError {
		t.Fatalf("out of range: code %d", code)
	}
	if !strings.Contains(stderr.String(), "usage: tgshd heartbeat --planned-reboot <seconds>") {
		t.Fatalf("stderr = %q", stderr)
	}
	if after, _ := os.ReadFile(hbPath); !bytes.Equal(before, after) {
		t.Fatal("a bad argument must not touch the heartbeat file")
	}
}

func TestRunHeartbeatErrors(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	unparsable := filepath.Join(dir, "broken.yaml")
	if err := os.WriteFile(unparsable, []byte("general: [unclosed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The heartbeat directory is a file: the write fails.
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	unwritable := filepath.Join(dir, "unwritable.yaml")
	body := "general:\n  heartbeat_file: " + filepath.Join(blocker, "heartbeat") + "\n"
	if err := os.WriteFile(unwritable, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, cfgPath := range []string{unparsable, unwritable} {
		cio, _, stderr := testIO(map[string]string{"BOT_HEALTH_CONFIG": cfgPath}, time.Unix(0, 0))
		if code := runHeartbeat([]string{"--planned-reboot", "60"}, cio); code != exitError {
			t.Fatalf("%s: code %d", cfgPath, code)
		}
		if stderr.Len() == 0 {
			t.Fatalf("%s: no error printed", cfgPath)
		}
	}
}

func TestRunCheckOnceWithoutHealthConfig(t *testing.T) {
	t.Parallel()

	cio, _, stderr := testIO(nil, time.Unix(0, 0))
	if code := runCheckOnce(t.Context(), &config{}, cio); code != exitError {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(stderr.String(), "BOT_HEALTH_CONFIG") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestRunCheckOnceUnparsableConfig(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "health.yaml")
	if err := os.WriteFile(path, []byte("general: [unclosed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cio, stdout, stderr := testIO(nil, time.Unix(0, 0))
	if code := runCheckOnce(t.Context(), &config{HealthConfig: path}, cio); code != exitError {
		t.Fatalf("code = %d", code)
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "health.yaml") {
		t.Fatalf("stdout = %q, stderr = %q", stdout, stderr)
	}
}

func TestRunCheckOnceNoChecksCreatesNothing(t *testing.T) {
	t.Parallel()

	path, stateDir := writeHealthConfig(t, "")
	cio, stdout, stderr := testIO(nil, time.Unix(0, 0))
	if code := runCheckOnce(t.Context(), &config{HealthConfig: path}, cio); code != exitOK {
		t.Fatalf("code = %d, stderr %q", code, stderr)
	}
	if got := stdout.String(); got != "SUMMARY 0 checks, 0 readings (0 good, 0 bad, 0 unknown), 0 samples, 0 errors\n" {
		t.Fatalf("stdout = %q", got)
	}
	if _, err := os.Stat(stateDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the state directory must not be created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "heartbeat")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the heartbeat must not be written: %v", err)
	}
}

func TestRunCheckOnceSetupErrorsExitOne(t *testing.T) {
	t.Parallel()

	// The ups section without BOT_NUT_* cannot run; the unknown section is an
	// entry error. Both are printed and the run still completes.
	path, _ := writeHealthConfig(t, "ups: {}\nbogus: {}\n")
	cio, stdout, _ := testIO(nil, time.Unix(0, 0))
	if code := runCheckOnce(t.Context(), &config{HealthConfig: path}, cio); code != exitError {
		t.Fatalf("code = %d", code)
	}
	out := stdout.String()
	for _, want := range []string{
		"ERROR config/bogus/invalid: ",
		"ERROR ups/config/missing: ",
		"SUMMARY 0 checks, 0 readings (0 good, 0 bad, 0 unknown), 0 samples, 2 errors\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout lacks %q:\n%s", want, out)
		}
	}
}

func TestRunCheckOnceLeavesTheStateDatabaseUntouched(t *testing.T) {
	t.Parallel()

	path, stateDir := writeHealthConfig(t, "")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(t.Context(), stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err = st.Close(); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(stateDir, store.FileName)
	old := time.Unix(1_600_000_000, 0)
	if err = os.Chtimes(dbPath, old, old); err != nil {
		t.Fatal(err)
	}
	before := dirListing(t, stateDir)

	cio, _, stderr := testIO(nil, time.Unix(0, 0))
	if code := runCheckOnce(t.Context(), &config{HealthConfig: path}, cio); code != exitOK {
		t.Fatalf("code = %d, stderr %q", code, stderr)
	}
	if stderr.Len() != 0 {
		t.Fatalf("the stored state must open read-only without warnings: %q", stderr)
	}

	info, err := os.Stat(dbPath)
	if err != nil || !info.ModTime().Equal(old) {
		t.Fatalf("health.db mtime changed: %v, %v", info.ModTime(), err)
	}
	if after := dirListing(t, stateDir); !slices.Equal(before, after) {
		t.Fatalf("state dir changed: %v -> %v", before, after)
	}
}

func TestRunCheckOnceUnreadableStateIsAWarning(t *testing.T) {
	t.Parallel()

	path, stateDir := writeHealthConfig(t, "")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Not a database: the run goes on without stored state.
	if err := os.WriteFile(filepath.Join(stateDir, store.FileName), []byte("not sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}
	cio, stdout, stderr := testIO(nil, time.Unix(0, 0))
	if code := runCheckOnce(t.Context(), &config{HealthConfig: path}, cio); code != exitOK {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(stderr.String(), "stored state not used") || !strings.Contains(stdout.String(), "SUMMARY ") {
		t.Fatalf("stdout = %q, stderr = %q", stdout, stderr)
	}
}

func TestCheckGraphIsComplete(t *testing.T) {
	t.Parallel()

	var sets []checkSet
	opts := checkGraph(&config{}, &healthSetup{}, zap.NewNop(), healthCheckProviders(), &sets)
	if err := fx.ValidateApp(opts...); err != nil {
		t.Fatalf("check --once graph: %v", err)
	}
}

func TestCollectCheckSetsUsesTheDaemonProviders(t *testing.T) {
	t.Parallel()

	hs := &healthSetup{Config: &hcfg.Config{
		Network:      &hcfg.Network{InternetTargets: []string{"198.51.100.20"}},
		UPS:          &hcfg.UPS{},
		Disks:        &hcfg.Disks{},
		Temperatures: &hcfg.Temperatures{},
		Kernel:       &hcfg.Kernel{},
		Containers:   &hcfg.Containers{},
		HTTP:         []hcfg.HTTP{{Name: "app", URL: "https://app.example.org/"}},
		Freshness:    []hcfg.Freshness{{Name: "backup", Path: "/var/lib/example/backup.stamp"}},
		Command:      []hcfg.Command{{Name: "queue", Run: []string{"/usr/local/bin/queue-ok"}}},
	}}
	cfg := &config{NutIP: "192.0.2.10", NutName: "ups", NutUser: "monitor", NutPassword: "secret"}

	sets, stop, err := collectCheckSets(t.Context(), cfg, hs, zap.NewNop(), healthCheckProviders())
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	set := mergeCheckSets(sets)
	names := make([]string, 0, len(set.Checks))
	for _, c := range set.Checks {
		names = append(names, c.Name)
	}
	want := []string{
		"command/queue", "containers", "disks", "freshness/backup", "http/app", "kernel", "network", "temps", "ups",
	}
	if !slices.Equal(names, want) || len(set.Errors) != 0 {
		t.Fatalf("checks = %v, errors = %+v", names, set.Errors)
	}
	// disks, temps, kernel, containers, freshness and command keep state.
	if len(set.Persisters) != 6 || set.Drives == nil || len(set.Background) != 1 || len(set.Fixes) != 1 {
		t.Fatalf("persisters = %d, drives = %v, background = %d, fixes = %d",
			len(set.Persisters), set.Drives, len(set.Background), len(set.Fixes))
	}
}

// persistingProviders contributes one check whose persister must be restored
// before it runs.
func persistingProviders(t *testing.T, p *fakePersister) fx.Option {
	t.Helper()

	return fx.Provide(fx.Annotate(
		func() checkSet {
			return checkSet{
				Checks: []health.Check{
					{Name: "kernel", Interval: time.Minute, Run: func(context.Context) health.CheckResult {
						p.mu.Lock()
						defer p.mu.Unlock()
						if len(p.restored) != 1 {
							t.Error("the check ran before its state was restored")
						}

						return health.CheckResult{}
					}},
				},
				Persisters: []runner.Persister{p},
			}
		},
		fx.ResultTags(healthCheckGroup),
	))
}

func TestRunCheckOnceRestoresTheStoredStateAndNeverSaves(t *testing.T) {
	t.Parallel()

	path, stateDir := writeHealthConfig(t, "")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(t.Context(), stateDir)
	if err != nil {
		t.Fatal(err)
	}
	stored := store.State{
		KV:        map[string]string{"kernel.state": "stored"},
		Baselines: []store.Baseline{{Serial: "TEST0000000001", Model: "Example NVMe SSD", Bus: "nvme", Retired: true}},
	}
	if err = st.Checkpoint(t.Context(), stored, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err = st.Close(); err != nil {
		t.Fatal(err)
	}

	p := &fakePersister{}
	cio, _, stderr := testIO(nil, time.Unix(0, 0))
	cio.checkProviders = persistingProviders(t, p)
	if code := runCheckOnce(t.Context(), &config{HealthConfig: path}, cio); code != exitOK {
		t.Fatalf("code = %d, stderr %q", code, stderr)
	}

	if len(p.restored) != 1 || p.restored[0].KV["kernel.state"] != "stored" ||
		len(p.restored[0].Baselines) != 1 || !p.restored[0].Baselines[0].Retired {
		t.Fatalf("restored = %+v, want the stored state", p.restored)
	}
	if p.saves != 0 {
		t.Fatalf("saved %d times; check --once must never save", p.saves)
	}
}

func TestRunCheckOnceWithoutStoredStateRestoresAnEmptyState(t *testing.T) {
	t.Parallel()

	path, _ := writeHealthConfig(t, "")
	p := &fakePersister{}
	cio, _, stderr := testIO(nil, time.Unix(0, 0))
	cio.checkProviders = persistingProviders(t, p)
	if code := runCheckOnce(t.Context(), &config{HealthConfig: path}, cio); code != exitOK {
		t.Fatalf("code = %d, stderr %q", code, stderr)
	}
	if len(p.restored) != 1 || p.restored[0].KV != nil || p.restored[0].Baselines != nil || p.saves != 0 {
		t.Fatalf("restored = %+v, saves = %d; want one empty restore and no save", p.restored, p.saves)
	}
}

type failingPersister struct{}

func (failingPersister) Restore(store.State) error {
	return errors.New("decode kernel state: bad json")
}
func (failingPersister) Save(*store.State) error { return errors.New("never called") }

func TestRunCheckOnceRestoreFailureIsASetupError(t *testing.T) {
	t.Parallel()

	path, _ := writeHealthConfig(t, "")
	cio, stdout, _ := testIO(nil, time.Unix(0, 0))
	cio.checkProviders = fx.Provide(fx.Annotate(
		func() checkSet { return checkSet{Persisters: []runner.Persister{failingPersister{}}} },
		fx.ResultTags(healthCheckGroup),
	))
	if code := runCheckOnce(t.Context(), &config{HealthConfig: path}, cio); code != exitError {
		t.Fatalf("code = %d", code)
	}
	if want := "ERROR health/state/restore: cannot restore state: decode kernel state: bad json\n"; !strings.Contains(
		stdout.String(), want,
	) {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
}

func dirListing(t *testing.T, dir string) []string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}

	return names
}

func TestRunChecksStopsWhenInterrupted(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ranLater := false
	checks := []health.Check{
		{Name: "a", Interval: time.Minute, Run: func(context.Context) health.CheckResult {
			cancel() // Ctrl-C while the check runs.

			return health.CheckResult{Readings: []health.Reading{{Key: "a/x", Verdict: health.Good, Text: "fine"}}}
		}},
		{Name: "b", Interval: time.Minute, Run: func(context.Context) health.CheckResult {
			ranLater = true

			return health.CheckResult{}
		}},
	}

	var (
		buf bytes.Buffer
		tl  tally
	)
	if !runChecks(ctx, &buf, checks, nil, &tl, runner.MaxRunTime) {
		t.Fatal("the run must report the interruption")
	}
	if ranLater {
		t.Fatal("no check may start after an interruption")
	}
	if got, want := buf.String(), "ERROR a/run/error: interrupted\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
	if tl.checks != 1 || tl.errors != 1 || tl.good != 0 {
		t.Fatalf("tally = %+v", tl)
	}
}

// fakeCheckProviders contributes the given checks to the health_checks group
// in place of the real providers.
func fakeCheckProviders(checks ...health.Check) fx.Option {
	return fx.Provide(fx.Annotate(
		func() checkSet { return checkSet{Checks: checks} },
		fx.ResultTags(healthCheckGroup),
	))
}

func TestRunCheckOnceBadVerdictExitsZero(t *testing.T) {
	t.Parallel()

	path, _ := writeHealthConfig(t, "")
	cio, stdout, stderr := testIO(nil, time.Unix(0, 0))
	cio.checkProviders = fakeCheckProviders(health.Check{
		Name: "fake", Interval: time.Minute, Run: func(context.Context) health.CheckResult {
			return health.CheckResult{
				Readings: []health.Reading{{Key: "fake/x", Verdict: health.Bad, Text: "broken"}},
				Samples:  []health.Sample{{Metric: "fake.value", Value: 7}},
				Err:      errors.New("partial read"),
			}
		},
	})

	if code := runCheckOnce(t.Context(), &config{HealthConfig: path}, cio); code != exitOK {
		t.Fatalf("a bad verdict and a failed run still mean it ran: code %d, stderr %q", code, stderr)
	}
	want := "BAD fake/x: broken\n" +
		"SAMPLE fake.value: 7\n" +
		"ERROR fake/run/error: partial read\n" +
		"SUMMARY 1 checks, 1 readings (0 good, 1 bad, 0 unknown), 1 samples, 1 errors\n"
	if got := stdout.String(); got != want {
		t.Fatalf("stdout:\n%s\nwant:\n%s", got, want)
	}
}

func TestRunCheckOnceInterruptedExitsOne(t *testing.T) {
	t.Parallel()

	path, _ := writeHealthConfig(t, "")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cio, stdout, stderr := testIO(nil, time.Unix(0, 0))
	cio.checkProviders = fakeCheckProviders(health.Check{
		Name: "slow", Interval: time.Minute, Run: func(ctx context.Context) health.CheckResult {
			cancel()
			<-ctx.Done()

			return health.CheckResult{Err: ctx.Err()}
		},
	})

	if code := runCheckOnce(ctx, &config{HealthConfig: path}, cio); code != exitError {
		t.Fatalf("code = %d", code)
	}
	if !strings.Contains(stdout.String(), "ERROR slow/run/error: interrupted\n") ||
		!strings.Contains(stdout.String(), "SUMMARY 1 checks") {
		t.Fatalf("stdout = %q", stdout)
	}
	if !strings.Contains(stderr.String(), "tgshd check: interrupted") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestWriteResultPrintsOneLinePerDistinctCheckerError(t *testing.T) {
	t.Parallel()

	var (
		buf bytes.Buffer
		tl  tally
	)
	err := errors.Join(
		health.CheckError{Subject: "TEST0000000001", Kind: "smart", Err: errors.New("read failed")},
		health.CheckError{Subject: "TEST0000000001", Kind: "smart", Err: errors.New("read failed again")},
		health.CheckError{Subject: "TEST0000000002", Kind: "smart", Err: errors.New("no\nanswer")},
		health.CheckError{Kind: "scan", Err: errors.New("lsblk failed")},
		errors.New("plain trouble"),
	)
	writeResult(&buf, "disks", health.CheckResult{Err: fmt.Errorf("run: %w", err)}, &tl)

	want := "ERROR disks/TEST0000000001/smart: TEST0000000001/smart: read failed\n" +
		"ERROR disks/TEST0000000002/smart: TEST0000000002/smart: no answer\n" +
		"ERROR disks/scan: scan: lsblk failed\n" +
		"ERROR disks/run/error: plain trouble\n"
	if got := buf.String(); got != want {
		t.Fatalf("output:\n%s\nwant:\n%s", got, want)
	}
	if tl.errors != 4 {
		t.Fatalf("errors = %d, want 4", tl.errors)
	}
}

// setUserVersion creates the state database of a one-shot run at the given
// schema version, with no tables.
func setUserVersion(t *testing.T, stateDir string, version int) {
	t.Helper()

	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(stateDir, store.FileName))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err = db.ExecContext(t.Context(), "PRAGMA user_version = "+strconv.Itoa(version)); err != nil {
		t.Fatal(err)
	}
}

func TestRunCheckOnceSchemaVersionFallback(t *testing.T) {
	t.Parallel()

	const v1Notice = "state database is at schema version 1; run the daemon once to migrate"
	tests := []struct {
		name       string
		version    int
		wantNotice bool
		wantWarn   string
	}{
		{name: "version one", version: 1, wantNotice: true},
		{name: "version zero is not claimed as one", version: 0, wantWarn: "stored state not used"},
		{name: "newer than the build", version: 99, wantWarn: "stored state not used"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			path, stateDir := writeHealthConfig(t, "")
			setUserVersion(t, stateDir, tt.version)
			cio, stdout, stderr := testIO(nil, time.Unix(0, 0))
			if code := runCheckOnce(t.Context(), &config{HealthConfig: path}, cio); code != exitOK {
				t.Fatalf("code = %d, stderr %q", code, stderr)
			}
			if !strings.Contains(stdout.String(), "SUMMARY ") {
				t.Fatalf("the run did not finish: %q", stdout)
			}
			if got := strings.Contains(stderr.String(), v1Notice); got != tt.wantNotice {
				t.Fatalf("v1 notice = %v, want %v: %q", got, tt.wantNotice, stderr)
			}
			if tt.wantWarn != "" && !strings.Contains(stderr.String(), tt.wantWarn) {
				t.Fatalf("stderr = %q, want %q", stderr, tt.wantWarn)
			}
		})
	}
}
