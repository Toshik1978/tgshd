package memguard

import "time"

// Mode is what the guard does about a culprit.
type Mode string

const (
	// ModeReport only reports what the guard would do.
	ModeReport Mode = "report"
	// ModeAct restarts or stops the culprit.
	ModeAct Mode = "act"
	// ModeOff disables the guard: its loop is not started.
	ModeOff Mode = "off"
)

// Action is what the guard does to a service.
type Action string

const (
	// ActionRestart restarts the service.
	ActionRestart Action = "restart"
	// ActionStop stops a service that ran away again within the repeat window.
	ActionStop Action = "stop"
	// ActionKill kills the service's cgroup when its runtime did not answer.
	ActionKill Action = "kill"
)

// OutcomeKind is what an outcome reports.
type OutcomeKind string

const (
	// OutcomeRestarted: the culprit was restarted.
	OutcomeRestarted OutcomeKind = "restarted"
	// OutcomeStopped: a repeat leaker was stopped.
	OutcomeStopped OutcomeKind = "stopped"
	// OutcomeKilled: the runtime did not answer and the cgroup was killed.
	OutcomeKilled OutcomeKind = "killed"
	// OutcomeWouldRestart: report mode, the culprit would have been restarted.
	OutcomeWouldRestart OutcomeKind = "would_restart"
	// OutcomeWouldStop: report mode, the culprit would have been stopped.
	OutcomeWouldStop OutcomeKind = "would_stop"
	// OutcomeNotRecovered: the action did not free memory.
	OutcomeNotRecovered OutcomeKind = "not_recovered"
	// OutcomeProtected: the culprit is never-touch; nothing was done.
	OutcomeProtected OutcomeKind = "protected"
	// OutcomeNoCulprit: no service grew enough; nothing was done.
	OutcomeNoCulprit OutcomeKind = "no_culprit"
	// OutcomeActionFailed: the action could not be run.
	OutcomeActionFailed OutcomeKind = "action_failed"
)

// State is the state of the danger episode.
type State string

const (
	// StateIdle: no danger.
	StateIdle State = "idle"
	// StateDanger: danger readings, not yet held for DangerFor.
	StateDanger State = "danger"
	// StateActed: the guard acted and waits for the recovery window.
	StateActed State = "acted"
	// StateReported: the episode was reported without an action.
	StateReported State = "reported"
	// StateNotRecovered: the action did not free memory.
	StateNotRecovered State = "not_recovered"
)

// Host is one host-level reading.
type Host struct {
	At           time.Time
	AvailablePct float64
	FullAvg10    float64
}

// Outcome is one thing the guard did or decided not to do.
type Outcome struct {
	Culprit

	At   time.Time
	Kind OutcomeKind
	// Action is the action decided, or for not_recovered the one that did
	// not free memory.
	Action       Action
	AvailablePct float64
	FullAvg10    float64
	// LastAction is when tgshd last acted on the service, for stopped and
	// would_stop.
	LastAction time.Time
	// Top is the largest service, or the largest process when no service holds
	// most of the memory, for not_recovered and no_culprit. The loop fills it.
	Top Top
	// Err is why the action failed, for action_failed.
	Err error
	// Timeout is how long the runtime was given to answer, for killed.
	Timeout time.Duration
}

// Decision is what one host reading makes the guard do.
type Decision struct {
	// Act is true when Outcome.Action must be run on Outcome.Service.
	Act     bool
	Outcome Outcome
	// Record is the intervention record of an Act decision, handed back
	// through ActionDone once the action has run.
	Record Record
}

// Episode is the danger episode in progress.
type Episode struct {
	State   State
	Since   time.Time
	Culprit Culprit
	Action  Action
	// ActedAt is when the action was decided; DoneAt is when it finished,
	// zero while it still runs. The recovery window starts at DoneAt.
	ActedAt time.Time
	DoneAt  time.Time
}

