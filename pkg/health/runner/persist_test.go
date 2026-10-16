package runner

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/Toshik1978/tgshd/pkg/health"
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
