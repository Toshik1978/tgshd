package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/Toshik1978/tgshd/pkg/health"
)

const (
	keyGrowthPrefix = "memory/growth/"

	// stillAtPeak is the share of the window maximum the latest point must
	// hold for growth to count as still climbing.
	stillAtPeak = 0.9
)

// point is one growth observation of a service.
type point struct {
	At   time.Time `json:"at"`
	Anon uint64    `json:"anon"`
}

// growth is the per-service history of the growth judgement: one point per
// run, kept for the growth window.
type growth struct {
	mu     sync.Mutex
	rings  map[string][]point
	opened map[string]bool
}

// judgeGrowth records one point per service and judges each service's rise
// over the growth window. Disabled when the window or warn level is zero.
func (c *Check) judgeGrowth(ctx context.Context, res *health.CheckResult, top *topLookup) {
	if c.settings.GrowthWindow <= 0 || c.settings.GrowthWarn == 0 {
		return
	}
	services, err := c.source.Services(ctx)
	if err != nil {
		res.Err = errors.Join(res.Err, health.CheckError{Subject: "services", Kind: kindUnreadable, Err: err})
		return
	}

	now := c.settings.now()
	c.growth.mu.Lock()
	defer c.growth.mu.Unlock()
	if c.growth.rings == nil {
		c.growth.rings, c.growth.opened = map[string][]point{}, map[string]bool{}
	}

	seen := make(map[string]bool, len(services))
	for _, s := range services {
		seen[s.Name] = true
		c.growth.rings[s.Name] = append(c.growth.rings[s.Name], point{At: now, Anon: s.Anon})
	}
	c.pruneLocked(now)

	for _, name := range slices.Sorted(maps.Keys(c.growth.rings)) {
		reading := health.Reading{Key: keyGrowthPrefix + name, Kind: health.State}
		if seen[name] {
			reading.Verdict, reading.Text = c.judgeService(name, top)
		} else {
			// Gone from the host: resolve whatever was open.
			delete(c.growth.opened, name)
			reading.Verdict, reading.Text = health.Good, name+" is gone"
		}
		res.Readings = append(res.Readings, reading)
	}
}

// judgeService applies research R12 to one service's ring. A condition that is
// open stays open until the rise falls below half the warn level. A problem
// text names the current top consumer (FR-014).
func (c *Check) judgeService(name string, top *topLookup) (health.Verdict, string) {
	ring := c.growth.rings[name]
	low, high := 0, 0
	for i, p := range ring {
		if p.Anon < ring[low].Anon {
			low = i
		}
		if p.Anon >= ring[high].Anon {
			high = i
		}
	}
	latest := ring[len(ring)-1]
	rise := latest.Anon - min(latest.Anon, ring[low].Anon)

	warn := c.settings.GrowthWarn
	climbing := rise >= warn && low < high &&
		float64(latest.Anon) >= stillAtPeak*float64(ring[high].Anon)
	if climbing || (c.growth.opened[name] && rise*2 >= warn) {
		c.growth.opened[name] = true

		return health.Bad, fmt.Sprintf(
			subject+"%s grew %s→%s in %s%s",
			name,
			number(float64(ring[low].Anon)/gib),
			size(latest.Anon),
			span(latest.At.Sub(ring[low].At)),
			top.suffix(),
		)
	}

	delete(c.growth.opened, name)

	return health.Good, subject + name + " at " + size(latest.Anon)
}

// pruneLocked drops points older than the window and services left empty.
func (c *Check) pruneLocked(now time.Time) {
	cutoff := now.Add(-c.settings.GrowthWindow)
	for name, ring := range c.growth.rings {
		ring = slices.DeleteFunc(ring, func(p point) bool { return p.At.Before(cutoff) })
		if len(ring) == 0 {
			delete(c.growth.rings, name)
			delete(c.growth.opened, name)
			continue
		}
		c.growth.rings[name] = ring
	}
}

// span is "40 min", or "3 h" from two hours.
func span(d time.Duration) string {
	if d >= 2*time.Hour {
		return number(d.Hours()) + " h"
	}

	return fmt.Sprintf("%d min", max(1, int(d.Round(time.Minute)/time.Minute)))
}

// growthState is the persisted form: the rings and the services whose growth
// condition is open (the hold-open flag, which a restart must not lose).
type growthState struct {
	Rings  map[string][]point `json:"rings"`
	Opened []string           `json:"opened,omitempty"`
}

// decodeGrowth reads the current format, or the bare rings map an earlier
// version wrote.
func decodeGrowth(data []byte) (growthState, error) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		return growthState{}, err //nolint:wrapcheck // wrapped by the caller
	}
	var state growthState
	// A bare map holds arrays of points; the current format holds an object.
	if rings, ok := probe["rings"]; ok && !bytes.HasPrefix(bytes.TrimSpace(rings), []byte("[")) {
		err := json.Unmarshal(data, &state)

		return state, err //nolint:wrapcheck // wrapped by the caller
	}
	err := json.Unmarshal(data, &state.Rings)

	return state, err //nolint:wrapcheck // wrapped by the caller
}

// Export serialises the growth history for the key-value store (kv
// memory.growth), dropping services gone for longer than the growth window.
func (c *Check) Export() ([]byte, error) {
	c.growth.mu.Lock()
	defer c.growth.mu.Unlock()
	if c.settings.GrowthWindow > 0 {
		c.pruneLocked(c.settings.now())
	}
	state := growthState{Rings: c.growth.rings, Opened: slices.Sorted(maps.Keys(c.growth.opened))}
	data, err := json.Marshal(state)
	if err != nil {
		return nil, fmt.Errorf("export memory growth: %w", err)
	}

	return data, nil
}

// Restore loads history written by Export. Empty data is a no-op.
func (c *Check) Restore(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	state, err := decodeGrowth(data)
	if err != nil {
		return fmt.Errorf("restore memory growth: %w", err)
	}
	c.growth.mu.Lock()
	defer c.growth.mu.Unlock()
	c.growth.rings = state.Rings
	if c.growth.rings == nil {
		c.growth.rings = map[string][]point{}
	}
	c.growth.opened = make(map[string]bool, len(state.Opened))
	for _, name := range state.Opened {
		c.growth.opened[name] = true
	}
	if c.settings.GrowthWindow > 0 {
		c.pruneLocked(c.settings.now())
	}

	return nil
}
