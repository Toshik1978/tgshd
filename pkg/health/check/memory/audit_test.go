package memory_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/Toshik1978/tgshd/pkg/health"
	"github.com/Toshik1978/tgshd/pkg/health/check/memory"
)

const keyLimit = "memory/limit"

func auditSettings(exclude ...string) memory.Settings {
	s := settings()
	s.AuditExclude = exclude

	return s
}

// thresholdSettings is auditSettings with the default 5m threshold over a 1m
// interval, on clock c.
func thresholdSettings(c *clock) memory.Settings {
	s := auditSettings()
	s.Interval = time.Minute
	s.Threshold = 5 * time.Minute
	s.Now = c.Now

	return s
}

// kept is a running container Docker keeps running, without a memory limit.
func kept(name string) memory.Container {
	return memory.Container{Name: name, Running: true, RestartPolicy: "unless-stopped"}
}

// limitEvents returns the audit's event readings.
func limitEvents(res health.CheckResult) []health.Reading {
	var out []health.Reading
	for _, r := range res.Readings {
		if r.Kind == health.Event && strings.HasPrefix(r.Key, keyLimit+"/") {
			out = append(out, r)
		}
	}

	return out
}

func noLimitReading(t *testing.T, res health.CheckResult) {
	t.Helper()
	for _, r := range res.Readings {
		if strings.HasPrefix(r.Key, keyLimit) {
			t.Fatalf("unexpected audit reading %+v", r)
		}
	}
}

// US3-1: one problem names every counted container, largest first.
func TestAuditRaisesOneProblemNamingEveryContainerLargestFirst(t *testing.T) {
	src := &fakeSource{
		host: healthy(),
		services: []memory.Service{
			{Name: "cache", Anon: gb / 10 * 12},
			{Name: "web-app", Anon: 31 * gb},
			{Name: "worker", Anon: 3 * gb},
			{Name: "db", Anon: 9 * gb},
		},
		containers: []memory.Container{
			kept("cache"),
			{Name: "web-app", Running: true, RestartPolicy: "always"},
			kept("worker"),
			{Name: "db", Running: true, RestartPolicy: "always", HasLimit: true},
			{Name: "backup", Running: false, RestartPolicy: "always"},
		},
	}

	res := memory.New(src, auditSettings()).Run(context.Background())

	r := find(t, res, keyLimit)
	want := "3 containers run without a memory limit: web-app 31 GB, worker 3 GB, cache 1.2 GB; top: web-app 31 GB"
	if r.Verdict != health.Bad || r.Kind != health.State || r.Text != want {
		t.Fatalf("reading = %+v, want text %q", r, want)
	}
	if ev := limitEvents(res); len(ev) != 0 {
		t.Fatalf("events on opening = %+v, want none", ev)
	}
	if src.svcCalls != 1 {
		t.Errorf("services read %d times, want once", src.svcCalls)
	}
}

func TestAuditNamesOneContainerAndOneWithoutASample(t *testing.T) {
	src := &fakeSource{host: healthy(), containers: []memory.Container{kept("web-app")}}

	r := find(t, memory.New(src, auditSettings()).Run(context.Background()), keyLimit)

	if want := "1 container runs without a memory limit: web-app"; r.Text != want {
		t.Fatalf("text = %q, want %q", r.Text, want)
	}
}

func TestAuditOrdersEqualAndUnsampledContainersByName(t *testing.T) {
	src := &fakeSource{
		host:     healthy(),
		services: []memory.Service{{Name: "worker", Anon: 2 * gb}, {Name: "cache", Anon: 2 * gb}},
		containers: []memory.Container{
			kept("queue-ok"), kept("worker"), kept("alpha"), kept("cache"),
		},
	}

	r := find(t, memory.New(src, auditSettings()).Run(context.Background()), keyLimit)

	want := "4 containers run without a memory limit: cache 2 GB, worker 2 GB, alpha, queue-ok; top: worker 2 GB"
	if r.Text != want {
		t.Fatalf("text = %q, want %q", r.Text, want)
	}
}

