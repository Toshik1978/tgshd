package memguard

import (
	"testing"
	"time"
)

const gib = uint64(1) << 30

// t0 is the time of the first sample in every test.
func t0() time.Time { return time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC) }

func testConfig() Config {
	return Config{
		CulpritWindow:    10 * time.Minute,
		CulpritMinGrowth: gib,
		NeverTouch:       []string{"db", "backup.service"},
		SelfCgroup:       "system.slice/tgshd.service",
	}
}

func container(name string, anon uint64) Sample {
	return Sample{Kind: KindContainer, Name: name, CgroupPath: "system.slice/docker-" + name + "id.scope", Anon: anon}
}

func unit(name string, anon uint64) Sample {
	return Sample{Kind: KindUnit, Name: name, CgroupPath: "system.slice/" + name, Anon: anon}
}

// feed observes one sample set per ServiceInterval, starting at t0, and
// returns the time of the last observation.
func feed(g *Guard, steps [][]Sample) time.Time {
	at := t0()
	for i, s := range steps {
		at = t0().Add(time.Duration(i) * ServiceInterval)
		g.ObserveServices(at, s)
	}

	return at
}

func TestServiceIdentity(t *testing.T) {
	cfg := testConfig()
	c := cfg.Service(KindContainer, "web-app", "system.slice/docker-abc.scope")
	if c.ID != "container:web-app" || c.Kind != KindContainer || c.Name != "web-app" ||
		c.CgroupPath != "system.slice/docker-abc.scope" || !c.Eligible || c.Protected {
		t.Fatalf("container service %+v", c)
	}
	u := cfg.Service(KindUnit, "worker.service", "system.slice/worker.service")
	if u.ID != "unit:worker.service" || !u.Eligible || u.Protected {
		t.Fatalf("unit service %+v", u)
	}
}

func TestEligibility(t *testing.T) {
	cases := []struct {
		name      string
		kind      Kind
		svc       string
		path      string
		eligible  bool
		protected bool
	}{
		{"plain unit", KindUnit, "worker.service", "system.slice/worker.service", true, false},
		{"nested unit", KindUnit, "job.service", "system.slice/apps.slice/job.service", true, false},
		{"container", KindContainer, "web-app", "system.slice/docker-abc.scope", true, false},
		{"never-touch container", KindContainer, "db", "system.slice/docker-def.scope", false, true},
		{"never-touch unit", KindUnit, "backup.service", "system.slice/backup.service", false, true},
		{"never-touch unit by its short name", KindUnit, "db.service", "system.slice/db.service", false, true},
		{"nested unit by its short name", KindUnit, "db.service", "system.slice/apps.slice/db.service", false, true},
		{"short name matches the whole name", KindUnit, "db2.service", "system.slice/db2.service", true, false},
		{"short name is for units only", KindContainer, "db.service", "system.slice/docker-fed.scope", true, false},
		{"never-touch glob container", KindContainer, "db-main", "system.slice/docker-a1.scope", false, true},
		{"never-touch glob unit", KindUnit, "db-main.service", "system.slice/db-main.service", false, true},
		{"never-touch glob misses", KindContainer, "dbx", "system.slice/docker-a2.scope", true, false},
		{"own unit", KindUnit, "tgshd.service", "system.slice/tgshd.service", false, false},
		{"own cgroup with leading slash", KindUnit, "tgshd.service", "/system.slice/tgshd.service", false, false},
		{"docker", KindUnit, "docker.service", "system.slice/docker.service", false, false},
		{"containerd", KindUnit, "containerd.service", "system.slice/containerd.service", false, false},
		{"dbus", KindUnit, "dbus.service", "system.slice/dbus.service", false, false},
		{"ssh", KindUnit, "ssh.service", "system.slice/ssh.service", false, false},
		{"sshd", KindUnit, "sshd.service", "system.slice/sshd.service", false, false},
		{
			"systemd-journald",
			KindUnit,
			"systemd-journald.service",
			"system.slice/systemd-journald.service",
			false,
			false,
		},
		{"user slice", KindUnit, "user@1000.service", "user.slice/user-1000.slice/user@1000.service", false, false},
		{"init scope", KindUnit, "init.scope", "init.scope", false, false},
		{"slice is not a service", KindUnit, "apps.slice", "system.slice/apps.slice", false, false},
		{"system slice itself", KindUnit, "system.slice", "system.slice", false, false},
		{"core beats never-touch", KindUnit, "dbus.service", "system.slice/dbus.service", false, false},
	}
	cfg := testConfig()
	cfg.NeverTouch = append(cfg.NeverTouch, "dbus.service", "db-*")
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := cfg.Service(tc.kind, tc.svc, tc.path)
			if s.Eligible != tc.eligible || s.Protected != tc.protected {
				t.Fatalf("Service(%s) eligible=%v protected=%v, want %v %v",
					tc.path, s.Eligible, s.Protected, tc.eligible, tc.protected)
			}
		})
	}
}

