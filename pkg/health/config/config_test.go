package config_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/Toshik1978/tgshd/pkg/health/config"
)

const day = 24 * time.Hour

func mustParse(t *testing.T, doc string) *config.Config {
	t.Helper()

	cfg, errs := config.Parse([]byte(doc), "health.yaml")
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %+v", errs)
	}

	return cfg
}

func TestLoadFull(t *testing.T) {
	cfg, errs := config.Load(filepath.Join("testdata", "full.yaml"))
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %+v", errs)
	}

	if cfg.General.MaintenanceLock != "/run/maintenance.lock" || cfg.General.Concurrency != 4 {
		t.Errorf("general = %+v", cfg.General)
	}
	if cfg.Summary == nil || cfg.Summary.Weekday != time.Monday || cfg.Summary.Time != "09:00" {
		t.Errorf("summary = %+v", cfg.Summary)
	}
	if cfg.Network == nil || cfg.Network.Mode != config.ModeSMS || len(cfg.Network.InternetTargets) != 2 {
		t.Errorf("network = %+v", cfg.Network)
	}
	if cfg.Disks == nil || cfg.Disks.SelftestMaxAge.Duration() != 8*day ||
		cfg.Disks.ExcludeSerials[0] != "TEST0000000001" {
		t.Errorf("disks = %+v", cfg.Disks)
	}
	if cfg.Temperatures == nil || cfg.Temperatures.Limits.DriveNVMe != 70 || cfg.Temperatures.Limits.DriveSATA != 55 {
		t.Errorf("temperatures = %+v", cfg.Temperatures)
	}
	if cfg.Containers == nil || cfg.Containers.Exclude[0] != "runner-*" {
		t.Errorf("containers = %+v", cfg.Containers)
	}
	if len(cfg.HTTP) != 2 {
		t.Fatalf("http = %+v", cfg.HTTP)
	}
	web := cfg.HTTP[0]
	if web.ConnectTo != "192.0.2.5" || web.Header == nil || web.Header.ValueEnv != "APP_TOKEN" ||
		web.Status != [2]int{200, 399} || web.DependsOn != "http/other-app" {
		t.Errorf("web = %+v", web)
	}
	if other := cfg.HTTP[1]; other.DoH == "" || !other.NeedsInternet {
		t.Errorf("doh entry must imply needs_internet: %+v", other)
	}
	if len(cfg.Freshness) != 1 || cfg.Freshness[0].Content != config.ContentEpoch ||
		cfg.Freshness[0].MaxAge.Duration() != 26*time.Hour {
		t.Errorf("freshness = %+v", cfg.Freshness)
	}
	if len(cfg.Command) != 1 {
		t.Fatalf("command = %+v", cfg.Command)
	}
	cmd := cfg.Command[0]
	if cmd.Fix == nil || cmd.Fix.After != 2 || cmd.Fix.RestartContainer != "worker" ||
		cmd.Report != config.ReportJournal {
		t.Errorf("command = %+v", cmd)
	}
	if cfg.ModemProbe == nil || cfg.ModemProbe.Weekday != time.Sunday || cfg.ModemProbe.MaxSilence.Duration() != 8*day {
		t.Errorf("modem_probe = %+v", cfg.ModemProbe)
	}
}

func TestDefaultsAndAbsence(t *testing.T) {
	cfg := mustParse(t, "")
	if cfg.Network != nil || cfg.UPS != nil || cfg.Disks != nil || cfg.Temperatures != nil ||
		cfg.Kernel != nil || cfg.Containers != nil || cfg.Summary != nil || cfg.ModemProbe != nil ||
		len(cfg.HTTP) != 0 || len(cfg.Freshness) != 0 || len(cfg.Command) != 0 {
		t.Errorf("absent sections must be nil: %+v", cfg)
	}
	if g := cfg.General; g.StateDir != "/var/lib/tgshd" || g.HeartbeatFile != "/run/tgshd/heartbeat" ||
		g.BootGrace.Duration() != 10*time.Minute || g.Concurrency != 4 || g.MaintenanceLock != "" {
		t.Errorf("general defaults = %+v", g)
	}

	cfg = mustParse(
		t,
		"kernel:\nups:\ntemperatures:\ndisks: {}\ncontainers:\nnetwork:\n  internet_targets: [203.0.113.1]\n",
	)
	if cfg.Kernel == nil || cfg.Kernel.Interval.Duration() != 15*time.Minute || cfg.Kernel.Mode != config.ModeTLG {
		t.Errorf("kernel = %+v", cfg.Kernel)
	}
	if cfg.UPS == nil || cfg.UPS.Interval.Duration() != 5*time.Minute || cfg.UPS.Threshold.Duration() != 5*time.Minute {
		t.Errorf("ups = %+v", cfg.UPS)
	}
	if cfg.Disks == nil || cfg.Disks.WearPercent != 80 || cfg.Disks.SelftestMaxAge.Duration() != 40*day {
		t.Errorf("disks = %+v", cfg.Disks)
	}
	if l := cfg.Temperatures.Limits; l.CPU != 90 || l.DriveNVMe != 60 || l.DriveSATA != 60 || l.GPU != 85 ||
		l.Board != 75 {
		t.Errorf("limits = %+v", l)
	}
	if cfg.Temperatures.Threshold.Duration() != 0 {
		t.Errorf("temperatures threshold = %v, want 0", cfg.Temperatures.Threshold.Duration())
	}
	if len(cfg.Temperatures.LimitsBySerial) != 0 || len(cfg.Temperatures.BoardSensors) != 0 {
		t.Errorf("temperatures = %+v", cfg.Temperatures)
	}
	if cfg.Containers.Threshold.Duration() != 15*time.Minute {
		t.Errorf("containers = %+v", cfg.Containers)
	}
	if cfg.Network.Mode != config.ModeSMS || cfg.Network.Threshold.Duration() != 5*time.Minute {
		t.Errorf("network = %+v", cfg.Network)
	}
}

// TestNetworkThresholdFollowsInterval: an absent network threshold is the
// network interval; an explicit one, 0 included, is kept (FR-035).
func TestNetworkThresholdFollowsInterval(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want time.Duration
	}{
		{"absent", "network:\n  internet_targets: [\"203.0.113.10\"]\n", 5 * time.Minute},
		{"absent with interval", "network:\n  interval: 2m\n  internet_targets: [\"203.0.113.10\"]\n", 2 * time.Minute},
		{"explicit zero", "network:\n  threshold: 0\n  internet_targets: [\"203.0.113.10\"]\n", 0},
		{"explicit", "network:\n  threshold: 10m\n  internet_targets: [\"203.0.113.10\"]\n", 10 * time.Minute},
		{"null", "network:\n  threshold:\n  internet_targets: [\"203.0.113.10\"]\n", 5 * time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := mustParse(t, tt.yaml)
			if got := cfg.Network.Threshold.Duration(); got != tt.want {
				t.Fatalf("threshold = %s, want %s", got, tt.want)
			}
			if cfg.Network.Mode != config.ModeSMS {
				t.Fatalf("mode = %q, want sms", cfg.Network.Mode)
			}
		})
	}
}