// US3-2: the problem resolves when the last container gains a limit or stops.
func TestAuditResolvesWhenTheSetEmpties(t *testing.T) {
	src := &fakeSource{host: healthy(), containers: []memory.Container{kept("web-app"), kept("worker")}}
	c := memory.New(src, auditSettings())
	if r := find(t, c.Run(context.Background()), keyLimit); r.Verdict != health.Bad {
		t.Fatalf("first run = %+v", r)
	}

	limited := kept("web-app")
	limited.HasLimit = true
	stopped := kept("worker")
	stopped.Running = false
	src.containers = []memory.Container{limited, stopped}

	res := c.Run(context.Background())
	r := find(t, res, keyLimit)
	if r.Verdict != health.Good || r.Kind != health.State || r.Text != "every container runs with a memory limit" {
		t.Fatalf("resolved reading = %+v", r)
	}
	if ev := limitEvents(res); len(ev) != 0 {
		t.Fatalf("events = %+v, want none", ev)
	}
}

func TestAuditIsGoodWithNoContainers(t *testing.T) {
	src := &fakeSource{host: healthy()}

	res := memory.New(src, auditSettings()).Run(context.Background())

	if r := find(t, res, keyLimit); r.Verdict != health.Good {
		t.Fatalf("reading = %+v", r)
	}
	if src.svcCalls != 0 {
		t.Errorf("services read %d times with nothing to name", src.svcCalls)
	}
}

// US3-3: excluded containers, by name or glob, are not counted.
func TestAuditSkipsExcludedContainers(t *testing.T) {
	src := &fakeSource{host: healthy(), containers: []memory.Container{
		kept("scratch-job"), kept("runner-7"), kept("runner"), kept("web-app"),
	}}

	r := find(t, memory.New(src, auditSettings("scratch-job", "runner-*")).Run(context.Background()), keyLimit)

	if want := "2 containers run without a memory limit: runner, web-app"; r.Text != want {
		t.Fatalf("text = %q, want %q", r.Text, want)
	}
}

// US3-4: a container without restart policy always/unless-stopped is not counted.
func TestAuditCountsOnlyContainersDockerKeepsRunning(t *testing.T) {
	for _, policy := range []string{"", "no", "on-failure"} {
		t.Run("policy "+policy, func(t *testing.T) {
			src := &fakeSource{host: healthy(), containers: []memory.Container{
				{Name: "scratch-job", Running: true, RestartPolicy: policy},
			}}

			r := find(t, memory.New(src, auditSettings()).Run(context.Background()), keyLimit)

			if r.Verdict != health.Good {
				t.Fatalf("reading = %+v, want not counted", r)
			}
		})
	}
}

// US3-5: a joiner is announced once; the open problem is not repeated.
func TestAuditAnnouncesAContainerJoiningAnOpenProblemOnce(t *testing.T) {
	src := &fakeSource{host: healthy(), containers: []memory.Container{kept("web-app")}}
	c := memory.New(src, auditSettings())
	c.Run(context.Background())

	src.containers = []memory.Container{kept("web-app"), kept("queue-ok")}
	res := c.Run(context.Background())

	ev := limitEvents(res)
	if len(ev) != 1 || ev[0].Key != keyLimit+"/queue-ok" || ev[0].Mode != health.ModeDefault ||
		ev[0].Text != "container queue-ok runs without a memory limit" {
		t.Fatalf("events = %+v, want one for queue-ok", ev)
	}
	if r := find(t, res, keyLimit); r.Verdict != health.Bad ||
		r.Text != "2 containers run without a memory limit: queue-ok, web-app" {
		t.Fatalf("problem = %+v", r)
	}

	if ev := limitEvents(c.Run(context.Background())); len(ev) != 0 {
		t.Fatalf("announced again: %+v", ev)
	}
}

