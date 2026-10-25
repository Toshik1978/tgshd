package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/Toshik1978/tgshd/pkg/health"
)

const keyLimitPrefix = "memory/limit/"

// audit remembers the containers judged on the previous run, so one that
// vanished is resolved once. The set is persisted (see AuditState), so a
// container removed while the daemon was down is resolved after the restart.
type audit struct {
	mu    sync.Mutex
	known map[string]bool
}

// judgeLimits points out running containers without a memory limit (FR-018).
// A container that stops, gains a limit or disappears resolves its condition.
// Without a container runtime the audit is skipped; an unreadable list is one
// checker error and leaves the conditions as they were.
func (c *Check) judgeLimits(ctx context.Context, res *health.CheckResult, top *topLookup) {
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
			reading.Text = fmt.Sprintf("container %s has no memory limit%s", ctr.Name, top.suffix())
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

// AuditState returns the persistable set of containers the limit audit has
// judged (kv memory.audit), kept apart from the growth history.
func (c *Check) AuditState() *AuditState { return &AuditState{audit: &c.audit} }

// AuditState exports and restores the limit audit's known containers.
type AuditState struct {
	audit *audit
}

// Export serialises the known container names.
func (s *AuditState) Export() ([]byte, error) {
	s.audit.mu.Lock()
	defer s.audit.mu.Unlock()
	data, err := json.Marshal(slices.Sorted(maps.Keys(s.audit.known)))
	if err != nil {
		return nil, fmt.Errorf("export memory audit: %w", err)
	}

	return data, nil
}

// Restore loads names written by Export. Empty data is a no-op.
func (s *AuditState) Restore(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	var names []string
	if err := json.Unmarshal(data, &names); err != nil {
		return fmt.Errorf("restore memory audit: %w", err)
	}
	s.audit.mu.Lock()
	defer s.audit.mu.Unlock()
	s.audit.known = make(map[string]bool, len(names))
	for _, n := range names {
		s.audit.known[n] = true
	}

	return nil
}
