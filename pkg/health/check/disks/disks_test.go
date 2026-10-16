package disks

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Toshik1978/tgshd/pkg/health"
	"github.com/Toshik1978/tgshd/pkg/health/config"
	"github.com/Toshik1978/tgshd/pkg/health/store"
)

const (
	serialSATA = "TEST0000000001"
	serialNVMe = "TEST0000000002"
	serialNew  = "TEST0000000003"
)

// output is one canned smartctl run.
type output struct {
	stdout []byte
	status int
	err    error
}

// fakeSource is a hand-written Source.
type fakeSource struct {
	nodes     []string
	drivesErr error
	runs      map[string]output
	// hook, when set, runs first in Run (drives are read concurrently).
	hook  func(ctx context.Context, node string) error
	mu    sync.Mutex
	calls []string
}

func (f *fakeSource) Drives() ([]string, error) {
	return f.nodes, f.drivesErr
}

func (f *fakeSource) Run(ctx context.Context, node string) ([]byte, int, error) {
	f.mu.Lock()
	f.calls = append(f.calls, node)
	o := f.runs[node]
	f.mu.Unlock()
	if f.hook != nil {
		if err := f.hook(ctx, node); err != nil {
			return nil, 0, err
		}
	}

	return o.stdout, o.status, o.err
}

func (f *fakeSource) set(node string, d drive) {
	if f.runs == nil {
		f.runs = map[string]output{}
	}
	if slices.Contains(f.nodes, node) {
		f.runs[node] = output{stdout: d.json()}
		return
	}
	f.nodes = append(f.nodes, node)
	f.runs[node] = output{stdout: d.json()}
}

func (f *fakeSource) remove(node string) {
	out := f.nodes[:0]
	for _, n := range f.nodes {
		if n != node {
			out = append(out, n)
		}
	}
	f.nodes = out
}

// drive describes a drive from which smartctl JSON is built.
type drive struct {
	serial  string
	model   string
	nvme    bool
	failed  bool
	temp    int
	hours   int64
	noTest  bool  // no finished self-test in the log
	testAge int64 // hours between the newest finished self-test and now
	testRaw *int64
	testBad bool
	ata     map[int]uint64
	nvmeCtr map[string]uint64
	warning int
	spare   int
	spareTh int
	used    int
	written uint64
}

func sata(serial string) drive {
	return drive{
		serial: serial, model: "Example SATA SSD", temp: 34, hours: 4200, testAge: 10,
		ata:     map[int]uint64{5: 0, 187: 0, 188: 0, 197: 0, 198: 0, 199: 0},
		written: 1000,
	}
}

func nvme(serial string) drive {
	return drive{
		serial: serial, model: "Example NVMe SSD", nvme: true, temp: 38, hours: 1500, testAge: 10,
		nvmeCtr: map[string]uint64{"media_errors": 0, "num_err_log_entries": 0},
		spare:   100, spareTh: 10, used: 1, written: 2000,
	}
}

func (d drive) json() []byte {
	m := map[string]any{
		"serial_number": d.serial,
		"model_name":    d.model,
		"smart_status":  map[string]any{"passed": !d.failed},
		"power_on_time": map[string]any{"hours": d.hours},
		"temperature":   map[string]any{"current": d.temp},
	}
	if d.nvme {
		d.nvmeJSON(m)
	} else {
		d.ataJSON(m)
	}
	out, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}

	return out
}

func (d drive) testHours() int64 {
	if d.testRaw != nil {
		return *d.testRaw
	}

	return d.hours - d.testAge
}

func (d drive) nvmeJSON(m map[string]any) {
	m["device"] = map[string]any{"name": "/dev/nvme0", "protocol": "NVMe"}
	hl := map[string]any{
		"critical_warning": d.warning, "available_spare": d.spare,
		"available_spare_threshold": d.spareTh, "percentage_used": d.used,
		"data_units_written": d.written,
	}
	for k, v := range d.nvmeCtr {
		hl[k] = v
	}
	m["nvme_smart_health_information_log"] = hl
	var table []any
	if !d.noTest {
		result := 0
		if d.testBad {
			result = 7
		}
		table = append(table, map[string]any{
			"self_test_result": map[string]any{"value": result, "string": "result"},
			"power_on_hours":   d.testHours(),
		})
	}
	m["nvme_self_test_log"] = map[string]any{"table": table}
}

func (d drive) ataJSON(m map[string]any) {
	m["device"] = map[string]any{"name": "/dev/sda", "protocol": "ATA"}
	m["logical_block_size"] = 512
	rows := make([]any, 0, 1+len(d.ata))
	rows = append(rows, map[string]any{"id": 241, "raw": map[string]any{"value": d.written}})
	for id, v := range d.ata {
		rows = append(rows, map[string]any{"id": id, "raw": map[string]any{"value": v}})
	}
	m["ata_smart_attributes"] = map[string]any{"table": rows}
	var table []any
	if !d.noTest {
		status := 0
		if d.testBad {
			status = 0x71
		}
		table = append(table, map[string]any{
			"status":         map[string]any{"value": status, "string": "Completed: read failure"},
			"lifetime_hours": d.testHours(),
		})
	}
	m["ata_smart_self_test_log"] = map[string]any{"standard": map[string]any{"table": table}}
}

// clock is a controllable time source.
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func (c *clock) add(d time.Duration) { c.t = c.t.Add(d) }