func TestEntryDefaults(t *testing.T) {
	cfg := mustParse(t, `
http:
  - {name: a, url: "https://a.example.org/"}
command:
  - {name: c, run: [/bin/true], fix: {restart_container: x}}
freshness:
  - {name: f, path: /tmp/f, max_age: 1d}
modem_probe: {phone_env: MODEM_PHONE}
summary: {}
`)
	h := cfg.HTTP[0]
	if h.Interval.Duration() != 10*time.Minute || h.Timeout.Duration() != 15*time.Second ||
		h.Status != [2]int{200, 399} || h.Mode != config.ModeTLG || h.NeedsInternet {
		t.Errorf("http defaults = %+v", h)
	}
	c := cfg.Command[0]
	if c.Report != config.ReportAlert || c.Timeout.Duration() != 20*time.Second ||
		c.Fix.After != 2 || c.Fix.MaxPerHour != 3 || c.Fix.GraceAfterStart.Duration() != 2*time.Minute {
		t.Errorf("command defaults = %+v / %+v", c, c.Fix)
	}
	if cfg.Freshness[0].Content != config.ContentEpoch {
		t.Errorf("freshness = %+v", cfg.Freshness[0])
	}
	if m := cfg.ModemProbe; m.Weekday != time.Sunday || m.Time != "11:02" ||
		m.ReplyWithin.Duration() != 30*time.Minute || m.MaxSilence.Duration() != 8*day {
		t.Errorf("modem = %+v", m)
	}
	if cfg.Summary.Weekday != time.Monday || cfg.Summary.Time != "09:00" {
		t.Errorf("summary = %+v", cfg.Summary)
	}
}