func TestAuditDoesNotAnnounceALeaver(t *testing.T) {
	src := &fakeSource{host: healthy(), containers: []memory.Container{kept("web-app"), kept("worker")}}
	c := memory.New(src, auditSettings())
	c.Run(context.Background())

	src.containers = []memory.Container{kept("web-app")}
	res := c.Run(context.Background())

	if ev := limitEvents(res); len(ev) != 0 {
		t.Fatalf("events = %+v, want none", ev)
	}
	if r := find(t, res, keyLimit); r.Text != "1 container runs without a memory limit: web-app" {
		t.Fatalf("problem = %+v", r)
	}
}

func TestAuditDoesNotAnnounceWhenTheProblemReopens(t *testing.T) {
	src := &fakeSource{host: healthy(), containers: []memory.Container{kept("web-app")}}
	c := memory.New(src, auditSettings())
	c.Run(context.Background())
	src.containers = nil
	c.Run(context.Background())

	src.containers = []memory.Container{kept("worker")}
	if ev := limitEvents(c.Run(context.Background())); len(ev) != 0 {
		t.Fatalf("events = %+v, want none: the problem opens anew", ev)
	}
}

func TestAuditDoesNotReAnnounceAfterARestart(t *testing.T) {
	src := &fakeSource{host: healthy(), containers: []memory.Container{kept("web-app"), kept("worker")}}
	before := memory.New(src, auditSettings())
	before.Run(context.Background())
	saved, err := before.AuditState().Export()
	if err != nil {
		t.Fatalf("export: %v", err)
	}

	// tgshd restarts; queue-ok joined meanwhile.
	src.containers = append(src.containers, kept("queue-ok"))
	after := memory.New(src, auditSettings())
	if err := after.AuditState().Restore(saved); err != nil {
		t.Fatalf("restore: %v", err)
	}

	ev := limitEvents(after.Run(context.Background()))
	if len(ev) != 1 || ev[0].Key != keyLimit+"/queue-ok" {
		t.Fatalf("events = %+v, want only queue-ok", ev)
	}
}

func TestAuditCutsTheListOnlyPastOneMessage(t *testing.T) {
	const units = 4096

	short := &fakeSource{host: healthy()}
	for i := range 60 {
		short.containers = append(short.containers, kept(fmt.Sprintf("worker-%02d", i)))
	}
	r := find(t, memory.New(short, auditSettings()).Run(context.Background()), keyLimit)
	if strings.Contains(r.Text, "more") || !strings.HasSuffix(r.Text, "worker-59") {
		t.Fatalf("60 short names were cut: %q", r.Text)
	}

	long := &fakeSource{host: healthy(), services: []memory.Service{{Name: "web-app", Anon: 31 * gb}}}
	for i := range 200 {
		long.containers = append(long.containers, kept(fmt.Sprintf("worker-%03d-%s", i, strings.Repeat("x", 30))))
	}
	long.containers = append(long.containers, kept("web-app"))
	r = find(t, memory.New(long, auditSettings()).Run(context.Background()), keyLimit)

	if n := len(utf16.Encode([]rune(r.Text))); n > units-200 {
		t.Fatalf("text is %d units, leaves no room for the message prefix", n)
	}
	if !strings.HasPrefix(r.Text, "201 containers run without a memory limit: web-app 31 GB, worker-000-") {
		t.Fatalf("head = %.80q", r.Text)
	}
	shown := strings.Count(r.Text, "worker-")
	want := fmt.Sprintf(", … and %d more; top: web-app 31 GB", 200-shown)
	if shown == 0 || !strings.HasSuffix(r.Text, want) {
		t.Fatalf("tail = %q, want suffix %q", r.Text[len(r.Text)-60:], want)
	}
}

func TestAuditReadFailureIsACheckerErrorAndKeepsTheSet(t *testing.T) {
	src := &fakeSource{host: healthy(), containers: []memory.Container{kept("web-app")}}
	c := memory.New(src, auditSettings())
	c.Run(context.Background())

	src.ctrErr = errors.New("docker down")
	res := c.Run(context.Background())
	var ce health.CheckError
	if !errors.As(res.Err, &ce) || ce.Subject != "containers" {
		t.Fatalf("err = %v, want a containers CheckError", res.Err)
	}
	find(t, res, "memory/available")
	noLimitReading(t, res)

	src.ctrErr = nil
	src.containers = append(src.containers, kept("queue-ok"))
	if ev := limitEvents(c.Run(context.Background())); len(ev) != 1 || ev[0].Key != keyLimit+"/queue-ok" {
		t.Fatalf("events = %+v, want only queue-ok once the list is readable", ev)
	}
}

