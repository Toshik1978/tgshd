package memory

import (
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
func (c *Check) judgeGrowth(ctx context.Context, res *health.CheckResult) {
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
			reading.Verdict, reading.Text = c.judgeService(name)
		} else {
			// Gone from the host: resolve whatever was open.
			delete(c.growth.opened, name)
			reading.Verdict, reading.Text = health.Good, name+" is gone"
		}
		res.Readings = append(res.Readings, reading)
	}
}

// judgeService applies research R12 to one service's ring. A condition that is
// open stays open until the rise falls below half the warn level.
func (c *Check) judgeService(name string) (health.Verdict, string) {
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
		return health.Bad, fmt.Sprintf("%s grew %s→%s in %s",
			name, number(float64(ring[low].Anon)/gib), size(latest.Anon), span(latest.At.Sub(ring[low].At)))
	}

	delete(c.growth.opened, name)

	return health.Good, name + " at " + size(latest.Anon)
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

	return fmt.Sprintf("%d min", int(d.Round(time.Minute)/time.Minute))
}

// Export serialises the growth history for the key-value store (kv
// memory.growth), dropping services gone for longer than the growth window.
func (c *Check) Export() ([]byte, error) {
	c.growth.mu.Lock()
	defer c.growth.mu.Unlock()
	if c.settings.GrowthWindow > 0 {
		c.pruneLocked(c.settings.now())
	}
	data, err := json.Marshal(c.growth.rings)
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
	var rings map[string][]point
	if err := json.Unmarshal(data, &rings); err != nil {
		return fmt.Errorf("restore memory growth: %w", err)
	}
	c.growth.mu.Lock()
	defer c.growth.mu.Unlock()
	c.growth.rings = rings
	if c.growth.rings == nil {
		c.growth.rings = map[string][]point{}
	}
	c.growth.opened = map[string]bool{}
	if c.settings.GrowthWindow > 0 {
		c.pruneLocked(c.settings.now())
	}

	return nil
}