func TestDurations(t *testing.T) {
	tests := []struct {
		in   string
		want time.Duration
		bad  bool
	}{
		{"30s", 30 * time.Second, false},
		{"6h", 6 * time.Hour, false},
		{"8d", 8 * day, false},
		{"1d12h", 36 * time.Hour, false},
		{"0", 0, false},
		{"abc", 0, true},
		{"d", 0, true},
		{"-1d", 0, true},
		{"1.5d", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			cfg, errs := config.Parse([]byte("ups:\n  threshold: "+tt.in+"\n"), "f")
			if tt.bad {
				if len(errs) != 1 || errs[0].Entry != "ups" {
					t.Fatalf("errs = %+v", errs)
				}
				if cfg.UPS != nil {
					t.Error("invalid section must be dropped")
				}

				return
			}
			if len(errs) != 0 {
				t.Fatalf("errs = %+v", errs)
			}
			if got := cfg.UPS.Threshold.Duration(); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEntryErrors(t *testing.T) {
	const okURL = `"https://a.example.org/"`

	tests := []struct {
		name   string
		doc    string
		entry  string
		reason string
	}{
		{"interval below tick", "ups: {interval: 10s}", "ups", "interval"},
		{"negative threshold", "ups: {threshold: -5m}", "ups", "threshold"},
		{
			"network threshold -1ns",
			"network: {threshold: -1ns, internet_targets: [203.0.113.10]}",
			"network",
			"threshold",
		},
		{
			"network threshold -5m",
			"network: {threshold: -5m, internet_targets: [203.0.113.10]}",
			"network",
			"threshold",
		},
		{"bad mode", "ups: {mode: pigeon}", "ups", "mode"},
		{"summary group upper", "ups: {summary_group: Backups}", "ups", "summary_group"},
		{"summary group underscore", "ups: {summary_group: back_ups}", "ups", "summary_group"},
		{
			"summary group on http",
			"http: [{name: a, url: " + okURL + ", summary_group: \"a b\"}]",
			"http/a",
			"summary_group",
		},
		{"unknown key in section", "ups: {bogus: 1}", "ups", "bogus"},
		{"wrong type", "disks: {wear_percent: lots}", "disks", "cannot unmarshal"},
		{"wear range", "disks: {wear_percent: 150}", "disks", "wear_percent"},
		{"max age zero", "disks: {selftest_max_age: 0}", "disks", "selftest_max_age"},
		{"spare floor low", "disks: {spare_floor_percent: -1}", "disks", "spare_floor_percent"},
		{"spare floor high", "disks: {spare_floor_percent: 101}", "disks", "spare_floor_percent"},
		{"force read short", "disks: {force_read_after: 59m}", "disks", "force_read_after"},
		{"include empty", "disks: {include_serials: [\"\"]}", "disks", "include_serials"},
		{"counter zero", "disks: {extra_counters: [0]}", "disks", "extra_counters"},
		{"counter high", "disks: {extra_counters: [256]}", "disks", "extra_counters"},
		{
			"serial counter high",
			"disks: {extra_counters_by_serial: {TEST0000000001: [256]}}",
			"disks",
			"TEST0000000001",
		},
		{"serial counter zero", "disks: {extra_counters_by_serial: {TEST0000000001: [0]}}", "disks", "TEST0000000001"},
		{"serial key empty", "disks: {extra_counters_by_serial: {\"\": [230]}}", "disks", "extra_counters_by_serial"},
		{"negative limit", "temperatures: {limits: {cpu: -1}}", "temperatures", "cpu"},
		{"cpu limit high", "temperatures: {limits: {cpu: 500}}", "temperatures", "limits.cpu"},
		{"nvme limit high", "temperatures: {limits: {drive_nvme: 151}}", "temperatures", "limits.drive_nvme"},
		{"sata limit high", "temperatures: {limits: {drive_sata: 151}}", "temperatures", "limits.drive_sata"},
		{"gpu limit high", "temperatures: {limits: {gpu: 151}}", "temperatures", "limits.gpu"},
		{"board limit high", "temperatures: {limits: {board: 151}}", "temperatures", "limits.board"},
		{
			"serial limit high",
			"temperatures: {limits_by_serial: {TEST0000000001: 151}}",
			"temperatures",
			"TEST0000000001",
		},
		{
			"serial limit negative",
			"temperatures: {limits_by_serial: {TEST0000000001: -1}}",
			"temperatures",
			"TEST0000000001",
		},
		{"serial limit empty key", "temperatures: {limits_by_serial: {\"\": 50}}", "temperatures", "limits_by_serial"},
		{"board sensor no chip", "temperatures: {board_sensors: [{label: temp1}]}", "temperatures", "chip"},
		{
			"board sensor limit high",
			"temperatures: {board_sensors: [{chip: acpitz, limit: 151}]}",
			"temperatures",
			"limit",
		},
		{
			"board sensor limit negative",
			"temperatures: {board_sensors: [{chip: acpitz, limit: -1}]}",
			"temperatures",
			"limit",
		},
		{"board sensor bad chip glob", "temperatures: {board_sensors: [{chip: \"[\"}]}", "temperatures", "chip"},
		{
			"board sensor bad label glob",
			"temperatures: {board_sensors: [{chip: acpitz, label: \"[\"}]}",
			"temperatures",
			"label",
		},
		{"network no targets", "network: {}", "network", "internet_targets"},
		{"bad summary weekday", "summary: {weekday: funday}", "summary", "weekday"},
		{"bad summary time", "summary: {time: \"25:61\"}", "summary", "time"},
		{"general concurrency", "general: {concurrency: 0}", "general", "concurrency"},
		{"modem missing phone", "modem_probe: {}", "modem_probe", "phone_env"},
		{"modem max silence", "modem_probe: {phone_env: X, max_silence: 0}", "modem_probe", "max_silence"},
		// Probes go out weekly: a shorter silence would alert every week.
		{"modem max silence a week", "modem_probe: {phone_env: X, max_silence: 7d}", "modem_probe", "max_silence"},
		{
			"modem max silence within reply",
			"modem_probe: {phone_env: X, reply_within: 2h, max_silence: 7d1h}",
			"modem_probe", "max_silence",
		},
		{"http bad name", "http: [{name: Web_App, url: " + okURL + "}]", "http/Web_App", "name"},
		{"http no name", "http: [{url: " + okURL + "}]", "http[0]", "name"},
		{"http bad url", "http: [{name: a, url: \"ftp://x\"}]", "http/a", "url"},
		{"http status low", "http: [{name: a, url: " + okURL + ", status: [99, 200]}]", "http/a", "status"},
		{"http status high", "http: [{name: a, url: " + okURL + ", status: [200, 600]}]", "http/a", "status"},
		{
			"http status three",
			"http: [{name: a, url: " + okURL + ", status: [200, 399, 500]}]",
			"http/a",
			"want 2 elements",
		},
		{"http status one", "http: [{name: a, url: " + okURL + ", status: [200]}]", "http/a", "want 2 elements"},
		{"http status order", "http: [{name: a, url: " + okURL + ", status: [300, 200]}]", "http/a", "status"},
		{
			"http exclusive",
			"http: [{name: a, url: " + okURL + ", connect_to: 192.0.2.1, doh: \"https://d.example.org/q\"}]",
			"http/a", "mutually exclusive",
		},
		{"http timeout", "http: [{name: a, url: " + okURL + ", timeout: 0}]", "http/a", "timeout"},
		{"http header incomplete", "http: [{name: a, url: " + okURL + ", header: {name: X}}]", "http/a", "value_env"},
		{
			"http header name not a token",
			"http: [{name: a, url: " + okURL + ", header: {name: \"X Token\", value_env: T}}]",
			"http/a", "header.name",
		},
		{
			"http header prefix control",
			"http: [{name: a, url: " + okURL + ", header: {name: X, value_env: T, prefix: \"Bearer\\n\"}}]",
			"http/a", "header.prefix",
		},
		{
			"freshness header name not a token",
			"freshness: [{name: f, url: " + okURL + ", max_age: 1h, header: {name: \"X:Y\", value_env: T}}]",
			"freshness/f", "header.name",
		},
		{
			"freshness header prefix control",
			"freshness: [{name: f, url: " + okURL + ", max_age: 1h, header: {name: X, value_env: T, prefix: \"a\\rb\"}}]",
			"freshness/f",
			"header.prefix",
		},
		{"http depends_on", "http: [{name: a, url: " + okURL + ", depends_on: nope}]", "http/a", "depends_on"},
		{"http unknown key", "http: [{name: a, url: " + okURL + ", nope: 1}]", "http/a", "nope"},
		{
			"freshness content",
			"freshness: [{name: f, path: /x, content: ctime, max_age: 1h}]",
			"freshness/f",
			"content",
		},
		{"freshness max age", "freshness: [{name: f, path: /x, max_age: 0}]", "freshness/f", "max_age"},
		{"freshness path", "freshness: [{name: f, max_age: 1h}]", "freshness/f", "path"},
		{"command empty argv0", "command: [{name: c, run: [\"\"]}]", "command/c", "run"},
		{"fix empty argv0", "command: [{name: c, run: [x], fix: {run: [\"\"]}}]", "command/c", "fix.run"},
		{"depends_on kind", "http: [{name: a, url: " + okURL + ", depends_on: bogus/x}]", "http/a", "kind"},
		{"command empty run", "command: [{name: c}]", "command/c", "run"},
		{"command report", "command: [{name: c, run: [x], report: sms}]", "command/c", "report"},
		{
			"command fix both",
			"command: [{name: c, run: [x], fix: {restart_container: a, run: [y]}}]",
			"command/c",
			"fix",
		},
		{"command fix none", "command: [{name: c, run: [x], fix: {after: 2}}]", "command/c", "fix"},
		{"command fix after", "command: [{name: c, run: [x], fix: {after: -1, run: [y]}}]", "command/c", "after"},
		{"command timeout", "command: [{name: c, run: [x], timeout: -1s}]", "command/c", "timeout"},
		// A timeout must end well inside the runner's 40 s run budget.
		{"command timeout ceiling", "command: [{name: c, run: [x], timeout: 21s}]", "command/c", "at most 20s"},
		{"http timeout ceiling", "http: [{name: a, url: " + okURL + ", timeout: 40s}]", "http/a", "at most 20s"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, errs := config.Parse([]byte(tt.doc), "health.yaml")
			if len(errs) != 1 {
				t.Fatalf("want 1 error, got %+v", errs)
			}
			e := errs[0]
			if e.File != "health.yaml" || e.Entry != tt.entry || !strings.Contains(e.Reason, tt.reason) {
				t.Errorf("got %+v, want entry %q reason containing %q", e, tt.entry, tt.reason)
			}
			if e.Error() == "" {
				t.Error("empty Error()")
			}
		})
	}
}

// TestURLErrorsDoNotEchoTheURL: entry errors are pushed to Telegram and email,
// so an invalid url never repeats its value (userinfo or query may be secret).
func TestURLErrorsDoNotEchoTheURL(t *testing.T) {
	const secret = "s3cret-token"
	for _, doc := range []string{
		`http: [{name: a, url: "ftp://user:` + secret + `@a.example.org/"}]`,
		`http: [{name: a, url: "https://a.example.org:bad/?key=` + secret + `"}]`,
		`http: [{name: a, url: "https://a.example.org/", doh: "ftp://d.example.org/?key=` + secret + `"}]`,
	} {
		_, errs := config.Parse([]byte(doc), "health.yaml")
		if len(errs) != 1 || !strings.Contains(errs[0].Reason, "must be an http or https URL") {
			t.Fatalf("%s: got %+v", doc, errs)
		}
		if strings.Contains(errs[0].Error(), secret) {
			t.Fatalf("%s: error echoes the url: %v", doc, errs[0])
		}
	}
}

func TestTimeoutAtTheCeilingIsValid(t *testing.T) {
	cfg := mustParse(t, `
command: [{name: c, run: [x], timeout: 20s}]
http: [{name: a, url: "https://a.example.org/", timeout: 20s}]
`)
	if len(cfg.Command) != 1 || len(cfg.HTTP) != 1 {
		t.Fatalf("entries at the ceiling must be kept: %+v %+v", cfg.Command, cfg.HTTP)
	}
}

// TestDependsOnTargets: depends_on must name a condition a configured check
// can raise, never the entry itself and never a cycle; otherwise the check
// would be skipped forever once that condition is bad, or never.
func TestDependsOnTargets(t *testing.T) {
	const others = `
network: {internet_targets: [192.0.2.1]}
modem_probe: {phone_env: X}
containers: {}
disks: {}
temperatures: {}
freshness: [{name: dump, path: /x, max_age: 1h}]
command: [{name: guard, run: [x]}]
`
	entry := func(name, dep string) string {
		return "  - {name: " + name + ", url: \"https://a.example.org/\", depends_on: " + dep + "}\n"
	}

	tests := []struct {
		name string
		http string
		// bad lists the http entries rejected, in order.
		bad    []string
		reason string
	}{
		{"another http entry", entry("a", "http/b") + entry("b", "network/connectivity"), nil, ""},
		{"freshness and command", entry("a", "freshness/dump") + entry("b", "command/guard"), nil, ""},
		{"network aliases", entry("a", "network/router") + entry("b", "network/internet"), nil, ""},
		{"modem probe", entry("a", "modem_probe/roundtrip"), nil, ""},
		{"dynamic keys of a configured section", entry("a", "containers/web"), nil, ""},
		{"itself", entry("a", "http/a"), []string{"http/a"}, "itself"},
		{"unknown http entry", entry("a", "http/nope"), []string{"http/a"}, "no configured check"},
		{"unknown freshness entry", entry("a", "freshness/nope"), []string{"http/a"}, "no configured check"},
		{"unknown network key", entry("a", "network/nope"), []string{"http/a"}, "no configured check"},
		{"unconfigured section", entry("a", "ups/main"), []string{"http/a"}, "no configured check"},
		{"temperatures", entry("a", "temperatures/cpu"), []string{"http/a"}, "per drive or sensor"},
		{"disks", entry("a", "disks/sda"), []string{"http/a"}, "per drive or sensor"},
		{"cycle", entry("a", "http/b") + entry("b", "http/a"), []string{"http/a", "http/b"}, "cycle"},
		{
			"target rejected",
			entry("a", "http/b") + entry("b", "http/b"),
			[]string{"http/b", "http/a"},
			"",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, errs := config.Parse([]byte(others+"http:\n"+tt.http), "health.yaml")

			var got []string
			for _, e := range errs {
				got = append(got, e.Entry)
				if !strings.Contains(e.Reason, "depends_on") || !strings.Contains(e.Reason, tt.reason) {
					t.Errorf("reason = %q, want depends_on and %q", e.Reason, tt.reason)
				}
			}
			if strings.Join(got, ",") != strings.Join(tt.bad, ",") {
				t.Fatalf("rejected = %v, want %v (%+v)", got, tt.bad, errs)
			}
			if want := strings.Count(tt.http, "name:") - len(tt.bad); len(cfg.HTTP) != want {
				t.Fatalf("kept %d http entries, want %d", len(cfg.HTTP), want)
			}
		})
	}

	// Without the network section its keys name nothing.
	_, errs := config.Parse([]byte("http:\n"+entry("a", "network/connectivity")), "health.yaml")
	if len(errs) != 1 || !strings.Contains(errs[0].Reason, "no configured check") {
		t.Fatalf("errs = %+v", errs)
	}
}

func TestValidEntriesKept(t *testing.T) {
	cfg, errs := config.Parse([]byte(`
ups: {interval: 1s}
kernel: {}
http:
  - {name: good, url: "https://a.example.org/"}
  - {name: Bad Name, url: "https://b.example.org/"}
  - {name: good, url: "https://c.example.org/"}
  - {name: also-good, url: "https://d.example.org/"}
`), "h.yaml")
	if len(errs) != 3 {
		t.Fatalf("errs = %+v", errs)
	}
	if cfg.UPS != nil || cfg.Kernel == nil {
		t.Errorf("sections: ups=%v kernel=%v", cfg.UPS, cfg.Kernel)
	}
	if len(cfg.HTTP) != 2 || cfg.HTTP[0].Name != "good" || cfg.HTTP[1].Name != "also-good" {
		t.Errorf("http = %+v", cfg.HTTP)
	}
	if !strings.Contains(errs[2].Reason, "duplicate") {
		t.Errorf("duplicate error = %+v", errs[2])
	}
}

func TestUnparseable(t *testing.T) {
	for name, doc := range map[string]string{
		"syntax":    "ups: [\n",
		"not a map": "- a\n- b\n",
		"dup key":   "ups: {}\nups: {}\n",
	} {
		t.Run(name, func(t *testing.T) {
			cfg, errs := config.Parse([]byte(doc), "h.yaml")
			if cfg != nil || len(errs) != 1 || errs[0].File != "h.yaml" || errs[0].Entry != "" {
				t.Errorf("cfg=%v errs=%+v", cfg, errs)
			}
		})
	}
}

func TestUnknownSectionKeepsOthers(t *testing.T) {
	cfg, errs := config.Parse([]byte("contaners: {}\nups: {}\nzzz: 1\n"), "h.yaml")
	if cfg == nil || cfg.UPS == nil {
		t.Fatalf("other sections must survive: %+v", cfg)
	}
	if len(errs) != 2 || errs[0].Entry != "contaners" || errs[1].Entry != "zzz" {
		t.Fatalf("errs = %+v", errs)
	}
	if !strings.Contains(errs[0].Reason, "line 1:") {
		t.Errorf("reason = %q", errs[0].Reason)
	}
}

func TestRealLineNumbers(t *testing.T) {
	doc := "kernel: {}\n\nhttp:\n  - name: a\n    url: \"https://a.example.org/\"\n    bogus: 1\n" +
		"  - name: b\n    url: \"ftp://x\"\n"
	_, errs := config.Parse([]byte(doc), "h.yaml")
	if len(errs) != 2 {
		t.Fatalf("errs = %+v", errs)
	}
	if !strings.Contains(errs[0].Reason, "line 6: field bogus") || strings.Contains(errs[0].Reason, "line 2") {
		t.Errorf("unknown key reason = %q", errs[0].Reason)
	}
	if !strings.HasPrefix(errs[1].Reason, "line 7: ") {
		t.Errorf("validation reason = %q", errs[1].Reason)
	}
}

func TestLoadErrors(t *testing.T) {
	cfg, errs := config.Load(filepath.Join(t.TempDir(), "missing.yaml"))
	if cfg != nil || len(errs) != 1 {
		t.Errorf("cfg=%v errs=%+v", cfg, errs)
	}

	p := filepath.Join(t.TempDir(), "h.yaml")
	if err := os.WriteFile(p, []byte("kernel: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if cfg, errs = config.Load(p); cfg == nil || len(errs) != 0 || cfg.Kernel == nil {
		t.Errorf("cfg=%v errs=%+v", cfg, errs)
	}
}

// TestExampleConfigParses guards the documented sample against drifting from
// the loader.
func TestExampleConfigParses(t *testing.T) {
	cfg, errs := config.Load(filepath.Join("..", "..", "..", "config", "health.example.yaml"))
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %+v", errs)
	}

	if cfg.Network == nil || cfg.UPS == nil || len(cfg.HTTP) != 1 || len(cfg.Command) != 1 || cfg.ModemProbe == nil {
		t.Errorf("sections missing: %+v", cfg)
	}
}

// TestExampleConfigDocumentsNewKeys fails when the sample leaves out a key
// that the 002 release added: each path must be an actual (uncommented) key.
func TestExampleConfigDocumentsNewKeys(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "config", "health.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}

	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}

	// A "[]" suffix descends into the first list entry that has the next key.
	paths := []string{
		"network.pause_in_quiet_windows", "disks.pause_in_quiet_windows",
		"freshness[].summary_group", "http[].summary_group",
		"general.maintenance_max_hold", "general.telegram_failing_after",
		"disks.spare_floor_percent", "disks.force_read_after", "disks.include_serials",
		"disks.extra_counters", "disks.extra_counters_by_serial",
		"temperatures.limits_by_serial", "temperatures.board_sensors",
		"http[].follow_redirects", "http[].auth_refusal", "http[].header.placeholders",
		"freshness[].future_tolerance", "freshness[].url", "freshness[].timeout", "freshness[].header",
		"command[].expect_output", "command[].forbid_output", "command[].timeout_verdict",
	}
	for _, path := range paths {
		if !hasPath(doc, strings.Split(path, ".")) {
			t.Errorf("example config lacks %s", path)
		}
	}
}

