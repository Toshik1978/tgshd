package command

import (
	"context"
	"errors"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/Toshik1978/tgshd/pkg/health"
	"github.com/Toshik1978/tgshd/pkg/health/config"
)

type call struct {
	argv    []string
	hasDead bool
}

type fakeRunner struct {
	probe func(ctx context.Context) (int, error)
	fix   func() (int, error)
	mu    sync.Mutex
	calls []call
}

func (f *fakeRunner) Run(ctx context.Context, argv []string) (int, error) {
	_, dl := ctx.Deadline()
	f.mu.Lock()
	f.calls = append(f.calls, call{argv: argv, hasDead: dl})
	f.mu.Unlock()
	if argv[0] == "/fix" {
		if f.fix != nil {
			return f.fix()
		}
		return 0, nil
	}

	return f.probe(ctx)
}

type fakeDocker struct {
	restarts  []string
	restartEr error
	started   time.Time
	startedEr error
}

func (d *fakeDocker) Restart(_ context.Context, name string) error {
	d.restarts = append(d.restarts, name)
	return d.restartEr
}

func (d *fakeDocker) StartedAt(context.Context, string) (time.Time, error) {
	return d.started, d.startedEr
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func exit(code int) func(context.Context) (int, error) {
	return func(context.Context) (int, error) { return code, nil }
}

func base() config.Command {
	return config.Command{
		Name:    "port-guard",
		Run:     []string{"/probe"},
		Timeout: config.Duration(time.Minute),
		Report:  config.ReportAlert,
	}
}

func fixCfg(f config.Fix) config.Command {
	c := base()
	c.Fix = &f
	return c
}

func epochTime() time.Time { return time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC) }

func newCheck(cfg config.Command, r Runner, d Docker, c *clock) *Check {
	return New(cfg, r, d, c.now, nil)
}

func TestVerdicts(t *testing.T) {
	tests := []struct {
		name    string
		probe   func(context.Context) (int, error)
		want    health.Verdict
		wantErr bool
	}{
		{"exit 0", exit(0), health.Good, false},
		{"exit 1", exit(1), health.Bad, false},
		{"timeout", func(context.Context) (int, error) { return -1, context.DeadlineExceeded }, health.Bad, false},
		{"not found", func(context.Context) (int, error) { return -1, exec.ErrNotFound }, health.Unknown, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ck := newCheck(base(), &fakeRunner{probe: tt.probe}, nil, &clock{epochTime()})
			res := ck.Run(context.Background())
			if (res.Err != nil) != tt.wantErr {
				t.Fatalf("err = %v", res.Err)
			}
			if tt.wantErr {
				if len(res.Readings) != 0 {
					t.Fatalf("readings on error: %v", res.Readings)
				}
				return
			}
			if len(res.Readings) != 1 {
				t.Fatalf("readings = %v", res.Readings)
			}
			r := res.Readings[0]
			if r.Key != "command/port-guard" || r.Verdict != tt.want || r.Kind != health.State {
				t.Fatalf("reading = %+v", r)
			}
			if tt.want == health.Bad && r.Text == "" {
				t.Fatal("empty text")
			}
		})
	}
}

func TestProbeGetsDeadline(t *testing.T) {
	r := &fakeRunner{probe: exit(0)}
	newCheck(base(), r, nil, &clock{epochTime()}).Run(context.Background())
	if len(r.calls) != 1 || !r.calls[0].hasDead {
		t.Fatalf("calls = %+v", r.calls)
	}
}

func TestNilDockerIsRuntimeError(t *testing.T) {
	ck := New(
		fixCfg(config.Fix{After: 1, RestartContainer: "w", MaxPerHour: 3}),
		&fakeRunner{probe: exit(1)},
		nil,
		(&clock{epochTime()}).now,
		nil,
	)
	res := ck.Run(context.Background())
	if res.Err == nil || len(res.Readings) != 1 || res.Readings[0].Verdict != health.Bad {
		t.Fatalf("res = %+v", res)
	}
	if len(ck.TakeFixes()) != 0 {
		t.Fatal("fix recorded")
	}
}