func TestOwnCgroupNestedInsideService(t *testing.T) {
	cfg := testConfig()
	cfg.SelfCgroup = "system.slice/tgshd.service/main"
	if s := cfg.Service(KindUnit, "tgshd.service", "system.slice/tgshd.service"); s.Eligible {
		t.Fatalf("the unit holding tgshd is eligible: %+v", s)
	}
	s := cfg.Service(KindUnit, "tgshd.service-extra.service", "system.slice/tgshd.service-extra.service")
	if !s.Eligible {
		t.Fatalf("a unit sharing a name prefix is not eligible: %+v", s)
	}
}

func TestRingDepth(t *testing.T) {
	g := New(testConfig())
	if g.depth != 121 {
		t.Fatalf("depth %d, want 121 (10m / 5s, both ends of the window)", g.depth)
	}
	if d := New(Config{CulpritWindow: time.Second}).depth; d != 1 {
		t.Fatalf("depth of a window below the interval %d, want 1", d)
	}
}

// US1-2: the largest container is stable, a smaller one grows: the growing
// one is the culprit.
func TestCulpritStableLargeVsGrowingSmall(t *testing.T) {
	g := New(testConfig())
	steps := make([][]Sample, 0, 60)
	for i := range 60 {
		steps = append(steps, []Sample{
			container("big", 30*gib),
			container("web-app", 2*gib+uint64(i)*gib/10),
		})
	}
	at := feed(g, steps)

	c, ok := g.Culprit(at)
	if !ok {
		t.Fatal("no culprit")
	}
	if c.Service.ID != "container:web-app" {
		t.Fatalf("culprit %s, want container:web-app", c.Service.ID)
	}
	if c.Before != 2*gib || c.Size != 2*gib+59*gib/10 || c.Growth != 59*gib/10 {
		t.Fatalf("culprit figures %+v", c)
	}
	if c.Window != 59*ServiceInterval {
		t.Fatalf("window %s, want %s", c.Window, 59*ServiceInterval)
	}
}

// A sawtooth cache grows and is flushed; growth is measured from the minimum
// in the window, so a big recent swing counts but the sample before a flush
// is no baseline.
func TestCulpritSawtooth(t *testing.T) {
	g := New(testConfig())
	steps := make([][]Sample, 0, 40)
	for i := range 40 {
		// 4..8 GiB sawtooth of period 10 samples, plus a steady grower.
		saw := 4*gib + uint64(i%10)*gib*4/10
		steps = append(steps, []Sample{
			container("cache", saw),
			container("web-app", 5*gib+uint64(i)*gib/20),
		})
	}
	at := feed(g, steps)

	c, ok := g.Culprit(at)
	if !ok {
		t.Fatal("no culprit")
	}
	// cache: the last sample is 4 + 9·0.4 = 7.6 GiB over a 4 GiB minimum, 3.6 GiB;
	// web-app grew 39/20 = 1.95 GiB. The cache's current swing is larger.
	if c.Service.ID != "container:cache" || c.Growth != 9*gib*4/10 {
		t.Fatalf("culprit %+v", c)
	}

	// Right after a flush the cache is at its minimum: no growth, the steady
	// grower is the culprit.
	at = at.Add(ServiceInterval)
	g.ObserveServices(at, []Sample{container("cache", 4*gib), container("web-app", 5*gib+40*gib/20)})
	c, ok = g.Culprit(at)
	if !ok || c.Service.ID != "container:web-app" || c.Growth != 2*gib {
		t.Fatalf("after flush culprit %+v ok=%v", c, ok)
	}
	// The cache's own growth starts from the flushed sample: a pre-flush peak
	// is never its minimum.
	cache, ok := g.entries[serviceKey{kind: KindContainer, name: "cache"}].growth(at, g.cfg.CulpritWindow)
	if !ok || cache.Growth != 0 || cache.Before != 4*gib || cache.Size != 4*gib {
		t.Fatalf("after flush the cache's growth %+v ok=%v, want none from 4 GiB", cache, ok)
	}
}