func hasPath(node any, parts []string) bool {
	if len(parts) == 0 {
		return true
	}

	key, isList := strings.CutSuffix(parts[0], "[]")

	m, ok := node.(map[string]any)
	if !ok {
		return false
	}

	child, ok := m[key]
	if !ok {
		return false
	}

	if !isList {
		return hasPath(child, parts[1:])
	}

	list, _ := child.([]any)
	for _, e := range list {
		if hasPath(e, parts[1:]) {
			return true
		}
	}

	return false
}

func TestDisksNewKeys(t *testing.T) {
	d := mustParse(t, "disks: {}\n").Disks
	if d.SpareFloorPercent != 20 || d.ForceReadAfter.Duration() != 24*time.Hour ||
		len(d.IncludeSerials) != 0 || len(d.ExtraCounters) != 0 || len(d.ExtraCountersBySerial) != 0 {
		t.Errorf("defaults = %+v", d)
	}

	d = mustParse(t, `disks:
  spare_floor_percent: 0
  force_read_after: 1h
  include_serials: [TEST0000000009]
  extra_counters: [5, 252, 255, 1]
  extra_counters_by_serial:
    TEST0000000001: [230]
`).Disks
	if d.SpareFloorPercent != 0 || d.ForceReadAfter.Duration() != time.Hour ||
		d.IncludeSerials[0] != "TEST0000000009" || len(d.ExtraCounters) != 4 ||
		d.ExtraCountersBySerial["TEST0000000001"][0] != 230 {
		t.Errorf("parsed = %+v", d)
	}
}

