package memguard

import "time"

// Culprit is the service whose memory grew the most within the culprit window.
type Culprit struct {
	Service Service
	// Before is the minimum anon in the window, in bytes.
	Before uint64
	// Size is the latest anon, in bytes.
	Size uint64
	// Growth is Size − Before, in bytes.
	Growth uint64
	// Window is the time from the minimum to the latest sample.
	Window time.Duration
}

// Culprit returns the eligible or protected service with the largest growth
// of anon within the culprit window ending at at (research R4). Growth must be
// at least CulpritMinGrowth, and never zero, even when CulpritMinGrowth is
// (FR-003); ties go to the larger size, then to the lower ID.
func (g *Guard) Culprit(at time.Time) (Culprit, bool) {
	var best Culprit
	found := false
	for _, e := range g.entries {
		c, ok := e.growth(at, g.cfg.CulpritWindow)
		if !ok || c.Growth == 0 || c.Growth < g.cfg.CulpritMinGrowth {
			continue
		}
		if !found || beats(c, best) {
			best, found = c, true
		}
	}

	return best, found
}

func beats(c, best Culprit) bool {
	switch {
	case c.Growth != best.Growth:
		return c.Growth > best.Growth
	case c.Size != best.Size:
		return c.Size > best.Size
	default:
		return c.Service.ID < best.Service.ID
	}
}

// growth measures the entry's growth from its minimum within window ending at
// at to its latest sample. It needs at least two samples in the window.
func (e *entry) growth(at time.Time, window time.Duration) (Culprit, bool) {
	r := &e.history
	if r.n < 2 {
		return Culprit{}, false
	}
	latest := r.latest()
	low, samples := latest, 0
	for i := range r.n {
		p := r.points[(r.next-1-i+2*len(r.points))%len(r.points)]
		if at.Sub(p.at) > window {
			break
		}
		samples++
		if p.anon < low.anon {
			low = p
		}
	}
	if samples < 2 {
		return Culprit{}, false
	}

	return Culprit{
		Service: e.service,
		Before:  low.anon,
		Size:    latest.anon,
		Growth:  latest.anon - low.anon,
		Window:  latest.at.Sub(low.at),
	}, true
}