func newClock() *clock {
	return &clock{t: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
}

func disksCfg() config.Disks {
	return config.Disks{
		Interval:       config.Duration(15 * time.Minute),
		WearPercent:    80,
		SelftestMaxAge: config.Duration(8 * 24 * time.Hour),
	}
}

func tempsCfg(threshold time.Duration) *config.Temperatures {
	return &config.Temperatures{
		Threshold: config.Duration(threshold),
		Limits:    config.Limits{DriveNVMe: 70, DriveSATA: 55},
	}
}

// newLoaded returns a check whose (empty) baselines are loaded.
func newLoaded(src Source, disks config.Disks, temps *config.Temperatures, now func() time.Time) *Check {
	c := New(src, disks, temps, now)
	c.LoadBaselines(nil)

	return c
}

// fixtureCheck returns a check over src that has already completed its first run.
func fixtureCheck(t *testing.T, src *fakeSource, clk *clock, temps *config.Temperatures) *Check {
	t.Helper()

	c := newLoaded(src, disksCfg(), temps, clk.now)
	if res := c.Run(t.Context()); res.Err != nil {
		t.Fatalf("first run: %v", res.Err)
	}
	clk.add(15 * time.Minute)

	return c
}

func find(res health.CheckResult, key string) (health.Reading, bool) {
	for _, r := range res.Readings {
		if r.Key == key {
			return r, true
		}
	}

	return health.Reading{}, false
}

func mustFind(t *testing.T, res health.CheckResult, key string) health.Reading {
	t.Helper()

	r, ok := find(res, key)
	if !ok {
		t.Fatalf("no reading %q in %+v", key, res.Readings)
	}

	return r
}

func events(res health.CheckResult) []health.Reading {
	var out []health.Reading
	for _, r := range res.Readings {
		if r.Kind == health.Event {
			out = append(out, r)
		}
	}

	return out
}

func sample(res health.CheckResult, metric string) (float64, bool) {
	for _, s := range res.Samples {
		if s.Metric == metric {
			return s.Value, true
		}
	}

	return 0, false
}

func TestFirstRunAnnouncesAllDrivesInOneMessage(t *testing.T) {
	src := &fakeSource{}
	src.set("sda", sata(serialSATA))
	src.set("nvme0n1", nvme(serialNVMe))
	clk := newClock()
	c := newLoaded(src, disksCfg(), tempsCfg(0), clk.now)

	res := c.Run(t.Context())
	if res.Err != nil {
		t.Fatalf("Run: %v", res.Err)
	}
	ev := events(res)
	if len(ev) != 1 {
		t.Fatalf("events = %+v, want exactly one", ev)
	}
	if ev[0].Key != "disks/drives" {
		t.Errorf("key = %q", ev[0].Key)
	}
	for _, want := range []string{serialSATA, serialNVMe, "Example SATA SSD", "Example NVMe SSD", "/dev/sda", "/dev/nvme0n1"} {
		if !strings.Contains(ev[0].Text, want) {
			t.Errorf("announce %q lacks %q", ev[0].Text, want)
		}
	}

	clk.add(15 * time.Minute)
	if ev := events(c.Run(t.Context())); len(ev) != 0 {
		t.Errorf("second run events = %+v", ev)
	}
}

func TestFirstRunWithoutDrivesStaysFirst(t *testing.T) {
	src := &fakeSource{}
	clk := newClock()
	c := newLoaded(src, disksCfg(), nil, clk.now)
	if res := c.Run(t.Context()); res.Err != nil || len(res.Readings) != 0 {
		t.Fatalf("empty run = %+v", res)
	}

	src.set("sda", sata(serialSATA))
	ev := events(c.Run(t.Context()))
	if len(ev) != 1 || ev[0].Key != "disks/drives" {
		t.Errorf("events = %+v, want the first-run inventory", ev)
	}
}

func TestNewDriveAnnounced(t *testing.T) {
	src := &fakeSource{}
	src.set("sda", sata(serialSATA))
	clk := newClock()
	c := fixtureCheck(t, src, clk, nil)

	d := nvme(serialNew)
	d.nvmeCtr["media_errors"] = 4 // pre-existing counters of a new drive are its baseline
	src.set("nvme0n1", d)
	res := c.Run(t.Context())
	ev := events(res)
	if len(ev) != 1 || ev[0].Key != "disks/"+serialNew+"/new" {
		t.Fatalf("events = %+v", ev)
	}
	if !strings.Contains(ev[0].Text, serialNew) || !strings.Contains(ev[0].Text, "Example NVMe SSD") {
		t.Errorf("text = %q", ev[0].Text)
	}
}

func TestRetiredSerialReappearsAsNew(t *testing.T) {
	src := &fakeSource{}
	src.set("sda", sata(serialSATA))
	clk := newClock()
	c := New(src, disksCfg(), nil, clk.now)
	c.LoadBaselines([]store.Baseline{
		{
			Serial: serialSATA, Model: "Example SATA SSD", Bus: "ATA", Retired: true,
			Data:        json.RawMessage(`{"counters":{"ata.5":0}}`),
			FirstSeenAt: clk.t.Add(-time.Hour), LastSeenAt: clk.t.Add(-time.Hour),
		},
		{
			Serial: serialNVMe, Model: "Example NVMe SSD", Bus: "NVMe",
			Data:        json.RawMessage(`{}`),
			FirstSeenAt: clk.t.Add(-time.Hour), LastSeenAt: clk.t.Add(-time.Hour),
		},
	})
	d := sata(serialSATA)
	d.ata[5] = 9 // would be a rise against the retired baseline
	src.set("sda", d)

	res := c.Run(t.Context())
	ev := events(res)
	if len(ev) != 1 || ev[0].Key != "disks/"+serialSATA+"/new" {
		t.Fatalf("events = %+v, want only the new-drive announcement", ev)
	}
	for _, b := range c.Baselines() {
		if b.Serial == serialSATA && (b.Retired || !b.FirstSeenAt.Equal(clk.t)) {
			t.Errorf("reappeared baseline = %+v", b)
		}
	}
}

func TestCounterRiseOneEventPerRise(t *testing.T) {
	src := &fakeSource{}
	d := sata(serialSATA)
	src.set("sda", d)
	clk := newClock()
	c := fixtureCheck(t, src, clk, nil)

	steps := []struct {
		value uint64
		want  string // "" ⇒ no event
	}{
		{2, "0→2"},
		{3, "2→3"},
		{3, ""},
		{1, ""}, // a fall lowers the baseline silently
		{2, "1→2"},
	}
	for i, s := range steps {
		d.ata[197] = s.value
		src.set("sda", d)
		ev := events(c.Run(t.Context()))
		clk.add(15 * time.Minute)
		if s.want == "" {
			if len(ev) != 0 {
				t.Errorf("step %d: events = %+v", i, ev)
			}
			continue
		}
		if len(ev) != 1 {
			t.Fatalf("step %d: events = %+v, want one", i, ev)
		}
		if ev[0].Key != "disks/"+serialSATA+"/ata.197" {
			t.Errorf("step %d: key = %q", i, ev[0].Key)
		}
		if !strings.Contains(ev[0].Text, s.want) || !strings.Contains(ev[0].Text, serialSATA) {
			t.Errorf("step %d: text = %q, want %q", i, ev[0].Text, s.want)
		}
	}
}

func TestSeveralCountersRiseSeparately(t *testing.T) {
	src := &fakeSource{}
	d := nvme(serialNVMe)
	src.set("nvme0n1", d)
	clk := newClock()
	c := fixtureCheck(t, src, clk, nil)

	d.nvmeCtr["media_errors"] = 1
	d.nvmeCtr["num_err_log_entries"] = 5
	src.set("nvme0n1", d)
	ev := events(c.Run(t.Context()))
	if len(ev) != 2 {
		t.Fatalf("events = %+v, want two", ev)
	}
	if ev[0].Key != "disks/"+serialNVMe+"/nvme.media_errors" || !strings.Contains(ev[0].Text, "0→1") {
		t.Errorf("first = %+v", ev[0])
	}
	if ev[1].Key != "disks/"+serialNVMe+"/nvme.num_err_log_entries" || !strings.Contains(ev[1].Text, "0→5") {
		t.Errorf("second = %+v", ev[1])
	}
}

func TestCounterNotReportedIsNotAnError(t *testing.T) {
	src := &fakeSource{}
	d := sata(serialSATA)
	delete(d.ata, 188)
	src.set("sda", d)
	clk := newClock()
	c := fixtureCheck(t, src, clk, nil)

	res := c.Run(t.Context())
	if res.Err != nil || len(events(res)) != 0 {
		t.Fatalf("run = %+v", res)
	}
	d.ata[188] = 6 // first report sets the baseline, no rise
	src.set("sda", d)
	if ev := events(c.Run(t.Context())); len(ev) != 0 {
		t.Errorf("events = %+v", ev)
	}
}

func checkAspects(t *testing.T, res health.CheckResult, want map[string]health.Verdict,
	badText map[string]string, absent []string,
) {
	t.Helper()

	for aspect, v := range want {
		r := mustFind(t, res, "disks/"+serialSATA+"/"+aspect)
		if r.Verdict != v || r.Kind != health.State {
			t.Errorf("%s = %+v, want verdict %v", aspect, r, v)
		}
		if v == health.Bad && !strings.Contains(r.Text, serialSATA) {
			t.Errorf("%s text %q lacks the serial", aspect, r.Text)
		}
	}
	for aspect, text := range badText {
		if r := mustFind(t, res, "disks/"+serialSATA+"/"+aspect); !strings.Contains(r.Text, text) {
			t.Errorf("%s text = %q, want %q", aspect, r.Text, text)
		}
	}
	for _, aspect := range absent {
		if r, ok := find(res, "disks/"+serialSATA+"/"+aspect); ok {
			t.Errorf("%s reported for a drive without it: %+v", aspect, r)
		}
	}
}

func TestDriveConditions(t *testing.T) {
	type want map[string]health.Verdict
	tests := []struct {
		name    string
		drive   drive
		want    want
		absent  []string
		badText map[string]string
	}{
		{
			name:  "healthy sata",
			drive: sata(serialSATA),
			want: want{
				"health": health.Good, "selftest": health.Good, "temp": health.Good, "missing": health.Good,
			},
			absent: []string{"nvme-warning", "spare", "wear"},
		},
		{
			name:  "healthy nvme",
			drive: nvme(serialSATA),
			want: want{
				"health": health.Good, "nvme-warning": health.Good, "spare": health.Good,
				"wear": health.Good, "selftest": health.Good, "temp": health.Good, "missing": health.Good,
			},
		},
		{
			name:    "health failed",
			drive:   func() drive { d := sata(serialSATA); d.failed = true; return d }(),
			want:    want{"health": health.Bad},
			badText: map[string]string{"health": "FAILED"},
		},
		{
			name:    "critical warning",
			drive:   func() drive { d := nvme(serialSATA); d.warning = 4; return d }(),
			want:    want{"nvme-warning": health.Bad},
			badText: map[string]string{"nvme-warning": "0x04"},
		},
		{
			name:    "spare at threshold",
			drive:   func() drive { d := nvme(serialSATA); d.spare = 10; return d }(),
			want:    want{"spare": health.Bad},
			badText: map[string]string{"spare": "10%"},
		},
		{
			name:  "spare above threshold",
			drive: func() drive { d := nvme(serialSATA); d.spare = 11; return d }(),
			want:  want{"spare": health.Good},
		},
		{
			name:    "wear above limit",
			drive:   func() drive { d := nvme(serialSATA); d.used = 81; return d }(),
			want:    want{"wear": health.Bad},
			badText: map[string]string{"wear": "81%"},
		},
		{
			name:  "wear at limit",
			drive: func() drive { d := nvme(serialSATA); d.used = 80; return d }(),
			want:  want{"wear": health.Good},
		},
		{
			name:    "self-test failed",
			drive:   func() drive { d := sata(serialSATA); d.testBad = true; return d }(),
			want:    want{"selftest": health.Bad},
			badText: map[string]string{"selftest": "failed"},
		},
		{
			name:    "nvme self-test failed",
			drive:   func() drive { d := nvme(serialSATA); d.testBad = true; return d }(),
			want:    want{"selftest": health.Bad},
			badText: map[string]string{"selftest": "failed"},
		},
		{
			name:   "no self-test on a new drive",
			drive:  func() drive { d := sata(serialSATA); d.noTest = true; return d }(),
			want:   want{"health": health.Good},
			absent: []string{"selftest"},
		},
		{
			name:    "self-test too old",
			drive:   func() drive { d := sata(serialSATA); d.testAge = 193; return d }(),
			want:    want{"selftest": health.Bad},
			badText: map[string]string{"selftest": "193 h"},
		},
		{
			name:  "self-test exactly max age",
			drive: func() drive { d := sata(serialSATA); d.testAge = 192; return d }(),
			want:  want{"selftest": health.Good},
		},
		{
			name: "ata self-test hours wrapped",
			drive: func() drive {
				d := sata(serialSATA)
				d.hours = 70000
				raw := int64(70000 - 10 - 65536)
				d.testRaw = &raw
				return d
			}(),
			want: want{"selftest": health.Good},
		},
		{
			name: "ata self-test hours wrapped and old",
			drive: func() drive {
				d := sata(serialSATA)
				d.hours = 70000
				raw := int64(70000 - 500 - 65536)
				d.testRaw = &raw
				return d
			}(),
			want: want{"selftest": health.Bad},
		},
		{
			name:    "sata too hot",
			drive:   func() drive { d := sata(serialSATA); d.temp = 56; return d }(),
			want:    want{"temp": health.Bad},
			badText: map[string]string{"temp": "56°C"},
		},
		{
			name:  "sata at limit",
			drive: func() drive { d := sata(serialSATA); d.temp = 55; return d }(),
			want:  want{"temp": health.Good},
		},
		{
			name:    "nvme too hot",
			drive:   func() drive { d := nvme(serialSATA); d.temp = 71; return d }(),
			want:    want{"temp": health.Bad},
			badText: map[string]string{"temp": "70°C"},
		},
		{
			name:  "nvme warm but within limit",
			drive: func() drive { d := nvme(serialSATA); d.temp = 60; return d }(),
			want:  want{"temp": health.Good},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := &fakeSource{}
			src.set("sda", tt.drive)
			c := newLoaded(src, disksCfg(), tempsCfg(0), newClock().now)
			res := c.Run(t.Context())
			if res.Err != nil {
				t.Fatalf("Run: %v", res.Err)
			}
			checkAspects(t, res, tt.want, tt.badText, tt.absent)
		})
	}
}

