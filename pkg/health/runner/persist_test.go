package runner

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Toshik1978/tgshd/pkg/health"
	"github.com/Toshik1978/tgshd/pkg/health/notify"
	"github.com/Toshik1978/tgshd/pkg/health/store"
)

// fakePersister logs its calls into order and keeps a value under kv key.
type fakePersister struct {
	key        string
	value      string
	order      *orderLog
	restored   []store.State
	restoreErr error
	saveErr    error
}

func (p *fakePersister) Restore(st store.State) error {
	p.order.add("restore " + p.key)
	st.KV = maps.Clone(st.KV) // The store's map is reused by later checkpoints.
	p.restored = append(p.restored, st)
	if p.restoreErr != nil {
		return p.restoreErr
	}
	p.value = st.KV[p.key]

	return nil
}

func (p *fakePersister) Save(st *store.State) error {
	p.order.add("save " + p.key)
	if p.saveErr != nil {
		return p.saveErr
	}
	st.KV[p.key] = p.value
	st.Baselines = append(st.Baselines, store.Baseline{Serial: "TEST0000000001", Retired: true})

	return nil
}

func persistFixture(t *testing.T, st *fakeStore, ps ...Persister) (*fixture, *orderLog, *fakeCheck) {
	t.Helper()

	order := &orderLog{}
	for _, p := range ps {
		if fp, ok := p.(*fakePersister); ok {
			fp.order = order
		}
	}
	c := newFakeCheck(order, "kernel", health.CheckResult{})
	f := newFixture(t, st, checkOf(c))
	f.r.opts.Persisters = ps

	return f, order, c
}

