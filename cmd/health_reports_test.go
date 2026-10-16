package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/fx"
	"go.uber.org/fx/fxtest"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/Toshik1978/tgshd/pkg/command"
	"github.com/Toshik1978/tgshd/pkg/health"
	"github.com/Toshik1978/tgshd/pkg/health/check/disks"
	hcfg "github.com/Toshik1978/tgshd/pkg/health/config"
	"github.com/Toshik1978/tgshd/pkg/health/notify"
	"github.com/Toshik1978/tgshd/pkg/health/runner"
	"github.com/Toshik1978/tgshd/pkg/health/store"
	"github.com/Toshik1978/tgshd/pkg/telegram"
)

const (
	testSerial1 = "TEST0000000001"
	testSerial2 = "TEST0000000002"
	testSerial3 = "TEST0000000003"
)

// fakeReportStore serves canned history and records retention runs.
type fakeReportStore struct {
	mu         sync.Mutex
	alerts     []store.Alert
	metrics    []store.MetricStats
	metricsErr error
	prefixes   []string
	retention  store.RetentionResult
	retainErr  error
	retainedAt []time.Time
}

func (f *fakeReportStore) Alerts(context.Context, string, time.Time, int) ([]store.Alert, error) {
	return f.alerts, nil
}

func (f *fakeReportStore) Metrics(_ context.Context, prefix string, _ time.Time) ([]store.MetricStats, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.prefixes = append(f.prefixes, prefix)
	if f.metricsErr != nil {
		return nil, f.metricsErr
	}
	var out []store.MetricStats
	for _, m := range f.metrics {
		if strings.HasPrefix(m.Metric, prefix) {
			out = append(out, m)
		}
	}

	return out, nil
}

func (f *fakeReportStore) Retention(_ context.Context, now time.Time) (store.RetentionResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.retainedAt = append(f.retainedAt, now)

	return f.retention, f.retainErr
}

// bookOf is a drive book over fixed baselines.
type bookOf []store.Baseline

func (b bookOf) Baselines() []store.Baseline { return slices.Clone(b) }
func (bookOf) Retire(string) bool            { return false }

// fakeRunner serves a fixed snapshot, records pushed events and expires a
// fixed set of queued messages.
type fakeRunner struct {
	mu       sync.Mutex
	open     []runner.OpenCondition
	pushed   [][3]string // check, text, record
	expired  []notify.QueuedMessage
	expireAt []time.Time
}

func (f *fakeRunner) Snapshot() runner.Snapshot {
	return runner.Snapshot{Open: slices.Clone(f.open)}
}

func (f *fakeRunner) Push(_ context.Context, check, text, record string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.pushed = append(f.pushed, [3]string{check, text, record})
}

func (f *fakeRunner) ExpireQueue(now time.Time, maxAge time.Duration) []notify.QueuedMessage {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.expireAt = append(f.expireAt, now.Add(-maxAge))

	return f.expired
}

func testBook() bookOf {
	return bookOf{
		{Serial: testSerial2, Model: "Example SATA SSD", Bus: "SATA"},
		{Serial: testSerial3, Model: "Example HDD", Bus: "SATA", Retired: true},
		{Serial: testSerial1, Model: "Example NVMe SSD", Bus: "NVMe"},
	}
}

