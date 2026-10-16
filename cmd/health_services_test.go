package main

import (
	"context"
	"errors"
	"io/fs"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/fx/fxtest"
	"go.uber.org/zap"

	"github.com/Toshik1978/tgshd/pkg/health"
	"github.com/Toshik1978/tgshd/pkg/health/check/command"
	"github.com/Toshik1978/tgshd/pkg/health/check/freshness"
	hcfg "github.com/Toshik1978/tgshd/pkg/health/config"
	"github.com/Toshik1978/tgshd/pkg/health/runner"
	"github.com/Toshik1978/tgshd/pkg/health/store"
	"github.com/Toshik1978/tgshd/pkg/host/docker"
)

// fakeDocker is a dockerAPI over a fixed container table.
type fakeDocker struct {
	mu         sync.Mutex
	containers map[string]docker.Container
	err        error
	restarted  []string
	closed     bool
}

func (f *fakeDocker) List(context.Context) ([]docker.Container, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make([]docker.Container, 0, len(f.containers))
	for _, c := range f.containers {
		out = append(out, c)
	}

	return out, nil
}

func (f *fakeDocker) Inspect(_ context.Context, name string) (docker.Container, error) {
	if f.err != nil {
		return docker.Container{}, f.err
	}
	c, ok := f.containers[name]
	if !ok {
		return docker.Container{}, errors.New("no such container")
	}

	return c, nil
}

func (f *fakeDocker) Restart(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.restarted = append(f.restarted, name)

	return f.err
}

func (f *fakeDocker) Close() error {
	f.closed = true

	return nil
}

func (f *fakeDocker) restarts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.restarted)
}

// fakeProbe is a command.Runner returning a fixed exit code per argv[0].
type fakeProbe struct {
	mu    sync.Mutex
	codes map[string]int
	ran   [][]string
}

func (f *fakeProbe) Run(_ context.Context, argv []string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.ran = append(f.ran, slices.Clone(argv))

	return f.codes[argv[0]], nil
}

func (f *fakeProbe) runs() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.ran)
}

func lazyOver(api dockerAPI, err error) *lazyDocker {
	return &lazyDocker{newClient: func() (dockerAPI, error) { return api, err }}
}

func commonOf(interval, threshold time.Duration) hcfg.Common {
	return hcfg.Common{
		Interval: hcfg.Duration(interval), Threshold: hcfg.Duration(threshold), Mode: hcfg.ModeTLG,
	}
}

func TestServiceChecksFollowTheConfig(t *testing.T) {
	t.Parallel()

	d := lazyOver(&fakeDocker{}, nil)
	logger := zap.NewNop()
	for _, hs := range []*healthSetup{{}, {Config: &hcfg.Config{}}} {
		for name, set := range map[string]checkSet{
			"containers": containersChecks(hs, d), "http": httpChecks(hs, d),
			"freshness": freshnessChecks(hs), "command": commandChecks(hs, d, logger),
		} {
			if len(set.Checks)+len(set.Errors)+len(set.Persisters)+len(set.Background)+len(set.Fixes) != 0 {
				t.Fatalf("%s without a section: %+v", name, set)
			}
		}
	}

	hs := &healthSetup{Config: &hcfg.Config{
		Containers: &hcfg.Containers{Common: commonOf(time.Minute, 0)},
		HTTP: []hcfg.HTTP{
			{Name: "app", URL: "https://app.example.org/", Common: commonOf(time.Minute, 0)},
			{Name: "api", URL: "https://api.example.org/", Common: commonOf(time.Minute, 0)},
		},
		Freshness: []hcfg.Freshness{
			{Name: "backup", Path: "/var/lib/example/backup.stamp", Common: commonOf(time.Minute, 0)},
			{Name: "sync", Path: "/var/lib/example/sync.stamp", Common: commonOf(time.Minute, 0)},
		},
		Command: []hcfg.Command{{
			Name: "queue", Run: []string{"/usr/local/bin/queue-ok"}, Timeout: hcfg.Duration(time.Second),
			Common: commonOf(time.Minute, 0),
		}},
	}}
	if set := containersChecks(hs, d); len(set.Checks) != 1 || set.Checks[0].Name != containersCheckName ||
		len(set.Persisters) != 1 {
		t.Fatalf("containers section: %+v", set)
	}
	if set := httpChecks(hs, d); len(set.Checks) != 2 || len(set.Errors) != 0 {
		t.Fatalf("http entries: %+v", set)
	}
	// One shared state persists every freshness entry.
	if set := freshnessChecks(hs); len(set.Checks) != 2 || len(set.Persisters) != 1 {
		t.Fatalf("freshness entries: %+v", set)
	}
	if set := commandChecks(hs, d, logger); len(set.Checks) != 1 || len(set.Persisters) != 1 ||
		len(set.Fixes) != 1 || set.Checks[0].Name != "command/queue" {
		t.Fatalf("command entries: %+v", set)
	}
}

