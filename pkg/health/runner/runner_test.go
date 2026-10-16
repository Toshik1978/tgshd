package runner

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/Toshik1978/tgshd/pkg/health"
	"github.com/Toshik1978/tgshd/pkg/health/config"
	"github.com/Toshik1978/tgshd/pkg/health/notify"
	"github.com/Toshik1978/tgshd/pkg/health/store"
)

const host = "host1.example.org"

// zone is the host's local zone in tests: streaks starting 03:00 UTC read 05:00.
func zone() *time.Location { return time.FixedZone("TST", 2*60*60) }

func t0() time.Time { return time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC) }

type fakeSender struct {
	mu   sync.Mutex
	sent []string
	fail bool
	// onSend runs at the start of every Send, e.g. to cancel the tick.
	onSend func()
}

func (f *fakeSender) Send(_ context.Context, text string) error {
	if f.onSend != nil {
		f.onSend()
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.fail {
		return errors.New("send failed")
	}

	f.sent = append(f.sent, text)

	return nil
}

func (f *fakeSender) texts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return slices.Clone(f.sent)
}

func (f *fakeSender) setFail(fail bool) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.fail = fail
}

type fakeHeartbeat struct {
	ticks []time.Time
	err   error
}

func (f *fakeHeartbeat) WriteTick(now time.Time) error {
	f.ticks = append(f.ticks, now)

	return f.err
}

type fakeMaintenance struct {
	held bool
	err  error
}

func (f *fakeMaintenance) MaintenanceHeld() (bool, error) { return f.held, f.err }

type fakeUptime struct {
	up  time.Duration
	err error
}

func (f *fakeUptime) Uptime() (time.Duration, error) { return f.up, f.err }

// fakeStore keeps the last checkpoint and mimics the real store's degraded
// mode: ErrDegraded once per outage, records kept until a write succeeds.
type fakeStore struct {
	mu      sync.Mutex
	state   store.State
	alerts  []store.Alert
	samples []store.SampleRow
	loadErr error
	// pending mimics the store retaining records while degraded.
	pendingAlerts  []store.Alert
	pendingSamples []store.SampleRow
	// onLoad runs inside Load, e.g. to act concurrently with it.
	onLoad      func()
	fail        bool
	degraded    bool
	checkpoints int
}

func (f *fakeStore) Load(context.Context) (store.State, error) {
	if f.onLoad != nil {
		f.onLoad()
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.state, f.loadErr
}

func (f *fakeStore) Checkpoint(ctx context.Context, dirty store.State, alerts []store.Alert,
	samples []store.SampleRow,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.checkpoints++
	f.pendingAlerts = append(f.pendingAlerts, alerts...)
	f.pendingSamples = append(f.pendingSamples, samples...)
	if err := ctx.Err(); err != nil {
		return err
	}
	if f.fail {
		if f.degraded {
			return nil
		}
		f.degraded = true

		return fmt.Errorf("%w: disk full", store.ErrDegraded)
	}
	f.degraded = false
	f.state.Conditions = slices.Clone(dirty.Conditions)
	f.state.RateMarks = slices.Clone(dirty.RateMarks)
	f.state.Queue = slices.Clone(dirty.Queue)
	if f.state.KV == nil {
		f.state.KV = map[string]string{}
	}
	maps.Copy(f.state.KV, dirty.KV)
	for i := range dirty.Baselines { // Upserted by serial, like the real store.
		b := &dirty.Baselines[i]
		j := slices.IndexFunc(f.state.Baselines, func(o store.Baseline) bool { return o.Serial == b.Serial })
		if j >= 0 {
			f.state.Baselines[j] = *b
		} else {
			f.state.Baselines = append(f.state.Baselines, *b)
		}
	}
	f.alerts = append(f.alerts, f.pendingAlerts...)
	f.samples = append(f.samples, f.pendingSamples...)
	f.pendingAlerts, f.pendingSamples = nil, nil

	return nil
}

func (f *fakeStore) Degraded() bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.degraded
}

func (f *fakeStore) alertKinds() []store.AlertKind {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]store.AlertKind, 0, len(f.alerts))
	for _, a := range f.alerts {
		out = append(out, a.Kind)
	}

	return out
}

type orderLog struct {
	mu    sync.Mutex
	names []string
}

func (o *orderLog) add(name string) {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.names = append(o.names, name)
}

func (o *orderLog) get() []string {
	o.mu.Lock()
	defer o.mu.Unlock()

	return slices.Clone(o.names)
}

type fakeCheck struct {
	mu     sync.Mutex
	name   string
	order  *orderLog
	result health.CheckResult
	runs   int
}

func newFakeCheck(order *orderLog, name string, res health.CheckResult) *fakeCheck {
	return &fakeCheck{name: name, order: order, result: res}
}

func (c *fakeCheck) Run(context.Context) health.CheckResult {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.runs++
	if c.order != nil {
		c.order.add(c.name)
	}

	return c.result
}

func (c *fakeCheck) set(res health.CheckResult) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.result = res
}

func (c *fakeCheck) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.runs
}

func reading(key string, v health.Verdict, text string) health.CheckResult {
	return health.CheckResult{Readings: []health.Reading{{Key: key, Verdict: v, Text: text}}}
}

func checkOf(c *fakeCheck) health.Check {
	return health.Check{Name: c.name, Interval: Tick, Mode: health.ModeTelegram, Run: c.Run}
}

type fixture struct {
	r     *Runner
	now   time.Time
	tlg   *fakeSender
	sms   *fakeSender
	hb    *fakeHeartbeat
	maint *fakeMaintenance
	up    *fakeUptime
	st    *fakeStore
}

func newFixture(t *testing.T, st *fakeStore, checks ...health.Check) *fixture {
	t.Helper()

	f := &fixture{
		now:   t0(),
		tlg:   &fakeSender{},
		sms:   &fakeSender{},
		hb:    &fakeHeartbeat{},
		maint: &fakeMaintenance{},
		up:    &fakeUptime{up: time.Hour},
		st:    st,
	}
	f.r = New(Options{
		Checks:      checks,
		BootGrace:   10 * time.Minute,
		Concurrency: 4,
		Heartbeat:   f.hb,
		Maintenance: f.maint,
		Uptime:      f.up,
		Store:       st,
		Host:        host,
		Telegram:    f.tlg,
		SMS:         f.sms,
		Now:         func() time.Time { return f.now },
		Location:    zone(),
	})

	return f
}

func (f *fixture) tick(t *testing.T) {
	t.Helper()

	if err := f.r.Do(context.Background()); err != nil {
		t.Fatalf("Do: %v", err)
	}
}

func (f *fixture) next(t *testing.T) {
	t.Helper()

	f.now = f.now.Add(Tick)
	f.tick(t)
}

func countContaining(texts []string, sub string) int {
	n := 0
	for _, s := range texts {
		if strings.Contains(s, sub) {
			n++
		}
	}

	return n
}

func TestWorkerIdentity(t *testing.T) {
	r := New(Options{})
	if r.Name() != "health" || r.Duration() != 30*time.Second {
		t.Fatalf("worker = %q every %v, want health every 30s", r.Name(), r.Duration())
	}
}

func TestQuietWindowsStillWriteHeartbeat(t *testing.T) {
	tests := []struct {
		name   string
		held   bool
		uptime time.Duration
		reason string
	}{
		{"maintenance lock held", true, time.Hour, health.ReasonMaintenance},
		{"boot grace", false, time.Minute, health.ReasonBootGrace},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newFakeCheck(nil, "ups", reading("ups/main", health.Bad, "on battery"))
			f := newFixture(t, &fakeStore{}, pausing(c))
			f.maint.held, f.up.up = tt.held, tt.uptime

			f.tick(t)

			if len(f.hb.ticks) != 1 || !f.hb.ticks[0].Equal(f.now) {
				t.Fatalf("heartbeat ticks = %v, want [%v]", f.hb.ticks, f.now)
			}
			if c.count() != 0 {
				t.Fatalf("check ran %d times inside a quiet window", c.count())
			}
			if got := f.r.Snapshot().Quiet; got != tt.reason {
				t.Fatalf("quiet reason = %q, want %q", got, tt.reason)
			}
			if f.st.checkpoints != 1 || len(f.tlg.texts()) != 0 {
				t.Fatalf("quiet tick checkpointed %d times (want 1) and sent %v", f.st.checkpoints, f.tlg.texts())
			}

			f.maint.held, f.up.up = false, time.Hour
			f.next(t)

			if c.count() != 1 || f.r.Snapshot().Quiet != "" || len(f.hb.ticks) != 2 {
				t.Fatalf("after quiet: runs=%d quiet=%q heartbeats=%d", c.count(), f.r.Snapshot().Quiet,
					len(f.hb.ticks))
			}
		})
	}
}

