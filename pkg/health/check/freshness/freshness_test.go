package freshness_test

import (
	"context"
	"errors"
	"io/fs"
	"testing"
	"time"

	"github.com/Toshik1978/tgshd/pkg/health"
	"github.com/Toshik1978/tgshd/pkg/health/check/freshness"
	"github.com/Toshik1978/tgshd/pkg/health/config"
)

func base() time.Time { return time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC) }

type fakeSource struct {
	mtime    time.Time
	mtimeErr error
	data     string
	readErr  error
}

func (f *fakeSource) ModTime(string) (time.Time, error) { return f.mtime, f.mtimeErr }
func (f *fakeSource) ReadFile(string) ([]byte, error)   { return []byte(f.data), f.readErr }

func entry(content config.Content) config.Freshness {
	return config.Freshness{
		Name:    "db-dump",
		Path:    "/var/lib/example/db",
		Content: content,
		MaxAge:  config.Duration(26 * time.Hour),
	}
}

func runAll(cfg config.Freshness, src *fakeSource, st freshness.State, now time.Time) health.CheckResult {
	c := freshness.New(cfg, src, st, func() time.Time { return now })

	return c.Run(context.Background())
}

func run(t *testing.T, cfg config.Freshness, src *fakeSource, st freshness.State, now time.Time) health.Reading {
	t.Helper()
	c := freshness.New(cfg, src, st, func() time.Time { return now })
	res := c.Run(context.Background())
	if res.Err != nil || len(res.Readings) != 1 {
		t.Fatalf("unexpected result: %+v", res)
	}
	r := res.Readings[0]
	if r.Key != "freshness/db-dump" {
		t.Fatalf("key = %q", r.Key)
	}

	return r
}

func noReading(t *testing.T, cfg config.Freshness, src *fakeSource, st freshness.State, now time.Time) {
	t.Helper()

	res := runAll(cfg, src, st, now)
	if res.Err != nil || len(res.Readings) != 0 {
		t.Fatalf("want no reading, got %+v", res)
	}
}

// A stale (Bad) file that is then deleted must not resolve its open problem.
func TestStaleThenMissingEmitsNothing(t *testing.T) {
	st := freshness.NewKVState(nil)
	cfg := entry(config.ContentMtime)

	stale := &fakeSource{mtime: base().Add(-30 * time.Hour)}
	if r := run(t, cfg, stale, st, base()); r.Verdict != health.Bad {
		t.Fatalf("stale = %v, want Bad", r.Verdict)
	}
	noReading(t, cfg, &fakeSource{mtimeErr: fs.ErrNotExist}, st, base().Add(time.Minute))
}

func TestMissingGraceUsesMarkPrecision(t *testing.T) {
	st := freshness.NewKVState(nil)
	cfg := entry(config.ContentMtime)
	missing := &fakeSource{mtimeErr: fs.ErrNotExist}

	noReading(t, cfg, missing, st, base().Add(900*time.Millisecond))
	// Exactly max_age after the stored second: still inside the grace.
	noReading(t, cfg, missing, st, base().Add(26*time.Hour+500*time.Millisecond))
}

func TestPresentFile(t *testing.T) {
	tests := []struct {
		name    string
		content config.Content
		src     fakeSource
		want    health.Verdict
	}{
		{"mtime fresh", config.ContentMtime, fakeSource{mtime: base().Add(-time.Hour)}, health.Good},
		{"mtime stale", config.ContentMtime, fakeSource{mtime: base().Add(-27 * time.Hour)}, health.Bad},
		{"mtime exactly max age", config.ContentMtime, fakeSource{mtime: base().Add(-26 * time.Hour)}, health.Good},
		{"mtime future", config.ContentMtime, fakeSource{mtime: base().Add(time.Hour)}, health.Good},
		{"mtime error", config.ContentMtime, fakeSource{mtimeErr: fs.ErrPermission}, health.Unknown},
		{"epoch fresh", config.ContentEpoch, fakeSource{data: "1768043400\n"}, health.Good},
		{"epoch stale", config.ContentEpoch, fakeSource{data: "1767900000"}, health.Bad},
		{"epoch garbage", config.ContentEpoch, fakeSource{data: "yesterday"}, health.Unknown},
		{"epoch empty", config.ContentEpoch, fakeSource{data: " \n"}, health.Unknown},
		{"epoch unreadable", config.ContentEpoch, fakeSource{readErr: fs.ErrPermission}, health.Unknown},
		{"epoch other error", config.ContentEpoch, fakeSource{readErr: errors.New("io error")}, health.Unknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := run(t, entry(tt.content), &tt.src, freshness.NewKVState(nil), base())
			if r.Verdict != tt.want {
				t.Errorf("verdict = %v, want %v (%s)", r.Verdict, tt.want, r.Text)
			}
			if r.Kind != health.State {
				t.Errorf("kind = %v", r.Kind)
			}
		})
	}
}

