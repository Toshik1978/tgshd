package memory_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Toshik1978/tgshd/pkg/health"
	"github.com/Toshik1978/tgshd/pkg/health/check/memory"
)

func start0() time.Time { return time.Date(2026, 1, 2, 10, 0, 0, 0, time.UTC) }

type clock struct{ now time.Time }

func (c *clock) Now() time.Time { return c.now }

func growthSettings(c *clock) memory.Settings {
	s := settings()
	s.GrowthWindow = time.Hour
	s.GrowthWarn = 4 * gb
	s.Now = c.Now

	return s
}

// step sets the service sizes, advances the clock by d and runs the check.
func step(chk *memory.Check, src *fakeSource, c *clock, d time.Duration, sizes map[string]uint64) health.CheckResult {
	c.now = c.now.Add(d)
	src.services = nil
	for name, anon := range sizes {
		src.services = append(src.services, memory.Service{Name: name, Anon: anon})
	}

	return chk.Run(context.Background())
}

func growthReading(t *testing.T, res health.CheckResult, name string) health.Reading {
	t.Helper()

	return find(t, res, "memory/growth/"+name)
}

func TestGrowthSteadyClimbOpens(t *testing.T) {
	c := &clock{now: start0()}
	src := &fakeSource{host: healthy()}
	chk := memory.New(src, growthSettings(c))

	var res health.CheckResult
	for i := range 5 { // 12, 16, 20, 24, 28 GB over 40 min
		res = step(chk, src, c, 10*time.Minute, map[string]uint64{"web-app": uint64(12+4*i) * gb})
		if i == 0 {
			if r := growthReading(t, res, "web-app"); r.Verdict != health.Good {
				t.Fatalf("first point = %+v", r)
			}
		}
	}

	r := growthReading(t, res, "web-app")
	if r.Verdict != health.Bad || r.Kind != health.State ||
		r.Text != "web-app grew 12→28 GB in 40 min; top: web-app 28 GB" {
		t.Fatalf("reading = %+v", r)
	}
}

func TestGrowthBelowWarnStaysGood(t *testing.T) {
	c := &clock{now: start0()}
	src := &fakeSource{host: healthy()}
	chk := memory.New(src, growthSettings(c))

	step(chk, src, c, time.Minute, map[string]uint64{"db": 10 * gb})
	res := step(chk, src, c, 10*time.Minute, map[string]uint64{"db": 13 * gb})

	r := growthReading(t, res, "db")
	if r.Verdict != health.Good || r.Text != "db at 13 GB" {
		t.Fatalf("reading = %+v", r)
	}
}

func TestGrowthSawtoothNeverOpens(t *testing.T) {
	c := &clock{now: start0()}
	src := &fakeSource{host: healthy()}
	chk := memory.New(src, growthSettings(c))

	// A cache that swings by less than growth_warn and is flushed back.
	for _, gbs := range []uint64{10, 12, 13, 10, 12, 13, 9, 11} {
		res := step(chk, src, c, 5*time.Minute, map[string]uint64{"db": gbs * gb})
		if r := growthReading(t, res, "db"); r.Verdict != health.Good {
			t.Fatalf("at %d GB: %+v", gbs, r)
		}
	}
}

func TestGrowthMinimumAfterMaximumNeverOpens(t *testing.T) {
	c := &clock{now: start0()}
	src := &fakeSource{host: healthy()}
	chk := memory.New(src, growthSettings(c))

	// Falling then flat at the bottom: rise is small, and falls do not warn.
	var res health.CheckResult
	for _, gbs := range []uint64{30, 20, 10, 10} {
		res = step(chk, src, c, 5*time.Minute, map[string]uint64{"db": gbs * gb})
	}
	if r := growthReading(t, res, "db"); r.Verdict != health.Good {
		t.Fatalf("reading = %+v", r)
	}
}

func TestGrowthFlatServiceStaysGood(t *testing.T) {
	c := &clock{now: start0()}
	src := &fakeSource{host: healthy()}
	chk := memory.New(src, growthSettings(c))

	var res health.CheckResult
	for range 70 {
		res = step(chk, src, c, time.Minute, map[string]uint64{"db": 12 * gb})
	}
	if r := growthReading(t, res, "db"); r.Verdict != health.Good || r.Text != "db at 12 GB" {
		t.Fatalf("reading = %+v", r)
	}
}

func TestGrowthWarnsWellBeforeDanger(t *testing.T) {
	// SC-003: 20 GB/h on a 62 GB host with 28 GB available warns at least
	// 30 minutes before available memory reaches 5 %.
	c := &clock{now: start0()}
	src := &fakeSource{host: healthy()}
	chk := memory.New(src, growthSettings(c))
	total := healthy().Total
	danger := total / 20
	start := uint64(10) * gb

	var warnedAt, dangerAt time.Duration
	for i := range 121 {
		elapsed := time.Duration(i) * time.Minute
		grown := uint64(float64(20*gb) * elapsed.Hours())
		host := healthy()
		host.Available = healthy().Available - min(grown, healthy().Available)
		src.host = host
		res := step(chk, src, c, time.Minute, map[string]uint64{"web-app": start + grown})
		if warnedAt == 0 && growthReading(t, res, "web-app").Verdict == health.Bad {
			warnedAt = elapsed
		}
		if dangerAt == 0 && host.Available < danger {
			dangerAt = elapsed
		}
	}
	if warnedAt == 0 || dangerAt == 0 || dangerAt-warnedAt < 30*time.Minute {
		t.Fatalf("warned at %v, danger at %v", warnedAt, dangerAt)
	}
}