func TestTemperatureNewKeys(t *testing.T) {
	cfg, errs := config.Parse([]byte(`temperatures:
  limits: {board: 70}
  limits_by_serial: {TEST0000000001: 0, TEST0000000002: 150}
  board_sensors:
    - {chip: "nct*", label: "SYSTIN"}
    - {chip: acpitz, limit: 0}
    - {chip: "pch_*", label: "*", limit: 80}
`), "f")
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %+v", errs)
	}

	temps := cfg.Temperatures
	if temps.LimitsBySerial["TEST0000000001"] != 0 || temps.LimitsBySerial["TEST0000000002"] != 150 ||
		len(temps.LimitsBySerial) != 2 {
		t.Errorf("limits_by_serial = %+v", temps.LimitsBySerial)
	}

	want := []struct {
		chip, label string
		limit       int
	}{{"nct*", "SYSTIN", 70}, {"acpitz", "", 0}, {"pch_*", "*", 80}}
	if len(temps.BoardSensors) != len(want) {
		t.Fatalf("board_sensors = %+v", temps.BoardSensors)
	}
	for i, w := range want {
		g := temps.BoardSensors[i]
		if g.Chip != w.chip || g.Label != w.label || g.Limit == nil || *g.Limit != w.limit {
			t.Errorf("board_sensors[%d] = %+v, want %+v", i, g, w)
		}
	}
}

func TestSummaryGroup(t *testing.T) {
	cfg := mustParse(t, `
ups: {summary_group: power-1}
http:
  - {name: a, url: "https://a.example.org/", summary_group: web}
  - {name: b, url: "https://b.example.org/"}
freshness:
  - {name: f, path: /x, max_age: 1h, summary_group: backups}
command:
  - {name: c, run: [x], summary_group: backups}
`)
	if cfg.UPS.SummaryGroup != "power-1" || cfg.HTTP[0].SummaryGroup != "web" || cfg.HTTP[1].SummaryGroup != "" ||
		cfg.Freshness[0].SummaryGroup != "backups" || cfg.Command[0].SummaryGroup != "backups" {
		t.Errorf("groups = %+v %+v %+v %+v", cfg.UPS, cfg.HTTP, cfg.Freshness, cfg.Command)
	}
}