func TestNetworkFirstOfflineSkipAndFlushBeforeDispatch(t *testing.T) {
	order := &orderLog{}
	netw := newFakeCheck(order, "network", reading(KeyNetwork, health.Bad, "internet unreachable"))
	web := newFakeCheck(order, "web", reading("http/web", health.Good, ""))
	disk := newFakeCheck(order, "disk", reading("disk/sda", health.Bad, "disk failing"))

	webCheck := checkOf(web)
	webCheck.NeedsInternet = true
	netCheck := checkOf(netw)
	netCheck.Mode = health.ModeSMS
	// network is listed last but must still run first.
	f := newFixture(t, &fakeStore{}, checkOf(disk), webCheck, netCheck)

	f.tick(t)

	if got := order.get(); len(got) != 2 || got[0] != "network" || got[1] != "disk" {
		t.Fatalf("run order = %v, want [network disk] (web skipped while offline)", got)
	}
	// The disk PROBLEM queued while offline goes by SMS too (FR-030).
	wantSMS := []string{
		host + ": PROBLEM: internet unreachable (since 05:00)",
		host + ": PROBLEM: disk failing (since 05:00)",
	}
	if got := f.sms.texts(); !slices.Equal(got, wantSMS) {
		t.Fatalf("sms = %q, want %q", got, wantSMS)
	}
	if got := f.tlg.texts(); len(got) != 0 {
		t.Fatalf("telegram sent %q while offline, want everything queued", got)
	}

	netw.set(reading(KeyNetwork, health.Good, ""))
	f.next(t)

	if web.count() != 1 {
		t.Fatalf("web ran %d times after the outside came back, want 1", web.count())
	}
	want := []string{
		host + ": PROBLEM: disk failing (since 05:00)",
		host + ": RESOLVED: internet unreachable",
	}
	if got := f.tlg.texts(); !slices.Equal(got, want) {
		t.Fatalf("telegram = %q, want queued PROBLEM flushed before the RESOLVED %q", got, want)
	}
}

func TestNetworkDownIsOfflineUntilGood(t *testing.T) {
	netw := newFakeCheck(nil, "network", reading(KeyNetwork, health.Bad, "gateway down"))
	netCheck := checkOf(netw)
	netCheck.Interval = 5 * time.Minute
	web := newFakeCheck(nil, "web", reading("http/web", health.Good, ""))
	webCheck := checkOf(web)
	webCheck.NeedsInternet = true
	f := newFixture(t, &fakeStore{}, netCheck, webCheck)

	f.tick(t)
	f.next(t) // network not due: the outside is still known down.

	if netw.count() != 1 || web.count() != 0 {
		t.Fatalf("network runs=%d web runs=%d, want 1 and 0", netw.count(), web.count())
	}

	netw.set(health.CheckResult{Err: errors.New("no ping capability")})
	f.now = f.now.Add(5 * time.Minute)
	f.tick(t)

	if web.count() != 0 {
		t.Fatal("an unreadable network check must not bring the outside back")
	}

	netw.set(reading(KeyNetwork, health.Good, ""))
	f.now = f.now.Add(5 * time.Minute)
	f.tick(t)

	if web.count() != 1 {
		t.Fatalf("web runs=%d after the router came back, want 1", web.count())
	}
}

func TestDependsOnSkipsWhileTargetBad(t *testing.T) {
	a := newFakeCheck(nil, "a", reading("http/a", health.Bad, "a down"))
	b := newFakeCheck(nil, "b", reading("http/b", health.Good, ""))
	aCheck := checkOf(a)
	aCheck.Threshold = 10 * time.Minute // a streak, not yet open, already counts as bad.
	bCheck := checkOf(b)
	bCheck.DependsOn = "http/a"
	f := newFixture(t, &fakeStore{}, aCheck, bCheck)

	f.tick(t) // http/a was not bad yet before this tick.
	f.next(t)

	if b.count() != 1 {
		t.Fatalf("b ran %d times, want 1 (skipped while http/a is bad)", b.count())
	}

	a.set(reading("http/a", health.Good, ""))
	f.next(t)
	f.next(t)

	if b.count() != 2 {
		t.Fatalf("b ran %d times after http/a recovered, want 2", b.count())
	}
}

func TestCheckErrIsCheckerErrorWithoutStateChange(t *testing.T) {
	c := newFakeCheck(nil, "ups", reading("ups/main", health.Bad, "on battery"))
	f := newFixture(t, &fakeStore{}, checkOf(c))
	f.tick(t)

	c.set(health.CheckResult{Err: errors.New("upsc: timeout")})
	f.next(t)
	f.next(t)

	tlg := f.tlg.texts()
	if countContaining(tlg, "RESOLVED") != 0 {
		t.Fatalf("a failed run resolved the problem: %q", tlg)
	}
	if n := countContaining(tlg, "CHECKER ERROR: ups: upsc: timeout"); n != 1 {
		t.Fatalf("checker errors = %d in %q, want 1 (rate limited)", n, tlg)
	}
	if open := f.r.Snapshot().Open; len(open) != 1 || open[0].Key != "ups/main" {
		t.Fatalf("open = %+v, want ups/main still open", open)
	}
	want := []store.AlertKind{store.KindProblem, store.KindCheckerError}
	if got := f.st.alertKinds(); !slices.Equal(got, want) {
		t.Fatalf("alert log = %v, want %v", got, want)
	}
	if a := f.st.alerts[1]; a.Key != "ups/run/error" || a.Check != "ups" {
		t.Fatalf("checker alert = %+v, want key ups/run/error", a)
	}
}

func TestPartialFailureAppliesReadings(t *testing.T) {
	c := newFakeCheck(nil, "temps", health.CheckResult{Readings: []health.Reading{
		{Key: "temps/cpu", Verdict: health.Bad, Text: "cpu hot"},
		{Key: "temps/board", Verdict: health.Bad, Text: "board hot"},
	}})
	f := newFixture(t, &fakeStore{}, checkOf(c))
	f.tick(t)

	// The GPU source failed; the cpu reading is still judged, board is absent.
	c.set(health.CheckResult{
		Readings: []health.Reading{{Key: "temps/cpu", Verdict: health.Good}},
		Err:      errors.New("nvidia-smi: timeout"),
	})
	f.next(t)

	tlg := f.tlg.texts()
	if countContaining(tlg, "RESOLVED: cpu hot") != 1 {
		t.Fatalf("telegram = %q, want the cpu RESOLVED despite the partial failure", tlg)
	}
	if countContaining(tlg, "CHECKER ERROR: temps: nvidia-smi: timeout") != 1 {
		t.Fatalf("telegram = %q, want the checker error", tlg)
	}
	if open := f.r.Snapshot().Open; len(open) != 1 || open[0].Key != "temps/board" {
		t.Fatalf("open = %+v, want temps/board untouched", open)
	}
}

func TestUnknownReadingsAreCheckerErrors(t *testing.T) {
	c := newFakeCheck(nil, "temps", health.CheckResult{Readings: []health.Reading{
		{Key: "temps/cpu", Verdict: health.Unknown, Text: "sensor unreadable"},
		{Key: "temps/gpu", Verdict: health.Unknown},
	}})
	f := newFixture(t, &fakeStore{}, checkOf(c))
	f.tick(t)

	want := []string{
		host + ": CHECKER ERROR: temps: sensor unreadable",
		host + ": CHECKER ERROR: temps: temps/gpu could not be read",
	}
	if got := f.tlg.texts(); !slices.Equal(got, want) {
		t.Fatalf("telegram = %q, want %q", got, want)
	}
	if len(f.st.alerts) != 2 || f.st.alerts[0].Key != "temps/cpu/unreadable" ||
		f.st.alerts[1].Key != "temps/gpu/unreadable" {
		t.Fatalf("alert log = %+v, want checker errors keyed temps/{cpu,gpu}/unreadable", f.st.alerts)
	}
	if conds := f.st.state.Conditions; len(conds) != 0 {
		t.Fatalf("conditions = %+v, want no state change", conds)
	}
}

func TestDispatchModesEventsAndSamples(t *testing.T) {
	c := newFakeCheck(nil, "disks", health.CheckResult{
		Readings: []health.Reading{
			{Key: "disks/TEST0000000001/health", Verdict: health.Bad, Text: "failing", Mode: health.ModeSMS},
			{Key: "disks/TEST0000000001/media", Kind: health.Event, Text: "media errors rose"},
		},
		Samples: []health.Sample{{Metric: "drive.TEST0000000001.wear", Value: 7}},
	})
	f := newFixture(t, &fakeStore{}, checkOf(c))
	f.tick(t)

	if got := f.sms.texts(); !slices.Equal(got, []string{host + ": PROBLEM: failing (since 05:00)"}) {
		t.Fatalf("sms = %q, want the PROBLEM (reading mode overrides the check)", got)
	}
	if got := f.tlg.texts(); !slices.Equal(got, []string{host + ": media errors rose"}) {
		t.Fatalf("telegram = %q, want the event", got)
	}
	if got := f.st.alertKinds(); !slices.Equal(got, []store.AlertKind{store.KindProblem, store.KindEvent}) {
		t.Fatalf("alert log = %v", got)
	}
	a := f.st.alerts[0]
	if !a.At.Equal(f.now) || a.Check != "disks" || a.Key != "disks/TEST0000000001/health" || a.Text != "failing" {
		t.Fatalf("problem record = %+v", a)
	}
	want := []store.SampleRow{{At: f.now, Metric: "drive.TEST0000000001.wear", Value: 7}}
	if !slices.Equal(f.st.samples, want) {
		t.Fatalf("samples = %+v, want %+v", f.st.samples, want)
	}
	if conds := f.st.state.Conditions; len(conds) != 1 || !conds[0].Open {
		t.Fatalf("checkpointed conditions = %+v, want one open", conds)
	}
}

