package memory_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
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

// rigInterval is the check interval of a rig.
const rigInterval = time.Minute

// rig runs the check as the runner does: a run's readings are applied to a
// real engine at the tick time, and Settings.Open answers from that engine.
type rig struct {
	eng       *health.Engine
	src       *fakeSource
	chk       *memory.Check
	now       time.Time
	threshold time.Duration
}

// newRig is a rig over src with the given threshold, starting at start0.
func newRig(src *fakeSource, threshold time.Duration) *rig {
	r := &rig{eng: health.NewEngine(), src: src, now: start0(), threshold: threshold}
	r.chk = r.check()

	return r
}

// check builds a check that answers Open from the rig's engine; a restarted
// tgshd is a new check over the same (restored) engine.
func (r *rig) check() *memory.Check {
	s := auditSettings()
	s.Open = func(key string) (bool, string) {
		c, ok := r.eng.Condition(key)

		return ok && c.Open, c.LastText
	}

	return memory.New(r.src, s)
}

// run runs and applies one tick and returns the keys of the joiner events
// the engine emitted.
func (r *rig) run(t *testing.T) []string {
	t.Helper()
	res := r.chk.Run(context.Background())
	var keys []string
	for _, a := range r.eng.Apply(r.now, "memory", rigInterval, r.threshold, res.Readings) {
		if a.Kind == health.AlertEvent && strings.HasPrefix(a.Key, keyLimit+"/") {
			keys = append(keys, a.Key)
		}
	}
	r.now = r.now.Add(rigInterval)

	return keys
}

// drop runs one tick whose run overran: the runner drops its readings.
func (r *rig) drop() {
	r.chk.Run(context.Background())
	r.now = r.now.Add(rigInterval)
}

// open reports whether the engine has the limit PROBLEM open.
func (r *rig) open() bool {
	c, ok := r.eng.Condition(keyLimit)

	return ok && c.Open
}

