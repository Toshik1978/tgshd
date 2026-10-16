package runner

import (
	"errors"
	"testing"
	"time"

	"github.com/Toshik1978/tgshd/pkg/health"
	"github.com/Toshik1978/tgshd/pkg/health/store"
)

const stuckText = "CHECKER ERROR: health: maintenance lock held since 2026-01-02 05:00 (more than 6h)"

// holdFor ticks every step until d has passed since the fixture's now.
func (f *fixture) holdFor(t *testing.T, d, step time.Duration) {
	t.Helper()

	end := f.now.Add(d)
	for f.now.Before(end) {
		f.now = f.now.Add(step)
		f.tick(t)
	}
}

// TestStuckMaintenanceLock: a maintenance lock held longer than
// general.maintenance_max_hold raises one checker error naming since when it
// is held; the quiet window itself stays (US2 acc. 5, FR-027, R16).
func TestStuckMaintenanceLock(t *testing.T) {
	ctrs := newFakeCheck(nil, "containers", reading("containers/app", health.Good, ""))
	f := newFixture(t, &fakeStore{}, pausing(ctrs))
	f.maint.held = true
	f.tick(t) // 05:00 host time: the lock is first seen held.

	f.holdFor(t, 6*time.Hour, 5*time.Minute)
	if n := countContaining(f.tlg.texts(), "maintenance lock held"); n != 0 {
		t.Fatalf("stuck reported at exactly the limit: %q", f.tlg.texts())
	}

	f.next(t)
	if n := countContaining(f.tlg.texts(), stuckText); n != 1 {
		t.Fatalf("telegram = %q, want one %q", f.tlg.texts(), stuckText)
	}
	if s := f.r.Snapshot(); s.Quiet != health.ReasonMaintenance {
		t.Fatalf("quiet = %q, want the window to stay", s.Quiet)
	}
	if ctrs.count() != 0 {
		t.Fatalf("the paused check ran %d times in the window", ctrs.count())
	}

	// Held on: the stuck error is rate limited to once a day, not re-sent
	// every tick.
	f.holdFor(t, 23*time.Hour, 5*time.Minute)
	if n := countContaining(f.tlg.texts(), stuckText); n != 1 {
		t.Fatalf("stuck error sent %d times within a day, want 1", n)
	}
	f.holdFor(t, time.Hour+5*time.Minute, 5*time.Minute)
	if n := countContaining(f.tlg.texts(), stuckText); n != 2 {
		t.Fatalf("stuck error sent %d times after a day, want 2", n)
	}
}

// TestMaintenanceObservation: a free lock clears the observation, and a gap
// of more than 5 minutes since the lock was last seen held restarts it.
func TestMaintenanceObservation(t *testing.T) {
	cases := []struct {
		name string
		gap  func(t *testing.T, f *fixture)
		want bool // the stuck error is raised at 05:00 + 6h + 30s
	}{
		{"continuous", func(*testing.T, *fixture) {}, true},
		{"freed", func(t *testing.T, f *fixture) {
			t.Helper()

			f.maint.held = false
			f.next(t)
			f.maint.held = true
		}, false},
		{"probe error keeps it", func(t *testing.T, f *fixture) {
			t.Helper()

			f.maint.err = errProbe
			f.next(t)
			f.maint.err = nil
			f.next(t) // Seen held again within 5 minutes of the last time.
		}, true},
		{"gap over 5 minutes", func(_ *testing.T, f *fixture) {
			f.now = f.now.Add(5*time.Minute + time.Second)
		}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t, &fakeStore{})
			f.maint.held = true
			start := f.now
			f.tick(t)
			f.holdFor(t, time.Hour, 5*time.Minute)
			c.gap(t, f)
			for f.now.Before(start.Add(6*time.Hour + Tick)) {
				f.now = minTime(f.now.Add(5*time.Minute), start.Add(6*time.Hour+Tick))
				f.tick(t)
			}
			got := countContaining(f.tlg.texts(), "maintenance lock held since") == 1
			if got != c.want {
				t.Fatalf("stuck raised = %v, want %v; telegram = %q", got, c.want, f.tlg.texts())
			}
		})
	}
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}

	return b
}

// TestStuckLimitSurvivesRestart: the observation is stored in kv
// health.maintenance, so a restart within 5 minutes of the last held tick
// keeps the stuck clock; a longer gap restarts it (R16).
func TestStuckLimitSurvivesRestart(t *testing.T) {
	for _, c := range []struct {
		name string
		down time.Duration
		want bool
	}{
		{"restart within 5 minutes", 5 * time.Minute, true},
		{"restart after a longer gap", 5*time.Minute + time.Second, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			st := &fakeStore{}
			f := newFixture(t, st)
			f.maint.held = true
			start := f.now
			f.tick(t)
			f.holdFor(t, 3*time.Hour, 5*time.Minute)
			if st.state.KV[KVMaintenance] == "" {
				t.Fatalf("kv = %v, want %s stored", st.state.KV, KVMaintenance)
			}

			g := newFixture(t, st)
			g.maint.held = true
			g.now = f.now.Add(c.down)
			g.tick(t)
			for g.now.Before(start.Add(6*time.Hour + Tick)) {
				g.now = minTime(g.now.Add(5*time.Minute), start.Add(6*time.Hour+Tick))
				g.tick(t)
			}
			got := countContaining(g.tlg.texts(), stuckText) == 1
			if got != c.want {
				t.Fatalf("stuck raised = %v, want %v; telegram = %q", got, c.want, g.tlg.texts())
			}
		})
	}
}