func TestTemperatureThreshold(t *testing.T) {
	src := &fakeSource{}
	d := nvme(serialNVMe)
	src.set("nvme0n1", d)
	clk := newClock()
	c := fixtureCheck(t, src, clk, tempsCfg(15*time.Minute))
	key := "disks/" + serialNVMe + "/temp"

	steps := []struct {
		temp    int
		advance time.Duration
		want    health.Verdict
	}{
		{72, 0, notJudged},                  // over the limit, below the threshold: no reading
		{72, 15 * time.Minute, health.Bad},  // 15 m over ⇒ PROBLEM
		{73, 15 * time.Minute, health.Bad},  // still over
		{65, 15 * time.Minute, health.Good}, // cooled down
		{72, 15 * time.Minute, notJudged},   // a new streak starts from zero
	}
	var overSince time.Time
	for i, s := range steps {
		clk.add(s.advance)
		d.temp = s.temp
		src.set("nvme0n1", d)
		if i == 0 {
			overSince = clk.now()
		}
		res := c.Run(t.Context())
		if s.want == notJudged {
			if r, ok := find(res, key); ok {
				t.Errorf("step %d: %+v, want no reading", i, r)
			}
			if _, ok := sample(res, "temp.drive."+serialNVMe); !ok {
				t.Errorf("step %d: temperature not sampled", i)
			}

			continue
		}
		r := mustFind(t, res, key)
		if r.Verdict != s.want {
			t.Errorf("step %d: %+v, want %v", i, r, s.want)
		}
		if i == 1 && !r.Since.Equal(overSince) {
			t.Errorf("step %d: Since = %v, want the first over-limit run %v", i, r.Since, overSince)
		}
	}
}