func TestChecksRunOnlyWhenDue(t *testing.T) {
	c := newFakeCheck(nil, "ups", reading("ups/main", health.Good, ""))
	chk := checkOf(c)
	chk.Interval = 5 * time.Minute
	f := newFixture(t, &fakeStore{}, chk)

	for range 10 {
		f.next(t)
	}

	if c.count() != 1 {
		t.Fatalf("ran %d times in 5 minutes, want 1", c.count())
	}

	f.next(t)

	if c.count() != 2 {
		t.Fatalf("ran %d times after 5m30s, want 2", c.count())
	}
}

func TestDegradedStoreReportedOnce(t *testing.T) {
	st := &fakeStore{fail: true}
	c := newFakeCheck(nil, "ups", reading("ups/main", health.Good, ""))
	f := newFixture(t, st, checkOf(c))

	f.tick(t)
	f.next(t)
	f.next(t)

	if n := countContaining(f.tlg.texts(), "CHECKER ERROR: health: cannot save state"); n != 1 {
		t.Fatalf("store failure reported %d times in %q, want once", n, f.tlg.texts())
	}
	if !f.r.Snapshot().StoreDegraded || c.count() != 3 {
		t.Fatalf("degraded=%v runs=%d, want degraded and still running", f.r.Snapshot().StoreDegraded, c.count())
	}

	st.mu.Lock()
	st.fail = false
	st.mu.Unlock()
	f.next(t)

	if f.r.Snapshot().StoreDegraded {
		t.Fatal("store still degraded after a successful checkpoint")
	}
	if got := f.st.alertKinds(); !slices.Equal(got, []store.AlertKind{store.KindCheckerError}) {
		t.Fatalf("alert log = %v, want the checker error recorded", got)
	}
}

func TestRestartRestoresStateQueueAndMarks(t *testing.T) {
	st := &fakeStore{}
	ups := newFakeCheck(nil, "ups", reading("ups/main", health.Bad, "on battery"))
	kern := newFakeCheck(nil, "kernel", health.CheckResult{Err: errors.New("kmsg unreadable")})
	f1 := newFixture(t, st, checkOf(ups), checkOf(kern))
	f1.tlg.setFail(true) // everything ends up in the persisted queue.
	f1.tick(t)

	if len(st.state.Queue) != 2 || len(st.state.RateMarks) == 0 || len(st.state.Conditions) != 1 {
		t.Fatalf("checkpoint = %+v, want 2 queued, rate marks and one condition", st.state)
	}

	f2 := newFixture(t, st, checkOf(ups), checkOf(kern))
	f2.now = f1.now.Add(Tick)
	f2.tick(t)

	want := []string{
		host + ": PROBLEM: on battery (since 05:00)",
		host + ": CHECKER ERROR: kernel: kmsg unreadable",
	}
	if got := f2.tlg.texts(); !slices.Equal(got, want) {
		t.Fatalf("after restart telegram = %q, want only the flushed queue %q", got, want)
	}

	ups.set(reading("ups/main", health.Good, ""))
	f2.next(t)

	if got := f2.tlg.texts(); len(got) != 3 || got[2] != host+": RESOLVED: on battery" {
		t.Fatalf("telegram = %q, want one RESOLVED", got)
	}
}

func TestFailedLoadKeepsStoredState(t *testing.T) {
	stored := store.State{
		Conditions: []health.Condition{{
			Key: "ups/main", Check: "ups", StreakStart: t0().Add(-time.Hour), LastReadingAt: t0().Add(-time.Minute),
			LastText: "on battery", Open: true, OpenedAt: t0().Add(-time.Hour),
		}},
		Queue: []notify.QueuedMessage{
			{ID: 5, CreatedAt: t0().Add(-time.Hour), Channel: "tlg", Text: "stored message"},
		},
		RateMarks: []health.RateMark{{Key: "kernel/run/error", Channel: "tlg", LastSentAt: t0().Add(-time.Hour)}},
	}
	st := &fakeStore{state: stored, loadErr: errors.New("database locked")}
	ups := newFakeCheck(nil, "ups", health.CheckResult{})
	disk := newFakeCheck(nil, "disk", reading("disk/sda", health.Bad, "disk failing"))
	f := newFixture(t, st, checkOf(ups), checkOf(disk))
	f.tlg.setFail(true)

	f.tick(t)

	if st.checkpoints != 0 {
		t.Fatalf("checkpointed %d times before state was loaded, want 0", st.checkpoints)
	}

	st.mu.Lock()
	st.loadErr = nil
	st.mu.Unlock()
	f.tlg.setFail(false)
	ups.set(reading("ups/main", health.Bad, "on battery"))
	f.next(t)

	want := []string{
		"stored message",
		host + ": CHECKER ERROR: health: cannot load state: database locked",
		host + ": PROBLEM: disk failing (since 05:00)",
	}
	if got := f.tlg.texts(); !slices.Equal(got, want) {
		t.Fatalf("telegram = %q, want stored queue first, no repeated ups PROBLEM: %q", got, want)
	}
	keys := make([]string, 0, len(st.state.Conditions))
	for _, c := range st.state.Conditions {
		keys = append(keys, c.Key)
	}
	if !slices.Equal(keys, []string{"disk/sda", "ups/main"}) {
		t.Fatalf("checkpointed conditions = %v, want stored ups/main merged with disk/sda", keys)
	}
	if len(st.state.Queue) != 0 {
		t.Fatalf("queue = %+v, want all flushed", st.state.Queue)
	}
	if got := st.alertKinds(); !slices.Equal(got, []store.AlertKind{store.KindProblem, store.KindCheckerError}) {
		t.Fatalf("alert log = %v, want the records kept while unloaded", got)
	}
	marks := make([]string, 0, len(st.state.RateMarks))
	for _, m := range st.state.RateMarks {
		marks = append(marks, m.Key)
	}
	if !slices.Equal(marks, []string{"health/store/load", "kernel/run/error"}) {
		t.Fatalf("rate marks = %v, want stored and in-memory merged", marks)
	}
}

func TestRecordsWaitingForLoadAreCapped(t *testing.T) {
	samples := make([]health.Sample, store.MaxPendingSamples/2+1)
	for i := range samples {
		samples[i] = health.Sample{Metric: "m", Value: float64(i)}
	}
	st := &fakeStore{loadErr: errors.New("database locked")}
	ups := newFakeCheck(nil, "ups", health.CheckResult{Samples: samples})
	f := newFixture(t, st, checkOf(ups))

	f.tick(t)
	f.next(t)
	f.next(t)

	if n := len(f.r.pendingSamples); n != store.MaxPendingSamples {
		t.Fatalf("pending samples = %d, want capped at %d", n, store.MaxPendingSamples)
	}
	if last := f.r.pendingSamples[len(f.r.pendingSamples)-1]; !last.At.Equal(f.now) {
		t.Fatalf("newest pending sample at %v, want the last tick %v", last.At, f.now)
	}
}

func TestFailedLoadSendsNothingAndNextTickRaisesIt(t *testing.T) {
	st := &fakeStore{loadErr: errors.New("database locked")}
	f := newFixture(t, st)

	// A Load at start must return at once: it never sends synchronously.
	if err := f.r.Load(context.Background()); err == nil {
		t.Fatal("Load() = nil, want the store error")
	}
	if got := f.tlg.texts(); len(got) != 0 {
		t.Fatalf("Load sent %q, want nothing until the next tick", got)
	}

	// The store recovers before the first tick: the start failure is still raised.
	st.mu.Lock()
	st.loadErr = nil
	st.mu.Unlock()
	f.tick(t)

	want := []string{host + ": CHECKER ERROR: health: cannot load state: database locked"}
	if got := f.tlg.texts(); !slices.Equal(got, want) {
		t.Fatalf("telegram = %q, want %q", got, want)
	}

	f.next(t)
	if got := f.tlg.texts(); len(got) != 1 {
		t.Fatalf("telegram = %q, want the failure raised once", got)
	}
}

func TestMessageQueuedDuringLoadSurvives(t *testing.T) {
	st := &fakeStore{state: store.State{
		Queue: []notify.QueuedMessage{{ID: 7, CreatedAt: t0(), Channel: "tlg", Text: "stored message"}},
	}}
	tlg := &fakeSender{fail: true}
	var r *Runner
	st.onLoad = func() {
		// Another goroutine (e.g. config errors reported at start) queues a
		// message while the store is being read.
		done := make(chan struct{})
		go func() {
			r.CheckerError("cfg/http/invalid", "bad entry")
			close(done)
		}()
		<-done
	}
	r = New(Options{Store: st, Telegram: tlg, Host: host, Now: t0})

	if err := r.Load(context.Background()); err != nil {
		t.Fatal(err)
	}

	q := r.notifier.Export()
	if len(q) != 2 || q[0].Text != "stored message" || !strings.Contains(q[1].Text, "bad entry") || q[1].ID <= 7 {
		t.Fatalf("queue = %+v, want the stored message then the one queued during Load", q)
	}
}

