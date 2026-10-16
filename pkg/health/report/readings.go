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
	Bus      string
	Model    string
	Serial   string
	HealthOK bool
	// Missing is set for a known drive that is no longer found; its health
	// then reads "missing".
	Missing bool
	// SparePct and SelfTestAge are nil when unknown.
	SparePct    *float64
	SelfTestAge *time.Duration
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
// (temp.drive.*, drive.*). Drives appear by bus, model and the last 4 serial
// characters; unknown values read "n/a".
func Disks(drives []Drive, stats []store.MetricStats) string {
	byName := make(map[string]store.MetricStats, len(stats))
	for _, m := range stats {
		byName[m.Metric] = m
	}

	lines := make([]string, 0, len(drives))
	for _, d := range drives {
		written := notAvailable
		if w, ok := byName[drivePrefix+d.Serial+writtenSuffix]; ok && w.HasCurrent {
			written = size(max(w.Current-w.Min, 0))
		}
		lines = append(
			lines,
			fmt.Sprintf("%s %s …%s: health %s, wear %s, spare %s, temp %s, written 7d %s, self-test %s",
				escape(d.Bus), escape(d.Model), escape(tail(d.Serial)), d.health(),
				unit(current(byName[drivePrefix+d.Serial+wearSuffix]), "%"), unit(pct(d.SparePct), "%"),
				unit(current(byName[driveTempPrefix+d.Serial]), "°C"), written, age(d.SelfTestAge)),
		)
	}
	if len(lines) == 0 {
		return "No drives"
	}

	return Truncate(strings.Join(lines, "\n"))
}

// health is the drive's health word: missing, OK or FAILING.
func (d *Drive) health() string {
	switch {
	case d.Missing:
		return "missing"
	case d.HealthOK:
		return "OK"
	default:
		return "FAILING"
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

func pct(p *float64) string {
	if p == nil {
		return notAvailable
	}

	return whole(*p)
}

func tail(serial string) string {
	r := []rune(serial)
	if len(r) > serialTail {
		r = r[len(r)-serialTail:]
	}

	return string(r)
}

func age(d *time.Duration) string {
	switch {
	case d == nil:
		return notAvailable
	case *d >= hoursPerDay*time.Hour:
		return fmt.Sprintf("%dd ago", int(*d/(hoursPerDay*time.Hour)))
	case *d >= time.Hour:
		return fmt.Sprintf("%dh ago", int(*d/time.Hour))
	default:
		return fmt.Sprintf("%dm ago", max(int(*d/time.Minute), 0))
	}
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
