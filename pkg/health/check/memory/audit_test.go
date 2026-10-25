package memory_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Toshik1978/tgshd/pkg/health"
	"github.com/Toshik1978/tgshd/pkg/health/check/memory"
)

func auditSettings(exclude ...string) memory.Settings {
	s := settings()
	s.AuditExclude = exclude

	return s
}

func TestAuditOpensForARunningContainerWithoutALimit(t *testing.T) {
	src := &fakeSource{host: healthy(), containers: []memory.Container{
		{Name: "web-app", Running: true, HasLimit: false},
		{Name: "db", Running: true, HasLimit: true},
	}}

	res := memory.New(src, auditSettings()).Run(context.Background())

	r := find(t, res, "memory/limit/web-app")
	if r.Verdict != health.Bad || r.Kind != health.State || r.Text != "container web-app has no memory limit" {
		t.Fatalf("web-app reading = %+v", r)
	}
	r = find(t, res, "memory/limit/db")
	if r.Verdict != health.Good || r.Text != "container db has a memory limit" {
		t.Fatalf("db reading = %+v", r)
	}
}

func TestAuditResolvesWhenALimitIsSet(t *testing.T) {
	src := &fakeSource{host: healthy(), containers: []memory.Container{{Name: "web-app", Running: true}}}
	c := memory.New(src, auditSettings())
	if r := find(t, c.Run(context.Background()), "memory/limit/web-app"); r.Verdict != health.Bad {
		t.Fatalf("first run = %+v", r)
	}

	src.containers = []memory.Container{{Name: "web-app", Running: true, HasLimit: true}}

	r := find(t, c.Run(context.Background()), "memory/limit/web-app")
	if r.Verdict != health.Good || r.Text != "container web-app has a memory limit" {
		t.Fatalf("after the limit = %+v", r)
	}
}

func TestAuditResolvesWhenTheContainerStopsOrGoes(t *testing.T) {
	src := &fakeSource{host: healthy(), containers: []memory.Container{
		{Name: "web-app", Running: true}, {Name: "worker", Running: true},
	}}
	c := memory.New(src, auditSettings())
	c.Run(context.Background())

	// web-app stops; worker is removed.
	src.containers = []memory.Container{{Name: "web-app", Running: false}}
	res := c.Run(context.Background())
	for _, key := range []string{"memory/limit/web-app", "memory/limit/worker"} {
		if r := find(t, res, key); r.Verdict != health.Good {
			t.Fatalf("%s = %+v, want resolved", key, r)
		}
	}
	if r := find(t, res, "memory/limit/web-app"); r.Text != "container web-app stopped" {
		t.Fatalf("text = %q", r.Text)
	}

	// The removed container is not reported again.
	res = c.Run(context.Background())
	for _, r := range res.Readings {
		if r.Key == "memory/limit/worker" {
			t.Fatalf("worker reported again: %+v", r)
		}
	}
}

func TestAuditSkipsExcludedContainers(t *testing.T) {
	src := &fakeSource{host: healthy(), containers: []memory.Container{
		{Name: "scratch-job", Running: true}, {Name: "web-app", Running: true},
	}}

	res := memory.New(src, auditSettings("scratch-job")).Run(context.Background())

	for _, r := range res.Readings {
		if r.Key == "memory/limit/scratch-job" {
			t.Fatalf("excluded container judged: %+v", r)
		}
	}
	find(t, res, "memory/limit/web-app")
}

func TestAuditReadFailureIsACheckerErrorAndKeepsTheOtherReadings(t *testing.T) {
	src := &fakeSource{host: healthy(), ctrErr: errors.New("docker down")}

	res := memory.New(src, auditSettings()).Run(context.Background())

	var ce health.CheckError
	if !errors.As(res.Err, &ce) || ce.Subject != "containers" {
		t.Fatalf("err = %v, want a containers CheckError", res.Err)
	}
	find(t, res, "memory/available")
}

func TestAuditIsSilentWithoutAContainerRuntime(t *testing.T) {
	src := &fakeSource{host: healthy(), ctrErr: memory.ErrNoContainers}

	res := memory.New(src, auditSettings()).Run(context.Background())

	if res.Err != nil {
		t.Fatalf("err = %v, want none", res.Err)
	}
}