func TestTickDeadlineCoversAllRuns(t *testing.T) {
	tests := []struct {
		name    string
		network bool
	}{
		{"batch only", false},
		{"network hung too", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			release := make(chan struct{})
			defer close(release)
			hung := func(context.Context) health.CheckResult {
				<-release // Ignores its context on purpose.

				return health.CheckResult{}
			}
			// More hung checks than the concurrency limit, each with a long interval.
			checks := make([]health.Check, 0, 6)
			for i := range 5 {
				checks = append(checks, health.Check{Name: fmt.Sprintf("h%d", i), Interval: 5 * time.Minute, Run: hung})
			}
			if tt.network {
				checks = append(checks, health.Check{Name: "network", Interval: 5 * time.Minute, Run: hung})
			}
			tlg := &fakeSender{}
			r := New(Options{
				Checks: checks, Concurrency: 2, RunBudget: 100 * time.Millisecond, Telegram: tlg, Host: host, Now: t0,
			})

			start := time.Now()
			if err := r.Do(context.Background()); err != nil {
				t.Fatal(err)
			}

			if elapsed := time.Since(start); elapsed > 2*time.Second {
				t.Fatalf("tick took %v, want it bounded by the 100ms tick budget", elapsed)
			}
			texts := tlg.texts()
			if len(texts) != len(checks) {
				t.Fatalf("telegram = %q, want one checker error per check", texts)
			}
			for _, chk := range checks {
				if countContaining(texts, "CHECKER ERROR: "+chk.Name+": ") != 1 {
					t.Fatalf("telegram = %q, want a timeout or not-started error for %s", texts, chk.Name)
				}
			}
			if n := countContaining(texts, "not started"); n == 0 {
				t.Fatalf("telegram = %q, want checks queued behind the limit reported as not started", texts)
			}
		})
	}
}

func TestNotStartedCheckRunsNextTick(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	hung := health.Check{Name: "hung", Interval: 5 * time.Minute, Run: func(context.Context) health.CheckResult {
		<-release

		return health.CheckResult{}
	}}
	later := newFakeCheck(nil, "later", reading("later/x", health.Good, ""))
	laterCheck := checkOf(later)
	laterCheck.Interval = 5 * time.Minute
	r := New(Options{
		Checks: []health.Check{hung, laterCheck}, Concurrency: 1, RunBudget: 50 * time.Millisecond, Now: t0,
	})

	if err := r.Do(context.Background()); err != nil {
		t.Fatal(err)
	}
	if later.count() != 0 {
		t.Fatalf("later ran %d times behind a hung check with concurrency 1, want 0", later.count())
	}
	if err := r.Do(context.Background()); err != nil {
		t.Fatal(err)
	}

	if later.count() != 1 {
		t.Fatalf("a check that was not started must run on the next tick, ran %d times", later.count())
	}
}

func TestHungCheckTimesOutAndIsNotRelaunched(t *testing.T) {
	release := make(chan struct{})
	var runs atomic.Int32
	hung := health.Check{Name: "hung", Interval: 50 * time.Millisecond, Run: func(context.Context) health.CheckResult {
		runs.Add(1)
		<-release // Ignores its context on purpose.

		return reading("hung/x", health.Bad, "late")
	}}
	ok := newFakeCheck(nil, "ok", reading("ok/x", health.Good, ""))
	f := newFixture(t, &fakeStore{}, hung, checkOf(ok))

	start := time.Now()
	f.tick(t)

	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("tick took %v, want it bounded by the run budget", elapsed)
	}
	if ok.count() != 1 {
		t.Fatal("a hung check blocked the others")
	}
	if n := countContaining(f.tlg.texts(), "CHECKER ERROR: hung: run did not finish within 50ms"); n != 1 {
		t.Fatalf("telegram = %q, want one timeout checker error", f.tlg.texts())
	}

	f.next(t)

	if runs.Load() != 1 {
		t.Fatalf("hung check launched %d times while still in flight, want 1", runs.Load())
	}

	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for f.r.running[0].Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if conds := f.r.engine.Conditions(); len(conds) != 0 {
		t.Fatalf("late result was applied: %+v", conds)
	}

	f.next(t)

	if runs.Load() != 2 {
		t.Fatalf("check launched %d times after it finished, want 2", runs.Load())
	}
}

func TestRunDeadline(t *testing.T) {
	tests := []struct {
		name     string
		interval time.Duration
		want     time.Duration
	}{
		{"capped at MaxRunTime", 5 * time.Minute, MaxRunTime},
		{"interval when shorter", Tick, Tick},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var left time.Duration
			chk := health.Check{Name: "c", Interval: tt.interval, Run: func(ctx context.Context) health.CheckResult {
				if dl, ok := ctx.Deadline(); ok {
					left = time.Until(dl)
				}

				return health.CheckResult{}
			}}
			r := New(Options{Checks: []health.Check{chk}, Now: t0})

			if err := r.Do(context.Background()); err != nil {
				t.Fatal(err)
			}

			if left <= tt.want-5*time.Second || left > tt.want {
				t.Fatalf("run deadline in %v, want about %v", left, tt.want)
			}
		})
	}
}

func TestShutdownDropsTickResults(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	chk := health.Check{Name: "ups", Interval: Tick, Run: func(context.Context) health.CheckResult {
		cancel()

		return health.CheckResult{
			Readings: []health.Reading{{Key: "ups/main", Verdict: health.Bad, Text: "on battery"}},
			Err:      errors.New("interrupted"),
		}
	}}
	f := newFixture(t, &fakeStore{}, chk)

	if err := f.r.Do(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Do = %v, want context.Canceled", err)
	}

	if len(f.tlg.texts()) != 0 || f.st.checkpoints != 0 || len(f.r.engine.Conditions()) != 0 {
		t.Fatalf("shutdown tick sent %q, checkpointed %d times, conditions %+v",
			f.tlg.texts(), f.st.checkpoints, f.r.engine.Conditions())
	}
}

func TestShutdownAfterTheRunsStillSavesTheTick(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := newFakeCheck(nil, "ups", health.CheckResult{
		Readings: []health.Reading{{Key: "ups/main", Verdict: health.Bad, Text: "on battery"}},
		Samples:  []health.Sample{{Metric: "ups.charge", Value: 50}},
	})
	f := newFixture(t, &fakeStore{}, checkOf(c))
	f.tlg.onSend = cancel // shutdown arrives while the tick delivers

	if err := f.r.Do(ctx); err != nil {
		t.Fatalf("Do = %v, want the saved tick to succeed", err)
	}
	if got := f.st.alertKinds(); !slices.Equal(got, []store.AlertKind{store.KindProblem}) {
		t.Fatalf("alert log = %v, want the sent PROBLEM", got)
	}
	if len(f.st.samples) != 1 {
		t.Fatalf("samples = %+v, want the tick's sample", f.st.samples)
	}
	if conds := f.st.state.Conditions; len(conds) != 1 || !conds[0].Open {
		t.Fatalf("checkpointed conditions = %+v, want one open", conds)
	}
}

func TestHeartbeatAndProbeErrorsAreCheckerErrors(t *testing.T) {
	c := newFakeCheck(nil, "ups", reading("ups/main", health.Good, ""))
	f := newFixture(t, &fakeStore{}, checkOf(c))
	f.hb.err = errors.New("read-only file system")
	f.maint.err = errors.New("permission denied")
	f.up.err = errors.New("no procfs")

	f.tick(t)

	if c.count() != 1 {
		t.Fatal("check did not run")
	}
	for _, sub := range []string{"heartbeat", "maintenance lock", "uptime"} {
		if countContaining(f.tlg.texts(), sub) != 1 {
			t.Fatalf("telegram = %q, want one checker error mentioning %q", f.tlg.texts(), sub)
		}
	}
}

func TestPanickingCheckIsCheckerError(t *testing.T) {
	chk := health.Check{Name: "boom", Interval: Tick, Run: func(context.Context) health.CheckResult {
		panic("nil map")
	}}
	f := newFixture(t, &fakeStore{}, chk)
	f.tick(t)

	if countContaining(f.tlg.texts(), "CHECKER ERROR: boom: panic: nil map") != 1 {
		t.Fatalf("telegram = %q", f.tlg.texts())
	}
}

func TestClear(t *testing.T) {
	st := &fakeStore{}
	c := newFakeCheck(nil, "ups", reading("ups/main", health.Bad, "on battery"))
	f := newFixture(t, st, checkOf(c))
	f.tick(t)

	sent := len(f.tlg.texts())
	if !f.r.Clear("ups/main") {
		t.Fatal("Clear of an open condition returned false")
	}
	if f.r.Clear("ups/other") {
		t.Fatal("Clear of an unknown key returned true")
	}

	c.set(health.CheckResult{})
	f.next(t)

	if len(f.tlg.texts()) != sent {
		t.Fatalf("Clear sent %q", f.tlg.texts()[sent:])
	}
	if open := f.r.Snapshot().Open; len(open) != 0 {
		t.Fatalf("open after clear = %+v", open)
	}
	want := []store.AlertKind{store.KindProblem, store.KindCleared}
	if got := st.alertKinds(); !slices.Equal(got, want) {
		t.Fatalf("alert log = %v, want %v", got, want)
	}
	if a := st.alerts[1]; a.Key != "ups/main" || a.Check != "ups" || !a.At.Equal(t0()) {
		t.Fatalf("cleared record = %+v", a)
	}
}