func TestBuildHTTPChecksUsesEachEntrysSettings(t *testing.T) {
	t.Parallel()

	entries := []hcfg.HTTP{
		{
			Name: "app", URL: "https://app.example.org/", Timeout: hcfg.Duration(time.Second),
			Status: [2]int{200, 399}, DependsOn: "command/queue",
			Interval: hcfg.Duration(2 * time.Minute), Threshold: hcfg.Duration(10 * time.Minute),
			Mode: hcfg.ModeSMS,
		},
		{
			Name: "api", URL: "https://api.example.org/", Timeout: hcfg.Duration(time.Second),
			Status: [2]int{200, 299}, DoH: "https://doh.example.org/dns-query",
			Interval: hcfg.Duration(time.Minute), Mode: hcfg.ModeTLG, NeedsInternet: true,
		},
		{Name: "broken", URL: "ftp://files.example.org/", Common: commonOf(time.Minute, 0)},
	}

	set := buildHTTPChecks(entries, lazyOver(&fakeDocker{}, nil))
	if len(set.Checks) != 2 {
		t.Fatalf("checks = %+v", set.Checks)
	}
	app, api := set.Checks[0], set.Checks[1]
	if app.Name != "http/app" || app.DependsOn != "command/queue" || app.Interval != 2*time.Minute ||
		app.Threshold != 10*time.Minute || app.Mode != health.ModeSMS || app.NeedsInternet {
		t.Fatalf("app = %+v", app)
	}
	if api.Name != "http/api" || api.DependsOn != "" || !api.NeedsInternet || api.Interval != time.Minute {
		t.Fatalf("api = %+v", api)
	}
	if len(set.Errors) != 1 || set.Errors[0].Key != "config/http/broken/invalid" {
		t.Fatalf("errors = %+v, want the unbuildable entry reported", set.Errors)
	}
}

func TestHTTPCheckJudgesItsContainer(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := lazyOver(&fakeDocker{containers: map[string]docker.Container{
		"web": {Name: "web", Running: true, Health: "unhealthy"},
	}}, nil)
	set := buildHTTPChecks([]hcfg.HTTP{{
		Name: "app", URL: srv.URL, Timeout: hcfg.Duration(5 * time.Second), Status: [2]int{200, 399},
		Container: "web", Common: commonOf(time.Minute, 0),
	}}, d)

	res := set.Checks[0].Run(t.Context())
	if !hasKey(res.Readings, "http/app", health.Bad) || res.Err != nil {
		t.Fatalf("result = %+v, want Bad for the unhealthy container", res)
	}
}

