package disks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

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
	// paths overrides the sysfs path of a node (see nodePath).
	paths map[string]string
	runs  map[string]output
	// hook, when set, runs first in Run (drives are read concurrently).
	hook func(ctx context.Context, node string) error
	// woken, when it has the node, is the output of a forced read
	// (wake=true); otherwise a forced read returns runs[node].
	woken map[string]output
	mu    sync.Mutex
	calls []string
	// wakes are the nodes of the forced reads, in order.
	wakes []string
}

func (f *fakeSource) Drives() ([]Node, error) {
	out := make([]Node, 0, len(f.nodes))
	for _, n := range f.nodes {
		out = append(out, Node{Name: n, Path: f.nodePath(n)})
	}

	return out, f.drivesErr
}

// Synthetic sysfs paths of the three transports.
const (
	pathSATA = "/sys/devices/pci0000:00/0000:00:1f.2/ata1/host0/target0:0:0/0:0:0:0/block/"
	pathNVMe = "/sys/devices/pci0000:00/0000:00:1d.0/0000:01:00.0/nvme/nvme0/"
	pathUSB  = "/sys/devices/pci0000:00/0000:00:14.0/usb2/2-1/2-1:1.0/host6/target6:0:0/6:0:0:0/block/"
)

// nodePath is the node's path in f.paths, else a SATA path (an NVMe one for an
// nvme node).
func (f *fakeSource) nodePath(node string) string {
	if p, ok := f.paths[node]; ok {
		return p
	}
	if strings.HasPrefix(node, "nvme") {
		return pathNVMe + node
	}

	return pathSATA + node
}

// setPath puts the node at the sysfs path.
func (f *fakeSource) setPath(node, path string) {
	if f.paths == nil {
		f.paths = map[string]string{}
	}
	f.paths[node] = path + node
}

func (f *fakeSource) Run(ctx context.Context, node string, wake bool) ([]byte, int, error) {
	f.mu.Lock()
	f.calls = append(f.calls, node)
	o := f.runs[node]
	if wake {
		f.wakes = append(f.wakes, node)
		if w, ok := f.woken[node]; ok {
			o = w
		}
	}
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
	serial    string
	model     string
	nvme      bool
	failed    bool
	noVerdict bool // smart_status missing from the output
	temp      int
	hours     int64
	noTest    bool  // no finished self-test in the log
	testAge   int64 // hours between the newest finished self-test and now
	testRaw   *int64
	testBad   bool
	// log, when set, is the whole self-test log, newest first, replacing the
	// single entry testAge/testBad describe.
	log []testRun
	ata map[int]uint64
	// ssdWear, when non-zero, makes a SATA drive a solid-state one whose
	// attribute 177 (wear levelling) has this normalised value.
	ssdWear int
	nvmeCtr map[string]uint64
	warning int
	spare   int
	spareTh int
	used    int
	written uint64
	// drop names top-level keys of the smartctl JSON to leave out.
	drop []string
}

// testRun is one self-test log entry: the lifetime hours it ran at and whether
// it failed.
type testRun struct {
	hours int64
	bad   bool
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
		"power_on_time": map[string]any{"hours": d.hours},
		"temperature":   map[string]any{"current": d.temp},
	}
	if !d.noVerdict {
		m["smart_status"] = map[string]any{"passed": !d.failed}
	}
	if d.nvme {
		d.nvmeJSON(m)
	} else {
		d.ataJSON(m)
	}
	for _, k := range d.drop {
		delete(m, k)
	}
	out, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}

	return out
}