func TestCulpritMinimumOutsideWindowIsForgotten(t *testing.T) {
	cfg := testConfig()
	cfg.CulpritWindow = time.Minute // 12 samples
	g := New(cfg)
	steps := make([][]Sample, 0, 21)
	steps = append(steps, []Sample{container("web-app", 1*gib)})
	for range 20 {
		steps = append(steps, []Sample{container("web-app", 10*gib)})
	}
	at := feed(g, steps)
	if c, ok := g.Culprit(at); ok {
		t.Fatalf("growth older than the window chose %+v", c)
	}
}

func TestCulpritSamplesOlderThanWindowIgnored(t *testing.T) {
	cfg := testConfig()
	cfg.CulpritWindow = time.Minute
	g := New(cfg)
	g.ObserveServices(t0(), []Sample{container("web-app", 1*gib)})
	// The sampler stalled: the next sample comes long after the window.
	at := t0().Add(10 * time.Minute)
	g.ObserveServices(at, []Sample{container("web-app", 5*gib)})
	if c, ok := g.Culprit(at); ok {
		t.Fatalf("a sample older than the window counted: %+v", c)
	}
}

func TestCulpritBelowMinGrowth(t *testing.T) {
	g := New(testConfig())
	at := feed(g, [][]Sample{
		{container("web-app", 2*gib)},
		{container("web-app", 3*gib-1)},
	})
	if c, ok := g.Culprit(at); ok {
		t.Fatalf("growth below culprit_min_growth chose %+v", c)
	}
	at = at.Add(ServiceInterval)
	g.ObserveServices(at, []Sample{container("web-app", 3*gib)})
	if c, ok := g.Culprit(at); !ok || c.Growth != gib {
		t.Fatalf("growth equal to culprit_min_growth: %+v ok=%v", c, ok)
	}
}

// TestCulpritNeedsGrowthWithoutMinGrowth: with culprit_min_growth unset the
// model still never blames a service that did not grow (FR-003).
func TestCulpritNeedsGrowthWithoutMinGrowth(t *testing.T) {
	g := New(Config{CulpritWindow: 10 * time.Minute})
	at := feed(g, [][]Sample{
		{container("web-app", 2*gib), container("cache", 30*gib)},
		{container("web-app", 2*gib), container("cache", 30*gib)},
	})
	if c, ok := g.Culprit(at); ok {
		t.Fatalf("a flat service is the culprit: %+v", c)
	}
}

func TestCulpritTiesGoToLargerSize(t *testing.T) {
	g := New(testConfig())
	at := feed(g, [][]Sample{
		{container("small", 1*gib), container("large", 10*gib), container("also-large", 10*gib)},
		{container("small", 3*gib), container("large", 12*gib), container("also-large", 12*gib)},
	})
	c, ok := g.Culprit(at)
	if !ok {
		t.Fatal("no culprit")
	}
	// large and also-large tie on growth and size: the lower ID wins so the
	// choice is deterministic.
	if c.Service.ID != "container:also-large" || c.Size != 12*gib {
		t.Fatalf("culprit %+v", c)
	}
	for range 20 {
		if again, _ := g.Culprit(at); again.Service.ID != c.Service.ID {
			t.Fatalf("culprit changed between calls: %s then %s", c.Service.ID, again.Service.ID)
		}
	}
}

// A protected (never-touch) service can still be the culprit, so the episode
// can report that tgshd did not act on it.
func TestCulpritProtected(t *testing.T) {
	g := New(testConfig())
	at := feed(g, [][]Sample{
		{container("db", 12*gib), container("web-app", 5*gib)},
		{container("db", 40*gib), container("web-app", 6*gib)},
	})
	c, ok := g.Culprit(at)
	if !ok || c.Service.ID != "container:db" || !c.Service.Protected || c.Service.Eligible {
		t.Fatalf("culprit %+v ok=%v", c, ok)
	}
}

