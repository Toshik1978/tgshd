package report

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/Toshik1978/tgshd/pkg/health/store"
)

const (
	notAvailable = "n/a"
	serialTail   = 4

	tempPrefix      = "temp."
	driveTempPrefix = "temp.drive."
	drivePrefix     = "drive."
	wearSuffix      = ".wear"
	writtenSuffix   = ".written_bytes"
	decimalUnit     = 1000.0
	hoursPerDay     = 24
)

// Drive is the per-drive facts shown by /disks that are not metrics.
type Drive struct {
	Node   string
	Bus    string
	Model  string
	Serial string
	// Transport is nvme, sata, sas, usb or other; empty when unknown.
	Transport string
	// Health is the last read's verdict: ok, failing or unknown. When empty,
	// HealthOK decides (the summary sets only that).
	Health   string
	HealthOK bool
	// Missing is set for a known drive that is no longer found; its health
	// then reads "missing".
	Missing bool
	// SparePct and SelfTestAge are nil when unknown (the summary's values).
	SparePct    *float64
	SelfTestAge *time.Duration

	// Temp, Wear, Spare and SpareThreshold are the last read's values; nil
	// reads "unknown", never 0. SpareFloor is the configured floor.
	Temp, Wear, Spare, SpareThreshold *int
	SpareFloor                        int
	SelfTest                          SelfTest
	// LastReadAge is the time since the last successful read; nil when the
	// drive was never read.
	LastReadAge *time.Duration
	// AsleepSince and UnreadableSince are zero when the drive is neither.
	AsleepSince, UnreadableSince time.Time
}

// SelfTest is what /disks knows of a drive's self-test log.
type SelfTest struct {
	// Format is "ok" or "unsupported"; empty means not read yet.
	Format string
	// Result is the newest finished test, "passed" or "failed"; empty when
	// the log has none.
	Result string
	// Age is the time since the last observed pass, or since the drive was
	// first seen when none was.
	Age time.Duration
	// NewestAge is the power-on time since the newest finished test; nil when
	// unknown.
	NewestAge *time.Duration
	// Unobserved is set when no pass has been observed since the drive was
	// first seen, so Age counts from first sight.
	Unobserved bool
}

// Temps renders the /temp reply: one line per temperature sensor with the
// current value and the 7-day min/avg/max. Drive temperatures are left to /disks.
func Temps(stats []store.MetricStats) string {
	var lines []string
	for _, m := range stats {
		if !strings.HasPrefix(m.Metric, tempPrefix) || strings.HasPrefix(m.Metric, driveTempPrefix) {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s: %s (7d min/avg/max %s/%s/%s)",
			escape(strings.TrimPrefix(m.Metric, tempPrefix)), unit(current(m), "°C"),
			whole(m.Min), whole(m.Avg), whole(m.Max)))
	}
	if len(lines) == 0 {
		return "No temperature sensors"
	}

	return Truncate(strings.Join(lines, "\n"))
}

// Disks renders the /disks reply from the drive facts and the metrics
// (temp.drive.*, drive.*). Drives appear by node, bus, model and the last 4
// serial characters. A value the last read lacked reads "unknown"; the
// 7-day figures and the data written come from the metrics and read "n/a"
// (or are left out) without samples.
func Disks(drives []Drive, stats []store.MetricStats) string {
	byName := make(map[string]store.MetricStats, len(stats))
	for _, m := range stats {
		byName[m.Metric] = m
	}

	lines := make([]string, 0, len(drives))
	for i := range drives {
		d := &drives[i]
		written := notAvailable
		if w, ok := byName[drivePrefix+d.Serial+writtenSuffix]; ok && w.HasCurrent {
			written = size(max(w.Current-w.Min, 0))
		}
		lines = append(lines, d.line(byName, written))
	}
	if len(lines) == 0 {
		return "No drives"
	}

	return Truncate(strings.Join(lines, "\n"))
}

// line renders one drive for /disks.
func (d *Drive) line(byName map[string]store.MetricStats, written string) string {
	parts := []string{
		"health " + d.health(),
		"wear " + valueStats(d.Wear, "%", byName[drivePrefix+d.Serial+wearSuffix]),
	}
	if d.hasSpare() {
		parts = append(parts, fmt.Sprintf("spare %s (floor %d%%, drive %s)",
			valueUnit(d.Spare, "%"), d.SpareFloor, valueUnit(d.SpareThreshold, "%")))
	}
	parts = append(parts,
		"temp "+valueStats(d.Temp, "°C", byName[driveTempPrefix+d.Serial]),
		"written 7d "+written,
		"self-test "+d.SelfTest.text(d.Transport, d.Bus),
		"last read "+lastRead(d.LastReadAge),
	)
	if !d.AsleepSince.IsZero() {
		parts = append(parts, "asleep since "+d.AsleepSince.Format(timeLayout))
	}
	if !d.UnreadableSince.IsZero() {
		parts = append(parts, "unreadable since "+d.UnreadableSince.Format(timeLayout))
	}

	return fmt.Sprintf("%s %s %s …%s: %s", escape(d.Node), escape(d.Bus), escape(d.Model),
		escape(tail(d.Serial)), strings.Join(parts, ", "))
}

