package runner

import (
	"context"
	"errors"
	"maps"
	"slices"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/Toshik1978/tgshd/pkg/health"
	"github.com/Toshik1978/tgshd/pkg/health/notify"
	"github.com/Toshik1978/tgshd/pkg/health/store"
)

// fakeFixes hands out the fixes queued by its check's runs.
type fakeFixes struct {
	mu    sync.Mutex
	fixes []Fix
	takes int
}

func (f *fakeFixes) add(fx ...Fix) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.fixes = append(f.fixes, fx...)
}

func (f *fakeFixes) TakeFixes() []Fix {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.takes++
	out := f.fixes
	f.fixes = nil

	return out
}

func fixAlerts(st *fakeStore) []store.Alert {
	st.mu.Lock()
	defer st.mu.Unlock()

	var out []store.Alert
	for _, a := range st.alerts {
		if a.Kind == store.KindFix {
			out = append(out, a)
		}
	}

	return out
}

func TestFixesAreRecordedAndSentOnlyWhenAsked(t *testing.T) {
	st := &fakeStore{}
	src := &fakeFixes{}
	at := t0().Add(-time.Second)
	chk := health.Check{
		Name: "command/app", Interval: Tick, Mode: health.ModeTelegram,
		Run: func(context.Context) health.CheckResult {
			// The fix is taken after the run that made it.
			src.add(
				Fix{
					At:    at,
					Check: "command",
					Key:   "command/app/fix",
					Text:  "command app: fix ran restart.sh",
					Send:  true,
				},
				Fix{At: at, Check: "command", Key: "command/quiet/fix", Text: "command quiet: fix ran tidy.sh"},
			)

			return reading("command/app", health.Bad, "command app: probe exited with status 1")
		},
	}
	f := newFixture(t, st, chk)
	f.r.opts.Fixes = []FixSource{src}

	f.tick(t)

	got := fixAlerts(st)
	want := []store.Alert{
		{
			At:    at,
			Kind:  store.KindFix,
			Check: "command",
			Key:   "command/app/fix",
			Text:  "command app: fix ran restart.sh",
		},
		{
			At:    at,
			Kind:  store.KindFix,
			Check: "command",
			Key:   "command/quiet/fix",
			Text:  "command quiet: fix ran tidy.sh",
		},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("fix records = %+v, want %+v", got, want)
	}
	sent := f.tlg.texts()
	if n := countContaining(sent, "command app: fix ran restart.sh"); n != 1 {
		t.Fatalf("telegram = %q, want the reported fix once", sent)
	}
	if n := countContaining(sent, "fix ran tidy.sh"); n != 0 {
		t.Fatalf("telegram = %q, a journal-only fix must not be sent", sent)
	}
}

func TestReportedFixesFollowTheEntrysMode(t *testing.T) {
	tests := []struct {
		mode         health.Mode
		offline      bool
		defaultIsSMS bool
		wantTLG      int
		wantSMS      int
	}{
		{mode: health.ModeTelegram, wantTLG: 1},
		{mode: health.ModeSMS, wantSMS: 1},
		{mode: health.ModeTelegramOrSMS, wantTLG: 1},
		// Telegram is known down: tlg-or-sms falls back to SMS.
		{mode: health.ModeTelegramOrSMS, offline: true, wantSMS: 1},
		// No mode: the notifier's default delivery mode.
		{mode: health.ModeDefault, wantTLG: 1},
		{mode: health.ModeDefault, defaultIsSMS: true, wantSMS: 1},
	}
	for _, tt := range tests {
		name := "mode " + string(tt.mode)
		if tt.offline {
			name += " offline"
		}
		if tt.defaultIsSMS {
			name += " default sms"
		}
		t.Run(name, func(t *testing.T) {
			src := &fakeFixes{}
			src.add(Fix{
				At: t0(), Check: "command", Key: "command/app/fix", Text: "command app: fix ran restart.sh",
				Send: true, Mode: tt.mode,
			})
			var checks []health.Check
			if tt.offline {
				// The outside is known down for the tick.
				checks = append(checks, health.Check{
					Name: notify.CheckNetwork, Interval: Tick, Run: func(context.Context) health.CheckResult {
						return reading(KeyInternet, health.Bad, "internet down")
					},
				})
			}
			f := newFixture(t, &fakeStore{}, checks...)
			f.r.opts.Fixes = []FixSource{src}
			if tt.defaultIsSMS {
				f.r.opts.Mode = notify.ModeSMS
				f.r = New(f.r.opts)
			}

			f.tick(t)

			if got := countContaining(f.tlg.texts(), "fix ran restart.sh"); got != tt.wantTLG {
				t.Fatalf("telegram = %q, want %d", f.tlg.texts(), tt.wantTLG)
			}
			if got := countContaining(f.sms.texts(), "fix ran restart.sh"); got != tt.wantSMS {
				t.Fatalf("sms = %q, want %d", f.sms.texts(), tt.wantSMS)
			}
		})
	}
}

