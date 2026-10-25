package memguard

import (
	"testing"
	"time"
)

const mib = uint64(1) << 20

// leak is a growth rate of 20 MiB/s, about 1.2 GB a minute.
const leak = 20 * mib

func episodeConfig(mode Mode) Config {
	cfg := testConfig()
	cfg.Mode = mode
	cfg.DangerAvailablePercent = 5
	cfg.DangerPressurePercent = 20
	cfg.DangerFor = 30 * time.Second
	cfg.RepeatWindow = 6 * time.Hour
	cfg.RecoveryWindow = 2 * time.Minute

	return cfg
}

// sim drives a guard one host sample a second and one service sample every
// ServiceInterval, the way the guard loop does. A restart resets the service's
// memory to its start size; a stop removes the service.
type sim struct {
	g         *Guard
	at        time.Time
	tick      int
	svcs      []Sample
	start     map[string]uint64
	decisions []Decision
}

func newSim(cfg Config, svcs ...Sample) *sim {
	s := &sim{g: New(cfg), at: t0(), svcs: svcs, start: make(map[string]uint64)}
	for _, sv := range svcs {
		s.start[sv.Name] = sv.Anon
	}

	return s
}

// run advances d with the host at avail % available and stall % full avg10
// pressure while the named services grow by rates bytes a second. It returns
// the decisions taken during d.
func (s *sim) run(d time.Duration, avail, stall float64, rates map[string]uint64) []Decision {
	var got []Decision
	for range int(d / time.Second) {
		for i := range s.svcs {
			s.svcs[i].Anon += rates[s.svcs[i].Name]
		}
		if s.tick%int(ServiceInterval/time.Second) == 0 {
			s.g.ObserveServices(s.at, s.svcs)
		}
		if dec, ok := s.g.ObserveHost(Host{At: s.at, AvailablePct: avail, FullAvg10: stall}); ok {
			got = append(got, dec)
			s.apply(dec)
		}
		s.at = s.at.Add(time.Second)
		s.tick++
	}
	s.decisions = append(s.decisions, got...)

	return got
}

func (s *sim) apply(dec Decision) {
	if !dec.Act {
		return
	}
	s.g.ActionDone(s.at, dec.Record, true)
	for i := range s.svcs {
		if s.svcs[i].Name != dec.Outcome.Service.Name {
			continue
		}
		if dec.Outcome.Action == ActionStop {
			s.svcs = append(s.svcs[:i], s.svcs[i+1:]...)
		} else {
			s.svcs[i].Anon = s.start[s.svcs[i].Name]
		}

		return
	}
}

func webLeak() map[string]uint64 { return map[string]uint64{"web-app": leak} }

func standardServices() []Sample {
	return []Sample{container("web-app", 2*gib), container("cache", 30*gib), unit("worker.service", gib)}
}

func one(t *testing.T, got []Decision) Decision {
	t.Helper()
	if len(got) != 1 {
		t.Fatalf("decisions = %+v, want exactly one", got)
	}

	return got[0]
}

func none(t *testing.T, got []Decision) {
	t.Helper()
	if len(got) != 0 {
		t.Fatalf("decisions = %+v, want none", got)
	}
}

// TestUS1_1RestartsTheGrowingService covers US1-1 (and US1-2: the large
// stable cache is left alone).
func TestUS1_1RestartsTheGrowingService(t *testing.T) {
	s := newSim(episodeConfig(ModeAct), standardServices()...)
	none(t, s.run(5*time.Minute, 40, 0, webLeak()))
	dangerAt := s.at

	dec := one(t, s.run(time.Minute, 3, 35, webLeak()))
	o := dec.Outcome
	if !dec.Act || o.Kind != OutcomeRestarted || o.Action != ActionRestart || o.Service.ID != "container:web-app" {
		t.Fatalf("decision %+v, want restart of web-app", dec)
	}
	if want := dangerAt.Add(30 * time.Second); !o.At.Equal(want) {
		t.Fatalf("acted at %v, want after danger_for at %v", o.At, want)
	}
	if o.Growth < 5*gib || o.Size != o.Before+o.Growth || o.AvailablePct != 3 || o.FullAvg10 != 35 {
		t.Fatalf("outcome %+v, want growth ≥ 5 GiB and the host readings", o)
	}
	if !o.LastAction.IsZero() {
		t.Fatalf("first restart has a previous action at %v", o.LastAction)
	}
	if ep := s.g.Episode(); ep.State != StateActed || !ep.Since.Equal(dangerAt) || !ep.ActedAt.Equal(o.At) ||
		ep.Action != ActionRestart || ep.Culprit.Service.ID != "container:web-app" {
		t.Fatalf("episode %+v", ep)
	}

	// The restart freed memory: the episode ends when the recovery window
	// has passed, with no further outcome.
	none(t, s.run(3*time.Minute, 40, 0, nil))
	if ep := s.g.Episode(); ep.State != StateIdle {
		t.Fatalf("episode %+v, want idle", ep)
	}
}