// hasSpare tells whether the drive reports an available spare: NVMe only.
func (d *Drive) hasSpare() bool {
	return d.Transport == "nvme" || strings.EqualFold(d.Bus, "nvme")
}

// health is the drive's health word: missing, OK, FAILING or unknown.
func (d *Drive) health() string {
	switch {
	case d.Missing:
		return "missing"
	case d.Health == "ok", d.Health == "" && d.HealthOK:
		return "OK"
	case d.Health == "failing", d.Health == "":
		return "FAILING"
	default:
		return "unknown"
	}
}

// text is the self-test words: a log that is not decoded (always so for SAS
// and SCSI) is "not readable", an empty format is "not read yet".
func (s SelfTest) text(transport, bus string) string {
	switch {
	case transport == "sas" || strings.EqualFold(bus, "scsi"),
		s.Format != "" && s.Format != "ok":
		return "not readable"
	case s.Format == "":
		return "not read yet"
	case s.Result == "":
		return "none (first seen " + ago(s.Age) + ")"
	}

	return s.result()
}

// result is the words of a finished test: its power-on age when known,
// otherwise, for a pass, the calendar age (counted from first sight when no
// pass was observed). A failure without an age reads plain "failed".
func (s SelfTest) result() string {
	switch {
	case s.NewestAge != nil:
		return s.Result + " " + ago(*s.NewestAge)
	case s.Result != "passed":
		return s.Result
	case s.Unobserved:
		return "passed before first seen " + ago(s.Age)
	default:
		return "passed " + ago(s.Age)
	}
}

// valueUnit renders a known value with its unit; nil reads "unknown".
func valueUnit(v *int, u string) string {
	if v == nil {
		return "unknown"
	}

	return fmt.Sprintf("%d%s", *v, u)
}

// valueStats renders the current value with the metric's 7-day
// min/avg/max when it has samples: "41°C (7d 35/38/44)".
func valueStats(v *int, u string, m store.MetricStats) string {
	out := valueUnit(v, u)
	if m.N == 0 {
		return out
	}

	return fmt.Sprintf("%s (7d %s/%s/%s)", out, whole(m.Min), whole(m.Avg), whole(m.Max))
}

// lastRead renders the time since the last read; a read in the last minute
// is "<1 m ago".
func lastRead(d *time.Duration) string {
	if d == nil {
		return notAvailable
	}

	return ago(*d)
}

// Ago renders a duration as ago does, for the one-shot CLI.
func Ago(d time.Duration) string { return ago(d) }

// ago renders a duration as "12 d ago", "5 h ago", "3 m ago" or "<1 m ago".
func ago(d time.Duration) string {
	switch {
	case d >= hoursPerDay*time.Hour:
		return fmt.Sprintf("%d d ago", int(d/(hoursPerDay*time.Hour)))
	case d >= time.Hour:
		return fmt.Sprintf("%d h ago", int(d/time.Hour))
	case d >= time.Minute:
		return fmt.Sprintf("%d m ago", int(d/time.Minute))
	default:
		return "<1 m ago"
	}
}

// unit appends the unit to a known value; "n/a" stays bare.
func unit(v, u string) string {
	if v == notAvailable {
		return v
	}

	return v + u
}

func current(m store.MetricStats) string {
	if !m.HasCurrent {
		return notAvailable
	}

	return whole(m.Current)
}

func whole(v float64) string {
	return fmt.Sprintf("%.0f", v)
}

func tail(serial string) string {
	r := []rune(serial)
	if len(r) > serialTail {
		r = r[len(r)-serialTail:]
	}

	return string(r)
}

// size formats bytes in decimal units (B, kB, MB, GB, TB, PB).
func size(b float64) string {
	units := [...]string{"B", "kB", "MB", "GB", "TB", "PB"}
	i := 0
	for b >= decimalUnit && i < len(units)-1 {
		b /= decimalUnit
		i++
	}
	if i == 0 || math.Round(b) >= 100 {
		return fmt.Sprintf("%.0f %s", b, units[i])
	}

	return fmt.Sprintf("%.1f %s", b, units[i])
}
