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
}

func (f *fakeSender) Send(_ context.Context, text string) error {
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

func (f *fakeStore) Checkpoint(_ context.Context, dirty store.State, alerts []store.Alert,
	samples []store.SampleRow,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.checkpoints++
	f.pendingAlerts = append(f.pendingAlerts, alerts...)
	f.pendingSamples = append(f.pendingSamples, samples...)
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
			f := newFixture(t, &fakeStore{}, checkOf(c))
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
	netw := newFakeCheck(order, "network", reading(KeyInternet, health.Bad, "internet unreachable"))
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
	wantSMS := []string{host + ": PROBLEM: internet unreachable (since 05:00)"}
	if got := f.sms.texts(); !slices.Equal(got, wantSMS) {
		t.Fatalf("sms = %q, want %q", got, wantSMS)
	}
	if got := f.tlg.texts(); len(got) != 0 {
		t.Fatalf("telegram sent %q while offline, want everything queued", got)
	}

	netw.set(reading(KeyInternet, health.Good, ""))
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

func TestRouterDownIsOfflineUntilGood(t *testing.T) {
	netw := newFakeCheck(nil, "network", reading(KeyRouter, health.Bad, "gateway down"))
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

	netw.set(reading(KeyRouter, health.Good, ""))
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
	if a := st.alerts[1]; a.Key != "temps/s1" || a.Check != "temps" || a.Text != "sensor gone" {
		t.Fatalf("cleared record = %+v", a)
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