// TestTemperatureIsPerDrive covers two same-model NVMe drives (identical
// sensor names) of which only one overheats: only its serial is Bad.
func TestTemperatureIsPerDrive(t *testing.T) {
	const serialHot, serialCool = "TEST0000000003", "TEST0000000004"
	src := &fakeSource{}
	hot, cool := nvme(serialHot), nvme(serialCool)
	hot.temp, cool.temp = 75, 45
	src.set("nvme0n1", hot)
	src.set("nvme1n1", cool)

	res := newLoaded(src, disksCfg(), tempsCfg(0), newClock().now).Run(t.Context())
	if res.Err != nil {
		t.Fatalf("Run: %v", res.Err)
	}
	if r := mustFind(t, res, "disks/"+serialHot+"/temp"); r.Verdict != health.Bad {
		t.Errorf("hot drive = %+v, want Bad", r)
	}
	if r := mustFind(t, res, "disks/"+serialCool+"/temp"); r.Verdict != health.Good {
		t.Errorf("cool drive = %+v, want Good", r)
	}
	for _, r := range res.Readings {
		if r.Verdict == health.Bad && r.Key != "disks/"+serialHot+"/temp" {
			t.Errorf("unexpected Bad reading %+v", r)
		}
	}
}

// notJudged marks a step whose condition must have no reading at all.
const notJudged health.Verdict = -1