// runs returns the self-test log entries, newest first.
func (d drive) runs() []testRun {
	switch {
	case d.log != nil:
		return d.log
	case d.noTest:
		return nil
	}

	return []testRun{{hours: d.testHours(), bad: d.testBad}}
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
	runs := d.runs()
	table := make([]any, 0, len(runs))
	for _, run := range runs {
		result := 0
		if run.bad {
			result = 7
		}
		table = append(table, map[string]any{
			"self_test_result": map[string]any{"value": result, "string": "result"},
			"power_on_hours":   run.hours,
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
	if d.ssdWear != 0 {
		m["rotation_rate"] = 0
		rows = append(
			rows,
			map[string]any{"id": ataWearLeveling, "value": d.ssdWear, "raw": map[string]any{"value": 1}},
		)
	}
	m["ata_smart_attributes"] = map[string]any{"table": rows}
	runs := d.runs()
	table := make([]any, 0, len(runs))
	for _, run := range runs {
		status := 0
		if run.bad {
			status = 0x71
		}
		table = append(table, map[string]any{
			"status":         map[string]any{"value": status, "string": "Completed: read failure"},
			"lifetime_hours": run.hours,
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
		ForceReadAfter: config.Duration(24 * time.Hour),
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
		if !strings.Contains(ev[0].Text, s.want) || !strings.Contains(ev[0].Text, serialSATA) ||
			!strings.Contains(ev[0].Text, "pending sectors (ATA 197) rose") {
			t.Errorf("step %d: text = %q, want %q", i, ev[0].Text, s.want)
		}
	}
}

func TestSeveralCountersRiseSeparately(t *testing.T) {
	src := &fakeSource{}
	d := sata(serialSATA)
	src.set("sda", d)
	clk := newClock()
	c := fixtureCheck(t, src, clk, nil)

	d.ata[5], d.ata[199] = 1, 5
	src.set("sda", d)
	ev := events(c.Run(t.Context()))
	if len(ev) != 2 {
		t.Fatalf("events = %+v, want two", ev)
	}
	if ev[0].Key != "disks/"+serialSATA+"/ata.199" || !strings.Contains(ev[0].Text, "0→5") {
		t.Errorf("first = %+v", ev[0])
	}
	if ev[1].Key != "disks/"+serialSATA+"/ata.5" || !strings.Contains(ev[1].Text, "0→1") {
		t.Errorf("second = %+v", ev[1])
	}
}

// TestBuiltinCounterRise proves a rise of each built-in counter, including the
// ones added in feature 002, raises one event against the drive's own baseline
// (US1 acceptance 1, SC-001).
func TestBuiltinCounterRise(t *testing.T) {
	for _, tc := range []struct {
		id   int
		name string
	}{
		{5, "reallocated sectors"},
		{179, "used reserved blocks"},
		{181, "program fail"},
		{182, "erase fail"},
		{183, "runtime bad blocks"},
		{187, "reported uncorrectable errors"},
		{188, "command timeouts"},
		{195, "hardware ECC"},
		{197, "pending sectors"},
		{198, "offline uncorrectable sectors"},
		{199, "UDMA CRC errors"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := &fakeSource{}
			d := sata(serialSATA)
			d.ata = map[int]uint64{tc.id: 4}
			src.set("sda", d)
			c := fixtureCheck(t, src, newClock(), nil)

			d.ata = map[int]uint64{tc.id: 6}
			src.set("sda", d)
			ev := events(c.Run(t.Context()))
			want := fmt.Sprintf("drive %s (%s): %s (ATA %d) rose 4→6", serialSATA, d.model, tc.name, tc.id)
			if len(ev) != 1 || ev[0].Text != want {
				t.Errorf("events = %+v, want text %q", ev, want)
			}
		})
	}
}

func TestExtraCounters(t *testing.T) {
	cfg := disksCfg()
	cfg.ExtraCounters = []int{1}
	cfg.ExtraCountersBySerial = map[string][]int{serialSATA: {232}, "TEST0000000099": {233}}
	src := &fakeSource{}
	d := sata(serialSATA)
	d.ata = map[int]uint64{1: 1, 232: 1, 233: 1}
	src.set("sda", d)
	c := newLoaded(src, cfg, nil, newClock().now)
	c.Run(t.Context())

	d.ata = map[int]uint64{1: 2, 232: 3, 233: 9}
	src.set("sda", d)
	ev := events(c.Run(t.Context()))
	if len(ev) != 2 {
		t.Fatalf("events = %+v, want ATA 1 and ATA 232 only", ev)
	}
	for i, want := range []string{"attribute 1 (ATA 1) rose 1→2", "attribute 232 (ATA 232) rose 1→3"} {
		if !strings.Contains(ev[i].Text, want) {
			t.Errorf("event %d = %q, want %q", i, ev[i].Text, want)
		}
	}
}

// TestNVMeErrorLogSilent proves a rising NVMe error log count neither raises
// an event nor stays in the baseline; it is only sampled (US1 acceptance 10).
func TestNVMeErrorLogSilent(t *testing.T) {
	src := &fakeSource{}
	d := nvme(serialNVMe)
	src.set("nvme0n1", d)
	c := fixtureCheck(t, src, newClock(), nil)
	c.LoadBaselines([]store.Baseline{{
		Serial: serialNVMe, Model: d.model, Bus: "NVMe",
		Data: json.RawMessage(`{"counters":{"nvme.media_errors":0,"nvme.num_err_log_entries":0}}`),
	}})

	d.nvmeCtr["num_err_log_entries"] = 5
	src.set("nvme0n1", d)
	res := c.Run(t.Context())
	if ev := events(res); len(ev) != 0 {
		t.Errorf("events = %+v, want none", ev)
	}
	if v, ok := sample(res, "drive."+serialNVMe+".nvme_err_log"); !ok || v != 5 {
		t.Errorf("nvme_err_log sample = %v, %v; want 5", v, ok)
	}
	var data baselineData
	if err := json.Unmarshal(c.Baselines()[0].Data, &data); err != nil {
		t.Fatal(err)
	}
	if _, ok := data.Counters["nvme.num_err_log_entries"]; ok {
		t.Errorf("baseline counters = %v, want the legacy key dropped", data.Counters)
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
			name:  "no self-test on a new drive",
			drive: func() drive { d := sata(serialSATA); d.noTest = true; return d }(),
			want:  want{"health": health.Good, "selftest": health.Good},
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
		"drive." + serialSATA + ".selftest_age_h": 0,
		"drive." + serialNVMe + ".selftest_age_h": 0,
		"drive." + serialNVMe + ".spare":          100,
		"drive." + serialNVMe + ".nvme_err_log":   0,
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
	clk := newClock()
	c := fixtureCheck(t, src, clk, nil)
	clk.add(47*time.Hour + 45*time.Minute) // 48 h after first seen
	res := c.Run(t.Context())
	for _, serial := range []string{serialSATA, serialNVMe} {
		if v, ok := sample(res, "drive."+serial+".selftest_age_h"); !ok || v != 48 {
			t.Errorf("%s self-test age = %v, %v; want 48 calendar hours since first seen", serial, v, ok)
		}
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
	src.set("sda", sata(serialSATA))
	clk := newClock()
	c := fixtureCheck(t, src, clk, nil)
	c.Run(t.Context())

	data := baselineJSON(t, c)
	if data.Format != "ok" || len(data.Head) != 1 || data.Head[0].Hours != 4190 {
		t.Errorf("self-test baseline = %+v", data)
	}
	if data.Newest == nil || data.Newest.Result != "passed" {
		t.Errorf("selftest_newest = %+v", data.Newest)
	}
	if data.OKAt != 0 {
		t.Errorf("selftest_ok_at = %d, want left alone for an adopted log", data.OKAt)
	}
}

// selfTestData is the self-test part of the stored baseline data.
type selfTestData struct {
	Head   []SelfTestID    `json:"selftest_head"`
	Newest *SelfTestNewest `json:"selftest_newest"`
	Format string          `json:"selftest_format"`
	OKAt   int64           `json:"selftest_ok_at"`
}

func baselineJSON(t *testing.T, c *Check) selfTestData {
	t.Helper()

	var data selfTestData
	if err := json.Unmarshal(c.Baselines()[0].Data, &data); err != nil {
		t.Fatal(err)
	}

	return data
}

func TestCounterNames(t *testing.T) {
	want := map[string]string{
		ATACounter(5):          "reallocated sectors",
		ATACounter(179):        "used reserved blocks",
		ATACounter(181):        "program fail",
		ATACounter(182):        "erase fail",
		ATACounter(183):        "runtime bad blocks",
		ATACounter(187):        "reported uncorrectable errors",
		ATACounter(188):        "command timeouts",
		ATACounter(195):        "hardware ECC",
		ATACounter(197):        "pending sectors",
		ATACounter(198):        "offline uncorrectable sectors",
		ATACounter(199):        "UDMA CRC errors",
		ATACounter(1):          "attribute 1",
		CounterNVMeMediaErrors: "media errors (NVMe)",
	}
	for k, w := range want {
		if got := counterName(k); got != w {
			t.Errorf("counterName(%q) = %q, want %q", k, got, w)
		}
	}
	if got := counterLabel(ATACounter(183)); got != "runtime bad blocks (ATA 183)" {
		t.Errorf("counterLabel = %q", got)
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
	if r := mustFind(t, c.Run(t.Context()), key); r.Verdict != health.Good {
		t.Errorf("at max age = %+v, want Good", r)
	}
	clk.add(15 * time.Minute)
	r := mustFind(t, c.Run(t.Context()), key)
	const want = "drive " + serialSATA + " (Example SATA SSD): last passed self-test 8 d ago, limit 8 d"
	if r.Verdict != health.Bad || r.Text != want {
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
	if r := mustFind(t, res, "disks/"+serialSATA+"/selftest"); r.Verdict != health.Good {
		t.Errorf("selftest = %+v, want Good", r)
	}
	if r, ok := find(res, "disks/"+serialSATA+"/temp"); ok {
		t.Errorf("temp judged without a temperature: %+v", r)
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

// TestNoVerdictKeepsProblem proves that a read without a health verdict never
// resolves an open "health FAILED" problem and raises its own checker error.
func TestNoVerdictKeepsProblem(t *testing.T) {
	src := &fakeSource{}
	d := sata(serialSATA)
	d.failed = true
	src.set("sda", d)
	clk := newClock()
	c := newLoaded(src, disksCfg(), nil, clk.now)
	engine := health.NewEngine()
	hkey := "disks/" + serialSATA + "/health"
	apply := func() health.CheckResult {
		res := c.Run(t.Context())
		engine.Apply(clk.now(), checkName, 15*time.Minute, 0, res.Readings)

		return res
	}

	apply()
	clk.add(15 * time.Minute)
	apply()
	if cond, ok := engine.Condition(hkey); !ok || !cond.Open {
		t.Fatalf("health problem not open after a failing verdict: %+v", cond)
	}

	d.noVerdict = true
	src.set("sda", d)
	clk.add(15 * time.Minute)
	res := c.Run(t.Context())
	alerts := engine.Apply(clk.now(), checkName, 15*time.Minute, 0, res.Readings)

	if r, ok := find(res, hkey); ok {
		t.Errorf("health reading without a verdict: %+v", r)
	}
	for _, a := range alerts {
		if a.Kind == health.AlertResolved {
			t.Errorf("resolved without a verdict: %+v", a)
		}
	}
	if cond, ok := engine.Condition(hkey); !ok || !cond.Open {
		t.Errorf("problem closed by a read without verdict: %+v", cond)
	}
	errs := health.CheckErrors(res.Err)
	if len(errs) != 1 || errs[0].Key(checkName) != hkey[:len(hkey)-len("health")]+"no-verdict" {
		t.Fatalf("checker errors = %+v (Err %v), want only no-verdict", errs, res.Err)
	}
	if _, ok := find(res, "disks/"+serialSATA+"/selftest"); !ok {
		t.Error("the rest of the read was not judged")
	}

	// The verdict comes back good: the problem resolves.
	d.noVerdict = false
	d.failed = false
	src.set("sda", d)
	clk.add(15 * time.Minute)
	res = c.Run(t.Context())
	resolved := false
	for _, a := range engine.Apply(clk.now(), checkName, 15*time.Minute, 0, res.Readings) {
		resolved = resolved || (a.Kind == health.AlertResolved && a.Key == hkey)
	}
	if !resolved {
		t.Error("problem not resolved by a good verdict")
	}
}

func TestBaselineRecordsHealthVerdict(t *testing.T) {
	src := &fakeSource{}
	d := sata(serialSATA)
	src.set("sda", d)
	clk := newClock()
	c := newLoaded(src, disksCfg(), nil, clk.now)

	for _, tt := range []struct {
		failed, none bool
		want         string
	}{{false, false, "ok"}, {true, false, "failing"}, {false, true, "unknown"}} {
		d.failed, d.noVerdict = tt.failed, tt.none
		src.set("sda", d)
		clk.add(15 * time.Minute)
		c.Run(t.Context())

		var data struct {
			Health string `json:"health"`
		}
		if err := json.Unmarshal(c.Baselines()[0].Data, &data); err != nil || data.Health != tt.want {
			t.Errorf("health = %q (%v), want %q", data.Health, err, tt.want)
		}
	}
}

// runFixture runs a check once over the testdata fixture name and returns the
// result of that run.
func runFixture(t *testing.T, name string, cfg config.Disks, temps *config.Temperatures) health.CheckResult {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("testdata", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	src := &fakeSource{nodes: []string{"n0"}, runs: map[string]output{"n0": {stdout: data}}}
	clk := newClock()

	return newLoaded(src, cfg, temps, clk.now).Run(t.Context())
}

func TestSpareFloor(t *testing.T) {
	const k = "disks/TEST0000000004/spare"
	cfg := disksCfg()
	cfg.SpareFloorPercent = 20

	r := mustFind(t, runFixture(t, "nvme_spare_19", cfg, nil), k)
	if r.Verdict != health.Bad || !strings.Contains(r.Text, "available spare 19% below the 20% floor") {
		t.Errorf("spare 19 over a 20%% floor = %+v, want Bad naming the floor", r)
	}

	cfg.SpareFloorPercent = 19
	if r := mustFind(t, runFixture(t, "nvme_spare_19", cfg, nil), k); r.Verdict != health.Good {
		t.Errorf("spare 19 at a 19%% floor = %+v, want Good", r)
	}
}

func TestSpareAtDriveThreshold(t *testing.T) {
	tests := []struct {
		name         string
		spare, th    int
		floor        int
		verdict      health.Verdict
		text         string
		noThreshold  bool
		wantNoReport bool
	}{
		{
			name: "at threshold, floor also crossed", spare: 10, th: 10, floor: 20, verdict: health.Bad,
			text: "available spare 10% at or below its threshold 10%",
		},
		{
			name: "below threshold", spare: 5, th: 10, floor: 20, verdict: health.Bad,
			text: "available spare 5% at or below its threshold 10%",
		},
		{
			name: "above threshold, below floor", spare: 15, th: 10, floor: 20, verdict: health.Bad,
			text: "available spare 15% below the 20% floor",
		},
		{name: "above both", spare: 50, th: 10, floor: 20, verdict: health.Good},
		{
			name: "threshold above floor", spare: 25, th: 30, floor: 20, verdict: health.Bad,
			text: "available spare 25% at or below its threshold 30%",
		},
		{
			name: "no threshold, floor alone", spare: 15, floor: 20, noThreshold: true, verdict: health.Bad,
			text: "available spare 15% below the 20% floor",
		},
		{name: "no threshold, above floor", spare: 25, floor: 20, noThreshold: true, verdict: health.Good},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := &fakeSource{}
			d := nvme(serialNVMe)
			d.spare, d.spareTh = tt.spare, tt.th
			src.set("nvme0n1", d)
			if tt.noThreshold {
				var m map[string]any
				if err := json.Unmarshal(d.json(), &m); err != nil {
					t.Fatal(err)
				}
				log, ok := m["nvme_smart_health_information_log"].(map[string]any)
				if !ok {
					t.Fatal("no health log in the drive JSON")
				}
				delete(log, "available_spare_threshold")
				out, _ := json.Marshal(m)
				src.runs["nvme0n1"] = output{stdout: out}
			}
			cfg := disksCfg()
			cfg.SpareFloorPercent = tt.floor

			r := mustFind(t, newLoaded(src, cfg, nil, newClock().now).Run(t.Context()), "disks/"+serialNVMe+"/spare")
			if r.Verdict != tt.verdict || !strings.Contains(r.Text, tt.text) {
				t.Errorf("reading = %+v, want %v containing %q", r, tt.verdict, tt.text)
			}
		})
	}
}

// gapKinds returns the checker-error keys of a result.
func gapKinds(res health.CheckResult) []string {
	errs := health.CheckErrors(res.Err)
	out := make([]string, 0, len(errs))
	for _, e := range errs {
		out = append(out, e.Key(checkName))
	}

	return out
}

func TestMissingSection(t *testing.T) {
	tests := []struct {
		name   string
		drive  func() drive
		kind   string
		absent []string // readings that must keep their state (not be emitted)
	}{
		{
			"ata attributes",
			func() drive { d := sata(serialSATA); d.drop = []string{"ata_smart_attributes"}; return d },
			"attributes",
			nil,
		},
		{
			"ata self-test log",
			func() drive { d := sata(serialSATA); d.drop = []string{"ata_smart_self_test_log"}; return d },
			"selftest-log",
			[]string{"selftest"},
		},
		{
			"nvme health log",
			func() drive { d := nvme(serialSATA); d.drop = []string{"nvme_smart_health_information_log"}; return d },
			"nvme-health-log",
			[]string{"nvme-warning", "spare", "wear"},
		},
		{
			"nvme self-test log",
			func() drive { d := nvme(serialSATA); d.drop = []string{"nvme_self_test_log"}; return d },
			"selftest-log",
			[]string{"selftest"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := &fakeSource{}
			d := tt.drive()
			src.set("n0", d)
			clk := newClock()
			cfg := disksCfg()
			cfg.SpareFloorPercent = 20
			c := newLoaded(src, cfg, nil, clk.now)
			full := d
			full.drop = nil
			src.set("n0", full)
			c.Run(t.Context())

			// Past selftest_max_age, so a self-test judged as absent would be Bad.
			clk.add(30 * 24 * time.Hour)
			src.set("n0", d)
			res := c.Run(t.Context())

			want := "disks/" + serialSATA + "/" + tt.kind
			if got := gapKinds(res); !slices.Equal(got, []string{want}) {
				t.Errorf("checker errors = %v, want [%s]", got, want)
			}
			for _, a := range tt.absent {
				if r, ok := find(res, "disks/"+serialSATA+"/"+a); ok {
					t.Errorf("%s judged during the gap: %+v", a, r)
				}
			}
			if r := mustFind(t, res, "disks/"+serialSATA+"/health"); r.Verdict != health.Good {
				t.Errorf("health = %+v, want still judged", r)
			}
		})
	}
}

func TestMissingSectionKeepsCounterBaseline(t *testing.T) {
	src := &fakeSource{}
	d := sata(serialSATA)
	d.ata[5] = 2
	src.set("sda", d)
	clk := newClock()
	c := fixtureCheck(t, src, clk, nil)

	gap := d
	gap.drop = []string{"ata_smart_attributes"}
	src.set("sda", gap)
	if res := c.Run(t.Context()); len(events(res)) != 0 {
		t.Errorf("events during the gap: %+v", events(res))
	}

	// The counter is back, one higher than before the gap: a rise.
	d.ata[5] = 3
	src.set("sda", d)
	clk.add(15 * time.Minute)
	if ev := events(c.Run(t.Context())); len(ev) != 1 || !strings.Contains(ev[0].Text, "2→3") {
		t.Errorf("events after the gap = %+v, want one rise 2→3", ev)
	}
}

func TestSATAWear(t *testing.T) {
	const k = "disks/TEST0000000001/wear"
	cfg := disksCfg()
	cfg.WearPercent = 10

	res := runFixture(t, "sata_ssd_177", cfg, nil)
	if r := mustFind(t, res, k); r.Verdict != health.Bad || !strings.Contains(r.Text, "wear 12% above 10%") {
		t.Errorf("wear 12 over a 10%% limit = %+v, want Bad", r)
	}
	if v, ok := sample(res, "drive.TEST0000000001.wear"); !ok || v != 12 {
		t.Errorf("wear sample = %v, %v, want 12", v, ok)
	}

	cfg.WearPercent = 80
	if r := mustFind(t, runFixture(t, "sata_ssd_177", cfg, nil), k); r.Verdict != health.Good {
		t.Errorf("wear 12 under an 80%% limit = %+v, want Good", r)
	}
}

func TestNoTemperature(t *testing.T) {
	for _, d := range []drive{sata(serialSATA), nvme(serialNVMe)} {
		d.drop = []string{"temperature"}
		src := &fakeSource{}
		src.set("n0", d)
		res := newLoaded(src, disksCfg(), tempsCfg(0), newClock().now).Run(t.Context())

		want := "disks/" + d.serial + "/no-temperature"
		if got := gapKinds(res); !slices.Equal(got, []string{want}) {
			t.Errorf("%s: checker errors = %v, want [%s]", d.serial, got, want)
		}
		if r, ok := find(res, "disks/"+d.serial+"/temp"); ok {
			t.Errorf("%s: temperature judged without a reading: %+v", d.serial, r)
		}
	}
}

// runAt runs c at the clock's current time with the drive on node sda.
func runAt(t *testing.T, c *Check, src *fakeSource, d drive) health.CheckResult {
	t.Helper()

	src.set("sda", d)

	return c.Run(t.Context())
}

func TestSelfTestCalendarAge(t *testing.T) {
	const k = "disks/" + serialSATA + "/selftest"
	src := &fakeSource{}
	clk := newClock()
	d := sata(serialSATA)
	d.log = []testRun{{hours: 4000}}
	c := newLoaded(src, disksCfg(), nil, clk.now)
	runAt(t, c, src, d)

	// The power-on hours do not move (a drive that is powered down most of
	// the time), the calendar does: 7 d is within the 8 d limit.
	clk.add(7 * 24 * time.Hour)
	if r := mustFind(t, runAt(t, c, src, d), k); r.Verdict != health.Good {
		t.Fatalf("after 7 d = %+v, want Good", r)
	}
	clk.add(25 * time.Hour)
	res := runAt(t, c, src, d)
	r := mustFind(t, res, k)
	if r.Verdict != health.Bad || !strings.HasSuffix(r.Text, "last passed self-test 8 d ago, limit 8 d") {
		t.Fatalf("after 8 d 1 h = %+v, want Bad", r)
	}
	if v, _ := sample(res, "drive."+serialSATA+".selftest_age_h"); v != 7*24+25 {
		t.Errorf("selftest_age_h = %v, want calendar hours %d", v, 7*24+25)
	}

	// A newly observed pass resets the age.
	d.hours += 1000
	d.log = []testRun{{hours: 5000}, {hours: 4000}}
	res = runAt(t, c, src, d)
	if r := mustFind(t, res, k); r.Verdict != health.Good {
		t.Errorf("after a new pass = %+v, want Good", r)
	}
	if v, _ := sample(res, "drive."+serialSATA+".selftest_age_h"); v != 0 {
		t.Errorf("selftest_age_h after a pass = %v, want 0", v)
	}
}

func TestSelfTestGrace(t *testing.T) {
	const k = "disks/" + serialSATA + "/selftest"
	src := &fakeSource{}
	clk := newClock()
	d := sata(serialSATA)
	d.hours = 20000
	d.log = []testRun{{hours: 100}} // a stale log, adopted on the first sight
	c := newLoaded(src, disksCfg(), nil, clk.now)

	res := runAt(t, c, src, d)
	if r := mustFind(t, res, k); r.Verdict != health.Good {
		t.Fatalf("first sight = %+v, want Good (grace)", r)
	}
	if ev := events(res); len(ev) != 1 || ev[0].Key != "disks/drives" {
		t.Errorf("events = %+v, want only the inventory", ev)
	}
	clk.add(8*24*time.Hour - time.Hour)
	if r := mustFind(t, runAt(t, c, src, d), k); r.Verdict != health.Good {
		t.Errorf("inside the grace = %+v, want Good", r)
	}
	clk.add(2 * time.Hour)
	if r := mustFind(t, runAt(t, c, src, d), k); r.Verdict != health.Bad {
		t.Errorf("after the grace = %+v, want Bad", r)
	}
}

func TestFailureThenPass(t *testing.T) {
	const k = "disks/" + serialSATA + "/selftest"
	const failKey = "disks/" + serialSATA + "/selftest-failure"
	src := &fakeSource{}
	clk := newClock()
	d := sata(serialSATA)
	d.log = []testRun{{hours: 4000}}
	c := newLoaded(src, disksCfg(), nil, clk.now)
	runAt(t, c, src, d)

	// A test failed and a newer one passed between two runs: the condition
	// is Good, the failure is reported once as an event.
	clk.add(time.Hour)
	d.log = []testRun{{hours: 4100}, {hours: 4050, bad: true}, {hours: 4000}}
	res := runAt(t, c, src, d)
	if r := mustFind(t, res, k); r.Verdict != health.Good {
		t.Errorf("selftest = %+v, want Good", r)
	}
	ev := events(res)
	if len(ev) != 1 || ev[0].Key != failKey ||
		ev[0].Text != "drive "+serialSATA+" (Example SATA SSD): self-test failed (Completed: read failure) at 4050 h; a newer test has passed since" {
		t.Fatalf("events = %+v", ev)
	}

	clk.add(time.Hour)
	if ev := events(runAt(t, c, src, d)); len(ev) != 0 {
		t.Errorf("failure reported again: %+v", ev)
	}

	// The newest finished test failing is the condition's, not an event.
	clk.add(time.Hour)
	d.log = slices.Insert(d.log, 0, testRun{hours: 4200, bad: true})
	res = runAt(t, c, src, d)
	if r := mustFind(t, res, k); r.Verdict != health.Bad || !strings.Contains(r.Text, "failed") {
		t.Errorf("selftest = %+v, want Bad", r)
	}
	if ev := events(res); len(ev) != 0 {
		t.Errorf("events = %+v, want none for the newest failure", ev)
	}
}

func TestUnsupportedSelfTestLog(t *testing.T) {
	for _, fixture := range []string{"sas_selftest", "selftest_unsupported"} {
		t.Run(fixture, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("testdata", fixture+".json"))
			if err != nil {
				t.Fatal(err)
			}
			src := &fakeSource{nodes: []string{"n0"}, runs: map[string]output{"n0": {stdout: data}}}
			clk := newClock()
			c := newLoaded(src, disksCfg(), nil, clk.now)
			c.Run(t.Context())
			clk.add(30 * 24 * time.Hour)
			res := c.Run(t.Context())

			for _, r := range res.Readings {
				if strings.HasSuffix(r.Key, "/selftest") || strings.HasSuffix(r.Key, "/selftest-failure") {
					t.Errorf("self-test judged on an unsupported log: %+v", r)
				}
			}
			for _, k := range gapKinds(res) {
				if strings.Contains(k, "selftest") {
					t.Errorf("checker error %s for an unsupported log", k)
				}
			}
			for _, sm := range res.Samples {
				if strings.HasSuffix(sm.Metric, ".selftest_age_h") {
					t.Errorf("self-test age sampled on an unsupported log: %+v", sm)
				}
			}
			if got := baselineJSON(t, c).Format; got != "unsupported" {
				t.Errorf("selftest_format = %q, want unsupported", got)
			}
		})
	}
}

func TestSelfTestLogMissingKeepsState(t *testing.T) {
	src := &fakeSource{}
	clk := newClock()
	d := sata(serialSATA)
	d.log = []testRun{{hours: 4000}}
	c := newLoaded(src, disksCfg(), nil, clk.now)
	runAt(t, c, src, d)
	clk.add(time.Hour)
	d.log = []testRun{{hours: 4100}, {hours: 4000}}
	runAt(t, c, src, d)
	before := c.Baselines()[0].Data

	clk.add(30 * 24 * time.Hour)
	d.drop = []string{"ata_smart_self_test_log"}
	res := runAt(t, c, src, d)

	if got := gapKinds(res); !slices.Equal(got, []string{"disks/" + serialSATA + "/selftest-log"}) {
		t.Errorf("checker errors = %v", got)
	}
	for _, r := range res.Readings {
		if strings.Contains(r.Key, "selftest") {
			t.Errorf("self-test judged without its log: %+v", r)
		}
	}
	var was, now selfTestData
	if err := json.Unmarshal(before, &was); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(c.Baselines()[0].Data, &now); err != nil {
		t.Fatal(err)
	}
	same := slices.Equal(was.Head, now.Head) && was.OKAt == now.OKAt && was.Format == now.Format
	if !same || *was.Newest != *now.Newest {
		t.Errorf("baseline changed across the gap: %+v -> %+v", was, now)
	}
	if was.OKAt == 0 || len(was.Head) != 2 {
		t.Errorf("setup did not store state: %+v", was)
	}
}

func TestUSBSkipped(t *testing.T) {
	// US1 acc. 8: a USB drive is neither read nor tracked nor missing.
	src := &fakeSource{}
	src.set("sda", sata(serialSATA))
	src.set("sdb", sata(serialNew))
	src.setPath("sdb", pathUSB)
	clk := newClock()
	c := newLoaded(src, disksCfg(), nil, clk.now)

	res := c.Run(t.Context())
	if res.Err != nil {
		t.Fatalf("Err = %v", res.Err)
	}
	if slices.Contains(src.calls, "sdb") {
		t.Errorf("the usb node was read: %v", src.calls)
	}
	if len(c.Baselines()) != 1 || c.Baselines()[0].Serial != serialSATA {
		t.Errorf("baselines = %+v, want only the sata drive", c.Baselines())
	}
	if data := string(c.Baselines()[0].Data); !strings.Contains(data, `"transport":"sata"`) {
		t.Errorf("baseline data = %s, want transport sata", data)
	}
	clk.add(15 * time.Minute)
	res = c.Run(t.Context())
	for _, r := range res.Readings {
		if strings.Contains(r.Key, serialNew) {
			t.Errorf("usb drive got a reading: %+v", r)
		}
	}
}

func TestIncludeSerials(t *testing.T) {
	src := &fakeSource{}
	src.set("sda", sata(serialSATA))
	src.set("sdb", sata(serialNew))
	src.set("sdc", sata("TEST0000000004"))
	src.setPath("sdb", pathUSB)
	src.setPath("sdc", pathUSB)
	cfg := disksCfg()
	cfg.IncludeSerials = []string{serialNew}
	clk := newClock()
	c := newLoaded(src, cfg, nil, clk.now)

	res := c.Run(t.Context())
	if res.Err != nil {
		t.Fatalf("Err = %v", res.Err)
	}
	got := make([]string, 0, 2)
	for _, b := range c.Baselines() {
		got = append(got, b.Serial)
	}
	if want := []string{serialSATA, serialNew}; !slices.Equal(got, want) {
		t.Fatalf("tracked = %v, want %v (the unlisted usb drive discarded)", got, want)
	}
	if data := string(c.Baselines()[1].Data); !strings.Contains(data, `"transport":"usb"`) {
		t.Errorf("baseline data = %s, want transport usb", data)
	}
	clk.add(15 * time.Minute)
	res = c.Run(t.Context())
	if r := mustFind(t, res, "disks/"+serialNew+"/missing"); r.Verdict != health.Good {
		t.Errorf("listed usb drive = %+v", r)
	}
	if _, ok := find(res, "disks/TEST0000000004/missing"); ok {
		t.Error("unlisted usb drive judged")
	}
}

func TestEmptyDiscovery(t *testing.T) {
	// US1 acc. 9: nothing found while drives are known suspends the missing check.
	src := &fakeSource{}
	src.set("sda", sata(serialSATA))
	src.set("nvme0n1", nvme(serialNVMe))
	clk := newClock()
	c := fixtureCheck(t, src, clk, nil)

	src.remove("sda")
	src.remove("nvme0n1")
	res := c.Run(t.Context())
	var ce health.CheckError
	if !errors.As(res.Err, &ce) || ce.Subject != "discovery" || ce.Kind != "empty" {
		t.Fatalf("Err = %#v, want the discovery/empty checker error", res.Err)
	}
	if want := "no drives found while 2 are known; missing-drive check suspended"; ce.Err == nil ||
		ce.Err.Error() != want {
		t.Errorf("text = %v, want %q", ce.Err, want)
	}
	if len(res.Readings) != 0 {
		t.Errorf("readings = %+v, want none", res.Readings)
	}

	// Retired and excluded drives are not counted.
	c.Retire(serialSATA)
	res = c.Run(t.Context())
	if !errors.As(res.Err, &ce) || !strings.Contains(ce.Err.Error(), "while 1 are known") {
		t.Errorf("Err = %v, want 1 known", res.Err)
	}
	c.Retire(serialNVMe)
	if res = c.Run(t.Context()); res.Err != nil {
		t.Errorf("no active drive known: Err = %v", res.Err)
	}
}

func TestUpgradeUSBNotMissing(t *testing.T) {
	obs, logs := observer.New(zapcore.WarnLevel)
	base := func(data string) store.Baseline {
		return store.Baseline{
			Serial: serialNew,
			Model:  "Example SSD",
			Data:   json.RawMessage(data),
			FirstSeenAt: time.Date(
				2026,
				9,
				1,
				0,
				0,
				0,
				0,
				time.UTC,
			),
			LastSeenAt: time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC),
		}
	}
	tests := []struct {
		name    string
		data    string
		include []string
		retired bool
	}{
		{"stored transport", `{"node":"sdb","transport":"usb"}`, nil, true},
		{"stored other", `{"node":"sdb","transport":"other"}`, nil, true},
		{"001 baseline, node discovered on usb", `{"node":"sdb"}`, nil, true},
		{"listed in include_serials", `{"node":"sdb","transport":"usb"}`, []string{serialNew}, false},
		{"stored sata", `{"node":"sdb","transport":"sata"}`, nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := &fakeSource{}
			src.set("sda", sata(serialSATA))
			src.set("sdb", sata(serialNew))
			src.setPath("sdb", pathUSB)
			cfg := disksCfg()
			cfg.IncludeSerials = tt.include
			c := New(src, cfg, nil, newClock().now, WithLogger(zap.New(obs)))
			c.LoadBaselines([]store.Baseline{base(tt.data)})
			logs.TakeAll()

			res := c.Run(t.Context())
			retired := slices.ContainsFunc(c.Baselines(), func(b store.Baseline) bool {
				return b.Serial == serialNew && b.Retired
			})
			if retired != tt.retired {
				t.Fatalf("retired = %v, want %v", retired, tt.retired)
			}
			if r, ok := find(res, "disks/"+serialNew+"/missing"); retired && ok {
				t.Errorf("retired drive judged missing: %+v", r)
			}
			lines := logs.TakeAll()
			if !retired {
				if len(lines) != 0 {
					t.Errorf("logged %v", lines)
				}

				return
			}
			if len(lines) != 1 || !strings.HasPrefix(lines[0].Message, "drive "+serialNew+" on a ") ||
				!strings.HasSuffix(
					lines[0].Message,
					" transport is no longer tracked; list it in include_serials to keep watching it",
				) {
				t.Errorf("log = %+v, want the one retire line", lines)
			}
		})
	}
}

func TestUpgradeRetireWithoutNodeAndNoMissing(t *testing.T) {
	// A stored usb drive that is gone entirely never opens "missing".
	src := &fakeSource{}
	src.set("sda", sata(serialSATA))
	c := New(src, disksCfg(), nil, newClock().now)
	c.LoadBaselines([]store.Baseline{{
		Serial: serialNew, Data: json.RawMessage(`{"node":"sdb","transport":"usb"}`),
	}})
	res := c.Run(t.Context())
	if r, ok := find(res, "disks/"+serialNew+"/missing"); ok {
		t.Errorf("missing = %+v", r)
	}
}

func TestMovedToUSB(t *testing.T) {
	// A SATA-baselined drive vanished and an unrelated USB disk took its node
	// name: the drive is missing, never hidden behind the USB node.
	const otherSerial = "TEST0000000009"
	tests := []struct {
		name string
		out  output
	}{
		{"awake", output{stdout: sata(serialNew).json()}},
		{"asleep", output{status: StandbyStatus}},
		{"unreadable", output{err: errors.New("device not ready")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := &fakeSource{}
			src.set("sda", sata(serialSATA))
			src.set("sdb", sata(serialNVMe))
			cfg := disksCfg()
			cfg.IncludeSerials = []string{otherSerial}
			clk := newClock()
			c := newLoaded(src, cfg, nil, clk.now)
			if res := c.Run(t.Context()); res.Err != nil {
				t.Fatal(res.Err)
			}
			clk.add(15 * time.Minute)

			src.setPath("sdb", pathUSB)
			src.runs["sdb"] = tt.out
			res := c.Run(t.Context())
			if r := mustFind(t, res, "disks/"+serialNVMe+"/missing"); r.Verdict != health.Bad {
				t.Errorf("missing = %+v, want Bad", r)
			}
			if res.Err != nil && strings.Contains(res.Err.Error(), "drive "+serialNVMe) {
				t.Errorf("Err = %v, the usb node stands in for the sata drive", res.Err)
			}
		})
	}
}

func TestSleepingUSBNodeDoesNotSuspendMissing(t *testing.T) {
	src := &fakeSource{}
	src.set("sda", sata(serialSATA))
	src.set("sdb", sata(serialNVMe))
	cfg := disksCfg()
	cfg.IncludeSerials = []string{"TEST0000000009"}
	clk := newClock()
	c := newLoaded(src, cfg, nil, clk.now)
	if res := c.Run(t.Context()); res.Err != nil {
		t.Fatal(res.Err)
	}
	clk.add(15 * time.Minute)

	src.remove("sdb")
	src.set("sdc", sata(serialNew))
	src.setPath("sdc", pathUSB)
	src.runs["sdc"] = output{status: StandbyStatus}
	res := c.Run(t.Context())
	if r := mustFind(t, res, "disks/"+serialNVMe+"/missing"); r.Verdict != health.Bad {
		t.Errorf("missing = %+v, want Bad despite the sleeping usb node", r)
	}
	if len(src.wakes) != 0 {
		t.Errorf("forced reads = %v, an unidentified external node is never woken", src.wakes)
	}
}
