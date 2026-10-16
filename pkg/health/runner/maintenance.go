package runner

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const (
	// KVMaintenance is the kv key of the maintenance-lock observation.
	KVMaintenance = "health.maintenance"
	// DefaultMaintenanceMaxHold is how long the maintenance lock may be held
	// before its checker error when Options.MaintenanceMaxHold is unset
	// (general.maintenance_max_hold).
	DefaultMaintenanceMaxHold = 6 * time.Hour
	// maintenanceGap is the longest time since the lock was last seen held
	// over which a hold counts as continuing, e.g. across a daemon restart.
	maintenanceGap = 5 * time.Minute

	keyMaintenanceStuck = "health/maintenance/stuck"
	heldLayout          = "2006-01-02 15:04"
)

// MaintenanceLock is the observation of the maintenance lock (R16): since
// when it is held and when a tick last found it held. Both are zero while the
// lock is free.
type MaintenanceLock struct {
	HeldSince    time.Time `json:"held_since,omitzero"`
	LastSeenHeld time.Time `json:"last_seen_held,omitzero"`
}

// observe returns the observation after a tick at now found the lock held or
// free. A hold continues only when it was last seen at most maintenanceGap
// ago; otherwise it starts again at now.
func (m MaintenanceLock) observe(now time.Time, held bool) MaintenanceLock {
	if !held {
		return MaintenanceLock{}
	}
	if m.HeldSince.IsZero() || now.Sub(m.LastSeenHeld) > maintenanceGap {
		m.HeldSince = now
	}
	m.LastSeenHeld = now

	return m
}

// stuck reports whether the lock has been held longer than maxHold at now.
func (m MaintenanceLock) stuck(now time.Time, maxHold time.Duration) bool {
	return !m.HeldSince.IsZero() && now.Sub(m.HeldSince) > maxHold
}

func (m MaintenanceLock) encode() (string, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("encode maintenance lock: %w", err)
	}

	return string(b), nil
}

// decodeMaintenance parses a kv value produced by encode; empty means none.
func decodeMaintenance(s string) (MaintenanceLock, error) {
	var m MaintenanceLock
	if s == "" {
		return m, nil
	}
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return MaintenanceLock{}, fmt.Errorf("decode maintenance lock: %w", err)
	}

	return m, nil
}

// observeMaintenance records what the tick's probe found and raises the
// checker error of a lock held longer than MaintenanceMaxHold (FR-027). The
// quiet window itself stays: the lock may belong to a long but legitimate
// job. A failed probe leaves the observation as it was.
func (r *Runner) observeMaintenance(c *cycle, held bool) {
	r.mu.Lock()
	r.maint = r.maint.observe(c.now, held)
	r.maintObserved = true
	m := r.maint
	r.mu.Unlock()

	if m.stuck(c.now, r.opts.MaintenanceMaxHold) {
		c.fail(keyMaintenanceStuck, fmt.Sprintf("maintenance lock held since %s (more than %s)",
			m.HeldSince.In(r.opts.Location).Format(heldLayout), formatHold(r.opts.MaintenanceMaxHold)))
	}
}

// exportMaintenance encodes the observation for kv.
func (r *Runner) exportMaintenance() (string, error) {
	r.mu.Lock()
	m := r.maint
	r.mu.Unlock()

	return m.encode()
}

// restoreMaintenance merges the stored observation into memory. Before any
// tick has probed the lock, the stored one is taken as is (the next tick
// judges its continuity). A tick that found the lock free wins. A hold seen
// since start continues the stored one when it began at most maintenanceGap
// after the lock was last seen held, keeping the earlier start.
func (r *Runner) restoreMaintenance(data string) error {
	stored, err := decodeMaintenance(data)
	if err != nil {
		return err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	switch {
	case stored.HeldSince.IsZero():
	case !r.maintObserved:
		r.maint = stored
	case r.maint.HeldSince.IsZero():
	case r.maint.HeldSince.Sub(stored.LastSeenHeld) <= maintenanceGap && stored.HeldSince.Before(r.maint.HeldSince):
		r.maint.HeldSince = stored.HeldSince
	}

	return nil
}

// formatHold renders a duration without trailing zero units, e.g. 6h, 1h30m.
func formatHold(d time.Duration) string {
	s := d.String()
	if d >= time.Minute && d%time.Minute == 0 {
		s = strings.TrimSuffix(s, "0s")
		if d%time.Hour == 0 {
			s = strings.TrimSuffix(s, "0m")
		}
	}

	return s
}