func TestForgetClearsSilently(t *testing.T) {
	st := &fakeStore{}
	c := newFakeCheck(nil, "temps", reading("temps/s1", health.Bad, "s1 hot"))
	f := newFixture(t, st, checkOf(c))
	f.tick(t)

	sent := len(f.tlg.texts())
	c.set(health.CheckResult{Forget: []string{"temps/s1", "temps/unknown"}})
	f.next(t)

	if len(f.tlg.texts()) != sent {
		t.Fatalf("Forget sent %q", f.tlg.texts()[sent:])
	}
	if open := f.r.Snapshot().Open; len(open) != 0 {
		t.Fatalf("open after forget = %+v", open)
	}
	want := []store.AlertKind{store.KindProblem, store.KindCleared}
	if got := st.alertKinds(); !slices.Equal(got, want) {
		t.Fatalf("alert log = %v, want %v", got, want)
	}
	if a := st.alerts[1]; a.Key != "temps/s1" || a.Check != "temps" || a.Text != "no longer judged" {
		t.Fatalf("cleared record = %+v", a)
	}
}

// TestForgetRecordFitsAFormerNetworkKey: the cleared record of a forgotten
// 001 network key does not claim a sensor is gone.
func TestForgetRecordFitsAFormerNetworkKey(t *testing.T) {
	st := &fakeStore{}
	st.state.Conditions = []health.Condition{{Key: "network/router", Check: "network", Open: true}}
	c := newFakeCheck(nil, "network", health.CheckResult{Forget: []string{"network/router"}})
	f := newFixture(t, st, checkOf(c))
	f.tick(t)

	if len(st.alerts) != 1 || st.alerts[0].Kind != store.KindCleared || st.alerts[0].Text != "no longer judged" {
		t.Fatalf("alert log = %+v, want one cleared record reading \"no longer judged\"", st.alerts)
	}
}

func TestForgetIsScopedToTheReportingCheck(t *testing.T) {
	st := &fakeStore{}
	a := newFakeCheck(nil, "a", reading("a/x", health.Bad, "a bad"))
	b := newFakeCheck(nil, "b", reading("b/y", health.Bad, "b bad"))
	f := newFixture(t, st, checkOf(a), checkOf(b))
	f.tick(t)

	a.set(health.CheckResult{Forget: []string{"b/y"}})
	b.set(health.CheckResult{})
	f.next(t)

	open := f.r.Snapshot().Open
	if len(open) != 2 {
		t.Fatalf("open = %+v, want both conditions still open", open)
	}
	if slices.Contains(st.alertKinds(), store.KindCleared) {
		t.Fatalf("alert log = %v, want no cleared record", st.alertKinds())
	}
}

func TestForgetWithErrAndRecordOrder(t *testing.T) {
	st := &fakeStore{}
	c := newFakeCheck(nil, "temps", reading("temps/s1", health.Bad, "s1 hot"))
	f := newFixture(t, st, checkOf(c))
	f.tick(t)

	c.set(health.CheckResult{
		Readings: []health.Reading{{Key: "temps/s1", Verdict: health.Good, Text: "s1 gone", Kind: health.Event}},
		Forget:   []string{"temps/s1"},
		Err:      errors.New("gpu: timeout"),
	})
	f.next(t)

	if open := f.r.Snapshot().Open; len(open) != 0 {
		t.Fatalf("open = %+v, want Forget applied despite Err", open)
	}
	got := st.alertKinds()
	iEvent := slices.Index(got, store.KindEvent)
	iClear := slices.Index(got, store.KindCleared)
	if iEvent < 0 || iClear < iEvent {
		t.Fatalf("alert log = %v, want the event before the clear", got)
	}
}

func TestSnapshot(t *testing.T) {
	c := newFakeCheck(nil, "ups", reading("ups/main", health.Bad, "on battery"))
	ok := newFakeCheck(nil, "web", reading("http/web", health.Good, ""))
	f := newFixture(t, &fakeStore{}, checkOf(c), checkOf(ok))

	if s := f.r.Snapshot(); !s.LastCycle.IsZero() || s.Checks != 2 {
		t.Fatalf("snapshot before any cycle = %+v", s)
	}

	f.tick(t)
	f.next(t)

	s := f.r.Snapshot()
	if s.Checks != 2 || !s.LastCycle.Equal(f.now) || s.Quiet != "" || s.StoreDegraded {
		t.Fatalf("snapshot = %+v", s)
	}
	if len(s.Open) != 1 || s.Open[0].Key != "ups/main" || s.Open[0].Text != "on battery" ||
		!s.Open[0].Since.Equal(t0()) {
		t.Fatalf("open = %+v, want ups/main on battery since %v", s.Open, t0())
	}
	if s.Open[0].Since.Format("15:04") != "05:00" {
		t.Fatalf("since = %v, want host local 05:00", s.Open[0].Since)
	}
}

func TestIsOpen(t *testing.T) {
	open := newFakeCheck(nil, "ups", reading("ups/main", health.Bad, "on battery"))
	streak := newFakeCheck(nil, "temps", reading("temps/board", health.Bad, "hot"))
	streakCheck := checkOf(streak)
	streakCheck.Threshold = 10 * time.Minute // bad, but not yet open
	f := newFixture(t, &fakeStore{}, checkOf(open), streakCheck)

	if f.r.IsOpen("ups/main") {
		t.Fatal("open before any cycle")
	}
	f.tick(t)

	for key, want := range map[string]bool{"ups/main": true, "temps/board": false, "http/none": false} {
		if got := f.r.IsOpen(key); got != want {
			t.Errorf("IsOpen(%q) = %v, want %v", key, got, want)
		}
	}
	if !f.r.Clear("ups/main") || f.r.IsOpen("ups/main") {
		t.Error("a cleared condition must not be open")
	}
}

func TestConcurrencyBound(t *testing.T) {
	const limit = 2

	var inflight, peak atomic.Int32
	barrier := make(chan struct{})
	var once sync.Once
	run := func(context.Context) health.CheckResult {
		n := inflight.Add(1)
		defer inflight.Add(-1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		if n == limit {
			once.Do(func() { close(barrier) })
		}
		// The first runs wait until limit of them are in flight together.
		select {
		case <-barrier:
		case <-time.After(5 * time.Second):
		}

		return health.CheckResult{}
	}
	checks := make([]health.Check, 0, 6)
	for i := range 6 {
		checks = append(checks, health.Check{Name: fmt.Sprintf("c%d", i), Interval: Tick, Run: run})
	}
	r := New(Options{Checks: checks, Concurrency: limit, Now: t0})

	if err := r.Do(context.Background()); err != nil {
		t.Fatal(err)
	}

	if got := peak.Load(); got != limit {
		t.Fatalf("peak concurrency = %d, want %d", got, limit)
	}
}

func TestQueueDropsAreLogged(t *testing.T) {
	queue := make([]notify.QueuedMessage, 0, notify.MaxQueue+1)
	for i := range notify.MaxQueue + 1 {
		queue = append(queue, notify.QueuedMessage{ID: int64(i + 1), CreatedAt: t0(), Channel: "tlg", Text: "x"})
	}
	core, logs := observer.New(zap.WarnLevel)
	r := New(Options{Store: &fakeStore{state: store.State{Queue: queue}}, Logger: zap.New(core), Now: t0})

	if err := r.Load(context.Background()); err != nil {
		t.Fatal(err)
	}

	entries := logs.FilterMessage("health message dropped from the retry queue").All()
	if len(entries) != 1 || entries[0].ContextMap()["id"] != int64(1) {
		t.Fatalf("drop log = %+v, want the oldest message (id 1) logged", entries)
	}
}

func TestDistinctCheckerErrors(t *testing.T) {
	ce := func(subject, kind, msg string) error {
		return health.CheckError{Subject: subject, Kind: kind, Err: errors.New(msg)}
	}
	c := newFakeCheck(nil, "disks", health.CheckResult{Err: errors.Join(
		ce("TEST0000000001", "no-verdict", "no verdict"),
		ce("TEST0000000002", "no-temperature", "no temperature"),
		ce("", "discovery", "scan failed"),
		errors.New("plain failure"),
	)})
	f := newFixture(t, &fakeStore{}, checkOf(c))
	f.tick(t)

	tlg := f.tlg.texts()
	for _, sub := range []string{
		"TEST0000000001/no-verdict: no verdict",
		"TEST0000000002/no-temperature: no temperature",
		"discovery: scan failed",
		"plain failure",
	} {
		if n := countContaining(tlg, sub); n != 1 {
			t.Fatalf("%q delivered %d times in %q, want 1", sub, n, tlg)
		}
	}
	keys := make([]string, 0, len(f.st.alerts))
	for _, a := range f.st.alerts {
		keys = append(keys, a.Key)
	}
	want := []string{
		"disks/TEST0000000001/no-verdict", "disks/TEST0000000002/no-temperature",
		"disks/discovery", "disks/run/error",
	}
	if !slices.Equal(keys, want) {
		t.Fatalf("alert keys = %v, want %v", keys, want)
	}

	// Same faults again, plus one new key: only the new one is delivered
	// while the others are inside their 24 h window.
	c.set(health.CheckResult{Err: errors.Join(
		ce("TEST0000000001", "no-verdict", "no verdict"),
		ce("TEST0000000003", "no-verdict", "third drive"),
	)})
	f.next(t)

	tlg = f.tlg.texts()
	if n := countContaining(tlg, "TEST0000000003/no-verdict: third drive"); n != 1 {
		t.Fatalf("new key delivered %d times in %q, want 1", n, tlg)
	}
	if n := countContaining(tlg, "TEST0000000001/no-verdict"); n != 1 {
		t.Fatalf("old key delivered %d times in %q, want 1 (rate limited)", n, tlg)
	}
}

func TestNestedPlainErrorKeepsRunErrorKey(t *testing.T) {
	ce := func(subject string) error {
		return health.CheckError{Subject: subject, Kind: "lost", Err: errors.New("lost " + subject)}
	}
	tests := []struct {
		name string
		err  error
	}{
		{"nested join", errors.Join(errors.Join(ce("a"), errors.New("plain one")), ce("b"), errors.New("plain two"))},
		{"wrapped join", fmt.Errorf("scan: %w", errors.Join(
			ce("a"), errors.New("plain one"), ce("b"), errors.New("plain two"),
		))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newFakeCheck(nil, "disks", health.CheckResult{Err: tt.err})
			f := newFixture(t, &fakeStore{}, checkOf(c))
			f.tick(t)

			keys := make([]string, 0, len(f.st.alerts))
			for _, a := range f.st.alerts {
				keys = append(keys, a.Key)
			}
			want := []string{"disks/a/lost", "disks/b/lost", "disks/run/error"}
			if !slices.Equal(keys, want) {
				t.Fatalf("alert keys = %v, want %v", keys, want)
			}
			tlg := f.tlg.texts()
			if countContaining(tlg, "plain one") != 1 || countContaining(tlg, "plain two") != 1 {
				t.Fatalf("a plain error was lost: %q", tlg)
			}
		})
	}
}

