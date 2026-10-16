package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	if cfg.Network.Mode != config.ModeSMS || cfg.Network.Threshold.Duration() != 0 {
		t.Errorf("network = %+v", cfg.Network)
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
		{"bad mode", "ups: {mode: pigeon}", "ups", "mode"},
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