func TestQuietAndDeliveryKeys(t *testing.T) {
	g := mustParse(t, "").General
	if g.MaintenanceMaxHold.Duration() != 6*time.Hour || g.TelegramFailingAfter.Duration() != 15*time.Minute {
		t.Errorf("general defaults = %+v", g)
	}

	g = mustParse(t, "general: {maintenance_max_hold: 1h, telegram_failing_after: 1m}\n").General
	if g.MaintenanceMaxHold.Duration() != time.Hour || g.TelegramFailingAfter.Duration() != time.Minute {
		t.Errorf("general parsed = %+v", g)
	}

	cfg := mustParse(t, `
network: {internet_targets: [203.0.113.1]}
ups:
disks:
temperatures:
kernel:
containers:
http: [{name: a, url: "https://a.example.org/"}]
command: [{name: c, run: [/bin/true]}]
freshness: [{name: f, path: /tmp/f, max_age: 1d}]
`)
	hardware := map[string]bool{
		"disks": cfg.Disks.PauseInQuietWindows, "temperatures": cfg.Temperatures.PauseInQuietWindows,
		"kernel": cfg.Kernel.PauseInQuietWindows,
	}
	for name, got := range hardware {
		if got {
			t.Errorf("%s pause_in_quiet_windows default = true, want false", name)
		}
	}
	others := map[string]bool{
		"network": cfg.Network.PauseInQuietWindows, "ups": cfg.UPS.PauseInQuietWindows,
		"containers": cfg.Containers.PauseInQuietWindows, "http": cfg.HTTP[0].PauseInQuietWindows,
		"command": cfg.Command[0].PauseInQuietWindows, "freshness": cfg.Freshness[0].PauseInQuietWindows,
	}
	for name, got := range others {
		if !got {
			t.Errorf("%s pause_in_quiet_windows default = false, want true", name)
		}
	}

	cfg = mustParse(t, "disks: {pause_in_quiet_windows: true}\nups: {pause_in_quiet_windows: false}\n")
	if !cfg.Disks.PauseInQuietWindows || cfg.UPS.PauseInQuietWindows {
		t.Errorf("explicit values not honoured: %+v %+v", cfg.Disks.Common, cfg.UPS.Common)
	}
}

func TestQuietAndDeliveryKeyErrors(t *testing.T) {
	for _, doc := range []string{
		"general: {maintenance_max_hold: 0s}\n",
		"general: {telegram_failing_after: 59s}\n",
	} {
		if _, errs := config.Parse([]byte(doc), "health.yaml"); len(errs) == 0 {
			t.Errorf("%q must be rejected", doc)
		}
	}
}

func TestServiceKeysDefaults(t *testing.T) {
	cfg := mustParse(t, `
http:
  - {name: a, url: "https://a.example.org/", header: {name: X-Key, value_env: KEY}}
freshness:
  - {name: f, path: /tmp/f, max_age: 1d}
  - {name: g, url: "https://b.example.org/stamp", max_age: 1d}
command:
  - {name: c, run: [/bin/true]}
`)
	h := cfg.HTTP[0]
	if h.FollowRedirects || h.AuthRefusal != config.AuthRefusalDown || len(h.Header.Placeholders) != 0 {
		t.Errorf("http defaults = %+v", h)
	}
	f, g := cfg.Freshness[0], cfg.Freshness[1]
	if f.FutureTolerance.Duration() != time.Hour || f.Timeout.Duration() != 15*time.Second || f.URL != "" ||
		f.Header != nil {
		t.Errorf("path freshness = %+v", f)
	}
	if g.FutureTolerance.Duration() != time.Hour || g.Timeout.Duration() != 15*time.Second {
		t.Errorf("url freshness = %+v", g)
	}
	if c := cfg.Command[0]; c.TimeoutVerdict != config.TimeoutBroken || c.ExpectOutput != "" || c.ForbidOutput != "" {
		t.Errorf("command defaults = %+v", c)
	}
}

func TestServiceKeysParsed(t *testing.T) {
	cfg := mustParse(t, `
http:
  - name: a
    url: "https://a.example.org/"
    follow_redirects: true
    auth_refusal: checker_error
    header: {name: X-Key, value_env: KEY, placeholders: [sample-key]}
freshness:
  - name: g
    url: "https://b.example.org/stamp"
    max_age: 1d
    future_tolerance: 0
    timeout: 20s
    header: {name: X-Key, value_env: KEY, prefix: "Bearer ", placeholders: [sample-key]}
command:
  - {name: c, run: [/bin/true], expect_output: "^ok", forbid_output: "(?i)degraded", timeout_verdict: unreadable}
`)
	h := cfg.HTTP[0]
	if !h.FollowRedirects || h.AuthRefusal != config.AuthRefusalCheckerError ||
		h.Header.Placeholders[0] != "sample-key" {
		t.Errorf("http = %+v", h)
	}
	g := cfg.Freshness[0]
	if g.FutureTolerance != 0 || g.Timeout.Duration() != 20*time.Second || g.Header.Placeholders[0] != "sample-key" {
		t.Errorf("freshness = %+v", g)
	}
	c := cfg.Command[0]
	if c.ExpectOutput != "^ok" || c.ForbidOutput != "(?i)degraded" || c.TimeoutVerdict != config.TimeoutUnreadable {
		t.Errorf("command = %+v", c)
	}
}

func TestServiceKeyErrors(t *testing.T) {
	const (
		httpOK  = `name: a, url: "https://a.example.org/"`
		freshOK = `name: f, max_age: 1d`
		hdr     = `{name: X-Key, value_env: KEY}`
	)

	tests := []struct {
		name   string
		doc    string
		entry  string
		reason string
	}{
		{"auth refusal", "http: [{" + httpOK + ", auth_refusal: ignore}]", "http/a", "auth_refusal"},
		{
			"http empty placeholder", "http: [{" + httpOK + ", header: {name: X, value_env: K, placeholders: [\"\"]}}]",
			"http/a", "placeholders",
		},
		{
			"negative tolerance", "freshness: [{" + freshOK + ", path: /tmp/f, future_tolerance: -1m}]",
			"freshness/f", "future_tolerance",
		},
		{"neither path nor url", "freshness: [{" + freshOK + "}]", "freshness/f", "exactly one of path or url"},
		{
			"both path and url", "freshness: [{" + freshOK + ", path: /tmp/f, url: \"https://b.example.org/\"}]",
			"freshness/f", "exactly one of path or url",
		},
		{"bad url scheme", "freshness: [{" + freshOK + ", url: \"ftp://b.example.org/\"}]", "freshness/f", "url"},
		{
			"url with mtime", "freshness: [{" + freshOK + ", url: \"https://b.example.org/\", content: mtime}]",
			"freshness/f", "content: epoch",
		},
		{"timeout without url", "freshness: [{" + freshOK + ", path: /tmp/f, timeout: 5s}]", "freshness/f", "timeout"},
		{
			"default timeout without url", "freshness: [{" + freshOK + ", path: /tmp/f, timeout: 15s}]",
			"freshness/f", "timeout",
		},
		{
			"timeout zero", "freshness: [{" + freshOK + ", url: \"https://b.example.org/\", timeout: 0}]",
			"freshness/f", "timeout",
		},
		{
			"timeout high", "freshness: [{" + freshOK + ", url: \"https://b.example.org/\", timeout: 21s}]",
			"freshness/f", "timeout",
		},
		{
			"header without url", "freshness: [{" + freshOK + ", path: /tmp/f, header: " + hdr + "}]",
			"freshness/f", "header",
		},
		{
			"freshness header name",
			"freshness: [{" + freshOK + ", url: \"https://b.example.org/\", header: {value_env: K}}]",
			"freshness/f",
			"header.name",
		},
		{
			"freshness empty placeholder",
			"freshness: [{" + freshOK + ", url: \"https://b.example.org/\", header: {name: X, value_env: K, placeholders: [\"\"]}}]",
			"freshness/f",
			"placeholders",
		},
		{"expect regexp", "command: [{name: c, run: [/bin/true], expect_output: \"(\"}]", "command/c", "expect_output"},
		{
			"forbid regexp",
			"command: [{name: c, run: [/bin/true], forbid_output: \"a(?=b)\"}]",
			"command/c",
			"forbid_output",
		},
		{
			"timeout verdict",
			"command: [{name: c, run: [/bin/true], timeout_verdict: down}]",
			"command/c",
			"timeout_verdict",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, errs := config.Parse([]byte(tt.doc), "test.yaml")
			if len(errs) != 1 || errs[0].Entry != tt.entry || !strings.Contains(errs[0].Reason, tt.reason) {
				t.Errorf("errs = %+v, want one for %s containing %q", errs, tt.entry, tt.reason)
			}
		})
	}
}

