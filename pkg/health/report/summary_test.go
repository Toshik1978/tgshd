package report

import (
	"fmt"
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
		{Node: "nvme0n1", Mounts: "/", Bus: "NVMe", Model: "Example NVMe SSD", Serial: s1, Health: "ok"},
		{Node: "sda", Mounts: "-", Bus: "SATA", Model: "Example <SSD>", Serial: s2, Health: "failing"},
		{Node: "nvme1n1", Bus: "NVMe", Model: "Example NVMe SSD", Serial: "TEST0000000003", Missing: true},
	}
	stats := []store.MetricStats{
		{Metric: "drive." + s1 + ".wear", Current: 7, HasCurrent: true},
		{Metric: "drive." + s1 + ".written_bytes", Current: 5.5e12, Min: 5.0e12, HasCurrent: true},
	}
	checks := services("command/backup", "containers", "http/api", "http/web-app", "network")
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
			SummaryInput{Host: "box", Drives: drives, DriveStats: stats, Services: checks, Open: open},
			"Weekly summary\n" +
				"Drives:\n" +
				"nvme0n1 (/) self-test not read yet, wear 7%\n" +
				"sda (-) self-test not read yet\n" +
				"nvme1n1 (?) missing, self-test not read yet\n" +
				"Services:\n" +
				"command/backup: ok\n" +
				"containers: ok\n" +
				"http/api: ok\n" +
				"http/web-app: HTTP 500 & <down>\n" +
				"network: ok\n" +
				"Open problems:\n" +
				"http/web-app: HTTP 500 & <down> (since 2026-10-12 08:00)\n" +
				"disks/" + s2 + "/health: SMART overall health FAILED (since 2026-10-12 08:00)",
		},
		{
			"empty",
			SummaryInput{Host: "box"},
			"Weekly summary\nDrives: none\nServices: none\nOpen problems: none",
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

func TestSummaryServiceLineTexts(t *testing.T) {
	t.Parallel()

	in := SummaryInput{
		Host:     "box",
		Services: services("containers", "http/api"),
		Open: []runner.OpenCondition{
			{Key: "containers/db", Text: "exited"},
			{Key: "containers/web", Text: "unhealthy"},
			// A key that merely starts with the name is not the check's.
			{Key: "containersx/a", Text: "other"},
		},
	}
	want := "Services:\ncontainers: exited; unhealthy\nhttp/api: ok\n"
	if got := Summary(in); !strings.Contains(got, want) {
		t.Errorf("got %q, want it to contain %q", got, want)
	}
}

func TestSummaryServiceGroups(t *testing.T) {
	t.Parallel()

	in := SummaryInput{
		Host: "box",
		Services: []ServiceCheck{
			{Name: "http/app", Group: "web"},
			{Name: "network"},
			{Name: "containers", Group: "web"},
			{Name: "command/queue", Group: "jobs"},
			{Name: "command/mail", Group: "jobs"},
		},
		Open: []runner.OpenCondition{
			{Key: "command/mail", Text: "exit status 2"},
			{Key: "command/queue", Text: "no output"},
		},
	}
	// A group sits where its first member is and joins the members' texts.
	want := "Services:\nweb: ok\nnetwork: ok\njobs: no output; exit status 2\n"
	if got := Summary(in); !strings.Contains(got, want) {
		t.Errorf("got %q, want it to contain %q", got, want)
	}
}

func TestSummaryFreshnessAge(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC)
	fresh := func(ago time.Duration) *FreshnessStamp {
		return &FreshnessStamp{At: now.Add(-ago), Present: true, ReadOK: true}
	}
	cases := []struct {
		name  string
		svcs  []ServiceCheck
		open  []runner.OpenCondition
		lines string
	}{
		{
			"under an hour",
			[]ServiceCheck{{Name: "freshness/db-dump", Freshness: fresh(59 * time.Minute)}},
			nil,
			"freshness/db-dump: <1 h ago",
		},
		{
			"hours",
			[]ServiceCheck{{Name: "freshness/db-dump", Freshness: fresh(30 * time.Hour)}},
			nil,
			"freshness/db-dump: 30 h ago",
		},
		{
			"47 hours is still hours",
			[]ServiceCheck{{Name: "freshness/db-dump", Freshness: fresh(47*time.Hour + 59*time.Minute)}},
			nil,
			"freshness/db-dump: 47 h ago",
		},
		{
			"days",
			[]ServiceCheck{{Name: "freshness/db-dump", Freshness: fresh(72 * time.Hour)}},
			nil,
			"freshness/db-dump: 3 d ago",
		},
		{
			"a stamp from the future",
			[]ServiceCheck{{Name: "freshness/db-dump", Freshness: fresh(-time.Hour)}},
			nil,
			"freshness/db-dump: in the future",
		},
		{
			"a stamp days in the future",
			[]ServiceCheck{{Name: "freshness/db-dump", Freshness: fresh(-72 * time.Hour)}},
			nil,
			"freshness/db-dump: in the future",
		},
		{
			"missing",
			[]ServiceCheck{{Name: "freshness/db-dump", Freshness: &FreshnessStamp{ReadOK: true}}},
			nil,
			"freshness/db-dump: none",
		},
		{
			"unreadable",
			[]ServiceCheck{{Name: "freshness/db-dump", Freshness: &FreshnessStamp{}}},
			nil,
			"freshness/db-dump: ?",
		},
		{
			"with a problem",
			[]ServiceCheck{{Name: "freshness/db-dump", Freshness: fresh(60 * time.Hour)}},
			[]runner.OpenCondition{{Key: "freshness/db-dump", Text: "stamp too old"}},
			"freshness/db-dump: stamp too old; 2 d ago",
		},
		{"group prefixes the names", []ServiceCheck{
			{Name: "freshness/db-dump", Group: "backups", Freshness: fresh(30 * time.Hour)},
			{Name: "freshness/offsite-copy", Group: "backups", Freshness: fresh(10 * time.Minute)},
		}, nil, "backups: db-dump 30 h ago; offsite-copy <1 h ago"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := Summary(SummaryInput{Host: "box", Now: now, Services: tc.svcs, Open: tc.open})
			if !strings.Contains(got, "Services:\n"+tc.lines+"\n") {
				t.Errorf("got %q, want the line %q", got, tc.lines)
			}
		})
	}
}