func TestMissingDockerLeavesOnlyDockerChecksUnknown(t *testing.T) {
	// The real adapter against a socket that does not exist (FR-019).
	t.Setenv("DOCKER_HOST", "unix://"+filepath.Join(t.TempDir(), "missing.sock"))

	stamp := filepath.Join(t.TempDir(), "backup.stamp")
	if err := os.WriteFile(stamp, []byte(strconv.FormatInt(time.Now().Unix(), 10)), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	hs := &healthSetup{Config: &hcfg.Config{
		Containers: &hcfg.Containers{Common: commonOf(time.Minute, 0)},
		HTTP: []hcfg.HTTP{
			{
				Name: "app", URL: srv.URL, Timeout: hcfg.Duration(5 * time.Second), Status: [2]int{200, 399},
				Container: "web", Common: commonOf(time.Minute, 0),
			},
			{
				Name: "plain", URL: srv.URL, Timeout: hcfg.Duration(5 * time.Second), Status: [2]int{200, 399},
				Common: commonOf(time.Minute, 0),
			},
		},
		Freshness: []hcfg.Freshness{{
			Name: "backup", Path: stamp, Content: hcfg.ContentEpoch, MaxAge: hcfg.Duration(time.Hour),
			Common: commonOf(time.Minute, 0),
		}},
	}}
	lc := fxtest.NewLifecycle(t)
	d := newLazyDocker(lc)
	lc.RequireStart()
	defer lc.RequireStop()
	set := mergeCheckSets([]checkSet{containersChecks(hs, d), httpChecks(hs, d), freshnessChecks(hs)})
	if len(set.Errors) != 0 {
		t.Fatalf("start errors = %+v; a missing Docker must not stop anything", set.Errors)
	}

	results := map[string]health.CheckResult{}
	for _, c := range set.Checks {
		results[c.Name] = c.Run(t.Context())
	}
	for _, name := range []string{"containers", "http/app"} {
		if res := results[name]; res.Err == nil || len(res.Readings) != 0 {
			t.Fatalf("%s = %+v, want a checker error and no verdict", name, res)
		}
	}
	if !hasKey(results["http/plain"].Readings, "http/plain", health.Good) {
		t.Fatalf("http/plain = %+v, want Good", results["http/plain"])
	}
	if !hasKey(results["freshness/backup"].Readings, "freshness/backup", health.Good) {
		t.Fatalf("freshness/backup = %+v, want Good", results["freshness/backup"])
	}
}

func TestLazyDocker(t *testing.T) {
	t.Parallel()

	started := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	api := &fakeDocker{containers: map[string]docker.Container{
		"web":      {Name: "web", Running: true, Health: "healthy", StartedAt: started},
		"plain":    {Name: "plain", Running: true},
		"none":     {Name: "none", Running: true, Health: "none"},
		"starting": {Name: "starting", Running: true, Health: "starting"},
		"sick":     {Name: "sick", Running: true, Health: "unhealthy"},
		"stopped":  {Name: "stopped", Status: "exited"},
	}}
	builds := 0
	failing := true
	d := &lazyDocker{newClient: func() (dockerAPI, error) {
		builds++
		if failing {
			return nil, errors.New("bad DOCKER_HOST")
		}

		return api, nil
	}}

	if _, err := d.List(t.Context()); err == nil {
		t.Fatal("List without a client: want error")
	}
	if _, err := d.ContainerHealthy(t.Context(), "web"); err == nil {
		t.Fatal("ContainerHealthy without a client: want error")
	}
	if err := d.Close(); err != nil {
		t.Fatalf("Close without a client: %v", err)
	}

	failing = false
	for name, want := range map[string]bool{
		"web": true, "plain": true, "none": true, "starting": false, "sick": false, "stopped": false,
	} {
		got, err := d.ContainerHealthy(t.Context(), name)
		if err != nil || got != want {
			t.Fatalf("ContainerHealthy(%s) = %v, %v; want %v", name, got, err, want)
		}
	}
	if _, err := d.ContainerHealthy(t.Context(), "absent"); err == nil {
		t.Fatal("ContainerHealthy(absent): want error")
	}
	if got, err := d.StartedAt(t.Context(), "web"); err != nil || !got.Equal(started) {
		t.Fatalf("StartedAt = %v, %v", got, err)
	}
	list, err := d.List(t.Context())
	if err != nil || len(list) != len(api.containers) {
		t.Fatalf("List = %+v, %v", list, err)
	}
	if err := d.Restart(t.Context(), "web"); err != nil || !slices.Equal(api.restarts(), []string{"web"}) {
		t.Fatalf("Restart: %v, restarted %v", err, api.restarts())
	}
	if builds != 3 {
		t.Fatalf("builds = %d, want retries until the client is built, then reuse", builds)
	}
	if err := d.Close(); err != nil || !api.closed {
		t.Fatalf("Close: %v, closed %v", err, api.closed)
	}
}

func TestBytesKV(t *testing.T) {
	t.Parallel()

	chk := command.New(hcfg.Command{Name: "queue", Run: []string{"queue-ok"}}, &fakeProbe{}, nil, time.Now, nil)
	kv := bytesKV{state: chk}
	if err := kv.Restore(""); err != nil {
		t.Fatalf("Restore of nothing (a fresh install): %v", err)
	}
	if err := kv.Restore(`{"strikes":1}`); err != nil {
		t.Fatal(err)
	}
	if got, err := kv.Export(); err != nil || !strings.Contains(got, `"strikes":1`) {
		t.Fatalf("Export = %q, %v", got, err)
	}
	if err := kv.Restore("{"); err == nil {
		t.Fatal("Restore of garbage: want error")
	}
}

func TestContainersStateSurvivesARestart(t *testing.T) {
	t.Parallel()

	api := &fakeDocker{containers: map[string]docker.Container{
		"web": {Name: "web", RestartPolicy: "always", Running: true, Status: "running"},
	}}
	set := buildContainersCheck(&hcfg.Containers{Common: commonOf(time.Minute, 0)}, lazyOver(api, nil), time.Now)
	if err := set.Persisters[0].Restore(store.State{}); err != nil {
		t.Fatalf("restore on a fresh install: %v", err)
	}
	set.Checks[0].Run(t.Context())

	var st store.State
	if err := set.Persisters[0].Save(&st); err != nil || !strings.Contains(st.KV[containersStateKey], "web") {
		t.Fatalf("saved kv = %v, %v", st.KV, err)
	}
}

func TestFreshnessStatePersists(t *testing.T) {
	t.Parallel()

	s := newFreshnessState([]string{"backup", "sync"})
	at := time.Unix(1_700_000_000, 0)
	stored := freshness.NewKVState(nil)
	stored.SetFirstMissing("backup", at)
	kv := stored.Export()
	kv["kernel.state"] = "not mine"

	if err := s.Restore(store.State{KV: kv}); err != nil {
		t.Fatal(err)
	}
	if got, ok := s.FirstMissing("backup"); !ok || !got.Equal(at) {
		t.Fatalf("FirstMissing = %v, %v", got, ok)
	}
	s.SetFirstMissing("sync", at)
	s.ClearFirstMissing("backup")

	// The store's kv is upsert-only, like the real one: a cleared mark must
	// be overwritten, or a restart would restore it.
	st := store.State{KV: maps.Clone(kv)}
	st.KV["kernel.state"] = "kept"
	saved := store.State{}
	if err := s.Save(&saved); err != nil {
		t.Fatal(err)
	}
	maps.Copy(st.KV, saved.KV)
	if st.KV["kernel.state"] != "kept" {
		t.Fatalf("saved kv = %v, want the other keys kept", st.KV)
	}

	restarted := newFreshnessState([]string{"backup", "sync"})
	if err := restarted.Restore(st); err != nil {
		t.Fatal(err)
	}
	if got, ok := restarted.FirstMissing("backup"); ok {
		t.Fatalf("cleared mark came back after a restart: %v", got)
	}
	if got, ok := restarted.FirstMissing("sync"); !ok || !got.Equal(at) {
		t.Fatalf("FirstMissing(sync) after a restart = %v, %v", got, ok)
	}
}

func TestOSFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	small := filepath.Join(dir, "stamp")
	big := filepath.Join(dir, "big")
	if err := os.WriteFile(small, []byte("1700000000\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(big, make([]byte, maxStampBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}

	var f osFiles
	if data, err := f.ReadFile(small); err != nil || string(data) != "1700000000\n" {
		t.Fatalf("ReadFile = %q, %v", data, err)
	}
	if _, err := f.ReadFile(big); err == nil {
		t.Fatal("ReadFile of an oversized file: want error")
	}
	if mt, err := f.ModTime(small); err != nil || mt.IsZero() {
		t.Fatalf("ModTime = %v, %v", mt, err)
	}
	for _, read := range []func(string) error{
		func(p string) error { _, err := f.ReadFile(p); return err },
		func(p string) error { _, err := f.ModTime(p); return err },
	} {
		if err := read(filepath.Join(dir, "missing")); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("missing file error = %v, want fs.ErrNotExist", err)
		}
	}
}

// failingCommand is a command entry whose probe fails and is fixed after one
// failure.
func failingCommand(fix hcfg.Fix, report hcfg.Report) hcfg.Command {
	fix.After, fix.MaxPerHour = 1, 3

	return hcfg.Command{
		Name: "queue", Run: []string{"queue-ok"}, Timeout: hcfg.Duration(time.Second), Fix: &fix,
		Report: report, Common: commonOf(time.Minute, 0),
	}
}

func TestCommandFixesAreHandedToTheRunner(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		report hcfg.Report
		mode   hcfg.Mode
		send   bool
	}{{hcfg.ReportAlert, hcfg.ModeSMS, true}, {hcfg.ReportJournal, hcfg.ModeTLGOrSMS, false}} {
		probe := &fakeProbe{codes: map[string]int{"queue-ok": 1}}
		api := &fakeDocker{containers: map[string]docker.Container{"worker": {Name: "worker"}}}
		cmd := failingCommand(hcfg.Fix{RestartContainer: "worker"}, tt.report)
		cmd.Mode = tt.mode
		set := buildCommandChecks([]hcfg.Command{cmd}, probe, lazyOver(api, nil), false, zap.NewNop())

		res := set.Checks[0].Run(t.Context())
		if !hasKey(res.Readings, "command/queue", health.Bad) {
			t.Fatalf("result = %+v", res)
		}
		if !slices.Equal(api.restarts(), []string{"worker"}) {
			t.Fatalf("restarted = %v, the daemon fixes", api.restarts())
		}
		fixes := set.Fixes[0].TakeFixes()
		if len(fixes) != 1 || fixes[0].Check != "command" || fixes[0].Key != "command/queue/fix" ||
			fixes[0].Send != tt.send || fixes[0].Mode != health.Mode(tt.mode) ||
			!strings.Contains(fixes[0].Text, "restarted container worker") {
			t.Fatalf("report %s: fixes = %+v", tt.report, fixes)
		}
		if again := set.Fixes[0].TakeFixes(); len(again) != 0 {
			t.Fatalf("fixes taken twice: %+v", again)
		}
	}
}