// TestMaintenanceStoredCleared: a free lock clears the stored observation.
func TestMaintenanceStoredCleared(t *testing.T) {
	st := &fakeStore{}
	f := newFixture(t, st)
	f.maint.held = true
	f.tick(t)
	f.maint.held = false
	f.next(t)

	m, err := decodeMaintenance(st.state.KV[KVMaintenance])
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !m.HeldSince.IsZero() || !m.LastSeenHeld.IsZero() {
		t.Fatalf("observation = %+v, want cleared", m)
	}
}

// TestMaintenanceRestoreMerge: state loaded late (after ticks ran from
// memory) keeps the stored start when memory's observation continues it, and
// memory wins when it saw the lock free.
func TestMaintenanceRestoreMerge(t *testing.T) {
	stored := MaintenanceLock{HeldSince: t0().Add(-2 * time.Hour), LastSeenHeld: t0().Add(-time.Minute)}
	data, err := stored.encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	for _, c := range []struct {
		name string
		held bool
		want time.Time
	}{
		{"memory continues the stored hold", true, stored.HeldSince},
		{"memory saw the lock free", false, time.Time{}},
	} {
		t.Run(c.name, func(t *testing.T) {
			st := &fakeStore{loadErr: errProbe}
			st.state.KV = map[string]string{KVMaintenance: data}
			f := newFixture(t, st)
			f.maint.held = c.held
			f.tick(t) // Load fails; the tick observes from memory.

			st.mu.Lock()
			st.loadErr = nil
			st.mu.Unlock()
			f.next(t)

			if got := f.r.Snapshot().HeldSince; !got.Equal(c.want) {
				t.Fatalf("held since = %v, want %v", got, c.want)
			}
		})
	}
}

func TestDecodeMaintenance(t *testing.T) {
	if m, err := decodeMaintenance(""); err != nil || m != (MaintenanceLock{}) {
		t.Fatalf("empty: %+v, %v", m, err)
	}
	if _, err := decodeMaintenance("{"); err == nil {
		t.Fatal("want an error for a malformed value")
	}
	r := New(Options{})
	if err := r.restoreMaintenance("{"); err == nil {
		t.Fatal("want a restore error for a malformed value")
	}
	if err := maintenancePersister(t, r).Restore(store.State{}); err != nil {
		t.Fatalf("restore of nothing: %v", err)
	}
}

// maintenancePersister finds the runner's own persister of kv
// health.maintenance by its key, whatever its place in the list.
func maintenancePersister(t *testing.T, r *Runner) KVPersister {
	t.Helper()

	for _, p := range ownPersisters(r) {
		if kv, ok := p.(KVPersister); ok && kv.Key == KVMaintenance {
			return kv
		}
	}
	t.Fatalf("no persister of %s", KVMaintenance)

	return KVPersister{}
}

// TestFailedMaintenanceProbeNeverOpensTheWindow: a probe that fails is a
// checker error and never a quiet window, even when it also reports the
// lock held.
func TestFailedMaintenanceProbeNeverOpensTheWindow(t *testing.T) {
	ctrs := newFakeCheck(nil, "containers", reading("containers/app", health.Good, ""))
	f := newFixture(t, &fakeStore{}, pausing(ctrs))
	f.maint.held = true
	f.maint.err = errProbe
	f.tick(t)

	if s := f.r.Snapshot(); s.Quiet != "" {
		t.Fatalf("quiet = %q, want no window on a failed probe", s.Quiet)
	}
	if ctrs.count() != 1 {
		t.Fatalf("the paused check ran %d times, want 1 outside a window", ctrs.count())
	}
	if n := countContaining(f.tlg.texts(), "cannot probe maintenance lock"); n != 1 {
		t.Fatalf("telegram = %q, want one probe checker error", f.tlg.texts())
	}
}

func TestFormatHold(t *testing.T) {
	for d, want := range map[time.Duration]string{
		6 * time.Hour:    "6h",
		90 * time.Minute: "1h30m",
		45 * time.Second: "45s",
		time.Minute:      "1m",
	} {
		if got := formatHold(d); got != want {
			t.Errorf("formatHold(%v) = %q, want %q", d, got, want)
		}
	}
}

var errProbe = errors.New("probe failed")