func TestAuditIsSilentWithoutAContainerRuntime(t *testing.T) {
	src := &fakeSource{host: healthy(), ctrErr: memory.ErrNoContainers}

	res := memory.New(src, auditSettings()).Run(context.Background())

	if res.Err != nil {
		t.Fatalf("err = %v, want none", res.Err)
	}
	noLimitReading(t, res)
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
	src := &fakeSource{host: healthy(), containers: []memory.Container{kept("worker")}}
	chk := memory.New(src, auditSettings())
	chk.Run(context.Background())

	// A late restore carries an older set; it must not forget worker.
	if err := chk.AuditState().Restore([]byte(`{"known":["db"]}`)); err != nil {
		t.Fatalf("restore: %v", err)
	}

	src.containers = []memory.Container{kept("worker"), kept("db")}
	if ev := limitEvents(chk.Run(context.Background())); len(ev) != 0 {
		t.Fatalf("events = %+v, want none", ev)
	}
}

// The PROBLEM opens only after threshold: a joiner before that (or on the
// opening run, whose PROBLEM names it) is not announced.
func TestAuditAnnouncesJoinersOnlyOnceTheProblemIsOpen(t *testing.T) {
	c := &clock{now: start0()}
	src := &fakeSource{host: healthy(), containers: []memory.Container{kept("web-app")}}
	chk := memory.New(src, thresholdSettings(c))

	// 10:00 opens the streak; 10:01 queue-ok joins; 10:05 reaches 5m and
	// worker joins on that run, named by the PROBLEM itself.
	joins := map[int]string{1: "queue-ok", 5: "worker"}
	for i := range 6 {
		c.now = start0().Add(time.Duration(i) * time.Minute)
		if name, ok := joins[i]; ok {
			src.containers = append(src.containers, kept(name))
		}
		if ev := limitEvents(chk.Run(context.Background())); len(ev) != 0 {
			t.Fatalf("run at +%dm: events = %+v, want none before the PROBLEM", i, ev)
		}
	}

	c.now = start0().Add(6 * time.Minute)
	src.containers = append(src.containers, kept("cache"))
	if ev := limitEvents(chk.Run(context.Background())); len(ev) != 1 || ev[0].Key != keyLimit+"/cache" {
		t.Fatalf("events = %+v, want cache once the PROBLEM is open", ev)
	}
}

func TestAuditSendsNoEventForAProblemThatNeverOpened(t *testing.T) {
	c := &clock{now: start0()}
	src := &fakeSource{host: healthy(), containers: []memory.Container{kept("web-app")}}
	chk := memory.New(src, thresholdSettings(c))
	chk.Run(context.Background())

	c.now = c.now.Add(time.Minute)
	src.containers = append(src.containers, kept("queue-ok"))
	chk.Run(context.Background())
	c.now = c.now.Add(time.Minute)
	src.containers = nil
	chk.Run(context.Background())

	// A new streak starts from zero.
	for i := range 3 {
		c.now = c.now.Add(time.Minute)
		src.containers = append(src.containers, kept(fmt.Sprintf("worker-%d", i)))
		if ev := limitEvents(chk.Run(context.Background())); len(ev) != 0 {
			t.Fatalf("events = %+v, want none: the new streak is under the threshold", ev)
		}
	}
}

