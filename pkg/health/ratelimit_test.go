package health_test

import (
	"testing"
	"time"

	"github.com/Toshik1978/tgshd/pkg/health"
)

func TestRateMarksAllow(t *testing.T) {
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	m := health.NewRateMarks()

	steps := []struct {
		name    string
		key     string
		channel string
		at      time.Duration
		want    bool
	}{
		{"first is allowed", "disk", "tlg", 0, true},
		{"within window denied", "disk", "tlg", 30 * time.Minute, false},
		{"other channel independent", "disk", "sms", 30 * time.Minute, true},
		{"other key independent", "net", "tlg", 30 * time.Minute, true},
		{"denied does not extend window", "disk", "tlg", time.Hour, true},
		{"window restarts from allow", "disk", "tlg", 90 * time.Minute, false},
		{"clock went backwards allows", "disk", "tlg", -time.Hour, true},
	}
	for _, s := range steps {
		if got := m.Allow(s.key, s.channel, t0.Add(s.at), time.Hour); got != s.want {
			t.Fatalf("%s: Allow = %v, want %v", s.name, got, s.want)
		}
	}
}

func TestRateMarksRevert(t *testing.T) {
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	m := health.NewRateMarks()
	m.Allow("disk", "email", t0, time.Hour)

	m.Revert("disk", "email", t0.Add(time.Minute)) // Not the mark taken: kept.
	if m.Allow("disk", "email", t0.Add(2*time.Minute), time.Hour) {
		t.Fatal("reverting a different mark must keep the window")
	}
	m.Revert("disk", "email", t0)
	if !m.Allow("disk", "email", t0.Add(2*time.Minute), time.Hour) {
		t.Fatal("a reverted mark must not limit the next send")
	}
}

func TestRateMarksKeyChannelNoCollision(t *testing.T) {
	now := time.Now()
	m := health.NewRateMarks()
	if !m.Allow("a", "b:c", now, time.Hour) || !m.Allow("a:b", "c", now, time.Hour) {
		t.Fatal("distinct key/channel pairs must not collide")
	}
}

func TestRateMarksExportRestore(t *testing.T) {
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	m := health.NewRateMarks()
	m.Allow("b", "tlg", t0, time.Hour)
	m.Allow("a", "sms", t0.Add(time.Minute), time.Hour)

	got := m.Export()
	want := []health.RateMark{
		{Key: "a", Channel: "sms", LastSentAt: t0.Add(time.Minute)},
		{Key: "b", Channel: "tlg", LastSentAt: t0},
	}
	if len(got) != len(want) {
		t.Fatalf("Export len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Export[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}

	r := health.NewRateMarks()
	r.Restore(got)
	if r.Allow("b", "tlg", t0.Add(30*time.Minute), time.Hour) {
		t.Fatal("restored mark must still rate-limit")
	}
	if !r.Allow("b", "tlg", t0.Add(time.Hour), time.Hour) {
		t.Fatal("restored mark must expire")
	}
}

func TestRateMarksMergeKeepsLater(t *testing.T) {
	t0 := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	early, late := t0, t0.Add(time.Hour)
	m := health.NewRateMarks()
	m.Allow("a", "tlg", early, time.Hour)
	m.Allow("b", "tlg", late, time.Hour)

	m.Merge([]health.RateMark{
		{Key: "a", Channel: "tlg", LastSentAt: late},
		{Key: "b", Channel: "tlg", LastSentAt: early},
		{Key: "c", Channel: "email", LastSentAt: early},
	})

	want := []health.RateMark{
		{Key: "a", Channel: "tlg", LastSentAt: late},
		{Key: "b", Channel: "tlg", LastSentAt: late},
		{Key: "c", Channel: "email", LastSentAt: early},
	}
	got := m.Export()
	if len(got) != len(want) {
		t.Fatalf("merged marks = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("merged[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}
