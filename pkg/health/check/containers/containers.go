// Package containers implements the container health check: containers that
// Docker keeps running (restart policy always or unless-stopped), minus an
// operator exclude list, must be running, not restarting, not unhealthy and
// not crash-looping.
package containers

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"slices"
	"sync"
	"time"

	"github.com/Toshik1978/tgshd/pkg/health"
)

const (
	policyAlways        = "always"
	policyUnlessStopped = "unless-stopped"
	statusRestarting    = "restarting"
	healthUnhealthy     = "unhealthy"
)

// Container is the subset of container state the check needs. Only these
// fields may ever reach a message (labels, commands and environment never do).
type Container struct {
	Name          string
	RestartPolicy string
	Status        string
	Health        string
	Running       bool
	RestartCount  int
	StartedAt     time.Time
}

// Source lists the containers known to the container engine.
type Source interface {
	List(ctx context.Context) ([]Container, error)
}

// observation is what the previous run saw of one watched container.
type observation struct {
	Stopped      bool      `json:"stopped"`
	Suspect      bool      `json:"suspect"`
	RestartCount int       `json:"restart_count"`
	StartedAt    time.Time `json:"started_at"`
}

// Check is the containers check. It keeps the previous observation of every
// watched container to detect restarts between runs.
type Check struct {
	source    Source
	exclude   []string
	threshold time.Duration
	now       func() time.Time

	mu   sync.Mutex
	prev map[string]observation
}

// New builds the check. exclude holds path.Match globs on container names and
// is validated here. A restarted container stays Bad until it has been up for
// threshold, measured from its StartedAt. A nil now means time.Now.
func New(source Source, exclude []string, threshold time.Duration, now func() time.Time) (*Check, error) {
	for _, glob := range exclude {
		if _, err := path.Match(glob, ""); err != nil {
			return nil, fmt.Errorf("exclude glob %q: %w", glob, err)
		}
	}

	if now == nil {
		now = time.Now
	}

	return &Check{
		source: source, exclude: exclude, threshold: threshold, now: now,
		prev: make(map[string]observation),
	}, nil
}

// Run lists containers and judges each watched one. A listing failure makes
// the whole result Unknown; it never reports a container as gone.
func (c *Check) Run(ctx context.Context) health.CheckResult {
	list, err := c.source.List(ctx)
	if err != nil {
		return health.CheckResult{Err: fmt.Errorf("list containers: %w", err)}
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	present := make(map[string]struct{}, len(list))
	watched := make(map[string]struct{}, len(list))
	var readings []health.Reading
	for _, ct := range list {
		present[ct.Name] = struct{}{}
		if !c.isWatched(ct) {
			continue
		}
		watched[ct.Name] = struct{}{}
		readings = append(readings, c.judge(ct))
	}

	var gone []string
	for name := range c.prev {
		if _, ok := watched[name]; !ok {
			gone = append(gone, name)
		}
	}
	slices.Sort(gone)
	for _, name := range gone {
		reason := "no longer exists"
		if _, ok := present[name]; ok {
			reason = "no longer watched"
		}
		delete(c.prev, name)
		readings = append(readings, health.Reading{
			Key:     key(name),
			Verdict: health.Good,
			Text:    fmt.Sprintf("container %s %s", name, reason),
		})
	}

	return health.CheckResult{Readings: readings}
}

// Export serialises the previous observations so a restart is not mistaken
// for a crash loop.
func (c *Check) Export() ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	data, err := json.Marshal(c.prev)
	if err != nil {
		return nil, fmt.Errorf("marshal observations: %w", err)
	}

	return data, nil
}

// Restore loads observations produced by Export.
func (c *Check) Restore(data []byte) error {
	prev := make(map[string]observation)
	if err := json.Unmarshal(data, &prev); err != nil {
		return fmt.Errorf("unmarshal observations: %w", err)
	}
	if prev == nil {
		prev = make(map[string]observation)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.prev = prev

	return nil
}

func (c *Check) isWatched(ct Container) bool {
	if ct.RestartPolicy != policyAlways && ct.RestartPolicy != policyUnlessStopped {
		return false
	}
	for _, glob := range c.exclude {
		if ok, _ := path.Match(glob, ct.Name); ok {
			return false
		}
	}

	return true
}

// isSuspect decides whether the container is within the crash-loop window.
func (c *Check) isSuspect(ct Container, prev observation, seen bool) bool {
	suspect := seen && prev.Suspect

	// Only a genuinely stopped previous state suppresses the restart delta.
	changed := prev.RestartCount != ct.RestartCount || !prev.StartedAt.Equal(ct.StartedAt)
	if seen && !prev.Stopped && changed {
		suspect = true
	}

	if suspect && ct.Running && ct.Status != statusRestarting && c.now().Sub(ct.StartedAt) >= c.threshold {
		suspect = false
	}

	return suspect
}

// judge produces the reading for one watched container and records it.
func (c *Check) judge(ct Container) health.Reading {
	prev, seen := c.prev[ct.Name]
	restarting := ct.Status == statusRestarting
	suspect := c.isSuspect(ct, prev, seen)

	reading := health.Reading{Key: key(ct.Name), Verdict: health.Bad}

	switch {
	case restarting:
		reading.Text = fmt.Sprintf("container %s is restarting", ct.Name)
	case !ct.Running:
		reading.Text = fmt.Sprintf("container %s is not running (%s)", ct.Name, ct.Status)
	case ct.Health == healthUnhealthy:
		reading.Text = fmt.Sprintf("container %s is unhealthy", ct.Name)
	case suspect:
		reading.Text = fmt.Sprintf("container %s restarted recently (restart count %d)", ct.Name, ct.RestartCount)
	default:
		reading.Verdict = health.Good
		reading.Text = fmt.Sprintf("container %s is running", ct.Name)
	}

	c.prev[ct.Name] = observation{
		Stopped:      !ct.Running && !restarting,
		Suspect:      suspect,
		RestartCount: ct.RestartCount,
		StartedAt:    ct.StartedAt,
	}

	return reading
}

func key(name string) string {
	return "containers/" + name
}
