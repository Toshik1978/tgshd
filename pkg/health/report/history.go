package report

import (
	"strings"
	"time"

	"github.com/Toshik1978/tgshd/pkg/health/store"
)

// DefaultPeriod is the /history period used when none is given.
const DefaultPeriod = "7d"

// ParsePeriod maps a /history period (24h, 7d or 30d) to its duration.
func ParsePeriod(s string) (time.Duration, bool) {
	switch s {
	case "24h":
		return 24 * time.Hour, true
	case "7d":
		return 7 * 24 * time.Hour, true
	case "30d":
		return 30 * 24 * time.Hour, true
	}

	return 0, false
}

// HistoryUsage is the reply to a malformed /history command.
func HistoryUsage() string {
	return "Usage: /history [check] [24h|7d|30d] (default 7d)"
}

// History renders alert log records (newest first) one per line as
// "<time> <KIND> <key>: <text>" in loc, or "No alerts in <period>".
func History(alerts []store.Alert, period string, loc *time.Location) string {
	if len(alerts) == 0 {
		return "No alerts in " + escape(period)
	}

	lines := make([]string, 0, len(alerts))
	for _, a := range alerts {
		kind := strings.ToUpper(strings.ReplaceAll(string(a.Kind), "_", " "))
		lines = append(lines, a.At.In(loc).Format(timeLayout)+" "+escape(kind)+" "+escape(a.Key)+": "+escape(a.Text))
	}

	return Truncate(strings.Join(lines, "\n"))
}
