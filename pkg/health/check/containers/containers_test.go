package containers_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Toshik1978/tgshd/pkg/health"
	"github.com/Toshik1978/tgshd/pkg/health/check/containers"
)

type fakeSource struct {
	list []containers.Container
	err  error
}

func (f *fakeSource) List(context.Context) ([]containers.Container, error) { return f.list, f.err }

func t0() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }

func up(name string) containers.Container {
	return containers.Container{
		Name: name, RestartPolicy: "always", Status: "running", Running: true, StartedAt: t0(),
	}
}

func find(t *testing.T, res health.CheckResult, key string) health.Reading {
	t.Helper()
	for _, r := range res.Readings {
		if r.Key == key {
			return r
		}
	}
	t.Fatalf("no reading %q in %+v", key, res.Readings)

	return health.Reading{}
}

func TestVerdicts(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*containers.Container)
		want    health.Verdict
		contain string
	}{
		{"healthy", func(*containers.Container) {}, health.Good, ""},
		{"stopped", func(c *containers.Container) { c.Running = false; c.Status = "exited" }, health.Bad, "exited"},
		{"restarting", func(c *containers.Container) { c.Status = "restarting" }, health.Bad, "restarting"},
		{"unhealthy", func(c *containers.Container) { c.Health = "unhealthy" }, health.Bad, "unhealthy"},
		{"starting health ok", func(c *containers.Container) { c.Health = "starting" }, health.Good, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := up("web")
			tt.mutate(&c)
			chk := newCheck(t, &fakeSource{list: []containers.Container{c}}, nil)
			r := find(t, chk.Run(context.Background()), "containers/web")
			if r.Verdict != tt.want || !strings.Contains(r.Text, tt.contain) || !strings.Contains(r.Text, "web") {
				t.Fatalf("got %+v", r)
			}
		})
	}
}

func TestWatchedSelection(t *testing.T) {
	a := up("web")
	b := up("runner-1")
	c := up("db")
	c.RestartPolicy = "unless-stopped"
	d := up("tool")
	d.RestartPolicy = "no"
	chk := newCheck(t, &fakeSource{list: []containers.Container{a, b, c, d}}, []string{"runner-*"})
	res := chk.Run(context.Background())
	if len(res.Readings) != 2 {
		t.Fatalf("readings: %+v", res.Readings)
	}
	find(t, res, "containers/web")
	find(t, res, "containers/db")
}

type clock struct{ now time.Time }

func (c *clock) Now() time.Time { return c.now }

const threshold = 15 * time.Minute

func newClockCheck(t *testing.T, src containers.Source, exclude []string, clk *clock) *containers.Check {
	t.Helper()

	chk, err := containers.New(src, exclude, threshold, clk.Now)
	if err != nil {
		t.Fatal(err)
	}

	return chk
}

func newCheck(t *testing.T, src containers.Source, exclude []string) *containers.Check {
	t.Helper()

	return newClockCheck(t, src, exclude, &clock{now: t0().Add(time.Minute)})
}

func TestInvalidExcludeGlob(t *testing.T) {
	if _, err := containers.New(&fakeSource{}, []string{"["}, threshold, nil); err == nil {
		t.Fatal("expected error")
	}
}

func TestCrashLoopSticky(t *testing.T) {
	clk := &clock{now: t0().Add(time.Minute)}
	src := &fakeSource{list: []containers.Container{up("web")}}
	chk := newClockCheck(t, src, nil, clk)
	chk.Run(context.Background())

	c := up("web")
	c.RestartCount = 1
	c.StartedAt = clk.now
	src.list = []containers.Container{c}
	r := find(t, chk.Run(context.Background()), "containers/web")
	if r.Verdict != health.Bad || !strings.Contains(r.Text, "restarted") {
		t.Fatalf("restart: %+v", r)
	}

	// Unchanged but up for less than the threshold: still Bad.
	clk.now = clk.now.Add(5 * time.Minute)
	if r := find(t, chk.Run(context.Background()), "containers/web"); r.Verdict != health.Bad {
		t.Fatalf("sticky: %+v", r)
	}

	// Up for the threshold: Good again.
	clk.now = c.StartedAt.Add(threshold)
	if r := find(t, chk.Run(context.Background()), "containers/web"); r.Verdict != health.Good {
		t.Fatalf("recovered: %+v", r)
	}
}