func TestParentCancelIsError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := &fakeRunner{probe: func(ctx context.Context) (int, error) { return -1, ctx.Err() }}
	res := newCheck(base(), r, nil, &clock{epochTime()}).Run(ctx)
	if res.Err == nil {
		t.Fatal("want error")
	}
}

func TestNoFixWithoutConfig(t *testing.T) {
	r := &fakeRunner{probe: exit(1)}
	ck := newCheck(base(), r, nil, &clock{epochTime()})
	for range 5 {
		ck.Run(context.Background())
	}
	if len(r.calls) != 5 || len(ck.TakeFixes()) != 0 {
		t.Fatalf("calls = %d", len(r.calls))
	}
}

func TestRestartContainerAfterStrikes(t *testing.T) {
	d := &fakeDocker{started: epochTime().Add(-time.Hour)}
	r := &fakeRunner{probe: exit(1)}
	fix := config.Fix{
		After:            2,
		RestartContainer: "worker",
		MaxPerHour:       3,
		GraceAfterStart:  config.Duration(2 * time.Minute),
	}
	ck := newCheck(fixCfg(fix), r, d, &clock{epochTime()})

	ck.Run(context.Background())
	if len(d.restarts) != 0 {
		t.Fatal("restarted after one strike")
	}
	res := ck.Run(context.Background())
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if len(d.restarts) != 1 || d.restarts[0] != "worker" {
		t.Fatalf("restarts = %v", d.restarts)
	}
	fx := ck.TakeFixes()
	if len(fx) != 1 || fx[0].Key != "command/port-guard/fix" || !fx[0].Send || fx[0].Text == "" ||
		!fx[0].At.Equal(epochTime()) {
		t.Fatalf("fixes = %+v", fx)
	}
	if len(ck.TakeFixes()) != 0 {
		t.Fatal("not drained")
	}
	// strikes reset: the next single failure does not fix
	ck.Run(context.Background())
	if len(d.restarts) != 1 {
		t.Fatal("second fix too early")
	}
}

func TestJournalKeepsFixOutOfTelegram(t *testing.T) {
	cfg := fixCfg(config.Fix{After: 1, RestartContainer: "worker", MaxPerHour: 3})
	cfg.Report = config.ReportJournal
	ck := newCheck(
		cfg,
		&fakeRunner{probe: exit(1)},
		&fakeDocker{started: epochTime().Add(-time.Hour)},
		&clock{epochTime()},
	)
	ck.Run(context.Background())
	fx := ck.TakeFixes()
	if len(fx) != 1 || fx[0].Send {
		t.Fatalf("fixes = %+v", fx)
	}
}

func TestRateLimitSlidingHour(t *testing.T) {
	d := &fakeDocker{started: epochTime().Add(-24 * time.Hour)}
	clk := &clock{epochTime()}
	ck := newCheck(
		fixCfg(config.Fix{After: 1, RestartContainer: "w", MaxPerHour: 3}),
		&fakeRunner{probe: exit(1)},
		d,
		clk,
	)
	for i := range 3 {
		clk.t = epochTime().Add(time.Duration(i) * 10 * time.Minute)
		ck.Run(context.Background())
	}
	clk.t = epochTime().Add(30 * time.Minute)
	ck.Run(context.Background())
	if len(d.restarts) != 3 {
		t.Fatalf("restarts = %d, want 3", len(d.restarts))
	}
	// the first fix (t=0) leaves the window after 60m
	clk.t = epochTime().Add(61 * time.Minute)
	ck.Run(context.Background())
	if len(d.restarts) != 4 {
		t.Fatalf("restarts = %d, want 4", len(d.restarts))
	}
}

