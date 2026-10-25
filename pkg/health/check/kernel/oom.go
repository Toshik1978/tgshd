package kernel

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/Toshik1978/tgshd/pkg/health"
)

const (
	// oomKey is the key of the out-of-memory kill events.
	oomKey = "kernel/oom"
	// maxOOMEvents caps the kill events of one Run (and of the previous-boot tail); the rest
	// are folded into one "and N more" message.
	maxOOMEvents = 5

	constraintMemcg = "CONSTRAINT_MEMCG"

	kib = 1024
	mib = 1024 * kib
	gib = 1024 * mib
)

var (
	// oomHeaderRe matches the record the kernel logs when it picks a victim.
	oomHeaderRe = regexp.MustCompile(
		`^oom-kill:constraint=(\w+),.*\btask_memcg=([^,]*),task=(.*),pid=(\d+)(?:,uid=\d+)?$`,
	)
	// oomKilledRe matches the record that follows it when the victim is killed.
	oomKilledRe = regexp.MustCompile(
		`^(?:Memory cgroup out of memory|Out of memory): Killed process (\d+) \((.*)\) ` +
			`total-vm:\d+kB(?:, anon-rss:(\d+)kB)?`,
	)
)

// Service is where an out-of-memory victim ran. An empty Name means no particular service
// (the root cgroup, or an unknown one).
type Service struct {
	Name      string
	Container bool // Name is a container name rather than a systemd unit or cgroup
}

// ServiceResolver maps the task_memcg cgroup path of an out-of-memory kill to the service the
// victim belonged to.
type ServiceResolver interface {
	Resolve(cgroup string) Service
}

// ContainerLookup returns the name of the container with the given id.
type ContainerLookup func(id string) (string, bool)

// CgroupResolver returns a ServiceResolver that reads a cgroup path: a docker-<id>.scope (or
// /docker/<id>) segment is a container, named by lookup (the shortened id when lookup is nil
// or does not know it); otherwise the first <unit>.service segment is the unit; the root
// cgroup is no service; any other path stands for itself.
func CgroupResolver(lookup ContainerLookup) cgroupResolver {
	return cgroupResolver{lookup: lookup}
}

type cgroupResolver struct {
	lookup ContainerLookup
}

// Resolve implements ServiceResolver.
func (r cgroupResolver) Resolve(cgroup string) Service {
	segs := strings.Split(strings.Trim(cgroup, "/"), "/")
	for i := range segs {
		if id, ok := containerID(segs, i); ok {
			return r.container(id)
		}
	}
	for _, seg := range segs {
		if strings.HasSuffix(seg, ".service") {
			return Service{Name: seg}
		}
	}
	if len(segs) == 1 && segs[0] == "" {
		return Service{}
	}

	return Service{Name: "/" + strings.Join(segs, "/")}
}

// container names the container with the given id, by its short id when unknown.
func (r cgroupResolver) container(id string) Service {
	if r.lookup != nil {
		if name, found := r.lookup(id); found && name != "" {
			return Service{Name: name, Container: true}
		}
	}

	return Service{Name: id[:min(len(id), 12)], Container: true}
}

// containerID returns the container id that segment i of a cgroup path stands for:
// docker-<id>.scope (systemd driver) or <id> under docker (cgroupfs driver).
func containerID(segs []string, i int) (string, bool) {
	id, ok := strings.CutPrefix(segs[i], "docker-")
	if ok {
		id, ok = strings.CutSuffix(id, ".scope")
	} else if i > 0 && segs[i-1] == "docker" {
		id, ok = segs[i], true
	}

	return id, ok && id != ""
}

// oomKill is one process the kernel killed for lack of memory.
type oomKill struct {
	constraint string
	cgroup     string
	hasCgroup  bool // a header record named the cgroup
	task       string
	pid        string
	rssKB      uint64
	hasRSS     bool
}

// oomPairer pairs each "oom-kill:" record with the "Killed process" record that follows it.
// A "Killed process" record with no header (older kernels) is still a kill, with an unknown
// cgroup; a header never followed by a kill is dropped.
type oomPairer struct {
	header *oomKill
}

// feed takes the next kernel message and returns the kill it completes, if any.
func (p *oomPairer) feed(msg string) (oomKill, bool) {
	if m := oomHeaderRe.FindStringSubmatch(msg); m != nil {
		p.header = &oomKill{constraint: m[1], cgroup: m[2], hasCgroup: true, task: m[3], pid: m[4]}

		return oomKill{}, false
	}
	m := oomKilledRe.FindStringSubmatch(msg)
	if m == nil {
		return oomKill{}, false
	}

	kill := oomKill{task: m[2], pid: m[1]}
	if h := p.header; h != nil && h.pid == m[1] {
		kill = *h
		kill.task = m[2]
	}
	p.header = nil
	if m[3] != "" {
		if kb, err := strconv.ParseUint(m[3], 10, 64); err == nil {
			kill.rssKB, kill.hasRSS = kb, true
		}
	}

	return kill, true
}

// oomReadings turns the kills into events, at most maxOOMEvents of them, then one message for
// the rest. prefix is "Kernel" or "Kernel (previous boot)".
func oomReadings(res ServiceResolver, kills []oomKill, prefix string) []health.Reading {
	var out []health.Reading
	for i, k := range kills {
		if i == maxOOMEvents {
			rest := len(kills) - maxOOMEvents
			noun := "kills"
			if rest == 1 {
				noun = "kill"
			}
			out = append(out, event(oomKey, fmt.Sprintf("%s: … and %d more out-of-memory %s", prefix, rest, noun)))

			break
		}
		out = append(out, event(oomKey, oomText(res, k, prefix)))
	}

	return out
}

// oomText words one kill.
func oomText(res ServiceResolver, k oomKill, prefix string) string {
	var svc Service
	if k.hasCgroup {
		svc = res.Resolve(k.cgroup)
	}
	victim := fmt.Sprintf("killed %s (pid %s)", k.task, k.pid)
	held := ""
	if k.hasRSS {
		held = ", " + memSize(k.rssKB*kib)
	}

	if k.constraint == constraintMemcg {
		who := "a cgroup"
		switch {
		case svc.Name == "":
		case svc.Container:
			who = "container " + svc.Name
		default:
			who = svc.Name
		}

		return fmt.Sprintf("%s: %s hit its memory limit, %s%s", prefix, who, victim, held)
	}

	where := ""
	switch {
	case svc.Name == "":
	case svc.Container:
		where = " in container " + svc.Name
	default:
		where = " in " + svc.Name
	}

	return fmt.Sprintf("%s: out of memory, %s%s%s", prefix, victim, where, held)
}

// memSize is "54 GB", "7.9 GB" or, below a gibibyte, "512 MB".
func memSize(b uint64) string {
	if b < gib {
		return strconv.FormatFloat(float64(b)/mib, 'f', 0, 64) + " MB"
	}
	v := float64(b) / gib
	if one := strconv.FormatFloat(v, 'f', 1, 64); v < 10 && one != "10.0" {
		return strings.TrimSuffix(one, ".0") + " GB"
	}

	return strconv.FormatFloat(v, 'f', 0, 64) + " GB"
}