// A restart keeps the open state: a joiner after it is announced.
func TestAuditKeepsTheOpenStateAcrossARestart(t *testing.T) {
	c := &clock{now: start0()}
	src := &fakeSource{host: healthy(), containers: []memory.Container{kept("web-app")}}
	before := memory.New(src, thresholdSettings(c))
	for i := range 6 {
		c.now = start0().Add(time.Duration(i) * time.Minute)
		before.Run(context.Background())
	}
	saved, err := before.AuditState().Export()
	if err != nil {
		t.Fatalf("export: %v", err)
	}

	after := memory.New(src, thresholdSettings(c))
	if err := after.AuditState().Restore(saved); err != nil {
		t.Fatalf("restore: %v", err)
	}
	c.now = start0().Add(8 * time.Minute)
	src.containers = append(src.containers, kept("queue-ok"))
	if ev := limitEvents(after.Run(context.Background())); len(ev) != 1 || ev[0].Key != keyLimit+"/queue-ok" {
		t.Fatalf("events = %+v, want queue-ok", ev)
	}
}

// Downtime adds at most 1.5 intervals to the streak, as in the engine: a
// streak 1m old before an hour's outage is not open after it.
func TestAuditCountsOnlyObservedTimeAcrossARestart(t *testing.T) {
	c := &clock{now: start0()}
	src := &fakeSource{host: healthy(), containers: []memory.Container{kept("web-app")}}
	before := memory.New(src, thresholdSettings(c))
	before.Run(context.Background())
	c.now = c.now.Add(time.Minute)
	before.Run(context.Background())
	saved, _ := before.AuditState().Export()

	after := memory.New(src, thresholdSettings(c))
	if err := after.AuditState().Restore(saved); err != nil {
		t.Fatalf("restore: %v", err)
	}
	c.now = c.now.Add(time.Hour)
	after.Run(context.Background()) // 1m + 1.5m observed
	c.now = c.now.Add(time.Minute)  // 3.5m observed
	src.containers = append(src.containers, kept("queue-ok"))
	if ev := limitEvents(after.Run(context.Background())); len(ev) != 0 {
		t.Fatalf("events = %+v, want none: only 3.5m observed", ev)
	}
}

// Minor 3: leaving and rejoining an open problem is a new join.
func TestAuditAnnouncesARejoinAsANewJoin(t *testing.T) {
	src := &fakeSource{host: healthy(), containers: []memory.Container{kept("web-app"), kept("worker")}}
	chk := memory.New(src, auditSettings())
	chk.Run(context.Background())
	src.containers = []memory.Container{kept("web-app")}
	chk.Run(context.Background())

	src.containers = []memory.Container{kept("web-app"), kept("worker")}
	if ev := limitEvents(chk.Run(context.Background())); len(ev) != 1 || ev[0].Key != keyLimit+"/worker" {
		t.Fatalf("events = %+v, want worker announced again", ev)
	}
}

// A container Docker is restarting (crash-loop backoff) stays counted, so
// it does not flap out of the set and back in as a joiner.
func TestAuditCountsAContainerDockerIsRestarting(t *testing.T) {
	looping := kept("worker")
	src := &fakeSource{host: healthy(), containers: []memory.Container{kept("web-app"), looping}}
	chk := memory.New(src, auditSettings())
	chk.Run(context.Background())

	looping.Running, looping.Restarting = false, true
	src.containers = []memory.Container{kept("web-app"), looping}
	res := chk.Run(context.Background())
	if r := find(t, res, keyLimit); r.Text != "2 containers run without a memory limit: web-app, worker" {
		t.Fatalf("problem = %+v, want worker still counted", r)
	}

	src.containers = []memory.Container{kept("web-app"), kept("worker")}
	if ev := limitEvents(chk.Run(context.Background())); len(ev) != 0 {
		t.Fatalf("events = %+v, want none", ev)
	}
}

func TestAuditCutsEveryNameWhenNoneFits(t *testing.T) {
	src := &fakeSource{host: healthy(), containers: []memory.Container{
		kept(strings.Repeat("x", 5000)), kept(strings.Repeat("y", 5000)),
	}}

	r := find(t, memory.New(src, auditSettings()).Run(context.Background()), keyLimit)

	if want := "2 containers run without a memory limit: … and 2 more"; r.Text != want {
		t.Fatalf("text = %.120q, want %q", r.Text, want)
	}
}