func TestSummaryModemProbe(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC)
	cases := []struct {
		name  string
		probe ModemState
		open  []runner.OpenCondition
		want  string
	}{
		{
			"round trip, none outstanding",
			ModemState{LastOK: now.Add(-72 * time.Hour)},
			nil, "modem_probe: last round trip 3 d ago, no probe outstanding",
		},
		{
			"outstanding",
			ModemState{LastOK: now.Add(-2 * time.Hour), Outstanding: true},
			nil, "modem_probe: last round trip 2 h ago, probe outstanding",
		},
		{"never", ModemState{}, nil, "modem_probe: last round trip never, no probe outstanding"},
		{
			"failure text",
			ModemState{LastOK: now.Add(-9 * 24 * time.Hour)},
			[]runner.OpenCondition{{Key: "modem_probe", Text: "no reply for 9 d"}},
			"modem_probe: no reply for 9 d; no probe outstanding",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			in := SummaryInput{
				Host: "box", Now: now, Open: tc.open,
				Services: []ServiceCheck{{Name: "modem_probe", Modem: &tc.probe}},
			}
			if got := Summary(in); !strings.Contains(got, "Services:\n"+tc.want+"\n") {
				t.Errorf("got %q, want the line %q", got, tc.want)
			}
		})
	}
}

// services builds plain service checks (no group, no stamps) from names.
func services(names ...string) []ServiceCheck {
	out := make([]ServiceCheck, len(names))
	for i, n := range names {
		out[i] = ServiceCheck{Name: n}
	}

	return out
}

// TestSummaryNeverShowsTheFullSerial: a drive whose node is not known yet (a
// baseline stored before nodes were recorded) is named by bus, model and the
// last 4 serial characters in place of the node, never the full serial.
func TestSummaryNeverShowsTheFullSerial(t *testing.T) {
	t.Parallel()

	const serial = "TEST0000000009"
	in := SummaryInput{
		Host:   "box",
		Drives: []Drive{{Bus: "NVMe", Model: "Example NVMe SSD", Serial: serial, Missing: true}},
	}
	got := Summary(in)
	want := "Drives:\nNVMe Example NVMe SSD …0009 (?) missing, self-test not read yet\n"
	if strings.Contains(got, serial) || !strings.Contains(got, want) {
		t.Errorf("got %q, want it to contain %q", got, want)
	}
}