func TestOneShotCommandChecksNeverFix(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name string
		fix  hcfg.Fix
		want string
	}{
		{"restart", hcfg.Fix{RestartContainer: "worker"}, "command queue: fix would restart container worker"},
		{"run", hcfg.Fix{Run: []string{"queue-fix", "--secret-arg"}}, "command queue: fix would run queue-fix"},
		// A fix command identical to the probe still never runs.
		{"same argv", hcfg.Fix{Run: []string{"queue-ok"}}, "command queue: fix would run queue-ok"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			probe := &fakeProbe{codes: map[string]int{"queue-ok": 1}}
			api := &fakeDocker{containers: map[string]docker.Container{"worker": {Name: "worker"}}}
			cmd := failingCommand(tt.fix, hcfg.ReportAlert)
			set := buildCommandChecks([]hcfg.Command{cmd}, probe, lazyOver(api, nil), true, zap.NewNop())

			res := set.Checks[0].Run(t.Context())
			if !hasKey(res.Readings, "command/queue", health.Bad) || res.Err != nil {
				t.Fatalf("result = %+v", res)
			}
			if runs := probe.runs(); len(runs) != 1 || !slices.Equal(runs[0], []string{"queue-ok"}) {
				t.Fatalf("ran %v, want the probe only", runs)
			}
			if len(api.restarts()) != 0 {
				t.Fatalf("restarted %v in a one-shot run", api.restarts())
			}
			fixes := set.Fixes[0].TakeFixes()
			if len(fixes) != 1 || fixes[0].Text != tt.want {
				t.Fatalf("fixes = %+v, want %q", fixes, tt.want)
			}

			// The next run probes again.
			set.Checks[0].Run(t.Context())
			if runs := probe.runs(); len(runs) != 2 || !slices.Equal(runs[1], []string{"queue-ok"}) {
				t.Fatalf("ran %v, want the probe again", runs)
			}
		})
	}
}