func TestDriveList(t *testing.T) {
	t.Parallel()

	open := []runner.OpenCondition{
		{Key: "disks/" + testSerial2 + "/health", Text: "SMART overall health FAILED"},
		{Key: "disks/" + testSerial1 + "/wear", Text: "worn"},
		{Key: "disks/" + testSerial1 + "/missing", Text: "gone"},
	}
	stats := []store.MetricStats{
		{Metric: "drive." + testSerial1 + ".spare", Current: 98, HasCurrent: true},
		{Metric: "drive." + testSerial1 + ".selftest_age_h", Current: 30, HasCurrent: true},
		// Only rolled-up rows: no current value, so unknown.
		{Metric: "drive." + testSerial2 + ".selftest_age_h", Min: 5, Max: 9},
		{Metric: "drive." + testSerial3 + ".spare", Current: 50, HasCurrent: true},
	}
	got := driveList(testBook().Baselines(), open, stats)
	want := []command.Drive{
		{Bus: "NVMe", Model: "Example NVMe SSD", Serial: testSerial1, HealthOK: true, Missing: true},
		{Bus: "SATA", Model: "Example SATA SSD", Serial: testSerial2, HealthOK: false},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for i := range want {
		g := got[i]
		if g.Bus != want[i].Bus || g.Model != want[i].Model || g.Serial != want[i].Serial ||
			g.HealthOK != want[i].HealthOK || g.Missing != want[i].Missing {
			t.Errorf("drive %d = %+v, want %+v", i, g, want[i])
		}
	}
	if got[0].SparePct == nil || *got[0].SparePct != 98 ||
		got[0].SelfTestAge == nil || *got[0].SelfTestAge != 30*time.Hour {
		t.Errorf("drive 1 spare %v, self-test age %v; want 98 and 30h", got[0].SparePct, got[0].SelfTestAge)
	}
	if got[1].SparePct != nil || got[1].SelfTestAge != nil {
		t.Errorf("drive 2 spare %v, self-test age %v; want unknown", got[1].SparePct, got[1].SelfTestAge)
	}
	if got := driveList(nil, open, stats); len(got) != 0 {
		t.Errorf("no baselines: %+v", got)
	}
}

// statusesOf serves fixed drive statuses.
type statusesOf []disks.DriveStatus

func (s statusesOf) Status() []disks.DriveStatus { return slices.Clone(s) }

func TestHealthReportsDrivesFromStatus(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC)
	asleep := now.Add(-2 * time.Hour)
	statuses := statusesOf{
		{
			Serial: testSerial1, Model: "Example NVMe SSD", Bus: disks.BusNVMe, Transport: "nvme", Node: "nvme0n1",
			Health: "ok", LastReadAt: now.Add(-3 * time.Hour), AsleepSince: asleep,
			Temperature: new(38), Wear: new(4), Spare: new(99), SpareThreshold: new(10),
			WearLimit: 80, SpareFloor: 20, SelfTestPassAge: 30 * time.Hour, SelfTestReadable: true,
			SelfTestFormat: disks.SelfTestFormatOK,
			SelfTestNewest: &disks.SelfTestNewest{Result: "passed"}, SelfTestNewestAge: new(26 * time.Hour),
			SelfTestPassObserved: true,
		},
		{
			Serial: testSerial2, Model: "Example SATA SSD", Bus: disks.BusATA, Transport: "sata", Node: "sda",
			Health: "failing", LastReadAt: now, UnreadableSince: asleep,
		},
	}
	h := &healthReports{statuses: statuses, logger: zap.NewNop(), now: func() time.Time { return now }}
	h.bind(&fakeRunner{open: []runner.OpenCondition{{Key: "disks/" + testSerial2 + "/missing"}}})

	got := h.Drives()
	if len(got) != 2 {
		t.Fatalf("got %+v, want two drives", got)
	}
	d := got[0]
	if d.Node != "nvme0n1" || d.Bus != "NVMe" || d.Transport != "nvme" || d.Health != "ok" || d.Missing ||
		d.Wear == nil || *d.Wear != 4 || d.Spare == nil || *d.Spare != 99 || *d.SpareThreshold != 10 ||
		d.SpareFloor != 20 || d.Temp == nil || *d.Temp != 38 {
		t.Errorf("drive 1 = %+v", d)
	}
	st := d.SelfTest
	if st.Format != "ok" || st.Result != "passed" || st.Age != 30*time.Hour || st.Unobserved ||
		st.NewestAge == nil || *st.NewestAge != 26*time.Hour {
		t.Errorf("self-test = %+v, want passed, 30h, newest 26h, observed", st)
	}
	if !got[1].SelfTest.Unobserved {
		t.Errorf("drive 2 pass was never observed: %+v", got[1].SelfTest)
	}
	if d.LastReadAge == nil || *d.LastReadAge != 3*time.Hour || !d.AsleepSince.Equal(asleep) {
		t.Errorf("last read %v, asleep %v", d.LastReadAge, d.AsleepSince)
	}
	d = got[1]
	if !d.Missing || d.Health != "failing" || d.Wear != nil || d.SelfTest.Format != "" ||
		d.LastReadAge == nil || *d.LastReadAge != 0 || !d.UnreadableSince.Equal(asleep) {
		t.Errorf("drive 2 = %+v", d)
	}
	if got := (&healthReports{}).Drives(); got != nil {
		t.Fatalf("no disks check: %+v", got)
	}
}

