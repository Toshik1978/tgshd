package memory

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/Toshik1978/tgshd/pkg/health"
)

// keyLimit is the rolled-up limit-audit condition; a joiner's event is keyed
// keyLimit + "/" + its name.
const keyLimit = "memory/limit"

// Docker restart policies that keep a container running: only these
// containers are services the audit counts.
const (
	policyAlways        = "always"
	policyUnlessStopped = "unless-stopped"
)

// gapCapFactor mirrors the engine's: one run adds at most this many intervals
// to the observed bad time.
const gapCapFactor = 1.5

// limitTextUnits bounds the problem text in UTF-16 code units: one Telegram
// message (4096) less room for the "<host>: PROBLEM: " prefix and the
// " (since …)" suffix the notifier adds.
const limitTextUnits = 4096 - 256

// audit remembers the containers counted on the previous run, so one that
// joins an open problem is announced once, and mirrors the engine's observed
// bad time of the current streak (badFor, last) to know whether its PROBLEM is
// open. All of it is persisted (see AuditState), so a restart neither
// announces the set again nor treats a young streak as open.
type audit struct {
	mu     sync.Mutex
	known  map[string]bool
	badFor time.Duration
	last   time.Time
	open   bool
}

// observe adds one non-empty run at now to the streak and reports whether the
// PROBLEM was open before it. Like the engine, a new streak starts at zero,
// each run adds the time since the previous one (at most gapCapFactor
// intervals), and the PROBLEM opens on the run that reaches threshold.
func (a *audit) observe(now time.Time, interval, threshold time.Duration) bool {
	wasOpen := a.open
	if len(a.known) == 0 {
		a.badFor, a.open = 0, false
	} else if !a.last.IsZero() {
		elapsed := now.Sub(a.last)
		if interval > 0 {
			elapsed = min(elapsed, time.Duration(gapCapFactor*float64(interval)))
		}
		a.badFor += max(elapsed, 0)
	}
	a.last = now
	if a.badFor >= threshold {
		a.open = true
	}

	return wasOpen
}

// reset ends the streak: the set is empty.
func (a *audit) reset() {
	a.known = map[string]bool{}
	a.badFor, a.last, a.open = 0, time.Time{}, false
}

// judgeLimits raises one problem naming every running container that Docker
// keeps running (restart policy always or unless-stopped) and that has no
// memory limit, largest first, except those in audit_exclude (FR-018). It
// resolves when none is left; a container joining the set once the PROBLEM is
// open (the set non-empty for threshold) is announced once by an event; one
// that leaves and rejoins is a new join. Without a container runtime the
// audit is skipped; an unreadable list is one checker error and leaves the
// condition and the counted set as they were.
func (c *Check) judgeLimits(ctx context.Context, res *health.CheckResult, top *topLookup) {
	containers, err := c.source.Containers(ctx)
	if errors.Is(err, ErrNoContainers) {
		return
	}
	if err != nil {
		res.Err = errors.Join(res.Err, health.CheckError{Subject: "containers", Kind: kindUnreadable, Err: err})
		return
	}

	counted := make(map[string]bool)
	for _, ctr := range containers {
		if c.counts(ctr) {
			counted[ctr.Name] = true
		}
	}

	c.audit.mu.Lock()
	defer c.audit.mu.Unlock()

	if len(counted) == 0 {
		res.Readings = append(res.Readings, health.Reading{
			Key: keyLimit, Kind: health.State, Verdict: health.Good,
			Text: "every container runs with a memory limit",
		})
		c.audit.reset()

		return
	}

	named := largestFirst(slices.Collect(maps.Keys(counted)), top.services())
	suffix := top.suffix()
	res.Readings = append(res.Readings, health.Reading{
		Key: keyLimit, Kind: health.State, Verdict: health.Bad, Text: limitText(named, suffix),
	})
	if c.audit.observe(c.settings.now(), c.settings.Interval, c.settings.Threshold) {
		for _, n := range named {
			if !c.audit.known[n.name] {
				res.Readings = append(res.Readings, health.Reading{
					Key: keyLimit + "/" + n.name, Kind: health.Event, Verdict: health.Bad,
					Text: fmt.Sprintf("container %s runs without a memory limit", n.name),
				})
			}
		}
	}
	c.audit.known = counted
}

// counts reports whether the audit counts ctr: running (or being restarted
// by Docker, so a crash loop does not flap), kept running by its own Docker
// restart policy, without a memory limit and not excluded.
func (c *Check) counts(ctr Container) bool {
	kept := ctr.RestartPolicy == policyAlways || ctr.RestartPolicy == policyUnlessStopped

	return (ctr.Running || ctr.Restarting) && kept && !ctr.HasLimit && !auditExcluded(c.settings.AuditExclude, ctr.Name)
}