func TestMissingFile(t *testing.T) {
	missing := &fakeSource{mtimeErr: fs.ErrNotExist, readErr: fs.ErrNotExist}
	st := freshness.NewKVState(nil)
	cfg := entry(config.ContentMtime)

	noReading(t, cfg, missing, st, base())
	noReading(t, cfg, missing, st, base().Add(26*time.Hour))
	if r := run(t, cfg, missing, st, base().Add(26*time.Hour+time.Second)); r.Verdict != health.Bad {
		t.Fatalf("after max age = %v, want Bad", r.Verdict)
	}

	// Reappears: mark cleared, a later disappearance restarts the clock.
	present := &fakeSource{mtime: base().Add(27 * time.Hour)}
	if r := run(t, cfg, present, st, base().Add(28*time.Hour)); r.Verdict != health.Good {
		t.Fatalf("reappeared = %v, want Good", r.Verdict)
	}
	if _, ok := st.FirstMissing("db-dump"); ok {
		t.Fatalf("mark not cleared: %v", st.Export())
	}
	noReading(t, cfg, missing, st, base().Add(60*time.Hour))
}

func TestMissingEpochFile(t *testing.T) {
	missing := &fakeSource{readErr: fs.ErrNotExist}
	st := freshness.NewKVState(nil)
	cfg := entry(config.ContentEpoch)
	noReading(t, cfg, missing, st, base())
	if r := run(t, cfg, missing, st, base().Add(27*time.Hour)); r.Verdict != health.Bad {
		t.Fatalf("verdict = %v, want Bad", r.Verdict)
	}
}

func TestUnreadableKeepsMark(t *testing.T) {
	st := freshness.NewKVState(nil)
	cfg := entry(config.ContentMtime)
	noReading(t, cfg, &fakeSource{mtimeErr: fs.ErrNotExist}, st, base())
	perm := &fakeSource{mtimeErr: fs.ErrPermission}
	if r := run(t, cfg, perm, st, base().Add(time.Hour)); r.Verdict != health.Unknown {
		t.Fatalf("verdict = %v, want Unknown", r.Verdict)
	}
	if r := run(t, cfg, &fakeSource{mtimeErr: fs.ErrNotExist}, st, base().Add(27*time.Hour)); r.Verdict != health.Bad {
		t.Fatalf("mark lost: %v", r.Verdict)
	}
}

func TestKVStateRoundTrip(t *testing.T) {
	st := freshness.NewKVState(map[string]string{
		"other":                           "x",
		"freshness.db-dump.first_missing": "100",
		"freshness.bad.first_missing":     "nope",
	})
	if ts, ok := st.FirstMissing("db-dump"); !ok || ts.Unix() != 100 {
		t.Fatalf("restore: %v %v", ts, ok)
	}
	if _, ok := st.FirstMissing("bad"); ok {
		t.Fatal("corrupt value must read as absent")
	}
	st.SetFirstMissing("a", time.Unix(5, 0))
	got := st.Export()
	if got["freshness.a.first_missing"] != "5" || got["freshness.db-dump.first_missing"] != "100" {
		t.Fatalf("export = %v", got)
	}
	if _, ok := got["other"]; ok {
		t.Fatal("export must only hold freshness keys")
	}
	st.ClearFirstMissing("a")
	if _, ok := st.FirstMissing("a"); ok {
		t.Fatal("not cleared")
	}
}

func TestRunKeepsStamp(t *testing.T) {
	stamp := base().Add(-3 * time.Hour)
	tests := []struct {
		name      string
		src       fakeSource
		wantStamp string
		wantOK    string
	}{
		{"mtime", fakeSource{mtime: stamp}, "1768035600", "1"},
		{"epoch", fakeSource{data: "1768028400\n"}, "1768028400", "1"},
		{"stale is still stamped", fakeSource{mtime: base().Add(-300 * time.Hour)}, "1766966400", "1"},
		{"missing", fakeSource{mtimeErr: fs.ErrNotExist}, "", "1"},
		{"unreadable", fakeSource{mtimeErr: fs.ErrPermission}, "", "0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := freshness.NewKVState(nil)
			// A previous good run must be replaced, not kept, by a failing one.
			runAll(entry(config.ContentMtime), &fakeSource{mtime: base().Add(-time.Hour)}, st, base())
			cfg := entry(config.ContentMtime)
			if tt.src.data != "" {
				cfg = entry(config.ContentEpoch)
			}
			runAll(cfg, &tt.src, st, base())

			got := st.Export()
			if got["freshness.db-dump.stamp"] != tt.wantStamp || got["freshness.db-dump.read_ok"] != tt.wantOK {
				t.Fatalf("kv = %v, want stamp %q read_ok %q", got, tt.wantStamp, tt.wantOK)
			}
		})
	}
}

func TestKVStateStamp(t *testing.T) {
	st := freshness.NewKVState(map[string]string{
		"freshness.a.stamp":   "200",
		"freshness.a.read_ok": "1",
		"freshness.b.read_ok": "0",
		"freshness.c.stamp":   "junk",
		"other.a.stamp":       "9",
	})
	if ts, present, ok := st.Stamp("a"); !present || !ok || ts.Unix() != 200 {
		t.Fatalf("a = %v %v %v", ts, present, ok)
	}
	if _, present, ok := st.Stamp("b"); present || ok {
		t.Fatalf("b = %v %v", present, ok)
	}
	if _, present, _ := st.Stamp("c"); present {
		t.Fatal("corrupt stamp must read as absent")
	}
	if _, ok := st.Export()["other.a.stamp"]; ok {
		t.Fatal("foreign key restored")
	}
}