// Record is one intervention.
type Record struct {
	ServiceID string    `json:"service_id"`
	Name      string    `json:"name"`
	Action    Action    `json:"action"`
	At        time.Time `json:"at"`
	Before    uint64    `json:"before"`
	Mode      Mode      `json:"mode"`
}

// ObserveHost judges one host reading (research R8) and returns the decision
// it leads to, if any:
//
//	idle ──(danger reading)──▶ danger ──(clear reading)──▶ idle
//	danger ──(held for DangerFor, culprit found, act)──▶ acted
//	danger ──(held for DangerFor, no culprit | protected | report)──▶ reported
//	acted ──(RecoveryWindow passed, clear held for DangerFor)──▶ idle
//	acted ──(still danger after RecoveryWindow)──▶ not_recovered
//	reported | not_recovered ──(clear for RecoveryWindow)──▶ idle
//
// Danger is AvailablePct below DangerAvailablePercent and FullAvg10 at or above
// DangerPressurePercent. Every episode yields at most one decision, plus one
// not_recovered outcome after an action. A decision with Act set must be run
// by the caller, which reports how it ended through ActionDone; its outcome
// reports success, and the caller replaces it when the action fails. Readings
// must come in time order.
func (g *Guard) ObserveHost(h Host) (Decision, bool) {
	danger := g.track(h)
	switch g.ep.State {
	case StateActed:
		return g.recovered(h, danger)
	case StateReported, StateNotRecovered:
		if !danger && h.At.Sub(g.clearSince) >= g.cfg.RecoveryWindow {
			g.ep = Episode{State: StateIdle}
		}

		return Decision{}, false
	case StateIdle, StateDanger:
	}
	if !danger {
		g.ep = Episode{State: StateIdle}

		return Decision{}, false
	}
	if g.ep.State == StateIdle {
		g.ep = Episode{State: StateDanger, Since: g.dangerSince}
	}
	if h.At.Sub(g.dangerSince) < g.cfg.DangerFor {
		return Decision{}, false
	}

	return g.decide(h), true
}

// track judges whether h is a danger reading and keeps when the current run of
// danger or clear readings began.
func (g *Guard) track(h Host) bool {
	danger := h.AvailablePct < g.cfg.DangerAvailablePercent && h.FullAvg10 >= g.cfg.DangerPressurePercent
	if danger {
		if g.dangerSince.IsZero() {
			g.dangerSince = h.At
		}
		g.clearSince = time.Time{}
	} else {
		g.dangerSince = time.Time{}
		if g.clearSince.IsZero() {
			g.clearSince = h.At
		}
	}

	return danger
}

// recovered judges an acted episode once its action has finished and the
// recovery window has passed since: clear held for DangerFor ends it, a danger
// reading makes it not_recovered with one outcome and no new action. A single
// clear reading never ends it (FR-006). While the action still runs nothing is
// judged, so the episode cannot end and no second action can be decided.
func (g *Guard) recovered(h Host, danger bool) (Decision, bool) {
	if g.ep.DoneAt.IsZero() || h.At.Sub(g.ep.DoneAt) < g.cfg.RecoveryWindow {
		return Decision{}, false
	}
	if !danger {
		if h.At.Sub(g.clearSince) >= g.cfg.DangerFor {
			g.ep = Episode{State: StateIdle}
		}

		return Decision{}, false
	}
	g.ep.State = StateNotRecovered
	out := g.outcome(h, OutcomeNotRecovered)
	out.Culprit, out.Action = g.ep.Culprit, g.ep.Action

	return Decision{Outcome: out}, true
}

