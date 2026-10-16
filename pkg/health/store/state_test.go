package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/Toshik1978/tgshd/pkg/health"
	"github.com/Toshik1978/tgshd/pkg/health/notify"
)

func ts(sec int64) time.Time { return time.Unix(sec, 0).UTC() }

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	return s
}

func sampleState() State {
	return State{
		Conditions: []health.Condition{
			{
				Key: "disks/TEST0000000001/wear", Check: "disks", StreakStart: ts(1000),
				BadFor: 90 * time.Second, LastReadingAt: ts(1090), LastText: "wear high",
				Open: true, OpenedAt: ts(1090),
			},
			{Key: "http/example", Check: "http"},
		},
		Baselines: []Baseline{{
			Serial: "TEST0000000001", Model: "Example NVMe SSD", Bus: "nvme",
			Data: json.RawMessage(`{"media_errors":2}`), FirstSeenAt: ts(500), LastSeenAt: ts(1090),
		}},
		KV:        map[string]string{"kernel.cursor": "abc", "first_run": "done"},
		RateMarks: []health.RateMark{{Key: "checker/disks", Channel: "tlg", LastSentAt: ts(900)}},
		Queue:     []notify.QueuedMessage{{ID: 7, CreatedAt: ts(1100), Channel: "sms", Text: "hello", Attempts: 2}},
	}
}

func count(t *testing.T, s *Store, table string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
		t.Fatal(err)
	}

	return n
}

func TestCheckpointLoadRoundTrip(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	want := sampleState()
	alerts := []Alert{{
		At: ts(1090), Kind: "problem", Check: "disks",
		Key: "disks/TEST0000000001/wear", Text: "wear high",
	}}
	samples := []SampleRow{{At: ts(1090), Metric: "ups.charge", Value: 99.5}}

	if err := s.Checkpoint(ctx, want, alerts, samples); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	got, err := s.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(got.Conditions, want.Conditions) {
		t.Errorf("conditions = %+v, want %+v", got.Conditions, want.Conditions)
	}
	if !reflect.DeepEqual(got.Baselines, want.Baselines) {
		t.Errorf("baselines = %+v, want %+v", got.Baselines, want.Baselines)
	}
	if !reflect.DeepEqual(got.KV, want.KV) {
		t.Errorf("kv = %+v, want %+v", got.KV, want.KV)
	}
	if !reflect.DeepEqual(got.RateMarks, want.RateMarks) {
		t.Errorf("rate marks = %+v, want %+v", got.RateMarks, want.RateMarks)
	}
	if !reflect.DeepEqual(got.Queue, want.Queue) {
		t.Errorf("queue = %+v, want %+v", got.Queue, want.Queue)
	}
	if n := count(t, s, "alerts"); n != 1 {
		t.Errorf("alerts = %d, want 1", n)
	}
	if n := count(t, s, "samples"); n != 1 {
		t.Errorf("samples = %d, want 1", n)
	}
}

func TestLoadEmpty(t *testing.T) {
	got, err := openTemp(t).Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got.Conditions)+len(got.Baselines)+len(got.KV)+len(got.RateMarks)+len(got.Queue) != 0 {
		t.Errorf("expected empty state, got %+v", got)
	}
}

func TestCheckpointReplacesSnapshots(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	if err := s.Checkpoint(ctx, sampleState(), nil, nil); err != nil {
		t.Fatal(err)
	}
	// The engine forgot every condition, the queue drained, rate marks expired.
	next := State{KV: map[string]string{"first_run": "done"}}
	if err := s.Checkpoint(ctx, next, nil, nil); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Conditions) != 0 {
		t.Errorf("resolved conditions reappeared: %+v", got.Conditions)
	}
	if len(got.Queue) != 0 || len(got.RateMarks) != 0 {
		t.Errorf("queue/rate marks not replaced: %+v %+v", got.Queue, got.RateMarks)
	}
	if len(got.Baselines) != 1 || got.KV["kernel.cursor"] != "abc" {
		t.Errorf("baselines and kv must persist across partial states: %+v", got)
	}
}

func TestCheckpointUpdatesBaselineAndKV(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	st := sampleState()
	if err := s.Checkpoint(ctx, st, nil, nil); err != nil {
		t.Fatal(err)
	}
	st.Baselines[0].Retired = true
	st.Baselines[0].LastSeenAt = ts(2000)
	st.KV["kernel.cursor"] = "def"
	if err := s.Checkpoint(ctx, st, nil, nil); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Baselines) != 1 || !got.Baselines[0].Retired || !got.Baselines[0].LastSeenAt.Equal(ts(2000)) {
		t.Errorf("baseline not updated: %+v", got.Baselines)
	}
	if got.KV["kernel.cursor"] != "def" {
		t.Errorf("kv not updated: %+v", got.KV)
	}
}

func TestAlertsAndSamplesAppend(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	a := Alert{At: ts(1), Kind: "event", Check: "kernel", Key: "k", Text: "t"}
	for range 2 {
		if err := s.Checkpoint(ctx, State{}, []Alert{a}, []SampleRow{{At: ts(1), Metric: "m", Value: 1}}); err != nil {
			t.Fatal(err)
		}
	}
	if n := count(t, s, "alerts"); n != 2 {
		t.Errorf("alerts = %d, want 2", n)
	}
	if n := count(t, s, "samples"); n != 2 {
		t.Errorf("samples = %d, want 2", n)
	}
}

