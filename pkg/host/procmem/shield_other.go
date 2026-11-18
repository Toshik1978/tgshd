//go:build !linux

package procmem

import (
	"errors"
	"fmt"
)

// Shield is a no-op stand-in off Linux: oom_score_adj and mlockall are
// Linux-only, and the daemon runs only there. It exists so cmd builds, lints
// and tests on a developer's Mac.
type Shield struct{}

// NewShield creates the stand-in Shield.
func NewShield() *Shield {
	return &Shield{}
}

// Apply reports that shielding is unavailable on this OS rather than
// pretending it took effect.
func (*Shield) Apply() error {
	return fmt.Errorf("starvation shield: %w", errors.ErrUnsupported)
}