func TestSummaryFitsOneMessage(t *testing.T) {
	t.Parallel()

	const host = "a-rather-long-host-name.example.org"
	open := make([]runner.OpenCondition, 300)
	for i := range open {
		open[i] = runner.OpenCondition{Key: "http/web-app", Text: strings.Repeat("x", 30) + " 😀 & <b>"}
	}
	got := Summary(SummaryInput{Host: host, Services: services("network"), Open: open})
	// The notifier prefixes "<host>: "; Telegram counts the text after
	// entity parsing, so the escaping it adds does not count.
	if n := units(host+": ") + units(got); n > MaxMessageUnits || !strings.HasSuffix(got, "…") {
		t.Fatalf("not cut: %d units with the prefix", n)
	}
	if n := units(host+": ") + units(got); n < MaxMessageUnits-100 {
		t.Errorf("cut too early: %d units with the prefix", n)
	}
	if !strings.HasPrefix(got, "Weekly summary\nDrives: none\nServices:\nnetwork: ok\nOpen problems:\n") {
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

func TestSummaryDriveLine(t *testing.T) {
	t.Parallel()

	day := 24 * time.Hour
	cases := []struct {
		name  string
		drive Drive
		want  string
	}{
		{
			"temperature, passed test and wear",
			Drive{
				Node: "sda", Mounts: "/, /home", Temp: new(41), Wear: new(8),
				SelfTest: SelfTest{Format: "ok", Result: "passed", Age: 12 * day},
			},
			"sda (/, /home) 41°C, self-test passed 12 d ago, wear 8%",
		},
		{
			"nil temperature and wear are omitted, not zero",
			Drive{Node: "sdb", Mounts: "-", SelfTest: SelfTest{Format: "ok", Result: "passed", Age: 30 * day}},
			"sdb (-) self-test passed 30 d ago",
		},
		{
			"mounts unknown",
			Drive{Node: "sdc", SelfTest: SelfTest{Format: "unsupported"}},
			"sdc (?) self-test not readable",
		},
		{
			"self-test not read yet",
			Drive{Node: "sdd", Mounts: "/data"},
			"sdd (/data) self-test not read yet",
		},
		{
			"unobserved pass",
			Drive{
				Node: "sde", Mounts: "-",
				SelfTest: SelfTest{Format: "ok", Result: "passed", Age: 3 * day, Unobserved: true},
			},
			"sde (-) self-test passed before first seen 3 d ago",
		},
		{
			"failed test",
			Drive{Node: "sdf", Mounts: "-", SelfTest: SelfTest{Format: "ok", Result: "failed"}},
			"sdf (-) self-test failed",
		},
		{
			"no test yet",
			Drive{Node: "sdg", Mounts: "-", SelfTest: SelfTest{Format: "ok", Age: 2 * day}},
			"sdg (-) self-test none (first seen 2 d ago)",
		},
		{
			"NVMe drive",
			Drive{
				Node: "nvme0n1", Mounts: "/var/lib", Temp: new(48), Wear: new(3),
				SelfTest: SelfTest{Format: "ok", Result: "passed", Age: 2 * day},
			},
			"nvme0n1 (/var/lib) 48°C, self-test passed 2 d ago, wear 3%",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := Summary(SummaryInput{Host: "box", Drives: []Drive{tc.drive}})
			if want := "Weekly summary\nDrives:\n" + tc.want + "\nServices: none\nOpen problems: none"; got != want {
				t.Errorf("got\n%s\nwant\n%s", got, want)
			}
		})
	}
}

func TestSummaryAsleepSince(t *testing.T) {
	t.Parallel()

	asleep := time.Date(2026, 10, 13, 14, 0, 0, 0, time.UTC) // a Tuesday
	unread := time.Date(2026, 10, 12, 9, 10, 0, 0, time.UTC) // a Monday
	day := 24 * time.Hour
	cases := []struct {
		name  string
		drive Drive
		want  string
	}{
		{
			"asleep",
			Drive{
				Node:        "sdb",
				Mounts:      "-",
				AsleepSince: asleep,
				SelfTest:    SelfTest{Format: "ok", Result: "passed", Age: 30 * day},
			},
			"sdb (-) asleep since Tue 14:00, self-test passed 30 d ago",
		},
		{
			"unreadable",
			Drive{Node: "sdc", UnreadableSince: unread, SelfTest: SelfTest{Format: "unsupported"}},
			"sdc (?) unreadable since Mon 09:10, self-test not readable",
		},
		{
			"missing",
			Drive{
				Node:     "sdd",
				Mounts:   "-",
				Missing:  true,
				SelfTest: SelfTest{Format: "ok", Result: "passed", Age: day},
			},
			"sdd (-) missing, self-test passed 1 d ago",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := Summary(SummaryInput{Host: "box", Drives: []Drive{tc.drive}})
			if !strings.Contains(got, "\n"+tc.want+"\n") {
				t.Errorf("got\n%s\nwant line %q", got, tc.want)
			}
		})
	}
}