func TestHealthReportsDailyRunsRetention(t *testing.T) {
	t.Parallel()

	core, logs := observer.New(zapcore.WarnLevel)
	created := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	st := &fakeReportStore{retention: store.RetentionResult{QueueDropped: []notify.QueuedMessage{
		{ID: 7, CreatedAt: created, Channel: notify.ChannelTelegram, Text: "box: PROBLEM: token secret", Attempts: 3},
	}}}
	loc := time.FixedZone("host", 3600)
	h := &healthReports{store: st, loc: loc, logger: zap.New(core)}
	now := time.Date(2026, 10, 16, 3, 0, 0, 0, time.UTC)

	var opts runner.Options
	h.hook(&opts)
	if opts.Daily == nil || opts.Weekly == nil {
		t.Fatalf("hooks not set: daily %v, weekly %v", opts.Daily != nil, opts.Weekly != nil)
	}
	// Without a bound runner only the store's retention runs.
	if err := opts.Daily(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	if len(st.retainedAt) != 1 || !st.retainedAt[0].Equal(now) || st.retainedAt[0].Location() != loc {
		t.Fatalf("retention ran at %v, want %v in the host zone", st.retainedAt, now)
	}
	entries := logs.All()
	if len(entries) != 1 || entries[0].Level != zapcore.WarnLevel {
		t.Fatalf("logs = %+v", entries)
	}
	fields := entries[0].ContextMap()
	if fields["id"] != int64(7) || fields["channel"] != notify.ChannelTelegram || fields["attempts"] != int64(3) {
		t.Errorf("fields = %v", fields)
	}
	for k, v := range fields {
		if s, ok := v.(string); ok && strings.Contains(s, "secret") {
			t.Errorf("field %s leaks the message text", k)
		}
	}

	st.retainErr = errors.New("disk full")
	if err := opts.Daily(t.Context(), now); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("err = %v, want the retention failure", err)
	}

	// A store not open yet (the runner's Load reports that) or closed is
	// skipped quietly.
	for _, notOpen := range []error{store.ErrNotOpen, store.ErrClosed} {
		st.retainErr = fmt.Errorf("wrapped: %w", notOpen)
		if err := opts.Daily(t.Context(), now); err != nil {
			t.Fatalf("err = %v, want the unopened store skipped quietly", err)
		}
	}
}