// expireAll drops every queued message as the daily 7-day expiry would.
func (f *fixture) expireAll(t *testing.T) {
	t.Helper()

	if dropped := f.r.ExpireQueue(f.now.Add(8*24*time.Hour), 7*24*time.Hour); len(dropped) == 0 {
		t.Fatal("nothing was queued to drop")
	}
}

const undeliveredLine = "Undelivered earlier:\nPROBLEM: a hot (since 05:00), no longer open\n\n"

// TestDroppedProblemReopens: a PROBLEM dropped undelivered while its
// condition is still open leaves the condition not open, so the next Bad
// reading raises it again (US2 acc. 3, SC-005).
func TestDroppedProblemReopens(t *testing.T) {
	c := newFakeCheck(nil, "disks", reading("disks/a", health.Bad, "a hot"))
	f := newFixture(t, &fakeStore{}, checkOf(c))
	f.tlg.setFail(true)
	f.sms.setFail(true)
	f.tick(t)
	f.expireAll(t)

	f.tlg.setFail(false)
	f.next(t)

	got := f.tlg.texts()
	if want := host + ": PROBLEM: a hot (since 05:00)"; !slices.Equal(got, []string{want}) {
		t.Fatalf("sent = %q, want the PROBLEM raised again: %q", got, want)
	}
}

// TestDroppedProblemNewStreakListed: a PROBLEM dropped undelivered after its
// condition resolved and started a new streak is listed ahead of the next
// message delivered to Telegram, once.
func TestDroppedProblemNewStreakListed(t *testing.T) {
	c := newFakeCheck(nil, "disks", reading("disks/a", health.Bad, "a hot"))
	chk := checkOf(c)
	chk.Threshold = 2 * Tick
	f := newFixture(t, &fakeStore{}, chk)
	f.tlg.setFail(true)
	f.sms.setFail(true)
	f.tick(t)
	f.next(t)
	f.next(t) // PROBLEM, queued.
	c.set(reading("disks/a", health.Good, "a ok"))
	f.next(t) // RESOLVED, queued.
	c.set(reading("disks/a", health.Bad, "a hot"))
	f.next(t) // A new streak, not open yet.
	if cond, ok := f.r.engine.Condition("disks/a"); !ok || cond.Open {
		t.Fatalf("condition = %+v, %v; want tracked and not open", cond, ok)
	}
	f.expireAll(t)

	f.tlg.setFail(false)
	f.r.Push(context.Background(), "test", "first", "first")
	f.r.Push(context.Background(), "test", "second", "second")

	want := []string{undeliveredLine + host + ": first", host + ": second"}
	if got := f.tlg.texts(); !slices.Equal(got, want) {
		t.Fatalf("sent = %q, want %q", got, want)
	}
	if cond, _ := f.r.engine.Condition("disks/a"); cond.Open {
		t.Fatal("listing the dropped PROBLEM opened the new streak")
	}
}

// TestDroppedResolvedListed: a PROBLEM dropped undelivered after its
// condition resolved (no longer tracked) is listed ahead of the next message
// delivered to Telegram; a failed delivery keeps it listed.
func TestDroppedResolvedListed(t *testing.T) {
	c := newFakeCheck(nil, "disks", reading("disks/a", health.Bad, "a hot"))
	f := newFixture(t, &fakeStore{}, checkOf(c))
	f.tlg.setFail(true)
	f.sms.setFail(true)
	f.tick(t)
	c.set(reading("disks/a", health.Good, "a ok"))
	f.next(t)
	if _, ok := f.r.engine.Condition("disks/a"); ok {
		t.Fatal("condition still tracked after it resolved")
	}
	f.expireAll(t)

	f.r.Push(context.Background(), "test", "lost", "lost") // Fails, queued.
	f.tlg.setFail(false)
	f.r.Push(context.Background(), "test", "first", "first")
	f.r.Push(context.Background(), "test", "second", "second")

	want := []string{undeliveredLine + host + ": first", host + ": second"}
	if got := f.tlg.texts(); !slices.Equal(got, want) {
		t.Fatalf("sent = %q, want %q", got, want)
	}
}

// TestDroppedStaleStreakListed: a PROBLEM of an earlier streak dropped while
// a later streak of its condition is open is listed, not reopened, so the
// later streak is not announced twice.
func TestDroppedStaleStreakListed(t *testing.T) {
	c := newFakeCheck(nil, "disks", reading("disks/a", health.Bad, "a hot"))
	f := newFixture(t, &fakeStore{}, checkOf(c))
	f.tlg.setFail(true)
	f.sms.setFail(true)
	f.tick(t) // PROBLEM1, queued.
	c.set(reading("disks/a", health.Good, "a ok"))
	f.next(t) // RESOLVED1, queued.
	c.set(reading("disks/a", health.Bad, "a hot"))
	f.next(t) // PROBLEM2, queued; the condition is open again.

	// Drop PROBLEM1 only.
	dropped := f.r.ExpireQueue(f.now, f.now.Sub(t0())-time.Second)
	if len(dropped) != 1 || dropped[0].Kind != string(notify.KindProblem) {
		t.Fatalf("dropped = %+v, want PROBLEM1 only", dropped)
	}
	if cond, _ := f.r.engine.Condition("disks/a"); !cond.Open {
		t.Fatal("the drop of an earlier streak's PROBLEM reopened the current streak")
	}

	f.tlg.setFail(false)
	f.next(t)
	f.next(t)

	got := f.tlg.texts()
	if n := countContaining(got, "no longer open"); n != 1 {
		t.Fatalf("sent = %q, want PROBLEM1 listed once", got)
	}
	if n := countContaining(got, host+": PROBLEM: a hot"); n != 1 {
		t.Fatalf("sent = %q, want PROBLEM2 sent once", got)
	}
}

// TestTrimmedProblemListed: a PROBLEM at the head of a queue trimmed at
// MaxQueue (here a stored queue without the plain text, whose condition is no
// longer tracked) is listed ahead of the next message delivered to Telegram.
func TestTrimmedProblemListed(t *testing.T) {
	queue := make([]notify.QueuedMessage, 0, notify.MaxQueue+1)
	queue = append(queue, notify.QueuedMessage{
		ID: 1, CreatedAt: t0(), Channel: notify.ChannelTelegram, Kind: string(notify.KindProblem),
		Check: "disks", Key: "disks/a", Text: host + ": PROBLEM: a hot (since 05:00)",
	})
	for i := range notify.MaxQueue {
		queue = append(queue, notify.QueuedMessage{
			ID: int64(
				i + 2,
			),
			CreatedAt: t0(),
			Channel:   notify.ChannelTelegram,
			Kind:      string(notify.KindEvent),
			Text:      "x",
		})
	}
	f := newFixture(t, &fakeStore{state: store.State{Queue: queue}})
	if err := f.r.Load(context.Background()); err != nil {
		t.Fatal(err)
	}

	f.r.Push(context.Background(), "test", "first", "first")

	if got := f.tlg.texts(); len(got) != 1 || got[0] != undeliveredLine+host+": first" {
		t.Fatalf("sent = %q, want the trimmed PROBLEM listed ahead of the first message", got)
	}
}