// TestUS1_3DangerMustHold covers US1-3 and FR-002: a single reading, readings
// that never hold for danger_for, and either condition alone never act.
func TestUS1_3DangerMustHold(t *testing.T) {
	cases := []struct {
		name  string
		steps func(s *sim)
	}{
		{"single reading", func(s *sim) {
			s.run(time.Second, 3, 35, webLeak())
			s.run(10*time.Minute, 40, 0, webLeak())
		}},
		{"flapping every 20 s", func(s *sim) {
			for range 20 {
				s.run(20*time.Second, 3, 35, webLeak())
				s.run(time.Second, 6, 35, webLeak())
			}
		}},
		{"pressure with memory left", func(s *sim) { s.run(10*time.Minute, 40, 90, webLeak()) }},
		{"low memory without pressure", func(s *sim) { s.run(10*time.Minute, 2, 10, webLeak()) }},
		{"available exactly at the floor", func(s *sim) { s.run(10*time.Minute, 5, 90, webLeak()) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newSim(episodeConfig(ModeAct), standardServices()...)
			s.run(5*time.Minute, 40, 0, webLeak())
			tc.steps(s)
			none(t, s.decisions)
			if ep := s.g.Episode(); ep.State != StateIdle {
				t.Fatalf("episode %+v, want idle", ep)
			}
		})
	}

	t.Run("pressure exactly at the danger level", func(t *testing.T) {
		s := newSim(episodeConfig(ModeAct), standardServices()...)
		s.run(5*time.Minute, 40, 0, webLeak())
		if dec := one(t, s.run(time.Minute, 4.9, 20, webLeak())); !dec.Act {
			t.Fatalf("decision %+v, want an action", dec)
		}
	})
}

// TestUS1_4RepeatLeakerIsStopped covers US1-4 and FR-005.
func TestUS1_4RepeatLeakerIsStopped(t *testing.T) {
	s := newSim(episodeConfig(ModeAct), standardServices()...)
	s.run(5*time.Minute, 40, 0, webLeak())
	first := one(t, s.run(time.Minute, 3, 35, webLeak()))
	s.run(time.Hour, 40, 0, nil)

	// It leaks again two hours after the restart.
	s.run(5*time.Minute, 40, 0, webLeak())
	dec := one(t, s.run(time.Minute, 3, 35, webLeak()))
	o := dec.Outcome
	if !dec.Act || o.Kind != OutcomeStopped || o.Action != ActionStop || o.Service.ID != "container:web-app" {
		t.Fatalf("decision %+v, want stop of web-app", dec)
	}
	if !o.LastAction.Equal(first.Outcome.At) {
		t.Fatalf("last action %v, want the restart at %v", o.LastAction, first.Outcome.At)
	}
}

func TestRestartAgainAfterRepeatWindow(t *testing.T) {
	cases := []struct {
		name   string
		repeat time.Duration
		gap    time.Duration
	}{
		{"window passed", 6 * time.Hour, 7 * time.Hour},
		{"stop rule disabled", 0, time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := episodeConfig(ModeAct)
			cfg.RepeatWindow = tc.repeat
			s := newSim(cfg, standardServices()...)
			s.run(5*time.Minute, 40, 0, webLeak())
			one(t, s.run(time.Minute, 3, 35, webLeak()))
			s.run(tc.gap, 40, 0, nil)
			s.run(5*time.Minute, 40, 0, webLeak())
			dec := one(t, s.run(time.Minute, 3, 35, webLeak()))
			if dec.Outcome.Kind != OutcomeRestarted || !dec.Outcome.LastAction.IsZero() {
				t.Fatalf("decision %+v, want a plain restart", dec)
			}
		})
	}
}