func TestContainerGrace(t *testing.T) {
	d := &fakeDocker{started: epochTime().Add(-time.Minute)}
	fix := config.Fix{After: 1, RestartContainer: "w", MaxPerHour: 3, GraceAfterStart: config.Duration(2 * time.Minute)}
	ck := newCheck(fixCfg(fix), &fakeRunner{probe: exit(1)}, d, &clock{epochTime()})
	ck.Run(context.Background())
	if len(d.restarts) != 0 || len(ck.TakeFixes()) != 0 {
		t.Fatal("fixed within grace")
	}
}

func TestStartedAtErrorIsPartialFailure(t *testing.T) {
	d := &fakeDocker{startedEr: errors.New("no socket")}
	ck := newCheck(
		fixCfg(config.Fix{After: 1, RestartContainer: "w", MaxPerHour: 3}),
		&fakeRunner{probe: exit(1)},
		d,
		&clock{epochTime()},
	)
	res := ck.Run(context.Background())
	if res.Err == nil || len(res.Readings) != 1 || res.Readings[0].Verdict != health.Bad {
		t.Fatalf("res = %+v", res)
	}
	if len(d.restarts) != 0 {
		t.Fatal("restarted blind")
	}
}

func TestRestartFailureRecorded(t *testing.T) {
	d := &fakeDocker{started: epochTime().Add(-time.Hour), restartEr: errors.New("boom")}
	ck := newCheck(
		fixCfg(config.Fix{After: 1, RestartContainer: "w", MaxPerHour: 3}),
		&fakeRunner{probe: exit(1)},
		d,
		&clock{epochTime()},
	)
	res := ck.Run(context.Background())
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	fx := ck.TakeFixes()
	if len(fx) != 1 || fx[0].Text == "" {
		t.Fatalf("fixes = %+v", fx)
	}
}

func TestRunFixAndGraceSinceLastFix(t *testing.T) {
	r := &fakeRunner{probe: exit(1)}
	clk := &clock{epochTime()}
	fix := config.Fix{After: 1, Run: []string{"/fix"}, MaxPerHour: 5, GraceAfterStart: config.Duration(2 * time.Minute)}
	ck := newCheck(fixCfg(fix), r, nil, clk)
	count := func() int {
		n := 0
		for _, c := range r.calls {
			if c.argv[0] == "/fix" {
				n++
			}
		}

		return n
	}
	ck.Run(context.Background())
	if count() != 1 {
		t.Fatalf("fix runs = %d", count())
	}
	clk.t = epochTime().Add(time.Minute)
	ck.Run(context.Background())
	if count() != 1 {
		t.Fatal("fix within grace of the last fix")
	}
	clk.t = epochTime().Add(3 * time.Minute)
	ck.Run(context.Background())
	if count() != 2 {
		t.Fatalf("fix runs = %d", count())
	}
}

func TestRunFixNonZeroRecorded(t *testing.T) {
	r := &fakeRunner{probe: exit(1), fix: func() (int, error) { return 3, nil }}
	ck := newCheck(fixCfg(config.Fix{After: 1, Run: []string{"/fix"}, MaxPerHour: 5}), r, nil, &clock{epochTime()})
	ck.Run(context.Background())
	if fx := ck.TakeFixes(); len(fx) != 1 || fx[0].Text == "" {
		t.Fatalf("fixes = %+v", fx)
	}
}

func TestGoodResetsStrikes(t *testing.T) {
	code := 1
	r := &fakeRunner{probe: func(context.Context) (int, error) { return code, nil }}
	d := &fakeDocker{started: epochTime().Add(-time.Hour)}
	ck := newCheck(fixCfg(config.Fix{After: 2, RestartContainer: "w", MaxPerHour: 3}), r, d, &clock{epochTime()})
	ck.Run(context.Background())
	code = 0
	ck.Run(context.Background())
	code = 1
	ck.Run(context.Background())
	if len(d.restarts) != 0 {
		t.Fatal("strikes not reset")
	}
}