// TestStoreRecoveryNotice: once a reported state-write failure ends, one
// recovery notice goes out (US2 acc. 7, FR-032), by Telegram, and by email
// when the failure went by email; the failure's rate marks are given back,
// so a new failure is reported at once.
func TestStoreRecoveryNotice(t *testing.T) {
	const (
		failText    = host + ": CHECKER ERROR: health: cannot save state"
		recoverText = host + ": CHECKER OK: health: state can be saved again"
	)
	tests := []struct {
		name  string
		mail  bool
		email int // recovery notices by email
	}{
		{"telegram only", false, 0},
		{"failure went by email", true, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := &fakeStore{}
			mailer := &fakeMailer{}
			c := newFakeCheck(nil, "ups", reading("ups/main", health.Good, ""))
			f := newFixture(t, st, checkOf(c))
			if tt.mail {
				f = mailFixture(t, st, mailer, checkOf(c))
			}
			setFail := func(fail bool) {
				st.mu.Lock()
				st.fail = fail
				st.mu.Unlock()
			}

			f.tick(t)
			if n := countContaining(f.tlg.texts(), recoverText); n != 0 {
				t.Fatalf("recovery notice sent %d times without a failure", n)
			}

			setFail(true)
			f.next(t)
			f.next(t) // Still failing: the store returns nil while degraded.
			if n := countContaining(f.tlg.texts(), recoverText); n != 0 {
				t.Fatalf("telegram = %q, want no recovery notice while the store is degraded", f.tlg.texts())
			}
			if n := countContaining(mailer.bodies, recoverText); n != 0 {
				t.Fatalf("emails = %q, want no recovery notice while the store is degraded", mailer.bodies)
			}
			setFail(false)
			f.next(t)
			f.next(t)

			if n := countContaining(f.tlg.texts(), recoverText); n != 1 {
				t.Fatalf("telegram = %q, want one recovery notice", f.tlg.texts())
			}
			if n := countContaining(mailer.bodies, recoverText); n != tt.email {
				t.Fatalf("emails = %q, want %d recovery notices", mailer.bodies, tt.email)
			}
			for _, m := range f.r.marks.Export() {
				if m.Key == keySave {
					t.Fatalf("rate mark %+v kept after the recovery", m)
				}
			}

			setFail(true)
			f.next(t)
			if n := countContaining(f.tlg.texts(), failText); n != 2 {
				t.Fatalf("telegram = %q, want the new failure reported at once", f.tlg.texts())
			}
		})
	}
}

// pausing returns the check of c set to pause in quiet windows.
func pausing(c *fakeCheck) health.Check {
	chk := checkOf(c)
	chk.PauseInQuiet = true

	return chk
}

// TestHardwareRunsInMaintenance: while the maintenance lock is held, a check
// not set to pause (disks by default) runs and its PROBLEM goes out in the
// same tick (US2 acc. 4, SC-006); a check set to pause (containers by
// default) is not judged and runs once the window ends; queued messages are
// flushed and the tick checkpoints as a normal one (FR-026, R15).
func TestHardwareRunsInMaintenance(t *testing.T) {
	disks := newFakeCheck(nil, "disks", reading("disks/TEST0000000001/reallocated", health.Good, ""))
	ctrs := newFakeCheck(nil, "containers", reading("containers/app", health.Good, ""))
	f := newFixture(t, &fakeStore{}, checkOf(disks), pausing(ctrs))
	f.tick(t)

	f.maint.held = true
	f.tlg.setFail(true)
	f.r.Push(context.Background(), "ups", "queued in the window", "queued")
	f.tlg.setFail(false)
	disks.set(reading("disks/TEST0000000001/reallocated", health.Bad, "reallocated sectors rose to 8"))
	ctrs.set(reading("containers/app", health.Bad, "app is down"))
	f.next(t)

	if f.r.Snapshot().Quiet != health.ReasonMaintenance {
		t.Fatal("the tick was not quiet")
	}
	if disks.count() != 2 || ctrs.count() != 1 {
		t.Fatalf("runs: disks %d (want 2), containers %d (want 1, not in the window)", disks.count(), ctrs.count())
	}
	texts := f.tlg.texts()
	if countContaining(texts, "PROBLEM: reallocated sectors rose to 8") != 1 ||
		countContaining(texts, "app is down") != 0 {
		t.Fatalf("telegram = %q, want the drive PROBLEM and nothing about the container", texts)
	}
	if countContaining(texts, "queued in the window") != 1 {
		t.Fatalf("telegram = %q, want the queued message flushed", texts)
	}
	f.st.mu.Lock()
	conds := slices.Clone(f.st.state.Conditions)
	f.st.mu.Unlock()
	if len(conds) != 1 || conds[0].Key != "disks/TEST0000000001/reallocated" {
		t.Fatalf("checkpointed conditions = %+v, want the drive condition", conds)
	}

	f.maint.held = false
	f.next(t)
	if ctrs.count() != 2 || countContaining(f.tlg.texts(), "PROBLEM: app is down") != 1 {
		t.Fatalf("containers ran %d times, telegram = %q, want it judged after the window",
			ctrs.count(), f.tlg.texts())
	}
}

// TestBootGraceHardware: in the boot-grace window after a reboot, the
// temperatures check (not set to pause) is judged and alerts at once, while
// the network check (set to pause) does not run (FR-026, SC-006).
func TestBootGraceHardware(t *testing.T) {
	order := &orderLog{}
	netw := newFakeCheck(order, "network", reading(KeyNetwork, health.Bad, "router down"))
	temps := newFakeCheck(order, "temperatures", reading("temperatures/cpu/package0", health.Bad, "CPU at 99°C"))
	f := newFixture(t, &fakeStore{}, pausing(netw), checkOf(temps))
	f.up.up = time.Minute

	f.tick(t)

	if f.r.Snapshot().Quiet != health.ReasonBootGrace {
		t.Fatal("the tick was not quiet")
	}
	if got := order.get(); !slices.Equal(got, []string{"temperatures"}) {
		t.Fatalf("runs = %q, want only the temperatures check", got)
	}
	if got := f.tlg.texts(); countContaining(got, "PROBLEM: CPU at 99°C") != 1 || countContaining(got, "router") != 0 {
		t.Fatalf("telegram = %q, want the temperature PROBLEM only", got)
	}
	if f.st.checkpoints != 1 {
		t.Fatalf("checkpoints = %d, want 1", f.st.checkpoints)
	}
}

// TestNoStoreRecoveryNoticeForAnUnreportedFailure: a state-write failure held
// back by its rate limit was never told to the operator, so its end sends no
// recovery notice.
func TestNoStoreRecoveryNoticeForAnUnreportedFailure(t *testing.T) {
	st := &fakeStore{state: store.State{RateMarks: []health.RateMark{
		{Key: keySave, Channel: notify.ChannelTelegram, LastSentAt: t0().Add(-time.Hour)},
	}}}
	c := newFakeCheck(nil, "ups", reading("ups/main", health.Good, ""))
	f := newFixture(t, st, checkOf(c))
	f.tick(t)

	st.mu.Lock()
	st.fail = true
	st.mu.Unlock()
	f.next(t)
	st.mu.Lock()
	st.fail = false
	st.mu.Unlock()
	f.next(t)

	if got := f.tlg.texts(); len(got) != 0 {
		t.Fatalf("telegram = %q, want neither the rate-limited failure nor a recovery notice", got)
	}
}

// TestSnapshotPaused: the snapshot lists the configured checks set to pause
// in quiet windows, in configuration order, and since when the maintenance
// lock is held while it is (FR-026, FR-027, R15).
func TestSnapshotPaused(t *testing.T) {
	disks := newFakeCheck(nil, "disks", reading("disks/TEST0000000001/reallocated", health.Good, ""))
	ctrs := newFakeCheck(nil, "containers", reading("containers/app", health.Good, ""))
	ups := newFakeCheck(nil, "ups", reading("ups/status", health.Good, ""))
	f := newFixture(t, &fakeStore{}, pausing(ctrs), checkOf(disks), pausing(ups))

	if got := f.r.Snapshot().Paused; !slices.Equal(got, []string{"containers", "ups"}) {
		t.Fatalf("paused = %q, want containers and ups", got)
	}

	f.maint.held = true
	f.tick(t)
	f.next(t)
	if got := f.r.Snapshot().HeldSince; !got.Equal(t0()) || got.Location().String() != zone().String() {
		t.Fatalf("held since = %v, want %v in the host zone", got, t0())
	}

	f.maint.held = false
	f.next(t)
	if got := f.r.Snapshot().HeldSince; !got.IsZero() {
		t.Fatalf("held since = %v after the lock was freed, want zero", got)
	}

	if got := newFixture(t, &fakeStore{}, checkOf(disks)).r.Snapshot().Paused; len(got) != 0 {
		t.Fatalf("paused = %q, want none", got)
	}
}