func TestFixesOfAnInterruptedTickReachTheAlertLog(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st := &fakeStore{state: store.State{KV: map[string]string{"kernel.state": "stored"}}}
	src := &fakeFixes{}
	at := t0().Add(-time.Second)
	chk := health.Check{Name: "command/app", Interval: Tick, Run: func(context.Context) health.CheckResult {
		src.add(
			Fix{At: at, Check: "command", Key: "command/app/fix", Text: "command app: fix ran restart.sh", Send: true},
		)
		cancel() // Shutdown while the check runs.

		return reading("command/app", health.Bad, "command app: probe exited with status 1")
	}}
	p := &fakePersister{key: "kernel.state", order: &orderLog{}}
	f := newFixture(t, st, chk)
	f.r.opts.Fixes = []FixSource{src}
	f.r.opts.Persisters = []Persister{p}

	if err := f.r.Do(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Do = %v, want context.Canceled", err)
	}

	got := fixAlerts(st)
	if len(got) != 1 || got[0].Key != "command/app/fix" || !got[0].At.Equal(at) {
		t.Fatalf("fix records = %+v, want the fix of the interrupted tick", got)
	}
	// Nothing else of the tick is kept: its results were never dispatched.
	if len(f.tlg.texts()) != 0 || len(st.state.Conditions) != 0 || len(st.alerts) != 1 {
		t.Fatalf("sent %q, conditions %+v, alerts %+v", f.tlg.texts(), st.state.Conditions, st.alerts)
	}
	if st.state.KV["kernel.state"] != "stored" || slices.Contains(p.order.get(), "save kernel.state") {
		t.Fatalf("kv = %v, calls %v: the checks' state must not be saved", st.state.KV, p.order.get())
	}
}

func TestLateFixesAreTakenNextTickAndNeverInAQuietWindow(t *testing.T) {
	st := &fakeStore{}
	src := &fakeFixes{}
	f := newFixture(t, st)
	f.r.opts.Fixes = []FixSource{src}

	f.tick(t)
	if src.takes != 1 || len(fixAlerts(st)) != 0 {
		t.Fatalf("takes = %d, records = %+v", src.takes, fixAlerts(st))
	}

	// A run abandoned after its deadline finishes its fix later.
	src.add(Fix{At: f.now, Check: "command", Key: "command/app/fix", Text: "late", Send: true})
	f.maint.held = true
	f.next(t)
	if src.takes != 1 {
		t.Fatalf("takes = %d, a quiet tick must not take fixes", src.takes)
	}

	f.maint.held = false
	f.next(t)
	if got := fixAlerts(st); len(got) != 1 || got[0].Text != "late" {
		t.Fatalf("records = %+v, want the late fix", got)
	}
	if countContaining(f.tlg.texts(), "late") != 1 {
		t.Fatalf("telegram = %q", f.tlg.texts())
	}
}

func TestFixesWithoutStoreAreStillSent(t *testing.T) {
	src := &fakeFixes{}
	src.add(
		Fix{At: t0(), Check: "command", Key: "command/app/fix", Text: "command app: fix ran restart.sh", Send: true},
	)
	f := newFixture(t, nil)
	f.r.opts.Store = nil
	f.r.opts.Fixes = []FixSource{src}

	f.tick(t)
	if countContaining(f.tlg.texts(), "fix ran restart.sh") != 1 {
		t.Fatalf("telegram = %q", f.tlg.texts())
	}
}

// The daemon logs at Warn in production (cmd newLogger), so a fix is logged at
// Warn to reach the journal.
func TestEveryFixReachesTheProductionLog(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	src := &fakeFixes{}
	src.add(
		Fix{At: t0(), Check: "command", Key: "command/app/fix", Text: "command app: fix ran restart.sh", Send: true},
		Fix{At: t0(), Check: "command", Key: "command/quiet/fix", Text: "command quiet: fix failed: exit status 2"},
	)
	f := newFixture(t, &fakeStore{})
	f.r.opts.Fixes = []FixSource{src}
	f.r.opts.Logger = zap.New(core)

	f.tick(t)

	entries := logs.FilterMessage("health fix action").All()
	want := []map[string]any{
		{"check": "command", "key": "command/app/fix", "outcome": "command app: fix ran restart.sh", "reported": true},
		{
			"check": "command", "key": "command/quiet/fix", "outcome": "command quiet: fix failed: exit status 2",
			"reported": false,
		},
	}
	if len(entries) != len(want) {
		t.Fatalf("fix log = %+v, want one Warn entry per fix", entries)
	}
	for i, e := range entries {
		if e.Level != zap.WarnLevel || !maps.Equal(e.ContextMap(), want[i]) {
			t.Fatalf("entry %d = %v %v, want Warn %v", i, e.Level, e.ContextMap(), want[i])
		}
	}
}