// TestUS1_5ProtectedCulprit covers US1-5: one protected outcome per episode
// and no action, in either mode.
func TestUS1_5ProtectedCulprit(t *testing.T) {
	for _, mode := range []Mode{ModeAct, ModeReport} {
		t.Run(string(mode), func(t *testing.T) {
			s := newSim(episodeConfig(mode), append(standardServices(), container("db", 12*gib))...)
			dbLeak := map[string]uint64{"db": leak}
			s.run(5*time.Minute, 40, 0, dbLeak)
			dec := one(t, s.run(20*time.Minute, 3, 45, dbLeak))
			o := dec.Outcome
			if dec.Act || o.Kind != OutcomeProtected || o.Service.ID != "container:db" || !o.Service.Protected ||
				o.Growth < gib {
				t.Fatalf("decision %+v, want protected db", dec)
			}
			if ep := s.g.Episode(); ep.State != StateReported {
				t.Fatalf("episode %+v, want reported", ep)
			}

			// Clear for less than the recovery window: same episode.
			s.run(time.Minute, 40, 0, nil)
			none(t, s.run(10*time.Minute, 3, 45, dbLeak))

			// Clear for the recovery window: the next danger is a new episode.
			s.run(2*time.Minute+time.Second, 40, 0, nil)
			if ep := s.g.Episode(); ep.State != StateIdle {
				t.Fatalf("episode %+v, want idle", ep)
			}
			if o := one(t, s.run(5*time.Minute, 3, 45, dbLeak)).Outcome; o.Kind != OutcomeProtected {
				t.Fatalf("outcome %+v, want protected", o)
			}
		})
	}
}

func TestNoCulpritReportedOncePerEpisode(t *testing.T) {
	s := newSim(episodeConfig(ModeAct), standardServices()...)
	s.run(5*time.Minute, 40, 0, nil)
	dec := one(t, s.run(20*time.Minute, 2, 50, nil))
	if dec.Act || dec.Outcome.Kind != OutcomeNoCulprit || dec.Outcome.Service.ID != "" ||
		dec.Outcome.AvailablePct != 2 {
		t.Fatalf("decision %+v, want no culprit", dec)
	}
}

// TestUS1_6RestartThatFreesNothing covers US1-6 and FR-006: one
// not_recovered outcome after the recovery window, and no second action in
// the episode even when another service grows.
func TestUS1_6RestartThatFreesNothing(t *testing.T) {
	s := newSim(episodeConfig(ModeAct), standardServices()...)
	s.run(5*time.Minute, 40, 0, webLeak())
	acted := one(t, s.run(31*time.Second, 3, 35, webLeak()))

	workerLeak := map[string]uint64{"worker.service": leak}
	none(t, s.run(2*time.Minute-time.Second, 3, 35, workerLeak))
	dec := one(t, s.run(time.Second, 3, 35, workerLeak))
	o := dec.Outcome
	if dec.Act || o.Kind != OutcomeNotRecovered || o.Service.ID != "container:web-app" || o.Action != ActionRestart {
		t.Fatalf("decision %+v, want not_recovered for the web-app restart", dec)
	}
	if want := acted.Outcome.At.Add(2 * time.Minute); !o.At.Equal(want) {
		t.Fatalf("not_recovered at %v, want %v", o.At, want)
	}
	if s.g.Episode().State != StateNotRecovered {
		t.Fatalf("episode %+v, want not_recovered", s.g.Episode())
	}

	none(t, s.run(30*time.Minute, 3, 35, workerLeak))

	// Clear for the recovery window ends the episode.
	s.run(2*time.Minute+time.Second, 40, 0, nil)
	if s.g.Episode().State != StateIdle {
		t.Fatalf("episode %+v, want idle", s.g.Episode())
	}
}