// TestExampleConfigIsValid: the shipped example loads without an entry error,
// so the file users start from never documents a rejected value.
func TestExampleConfigIsValid(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "config", "health.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if _, errs := config.Parse(data, "health.example.yaml"); len(errs) != 0 {
		t.Fatalf("entry errors: %+v", errs)
	}
}

func TestMemoryDefaults(t *testing.T) {
	if cfg := mustParse(t, ""); cfg.Memory != nil {
		t.Fatalf("absent memory must be nil: %+v", cfg.Memory)
	}

	m := mustParse(t, "memory:\n").Memory
	if m == nil {
		t.Fatal("present memory section must not be nil")
	}

	if m.Interval.Duration() != time.Minute || m.Threshold.Duration() != 5*time.Minute ||
		m.Mode != config.ModeTLG || !m.PauseInQuietWindows {
		t.Errorf("common defaults = %+v", m)
	}
	if m.WarnAvailablePercent != 15 || m.WarnSwapPercent != 80 || m.WarnPressurePercent != 10 ||
		m.GrowthWindow.Duration() != time.Hour || m.GrowthWarn != 4<<30 ||
		len(m.AuditExclude) != 0 {
		t.Errorf("warning defaults = %+v", m)
	}

	g := m.Guard
	if g.Intervene != config.InterveneReport || len(g.NeverTouch) != 0 || g.DangerAvailablePercent != 5 ||
		g.DangerPressurePercent != 20 || g.DangerFor.Duration() != 30*time.Second ||
		g.CulpritWindow.Duration() != 10*time.Minute || g.CulpritMinGrowth != 1<<30 ||
		g.RepeatWindow.Duration() != 6*time.Hour || g.RecoveryWindow.Duration() != 2*time.Minute ||
		g.ActionTimeout.Duration() != time.Minute || g.FailedMode != config.ModeSMS {
		t.Errorf("guard defaults = %+v", g)
	}
}

func TestMemoryParsed(t *testing.T) {
	m := mustParse(t, `
memory:
  interval: 2m
  threshold: 0s
  mode: tlg-or-sms
  pause_in_quiet_windows: false
  warn_available_percent: 20
  warn_swap_percent: 90
  warn_pressure_percent: 15
  growth_window: 2h
  growth_warn: 512MB
  audit_exclude: [scratch-job, "a.b@c_d-e"]
  guard:
    intervene: act
    never_touch: [db, backup.service]
    danger_available_percent: 8
    danger_pressure_percent: 30
    danger_for: 1m
    culprit_window: 20m
    culprit_min_growth: 2GB
    repeat_window: 0
    recovery_window: 5m
    action_timeout: 2m
    failed_mode: tlg
`).Memory
	if m.Interval.Duration() != 2*time.Minute || m.Mode != config.ModeTLGOrSMS || m.PauseInQuietWindows ||
		m.WarnAvailablePercent != 20 || m.WarnSwapPercent != 90 || m.WarnPressurePercent != 15 ||
		m.GrowthWindow.Duration() != 2*time.Hour || m.GrowthWarn != 512<<20 ||
		len(m.AuditExclude) != 2 || m.AuditExclude[0] != "scratch-job" {
		t.Errorf("memory = %+v", m)
	}

	g := m.Guard
	if g.Intervene != config.InterveneAct || len(g.NeverTouch) != 2 || g.NeverTouch[1] != "backup.service" ||
		g.DangerAvailablePercent != 8 || g.DangerPressurePercent != 30 || g.DangerFor.Duration() != time.Minute ||
		g.CulpritWindow.Duration() != 20*time.Minute || g.CulpritMinGrowth != 2<<30 || g.RepeatWindow != 0 ||
		g.RecoveryWindow.Duration() != 5*time.Minute || g.ActionTimeout.Duration() != 2*time.Minute ||
		g.FailedMode != config.ModeTLG {
		t.Errorf("guard = %+v", g)
	}

	for _, v := range []string{"off", "report", "act"} {
		got := mustParse(t, "memory: {guard: {intervene: "+v+"}}\n").Memory.Guard.Intervene
		if string(got) != v {
			t.Errorf("intervene %q parsed as %q", v, got)
		}
	}
}

// TestMemoryRanges: every range accepts its edges and rejects one step outside.
func TestMemoryRanges(t *testing.T) {
	tests := []struct {
		key, section string
		ok, bad      []string
	}{
		{"warn_available_percent", "", []string{"6", "99"}, []string{"0", "100"}},
		{"warn_swap_percent", "", []string{"1", "100"}, []string{"0", "101"}},
		{"warn_pressure_percent", "", []string{"1", "100"}, []string{"0", "101"}},
		{"growth_window", "", []string{"10m", "24h"}, []string{"9m59s", "24h1s"}},
		{"danger_available_percent", "guard", []string{"1", "14"}, []string{"0", "15"}},
		{"danger_pressure_percent", "guard", []string{"1", "100"}, []string{"0", "101"}},
		{"danger_for", "guard", []string{"5s", "5m"}, []string{"4s", "5m1s"}},
		{"culprit_window", "guard", []string{"1m", "1h"}, []string{"59s", "1h1s"}},
		{"repeat_window", "guard", []string{"0", "1s", "6h"}, []string{"-1s"}},
		{"recovery_window", "guard", []string{"30s", "30m"}, []string{"29s", "30m1s"}},
		{"action_timeout", "guard", []string{"10s", "5m"}, []string{"9s", "5m1s"}},
		{"interval", "", []string{"30s"}, []string{"29s"}},
		{"threshold", "", []string{"0s"}, []string{"-1s"}},
	}

	doc := func(section, key, v string) string {
		if section == "" {
			return "memory: {" + key + ": " + v + "}\n"
		}

		return "memory: {" + section + ": {" + key + ": " + v + "}}\n"
	}

	for _, tt := range tests {
		for _, v := range tt.ok {
			cfg, errs := config.Parse([]byte(doc(tt.section, tt.key, v)), "health.yaml")
			if len(errs) != 0 || cfg.Memory == nil {
				t.Errorf("%s=%s must be valid: %+v", tt.key, v, errs)
			}
		}

		for _, v := range tt.bad {
			cfg, errs := config.Parse([]byte(doc(tt.section, tt.key, v)), "health.yaml")
			if len(errs) != 1 || errs[0].Entry != "memory" || !strings.Contains(errs[0].Reason, tt.key) {
				t.Errorf("%s=%s must be one memory error naming the key: %+v", tt.key, v, errs)
			}
			if cfg.Memory != nil {
				t.Errorf("%s=%s: invalid section must be skipped", tt.key, v)
			}
		}
	}
}

