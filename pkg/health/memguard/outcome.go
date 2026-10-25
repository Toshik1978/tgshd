package memguard

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Toshik1978/tgshd/pkg/health"
)

// Top is the largest memory holder named in a report: a service, or a process
// when no service holds most of the memory.
type Top struct {
	Name string
	// PID is set for a process.
	PID  int
	Size uint64
	// Slice is the cgroup of a process, shown in no_culprit reports.
	Slice string
}

// Message is one guard outcome ready to push as an event.
type Message struct {
	// Key is memory/guard/<service>.
	Key  string
	Text string
	// Mode is the delivery channel of the message.
	Mode health.Mode
}

// hostKey is the key of an outcome that names no service.
const hostKey = "memory/guard/host"

// Message renders o as an event message (contracts/messages.md, "Guard
// outcomes"). Restarted and the would_* previews go through Channel, so a
// report-mode preview never wakes the operator; every outcome where tgshd did
// not fix the problem goes through FailedChannel.
func (c Config) Message(o Outcome) Message {
	m := Message{Key: hostKey, Mode: c.FailedChannel, Text: text(o)}
	if name := o.Service.Name; name != "" {
		m.Key = "memory/guard/" + name
	}
	if o.Kind == OutcomeRestarted || o.Kind == OutcomeWouldRestart || o.Kind == OutcomeWouldStop {
		m.Mode = c.Channel
	}

	return m
}

// text renders the message of o.
func text(o Outcome) string {
	switch o.Kind {
	case OutcomeRestarted:
		return "memory: restarted " + subject(o) + " (" + growth(o) + "); " + host(o, false)
	case OutcomeWouldRestart:
		return "memory: would restart " + subject(o) + " (" + growth(o) + "); " + host(o, false) + preview
	case OutcomeStopped:
		return "memory: stopped " + subject(o) + ": " + repeat(o) + " (" + growth(o) + "); " + available(o)
	case OutcomeWouldStop:
		subj, grew := subject(o), growth(o)

		return fmt.Sprintf("memory: would stop %s: %s (%s); %s%s", subj, repeat(o), grew, host(o, false), preview)
	case OutcomeKilled:
		return killed(o)
	case OutcomeNotRecovered:
		return notRecovered(o)
	case OutcomeProtected:
		return "memory: " + host(o, false) + "; " + o.Service.Name + " is growing (" + growthOf(o) +
			") but is never-touch, tgshd did not act"
	case OutcomeNoCulprit:
		return noCulprit(o)
	case OutcomeActionFailed:
		return actionFailed(o)
	}

	return "memory: unknown guard outcome " + string(o.Kind)
}

func actionFailed(o Outcome) string {
	reason := "unknown error"
	if o.Err != nil {
		reason = o.Err.Error()
	}

	return "memory: could not " + string(o.Action) + " " + o.Service.Name + " (" + formatSize(o.Size) + "): " + reason +
		"; " + available(o)
}

func notRecovered(o Outcome) string {
	hostNow := host(o, true)
	s := fmt.Sprintf("memory: %s %s (%s before) did not free memory; %s",
		gerund(o.Action), o.Service.Name, formatSize(o.Size), hostNow)
	if o.Top.Name != "" {
		s += "; top: " + describe(o.Top)
	}

	return s
}

func noCulprit(o Outcome) string {
	s := "memory: " + host(o, false) + "; no service is growing"
	if o.Top.Name != "" {
		s += "; top process: " + describe(o.Top)
		if o.Top.Slice != "" {
			s += " in " + o.Top.Slice
		}
	}

	return s
}

const preview = " — intervene is report"

// subject names the service with its kind: "container web-app" or "service
// backup.service".
func subject(o Outcome) string {
	if o.Service.Kind == KindContainer {
		return "container " + o.Service.Name
	}

	return "service " + o.Service.Name
}

// runtime is what answers a restart or stop of the service.
func runtime(o Outcome) string {
	if o.Service.Kind == KindContainer {
		return "docker"
	}

	return "systemctl"
}

func gerund(a Action) string {
	switch a {
	case ActionStop:
		return "stopping"
	case ActionKill:
		return "killing"
	case ActionRestart:
		return "restarting"
	}

	return string(a) + "ing"
}

// growth is "grew 12→31 GB in 10 min".
func growth(o Outcome) string {
	return "grew " + growthOf(o)
}

// growthOf is "12→31 GB in 10 min".
func growthOf(o Outcome) string {
	span := strings.Replace(formatSpan(o.Window), "m", " min", 1)

	return fmt.Sprintf("%s→%s in %s", formatGB(o.Before, false), formatGB(o.Size, true), span)
}

// repeat is "ran away again 2h10m after tgshd restarted it".
func repeat(o Outcome) string {
	return "ran away again " + formatSpan(o.At.Sub(o.LastAction)) + " after tgshd restarted it"
}

// available is "host 4% available".
func available(o Outcome) string {
	return "host " + percent(o.AvailablePct) + " available"
}

// host is "host 4% available, stalled 35%"; still turns it into "host still …".
func host(o Outcome, still bool) string {
	s := "host "
	if still {
		s += "still "
	}

	return s + percent(o.AvailablePct) + " available, stalled " + percent(o.FullAvg10)
}

func percent(v float64) string {
	return strconv.FormatFloat(v, 'f', 0, 64) + "%"
}

// describe is "web-app 31 GB" or "python3 (pid 4242) 40 GB".
func describe(t Top) string {
	name := t.Name
	if t.PID != 0 {
		name += " (pid " + strconv.Itoa(t.PID) + ")"
	}

	return name + " " + formatSize(t.Size)
}

// formatSize shows GB with one decimal below 10 GB and none above.
func formatSize(b uint64) string {
	return formatGB(b, true)
}

// formatGB shows b in GB with one decimal below 10 GB and none above. With
// unit it ends in " GB" ("3.0 GB"); without, a whole number drops its ".0", as
// the low end of a range does ("3→22 GB").
func formatGB(b uint64, unit bool) string {
	v := float64(b) / float64(1<<30)
	n := strconv.FormatFloat(v, 'f', 0, 64)
	if one := strconv.FormatFloat(v, 'f', 1, 64); v < 10 && one != "10.0" {
		n = one
	}
	if !unit {
		return strings.TrimSuffix(n, ".0")
	}

	return n + " GB"
}

// formatSpan shows d rounded to whole minutes, at least one: "10m", "2h10m".
func formatSpan(d time.Duration) string {
	mins := max(int((d+30*time.Second)/time.Minute), 1)
	h, m := mins/60, mins%60
	switch {
	case h == 0:
		return strconv.Itoa(m) + "m"
	case m == 0:
		return strconv.Itoa(h) + "h"
	}

	return strconv.Itoa(h) + "h" + strconv.Itoa(m) + "m"
}

// killed renders the killed outcome: the runtime either did not answer within
// the action timeout or could not be reached at all.
func killed(o Outcome) string {
	why := fmt.Sprintf("did not answer within %ds", int(o.Timeout.Seconds()))
	if o.Unreachable {
		why = "could not be reached"
	}

	return fmt.Sprintf("memory: killed %s through the kernel: %s %s (%s); %s",
		subject(o), runtime(o), why, growth(o), available(o))
}