func TestActedHoldsForTheRecoveryWindow(t *testing.T) {
	s := newSim(episodeConfig(ModeAct), standardServices()...)
	s.run(5*time.Minute, 40, 0, webLeak())
	one(t, s.run(31*time.Second, 3, 35, webLeak()))

	// Danger flaps back within the recovery window: no second action.
	workerLeak := map[string]uint64{"worker.service": leak}
	s.run(10*time.Second, 40, 0, workerLeak)
	none(t, s.run(time.Minute, 3, 35, workerLeak))
	if s.g.Episode().State != StateActed {
		t.Fatalf("episode %+v, want acted", s.g.Episode())
	}
}

// TestActedNeedsSustainedClear covers FR-006: one clear reading that lands on
// the recovery deadline does not end the episode, so the danger that follows
// is reported as not_recovered instead of acting on a second service.
func TestActedNeedsSustainedClear(t *testing.T) {
	s := newSim(episodeConfig(ModeAct), standardServices()...)
	s.run(5*time.Minute, 40, 0, webLeak())
	one(t, s.run(31*time.Second, 3, 35, webLeak()))

	workerLeak := map[string]uint64{"worker.service": leak}
	none(t, s.run(2*time.Minute-time.Second, 3, 35, workerLeak))
	none(t, s.run(time.Second, 6, 35, workerLeak)) // the deadline reading
	dec := one(t, s.run(5*time.Minute, 3, 35, workerLeak))
	if dec.Act || dec.Outcome.Kind != OutcomeNotRecovered || dec.Outcome.Service.ID != "container:web-app" {
		t.Fatalf("decision %+v, want not_recovered for web-app and no action", dec)
	}
}

func TestActedEndsWhenClearHolds(t *testing.T) {
	s := newSim(episodeConfig(ModeAct), standardServices()...)
	s.run(5*time.Minute, 40, 0, webLeak())
	one(t, s.run(31*time.Second, 3, 35, webLeak()))

	// Memory comes back shortly before the deadline: the episode ends once
	// clear has held for danger_for.
	s.run(2*time.Minute-10*time.Second, 3, 35, nil)
	none(t, s.run(30*time.Second, 40, 0, nil))
	if s.g.Episode().State != StateActed {
		t.Fatalf("episode %+v, want acted while clear has not held", s.g.Episode())
	}
	none(t, s.run(time.Second, 40, 0, nil))
	if s.g.Episode().State != StateIdle {
		t.Fatalf("episode %+v, want idle", s.g.Episode())
	}
}

// TestUS1_7ReportModeWouldRestart covers US1-7 and FR-001.
func TestUS1_7ReportModeWouldRestart(t *testing.T) {
	s := newSim(episodeConfig(ModeReport), standardServices()...)
	s.run(5*time.Minute, 40, 0, webLeak())
	dec := one(t, s.run(20*time.Minute, 3, 35, webLeak()))
	o := dec.Outcome
	if dec.Act || o.Kind != OutcomeWouldRestart || o.Action != ActionRestart || o.Service.ID != "container:web-app" {
		t.Fatalf("decision %+v, want would_restart of web-app", dec)
	}
	if ep := s.g.Episode(); ep.State != StateReported {
		t.Fatalf("episode %+v, want reported", ep)
	}

	// A report record never counts as a restart: the next episode is again
	// a would_restart, not a would_stop.
	s.run(3*time.Minute, 40, 0, nil)
	if o := one(t, s.run(time.Minute, 3, 35, webLeak())).Outcome; o.Kind != OutcomeWouldRestart {
		t.Fatalf("outcome %+v, want would_restart", o)
	}
	if recs := s.g.Export(s.at); len(recs) != 2 || recs[0].Mode != ModeReport || recs[1].Mode != ModeReport {
		t.Fatalf("records %+v, want two report records", recs)
	}
}