// TestOfflineFromConnectivity: the outside is known down exactly while the
// network/connectivity condition is bad, whatever its cause text says; the
// 001 keys no longer make the runner offline (FR-034, R21).
func TestOfflineFromConnectivity(t *testing.T) {
	if KeyNetwork != "network/connectivity" {
		t.Fatalf("KeyNetwork = %q", KeyNetwork)
	}
	netw := newFakeCheck(nil, "network", reading(KeyNetwork, health.Bad, "No usable default route"))
	web := newFakeCheck(nil, "web", reading("http/web", health.Good, ""))
	webCheck := checkOf(web)
	webCheck.NeedsInternet = true
	f := newFixture(t, &fakeStore{}, checkOf(netw), webCheck)

	f.tick(t)
	if web.count() != 0 {
		t.Fatalf("web ran %d times while connectivity is bad, want 0", web.count())
	}

	netw.set(reading(KeyNetwork, health.Bad, "Internet is unreachable (router 192.0.2.1 answers)"))
	f.next(t)
	if web.count() != 0 {
		t.Fatalf("web ran %d times after the cause changed, want 0", web.count())
	}

	// A former key going bad does not make the runner offline.
	netw.set(health.CheckResult{Readings: []health.Reading{
		{Key: KeyNetwork, Verdict: health.Good, Text: "Network is reachable"},
		{Key: "network/router", Verdict: health.Bad, Text: "Router 192.0.2.1 is not answering"},
	}})
	f.next(t)
	if web.count() != 1 {
		t.Fatalf("web ran %d times once connectivity is good, want 1", web.count())
	}
}

// TestDependsOnFormerNetworkKeys: a depends_on naming a 001 network key
// (network/router or network/internet) follows network/connectivity, so a 001
// config keeps working.
func TestDependsOnFormerNetworkKeys(t *testing.T) {
	for _, dep := range []string{"network/router", "network/internet", KeyNetwork} {
		t.Run(dep, func(t *testing.T) {
			netw := newFakeCheck(nil, "network", reading(KeyNetwork, health.Bad, "Router 192.0.2.1 is not answering"))
			b := newFakeCheck(nil, "b", reading("http/b", health.Good, ""))
			bCheck := checkOf(b)
			bCheck.DependsOn = dep
			f := newFixture(t, &fakeStore{}, checkOf(netw), bCheck)

			f.tick(t)
			if b.count() != 0 {
				t.Fatalf("b ran %d times while connectivity is bad, want 0", b.count())
			}

			netw.set(reading(KeyNetwork, health.Good, "Network is reachable"))
			f.next(t)
			if b.count() != 1 {
				t.Fatalf("b ran %d times once connectivity is good, want 1", b.count())
			}
		})
	}
}

// TestNetworkUnknownSkipsInternetChecks: while the last network run was
// unreadable (an error, no reading, or only an unknown one), checks that need
// the internet are not run, yet the outside is not known down, so Telegram is
// still attempted (FR-036, R21).
func TestNetworkUnknownSkipsInternetChecks(t *testing.T) {
	cases := map[string]health.CheckResult{
		"error":      {Err: errors.New("open /proc/net/route: permission denied"), Forget: []string{"network/router"}},
		"no reading": {Forget: []string{"network/router"}},
		"unknown":    {Readings: []health.Reading{{Key: KeyNetwork, Verdict: health.Unknown, Text: "cannot ping"}}},
	}
	for name, unreadable := range cases {
		t.Run(name, func(t *testing.T) {
			netw := newFakeCheck(nil, "network", unreadable)
			netCheck := checkOf(netw)
			netCheck.Interval = 5 * time.Minute
			web := newFakeCheck(nil, "web", reading("http/web", health.Good, ""))
			webCheck := checkOf(web)
			webCheck.NeedsInternet = true
			disk := newFakeCheck(nil, "disk", reading("disk/sda", health.Bad, "disk failing"))
			f := newFixture(t, &fakeStore{}, netCheck, webCheck, checkOf(disk))

			f.tick(t)
			f.next(t) // network not due: still unknown from its last run.

			if web.count() != 0 {
				t.Fatalf("web ran %d times while the network is unknown, want 0", web.count())
			}
			if disk.count() != 2 {
				t.Fatalf("disk ran %d times, want 2", disk.count())
			}
			if got := countContaining(f.tlg.texts(), "PROBLEM: disk failing"); got != 1 {
				t.Fatalf("telegram = %q, want the disk PROBLEM attempted on Telegram", f.tlg.texts())
			}

			netw.set(reading(KeyNetwork, health.Good, "Network is reachable"))
			f.now = f.now.Add(5 * time.Minute)
			f.tick(t)

			if web.count() != 1 {
				t.Fatalf("web ran %d times once the network is readable, want 1", web.count())
			}
		})
	}
}

// TestNetworkThresholdDefault: with the network threshold left out of the
// config it is one interval, so a single lost round heals without a message
// while an outage confirmed on the next cycle is one PROBLEM by SMS (FR-035).
func TestNetworkThresholdDefault(t *testing.T) {
	cfg, errs := config.Parse([]byte("network:\n  internet_targets: [\"203.0.113.10\"]\n"), "health.yaml")
	if len(errs) != 0 || cfg.Network == nil {
		t.Fatalf("parse: %v", errs)
	}
	n := cfg.Network
	if n.Threshold.Duration() != n.Interval.Duration() || n.Interval.Duration() != 5*time.Minute {
		t.Fatalf("threshold = %s, interval = %s, want both 5m", n.Threshold.Duration(), n.Interval.Duration())
	}

	netw := newFakeCheck(nil, "network", reading(KeyNetwork, health.Bad, "Router 192.0.2.1 is not answering"))
	netCheck := checkOf(netw)
	netCheck.Interval = n.Interval.Duration()
	netCheck.Threshold = n.Threshold.Duration()
	netCheck.Mode = health.ModeSMS
	f := newFixture(t, &fakeStore{}, netCheck)

	// One lost round heals by itself: no message.
	f.tick(t)
	netw.set(reading(KeyNetwork, health.Good, "Network is reachable"))
	f.now = f.now.Add(5 * time.Minute)
	f.tick(t)
	if got := append(f.sms.texts(), f.tlg.texts()...); len(got) != 0 {
		t.Fatalf("sent %q for a single lost round, want nothing", got)
	}

	// Bad on two consecutive cycles: one PROBLEM by SMS.
	netw.set(reading(KeyNetwork, health.Bad, "Router 192.0.2.1 is not answering"))
	f.now = f.now.Add(5 * time.Minute)
	f.tick(t)
	f.now = f.now.Add(5 * time.Minute)
	f.tick(t)
	if got := countContaining(f.sms.texts(), "PROBLEM: Router 192.0.2.1 is not answering"); got != 1 {
		t.Fatalf("sms = %q, want one PROBLEM", f.sms.texts())
	}
}

// TestNetworkUnknownAtBootWhilePaused: at boot the network check pauses in the
// boot quiet window, so it has never been read; a check that needs the
// internet and does not pause waits for the first readable network run
// (FR-036).
func TestNetworkUnknownAtBootWhilePaused(t *testing.T) {
	netw := newFakeCheck(nil, "network", reading(KeyNetwork, health.Good, "Network is reachable"))
	web := newFakeCheck(nil, "web", reading("http/web", health.Good, ""))
	webCheck := checkOf(web)
	webCheck.NeedsInternet = true
	f := newFixture(t, &fakeStore{}, pausing(netw), webCheck)
	f.up.up = time.Minute // inside the boot grace: a quiet tick

	f.tick(t)
	if netw.count() != 0 || web.count() != 0 {
		t.Fatalf("runs: network %d, web %d; want neither before the network was read", netw.count(), web.count())
	}

	f.up.up = time.Hour // the boot grace is over
	f.next(t)
	if netw.count() != 1 || web.count() != 1 {
		t.Fatalf("runs: network %d, web %d; want both once the network is read", netw.count(), web.count())
	}
}

// TestNetworkStateKeptWhilePaused: while the network check is paused by a
// quiet window, its last run decides whether checks that need the internet
// run: unreadable keeps them waiting, readable lets them run (FR-036).
func TestNetworkStateKeptWhilePaused(t *testing.T) {
	cases := []struct {
		name string
		last health.CheckResult
		want int
	}{
		{"unreadable", health.CheckResult{Err: errors.New("open /proc/net/route: permission denied")}, 0},
		{"reachable", reading(KeyNetwork, health.Good, "Network is reachable"), 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			netw := newFakeCheck(nil, "network", tc.last)
			web := newFakeCheck(nil, "web", reading("http/web", health.Good, ""))
			webCheck := checkOf(web)
			webCheck.NeedsInternet = true
			f := newFixture(t, &fakeStore{}, pausing(netw), webCheck)

			f.tick(t)
			f.maint.held = true
			f.now = f.now.Add(webCheck.Interval)
			f.tick(t)

			if netw.count() != 1 {
				t.Fatalf("network ran %d times, want 1 (paused in the window)", netw.count())
			}
			if web.count() != tc.want {
				t.Fatalf("web ran %d times, want %d", web.count(), tc.want)
			}
		})
	}
}
