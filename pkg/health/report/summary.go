package report

import (
	"fmt"
	"iter"
	"slices"
	"strings"
	"time"

	"github.com/Toshik1978/tgshd/pkg/health/runner"
	"github.com/Toshik1978/tgshd/pkg/health/store"
)

const (
	summaryTitle = "Weekly summary"
	// sinceLayout is the weekday and time of an "asleep since" mark.
	sinceLayout = "Mon 15:04"
)

// SummaryInput is what the weekly summary is built from.
type SummaryInput struct {
	// Host is the name the notifier prefixes the pushed message with
	// (FR-010); the summary leaves room for that prefix.
	Host string
	// Drives are the known drives and DriveStats their drive.* metrics.
	Drives     []Drive
	DriveStats []store.MetricStats
	// Services are the configured checks the services block lists, in order.
	Services []ServiceCheck
	// Now is the time the stamp and round-trip ages are measured against.
	Now time.Time
	// Open are the open problems, as /status shows them.
	Open []runner.OpenCondition
}

// ServiceCheck is one configured check of the services block.
type ServiceCheck struct {
	// Name is the check name, as in the open conditions' keys ("network",
	// "http/api"); Group is its summary_group, empty for none.
	Name  string
	Group string
	// Freshness is set for a freshness check: the stamp it last saw.
	Freshness *FreshnessStamp
	// Modem is set for the modem probe: its round-trip state.
	Modem *ModemState
}

// FreshnessStamp is what a freshness check last saw of its file.
type FreshnessStamp struct {
	// At is the file's timestamp, meaningful when Present.
	At      time.Time
	Present bool
	// ReadOK tells a missing file (true) from an unreadable one (false).
	ReadOK bool
}

// ModemState is the modem probe's round-trip state.
type ModemState struct {
	// LastOK is the last successful round trip; zero means never.
	LastOK time.Time
	// Outstanding is whether a probe was sent and not yet answered.
	Outstanding bool
}

// Summary renders the weekly summary as the plain text of one pushed event:
// per-drive lines (node, mounts, temperature, self-test, wear), a services
// block and the open problems. The notifier adds the host prefix and escapes
// the text; together they fit one Telegram message. When it is too long the
// services section is shortened first (ok lines fold into "… and N more ok",
// then lines from the end become "… N more"), then the drives ("… N more
// drives"), and last the open problems, cut with a trailing ellipsis.
func Summary(in SummaryInput) string {
	limit := max(MaxMessageUnits-units(in.Host+": "), units(summaryTitle+ellipsis))
	drives, services, open := summaryDrives(in), summaryServices(in), summaryOpen(in.Open)

	assemble := func() []string {
		return slices.Concat([]string{summaryTitle}, drives.flat(), services.flat(), open)
	}
	fits := func() bool { return units(strings.Join(assemble(), "\n")) <= limit }

	// Cut order (FR-049): services, then drives, then the open problems by
	// truncateLines, which keeps that section whole while it fits.
	if !fits() {
		services.cut(fits, (*section).foldCandidates)
	}
	if !fits() {
		drives.cut(fits, (*section).dropCandidates)
	}

	return truncateLines(assemble(), limit)
}

// section is a block of the summary: a head line and the lines under it.
// okLines marks the lines that only say "ok" (services).
type section struct {
	head    string
	lines   []string
	okLines []bool
}

func (s *section) flat() []string {
	return append([]string{s.head}, s.lines...)
}

// cut tries the candidate line sets, least cut first, until the summary fits;
// the most cut one stays when none does.
func (s *section) cut(fits func() bool, candidates func(*section) iter.Seq[[]string]) {
	orig := *s
	for c := range candidates(&orig) {
		s.lines = c
		if fits() {
			return
		}
	}
}

// foldCandidates folds ok lines, from the end, into "… and N more ok" one at
// a time; once all are folded, drops the others from the end for "… N more"
// (counting every line that went).
func (s *section) foldCandidates() iter.Seq[[]string] {
	return func(yield func([]string) bool) {
		var others []string
		var oks []int
		for i, l := range s.lines {
			if s.okLines[i] {
				oks = append(oks, i)
			} else {
				others = append(others, l)
			}
		}

		for k := 1; k <= len(oks); k++ {
			kept := s.fold(oks[len(oks)-k:])
			// A fold that does not shrink the section is not a cut.
			if linesUnits(kept) >= linesUnits(s.lines) {
				continue
			}
			if !yield(kept) {
				return
			}
		}

		for j := range slices.Backward(others) {
			if !yield(append(slices.Clone(others[:j]), fmt.Sprintf("%s %d more", ellipsis, len(s.lines)-j))) {
				return
			}
		}
	}
}