func TestExportRestore(t *testing.T) {
	d := &fakeDocker{started: epochTime().Add(-24 * time.Hour)}
	cfg := fixCfg(config.Fix{After: 2, RestartContainer: "w", MaxPerHour: 1})
	clk := &clock{epochTime()}
	a := newCheck(cfg, &fakeRunner{probe: exit(1)}, d, clk)
	a.Run(context.Background())
	data, err := a.Export()
	if err != nil {
		t.Fatal(err)
	}
	b := newCheck(cfg, &fakeRunner{probe: exit(1)}, d, clk)
	if err := b.Restore(data); err != nil {
		t.Fatal(err)
	}
	b.Run(context.Background())
	if len(d.restarts) != 1 {
		t.Fatalf("restarts = %d", len(d.restarts))
	}
	// the fix history is restored: the 1-per-hour limit holds
	data, _ = b.Export()
	c := newCheck(cfg, &fakeRunner{probe: exit(1)}, d, clk)
	if err := c.Restore(data); err != nil {
		t.Fatal(err)
	}
	c.Run(context.Background())
	c.Run(context.Background())
	if len(d.restarts) != 1 {
		t.Fatalf("restarts = %d, want 1", len(d.restarts))
	}
	if err := c.Restore([]byte("{bad")); err == nil {
		t.Fatal("want error")
	}
	if err := c.Restore(nil); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentRunsFixOnce(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 8)
	d := &blockingDocker{
		started: epochTime().Add(-time.Hour),
		release: release,
		entered: entered,
	}
	cfg := fixCfg(config.Fix{After: 1, RestartContainer: "w", MaxPerHour: 5})
	ck := newCheck(cfg, &fakeRunner{probe: exit(1)}, d, &clock{epochTime()})

	var wg sync.WaitGroup

	for range 4 {
		wg.Go(func() {
			ck.Run(context.Background())
		})
	}

	<-entered
	// let the other runs reach the in-flight guard, then finish the restart
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()

	if n := d.count(); n != 1 {
		t.Fatalf("restarts = %d, want 1", n)
	}

	if fx := ck.TakeFixes(); len(fx) != 1 {
		t.Fatalf("fixes = %d, want 1", len(fx))
	}
}

type blockingDocker struct {
	fakeDocker

	mu      sync.Mutex
	n       int
	release chan struct{}
	entered chan struct{}
}

func (b *blockingDocker) Restart(context.Context, string) error {
	b.mu.Lock()
	b.n++
	b.mu.Unlock()

	b.entered <- struct{}{}
	<-b.release

	return nil
}

func (b *blockingDocker) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.n
}

func TestDryRunReportsTheFixWithoutTakingIt(t *testing.T) {
	tests := []struct {
		name string
		fix  config.Fix
		want string
	}{
		{
			"restart",
			config.Fix{After: 1, MaxPerHour: 3, RestartContainer: "web"},
			"command port-guard: fix would restart container web",
		},
		{
			"run",
			config.Fix{After: 1, MaxPerHour: 3, Run: []string{"/fix", "--token=hidden"}},
			"command port-guard: fix would run /fix",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &fakeRunner{probe: exit(1)}
			d := &fakeDocker{}
			c := &clock{t: epochTime()}
			ck := New(fixCfg(tt.fix), r, d, c.now, nil, WithDryRun())

			res := ck.Run(context.Background())
			if res.Err != nil || len(res.Readings) != 1 || res.Readings[0].Verdict != health.Bad {
				t.Fatalf("result = %+v", res)
			}
			if len(r.calls) != 1 || r.calls[0].argv[0] != "/probe" || len(d.restarts) != 0 {
				t.Fatalf("calls = %+v, restarts = %v: a dry run only probes", r.calls, d.restarts)
			}
			fixes := ck.TakeFixes()
			if len(fixes) != 1 || fixes[0].Text != tt.want {
				t.Fatalf("fixes = %+v, want %q", fixes, tt.want)
			}
		})
	}
}