// breakWrites makes every kv insert fail; fixWrites undoes it.
func breakWrites(t *testing.T, s *Store) {
	t.Helper()
	_, err := s.db.ExecContext(context.Background(),
		`CREATE TRIGGER fail_kv BEFORE INSERT ON kv BEGIN SELECT RAISE(ABORT, 'injected'); END`)
	if err != nil {
		t.Fatal(err)
	}
}

func fixWrites(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(), `DROP TRIGGER fail_kv`); err != nil {
		t.Fatal(err)
	}
}

func TestDegradedModeFlushesOnRecovery(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	breakWrites(t, s)

	err := s.Checkpoint(ctx, sampleState(),
		[]Alert{{At: ts(1), Kind: "problem", Check: "c", Key: "k1", Text: "a"}},
		[]SampleRow{{At: ts(1), Metric: "m", Value: 1}})
	if !errors.Is(err, ErrDegraded) {
		t.Fatalf("first failure = %v, want ErrDegraded", err)
	}
	if n := count(t, s, "alerts"); n != 0 {
		t.Fatalf("failed transaction must roll back, alerts = %d", n)
	}

	// Still failing: reported once only; data keeps accumulating.
	err = s.Checkpoint(ctx, State{KV: map[string]string{"first_run": "done"}},
		[]Alert{{At: ts(2), Kind: "resolved", Check: "c", Key: "k1", Text: "b"}},
		[]SampleRow{{At: ts(2), Metric: "m", Value: 2}})
	if err != nil {
		t.Fatalf("repeat failure must not re-report, got %v", err)
	}

	fixWrites(t, s)
	if err = s.Checkpoint(ctx, State{KV: map[string]string{"x": "y"}}, nil, nil); err != nil {
		t.Fatalf("recovery: %v", err)
	}
	if n := count(t, s, "alerts"); n != 2 {
		t.Errorf("alerts after flush = %d, want 2", n)
	}
	if n := count(t, s, "samples"); n != 2 {
		t.Errorf("samples after flush = %d, want 2", n)
	}
	got, err := s.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Baselines) != 1 || got.Baselines[0].Serial != "TEST0000000001" || got.KV["kernel.cursor"] != "abc" {
		t.Errorf("state from the first failed call lost: %+v", got)
	}
	if len(got.Conditions) != 0 || len(got.Queue) != 0 {
		t.Errorf("latest snapshot must win: %+v", got)
	}
	if s.Degraded() {
		t.Error("Degraded() = true after recovery")
	}
	if got.KV["x"] != "y" || got.KV["first_run"] != "done" {
		t.Errorf("kv = %+v", got.KV)
	}

	// A new failure after recovery is reported again.
	breakWrites(t, s)
	err = s.Checkpoint(ctx, State{KV: map[string]string{"z": "1"}}, nil, nil)
	if !errors.Is(err, ErrDegraded) {
		t.Errorf("new failure = %v, want ErrDegraded", err)
	}
}

func TestLoadMissingTableFails(t *testing.T) {
	s := openTemp(t)
	if _, err := s.db.ExecContext(context.Background(), `DROP TABLE queue`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(context.Background()); err == nil {
		t.Error("expected error")
	}
}

func TestInvalidInputIsRejectedNotBuffered(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	bad := []Alert{{At: ts(1), Kind: "bogus", Check: "c", Key: "k", Text: "t"}}
	err := s.Checkpoint(ctx, State{}, bad, nil)
	if err == nil || errors.Is(err, ErrDegraded) {
		t.Fatalf("bad kind = %v, want plain error", err)
	}
	badQ := State{Queue: []notify.QueuedMessage{{ID: 1, CreatedAt: ts(1), Channel: "fax"}}}
	if err = s.Checkpoint(ctx, badQ, nil, nil); err == nil || errors.Is(err, ErrDegraded) {
		t.Fatalf("bad channel = %v, want plain error", err)
	}
	good := []Alert{{At: ts(2), Kind: KindEvent, Check: "c", Key: "k", Text: "t"}}
	if err = s.Checkpoint(ctx, State{}, good, nil); err != nil {
		t.Fatalf("later good checkpoint: %v", err)
	}
	if n := count(t, s, "alerts"); n != 1 {
		t.Errorf("alerts = %d, want 1", n)
	}
}

func TestCancelledContextIsNotDegraded(t *testing.T) {
	s := openTemp(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := s.Checkpoint(ctx, sampleState(), nil, nil)
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrDegraded) {
		t.Fatalf("err = %v, want context.Canceled only", err)
	}
	if s.Degraded() {
		t.Error("cancelled context must not degrade the store")
	}
}

func TestPendingSnapshotIsCloned(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	breakWrites(t, s)
	st := sampleState()
	_ = s.Checkpoint(ctx, st, nil, nil)
	st.Conditions[0].Key = "mutated"
	st.Queue[0].Text = "mutated"
	fixWrites(t, s)
	if err := s.Checkpoint(ctx, State{}, nil, nil); err != nil {
		t.Fatal(err)
	}
	got, err := s.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Conditions) != 0 {
		t.Fatalf("latest empty snapshot must win: %+v", got.Conditions)
	}
}

func TestLoadCorruptRateMarks(t *testing.T) {
	s := openTemp(t)
	_, err := s.db.ExecContext(context.Background(),
		`INSERT INTO kv (k, v) VALUES ('rate_marks', 'not json')`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got.RateMarks) != 0 {
		t.Errorf("rate marks = %+v, want none", got.RateMarks)
	}
}