func TestReportModeWouldStopAfterAnActedRestart(t *testing.T) {
	act := newSim(episodeConfig(ModeAct), standardServices()...)
	act.run(5*time.Minute, 40, 0, webLeak())
	one(t, act.run(time.Minute, 3, 35, webLeak()))

	s := newSim(episodeConfig(ModeReport), standardServices()...)
	s.at = act.at.Add(time.Hour)
	s.g.Restore(s.at, act.g.Export(act.at))
	s.run(5*time.Minute, 40, 0, webLeak())
	dec := one(t, s.run(time.Minute, 3, 35, webLeak()))
	if dec.Act || dec.Outcome.Kind != OutcomeWouldStop || dec.Outcome.Action != ActionStop {
		t.Fatalf("decision %+v, want would_stop", dec)
	}
}

// TestRestoreMidEpisode covers FR-009: tgshd restarts right after a restart
// and the danger goes on; the new guard stops the repeat leaker.
func TestRestoreMidEpisode(t *testing.T) {
	before := newSim(episodeConfig(ModeAct), standardServices()...)
	before.run(5*time.Minute, 40, 0, webLeak())
	first := one(t, before.run(time.Minute, 3, 35, webLeak()))
	saved := before.g.Export(before.at)

	after := newSim(episodeConfig(ModeAct), before.svcs...)
	after.at = before.at.Add(10 * time.Second)
	after.g.Restore(after.at, saved)
	after.run(2*time.Minute, 40, 0, webLeak())
	dec := one(t, after.run(time.Minute, 3, 35, webLeak()))
	if !dec.Act || dec.Outcome.Kind != OutcomeStopped || !dec.Outcome.LastAction.Equal(first.Outcome.At) {
		t.Fatalf("decision %+v, want a stop after the restored restart", dec)
	}
}

func TestExportRestoreDropOldRecords(t *testing.T) {
	g := New(episodeConfig(ModeAct))
	now := t0().Add(24 * time.Hour)
	g.Restore(now, []Record{
		{ServiceID: "container:old", Name: "old", Action: ActionRestart, At: now.Add(-7 * time.Hour), Mode: ModeAct},
		{
			ServiceID: "container:web-app",
			Name:      "web-app",
			Action:    ActionRestart,
			At:        now.Add(-time.Hour),
			Before:    gib,
			Mode:      ModeAct,
		},
		{
			ServiceID: "unit:worker.service",
			Name:      "worker.service",
			Action:    ActionRestart,
			At:        now.Add(-5 * time.Hour),
			Mode:      ModeReport,
		},
	})
	got := g.Export(now)
	if len(got) != 2 || got[0].ServiceID != "container:web-app" || got[0].Before != gib || got[1].Mode != ModeReport {
		t.Fatalf("export %+v, want the web-app and worker records", got)
	}
	later := g.Export(now.Add(4*time.Hour + 30*time.Minute))
	if len(later) != 1 || later[0].ServiceID != "container:web-app" {
		t.Fatalf("later export %+v, want only the web-app record", later)
	}

	got[0].Name = "changed"
	if g.Export(now)[0].Name != "web-app" {
		t.Fatal("Export shares its slice with the guard")
	}
}

// scenarios are every runaway story the guard is tested against; SC-004 and
// SC-005 must hold in each of them.
func scenarios() map[string]func() *sim {
	dbLeak := map[string]uint64{"db": leak}
	withDB := func(mode Mode) *sim {
		return newSim(episodeConfig(mode), append(standardServices(), container("db", 12*gib))...)
	}

	return map[string]func() *sim{
		"repeat leaker over a day": func() *sim {
			s := newSim(episodeConfig(ModeAct), standardServices()...)
			for range 12 {
				s.run(5*time.Minute, 40, 0, webLeak())
				s.run(5*time.Minute, 3, 35, webLeak())
				s.run(90*time.Minute, 40, 0, nil)
			}

			return s
		},
		"restart that frees nothing": func() *sim {
			s := newSim(episodeConfig(ModeAct), standardServices()...)
			s.run(5*time.Minute, 40, 0, webLeak())
			s.run(time.Hour, 3, 35, webLeak())

			return s
		},
		"protected leak in act mode": func() *sim {
			s := withDB(ModeAct)
			for range 5 {
				s.run(5*time.Minute, 40, 0, dbLeak)
				s.run(10*time.Minute, 3, 45, dbLeak)
			}

			return s
		},
		"protected and eligible leak": func() *sim {
			s := withDB(ModeAct)
			both := map[string]uint64{"db": 2 * leak, "web-app": leak}
			s.run(5*time.Minute, 40, 0, both)
			s.run(30*time.Minute, 3, 45, both)

			return s
		},
		"protected leak in report mode": func() *sim {
			s := withDB(ModeReport)
			s.run(5*time.Minute, 40, 0, dbLeak)
			s.run(time.Hour, 3, 45, dbLeak)

			return s
		},
	}
}

