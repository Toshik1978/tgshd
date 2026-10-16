package report

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Toshik1978/tgshd/pkg/health/runner"
	"github.com/Toshik1978/tgshd/pkg/health/store"
)

func TestSummary(t *testing.T) {
	t.Parallel()

	const s1, s2 = "TEST0000000001", "TEST0000000002"
	at := time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC)
	drives := []Drive{
		{Bus: "NVMe", Model: "Example NVMe SSD", Serial: s1, HealthOK: true},
		{Bus: "SATA", Model: "Example <SSD>", Serial: s2},
		{Bus: "NVMe", Model: "Example NVMe SSD", Serial: "TEST0000000003", HealthOK: true, Missing: true},
	}
	stats := []store.MetricStats{
		{Metric: "drive." + s1 + ".wear", Current: 7, HasCurrent: true},
		{Metric: "drive." + s1 + ".written_bytes", Current: 5.5e12, Min: 5.0e12, HasCurrent: true},
	}
	checks := []string{"command/backup", "containers", "disks", "http/api", "http/web-app", "network"}
	open := []runner.OpenCondition{
		{Key: "http/web-app", Text: "HTTP 500 & <down>", Since: at},
		{Key: "disks/" + s2 + "/health", Text: "SMART overall health FAILED", Since: at},
	}

	cases := []struct {
		name string
		in   SummaryInput
		want string
	}{
		{
			"full",
			SummaryInput{Host: "box", Drives: drives, DriveStats: stats, Checks: checks, Open: open},
			"Weekly summary\n" +
				"Drives:\n" +
				"NVMe Example NVMe SSD …0001: health OK, wear 7%, written 7d 500 GB\n" +
				"SATA Example <SSD> …0002: health FAILING, wear n/a, written 7d n/a\n" +
				"NVMe Example NVMe SSD …0003: health missing, wear n/a, written 7d n/a\n" +
				"Services:\n" +
				"containers: OK (1 check)\n" +
				"http: 1 open (1 of 2 checks OK)\n" +
				"command: OK (1 check)\n" +
				"Open problems:\n" +
				"http/web-app: HTTP 500 & <down> (since 2026-10-12 08:00)\n" +
				"disks/" + s2 + "/health: SMART overall health FAILED (since 2026-10-12 08:00)",
		},
		{
			"empty",
			SummaryInput{Host: "box"},
			"Weekly summary\nDrives: none\nServices: none\nOpen problems: none",
		},
		{
			"drives unavailable",
			SummaryInput{
				Host: "box &co", Drives: drives, DrivesErr: errors.New("store closed"),
				Checks: []string{"freshness/backup"},
			},
			"Weekly summary\nDrives: unavailable\nServices:\nfreshness: OK (1 check)\nOpen problems: none",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := Summary(tc.in); got != tc.want {
				t.Errorf("got\n%s\nwant\n%s", got, tc.want)
			}
		})
	}
}

func TestSummaryServicesCountOpenConditionsPerType(t *testing.T) {
	t.Parallel()

	in := SummaryInput{
		Host:   "box",
		Checks: []string{"containers"},
		Open: []runner.OpenCondition{
			{Key: "containers/db", Text: "exited"},
			{Key: "containers/web", Text: "unhealthy"},
			// A key that merely starts with the name is not the check's.
			{Key: "containersx/a", Text: "other"},
		},
	}
	if got := Summary(in); !strings.Contains(got, "\ncontainers: 2 open (0 of 1 checks OK)\n") {
		t.Errorf("got %q", got)
	}
}

func TestSummaryNeverShowsTheFullSerial(t *testing.T) {
	t.Parallel()

	const serial = "TEST0000000009"
	in := SummaryInput{
		Host:   "box",
		Drives: []Drive{{Bus: "NVMe", Model: "Example NVMe SSD", Serial: serial, HealthOK: true}},
	}
	got := Summary(in)
	if strings.Contains(got, serial) || !strings.Contains(got, "…0009") {
		t.Errorf("got %q", got)
	}
}

func TestSummaryFitsOneMessage(t *testing.T) {
	t.Parallel()

	const host = "a-rather-long-host-name.example.org"
	open := make([]runner.OpenCondition, 300)
	for i := range open {
		open[i] = runner.OpenCondition{Key: "http/web-app", Text: strings.Repeat("x", 30) + " 😀 & <b>"}
	}
	got := Summary(SummaryInput{Host: host, Checks: []string{"http/web-app"}, Open: open})
	// The notifier prefixes "<host>: "; Telegram counts the text after
	// entity parsing, so the escaping it adds does not count.
	if n := units(host+": ") + units(got); n > MaxMessageUnits || !strings.HasSuffix(got, "…") {
		t.Fatalf("not cut: %d units with the prefix", n)
	}
	if n := units(host+": ") + units(got); n < MaxMessageUnits-100 {
		t.Errorf("cut too early: %d units with the prefix", n)
	}
	if !strings.HasPrefix(got, "Weekly summary\nDrives: none\nServices:\nhttp: 300 open (0 of 1 checks OK)\n") {
		t.Errorf("the head must survive the cut: %q", got[:80])
	}
	// Whole lines are dropped: the last kept line is complete.
	lines := strings.Split(strings.TrimSuffix(got, "…"), "\n")
	if last := lines[len(lines)-1]; !strings.HasSuffix(last, "(since 0001-01-01 00:00)") {
		t.Errorf("last line cut mid-way: %q", last)
	}
}

func TestSummaryKeepsTheTitleForAnOverlongHost(t *testing.T) {
	t.Parallel()

	got := Summary(SummaryInput{Host: strings.Repeat("h", MaxMessageUnits)})
	if got != "Weekly summary…" {
		t.Errorf("got %q", got)
	}
}