func TestRestartingThenRunningIsBad(t *testing.T) {
	clk := &clock{now: t0().Add(time.Minute)}
	r1 := up("web")
	r1.Status = "restarting"
	src := &fakeSource{list: []containers.Container{r1}}
	chk := newClockCheck(t, src, nil, clk)
	chk.Run(context.Background())

	c := up("web")
	c.RestartCount = 1
	c.StartedAt = clk.now
	src.list = []containers.Container{c}
	if r := find(t, chk.Run(context.Background()), "containers/web"); r.Verdict != health.Bad {
		t.Fatalf("got %+v", r)
	}
}

func TestSlowLoopStaysBad(t *testing.T) {
	clk := &clock{now: t0().Add(time.Minute)}
	src := &fakeSource{}
	chk := newClockCheck(t, src, nil, clk)
	c := up("web")
	src.list = []containers.Container{c}
	chk.Run(context.Background())

	for i := range 8 {
		clk.now = clk.now.Add(5 * time.Minute)
		if i%2 == 0 {
			c.RestartCount++
			c.StartedAt = clk.now
		}
		src.list = []containers.Container{c}
		if r := find(t, chk.Run(context.Background()), "containers/web"); r.Verdict != health.Bad {
			t.Fatalf("run %d: %+v", i, r)
		}
	}
}

func TestChangeAfterDownIsNotCrashLoop(t *testing.T) {
	down := up("web")
	down.Running = false
	down.Status = "exited"
	src := &fakeSource{list: []containers.Container{down}}
	chk := newCheck(t, src, nil)
	chk.Run(context.Background())
	c := up("web")
	c.RestartCount = 3
	c.StartedAt = t0().Add(time.Hour)
	src.list = []containers.Container{c}
	if r := find(t, chk.Run(context.Background()), "containers/web"); r.Verdict != health.Good {
		t.Fatalf("got %+v", r)
	}
}

func TestGoneAndUnwatched(t *testing.T) {
	src := &fakeSource{list: []containers.Container{up("web"), up("db")}}
	chk := newCheck(t, src, nil)
	chk.Run(context.Background())

	db := up("db")
	db.RestartPolicy = "no"
	src.list = []containers.Container{db}
	res := chk.Run(context.Background())
	if r := find(t, res, "containers/web"); r.Verdict != health.Good || r.Text != "container web no longer exists" {
		t.Fatalf("web: %+v", r)
	}
	if r := find(t, res, "containers/db"); r.Verdict != health.Good || r.Text != "container db no longer watched" {
		t.Fatalf("db: %+v", r)
	}
	if res = chk.Run(context.Background()); len(res.Readings) != 0 {
		t.Fatalf("expected forgotten: %+v", res.Readings)
	}
}

func TestDockerUnavailable(t *testing.T) {
	src := &fakeSource{list: []containers.Container{up("web")}}
	chk := newCheck(t, src, nil)
	chk.Run(context.Background())
	src.err = errors.New("daemon down")
	res := chk.Run(context.Background())
	if res.Err == nil || len(res.Readings) != 0 {
		t.Fatalf("got %+v", res)
	}
	src.err = nil
	if r := find(t, chk.Run(context.Background()), "containers/web"); r.Verdict != health.Good {
		t.Fatalf("got %+v", r)
	}
}

func TestExportRestore(t *testing.T) {
	src := &fakeSource{list: []containers.Container{up("web")}}
	chk := newCheck(t, src, nil)
	chk.Run(context.Background())
	data, err := chk.Export()
	if err != nil {
		t.Fatal(err)
	}

	c := up("web")
	c.RestartCount = 2
	fresh := newCheck(t, &fakeSource{list: []containers.Container{c}}, nil)
	if err := fresh.Restore(data); err != nil {
		t.Fatal(err)
	}
	if r := find(t, fresh.Run(context.Background()), "containers/web"); r.Verdict != health.Bad {
		t.Fatalf("restored: %+v", r)
	}
	if err := fresh.Restore([]byte("{")); err == nil {
		t.Fatal("expected error")
	}

	if err := fresh.Restore([]byte("null")); err != nil {
		t.Fatal(err)
	}

	fresh.Run(context.Background())
}