func TestTemperatureGapIsCapped(t *testing.T) {
	src := &fakeSource{}
	d := sata(serialSATA)
	d.temp = 60
	src.set("sda", d)
	clk := newClock()
	c := fixtureCheck(t, src, clk, tempsCfg(30*time.Minute))
	key := "disks/" + serialSATA + "/temp"

	clk.add(2 * time.Hour) // an outage counts at most 1.5 × the interval
	if r, ok := find(c.Run(t.Context()), key); ok {
		t.Fatalf("after gap = %+v, want no reading (22.5 m < 30 m)", r)
	}
	clk.add(15 * time.Minute)
	if r := mustFind(t, c.Run(t.Context()), key); r.Verdict != health.Bad {
		t.Errorf("after 37.5 m = %+v, want Bad", r)
	}
}

func TestTemperatureNotJudgedWithoutSection(t *testing.T) {
	src := &fakeSource{}
	d := sata(serialSATA)
	d.temp = 90
	src.set("sda", d)
	res := newLoaded(src, disksCfg(), nil, newClock().now).Run(t.Context())
	if r, ok := find(res, "disks/"+serialSATA+"/temp"); ok {
		t.Errorf("temp judged without a temperatures section: %+v", r)
	}
	if v, ok := sample(res, "temp.drive."+serialSATA); !ok || v != 90 {
		t.Errorf("temp sample = %v, %v", v, ok)
	}
}

func TestSamples(t *testing.T) {
	src := &fakeSource{}
	src.set("sda", sata(serialSATA))
	src.set("nvme0n1", nvme(serialNVMe))
	res := newLoaded(src, disksCfg(), nil, newClock().now).Run(t.Context())

	want := map[string]float64{
		"temp.drive." + serialSATA:                34,
		"drive." + serialSATA + ".written_bytes":  1000 * 512,
		"temp.drive." + serialNVMe:                38,
		"drive." + serialNVMe + ".wear":           1,
		"drive." + serialNVMe + ".written_bytes":  2000 * 512000,
		"drive." + serialSATA + ".selftest_age_h": 10,
		"drive." + serialNVMe + ".selftest_age_h": 10,
		"drive." + serialNVMe + ".spare":          100,
	}
	for metric, v := range want {
		if got, ok := sample(res, metric); !ok || got != v {
			t.Errorf("%s = %v, %v; want %v", metric, got, ok, v)
		}
	}
	if _, ok := sample(res, "drive."+serialSATA+".wear"); ok {
		t.Error("SATA wear sampled, want not reported")
	}
	if _, ok := sample(res, "drive."+serialSATA+".spare"); ok {
		t.Error("SATA spare sampled, want not reported")
	}
	if len(res.Samples) != len(want) {
		t.Errorf("samples = %+v", res.Samples)
	}
}

func TestSelfTestAgeSample(t *testing.T) {
	failed := sata(serialSATA)
	failed.testBad = true
	none := nvme(serialNVMe)
	none.noTest = true

	src := &fakeSource{}
	src.set("sda", failed)
	src.set("nvme0n1", none)
	res := newLoaded(src, disksCfg(), nil, newClock().now).Run(t.Context())
	if v, ok := sample(res, "drive."+serialSATA+".selftest_age_h"); !ok || v != 10 {
		t.Errorf("failed self-test age = %v, %v; want 10 (the newest finished test)", v, ok)
	}
	if _, ok := sample(res, "drive."+serialNVMe+".selftest_age_h"); ok {
		t.Error("self-test age sampled without a finished self-test")
	}
}