// fold returns the lines without the ok lines at the indexes in folded, plus
// the "… and N more ok" line.
func (s *section) fold(folded []int) []string {
	kept := make([]string, 0, len(s.lines))
	for i, l := range s.lines {
		if !slices.Contains(folded, i) {
			kept = append(kept, l)
		}
	}

	return append(kept, fmt.Sprintf("%s and %d more ok", ellipsis, len(folded)))
}

func linesUnits(lines []string) int { return units(strings.Join(lines, "\n")) }

// dropCandidates drops lines from the end for "… N more drives".
func (s *section) dropCandidates() iter.Seq[[]string] {
	return func(yield func([]string) bool) {
		for j := range slices.Backward(s.lines) {
			if !yield(append(slices.Clone(s.lines[:j]), fmt.Sprintf("%s %d more drives", ellipsis, len(s.lines)-j))) {
				return
			}
		}
	}
}

// summaryDrives renders the drives section, one line per drive.
func summaryDrives(in SummaryInput) *section {
	if len(in.Drives) == 0 {
		return &section{head: "Drives: none"}
	}

	byName := make(map[string]store.MetricStats, len(in.DriveStats))
	for _, m := range in.DriveStats {
		byName[m.Metric] = m
	}
	drives := &section{head: "Drives:"}
	for i := range in.Drives {
		d := &in.Drives[i]
		drives.lines = append(drives.lines, d.summaryLine(byName[drivePrefix+d.Serial+wearSuffix]))
	}

	return drives
}

// summaryLine renders a drive: "sda (/, /home) 41°C, self-test passed 12 d
// ago, wear 8%". Values the last read lacked are left out; the self-test
// words are those of /disks. A drive whose node is not known yet (a baseline
// stored before nodes were recorded) is named by bus, model and the last 4
// serial characters instead.
func (d *Drive) summaryLine(wear store.MetricStats) string {
	node := d.Node
	if node == "" {
		node = fmt.Sprintf("%s %s …%s", d.Bus, d.Model, tail(d.Serial))
	}
	mounts := d.Mounts
	if mounts == "" {
		mounts = mountsUnknown
	}
	parts := []string{}
	if d.Missing {
		parts = append(parts, "missing")
	}
	if d.Temp != nil {
		parts = append(parts, fmt.Sprintf("%d°C", *d.Temp))
	}
	if !d.AsleepSince.IsZero() {
		parts = append(parts, "asleep since "+d.AsleepSince.Format(sinceLayout))
	}
	if !d.UnreadableSince.IsZero() {
		parts = append(parts, "unreadable since "+d.UnreadableSince.Format(sinceLayout))
	}
	parts = append(parts, "self-test "+d.SelfTest.text(d.Transport, d.Bus))
	switch {
	case d.Wear != nil:
		parts = append(parts, fmt.Sprintf("wear %d%%", *d.Wear))
	case wear.HasCurrent:
		parts = append(parts, "wear "+unit(current(wear), "%"))
	}

	return fmt.Sprintf("%s (%s) %s", node, mounts, strings.Join(parts, ", "))
}

// isHardwareCheck reports whether the services block leaves the named check
// out: the drives section covers it and its problems appear under the open
// problems. A summary_group on it has no effect.
func isHardwareCheck(name string) bool {
	return name == "disks" || name == "temps" || name == "kernel"
}

// serviceGroup is one line of the services block: a check on its own, or the
// checks that share a summary_group.
type serviceGroup struct {
	label   string
	grouped bool
	members []ServiceCheck
}