func TestGrowthHysteresisAndResolve(t *testing.T) {
	c := &clock{now: start0()}
	src := &fakeSource{host: healthy()}
	chk := memory.New(src, growthSettings(c))

	step(chk, src, c, time.Minute, map[string]uint64{"web-app": 10 * gb})
	step(chk, src, c, 5*time.Minute, map[string]uint64{"web-app": 12 * gb})
	res := step(chk, src, c, 6*time.Minute, map[string]uint64{"web-app": 15 * gb})
	if r := growthReading(t, res, "web-app"); r.Verdict != health.Bad {
		t.Fatalf("not open: %+v", r)
	}

	// The window slides past the early points: rise is now 3 GB, between
	// half (2 GB) and the full warn level (4 GB), so it stays open.
	res = step(chk, src, c, 55*time.Minute, map[string]uint64{"web-app": 18 * gb})
	if r := growthReading(t, res, "web-app"); r.Verdict != health.Bad {
		t.Fatalf("hysteresis lost: %+v", r)
	}

	// Falls back to the bottom: rise 0, resolved.
	step(chk, src, c, 5*time.Minute, map[string]uint64{"web-app": 15 * gb})
	res = step(chk, src, c, 5*time.Minute, map[string]uint64{"web-app": 15 * gb})
	r := growthReading(t, res, "web-app")
	if r.Verdict != health.Good || r.Text != "web-app at 15 GB" {
		t.Fatalf("reading = %+v", r)
	}
}

func TestGrowthServiceGoneResolves(t *testing.T) {
	c := &clock{now: start0()}
	src := &fakeSource{host: healthy()}
	chk := memory.New(src, growthSettings(c))

	step(chk, src, c, time.Minute, map[string]uint64{"web-app": 10 * gb})
	res := step(chk, src, c, 10*time.Minute, map[string]uint64{"web-app": 20 * gb})
	if growthReading(t, res, "web-app").Verdict != health.Bad {
		t.Fatal("not open")
	}

	res = step(chk, src, c, time.Minute, map[string]uint64{})
	r := growthReading(t, res, "web-app")
	if r.Verdict != health.Good || !strings.Contains(r.Text, "web-app") {
		t.Fatalf("reading = %+v", r)
	}

	// Gone for longer than the window: no more readings, and no state kept.
	res = step(chk, src, c, 2*time.Hour, map[string]uint64{})
	for _, rd := range res.Readings {
		if rd.Key == "memory/growth/web-app" {
			t.Fatalf("stale reading %+v", rd)
		}
	}
	data, err := chk.Export()
	if err != nil || strings.Contains(string(data), "web-app") {
		t.Fatalf("export = %s, %v", data, err)
	}
}

func TestGrowthExportRestore(t *testing.T) {
	c := &clock{now: start0()}
	src := &fakeSource{host: healthy()}
	chk := memory.New(src, growthSettings(c))

	step(chk, src, c, time.Minute, map[string]uint64{"web-app": 10 * gb})
	step(chk, src, c, 10*time.Minute, map[string]uint64{"web-app": 16 * gb})
	data, err := chk.Export()
	if err != nil {
		t.Fatal(err)
	}

	fresh := memory.New(src, growthSettings(c))
	if err := fresh.Restore(data); err != nil {
		t.Fatal(err)
	}
	res := step(fresh, src, c, 10*time.Minute, map[string]uint64{"web-app": 22 * gb})
	r := growthReading(t, res, "web-app")
	if r.Verdict != health.Bad || r.Text != "web-app grew 10→22 GB in 20 min; top: web-app 22 GB" {
		t.Fatalf("reading = %+v", r)
	}

	if err := fresh.Restore(nil); err != nil {
		t.Fatalf("empty restore: %v", err)
	}
	if err := fresh.Restore([]byte("{")); err == nil {
		t.Fatal("bad data accepted")
	}
}

func TestGrowthDisabledWithoutSettings(t *testing.T) {
	src := &fakeSource{host: healthy(), services: []memory.Service{{Name: "db", Anon: gb}}}
	res := memory.New(src, settings()).Run(context.Background())
	for _, r := range res.Readings {
		if strings.HasPrefix(r.Key, "memory/growth/") {
			t.Fatalf("unexpected %+v", r)
		}
	}
}

func TestGrowthServicesErrorIsCheckerError(t *testing.T) {
	c := &clock{now: start0()}
	src := &fakeSource{host: healthy(), svcErr: context.DeadlineExceeded}
	res := memory.New(src, growthSettings(c)).Run(context.Background())
	if res.Err == nil {
		t.Fatal("expected checker error")
	}
}

func TestGrowthProblemTopLookupHappensOncePerRun(t *testing.T) {
	c := &clock{now: start0()}
	src := &fakeSource{host: healthy(), containers: []memory.Container{{Name: "db", Running: true}}}
	chk := memory.New(src, growthSettings(c))

	var res health.CheckResult
	for i := range 5 {
		src.svcCalls = 0
		res = step(chk, src, c, 10*time.Minute, map[string]uint64{
			"web-app": uint64(12+4*i) * gb, "worker": uint64(11+4*i) * gb,
		})
	}

	for _, name := range []string{"web-app", "worker"} {
		r := growthReading(t, res, name)
		if r.Verdict != health.Bad || !strings.HasSuffix(r.Text, "; top: web-app 28 GB") {
			t.Errorf("%s reading = %+v", name, r)
		}
	}
	if r := find(t, res, "memory/limit/db"); r.Verdict != health.Bad || !strings.Contains(r.Text, "; top: ") {
		t.Errorf("limit reading = %+v", r)
	}
	// One read for the growth history, one shared top lookup for every problem.
	if src.svcCalls != 2 {
		t.Errorf("services read %d times, want 2", src.svcCalls)
	}
}