func TestMissingDrive(t *testing.T) {
	src := &fakeSource{}
	src.set("sda", sata(serialSATA))
	src.set("nvme0n1", nvme(serialNVMe))
	clk := newClock()
	c := fixtureCheck(t, src, clk, nil)

	src.remove("nvme0n1")
	res := c.Run(t.Context())
	r := mustFind(t, res, "disks/"+serialNVMe+"/missing")
	if r.Verdict != health.Bad || r.Kind != health.State {
		t.Fatalf("missing = %+v", r)
	}
	if !strings.Contains(r.Text, serialNVMe) || !strings.Contains(r.Text, "Example NVMe SSD") {
		t.Errorf("text = %q", r.Text)
	}
	if r := mustFind(t, res, "disks/"+serialSATA+"/missing"); r.Verdict != health.Good {
		t.Errorf("present drive missing = %+v", r)
	}
	if _, ok := find(res, "disks/"+serialNVMe+"/health"); ok {
		t.Error("missing drive judged for health")
	}

	src.set("nvme0n1", nvme(serialNVMe))
	res = c.Run(t.Context())
	if r := mustFind(t, res, "disks/"+serialNVMe+"/missing"); r.Verdict != health.Good {
		t.Errorf("returned drive missing = %+v", r)
	}
	if ev := events(res); len(ev) != 0 {
		t.Errorf("returned known drive announced: %+v", ev)
	}
}

func TestRetire(t *testing.T) {
	src := &fakeSource{}
	src.set("sda", sata(serialSATA))
	src.set("nvme0n1", nvme(serialNVMe))
	clk := newClock()
	c := fixtureCheck(t, src, clk, nil)

	if c.Retire("TEST9999999999") {
		t.Error("Retire(unknown) = true")
	}
	if !c.Retire(serialNVMe) {
		t.Fatal("Retire(known) = false")
	}
	src.remove("nvme0n1")
	res := c.Run(t.Context())
	if r, ok := find(res, "disks/"+serialNVMe+"/missing"); ok {
		t.Errorf("retired drive judged missing: %+v", r)
	}
	for _, b := range c.Baselines() {
		if b.Serial == serialNVMe && !b.Retired {
			t.Errorf("baseline not retired: %+v", b)
		}
	}
}

func TestDiscoveryFailure(t *testing.T) {
	src := &fakeSource{}
	src.set("sda", sata(serialSATA))
	clk := newClock()
	c := fixtureCheck(t, src, clk, nil)

	src.drivesErr = errors.New("sysfs unavailable")
	res := c.Run(t.Context())
	if res.Err == nil || !strings.Contains(res.Err.Error(), "sysfs unavailable") {
		t.Fatalf("Err = %v", res.Err)
	}
	if len(res.Readings) != 0 {
		t.Errorf("readings on failed discovery: %+v", res.Readings)
	}
}

func TestContextCancelled(t *testing.T) {
	src := &fakeSource{}
	src.set("sda", sata(serialSATA))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	res := newLoaded(src, disksCfg(), nil, newClock().now).Run(ctx)
	if !errors.Is(res.Err, context.Canceled) {
		t.Errorf("Err = %v", res.Err)
	}
}

// checkUnreadable asserts that res reports unreadable drives through Err only,
// with each of want in the error text.
func checkUnreadable(t *testing.T, res health.CheckResult, want ...string) {
	t.Helper()

	if !errors.Is(res.Err, ErrUnreadable) {
		t.Fatalf("Err = %v, want ErrUnreadable", res.Err)
	}
	for _, w := range want {
		if !strings.Contains(res.Err.Error(), w) {
			t.Errorf("Err %q lacks %q", res.Err, w)
		}
	}
	for _, r := range res.Readings {
		if r.Verdict == health.Unknown && r.Kind == health.State {
			t.Errorf("Unknown reading: %+v", r)
		}
	}
}

func TestUnreadableKnownDriveReportedInErr(t *testing.T) {
	for _, tt := range []struct {
		name string
		out  output
		text string
	}{
		{
			"open failure",
			output{stdout: []byte(`{"smartctl":{"messages":[{"string":"open failed"}]}}`), status: 2},
			"open failed",
		},
		{"exec failure", output{err: errors.New("smartctl timed out")}, "smartctl timed out"},
		{"garbage", output{stdout: []byte("not json")}, "decode"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			src := &fakeSource{}
			src.set("sda", sata(serialSATA))
			src.set("nvme0n1", nvme(serialNVMe))
			clk := newClock()
			c := fixtureCheck(t, src, clk, tempsCfg(0))

			src.runs["nvme0n1"] = tt.out
			res := c.Run(t.Context())
			checkUnreadable(t, res, "/dev/nvme0n1", serialNVMe, tt.text)
			for _, r := range res.Readings {
				if strings.Contains(r.Key, serialNVMe) {
					t.Errorf("unreadable drive judged: %+v", r)
				}
			}
			if r := mustFind(t, res, "disks/"+serialSATA+"/health"); r.Verdict != health.Good {
				t.Errorf("readable drive = %+v", r)
			}

			src.set("nvme0n1", nvme(serialNVMe))
			res = c.Run(t.Context())
			if res.Err != nil || len(events(res)) != 0 {
				t.Errorf("readable again = %+v", res)
			}
		})
	}
}

