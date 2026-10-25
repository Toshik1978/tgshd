package memory

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/Toshik1978/tgshd/pkg/health"
)

const keyLimitPrefix = "memory/limit/"

// audit remembers the containers judged on the previous run, so one that
// vanished is resolved once.
type audit struct {
	mu    sync.Mutex
	known map[string]bool
}

// judgeLimits points out running containers without a memory limit (FR-018).
// A container that stops, gains a limit or disappears resolves its condition.
// Without a container runtime the audit is skipped; an unreadable list is one
// checker error and leaves the conditions as they were.
func (c *Check) judgeLimits(ctx context.Context, res *health.CheckResult) {
	containers, err := c.source.Containers(ctx)
	if errors.Is(err, ErrNoContainers) {
		return
	}
	if err != nil {
		res.Err = errors.Join(res.Err, health.CheckError{Subject: "containers", Kind: kindUnreadable, Err: err})
		return
	}

	c.audit.mu.Lock()
	defer c.audit.mu.Unlock()

	seen := make(map[string]bool, len(containers))
	for _, ctr := range containers {
		if slices.Contains(c.settings.AuditExclude, ctr.Name) {
			continue
		}
		seen[ctr.Name] = true

		reading := health.Reading{Key: keyLimitPrefix + ctr.Name, Kind: health.State, Verdict: health.Good}
		switch {
		case !ctr.Running:
			reading.Text = fmt.Sprintf("container %s stopped", ctr.Name)
		case ctr.HasLimit:
			reading.Text = fmt.Sprintf("container %s has a memory limit", ctr.Name)
		default:
			reading.Verdict = health.Bad
			reading.Text = fmt.Sprintf("container %s has no memory limit", ctr.Name)
		}
		res.Readings = append(res.Readings, reading)
	}

	// Gone from the host: resolve once.
	for _, name := range slices.Sorted(maps.Keys(c.audit.known)) {
		if !seen[name] {
			res.Readings = append(res.Readings, health.Reading{
				Key: keyLimitPrefix + name, Kind: health.State, Verdict: health.Good,
				Text: fmt.Sprintf("container %s stopped", name),
			})
		}
	}
	c.audit.known = seen
}
