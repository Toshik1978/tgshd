package health

import (
	"slices"
	"strings"
	"sync"
	"time"
)

// gapCapFactor bounds the time one reading may add to a streak, as a multiple
// of the check interval, so outages, quiet windows and unreadable stretches
// never count towards a threshold.
const gapCapFactor = 1.5

// AlertKind classifies an Alert produced by the engine.
type AlertKind int

// Alert kinds.
const (
	// AlertProblem is sent when a condition's bad time reaches its threshold.
	AlertProblem AlertKind = iota
	// AlertResolved is sent when an open condition turns good.
	AlertResolved
	// AlertEvent is a one-off message from an Event reading.
	AlertEvent
)

// Alert is what the engine asks a notifier to deliver.
type Alert struct {
	Kind  AlertKind
	Check string
	Key   string
	Text  string
	// StreakStart is when the bad streak began (zero for AlertEvent).
	StreakStart time.Time
	// Mode is the reading's delivery override (ModeDefault if none).
	Mode Mode
}

// Condition is the persistent state of one monitored condition.
type Condition struct {
	Key           string
	Check         string
	StreakStart   time.Time
	BadFor        time.Duration
	LastReadingAt time.Time
	LastText      string
	Open          bool
	OpenedAt      time.Time
}

// Engine is the pure threshold state machine. It is safe for concurrent use.
type Engine struct {
	mu    sync.Mutex
	conds map[string]*Condition
}

// NewEngine returns an engine with no conditions.
func NewEngine() *Engine {
	return &Engine{conds: make(map[string]*Condition)}
}

// Apply feeds one run of a check to the engine and returns the alerts to
// deliver. interval is the check's period and threshold how long a condition
// must be bad (counting only observed time) before a problem is raised.
func (e *Engine) Apply(now time.Time, check string, interval, threshold time.Duration, readings []Reading) []Alert {
	e.mu.Lock()
	defer e.mu.Unlock()

	var out []Alert
	for _, r := range readings {
		if r.Kind == Event {
			out = append(out, Alert{Kind: AlertEvent, Check: check, Key: r.Key, Text: r.Text, Mode: r.Mode})

			continue
		}
		switch r.Verdict {
		case Bad:
			out = e.bad(out, now, check, interval, threshold, r)
		case Good:
			out = e.good(out, check, r)
		case Unknown:
			// Unreadable: no transition, LastReadingAt stays put.
		}
	}

	return out
}

// Clear returns the condition to Healthy without any message. It reports
// whether the engine was tracking the key.
func (e *Engine) Clear(key string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	_, ok := e.conds[key]
	delete(e.conds, key)
	return ok
}

// Condition returns a copy of the state tracked for key.
func (e *Engine) Condition(key string) (Condition, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	c, ok := e.conds[key]
	if !ok {
		return Condition{}, false
	}

	return *c, true
}

// Conditions exports every tracked condition (streaks and open ones), sorted
// by key, for persistence.
func (e *Engine) Conditions() []Condition {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Condition, 0, len(e.conds))
	for _, c := range e.conds {
		out = append(out, *c)
	}
	slices.SortFunc(out, func(a, b Condition) int { return strings.Compare(a.Key, b.Key) })

	return out
}

// OpenConditions exports only the conditions with a problem sent and not yet
// resolved, sorted by key.
func (e *Engine) OpenConditions() []Condition {
	all := e.Conditions()
	return slices.DeleteFunc(all, func(c Condition) bool { return !c.Open })
}

// Restore replaces the engine's state with previously exported conditions.
func (e *Engine) Restore(conds []Condition) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.conds = make(map[string]*Condition, len(conds))
	for i := range conds {
		c := conds[i]
		e.conds[c.Key] = &c
	}
}

// Merge adds previously exported conditions whose key the engine does not
// track; tracked keys keep their current state. It is atomic with respect to
// Apply and Clear, so a key cleared before the merge stays cleared unless
// the merged state holds it.
func (e *Engine) Merge(conds []Condition) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i := range conds {
		if _, ok := e.conds[conds[i].Key]; ok {
			continue
		}
		c := conds[i]
		e.conds[c.Key] = &c
	}
}

func (e *Engine) bad(out []Alert, now time.Time, check string, interval, threshold time.Duration, r Reading) []Alert {
	c, ok := e.conds[r.Key]
	if !ok {
		c = &Condition{Key: r.Key, Check: check, StreakStart: now}
		e.conds[r.Key] = c
	} else if !c.LastReadingAt.IsZero() {
		elapsed := min(now.Sub(c.LastReadingAt), time.Duration(gapCapFactor*float64(interval)))
		c.BadFor += max(elapsed, 0)
	}
	c.Check = check
	c.LastReadingAt = now
	c.LastText = r.Text
	if c.Open || c.BadFor < threshold {
		return out
	}
	c.Open = true
	c.OpenedAt = now

	return append(out, Alert{
		Kind: AlertProblem, Check: check, Key: r.Key, Text: r.Text,
		StreakStart: c.StreakStart, Mode: r.Mode,
	})
}

func (e *Engine) good(out []Alert, check string, r Reading) []Alert {
	c, ok := e.conds[r.Key]
	if !ok {
		return out
	}
	delete(e.conds, r.Key)
	if !c.Open {
		return out
	}
	text := r.Text
	if text == "" {
		text = c.LastText
	}

	return append(out, Alert{
		Kind: AlertResolved, Check: check, Key: r.Key, Text: text,
		StreakStart: c.StreakStart, Mode: r.Mode,
	})
}