func TestUnreadableUnattributedNodeLeavesMissingUnjudged(t *testing.T) {
	src := &fakeSource{}
	src.set("sda", sata(serialSATA))
	src.set("nvme0n1", nvme(serialNVMe))
	clk := newClock()
	c := fixtureCheck(t, src, clk, nil)

	// The NVMe drive moved to a new node that cannot be read: it may be the
	// missing drive, so missing is not judged.
	src.remove("nvme0n1")
	src.nodes = append(src.nodes, "nvme1n1")
	src.runs["nvme1n1"] = output{status: 2}
	res := c.Run(t.Context())
	checkUnreadable(t, res, "/dev/nvme1n1", "missing-drive check suspended this run")
	if strings.Contains(res.Err.Error(), serialNVMe) {
		t.Errorf("Err %q names a serial it cannot know", res.Err)
	}
	for _, r := range res.Readings {
		if strings.HasPrefix(r.Key, "disks/"+serialNVMe+"/") {
			t.Errorf("unattributable drive judged: %+v", r)
		}
	}
	if r := mustFind(t, res, "disks/"+serialSATA+"/missing"); r.Verdict != health.Good {
		t.Errorf("present drive missing = %+v", r)
	}
}

func TestUnreadableOnFirstRun(t *testing.T) {
	src := &fakeSource{}
	src.set("sda", sata(serialSATA))
	src.nodes = append(src.nodes, "sdb", "sdc")
	src.runs["sdb"] = output{status: 2}
	src.runs["sdc"] = output{err: errors.New("smartctl timed out")}
	res := newLoaded(src, disksCfg(), nil, newClock().now).Run(t.Context())
	checkUnreadable(t, res, "/dev/sdb", "/dev/sdc", "smartctl timed out")
	ev := events(res)
	if len(ev) != 1 || !strings.Contains(ev[0].Text, serialSATA) || strings.Contains(ev[0].Text, "sdb") {
		t.Errorf("events = %+v", ev)
	}
}

func TestExcludeSerials(t *testing.T) {
	src := &fakeSource{}
	src.set("sda", sata(serialSATA))
	src.set("nvme0n1", nvme(serialNVMe))
	cfg := disksCfg()
	cfg.ExcludeSerials = []string{serialNVMe, serialNew}
	clk := newClock()
	c := newLoaded(src, cfg, tempsCfg(0), clk.now)

	res := c.Run(t.Context())
	ev := events(res)
	if len(ev) != 1 || !strings.Contains(ev[0].Text, serialSATA) || strings.Contains(ev[0].Text, serialNVMe) {
		t.Errorf("events = %+v, want only the SATA drive announced", ev)
	}
	for _, r := range res.Readings {
		if strings.Contains(r.Key, serialNVMe) {
			t.Errorf("excluded drive reported: %+v", r)
		}
	}
	for _, s := range res.Samples {
		if strings.Contains(s.Metric, serialNVMe) {
			t.Errorf("excluded drive sampled: %+v", s)
		}
	}
	for _, b := range c.Baselines() {
		if b.Serial == serialNVMe {
			t.Errorf("excluded drive baselined: %+v", b)
		}
	}

	// A known drive excluded later is no longer judged, missing included.
	c.LoadBaselines(append(c.Baselines(), store.Baseline{
		Serial: serialNew, Model: "Example HDD", Bus: "ATA", FirstSeenAt: clk.t, LastSeenAt: clk.t,
	}))
	for _, r := range c.Run(t.Context()).Readings {
		if strings.Contains(r.Key, serialNew) {
			t.Errorf("excluded known drive reported: %+v", r)
		}
	}
}

func TestDuplicateSerialJudgedOnce(t *testing.T) {
	src := &fakeSource{}
	src.set("sda", sata(serialSATA))
	src.set("sdb", sata(serialSATA))
	res := newLoaded(src, disksCfg(), nil, newClock().now).Run(t.Context())
	n := 0
	for _, r := range res.Readings {
		if r.Key == "disks/"+serialSATA+"/health" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("health readings = %d, want 1", n)
	}
}

func TestBaselinesRoundTrip(t *testing.T) {
	src := &fakeSource{}
	d := sata(serialSATA)
	d.ata[5] = 3
	src.set("sda", d)
	clk := newClock()
	first := clk.t
	c := fixtureCheck(t, src, clk, nil)
	c.Run(t.Context())

	exported := c.Baselines()
	if len(exported) != 1 {
		t.Fatalf("baselines = %+v", exported)
	}
	b := exported[0]
	if b.Serial != serialSATA || b.Model != "Example SATA SSD" || b.Bus != "ATA" || b.Retired {
		t.Errorf("identity = %+v", b)
	}
	if !b.FirstSeenAt.Equal(first) || !b.LastSeenAt.Equal(clk.t) {
		t.Errorf("seen = %v .. %v", b.FirstSeenAt, b.LastSeenAt)
	}
	var data struct {
		Counters map[string]int64 `json:"counters"`
	}
	if err := json.Unmarshal(b.Data, &data); err != nil || data.Counters["ata.5"] != 3 {
		t.Errorf("data = %s (%v)", b.Data, err)
	}

	// A restarted daemon restores the baselines: not a first run, rises still detected.
	restored := New(src, disksCfg(), nil, clk.now)
	restored.LoadBaselines(exported)
	d.ata[5] = 4
	src.set("sda", d)
	ev := events(restored.Run(t.Context()))
	if len(ev) != 1 || !strings.Contains(ev[0].Text, "3→4") {
		t.Errorf("events after restore = %+v", ev)
	}
}

