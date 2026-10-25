package main

import (
	"context"
	"errors"
	"fmt"
	"reflect"
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
	"github.com/Toshik1978/tgshd/pkg/health/check/memory"
	"github.com/Toshik1978/tgshd/pkg/health/check/modemprobe"
	"github.com/Toshik1978/tgshd/pkg/health/check/temps"
	hcfg "github.com/Toshik1978/tgshd/pkg/health/config"
	"github.com/Toshik1978/tgshd/pkg/health/notify"
	"github.com/Toshik1978/tgshd/pkg/health/report"
	"github.com/Toshik1978/tgshd/pkg/health/runner"
	"github.com/Toshik1978/tgshd/pkg/health/store"
	"github.com/Toshik1978/tgshd/pkg/host/sysfs"
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

// TestHealthReportsWithoutStoreStillExpiresTheQueue: in memory-only mode the
// daily job still expires undelivered messages; there is no retention to run.
func TestHealthReportsWithoutStoreStillExpiresTheQueue(t *testing.T) {
	t.Parallel()

	var opts runner.Options
	h := &healthReports{loc: time.UTC, logger: zap.NewNop()}
	h.hook(&opts)
	if opts.Daily == nil || opts.Weekly == nil {
		t.Fatalf("daily %v, weekly %v; want both jobs", opts.Daily != nil, opts.Weekly != nil)
	}
	r := &fakeRunner{}
	h.bind(r)
	now := time.Date(2026, 10, 16, 3, 0, 0, 0, time.UTC)

	if err := opts.Daily(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	if len(r.expireAt) != 1 {
		t.Fatalf("queue expired %d times, want once", len(r.expireAt))
	}
}

// fakeTopology serves fixed mounts and block devices.
type fakeTopology struct {
	mounts   []sysfs.MountEntry
	blocks   []sysfs.BlockDevice
	mountErr error
	blockErr error
}

func (f fakeTopology) MountInfo() ([]sysfs.MountEntry, error) { return f.mounts, f.mountErr }

func (f fakeTopology) BlockTopology() ([]sysfs.BlockDevice, error) { return f.blocks, f.blockErr }

func dev(major, minor uint32) sysfs.DevNum { return sysfs.DevNum{Major: major, Minor: minor} }

// testTopology: nvme0n1p2 holds /, sda1 is under md0 which holds /data.
func testTopology() fakeTopology {
	return fakeTopology{
		mounts: []sysfs.MountEntry{
			{Dev: dev(259, 2), MountPoint: "/"},
			{Dev: dev(9, 0), MountPoint: "/data"},
			{Dev: dev(0, 25), MountPoint: "/proc"},
		},
		blocks: []sysfs.BlockDevice{
			{Name: "md0", Dev: dev(9, 0)},
			{
				Name: "nvme0n1", Dev: dev(259, 0),
				Partitions: []sysfs.Partition{{Name: "nvme0n1p2", Dev: dev(259, 2)}},
			},
			{
				Name: "sda", Dev: dev(8, 0),
				Partitions: []sysfs.Partition{{
					Name: "sda1", Dev: dev(8, 1), Holders: []sysfs.Holder{{Name: "md0", Dev: dev(9, 0)}},
				}},
			},
		},
	}
}

// fakeStamps serves fixed freshness stamps.
type fakeStamps map[string]report.FreshnessStamp

func (f fakeStamps) Stamp(name string) (stamp time.Time, present, readOK bool) {
	s := f[name]

	return s.At, s.Present, s.ReadOK
}

// fakeProbeStatus serves a fixed modem probe state.
type fakeProbeStatus modemprobe.State

func (f fakeProbeStatus) Status() modemprobe.State { return modemprobe.State(f) }

func weeklyReports(st *fakeReportStore, r *fakeRunner, now time.Time) *healthReports {
	h := &healthReports{
		checks: []string{
			"containers", "disks", "freshness/db-dump", "freshness/offsite", "http/web-app", "modem_probe",
		},
		groups: map[string]string{"freshness/db-dump": "backups", "freshness/offsite": "backups"},
		host:   "box", logger: zap.NewNop(), now: func() time.Time { return now },
		topology: testTopology(),
		stamps: fakeStamps{
			"db-dump": {At: now.Add(-30 * time.Hour), Present: true, ReadOK: true},
			"offsite": {ReadOK: true},
		},
		probe: fakeProbeStatus{LastOKAt: now.Add(-3 * 24 * time.Hour)},
		statuses: statusesOf{
			{
				Serial: testSerial1, Model: "Example NVMe SSD", Bus: disks.BusNVMe, Transport: "nvme",
				Node: "nvme0n1", Health: "ok", Temperature: new(48), Wear: new(4),
				SelfTestFormat: disks.SelfTestFormatOK, SelfTestNewest: &disks.SelfTestNewest{Result: "passed"},
				SelfTestPassAge: 2 * 24 * time.Hour, SelfTestPassObserved: true,
			},
			{
				Serial: testSerial2, Model: "Example SATA SSD", Bus: disks.BusATA, Transport: "sata", Node: "sda",
				Health: "ok", AsleepSince: time.Date(2026, 10, 13, 14, 0, 0, 0, time.UTC),
				SelfTestFormat: disks.SelfTestFormatOK, SelfTestNewest: &disks.SelfTestNewest{Result: "passed"},
				SelfTestPassAge: 30 * 24 * time.Hour, SelfTestPassObserved: true,
			},
		},
	}
	if st != nil {
		h.store = st
	}
	r.open = []runner.OpenCondition{{Key: "http/web-app", Text: "HTTP 500 <x>", Since: now.Add(-50 * time.Minute)}}
	h.bind(r)

	return h
}

// TestHealthReportsWeeklyPushesTheSummary: the summary is fed from the disks
// check's status with the mounts read at report time, the stored wear of a
// drive not read since the start, the summary groups, the freshness stamps
// and the modem probe state.
func TestHealthReportsWeeklyPushesTheSummary(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	st := &fakeReportStore{metrics: []store.MetricStats{
		{Metric: "drive." + testSerial2 + ".wear", Current: 7, HasCurrent: true},
		{Metric: "temp.cpu", Current: 40, HasCurrent: true},
	}}
	r := &fakeRunner{}
	if err := weeklyReports(st, r, now).weekly(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	if len(r.pushed) != 1 || r.pushed[0][0] != "summary" || r.pushed[0][2] != "Weekly summary sent" {
		t.Fatalf("pushed = %q, want the summary with a one-line record", r.pushed)
	}
	want := "Weekly summary\n" +
		"Drives:\n" +
		"nvme0n1 (/) 48°C, self-test passed 2 d ago, wear 4%\n" +
		"sda (/data) asleep since Tue 14:00, self-test passed 30 d ago, wear 7%\n" +
		"Services:\n" +
		"containers: ok\n" +
		"backups: db-dump 30 h ago; offsite none\n" +
		"http/web-app: HTTP 500 <x>\n" +
		"modem_probe: last round trip 3 d ago, no probe outstanding\n" +
		"Open problems:\n" +
		"http/web-app: HTTP 500 <x> (since 2026-10-18 08:10)"
	if got := r.pushed[0][1]; got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	if !slices.Equal(st.prefixes, []string{"drive."}) {
		t.Errorf("metrics read = %v", st.prefixes)
	}
}

// TestHealthReportsWeeklyWithoutHistory: the drive lines come from the disks
// check, so without a state database (or when it cannot be read) they are
// still shown, only without the stored wear.
func TestHealthReportsWeeklyWithoutHistory(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	for _, st := range []*fakeReportStore{nil, {metricsErr: errors.New("locked")}} {
		r := &fakeRunner{}
		if err := weeklyReports(st, r, now).weekly(t.Context(), now); err != nil {
			t.Fatal(err)
		}
		want := "\nsda (/data) asleep since Tue 14:00, self-test passed 30 d ago\nServices:\n"
		if len(r.pushed) != 1 || !strings.Contains(r.pushed[0][1], want) {
			t.Fatalf("pushed = %q, want %q", r.pushed, want)
		}
	}
}

// TestHealthReportsWeeklyMountsUnknown: a failed mount or topology read shows
// "?" for every drive; without the disks check the section reads "none".
func TestHealthReportsWeeklyMountsUnknown(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	for _, topo := range []fakeTopology{
		{mountErr: errors.New("no procfs")},
		{blockErr: errors.New("no sysfs")},
	} {
		r := &fakeRunner{}
		h := weeklyReports(nil, r, now)
		h.topology = topo
		if err := h.weekly(t.Context(), now); err != nil {
			t.Fatal(err)
		}
		got := r.pushed[0][1]
		if !strings.Contains(got, "\nnvme0n1 (?) 48°C") || !strings.Contains(got, "\nsda (?) asleep") {
			t.Errorf("got\n%s\nwant both drives' mounts unknown", got)
		}
	}

	r := &fakeRunner{}
	h := weeklyReports(nil, r, now)
	h.statuses = nil
	if err := h.weekly(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.pushed[0][1], "\nDrives: none\n") {
		t.Errorf("got %q, want no drives", r.pushed[0][1])
	}
}

// TestHealthReportsWeeklyMissingDriveMountsUnknown: a missing drive's node
// is stale (the kernel may have given it to another drive), so its mounts
// read "?" rather than those of whatever holds the node now.
func TestHealthReportsWeeklyMissingDriveMountsUnknown(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	r := &fakeRunner{}
	h := weeklyReports(nil, r, now)
	r.open = append(r.open, runner.OpenCondition{Key: "disks/" + testSerial2 + "/missing", Text: "gone", Since: now})
	if err := h.weekly(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	got := r.pushed[0][1]
	if !strings.Contains(got, "\nsda (?) missing, asleep since") || !strings.Contains(got, "\nnvme0n1 (/) 48°C") {
		t.Errorf("got\n%s\nwant the missing sda's mounts unknown and nvme0n1's read", got)
	}
}

// TestReportTopologyDropsZeroDevices: a device, partition or holder whose
// device number could not be read ("0:0") never matches a mount entry.
func TestReportTopologyDropsZeroDevices(t *testing.T) {
	t.Parallel()

	got := reportTopology([]sysfs.BlockDevice{
		{Name: "loop0"},
		{
			Name: "sda", Dev: dev(8, 0), Holders: []sysfs.Holder{{Name: "dm-9"}, {Name: "dm-0", Dev: dev(253, 0)}},
			Partitions: []sysfs.Partition{
				{Name: "sda1", Dev: dev(8, 1), Holders: []sysfs.Holder{{Name: "md0"}, {Name: "md1", Dev: dev(9, 1)}}},
				{Name: "sda2"},
			},
		},
	})
	want := []report.BlockDevice{{
		Name: "sda", Dev: "8:0", Holders: []report.BlockHolder{{Name: "dm-0", Dev: "253:0"}},
		Partitions: []report.BlockPartition{
			{Name: "sda1", Dev: "8:1", Holders: []report.BlockHolder{{Name: "md1", Dev: "9:1"}}},
		},
	}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("got %+v, want %+v", got, want)
	}

	entries := reportMounts([]sysfs.MountEntry{{Dev: dev(8, 1), MountPoint: "/"}, {MountPoint: "/x"}})
	if len(entries) != 1 || entries[0] != (report.MountEntry{Dev: "8:1", MountPoint: "/"}) {
		t.Errorf("mount entries = %+v, want only 8:1 on /", entries)
	}
	// A zero device in the mount table would otherwise match a zero holder.
	if m := report.DriveMounts("sda", entries, got, nil); m != "/" {
		t.Errorf("mounts = %q", m)
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
	h := weeklyReports(st, &fakeRunner{}, time.Now())
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
	hs := weeklyReports(nil, &fakeRunner{}, time.Now()).handlers(zap.NewNop(), pub)
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

	failing := weeklyReports(
		nil,
		&fakeRunner{},
		time.Now(),
	).handlers(zap.NewNop(), &fakePublisher{err: errors.New("down")})
	if err := failing[0].Handle(t.Context(), 42, "/history"); err == nil {
		t.Fatal("a failed reply must be returned")
	}
}

// TestNewHealthReportsTakesTheDisksCheckStatus: the real disks check is the
// reports' status source.
func TestNewHealthReportsTakesTheDisksCheckStatus(t *testing.T) {
	t.Parallel()

	set := buildDisksCheck(&hcfg.Disks{}, nil, nil, time.Now, zap.NewNop(), false)
	h := newHealthReports(&healthSetup{Host: "box"}, set, testTopology(), zap.NewNop())
	if h.statuses == nil || h.topology == nil {
		t.Fatalf("statuses = %v, topology = %v; want the disks check and the reader", h.statuses, h.topology)
	}
}

func TestNewHealthReportsFromSetup(t *testing.T) {
	t.Parallel()

	stamps, probe := fakeStamps{}, fakeProbeStatus{}
	set := checkSet{
		Checks: []health.Check{{Name: "containers"}, {Name: "http/web-app"}},
		Drives: testBook(),
		Stamps: stamps,
		Probe:  probe,
	}
	cfg := &hcfg.Config{Containers: &hcfg.Containers{SummaryGroup: "apps"}}
	h := newHealthReports(&healthSetup{Host: "box", Config: cfg}, set, nil, zap.NewNop())
	if h.store != nil || h.host != "box" || h.loc == nil || h.now == nil || h.statuses != nil ||
		h.topology != nil || h.stamps == nil || h.probe == nil || h.groups["containers"] != "apps" ||
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

type fakeTempSensors []temps.SensorStatus

func (f fakeTempSensors) Sensors() []temps.SensorStatus { return f }

func TestHealthReportsTempSensors(t *testing.T) {
	t.Parallel()

	if got := (&healthReports{}).TempSensors(); got != nil {
		t.Fatalf("without the temperatures check = %+v, want none", got)
	}
	since := time.Date(2026, 3, 2, 9, 30, 0, 0, time.UTC)
	h := newHealthReports(&healthSetup{}, checkSet{Temps: fakeTempSensors{
		{Metric: "temp.gpu.0", Label: "gpu 0", Limit: 80, Since: since},
	}}, nil, zap.NewNop())
	want := []report.Sensor{{Metric: "temp.gpu.0", Label: "gpu 0", Limit: 80, Since: since}}
	if got := h.TempSensors(); !slices.Equal(got, want) {
		t.Fatalf("TempSensors = %+v, want %+v", got, want)
	}

	merged := mergeCheckSets([]checkSet{{}, {Temps: fakeTempSensors{}}})
	if merged.Temps == nil {
		t.Fatal("mergeCheckSets dropped the temperatures sensors")
	}
}

// fakeMemoryFigures serves fixed memory figures.
type fakeMemoryFigures struct {
	fig memory.Figures
	err error
	n   *int
}

func (f fakeMemoryFigures) Figures(_ context.Context, n int) (memory.Figures, error) {
	if f.n != nil {
		*f.n = n
	}

	return f.fig, f.err
}

// fakeGuardStatus serves a fixed guard line.
type fakeGuardStatus report.Guard

func (f fakeGuardStatus) status() report.Guard { return report.Guard(f) }

func testMemoryFigures() memory.Figures {
	return memory.Figures{
		Host: memory.Host{
			Total: 62 << 30, Available: 28 << 30, SwapTotal: 8 << 30, SwapFree: 6 << 30, SomeAvg60: 1, FullAvg10: 3,
			HasPressure: true,
		},
		Top: []memory.Service{{Name: "web-app", Anon: 13 << 30}, {Name: "db", Anon: 9 << 30}},
	}
}

func TestHealthReportsMemoryStatus(t *testing.T) {
	t.Parallel()

	if got := (&healthReports{}).MemoryStatus(t.Context()); got != nil {
		t.Fatalf("without the memory check = %+v, want no block", got)
	}

	var n int
	h := newHealthReports(&healthSetup{}, checkSet{Memory: fakeMemoryFigures{fig: testMemoryFigures(), n: &n}}, nil,
		zap.NewNop())
	want := &report.Memory{
		Total: 62 << 30, Available: 28 << 30, SwapTotal: 8 << 30, SwapUsed: 2 << 30, Stalled: 3, HasPressure: true,
		Top:   []report.Consumer{{Name: "web-app", Size: 13 << 30}, {Name: "db", Size: 9 << 30}},
		Guard: &report.Guard{Mode: "off"},
	}
	if got := h.MemoryStatus(t.Context()); !reflect.DeepEqual(got, want) || n != 3 {
		t.Fatalf("MemoryStatus = %+v (top %d), want %+v (top 3)", got, n, want)
	}

	last := &report.GuardAction{Action: "restart", Service: "web-app", Report: true, At: time.Now()}
	h.guard = fakeGuardStatus{Mode: "report", Last: last}
	if got := h.MemoryStatus(t.Context()).Guard; got == nil || got.Mode != "report" || got.Last != last {
		t.Fatalf("guard = %+v, want the guard's own line", got)
	}

	h.memory = fakeMemoryFigures{err: errors.New("read host memory: no procfs")}
	if got := h.MemoryStatus(t.Context()); !got.Unreadable || got.Guard == nil {
		t.Fatalf("host unreadable: %+v, want unreadable with the guard line", got)
	}

	fig := testMemoryFigures()
	fig.Top = nil
	h.memory = fakeMemoryFigures{fig: fig, err: errors.New("read services: no system.slice")}
	if got := h.MemoryStatus(t.Context()); got.Unreadable || got.Total != 62<<30 || got.Top != nil {
		t.Fatalf("services unreadable: %+v, want the host figures without top", got)
	}
}

func TestHealthReportsWeeklyMemoryLine(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	r := &fakeRunner{}
	h := weeklyReports(nil, r, now)
	h.checks = append(h.checks, memoryCheckName)
	h.memory = fakeMemoryFigures{fig: testMemoryFigures()}
	if err := h.weekly(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	want := "\nmemory: 45% available (28 of 62 GB), swap 25% used, stalled 3%; top: web-app 13 GB\n"
	if len(r.pushed) != 1 || !strings.Contains(r.pushed[0][1], want) {
		t.Fatalf("pushed = %q, want the memory line %q", r.pushed, want)
	}
}

func TestMergeCheckSetsKeepsTheMemoryFigures(t *testing.T) {
	t.Parallel()

	if merged := mergeCheckSets([]checkSet{{}, {Memory: fakeMemoryFigures{}}}); merged.Memory == nil {
		t.Fatal("mergeCheckSets dropped the memory figures")
	}
}
