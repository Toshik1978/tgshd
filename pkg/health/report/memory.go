package report

import (
	"strconv"
	"strings"
	"time"
)

// guardLayout is the day and time of the guard's last action ("2 Oct 01:12").
const guardLayout = "2 Jan 15:04"

const gibibyte = float64(1 << 30)

// Memory is the memory block of /status and the memory line of the weekly
// summary (contracts/messages.md).
type Memory struct {
	// Unreadable is set when the host memory could not be read; the figures
	// are then zero.
	Unreadable bool
	// Total and Available are the host memory, SwapTotal and SwapUsed its
	// swap, in bytes.
	Total, Available    uint64
	SwapTotal, SwapUsed uint64
	// Stalled is the full avg10 memory pressure in percent; HasPressure is
	// false on a host without PSI.
	Stalled     float64
	HasPressure bool
	// Top are the largest services, largest first.
	Top []Consumer
	// Guard is the guard line of /status; nil leaves it out.
	Guard *Guard
}

// Consumer is a service and the memory it holds, in bytes.
type Consumer struct {
	Name string
	Size uint64
}

// Guard is the memory guard's mode ("act", "report", "off" or why it does not
// run) and its latest intervention.
type Guard struct {
	Mode string
	// Last is nil before the guard has done anything.
	Last *GuardAction
}

// GuardAction is one intervention of the guard: Action is "restart", "stop"
// or "kill"; Report marks one only reported (intervene: report).
type GuardAction struct {
	Action  string
	Service string
	Report  bool
	At      time.Time
}

// memoryLines renders the /status memory block.
func memoryLines(m *Memory) []string {
	lines := []string{"Memory: " + m.figures()}
	if !m.Unreadable && len(m.Top) > 0 {
		top := make([]string, 0, len(m.Top))
		for _, c := range m.Top {
			top = append(top, escape(c.Name)+" "+gigabytes(c.Size))
		}
		lines = append(lines, "Top: "+strings.Join(top, ", "))
	}
	if m.Guard != nil {
		lines = append(lines, "Guard: "+m.Guard.text())
	}

	return lines
}

// figures is "45% available (28 of 62 GB), swap 20% used, stalled 0%", or
// "unreadable".
func (m *Memory) figures() string {
	if m.Unreadable {
		return "unreadable"
	}
	parts := []string{
		percentOf(m.Available, m.Total) + " available (" + gbNumber(m.Available) + " of " + gigabytes(m.Total) + ")",
	}
	if m.SwapTotal > 0 {
		parts = append(parts, "swap "+percentOf(m.SwapUsed, m.SwapTotal)+" used")
	}
	if m.HasPressure {
		parts = append(parts, "stalled "+strconv.FormatFloat(m.Stalled, 'f', 0, 64)+"%")
	}

	return strings.Join(parts, ", ")
}

// summaryFact is the summary's memory line body: the figures and the largest
// service.
func (m *Memory) summaryFact() string {
	fact := m.figures()
	if !m.Unreadable && len(m.Top) > 0 {
		fact += "; top: " + m.Top[0].Name + " " + gigabytes(m.Top[0].Size)
	}

	return fact
}

// text is "report — last: would restart web-app 2 Oct 01:12", or "off".
func (g *Guard) text() string {
	mode := escape(g.Mode)
	if g.Mode == "off" {
		return mode
	}
	last := noneText
	if a := g.Last; a != nil {
		last = actionText(a.Action, a.Report) + " " + escape(a.Service) + " " + a.At.Format(guardLayout)
	}

	return mode + " — last: " + last
}

// actionText is "restarted", "stopped" or "killed", or "would restart" (and
// so on) for a reported action.
func actionText(action string, report bool) string {
	if report {
		return "would " + action
	}
	switch action {
	case "restart":
		return "restarted"
	case "stop":
		return "stopped"
	case "kill":
		return "killed"
	}

	return action
}

func percentOf(part, whole uint64) string {
	pct := 0.0
	if whole > 0 {
		pct = float64(part) * 100 / float64(whole)
	}

	return strconv.FormatFloat(pct, 'f', 0, 64) + "%"
}

// gigabytes is "31 GB" or "5.6 GB".
func gigabytes(b uint64) string { return gbNumber(b) + " GB" }

// gbNumber shows one decimal below 10 GB and none above; a whole value has no
// decimal.
func gbNumber(b uint64) string {
	v := float64(b) / gibibyte
	if one := strconv.FormatFloat(v, 'f', 1, 64); v < 10 && one != "10.0" {
		return strings.TrimSuffix(one, ".0")
	}

	return strconv.FormatFloat(v, 'f', 0, 64)
}