func TestCorruptBaselineDataRebaselines(t *testing.T) {
	src := &fakeSource{}
	d := sata(serialSATA)
	d.ata[5] = 7
	src.set("sda", d)
	clk := newClock()
	c := New(src, disksCfg(), nil, clk.now)
	c.LoadBaselines([]store.Baseline{{
		Serial: serialSATA, Model: "Example SATA SSD", Bus: "ATA", Data: json.RawMessage(`{broken`),
		FirstSeenAt: clk.t, LastSeenAt: clk.t,
	}})
	if ev := events(c.Run(t.Context())); len(ev) != 0 {
		t.Errorf("events = %+v", ev)
	}
	d.ata[5] = 8
	src.set("sda", d)
	if ev := events(c.Run(t.Context())); len(ev) != 1 {
		t.Errorf("rise after re-baseline = %+v", ev)
	}
}

func TestSelfTestBaselineTracked(t *testing.T) {
	src := &fakeSource{}
	d := nvme(serialNVMe)
	src.set("nvme0n1", d)
	clk := newClock()
	seen := clk.t
	c := fixtureCheck(t, src, clk, nil)
	c.Run(t.Context())

	var data struct {
		Hours int64 `json:"selftest_last_seen_hours"`
		OKAt  int64 `json:"selftest_ok_at"`
	}
	if err := json.Unmarshal(c.Baselines()[0].Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.Hours != 1490 || data.OKAt != seen.Unix() {
		t.Errorf("self-test baseline = %+v", data)
	}
}

func TestCounterNames(t *testing.T) {
	keys := []string{
		CounterNVMeMediaErrors, CounterNVMeErrorLog,
		ATACounter(5), ATACounter(187), ATACounter(188), ATACounter(197), ATACounter(198), ATACounter(199),
	}
	names := map[string]bool{}
	for _, k := range keys {
		n := counterName(k)
		if n == k || names[n] {
			t.Errorf("counterName(%q) = %q", k, n)
		}
		names[n] = true
	}
	if counterName("ata.1") != "ata.1" {
		t.Error("unknown counter not shown by key")
	}
}

func TestNoSelfTestBadOnlyAfterMaxAge(t *testing.T) {
	src := &fakeSource{}
	d := sata(serialSATA)
	d.noTest = true
	src.set("sda", d)
	clk := newClock()
	c := fixtureCheck(t, src, clk, nil) // first seen at the start time
	key := "disks/" + serialSATA + "/selftest"

	clk.add(8*24*time.Hour - 15*time.Minute) // exactly selftest_max_age after first seen
	if r, ok := find(c.Run(t.Context()), key); ok {
		t.Errorf("at max age = %+v, want no reading", r)
	}
	clk.add(15 * time.Minute)
	r := mustFind(t, c.Run(t.Context()), key)
	if r.Verdict != health.Bad || !strings.Contains(r.Text, "no finished self-test") {
		t.Errorf("past max age = %+v, want Bad", r)
	}

	// The deadline counts from the stored FirstSeenAt, across restarts.
	restored := New(src, disksCfg(), nil, clk.now)
	restored.LoadBaselines(c.Baselines())
	if r := mustFind(t, restored.Run(t.Context()), key); r.Verdict != health.Bad {
		t.Errorf("after restore = %+v, want Bad", r)
	}
}

func TestRunRefusedUntilBaselinesLoaded(t *testing.T) {
	src := &fakeSource{}
	src.set("sda", sata(serialSATA))
	c := New(src, disksCfg(), nil, newClock().now)

	res := c.Run(t.Context())
	if !errors.Is(res.Err, ErrBaselinesNotLoaded) || len(res.Readings) != 0 || len(res.Samples) != 0 {
		t.Fatalf("unloaded run = %+v", res)
	}
	if len(src.calls) != 0 {
		t.Errorf("drives read before baselines loaded: %v", src.calls)
	}

	c.LoadBaselines(nil) // nothing stored: a genuine first run
	res = c.Run(t.Context())
	if res.Err != nil {
		t.Fatalf("Err = %v", res.Err)
	}
	if ev := events(res); len(ev) != 1 || ev[0].Key != "disks/drives" {
		t.Errorf("events = %+v, want the first-run inventory", ev)
	}
}

func TestSelfTestWithoutHoursIsNotStale(t *testing.T) {
	src := &fakeSource{nodes: []string{"sda"}, runs: map[string]output{"sda": {stdout: []byte(`{
		"serial_number": "TEST0000000001", "model_name": "Example SATA SSD",
		"device": {"name": "/dev/sda", "protocol": "ATA"},
		"ata_smart_self_test_log": {"standard": {"table": [{"status": {"value": 0, "string": "ok"}}]}}
	}`)}}}
	res := newLoaded(src, disksCfg(), tempsCfg(0), newClock().now).Run(t.Context())
	if r, ok := find(res, "disks/"+serialSATA+"/selftest"); ok {
		t.Errorf("selftest judged without an age: %+v", r)
	}
	if r, ok := find(res, "disks/"+serialSATA+"/temp"); ok {
		t.Errorf("temp judged without a temperature: %+v", r)
	}
	if len(res.Samples) != 0 {
		t.Errorf("samples = %+v", res.Samples)
	}
}

func TestClampInt64(t *testing.T) {
	if got := clampInt64(math.MaxUint64); got != math.MaxInt64 {
		t.Errorf("clampInt64(max) = %d", got)
	}
	if got := clampInt64(42); got != 42 {
		t.Errorf("clampInt64(42) = %d", got)
	}
}