func TestSummarySATAWear(t *testing.T) {
	t.Parallel()

	// A SATA SSD's wear comes from the drive's reading, as an NVMe one does.
	d := Drive{
		Node: "sda", Bus: "SATA", Transport: "sata", Mounts: "/", Wear: new(8),
		SelfTest: SelfTest{Format: "ok", Result: "passed", Age: 24 * time.Hour},
	}
	got := Summary(SummaryInput{Host: "box", Drives: []Drive{d}})
	if want := "\nsda (/) self-test passed 1 d ago, wear 8%\n"; !strings.Contains(got, want) {
		t.Errorf("got\n%s\nwant line %q", got, want)
	}
	if strings.Contains(got, "spare") {
		t.Errorf("a SATA drive must not show spare: %q", got)
	}
}

// summaryUnits is the size of a summary as Telegram counts it, with the host
// prefix the notifier adds.
func summaryUnits(host, text string) int { return units(host+": ") + units(text) }

func manyServices(n int) []ServiceCheck {
	out := make([]ServiceCheck, n)
	for i := range out {
		out[i] = ServiceCheck{Name: fmt.Sprintf("command/job-%03d", i)}
	}

	return out
}

func manyDrives(n int) []Drive {
	out := make([]Drive, n)
	for i := range out {
		out[i] = Drive{
			Node:   fmt.Sprintf("sd%c", 'a'+i%26),
			Mounts: "/srv/" + strings.Repeat("x", 40),
			Serial: fmt.Sprintf(
				"TEST%010d",
				i,
			),
			SelfTest: SelfTest{Format: "ok", Result: "passed", Age: 24 * time.Hour},
		}
	}

	return out
}

func countLines(text, prefix string) int {
	n := 0
	for l := range strings.SplitSeq(text, "\n") {
		if strings.HasPrefix(l, prefix) {
			n++
		}
	}

	return n
}

func openFor(svcs []ServiceCheck, at time.Time) []runner.OpenCondition {
	out := make([]runner.OpenCondition, len(svcs))
	for i := range svcs {
		out[i] = runner.OpenCondition{Key: svcs[i].Name, Text: "exit status 2", Since: at}
	}

	return out
}

func TestSummaryCutOrder(t *testing.T) {
	t.Parallel()

	const host = "box.example.org"
	at := time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC)
	api := []runner.OpenCondition{{Key: "http/api", Text: "status 502, expected 200-399", Since: at}}
	apiLine := "http/api: status 502, expected 200-399 (since 2026-10-12 08:00)"

	t.Run("fits untouched", func(t *testing.T) {
		t.Parallel()

		got := Summary(SummaryInput{Host: host, Drives: manyDrives(2), Services: manyServices(3), Open: api})
		if strings.Contains(got, "more") || strings.Contains(got, "…") {
			t.Errorf("cut a summary that fits:\n%s", got)
		}
	})

	t.Run("ok service lines fold first", func(t *testing.T) {
		t.Parallel()

		got := Summary(SummaryInput{Host: host, Drives: manyDrives(3), Services: manyServices(300), Open: api})
		if n := summaryUnits(host, got); n > MaxMessageUnits || n < MaxMessageUnits-120 {
			t.Fatalf("size %d, want just under %d", n, MaxMessageUnits)
		}
		kept := countLines(got, "command/job-")
		want := fmt.Sprintf("\n… and %d more ok\nOpen problems:", 300-kept)
		if kept == 0 || !strings.Contains(got, want) {
			t.Errorf("want a fold line for %d lines after %d kept:\n%s", 300-kept, kept, got)
		}
		if countLines(got, "sd") != 3 || !strings.HasSuffix(got, apiLine) {
			t.Errorf("drives or open problems touched:\n%s", got)
		}
		if strings.Contains(got, " more\n") || strings.Contains(got, "more drives") {
			t.Errorf("cut beyond the ok lines:\n%s", got)
		}
	})

	t.Run("lines from the end become N more", func(t *testing.T) {
		t.Parallel()

		svcs := manyServices(55)
		open := openFor(svcs, at)
		got := Summary(SummaryInput{Host: host, Drives: manyDrives(3), Services: svcs, Open: open})
		kept := countLines(got, "command/job-") - len(open) // the open section repeats every name
		if kept <= 0 || kept >= 55 || !strings.Contains(got, fmt.Sprintf("\n… %d more\n", 55-kept)) {
			t.Fatalf("want %d kept service lines and an N more line:\n%s", kept, got)
		}
		if countLines(got, "sd") != 3 || strings.Contains(got, "more drives") ||
			!strings.HasSuffix(got, "exit status 2 (since 2026-10-12 08:00)") {
			t.Errorf("drives or open problems touched:\n%s", got)
		}
		if summaryUnits(host, got) > MaxMessageUnits {
			t.Errorf("too long: %d", summaryUnits(host, got))
		}
	})
}