// wantEvents fails unless got names exactly the joiners, in order.
func wantEvents(t *testing.T, what string, got []string, joiners ...string) {
	t.Helper()
	want := make([]string, len(joiners))
	for i, n := range joiners {
		want[i] = keyLimit + "/" + n
	}
	if !slices.Equal(got, want) {
		t.Fatalf("%s: events = %q, want %q", what, got, want)
	}
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
	want := "memory: 3 containers run without a memory limit: web-app 31 GB, worker 3 GB, cache 1.2 GB"
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

	if want := "memory: 1 container runs without a memory limit: web-app"; r.Text != want {
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

	want := "memory: 4 containers run without a memory limit: cache 2 GB, worker 2 GB, alpha, queue-ok"
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
	if r.Verdict != health.Good || r.Kind != health.State ||
		r.Text != "memory: every container runs with a memory limit" {
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

	if want := "memory: 2 containers run without a memory limit: runner, web-app"; r.Text != want {
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
	if !strings.HasPrefix(r.Text, "memory: 201 containers run without a memory limit: web-app 31 GB, worker-000-") {
		t.Fatalf("head = %.80q", r.Text)
	}
	shown := strings.Count(r.Text, "worker-")
	want := fmt.Sprintf(", … and %d more", 200-shown)
	if shown == 0 || !strings.HasSuffix(r.Text, want) {
		t.Fatalf("tail = %q, want suffix %q", r.Text[len(r.Text)-60:], want)
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

func TestAuditCutsEveryNameWhenNoneFits(t *testing.T) {
	src := &fakeSource{host: healthy(), containers: []memory.Container{
		kept(strings.Repeat("x", 5000)), kept(strings.Repeat("y", 5000)),
	}}

	r := find(t, memory.New(src, auditSettings()).Run(context.Background()), keyLimit)

	if want := "memory: 2 containers run without a memory limit: … and 2 more"; r.Text != want {
		t.Fatalf("text = %.120q, want %q", r.Text, want)
	}
}

// US3-5: a joiner is announced once; the open problem is not repeated.
func TestAuditAnnouncesAContainerJoiningAnOpenProblemOnce(t *testing.T) {
	src := &fakeSource{host: healthy(), containers: []memory.Container{kept("web-app")}}
	r := newRig(src, 0)
	wantEvents(t, "opening run", r.run(t))

	src.containers = []memory.Container{kept("web-app"), kept("queue-ok")}
	res := r.chk.Run(context.Background())
	ev := limitEvents(res)
	if len(ev) != 1 || ev[0].Key != keyLimit+"/queue-ok" || ev[0].Mode != health.ModeDefault ||
		ev[0].Text != "memory: container queue-ok runs without a memory limit" {
		t.Fatalf("events = %+v, want one for queue-ok", ev)
	}
	if rd := find(t, res, keyLimit); rd.Verdict != health.Bad ||
		rd.Text != "memory: 2 containers run without a memory limit: queue-ok, web-app" {
		t.Fatalf("problem = %+v", rd)
	}
	r.eng.Apply(r.now, "memory", rigInterval, 0, res.Readings)

	wantEvents(t, "next run", r.run(t))
}

func TestAuditDoesNotAnnounceALeaver(t *testing.T) {
	src := &fakeSource{host: healthy(), containers: []memory.Container{kept("web-app"), kept("worker")}}
	r := newRig(src, 0)
	r.run(t)

	src.containers = []memory.Container{kept("web-app")}
	res := r.chk.Run(context.Background())
	if ev := limitEvents(res); len(ev) != 0 {
		t.Fatalf("events = %+v, want none", ev)
	}
	if rd := find(t, res, keyLimit); rd.Text != "memory: 1 container runs without a memory limit: web-app" {
		t.Fatalf("problem = %+v", rd)
	}
}

func TestAuditDoesNotAnnounceWhenTheProblemReopens(t *testing.T) {
	src := &fakeSource{host: healthy(), containers: []memory.Container{kept("web-app")}}
	r := newRig(src, 0)
	r.run(t)
	src.containers = nil
	r.run(t)

	src.containers = []memory.Container{kept("worker")}
	wantEvents(t, "the problem opens anew", r.run(t))
}

// Leaving and rejoining an open problem is a new join.
func TestAuditAnnouncesARejoinAsANewJoin(t *testing.T) {
	src := &fakeSource{host: healthy(), containers: []memory.Container{kept("web-app"), kept("worker")}}
	r := newRig(src, 0)
	r.run(t)
	src.containers = []memory.Container{kept("web-app")}
	r.run(t)

	src.containers = []memory.Container{kept("web-app"), kept("worker")}
	wantEvents(t, "rejoin", r.run(t), "worker")
}

// A container Docker is restarting (crash-loop backoff) stays counted, so
// it does not flap out of the set and back in as a joiner.
func TestAuditCountsAContainerDockerIsRestarting(t *testing.T) {
	looping := kept("worker")
	src := &fakeSource{host: healthy(), containers: []memory.Container{kept("web-app"), looping}}
	r := newRig(src, 0)
	r.run(t)

	looping.Running, looping.Restarting = false, true
	src.containers = []memory.Container{kept("web-app"), looping}
	res := r.chk.Run(context.Background())
	if rd := find(t, res, keyLimit); rd.Text != "memory: 2 containers run without a memory limit: web-app, worker" {
		t.Fatalf("problem = %+v, want worker still counted", rd)
	}
	r.eng.Apply(r.now, "memory", rigInterval, 0, res.Readings)

	src.containers = []memory.Container{kept("web-app"), kept("worker")}
	wantEvents(t, "back to running", r.run(t))
}

func TestAuditReadFailureIsACheckerErrorAndKeepsTheSet(t *testing.T) {
	src := &fakeSource{host: healthy(), containers: []memory.Container{kept("web-app")}}
	r := newRig(src, 0)
	r.run(t)

	src.ctrErr = errors.New("docker down")
	res := r.chk.Run(context.Background())
	var ce health.CheckError
	if !errors.As(res.Err, &ce) || ce.Subject != "containers" {
		t.Fatalf("err = %v, want a containers CheckError", res.Err)
	}
	find(t, res, "memory/available")
	noLimitReading(t, res)
	r.eng.Apply(r.now, "memory", rigInterval, 0, res.Readings)

	src.ctrErr = nil
	src.containers = append(src.containers, kept("queue-ok"))
	wantEvents(t, "readable again", r.run(t), "queue-ok")
}

// The engine opens the PROBLEM after threshold: a joiner before that (or on
// the opening run, whose PROBLEM names it) is not announced.
func TestAuditAnnouncesJoinersOnlyOnceTheProblemIsOpen(t *testing.T) {
	src := &fakeSource{host: healthy(), containers: []memory.Container{kept("web-app")}}
	r := newRig(src, 5*time.Minute)

	// 10:00 opens the streak; 10:01 queue-ok joins; 10:05 reaches 5m and
	// worker joins on that run, named by the PROBLEM itself.
	joins := map[int]string{1: "queue-ok", 5: "worker"}
	for i := range 6 {
		if name, ok := joins[i]; ok {
			src.containers = append(src.containers, kept(name))
		}
		wantEvents(t, fmt.Sprintf("run at +%dm", i), r.run(t))
	}
	if !r.open() {
		t.Fatal("the engine has not opened the PROBLEM at +5m")
	}

	src.containers = append(src.containers, kept("cache"))
	wantEvents(t, "once open", r.run(t), "cache")
}

// The audit asks the engine, so its own clock cannot make it
// disagree on the opening run. Here the check's clock lags the tick on the
// first run (a slow Docker read), so a mirror built on it would stay closed
// one run longer than the engine.
func TestAuditFollowsTheEngineOnTheOpeningRun(t *testing.T) {
	src := &fakeSource{host: healthy(), containers: []memory.Container{kept("web-app")}}
	r := newRig(src, 5*time.Minute)
	s := auditSettings()
	s.Open = func(key string) (bool, string) {
		c, ok := r.eng.Condition(key)

		return ok && c.Open, c.LastText
	}
	lag := 300 * time.Millisecond
	s.Now = func() time.Time { return r.now.Add(lag) }
	r.chk = memory.New(src, s)

	for range 6 {
		r.run(t)
		lag = 0
	}
	if !r.open() {
		t.Fatal("the engine has not opened the PROBLEM at +5m")
	}

	src.containers = append(src.containers, kept("queue-ok"))
	wantEvents(t, "first run after the engine opened", r.run(t), "queue-ok")
}

// A run the runner drops (an overrun) emitted nothing, so its joiner
// is announced by the next applied run, also after more dropped runs in a
// row than the audit remembers.
func TestAuditAnnouncesAJoinerSeenOnlyByADroppedRun(t *testing.T) {
	for _, drops := range []int{1, 2, 12} {
		t.Run(fmt.Sprintf("%d dropped", drops), func(t *testing.T) {
			src := &fakeSource{host: healthy(), containers: []memory.Container{kept("web-app")}}
			r := newRig(src, 0)
			r.run(t)

			src.containers = append(src.containers, kept("queue-ok"))
			for range drops {
				r.drop()
			}
			wantEvents(t, "first applied run", r.run(t), "queue-ok")
			wantEvents(t, "next run", r.run(t))
		})
	}
}

// /clear ends the engine's condition; no joiner is announced until
// the PROBLEM opens again, and the re-raised PROBLEM names the joiners.
func TestAuditSendsNoJoinerEventAfterAClear(t *testing.T) {
	src := &fakeSource{host: healthy(), containers: []memory.Container{kept("web-app")}}
	r := newRig(src, 5*time.Minute)
	for range 6 {
		r.run(t)
	}
	if !r.open() || !r.eng.Clear(keyLimit) {
		t.Fatal("want an open PROBLEM to clear")
	}

	for i := range 6 {
		src.containers = append(src.containers, kept(fmt.Sprintf("worker-%d", i)))
		wantEvents(t, fmt.Sprintf("run %d after the clear", i), r.run(t))
	}
	if !r.open() {
		t.Fatal("the PROBLEM has not opened again")
	}
	src.containers = append(src.containers, kept("cache"))
	wantEvents(t, "once open again", r.run(t), "cache")
}

func TestAuditSendsNoEventForAProblemThatNeverOpened(t *testing.T) {
	src := &fakeSource{host: healthy(), containers: []memory.Container{kept("web-app")}}
	r := newRig(src, 5*time.Minute)
	r.run(t)
	src.containers = append(src.containers, kept("queue-ok"))
	r.run(t)
	src.containers = nil
	r.run(t)

	// A new streak starts from zero.
	for i := range 3 {
		src.containers = append(src.containers, kept(fmt.Sprintf("worker-%d", i)))
		wantEvents(t, "the new streak is under the threshold", r.run(t))
	}
}

// A restart (a new check over the restored engine and kv memory.audit)
// neither announces the set again nor misses a joiner after it, even one
// announced by the last run before the stop.
func TestAuditDoesNotReAnnounceAfterARestart(t *testing.T) {
	src := &fakeSource{host: healthy(), containers: []memory.Container{kept("web-app")}}
	r := newRig(src, 0)
	r.run(t)
	src.containers = append(src.containers, kept("worker"))
	wantEvents(t, "before the stop", r.run(t), "worker")
	saved, err := r.chk.AuditState().Export()
	if err != nil {
		t.Fatalf("export: %v", err)
	}

	r.chk = r.check()
	if err := r.chk.AuditState().Restore(saved); err != nil {
		t.Fatalf("restore: %v", err)
	}
	src.containers = append(src.containers, kept("queue-ok"))
	wantEvents(t, "after the restart", r.run(t), "queue-ok")
}

// The previous kv memory.audit carried a mirrored streak; only its known set
// is read.
func TestAuditRestoresThePreviousFormat(t *testing.T) {
	src := &fakeSource{host: healthy(), containers: []memory.Container{kept("web-app"), kept("worker")}}
	r := newRig(src, 0)
	r.run(t)

	r.chk = r.check()
	old := `{"known":["web-app","worker"],"bad_for":300000000000,"last":"2026-01-02T10:00:00Z","open":true}`
	if err := r.chk.AuditState().Restore([]byte(old)); err != nil {
		t.Fatalf("restore: %v", err)
	}
	src.containers = append(src.containers, kept("queue-ok"))
	wantEvents(t, "after the restart", r.run(t), "queue-ok")
}

// With an open PROBLEM but nothing to compare against (its kv entry was
// lost), the counted set is taken silently rather than announced whole.
func TestAuditDoesNotAnnounceTheWholeSetWithoutAKnownSet(t *testing.T) {
	src := &fakeSource{host: healthy(), containers: []memory.Container{kept("web-app"), kept("worker")}}
	r := newRig(src, 0)
	r.run(t)

	r.chk = r.check()
	wantEvents(t, "first run without memory.audit", r.run(t))
	src.containers = append(src.containers, kept("queue-ok"))
	wantEvents(t, "a later joiner", r.run(t), "queue-ok")
}

// A store that loads late (unreadable at start) restores kv memory.audit and
// merges the stored conditions after runs were applied; the engine keeps its
// own young streak, and the audit follows the engine: nothing is
// announced before that streak opens, and the PROBLEM then names everyone.
func TestAuditFollowsTheEngineAfterALateLoad(t *testing.T) {
	src := &fakeSource{host: healthy(), containers: []memory.Container{kept("worker")}}
	r := newRig(src, 5*time.Minute)
	r.run(t)

	if err := r.chk.AuditState().Restore([]byte(`{"known":["db"]}`)); err != nil {
		t.Fatalf("restore: %v", err)
	}
	r.eng.Merge([]health.Condition{{Key: keyLimit, Check: "memory", Open: true, LastText: "stored text"}})

	src.containers = []memory.Container{kept("worker"), kept("db"), kept("queue-ok")}
	for i := range 5 {
		wantEvents(t, fmt.Sprintf("run %d after the late load", i), r.run(t))
	}
	if !r.open() {
		t.Fatal("the PROBLEM has not opened")
	}
	src.containers = append(src.containers, kept("cache"))
	wantEvents(t, "once open", r.run(t), "cache")
}

// A run that counts nothing and is dropped leaves the open PROBLEM as it
// was, so the joiner of the next applied run is still announced.
func TestAuditAnnouncesAJoinerAfterADroppedGoodRun(t *testing.T) {
	src := &fakeSource{host: healthy(), containers: []memory.Container{kept("web-app")}}
	r := newRig(src, 0)
	r.run(t)

	src.containers = nil
	r.drop()
	src.containers = []memory.Container{kept("web-app"), kept("queue-ok")}
	wantEvents(t, "first applied run", r.run(t), "queue-ok")
	wantEvents(t, "next run", r.run(t))
}

// Past the cut, two sets can share one text: a joiner hidden behind
// "… and K more" is announced once, not on every run the text stays.
func TestAuditAnnouncesAHiddenJoinerOnceWhenCutTextsCollide(t *testing.T) {
	shown := make([]memory.Container, 0, 40)
	for i := range 40 {
		shown = append(shown, kept(fmt.Sprintf("a%02d-%s", i, strings.Repeat("x", 100))))
	}
	src := &fakeSource{host: healthy(), containers: append(slices.Clone(shown), kept("z1"), kept("z2"))}
	r := newRig(src, 0)
	r.run(t)

	// z1 leaves and z3 joins: the count and the shown names stay the same.
	src.containers = append(slices.Clone(shown), kept("z2"), kept("z3"))
	if c, _ := r.eng.Condition(keyLimit); !strings.Contains(c.LastText, "more") {
		t.Fatalf("text %.80q… is not cut", c.LastText)
	}
	wantEvents(t, "the join", r.run(t), "z3")
	for i := range 3 {
		wantEvents(t, fmt.Sprintf("run %d after the join", i), r.run(t))
	}
}