func TestPersistersRestoreBeforeTheFirstRunAndSaveEachCheckpoint(t *testing.T) {
	st := &fakeStore{state: store.State{KV: map[string]string{"kernel.state": "stored"}}}
	p := &fakePersister{key: "kernel.state"}
	f, order, _ := persistFixture(t, st, p)

	f.tick(t)
	p.value = "advanced"
	f.next(t)

	want := []string{"restore kernel.state", "kernel", "save kernel.state", "kernel", "save kernel.state"}
	if got := order.get(); !slices.Equal(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	if len(p.restored) != 1 || p.restored[0].KV["kernel.state"] != "stored" {
		t.Fatalf("restored = %+v, want the stored state once", p.restored)
	}
	if got := st.state.KV["kernel.state"]; got != "advanced" {
		t.Fatalf("checkpointed kv = %q, want the saved value", got)
	}
	if len(st.state.Baselines) != 1 || !st.state.Baselines[0].Retired {
		t.Fatalf("checkpointed baselines = %+v, want the saved baseline", st.state.Baselines)
	}
}

func TestPersistersRestoreAtStartLoad(t *testing.T) {
	st := &fakeStore{state: store.State{KV: map[string]string{"temps.seen": "{}"}}}
	p := &fakePersister{key: "temps.seen"}
	f, order, _ := persistFixture(t, st, p)

	if err := f.r.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := order.get(); !slices.Equal(got, []string{"restore temps.seen"}) {
		t.Fatalf("calls after Load = %v, want the restore only", got)
	}
	f.tick(t)
	if len(p.restored) != 1 {
		t.Fatalf("restored %d times, want once", len(p.restored))
	}
}

func TestPersistersRestoreEmptyStateWithoutStore(t *testing.T) {
	p := &fakePersister{key: "kernel.state"}
	order := &orderLog{}
	p.order = order
	c := newFakeCheck(order, "kernel", health.CheckResult{})
	r := New(Options{Checks: []health.Check{checkOf(c)}, Persisters: []Persister{p}, Now: t0})

	if err := r.Do(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := r.Do(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Without a store nothing is checkpointed, so nothing is saved.
	if got := order.get(); !slices.Equal(got, []string{"restore kernel.state", "kernel"}) {
		t.Fatalf("calls = %v", got)
	}
	if len(p.restored) != 1 || p.restored[0].KV != nil || p.restored[0].Baselines != nil {
		t.Fatalf("restored = %+v, want one empty state", p.restored)
	}
}

func TestPersistersWaitForASuccessfulLoad(t *testing.T) {
	st := &fakeStore{
		state:   store.State{KV: map[string]string{"kernel.state": "stored"}},
		loadErr: errors.New("database locked"),
	}
	p := &fakePersister{key: "kernel.state"}
	f, order, _ := persistFixture(t, st, p)

	f.tick(t)
	if got := order.get(); !slices.Equal(got, []string{"kernel"}) {
		t.Fatalf("calls while unloaded = %v, want no restore and no save", got)
	}

	st.mu.Lock()
	st.loadErr = nil
	st.mu.Unlock()
	f.next(t)

	want := []string{"kernel", "restore kernel.state", "kernel", "save kernel.state"}
	if got := order.get(); !slices.Equal(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
	if p.value != "stored" || st.state.KV["kernel.state"] != "stored" {
		t.Fatalf("value = %q, stored %q", p.value, st.state.KV["kernel.state"])
	}
}

func TestPersisterErrorsAreCheckerErrors(t *testing.T) {
	st := &fakeStore{}
	bad := &fakePersister{key: "kernel.state", restoreErr: errors.New("decode kernel state: bad json")}
	good := &fakePersister{key: "temps.seen", value: "{}", saveErr: errors.New("encode seen sensors: bad")}
	worse := &fakePersister{key: "other.state", restoreErr: errors.New("decode other state: bad json")}
	f, order, _ := persistFixture(t, st, bad, good, worse)

	f.tick(t)

	want := []string{
		"restore kernel.state", "restore temps.seen", "restore other.state", "kernel",
		"save kernel.state", "save temps.seen", "save other.state",
	}
	if got := order.get(); !slices.Equal(got, want) {
		t.Fatalf("calls = %v, want every persister restored and saved despite errors: %v", got, want)
	}
	texts := f.tlg.texts()
	restored := "CHECKER ERROR: health: cannot restore state: decode kernel state: bad json; " +
		"decode other state: bad json"
	if countContaining(texts, restored) != 1 ||
		countContaining(texts, "CHECKER ERROR: health: cannot save state: encode seen sensors: bad") != 1 {
		t.Fatalf("telegram = %q, want the restore failures in one message and the save failure", texts)
	}
	if _, ok := st.state.KV["kernel.state"]; !ok {
		t.Fatalf("checkpointed kv = %v, want the other persister's state still saved", st.state.KV)
	}
	if st.checkpoints != 1 {
		t.Fatalf("checkpoints = %d, want the tick still checkpointed", st.checkpoints)
	}
}

type fakeKVState struct {
	data       string
	restoreErr error
	exportErr  error
}

func (s *fakeKVState) Export() (string, error) { return s.data, s.exportErr }

func (s *fakeKVState) Restore(data string) error {
	if s.restoreErr != nil {
		return s.restoreErr
	}
	s.data = data

	return nil
}

func TestKVPersister(t *testing.T) {
	t.Parallel()

	s := &fakeKVState{data: "memory"}
	p := KVPersister{Key: "kernel.state", State: s}

	if err := p.Restore(store.State{KV: map[string]string{"kernel.state": "stored", "other": "x"}}); err != nil {
		t.Fatal(err)
	}
	if s.data != "stored" {
		t.Fatalf("restored = %q", s.data)
	}
	if err := p.Restore(store.State{}); err != nil || s.data != "" {
		t.Fatalf("absent key: data %q, err %v; want the empty value handed over", s.data, err)
	}

	s.data = "saved"
	st := store.State{}
	if err := p.Save(&st); err != nil || st.KV["kernel.state"] != "saved" {
		t.Fatalf("save into a nil kv: %v, %v", st.KV, err)
	}
	st.KV["other"] = "kept"
	if err := p.Save(&st); err != nil || len(st.KV) != 2 {
		t.Fatalf("save keeps other keys: %v, %v", st.KV, err)
	}

	s.restoreErr = errors.New("bad json")
	if err := p.Restore(store.State{}); err == nil || !strings.Contains(err.Error(), "kernel.state") ||
		!errors.Is(err, s.restoreErr) {
		t.Fatalf("restore error = %v, want it wrapped with the key", err)
	}
	s.exportErr = errors.New("cannot encode")
	st = store.State{KV: map[string]string{"kernel.state": "old"}}
	if err := p.Save(&st); err == nil || !errors.Is(err, s.exportErr) || st.KV["kernel.state"] != "old" {
		t.Fatalf("save error = %v, kv %v; want it wrapped and the old value left alone", err, st.KV)
	}
}

func TestCheckpointCarriesQueuedKindCheckAndKey(t *testing.T) {
	c := newFakeCheck(nil, "ups", reading("ups/main", health.Bad, "on battery"))
	st := &fakeStore{}
	f := newFixture(t, st, checkOf(c))
	f.tick(t)

	f.tlg.setFail(true)
	c.set(reading("ups/main", health.Bad, "on battery"))
	f.next(t)
	c.set(reading("ups/main", health.Good, "on mains"))
	f.next(t)
	f.r.Push(context.Background(), "summary", "Weekly summary", "Weekly summary sent")
	f.next(t)

	got := make([]string, 0, len(st.state.Queue))
	for _, m := range st.state.Queue {
		got = append(got, m.Kind+"|"+m.Check+"|"+m.Key)
	}
	want := []string{"resolved|ups|ups/main", "event|summary|summary"}
	if !slices.Equal(got, want) {
		t.Fatalf("stored queue kind|check|key = %q, want %q", got, want)
	}
}

// mailFixture is newFixture with a mailer and a 2-minute telegram_failing_after.
func mailFixture(t *testing.T, st *fakeStore, mailer *fakeMailer, checks ...health.Check) *fixture {
	t.Helper()

	f := newFixture(t, st, checks...)
	f.r = New(Options{
		Checks: checks, Store: st, Host: host, Telegram: f.tlg, SMS: f.sms, Mailer: mailer,
		TelegramFailingAfter: 2 * time.Minute, Maintenance: f.maint,
		Now: func() time.Time { return f.now }, Location: zone(),
	})

	return f
}

// TestRestartSendsTheTelegramFailingEmailOnce: the Telegram failure and its
// notice are kept in kv notify.channels, so a restart during the same
// outage neither starts it anew nor emails again (FR-029).
func TestRestartSendsTheTelegramFailingEmailOnce(t *testing.T) {
	st := &fakeStore{}
	mailer := &fakeMailer{}
	f1 := mailFixture(t, st, mailer)
	f1.tlg.setFail(true)
	f1.r.Push(context.Background(), "ups", "test", "test") // Failing since t0.
	f1.tick(t)
	for range 4 {
		f1.next(t)
	}
	if got := mailer.count(); got != 1 {
		t.Fatalf("emails = %d before the restart, want the one notice", got)
	}

	f2 := mailFixture(t, st, mailer)
	f2.tlg.setFail(true)
	f2.now = f1.now.Add(Tick)
	f2.r.Push(context.Background(), "ups", "test", "test")
	f2.tick(t)
	for range 10 {
		f2.next(t)
	}
	if got := mailer.count(); got != 1 {
		t.Fatalf("emails = %q, want no second notice after the restart", mailer.bodies)
	}
	if h := f2.r.notifier.TelegramHealth(); !h.FailingSince.Equal(t0()) {
		t.Fatalf("telegram = %+v, want still failing since %v", h, t0())
	}
}

// TestRestartKeepsQueuedProblemsSMSSent: a queued PROBLEM sent by SMS
// through the fallback is not sent by SMS again after a restart (its key is
// kept in kv notify.sms_problems), and its RESOLVED still goes by SMS
// (FR-030).
func TestRestartKeepsQueuedProblemsSMSSent(t *testing.T) {
	st := &fakeStore{}
	ups := newFakeCheck(nil, "ups", reading("ups/main", health.Bad, "on battery"))
	f1 := newFixture(t, st, checkOf(ups))
	f1.tlg.setFail(true)
	f1.tick(t)
	problem := host + ": PROBLEM: on battery (since 05:00)"
	if got := f1.sms.texts(); !slices.Equal(got, []string{problem}) {
		t.Fatalf("sms before the restart = %q, want the PROBLEM once", got)
	}

	f2 := newFixture(t, st, checkOf(ups))
	f2.tlg.setFail(true)
	f2.now = f1.now.Add(Tick)
	f2.tick(t)
	f2.next(t)
	if got := f2.sms.texts(); len(got) != 0 {
		t.Fatalf("sms after the restart = %q, want the restored PROBLEM not sent again", got)
	}

	ups.set(reading("ups/main", health.Good, "on mains"))
	f2.next(t)
	if got := f2.sms.texts(); !slices.Equal(got, []string{host + ": RESOLVED: on mains"}) {
		t.Fatalf("sms = %q, want the RESOLVED by SMS", got)
	}
}

// TestRestartKeepsTheUndeliveredList: listed PROBLEMs and the Folded count
// are kept in kv notify.undelivered until a delivered message has shown them
// (FR-031).
func TestRestartKeepsTheUndeliveredList(t *testing.T) {
	st := &fakeStore{}
	kern := newFakeCheck(nil, "kernel", health.CheckResult{})
	f1 := newFixture(t, st, checkOf(kern))
	for i := range notify.MaxUndelivered + 2 {
		f1.r.notifier.ListUndelivered(notify.QueuedMessage{
			Kind:  string(notify.KindProblem),
			Key:   fmt.Sprintf("command/job%02d", i),
			Plain: fmt.Sprintf("job%02d failed", i),
		})
	}
	f1.tick(t)

	f2 := newFixture(t, st, checkOf(kern))
	f2.now = f1.now.Add(Tick)
	f2.tick(t)
	f2.r.Push(context.Background(), "test", "next", "next")

	texts := f2.tlg.texts()
	if len(texts) != 1 || strings.Count(texts[0], ", no longer open\n") != notify.MaxUndelivered ||
		!strings.Contains(texts[0], "PROBLEM: job00 failed, no longer open\n") ||
		!strings.Contains(texts[0], "… and 2 more undelivered PROBLEMs\n") {
		t.Fatalf("telegram after the restart = %q, want the listed PROBLEMs and the counted line", texts)
	}

	f2.next(t)
	u, err := notify.DecodeUndelivered(st.state.KV[notify.KVUndelivered])
	if err != nil || !u.Empty() {
		t.Fatalf("stored undelivered = %+v, %v; want empty once shown", u, err)
	}
}

// TestCorruptDeliveryStateIsACheckerError: a stored delivery state that does
// not decode is a checker error; the runner carries on.
func TestCorruptDeliveryStateIsACheckerError(t *testing.T) {
	st := &fakeStore{state: store.State{KV: map[string]string{notify.KVChannels: "{"}}}
	f := newFixture(t, st, checkOf(newFakeCheck(nil, "kernel", health.CheckResult{})))
	f.tick(t)

	if got := countContaining(f.tlg.texts(), "cannot restore state: restore "+notify.KVChannels); got != 1 {
		t.Fatalf("telegram = %q, want the restore failure", f.tlg.texts())
	}
	if _, ok := st.state.KV[notify.KVChannels]; !ok || st.state.KV[notify.KVChannels] == "{" {
		t.Fatalf("stored channels = %q, want rewritten from memory", st.state.KV[notify.KVChannels])
	}
}