// TestSC004NeverTouchIsNeverActedOn: a never-touch service is never
// restarted or stopped in any scenario.
func TestSC004NeverTouchIsNeverActedOn(t *testing.T) {
	for name, run := range scenarios() {
		t.Run(name, func(t *testing.T) {
			for _, dec := range run().decisions {
				if dec.Act && (dec.Outcome.Service.Protected || dec.Outcome.Service.Name == "db") {
					t.Fatalf("acted on a never-touch service: %+v", dec)
				}
			}
		})
	}
}

// TestSC005NoRestartTwiceWithinRepeatWindow: no service is restarted more
// than once within the repeat window in any scenario.
func TestSC005NoRestartTwiceWithinRepeatWindow(t *testing.T) {
	for name, run := range scenarios() {
		t.Run(name, func(t *testing.T) {
			s := run()
			last := map[string]time.Time{}
			for _, dec := range s.decisions {
				if !dec.Act || dec.Outcome.Action != ActionRestart {
					continue
				}
				id := dec.Outcome.Service.ID
				if prev, ok := last[id]; ok && dec.Outcome.At.Sub(prev) <= 6*time.Hour {
					t.Fatalf("%s restarted at %v and again at %v", id, prev, dec.Outcome.At)
				}
				last[id] = dec.Outcome.At
			}
		})
	}
}

// TestObserveHostSteadyStateDoesNotAllocate: judging a reading that leads to
// no decision allocates nothing, idle or mid-episode.
func TestObserveHostSteadyStateDoesNotAllocate(t *testing.T) {
	s := newSim(episodeConfig(ModeAct), standardServices()...)
	s.run(5*time.Minute, 40, 0, webLeak())
	at := s.at
	idle := testing.AllocsPerRun(100, func() {
		at = at.Add(time.Second)
		s.g.ObserveHost(Host{At: at, AvailablePct: 40})
	})
	s.at = at.Add(time.Second)
	one(t, s.run(time.Minute, 3, 35, webLeak()))
	at = s.at
	acted := testing.AllocsPerRun(100, func() {
		at = at.Add(time.Millisecond)
		s.g.ObserveHost(Host{At: at, AvailablePct: 3, FullAvg10: 35})
	})
	r := newSim(episodeConfig(ModeReport), standardServices()...)
	r.run(5*time.Minute, 40, 0, webLeak())
	one(t, r.run(time.Minute, 3, 35, webLeak()))
	at = r.at
	reported := testing.AllocsPerRun(100, func() {
		at = at.Add(time.Second)
		r.g.ObserveHost(Host{At: at, AvailablePct: 40})
	})
	if idle != 0 || acted != 0 || reported != 0 {
		t.Fatalf("allocations per reading: idle %v, acted %v, reported %v; want 0", idle, acted, reported)
	}
}

func TestDangerStateWhileBuilding(t *testing.T) {
	s := newSim(episodeConfig(ModeAct), standardServices()...)
	s.run(5*time.Minute, 40, 0, webLeak())
	since := s.at
	none(t, s.run(10*time.Second, 3, 35, webLeak()))
	if ep := s.g.Episode(); ep.State != StateDanger || !ep.Since.Equal(since) {
		t.Fatalf("episode %+v, want danger since %v", ep, since)
	}
	s.run(time.Second, 40, 0, webLeak())
	if ep := s.g.Episode(); ep.State != StateIdle {
		t.Fatalf("episode %+v, want idle after a clear reading", ep)
	}
}