func TestAuditProblemNamesTheTopConsumer(t *testing.T) {
	src := &fakeSource{
		host:       healthy(),
		services:   []memory.Service{{Name: "web-app", Anon: 31 * gb}},
		containers: []memory.Container{{Name: "web-app", Running: true}, {Name: "worker", Running: true}},
	}

	res := memory.New(src, auditSettings()).Run(context.Background())

	for _, name := range []string{"web-app", "worker"} {
		want := "container " + name + " has no memory limit; top: web-app 31 GB"
		if r := find(t, res, "memory/limit/"+name); r.Verdict != health.Bad || r.Text != want {
			t.Errorf("%s reading = %+v, want text %q", name, r, want)
		}
	}
	if src.svcCalls != 1 {
		t.Errorf("services read %d times, want once for the top lookup", src.svcCalls)
	}
}

func TestAuditResolvedTextsCarryNoTopConsumer(t *testing.T) {
	src := &fakeSource{
		host:       healthy(),
		services:   []memory.Service{{Name: "web-app", Anon: 31 * gb}},
		containers: []memory.Container{{Name: "web-app", Running: true, HasLimit: true}},
	}

	res := memory.New(src, auditSettings()).Run(context.Background())

	if r := find(t, res, "memory/limit/web-app"); r.Text != "container web-app has a memory limit" {
		t.Fatalf("reading = %+v", r)
	}
	if src.svcCalls != 0 {
		t.Errorf("services read %d times with nothing to name", src.svcCalls)
	}
}

func TestAuditResolvesAContainerRemovedWhileTheDaemonWasDown(t *testing.T) {
	src := &fakeSource{host: healthy(), containers: []memory.Container{
		{Name: "web-app", Running: true}, {Name: "worker", Running: true},
	}}
	before := memory.New(src, auditSettings())
	before.Run(context.Background())
	saved, err := before.AuditState().Export()
	if err != nil {
		t.Fatalf("export: %v", err)
	}

	// The daemon restarts and worker is gone from the host meanwhile.
	src.containers = []memory.Container{{Name: "web-app", Running: true}}
	after := memory.New(src, auditSettings())
	if err := after.AuditState().Restore(saved); err != nil {
		t.Fatalf("restore: %v", err)
	}

	res := after.Run(context.Background())
	if r := find(t, res, "memory/limit/worker"); r.Verdict != health.Good || r.Text != "container worker stopped" {
		t.Fatalf("worker = %+v, want resolved", r)
	}
	if r := find(t, res, "memory/limit/web-app"); r.Verdict != health.Bad {
		t.Fatalf("web-app = %+v, want still open", r)
	}
}

func TestAuditKeepsRestoredContainersWhenTheListErrors(t *testing.T) {
	src := &fakeSource{host: healthy(), containers: []memory.Container{{Name: "worker", Running: true}}}
	before := memory.New(src, auditSettings())
	before.Run(context.Background())
	saved, _ := before.AuditState().Export()

	src.ctrErr = errors.New("docker down")
	after := memory.New(src, auditSettings())
	if err := after.AuditState().Restore(saved); err != nil {
		t.Fatalf("restore: %v", err)
	}
	for _, r := range after.Run(context.Background()).Readings {
		if r.Key == "memory/limit/worker" {
			t.Fatalf("unexpected reading on a list error: %+v", r)
		}
	}

	src.ctrErr = nil
	src.containers = nil
	if r := find(t, after.Run(context.Background()), "memory/limit/worker"); r.Verdict != health.Good {
		t.Fatalf("worker = %+v, want resolved once the list is readable", r)
	}
}

func TestAuditRestoreRejectsGarbage(t *testing.T) {
	c := memory.New(&fakeSource{host: healthy()}, auditSettings())
	if err := c.AuditState().Restore([]byte("{")); err == nil {
		t.Fatal("want an error for malformed data")
	}
	if err := c.AuditState().Restore(nil); err != nil {
		t.Fatalf("empty restore: %v", err)
	}
}

func TestAuditRestoreAfterARunKeepsTheFresherSet(t *testing.T) {
	src := &fakeSource{host: healthy(), containers: []memory.Container{{Name: "worker", Running: true}}}
	chk := memory.New(src, auditSettings())
	chk.Run(context.Background())

	// A late restore carries an older set; it must not forget worker.
	if err := chk.AuditState().Restore([]byte(`["db"]`)); err != nil {
		t.Fatalf("restore: %v", err)
	}

	src.containers = nil
	res := chk.Run(context.Background())
	for _, name := range []string{"worker", "db"} {
		if r := find(t, res, "memory/limit/"+name); r.Verdict != health.Good {
			t.Fatalf("%s = %+v, want resolved", name, r)
		}
	}
}