func TestRunCheckOnceReportsButNeverRunsFixes(t *testing.T) {
	t.Parallel()

	marker := filepath.Join(t.TempDir(), "fixed")
	body := "command:\n" +
		"  - name: queue\n" +
		"    run: [\"false\"]\n" +
		"    timeout: 10s\n" +
		"    fix:\n" +
		"      after: 1\n" +
		"      run: [\"touch\", \"" + marker + "\"]\n"
	path, _ := writeHealthConfig(t, body)
	cio, stdout, stderr := testIO(nil, time.Unix(0, 0))

	if code := runCheckOnce(t.Context(), &config{HealthConfig: path}, cio); code != exitOK {
		t.Fatalf("code = %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if _, err := os.Stat(marker); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the fix command ran in check --once: %v", err)
	}
	out := stdout.String()
	for _, want := range []string{
		"BAD command/queue: command queue: probe exited with status 1\n",
		"WOULD FIX command/queue/fix: command queue: fix would run touch\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout lacks %q:\n%s", want, out)
		}
	}
}

func TestDaemonCommandFixRuns(t *testing.T) {
	t.Parallel()

	marker := filepath.Join(t.TempDir(), "fixed")
	cmd := failingCommand(hcfg.Fix{Run: []string{"touch", marker}}, hcfg.ReportJournal)
	cmd.Run = []string{"false"}
	hs := &healthSetup{Config: &hcfg.Config{Command: []hcfg.Command{cmd}}}
	set := commandChecks(hs, lazyOver(&fakeDocker{}, nil), zap.NewNop())

	set.Checks[0].Run(t.Context())
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the daemon's fix did not run: %v", err)
	}
	if fixes := set.Fixes[0].TakeFixes(); len(fixes) != 1 || fixes[0].Send {
		t.Fatalf("fixes = %+v", fixes)
	}
}

func TestMergeCheckSetsKeepsFixes(t *testing.T) {
	t.Parallel()

	a, b := &fakeFixSource{}, &fakeFixSource{}
	set := mergeCheckSets([]checkSet{{Fixes: []runner.FixSource{a}}, {Fixes: []runner.FixSource{b}}})
	if len(set.Fixes) != 2 {
		t.Fatalf("fixes = %v", set.Fixes)
	}
}

type fakeFixSource struct{ fixes []runner.Fix }

func (f *fakeFixSource) TakeFixes() []runner.Fix {
	out := f.fixes
	f.fixes = nil

	return out
}