func TestSummaryCutOrderDrivesThenOpen(t *testing.T) {
	t.Parallel()

	const host = "box.example.org"
	at := time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC)
	api := []runner.OpenCondition{{Key: "http/api", Text: "status 502, expected 200-399", Since: at}}
	apiLine := "http/api: status 502, expected 200-399 (since 2026-10-12 08:00)"

	t.Run("drives are cut after the services", func(t *testing.T) {
		t.Parallel()

		got := Summary(SummaryInput{Host: host, Drives: manyDrives(60), Services: manyServices(2), Open: api})
		kept := countLines(got, "sd")
		if kept == 0 || kept >= 60 ||
			!strings.Contains(got, fmt.Sprintf("\n… %d more drives\nServices:\n… and 2 more ok\n", 60-kept)) {
			t.Fatalf("want %d kept drives and the services folded:\n%s", kept, got)
		}
		if !strings.HasSuffix(got, apiLine) || summaryUnits(host, got) > MaxMessageUnits {
			t.Errorf("open problems touched or too long:\n%s", got)
		}
	})

	t.Run("open problems are cut last", func(t *testing.T) {
		t.Parallel()

		open := make([]runner.OpenCondition, 200)
		for i := range open {
			open[i] = runner.OpenCondition{Key: fmt.Sprintf("command/job-%03d", i), Text: "exit status 2", Since: at}
		}
		got := Summary(
			SummaryInput{
				Host:     host,
				Drives:   manyDrives(60),
				Services: services("network", "ups", "containers", "web", "backups"),
				Open:     open,
			},
		)
		if !strings.HasPrefix(
			got,
			"Weekly summary\nDrives:\n… 60 more drives\nServices:\n… and 5 more ok\nOpen problems:\ncommand/job-000: ",
		) ||
			!strings.HasSuffix(got, "…") ||
			summaryUnits(host, got) > MaxMessageUnits {
			t.Errorf("got:\n%s", got)
		}
	})
}

func TestSummaryOpenProblemsKept(t *testing.T) {
	t.Parallel()

	const host = "box.example.org"
	at := time.Date(2026, 10, 12, 8, 0, 0, 0, time.UTC)
	// About 3 000 units of open problems fit once drives and services shrink.
	open := make([]runner.OpenCondition, 50)
	for i := range open {
		open[i] = runner.OpenCondition{Key: fmt.Sprintf("containers/c-%02d", i), Text: "exited (137)", Since: at}
	}
	got := Summary(SummaryInput{Host: host, Drives: manyDrives(80), Services: manyServices(200), Open: open})
	if n := countLines(got, "containers/c-"); n != 50 || strings.HasSuffix(got, "…") {
		t.Errorf("open problems must stay whole while they fit, kept %d:\n%s", n, got)
	}
	if summaryUnits(host, got) > MaxMessageUnits {
		t.Errorf("too long: %d", summaryUnits(host, got))
	}
	if !strings.Contains(got, "\nOpen problems:\ncontainers/c-00: ") {
		t.Errorf("the open-problems section is missing:\n%s", got)
	}
}

func TestSummaryServicesOmitHardwareChecks(t *testing.T) {
	t.Parallel()

	in := SummaryInput{
		Host:     "box",
		Services: []ServiceCheck{{Name: "disks", Group: "hw"}, {Name: "temps"}, {Name: "kernel"}, {Name: "network"}},
		Open:     []runner.OpenCondition{{Key: "temps/cpu", Text: "too hot"}},
	}
	got := Summary(in)
	if !strings.Contains(got, "Services:\nnetwork: ok\nOpen problems:\n") || strings.Contains(got, "temps: ") ||
		strings.Contains(got, "hw: ") || strings.Contains(got, "disks: ") || strings.Contains(got, "kernel: ") {
		t.Errorf("hardware checks must not have a services line:\n%s", got)
	}

	only := Summary(SummaryInput{Host: "box", Services: services("disks", "temps", "kernel")})
	if !strings.Contains(only, "\nServices: none\n") {
		t.Errorf("got %q", only)
	}
}