// summaryServices renders one line per configured check, or per summary_group
// (placed where its first member is): "ok" or the open problems' texts joined
// with "; ", with the freshness ages and the modem probe state after them.
func summaryServices(in SummaryInput) *section {
	var groups []*serviceGroup
	byGroup := map[string]*serviceGroup{}
	for _, c := range in.Services {
		if isHardwareCheck(c.Name) {
			continue
		}
		if c.Group == "" {
			groups = append(groups, &serviceGroup{label: c.Name, members: []ServiceCheck{c}})
			continue
		}
		g, ok := byGroup[c.Group]
		if !ok {
			g = &serviceGroup{label: c.Group, grouped: true}
			byGroup[c.Group] = g
			groups = append(groups, g)
		}
		g.members = append(g.members, c)
	}
	if len(groups) == 0 {
		return &section{head: "Services: none"}
	}

	services := &section{head: "Services:"}
	for _, g := range groups {
		body := g.text(in.Now, in.Open)
		services.lines = append(services.lines, g.label+": "+body)
		services.okLines = append(services.okLines, body == "ok")
	}

	return services
}

// text is the line's body. Problems come first, then the facts of the
// freshness and modem checks; inside a group a fact is prefixed with its
// check's name, and a group with nothing to show reads "ok".
func (g *serviceGroup) text(now time.Time, open []runner.OpenCondition) string {
	problems := make([]string, 0, len(open))
	var facts []string
	for _, m := range g.members {
		own := openTexts(m.Name, open)
		problems = append(problems, own...)

		switch {
		case m.Freshness != nil:
			facts = append(facts, g.prefixed(m, m.Freshness.ageText(now)))
		case m.Modem != nil:
			facts = append(facts, g.prefixed(m, m.Modem.text(now, len(own) > 0)))
		}
	}

	return joinBody(strings.Join(problems, "; "), facts)
}

// joinBody puts the facts after the problems text; with neither it reads "ok".
func joinBody(problems string, facts []string) string {
	list := strings.Join(facts, "; ")
	switch {
	case problems == "" && list == "":
		return "ok"
	case problems == "":
		return list
	case list != "":
		return problems + "; " + list
	}

	return problems
}

// openTexts returns the texts of the open conditions of the check named name:
// its key is the name or starts with name + "/".
func openTexts(name string, open []runner.OpenCondition) []string {
	var texts []string
	for _, c := range open {
		if c.Key == name || strings.HasPrefix(c.Key, name+"/") {
			texts = append(texts, c.Text)
		}
	}

	return texts
}

// prefixed names the check a fact belongs to inside a group: the part of its
// name after the kind ("freshness/db-dump" reads "db-dump").
func (g *serviceGroup) prefixed(m ServiceCheck, fact string) string {
	if !g.grouped {
		return fact
	}
	_, name, found := strings.Cut(m.Name, "/")
	if !found {
		name = m.Name
	}

	return name + " " + fact
}

// ageText is "<1 h ago", "N h ago" below 48 h, "N d ago", "in the future"
// for a stamp dated after now, "none" for a missing file and "?" for an
// unreadable one.
func (f *FreshnessStamp) ageText(now time.Time) string {
	switch {
	case f.Present && f.At.After(now):
		return "in the future"
	case f.Present:
		return ageText(now.Sub(f.At)) + " ago"
	case f.ReadOK:
		return "none"
	}

	return "?"
}

// text is "last round trip <age> ago" or "last round trip never" (left out
// when the probe has a failure text of its own), then whether a probe is
// outstanding.
func (m *ModemState) text(now time.Time, failing bool) string {
	outstanding := "no probe outstanding"
	if m.Outstanding {
		outstanding = "probe outstanding"
	}
	if failing {
		return outstanding
	}
	trip := "last round trip never"
	if !m.LastOK.IsZero() {
		trip = "last round trip " + ageText(now.Sub(m.LastOK)) + " ago"
	}

	return trip + ", " + outstanding
}

// ageText renders an age as "<1 h", "N h" below 48 h or "N d".
func ageText(d time.Duration) string {
	switch hours := int(d / time.Hour); {
	case hours < 1:
		return "<1 h"
	case hours < 48:
		return fmt.Sprintf("%d h", hours)
	default:
		return fmt.Sprintf("%d d", hours/24)
	}
}

// summaryOpen renders the open problems one per line, as /status does.
func summaryOpen(open []runner.OpenCondition) []string {
	if len(open) == 0 {
		return []string{"Open problems: none"}
	}
	lines := []string{"Open problems:"}
	for _, c := range open {
		lines = append(lines, fmt.Sprintf("%s: %s (since %s)", c.Key, c.Text, c.Since.Format(timeLayout)))
	}

	return lines
}