// decide ends the danger state: it picks the culprit and the action.
func (g *Guard) decide(h Host) Decision {
	c, ok := g.Culprit(h.At)
	if !ok {
		g.ep.State = StateReported

		return Decision{Outcome: g.outcome(h, OutcomeNoCulprit)}
	}
	g.ep.Culprit = c
	out := g.outcome(h, OutcomeProtected)
	out.Culprit = c
	if c.Service.Protected {
		g.ep.State = StateReported

		return Decision{Outcome: out}
	}

	out.Action, out.LastAction = g.actionFor(c.Service.ID, h.At)
	rec := Record{
		ServiceID: c.Service.ID, Name: c.Service.Name, Action: out.Action, At: h.At, Before: c.Size, Mode: ModeReport,
	}
	stop := out.Action == ActionStop
	if g.cfg.Mode != ModeAct {
		g.addRecord(h.At, rec)
		g.ep.State = StateReported
		out.Kind = pick(stop, OutcomeWouldStop, OutcomeWouldRestart)

		return Decision{Outcome: out}
	}
	rec.Mode = ModeAct
	g.ep.State, g.ep.Action, g.ep.ActedAt = StateActed, out.Action, h.At
	out.Kind = pick(stop, OutcomeStopped, OutcomeRestarted)

	return Decision{Act: true, Outcome: out, Record: rec}
}

// ActionDone tells the guard that the action of an Act decision finished at
// at; r is the decision's record with Action set to what was run (kill after
// the fallback). The recovery window of the episode starts at at. Only an
// action that succeeded is kept as an intervention and counts toward the
// repeat window (FR-005: restarted by tgshd). A failed one turns its episode,
// if still in progress, into a reported one, so no not_recovered outcome
// follows an action that never ran.
func (g *Guard) ActionDone(at time.Time, r Record, ok bool) {
	current := g.ep.State == StateActed && g.ep.ActedAt.Equal(r.At)
	if !ok {
		if current {
			g.ep.State = StateReported
		}

		return
	}
	if current {
		g.ep.Action, g.ep.DoneAt = r.Action, at
	}
	g.addRecord(r.At, r)
}

func pick(stop bool, ifStop, ifRestart OutcomeKind) OutcomeKind {
	if stop {
		return ifStop
	}

	return ifRestart
}

func (g *Guard) outcome(h Host, kind OutcomeKind) Outcome {
	return Outcome{At: h.At, Kind: kind, AvailablePct: h.AvailablePct, FullAvg10: h.FullAvg10}
}

// actionFor is stop when tgshd acted on the service within the repeat window
// (report records never count), else restart. It also returns when tgshd last
// acted on it.
func (g *Guard) actionFor(id string, at time.Time) (Action, time.Time) {
	if g.cfg.RepeatWindow <= 0 {
		return ActionRestart, time.Time{}
	}
	var last time.Time
	for _, r := range g.records {
		if r.ServiceID == id && r.Mode == ModeAct && g.live(r, at) && r.At.After(last) {
			last = r.At
		}
	}
	if last.IsZero() {
		return ActionRestart, time.Time{}
	}

	return ActionStop, last
}

// live reports whether r is still within the repeat window at at.
func (g *Guard) live(r Record, at time.Time) bool {
	return at.Sub(r.At) <= g.cfg.RepeatWindow
}

func (g *Guard) addRecord(at time.Time, r Record) {
	g.records = g.prune(g.records[:0], g.records, at)
	g.records = append(g.records, r)
}

// prune appends to dst the records of src still within the repeat window.
func (g *Guard) prune(dst, src []Record, at time.Time) []Record {
	for _, r := range src {
		if g.live(r, at) {
			dst = append(dst, r)
		}
	}

	return dst
}

// Episode returns the danger episode in progress.
func (g *Guard) Episode() Episode {
	return g.ep
}

// Export returns a copy of the intervention records still within the repeat
// window at at, for the kv state memory.guard.
func (g *Guard) Export(at time.Time) []Record {
	return g.prune(nil, g.records, at)
}

// Restore replaces the intervention records with those of records still
// within the repeat window at at. The episode itself is not persisted.
func (g *Guard) Restore(at time.Time, records []Record) {
	g.records = g.prune(nil, records, at)
}
