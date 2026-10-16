package report

import (
	"fmt"
	"slices"
	"strings"

	"github.com/Toshik1978/tgshd/pkg/health/runner"
	"github.com/Toshik1978/tgshd/pkg/health/store"
)

const (
	summaryTitle = "Weekly summary"
	unavailable  = "unavailable"
)

// SummaryInput is what the weekly summary is built from.
type SummaryInput struct {
	// Host is the name the notifier prefixes the pushed message with
	// (FR-010); the summary leaves room for that prefix.
	Host string
	// Drives are the known drives and DriveStats their drive.* metrics; a
	// non-nil DrivesErr makes the drives section read "unavailable".
	Drives     []Drive
	DriveStats []store.MetricStats
	DrivesErr  error
	// Checks are the names of the configured checks; the services block counts
	// the containers, http, freshness and command ones.
	Checks []string
	// Open are the open problems, as /status shows them.
	Open []runner.OpenCondition
}

// Summary renders the weekly summary as the plain text of one pushed event:
// per-drive lines (health, wear, data written in the last 7 days), a services
// block and the open problems. The notifier adds the host prefix and escapes
// the text; together they fit one Telegram message, trailing lines being cut
// with an ellipsis.
func Summary(in SummaryInput) string {
	lines := slices.Concat(
		[]string{summaryTitle}, summaryDrives(in), summaryServices(in.Checks, in.Open), summaryOpen(in.Open),
	)
	limit := max(MaxMessageUnits-units(in.Host+": "), units(summaryTitle+ellipsis))

	return truncateLines(lines, limit)
}

// summaryDrives renders the drives section; drives appear by bus, model and
// the last 4 serial characters.
func summaryDrives(in SummaryInput) []string {
	switch {
	case in.DrivesErr != nil:
		return []string{"Drives: " + unavailable}
	case len(in.Drives) == 0:
		return []string{"Drives: none"}
	}

	byName := make(map[string]store.MetricStats, len(in.DriveStats))
	for _, m := range in.DriveStats {
		byName[m.Metric] = m
	}
	lines := []string{"Drives:"}
	for i := range in.Drives {
		d := &in.Drives[i]
		written := notAvailable
		if w, ok := byName[drivePrefix+d.Serial+writtenSuffix]; ok && w.HasCurrent {
			written = size(max(w.Current-w.Min, 0))
		}
		lines = append(lines, fmt.Sprintf("%s %s …%s: health %s, wear %s, written 7d %s",
			d.Bus, d.Model, tail(d.Serial), d.health(),
			unit(current(byName[drivePrefix+d.Serial+wearSuffix]), "%"), written))
	}

	return lines
}

// summaryServices renders one line per configured service check type: OK, or
// the number of its open conditions and how many of its checks have none.
func summaryServices(checks []string, open []runner.OpenCondition) []string {
	var lines []string
	for _, kind := range [...]string{"containers", "http", "freshness", "command"} {
		total, okChecks, problems := 0, 0, 0
		for _, name := range checks {
			if name != kind && !strings.HasPrefix(name, kind+"/") {
				continue
			}
			total++
			n := countOpen(name, open)
			problems += n
			if n == 0 {
				okChecks++
			}
		}
		switch {
		case total == 0:
		case problems == 0:
			lines = append(lines, fmt.Sprintf("%s: OK (%s)", kind, plural(total, "check")))
		default:
			lines = append(lines, fmt.Sprintf("%s: %d open (%d of %d checks OK)", kind, problems, okChecks, total))
		}
	}
	if len(lines) == 0 {
		return []string{"Services: none"}
	}

	return append([]string{"Services:"}, lines...)
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

// countOpen counts the open conditions of the check named name: its key is
// the name or starts with name + "/".
func countOpen(name string, open []runner.OpenCondition) int {
	n := 0
	for _, c := range open {
		if c.Key == name || strings.HasPrefix(c.Key, name+"/") {
			n++
		}
	}

	return n
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}

	return fmt.Sprintf("%d %ss", n, noun)
}