// namedContainer is a counted container with its memory (Anon) from the
// services read; sampled is false when no service matched its name.
type namedContainer struct {
	name    string
	anon    uint64
	sampled bool
}

func (n namedContainer) String() string {
	if !n.sampled {
		return n.name
	}

	return n.name + " " + size(n.anon)
}

// largestFirst orders names by their service's Anon, largest first; ties
// and containers without a sample (last) go by name.
func largestFirst(names []string, services []Service) []namedContainer {
	anon := make(map[string]uint64, len(services))
	for _, s := range services {
		anon[s.Name] = s.Anon
	}
	out := make([]namedContainer, len(names))
	for i, name := range names {
		a, ok := anon[name]
		out[i] = namedContainer{name: name, anon: a, sampled: ok}
	}
	slices.SortFunc(out, func(a, b namedContainer) int {
		if a.sampled != b.sampled {
			if a.sampled {
				return -1
			}

			return 1
		}

		return cmp.Or(cmp.Compare(b.anon, a.anon), strings.Compare(a.name, b.name))
	})

	return out
}

// limitText is "3 containers run without a memory limit: web-app 31 GB,
// worker 3 GB, cache 1.2 GB" plus suffix. Only when that would not fit
// limitTextUnits are the last names cut to "… and K more".
func limitText(named []namedContainer, suffix string) string {
	head := strconv.Itoa(len(named)) + " containers run without a memory limit: "
	if len(named) == 1 {
		head = "1 container runs without a memory limit: "
	}
	parts := make([]string, len(named))
	total := units(head) + units(suffix)
	for i, n := range named {
		parts[i] = n.String()
		total += units(parts[i])
	}
	total += 2 * (len(parts) - 1) // ", " separators
	if total <= limitTextUnits {
		return head + strings.Join(parts, ", ") + suffix
	}

	// Keep the longest prefix that fits with its "… and K more" tail; the
	// running length makes this one pass.
	fixed := units(head) + units(suffix)
	shown, used := 0, 0
	for i, part := range parts {
		width := units(part)
		if i > 0 {
			width += 2
		}
		more := units(fmt.Sprintf(", … and %d more", len(parts)-i-1))
		if fixed+used+width+more > limitTextUnits {
			break
		}
		used += width
		shown = i + 1
	}
	if shown == 0 {
		return head + fmt.Sprintf("… and %d more", len(parts)) + suffix
	}

	return head + strings.Join(parts[:shown], ", ") + fmt.Sprintf(", … and %d more", len(parts)-shown) + suffix
}

// units counts s in UTF-16 code units, as Telegram does.
func units(s string) int { return len(utf16.Encode([]rune(s))) }

// AuditState returns the persistable set of containers the limit audit
// counted on its last run (kv memory.audit), kept apart from the growth
// history.
func (c *Check) AuditState() *AuditState { return &AuditState{audit: &c.audit} }

// AuditState exports and restores the limit audit's counted containers.
type AuditState struct {
	audit *audit
}

// auditRecord is the persisted form of audit.
type auditRecord struct {
	Known  []string      `json:"known"`
	BadFor time.Duration `json:"bad_for"`
	Last   time.Time     `json:"last"`
	Open   bool          `json:"open"`
}

// Export serialises the counted container names and the streak.
func (s *AuditState) Export() ([]byte, error) {
	s.audit.mu.Lock()
	defer s.audit.mu.Unlock()
	data, err := json.Marshal(auditRecord{
		Known: slices.Sorted(maps.Keys(s.audit.known)), BadFor: s.audit.badFor, Last: s.audit.last, Open: s.audit.open,
	})
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
	var rec auditRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return fmt.Errorf("restore memory audit: %w", err)
	}
	s.audit.mu.Lock()
	defer s.audit.mu.Unlock()
	if s.audit.known == nil {
		s.audit.known = make(map[string]bool, len(rec.Known))
	}
	// A union: a restore that lands after a run must not forget what that
	// run counted. The worst case is a joiner that is not announced. The
	// fresher streak wins; an open PROBLEM stays open.
	for _, n := range rec.Known {
		s.audit.known[n] = true
	}
	if rec.Last.After(s.audit.last) {
		s.audit.badFor, s.audit.last = rec.BadFor, rec.Last
	}
	s.audit.open = s.audit.open || rec.Open

	return nil
}

// auditExcluded reports whether name matches an audit_exclude name or
// path.Match glob. Patterns are validated at config load.
func auditExcluded(exclude []string, name string) bool {
	for _, glob := range exclude {
		if ok, _ := path.Match(glob, name); ok {
			return true
		}
	}

	return false
}