func TestMemoryDangerBelowWarn(t *testing.T) {
	tests := []struct {
		doc string
		ok  bool
	}{
		{"memory: {warn_available_percent: 10, guard: {danger_available_percent: 9}}", true},
		{"memory: {warn_available_percent: 10, guard: {danger_available_percent: 10}}", false},
		{"memory: {warn_available_percent: 3}", false}, // default danger 5
		{"memory: {warn_available_percent: 6}", true},
		{"memory: {warn_available_percent: 99, guard: {danger_available_percent: 50}}", true},
	}
	for _, tt := range tests {
		_, errs := config.Parse([]byte(tt.doc+"\n"), "health.yaml")
		if tt.ok && len(errs) != 0 {
			t.Errorf("%s: unexpected %+v", tt.doc, errs)
		}
		if !tt.ok && (len(errs) != 1 || !strings.Contains(errs[0].Reason, "danger_available_percent")) {
			t.Errorf("%s: want danger_available_percent error: %+v", tt.doc, errs)
		}
	}
}

func TestMemorySizes(t *testing.T) {
	tests := []struct {
		in   string
		want uint64
		ok   bool
	}{
		{"1B", 1, true},
		{"1KB", 1 << 10, true},
		{"512MB", 512 << 20, true},
		{"4GB", 4 << 30, true},
		{"2TB", 2 << 40, true},
		{"16777215TB", 16777215 << 40, true},
		{"16777216TB", 0, false}, // overflows uint64
		{"0GB", 0, false},        // not positive
		{"0", 0, false},
		{"4", 0, false}, // unit required
		{"GB", 0, false},
		{"-1GB", 0, false},
		{"1.5GB", 0, false},
		{"4PB", 0, false},
		{"4gb", 0, false},
		{"", 0, false},
	}
	for _, tt := range tests {
		for _, key := range []string{"growth_warn", "guard: {culprit_min_growth"} {
			doc := "memory: {" + key + ": \"" + tt.in + "\"}}\n"
			if key == "growth_warn" {
				doc = "memory: {growth_warn: \"" + tt.in + "\"}\n"
			}

			cfg, errs := config.Parse([]byte(doc), "health.yaml")
			if tt.ok {
				if len(errs) != 0 {
					t.Errorf("%s %q: unexpected %+v", key, tt.in, errs)

					continue
				}
				got := cfg.Memory.GrowthWarn
				if key != "growth_warn" {
					got = cfg.Memory.Guard.CulpritMinGrowth
				}
				if uint64(got) != tt.want {
					t.Errorf("%s %q = %d, want %d", key, tt.in, got, tt.want)
				}
			} else if len(errs) != 1 || errs[0].Entry != "memory" {
				t.Errorf("%s %q must be one memory error: %+v", key, tt.in, errs)
			}
		}
	}

	if _, errs := config.Parse([]byte("memory: {growth_warn: [1]}\n"), "health.yaml"); len(errs) != 1 {
		t.Errorf("non-scalar size must be rejected: %+v", errs)
	}
}

func TestMemoryErrors(t *testing.T) {
	tests := []struct {
		name, doc, reason string
	}{
		{"unknown key", "memory: {bogus: 1}", "bogus"},
		{"unknown guard key", "memory: {guard: {bogus: 1}}", "bogus"},
		{"needs_internet", "memory: {needs_internet: true}", "needs_internet"},
		{"depends_on", "memory: {depends_on: http/a}", "depends_on"},
		{"summary_group", "memory: {summary_group: x}", "summary_group"},
		{"mode", "memory: {mode: email}", "mode"},
		{"failed_mode", "memory: {guard: {failed_mode: email}}", "failed_mode"},
		{"intervene", "memory: {guard: {intervene: maybe}}", "intervene"},
		{"empty intervene", "memory: {guard: {intervene: \"\"}}", "intervene"},
		{"never_touch space", "memory: {guard: {never_touch: [\"a b\"]}}", "never_touch"},
		{"never_touch empty", "memory: {guard: {never_touch: [\"\"]}}", "never_touch"},
		{"never_touch slash", "memory: {guard: {never_touch: [a/b]}}", "never_touch"},
		{"audit_exclude glob", "memory: {audit_exclude: [\"runner-*\"]}", "audit_exclude"},
		{"audit_exclude empty", "memory: {audit_exclude: [\"\"]}", "audit_exclude"},
	}
	for _, tt := range tests {
		cfg, errs := config.Parse([]byte(tt.doc+"\n"), "health.yaml")
		if len(errs) != 1 || errs[0].Entry != "memory" || !strings.Contains(errs[0].Reason, tt.reason) {
			t.Errorf("%s: want one memory error naming %q: %+v", tt.name, tt.reason, errs)
		}
		if cfg.Memory != nil {
			t.Errorf("%s: invalid section must be skipped", tt.name)
		}
	}

	cfg, errs := config.Parse([]byte("memory: {bogus: 1}\nups:\n"), "health.yaml")
	if len(errs) != 1 || cfg.UPS == nil {
		t.Errorf("an invalid memory section must not disable the others: %+v", errs)
	}
}

// TestExampleConfigDocumentsMemory: the sample carries an active memory
// section whose values are the shipped defaults, so a copy of it behaves like
// an absent section except that it is visible and tunable.
func TestExampleConfigDocumentsMemory(t *testing.T) {
	cfg, errs := config.Load(filepath.Join("..", "..", "..", "config", "health.example.yaml"))
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %+v", errs)
	}

	m := cfg.Memory
	if m == nil {
		t.Fatal("example config has no memory section")
	}

	want := mustParse(t, "memory:\n").Memory
	if m.Guard.Intervene != config.InterveneReport {
		t.Errorf("example intervene = %q, want report", m.Guard.Intervene)
	}

	m.AuditExclude, m.Guard.NeverTouch = nil, nil
	want.AuditExclude, want.Guard.NeverTouch = nil, nil
	if !reflect.DeepEqual(m, want) {
		t.Errorf("example memory values differ from the defaults:\n got %+v\nwant %+v", m, want)
	}
}
