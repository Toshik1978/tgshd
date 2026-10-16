package health

import "time"

// Reasons a quiet window is open, shown in /status.
const (
	// ReasonMaintenance means the maintenance lock is held exclusively by another process.
	ReasonMaintenance = "maintenance"
	// ReasonBootGrace means the host booted less than the boot grace period ago.
	ReasonBootGrace = "boot grace"
)

// Quiet reports whether nothing may be judged right now, and why. A held
// maintenance lock takes precedence over the boot grace period. A grace of zero
// or less disables the boot grace window.
func Quiet(maintHeld bool, uptime, grace time.Duration) (bool, string) {
	if maintHeld {
		return true, ReasonMaintenance
	}
	if grace > 0 && uptime < grace {
		return true, ReasonBootGrace
	}

	return false, ""
}