func TestCulpritNeverCoreOrSelf(t *testing.T) {
	g := New(testConfig())
	core := func(anon uint64) []Sample {
		return []Sample{
			unit("docker.service", anon),
			unit("containerd.service", anon),
			unit("dbus.service", anon),
			unit("ssh.service", anon),
			unit("sshd.service", anon),
			unit("systemd-journald.service", anon),
			unit("tgshd.service", anon),
			{
				Kind:       KindUnit,
				Name:       "user@1000.service",
				CgroupPath: "user.slice/user-1000.slice/user@1000.service",
				Anon:       anon,
			},
			{Kind: KindUnit, Name: "init.scope", CgroupPath: "init.scope", Anon: anon},
			{Kind: KindUnit, Name: "apps.slice", CgroupPath: "system.slice/apps.slice", Anon: anon},
			unit("worker.service", anon/20),
		}
	}
	at := feed(g, [][]Sample{core(1 * gib), core(40 * gib)})
	c, ok := g.Culprit(at)
	if !ok || c.Service.ID != "unit:worker.service" {
		t.Fatalf("culprit %+v ok=%v, want only the ordinary unit", c, ok)
	}
	for key, e := range g.entries {
		if key.name != "worker.service" && e.history.n != 0 {
			t.Fatalf("kept history for %s, want only the eligible one", key.name)
		}
	}
}

func TestCulpritRemovedServiceDropsRing(t *testing.T) {
	g := New(testConfig())
	at := feed(g, [][]Sample{
		{container("web-app", 1*gib)},
		{},
		{container("web-app", 5*gib)},
	})
	if c, ok := g.Culprit(at); ok {
		t.Fatalf("a service that came back without new history is the culprit: %+v", c)
	}
	at = at.Add(ServiceInterval)
	g.ObserveServices(at, []Sample{container("web-app", 7*gib)})
	if c, ok := g.Culprit(at); !ok || c.Before != 5*gib || c.Growth != 2*gib {
		t.Fatalf("culprit from new history %+v ok=%v", c, ok)
	}
}

func TestCulpritRenamedServiceDropsRing(t *testing.T) {
	g := New(testConfig())
	path := "system.slice/docker-abc.scope"
	at := feed(g, [][]Sample{
		{{Kind: KindContainer, Name: "web-app", CgroupPath: path, Anon: 1 * gib}},
		{{Kind: KindContainer, Name: "web-app-2", CgroupPath: path, Anon: 9 * gib}},
	})
	if c, ok := g.Culprit(at); ok {
		t.Fatalf("a renamed service inherited history: %+v", c)
	}
	if _, kept := g.entries[serviceKey{KindContainer, "web-app"}]; kept {
		t.Fatal("the old name's ring was not dropped")
	}
}

// A container recreated under the same name lives in a new cgroup; its old
// history does not count.
func TestCulpritRecreatedServiceDropsRing(t *testing.T) {
	g := New(testConfig())
	at := feed(g, [][]Sample{
		{{Kind: KindContainer, Name: "web-app", CgroupPath: "system.slice/docker-old.scope", Anon: 1 * gib}},
		{{Kind: KindContainer, Name: "web-app", CgroupPath: "system.slice/docker-new.scope", Anon: 9 * gib}},
	})
	if c, ok := g.Culprit(at); ok {
		t.Fatalf("a recreated container inherited history: %+v", c)
	}
	at = at.Add(ServiceInterval)
	g.ObserveServices(
		at,
		[]Sample{{Kind: KindContainer, Name: "web-app", CgroupPath: "system.slice/docker-new.scope", Anon: 11 * gib}},
	)
	c, ok := g.Culprit(at)
	if !ok || c.Service.CgroupPath != "system.slice/docker-new.scope" || c.Growth != 2*gib {
		t.Fatalf("culprit %+v ok=%v", c, ok)
	}
}

func TestCulpritNone(t *testing.T) {
	g := New(testConfig())
	if c, ok := g.Culprit(t0()); ok {
		t.Fatalf("culprit without samples: %+v", c)
	}
}

func TestObserveAndCulpritDoNotAllocate(t *testing.T) {
	g := New(testConfig())
	samples := []Sample{
		container("big", 30*gib),
		container("web-app", 2*gib),
		unit("worker.service", gib),
		unit("docker.service", gib),
		{
			Kind:       KindUnit,
			Name:       "user@1000.service",
			CgroupPath: "user.slice/user-1000.slice/user@1000.service",
			Anon:       gib,
		},
	}
	at := t0()
	g.ObserveServices(at, samples)
	allocs := testing.AllocsPerRun(200, func() {
		at = at.Add(ServiceInterval)
		samples[1].Anon += gib / 10
		g.ObserveServices(at, samples)
		g.Culprit(at)
	})
	if allocs != 0 {
		t.Fatalf("steady state allocates %v times per sample", allocs)
	}
}