func TestHealthReportsDailyExpiresTheQueueAndLogsEachDropOnce(t *testing.T) {
	t.Parallel()

	core, logs := observer.New(zapcore.WarnLevel)
	created := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	held := notify.QueuedMessage{ID: 7, CreatedAt: created, Channel: notify.ChannelTelegram, Text: "a"}
	storedOnly := notify.QueuedMessage{ID: 8, CreatedAt: created, Channel: notify.ChannelSMS, Text: "b"}
	st := &fakeReportStore{retention: store.RetentionResult{QueueDropped: []notify.QueuedMessage{held, storedOnly}}}
	r := &fakeRunner{expired: []notify.QueuedMessage{held}}
	h := &healthReports{store: st, loc: time.UTC, logger: zap.New(core)}
	h.bind(r)
	now := time.Date(2026, 10, 16, 3, 0, 0, 0, time.UTC)

	if err := h.daily(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	if len(r.expireAt) != 1 || !r.expireAt[0].Equal(now.Add(-7*24*time.Hour)) {
		t.Fatalf("expired before %v, want 7 days before now", r.expireAt)
	}
	// The runner logs what it expired (through its drop log); the reports log
	// only the stored message it did not hold.
	entries := logs.All()
	if len(entries) != 1 || entries[0].ContextMap()["id"] != int64(8) {
		t.Fatalf("logs = %+v, want only message 8", entries)
	}
}

func TestHealthReportsWithoutStoreHasNoDailyJob(t *testing.T) {
	t.Parallel()

	var opts runner.Options
	(&healthReports{}).hook(&opts)
	if opts.Daily != nil || opts.Weekly == nil {
		t.Fatalf("daily %v, weekly %v; want only the weekly summary", opts.Daily != nil, opts.Weekly != nil)
	}
}

func weeklyReports(st *fakeReportStore, r *fakeRunner) *healthReports {
	h := &healthReports{
		book: testBook(), checks: []string{"containers", "disks", "http/web-app"},
		host: "box", logger: zap.NewNop(), now: time.Now,
		statuses: statusesOf{
			{
				Serial: testSerial1,
				Model:  "Example NVMe SSD",
				Bus:    disks.BusNVMe,
				Node:   "nvme0n1",
				Health: "ok",
				Wear:   new(4),
			},
			{Serial: testSerial2, Model: "Example SATA SSD", Bus: disks.BusATA, Node: "sda", Health: "ok"},
		},
	}
	if st != nil {
		h.store = st
	}
	r.open = []runner.OpenCondition{{Key: "http/web-app", Text: "HTTP 500 <x>"}}
	h.bind(r)

	return h
}

func TestHealthReportsWeeklyPushesTheSummary(t *testing.T) {
	t.Parallel()

	st := &fakeReportStore{metrics: []store.MetricStats{
		{Metric: "drive." + testSerial1 + ".wear", Current: 4, HasCurrent: true},
		{Metric: "drive." + testSerial1 + ".written_bytes", Current: 3e9, Min: 1e9, HasCurrent: true},
		{Metric: "temp.cpu", Current: 40, HasCurrent: true},
	}}
	r := &fakeRunner{}
	if err := weeklyReports(st, r).weekly(t.Context(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(r.pushed) != 1 || r.pushed[0][0] != "summary" || r.pushed[0][2] != "Weekly summary sent" {
		t.Fatalf("pushed = %q, want the summary with a one-line record", r.pushed)
	}
	got := r.pushed[0][1]
	for _, want := range []string{
		"Weekly summary\nDrives:\n",
		"NVMe Example NVMe SSD …0001: health OK, wear 4%, written 7d 2.0 GB\n",
		"SATA Example SATA SSD …0002: health OK, wear n/a, written 7d n/a\n",
		"Services:\ncontainers: OK (1 check)\nhttp: 1 open (0 of 1 checks OK)\n",
		"Open problems:\nhttp/web-app: HTTP 500 <x>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary lacks %q:\n%s", want, got)
		}
	}
	if strings.HasPrefix(got, "box") || strings.Contains(got, testSerial3[len(testSerial3)-4:]) ||
		strings.Contains(got, testSerial1) {
		t.Errorf("host prefix (the notifier adds it), retired drive or full serial shown:\n%s", got)
	}
	if !slices.Equal(st.prefixes, []string{"drive."}) {
		t.Errorf("metrics read = %v", st.prefixes)
	}
}

func TestHealthReportsWeeklyWithoutHistory(t *testing.T) {
	t.Parallel()

	for _, st := range []*fakeReportStore{nil, {metricsErr: errors.New("locked")}} {
		r := &fakeRunner{}
		if err := weeklyReports(st, r).weekly(t.Context(), time.Now()); err != nil {
			t.Fatal(err)
		}
		if len(r.pushed) != 1 || !strings.Contains(r.pushed[0][1], "\nDrives: unavailable\nServices:\n") {
			t.Fatalf("pushed = %q, want the drives section unavailable and the rest sent", r.pushed)
		}
	}
}

func TestHealthReportsWeeklyBeforeBind(t *testing.T) {
	t.Parallel()

	if err := (&healthReports{}).weekly(t.Context(), time.Now()); !errors.Is(err, errNotBound) {
		t.Fatalf("err = %v, want errNotBound", err)
	}
}

// handlerIndex returns the index of the /name handler in hs.
func handlerIndex(t *testing.T, hs []telegram.Handler, name string) int {
	t.Helper()

	i := slices.IndexFunc(hs, func(h telegram.Handler) bool { return h.Name() == name })
	if i < 0 {
		t.Fatalf("no /%s handler", name)
	}

	return i
}

func TestHealthReportsHandlers(t *testing.T) {
	t.Parallel()

	st := &fakeReportStore{
		alerts: []store.Alert{{At: time.Now(), Kind: store.KindProblem, Key: "http/web-app", Text: "HTTP 500"}},
		metrics: []store.MetricStats{
			{Metric: "temp.cpu", Current: 41, HasCurrent: true},
			{Metric: "drive." + testSerial1 + ".wear", Current: 4, HasCurrent: true},
		},
	}
	pub := &fakePublisher{}
	h := weeklyReports(st, &fakeRunner{})
	hs := h.handlers(zap.NewNop(), pub)
	for _, c := range []struct{ name, want string }{
		{"history", "PROBLEM http/web-app: HTTP 500"},
		{"temp", "cpu: 41°C"},
		{"disks", "NVMe Example NVMe SSD …0001: health OK, wear 4%"},
	} {
		if err := hs[handlerIndex(t, hs, c.name)].Handle(t.Context(), 42, "/"+c.name); err != nil {
			t.Fatal(err)
		}
		msgs := pub.messages()
		if got := msgs[len(msgs)-1]; !strings.Contains(got, c.want) {
			t.Errorf("/%s = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestHealthReportsHandlersWithoutStore(t *testing.T) {
	t.Parallel()

	pub := &fakePublisher{}
	hs := weeklyReports(nil, &fakeRunner{}).handlers(zap.NewNop(), pub)
	names := make([]string, 0, len(hs))
	for _, h := range hs {
		names = append(names, h.Name())
		if !h.Enabled() {
			t.Errorf("/%s disabled", h.Name())
		}
		if err := h.Handle(t.Context(), 42, "/"+h.Name()); err != nil {
			t.Fatal(err)
		}
	}
	if !slices.Equal(names, []string{"history", "temp", "disks"}) {
		t.Fatalf("handlers = %v", names)
	}
	for _, got := range pub.messages() {
		if got != noHistoryReply {
			t.Errorf("reply = %q, want %q", got, noHistoryReply)
		}
	}

	failing := weeklyReports(nil, &fakeRunner{}).handlers(zap.NewNop(), &fakePublisher{err: errors.New("down")})
	if err := failing[0].Handle(t.Context(), 42, "/history"); err == nil {
		t.Fatal("a failed reply must be returned")
	}
}

// TestNewHealthReportsTakesTheDisksCheckStatus: the real disks check is the
// reports' status source.
func TestNewHealthReportsTakesTheDisksCheckStatus(t *testing.T) {
	t.Parallel()

	set := buildDisksCheck(&hcfg.Disks{}, nil, nil, time.Now, zap.NewNop(), false)
	h := newHealthReports(&healthSetup{Host: "box"}, set, zap.NewNop())
	if h.statuses == nil {
		t.Fatal("statuses = nil, want the disks check")
	}
}

func TestNewHealthReportsFromSetup(t *testing.T) {
	t.Parallel()

	set := checkSet{
		Checks: []health.Check{{Name: "containers"}, {Name: "http/web-app"}},
		Drives: testBook(),
	}
	h := newHealthReports(&healthSetup{Host: "box"}, set, zap.NewNop())
	if h.store != nil || h.host != "box" || h.loc == nil || h.now == nil || h.book == nil ||
		!slices.Equal(h.checks, []string{"containers", "http/web-app"}) {
		t.Fatalf("reports = %+v", h)
	}
}

// reportHandlers collects the command_handler group of a health graph.
type reportHandlers struct {
	fx.In

	Handlers []telegram.Handler `group:"command_handler"`
}

func TestHealthGraphRegistersReportCommands(t *testing.T) {
	t.Parallel()

	path, _ := writeHealthConfig(t, "summary:\n  weekday: monday\n  time: \"09:00\"\n")
	var got reportHandlers
	app := fxtest.New(t,
		fx.NopLogger,
		fx.Supply(&config{HealthConfig: path, ChatID: 9}),
		fx.Provide(
			zap.NewNop,
			func() command.Publisher { return &fakePublisher{} },
		),
		healthProviders(),
		fx.Populate(&got),
	)
	app.RequireStart().RequireStop()

	names := make([]string, 0, len(got.Handlers))
	for _, h := range got.Handlers {
		names = append(names, h.Name())
	}
	for _, want := range []string{"history", "temp", "disks"} {
		if !slices.Contains(names, want) {
			t.Errorf("handlers = %v, want /%s", names, want)
		}
	}
}
