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

// limitTextUnits bounds the problem text in UTF-16 code units: one Telegram
// message (4096) less room for the "<host>: PROBLEM: " prefix and the
// " (since …)" suffix the notifier adds.
const limitTextUnits = 4096 - 256

// maxAuditRuns bounds the runs whose texts the audit remembers. Past that
// many dropped runs in a row it falls back to known, the last matched set,
// so a joiner may be announced twice but is never missed.
const maxAuditRuns = 8

// audit decides which counted containers are joiners. The engine alone says
// whether the PROBLEM is open; the audit only matches the text the engine
// last applied to the run that produced it, so a joiner counts as announced
// only once a run that announced it (or a PROBLEM naming it) was applied.
type audit struct {
	mu sync.Mutex
	// known is the counted set of the last run the engine is known to have
	// applied (persisted as kv memory.audit); it answers when the engine's
	// text matches no remembered run, e.g. after a restart.
	known map[string]bool
	// runs are this process's latest runs that raised the problem, oldest
	// first: each Bad text with the set it counted.
	runs []auditRun
}

// auditRun is one run's problem text and the containers it counted.
type auditRun struct {
	text  string
	names map[string]bool
}

// applied returns the counted set of the last run the engine applied, found
// by the engine's text; older runs are forgotten, as the engine will never
// go back to them. The earliest run with that text is taken, so two runs
// with one text (possible only past the cut) announce again rather than miss.
// Without a match it returns known.
func (a *audit) applied(text string) map[string]bool {
	for i, run := range a.runs {
		if run.text == text {
			a.runs = a.runs[i:]
			a.known = run.names

			return run.names
		}
	}

	return a.known
}

// remember adds a run's text and counted set, keeping the latest
// maxAuditRuns.
func (a *audit) remember(text string, names map[string]bool) {
	a.runs = append(a.runs, auditRun{text: text, names: names})
	if over := len(a.runs) - maxAuditRuns; over > 0 {
		a.runs = slices.Delete(a.runs, 0, over)
	}
}

// reset forgets everything: the set is empty and the condition resolves.
func (a *audit) reset() {
	a.known = map[string]bool{}
	a.runs = nil
}

// judgeLimits raises one problem naming every running container that Docker
// keeps running (restart policy always or unless-stopped) and that has no
// memory limit, largest first, except those in audit_exclude (FR-018). It
// resolves when none is left. While the engine has the PROBLEM open, a
// counted container the last applied run did not count is announced once by
// an event; one that leaves and rejoins is a new join. Without a container
// runtime the audit is skipped; an unreadable list is one checker error and
// leaves the condition and the counted set as they were.
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
	text := limitText(named, top.suffix())
	res.Readings = append(res.Readings, health.Reading{
		Key: keyLimit, Kind: health.State, Verdict: health.Bad, Text: text,
	})
	open, last := c.settings.open(keyLimit)
	known := c.audit.applied(last)
	// An open PROBLEM with no set to compare against (its kv entry lost)
	// takes the set silently rather than announcing all of it.
	if open && len(known) > 0 {
		for _, n := range named {
			if !known[n.name] {
				res.Readings = append(res.Readings, health.Reading{
					Key: keyLimit + "/" + n.name, Kind: health.Event, Verdict: health.Bad,
					Text: fmt.Sprintf("container %s runs without a memory limit", n.name),
				})
			}
		}
	}
	c.audit.remember(text, counted)
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

// AuditState returns the persistable set of containers counted by the last
// run the engine applied (kv memory.audit), kept apart from the growth
// history.
func (c *Check) AuditState() *AuditState { return &AuditState{check: c} }

// AuditState exports and restores the limit audit's counted containers.
type AuditState struct {
	check *Check
}

// auditRecord is the persisted form of audit. The previous form also held a
// mirrored streak (bad_for, last, open); those fields are ignored on restore.
type auditRecord struct {
	Known []string `json:"known"`
}

// Export serialises the set counted by the last run the engine applied. It
// runs at the checkpoint, after the tick's runs were applied, so the engine's
// text names the run whose set is saved.
func (s *AuditState) Export() ([]byte, error) {
	a := &s.check.audit
	_, last := s.check.settings.open(keyLimit)
	a.mu.Lock()
	known := a.applied(last)
	data, err := json.Marshal(auditRecord{Known: slices.Sorted(maps.Keys(known))})
	a.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("export memory audit: %w", err)
	}

	return data, nil
}

// Restore loads names written by Export (or by the previous form). Empty
// data is a no-op.
func (s *AuditState) Restore(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	var rec auditRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return fmt.Errorf("restore memory audit: %w", err)
	}
	a := &s.check.audit
	a.mu.Lock()
	defer a.mu.Unlock()
	// A union: a restore that lands after a run must not forget what that
	// run counted. The worst case is a joiner that is not announced. A copy,
	// as known may be a remembered run's set.
	known := maps.Clone(a.known)
	if known == nil {
		known = make(map[string]bool, len(rec.Known))
	}
	for _, n := range rec.Known {
		known[n] = true
	}
	a.known = known

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
