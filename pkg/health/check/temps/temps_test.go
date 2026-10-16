package temps_test

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Toshik1978/tgshd/pkg/health"
	"github.com/Toshik1978/tgshd/pkg/health/check/temps"
	"github.com/Toshik1978/tgshd/pkg/health/config"
)

type fakeHwmon struct {
	sensors []temps.Sensor
	err     error
}

func (f *fakeHwmon) Sensors() ([]temps.Sensor, error) { return f.sensors, f.err }

type fakeGPU struct {
	gpus        []temps.GPU
	err         error
	hasDeadline bool
}

func (f *fakeGPU) GPUs(ctx context.Context) ([]temps.GPU, error) {
	_, f.hasDeadline = ctx.Deadline()
	return f.gpus, f.err
}

// fakeOpen is a fake temps.OpenState: the keys whose condition is open. A nil
// one answers that no condition is open.
type fakeOpen map[string]bool

func (f fakeOpen) IsOpen(key string) bool { return f[key] }

// cfg lists every acpitz input as a board sensor at the board limit.
func cfg() config.Temperatures {
	return config.Temperatures{
		Limits:       config.Limits{CPU: 85, GPU: 80, Board: 70},
		BoardSensors: []config.BoardSensor{{Chip: "acpitz", Label: "*", Limit: new(70)}},
	}
}

func find(res health.CheckResult, key string) []health.Reading {
	var out []health.Reading
	for _, r := range res.Readings {
		if r.Key == key {
			out = append(out, r)
		}
	}

	return out
}

func TestClassesAndVerdicts(t *testing.T) {
	hw := &fakeHwmon{sensors: []temps.Sensor{
		{Chip: "coretemp", Label: "Package id 0", Celsius: 90},
		{Chip: "k10temp", Label: "Tctl", Celsius: 50},
		{Chip: "acpitz", Label: "temp1", Celsius: 71},
		{Chip: "nvme", Label: "Composite", Celsius: 99},
		{Chip: "drivetemp", Label: "temp1", Celsius: 99},
	}}
	gpu := &fakeGPU{gpus: []temps.GPU{{Index: 0, Name: "Example GPU", Celsius: new(60.0)}}}
	res := temps.New(hw, gpu, cfg(), fakeOpen(nil)).Run(context.Background())
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if !gpu.hasDeadline {
		t.Error("GPUs ctx has no deadline")
	}
	want := map[string]health.Verdict{
		"temps/coretemp/package-id-0": health.Bad,
		"temps/k10temp/tctl":          health.Good,
		"temps/acpitz/temp1":          health.Bad,
		"temps/gpu/0":                 health.Good,
	}
	if len(res.Readings) != len(want) {
		t.Fatalf("readings = %+v", res.Readings)
	}
	for k, v := range want {
		r := find(res, k)
		if len(r) != 1 || r[0].Verdict != v || r[0].Kind != health.State {
			t.Errorf("%s = %+v, want %v", k, r, v)
		}
	}
	samples := map[string]float64{}
	for _, s := range res.Samples {
		samples[s.Metric] = s.Value
	}
	for m, v := range map[string]float64{
		"temp.cpu.coretemp.package-id-0": 90,
		"temp.cpu.k10temp.tctl":          50,
		"temp.board.acpitz.temp1":        71,
		"temp.gpu.0":                     60,
	} {
		if samples[m] != v {
			t.Errorf("sample %s = %v, want %v", m, samples[m], v)
		}
	}
}

func TestZeroLimitMeansNoLimit(t *testing.T) {
	hw := &fakeHwmon{sensors: []temps.Sensor{{Chip: "acpitz", Label: "temp1", Celsius: 120}}}
	zero := config.Temperatures{BoardSensors: []config.BoardSensor{{Chip: "acpitz", Limit: new(0)}}}
	res := temps.New(hw, nil, zero, fakeOpen(nil)).Run(context.Background())
	if r := find(res, "temps/acpitz/temp1"); len(r) != 1 || r[0].Verdict != health.Good {
		t.Errorf("got %+v", r)
	}
}

func TestGoneReportedOnce(t *testing.T) {
	hw := &fakeHwmon{sensors: []temps.Sensor{
		{Chip: "acpitz", Label: "temp1", Device: "/sys/devices/virtual/thermal/thermal_zone0", Celsius: 80},
		{Chip: "nct6775", Label: "SYSTIN", Celsius: 40, Crit: new(80.0)},
	}}
	c := temps.New(hw, nil, cfg(), fakeOpen{"temps/acpitz/temp1": true})
	c.Run(context.Background())
	hw.sensors = hw.sensors[1:]
	res := c.Run(context.Background())
	var ev, good int
	for _, r := range find(res, "temps/acpitz/temp1") {
		switch {
		case r.Kind == health.Event:
			ev++
			if r.Text != "acpitz temp1 gone" {
				t.Errorf("text = %q", r.Text)
			}
		case r.Verdict == health.Good:
			good++
		}
	}
	if ev != 1 || good != 0 || !slices.Equal(res.Forget, []string{"temps/acpitz/temp1"}) {
		t.Fatalf("event=%d good=%d in %+v", ev, good, res.Readings)
	}
	if r := c.Run(context.Background()); len(find(r, "temps/acpitz/temp1")) != 0 {
		t.Errorf("gone reported twice: %+v", r.Readings)
	}
}

func TestAdapterErrorEmitsNothingNotGone(t *testing.T) {
	hw := &fakeHwmon{sensors: []temps.Sensor{{Chip: "acpitz", Label: "temp1", Celsius: 40}}}
	gpu := &fakeGPU{gpus: []temps.GPU{{Index: 0, Celsius: new(40.0)}}}
	c := temps.New(hw, gpu, cfg(), fakeOpen(nil))
	c.Run(context.Background())

	hw.err = errors.New("boom")
	hw.sensors = nil
	res := c.Run(context.Background())
	if res.Err == nil {
		t.Fatal("want Err for a failing source")
	}
	if len(find(res, "temps/gpu/0")) != 1 {
		t.Errorf("healthy source must still be judged: %+v", res.Readings)
	}
	for _, r := range res.Readings {
		if r.Kind == health.Event {
			t.Errorf("unexpected event %+v", r)
		}
	}
	if r := find(res, "temps/acpitz/temp1"); len(r) != 0 {
		t.Errorf("failed source must emit no readings: %+v", r)
	}
	for _, r := range res.Readings {
		if r.Verdict == health.Unknown {
			t.Errorf("unexpected unknown reading %+v", r)
		}
	}

	hw.err = nil
	hw.sensors = []temps.Sensor{{Chip: "acpitz", Label: "temp1", Celsius: 40}}
	for _, r := range c.Run(context.Background()).Readings {
		if r.Kind == health.Event {
			t.Errorf("unexpected event %+v", r)
		}
	}

	hw.err = errors.New("boom")
	gpu.err = errors.New("boom")
	if res = c.Run(context.Background()); res.Err == nil {
		t.Error("want Err when every source fails")
	}

	// A source failing since start is not silent.
	c = temps.New(&fakeHwmon{err: errors.New("boom")}, &fakeGPU{}, cfg(), fakeOpen(nil))
	if res = c.Run(context.Background()); res.Err == nil {
		t.Error("want Err for a source failing since start")
	}
}

func TestGPUGone(t *testing.T) {
	gpu := &fakeGPU{gpus: []temps.GPU{{Index: 1, Name: "Example GPU", Celsius: new(90.0)}}}
	c := temps.New(nil, gpu, cfg(), fakeOpen{"temps/gpu/1": true})
	if r := find(c.Run(context.Background()), "temps/gpu/1"); len(r) != 1 || r[0].Verdict != health.Bad {
		t.Fatalf("got %+v", r)
	}
	gpu.gpus = nil
	if r := find(c.Run(context.Background()), "temps/gpu/1"); len(r) != 1 || r[0].Kind != health.Event {
		t.Fatalf("got %+v", r)
	}
}

func TestDuplicateKeysAreDisambiguated(t *testing.T) {
	hw := &fakeHwmon{sensors: []temps.Sensor{
		{Chip: "coretemp", Label: "Package id 0", Celsius: 40},
		{Chip: "coretemp", Label: "Package id 0", Celsius: 41},
	}}
	res := temps.New(hw, nil, cfg(), fakeOpen(nil)).Run(context.Background())
	if len(res.Readings) != 2 || res.Readings[0].Key == res.Readings[1].Key {
		t.Errorf("keys collide: %+v", res.Readings)
	}
}

func TestBadSensorDisappears(t *testing.T) {
	hw := &fakeHwmon{sensors: []temps.Sensor{{Chip: "acpitz", Label: "temp1", Celsius: 99}}}
	c := temps.New(hw, nil, cfg(), fakeOpen{"temps/acpitz/temp1": true})
	c.Run(context.Background())
	hw.sensors = nil
	res := c.Run(context.Background())
	var events int
	for _, r := range res.Readings {
		if r.Kind != health.Event {
			t.Errorf("unexpected state reading %+v, want only the gone event", r)

			continue
		}
		events++
		if r.Text != "acpitz temp1 gone" {
			t.Errorf("event text = %q", r.Text)
		}
	}
	if events != 1 {
		t.Errorf("events = %d, want 1", events)
	}
	if len(res.Forget) != 1 || res.Forget[0] != "temps/acpitz/temp1" {
		t.Errorf("Forget = %q, want the sensor key", res.Forget)
	}
}

func TestExportRestore(t *testing.T) {
	hw := &fakeHwmon{sensors: []temps.Sensor{{Chip: "acpitz", Label: "temp1", Celsius: 40}}}
	gpu := &fakeGPU{gpus: []temps.GPU{{Index: 0, Celsius: new(40.0)}}}
	c := temps.New(hw, gpu, cfg(), fakeOpen(nil))
	c.Run(context.Background())
	data, err := c.Export()
	if err != nil {
		t.Fatal(err)
	}

	hw2 := &fakeHwmon{}
	gpu2 := &fakeGPU{}
	c2 := temps.New(hw2, gpu2, cfg(), fakeOpen{"temps/acpitz/temp1": true, "temps/gpu/0": true})
	if err := c2.Restore(data); err != nil {
		t.Fatal(err)
	}
	res := c2.Run(context.Background())
	for _, k := range []string{"temps/acpitz/temp1", "temps/gpu/0"} {
		if r := find(res, k); len(r) != 1 || r[0].Kind != health.Event {
			t.Errorf("%s = %+v, want one gone event", k, r)
		}
	}
	if err := c2.Restore(""); err != nil {
		t.Error(err)
	}
	if err := c2.Restore("{not json"); err == nil {
		t.Error("want error for bad data")
	}
	if err := c2.Restore("{}"); err != nil {
		t.Error(err)
	}
}

func TestLimitBoundaryAndLabelFallback(t *testing.T) {
	hw := &fakeHwmon{sensors: []temps.Sensor{
		{Chip: "coretemp", Label: "Package id 0", Celsius: 84.9},
		{Chip: "Weird Chip/1", Label: "", Celsius: 10, Crit: new(100.0)},
	}}
	res := temps.New(hw, nil, cfg(), fakeOpen(nil)).Run(context.Background())
	if r := find(res, "temps/coretemp/package-id-0"); len(r) != 1 || r[0].Verdict != health.Good {
		t.Errorf("below the limit = %+v, want Good", r)
	}
	if r := find(res, "temps/weird-chip-1/temp"); len(r) != 1 {
		t.Errorf("fallback key missing: %+v", res.Readings)
	}
}

func TestNoGPUsSentinelAndNilTemperature(t *testing.T) {
	gpu := &fakeGPU{
		gpus: []temps.GPU{{Index: 0, Name: "Example GPU", Celsius: new(60.0)}, {Index: 1, Name: "Example GPU"}},
	}
	c := temps.New(nil, gpu, cfg(), fakeOpen(nil))
	res := c.Run(context.Background())
	if res.Err != nil || len(find(res, "temps/gpu/0")) != 1 {
		t.Fatalf("got %+v", res)
	}
	if r := find(res, "temps/gpu/1"); len(r) != 1 || r[0].Verdict != health.Unknown {
		t.Fatalf("nil temperature = %+v, want Unknown", r)
	}
	// A seen GPU missing from a no-devices answer has stopped reporting: it is
	// unreadable, not gone.
	gpu.gpus, gpu.err = nil, temps.ErrNoGPUs
	res = c.Run(context.Background())
	if res.Err != nil || len(res.Forget) != 0 {
		t.Fatalf("err = %v, forget = %q", res.Err, res.Forget)
	}
	for _, key := range []string{"temps/gpu/0", "temps/gpu/1"} {
		if r := find(res, key); len(r) != 1 || r[0].Verdict != health.Unknown || r[0].Kind != health.State {
			t.Fatalf("%s = %+v, want Unknown", key, r)
		}
	}
}

// cpuHost is a synthetic two-socket AMD host plus a one-package Intel host:
// per-core, Tctl and Tccd inputs next to the package sensors.
func cpuHost(hotCore float64) []temps.Sensor {
	const amd0, amd1 = "/sys/devices/pci0000:00/0000:00:18.3", "/sys/devices/pci0000:00/0000:00:19.3"
	out := make([]temps.Sensor, 0, 25)
	out = append(out,
		// Second package first: instances are ordered by device path.
		temps.Sensor{Chip: "k10temp", Label: "Tctl", Device: amd1, Celsius: 72},
		temps.Sensor{Chip: "k10temp", Label: "Tdie", Device: amd1, Celsius: 62},
		temps.Sensor{Chip: "k10temp", Label: "Tccd1", Device: amd1, Celsius: 99},
		temps.Sensor{Chip: "k10temp", Label: "Tctl", Device: amd0, Celsius: 70},
		temps.Sensor{Chip: "k10temp", Label: "Tdie", Device: amd0, Celsius: 60},
		temps.Sensor{Chip: "k10temp", Label: "Tccd1", Device: amd0, Celsius: 99},
		temps.Sensor{Chip: "zenpower", Label: "Tctl", Device: "/sys/devices/pci0000:00/0000:00:1a.3", Celsius: 55},
		temps.Sensor{Chip: "zenpower", Label: "Tccd1", Device: "/sys/devices/pci0000:00/0000:00:1a.3", Celsius: 99},
		temps.Sensor{Chip: "coretemp", Label: "Package id 0", Device: "/sys/devices/platform/coretemp.0", Celsius: 50},
	)
	for i := range 16 {
		c := 48.0
		if i == 3 {
			c = hotCore
		}
		out = append(out, temps.Sensor{
			Chip: "coretemp", Label: "Core " + strconv.Itoa(i), Device: "/sys/devices/platform/coretemp.0", Celsius: c,
		})
	}

	return out
}

func TestPackageOnly(t *testing.T) {
	res := temps.New(&fakeHwmon{sensors: cpuHost(99)}, nil, cfg(), fakeOpen(nil)).Run(context.Background())
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	want := map[string]float64{
		"temps/coretemp/package-id-0": 50,
		"temps/k10temp/tdie":          60,
		"temps/k10temp/tdie-2":        62,
		"temps/zenpower/tctl":         55,
	}
	if len(res.Readings) != len(want) {
		t.Fatalf("readings = %+v, want one per package", res.Readings)
	}
	for k, c := range want {
		r := find(res, k)
		if len(r) != 1 || r[0].Verdict != health.Good || r[0].Kind != health.State {
			t.Errorf("%s = %+v, want one Good state", k, r)
		}
		if len(r) == 1 && !strings.Contains(r[0].Text, strconv.FormatFloat(c, 'f', 1, 64)) {
			t.Errorf("%s text = %q, want %v", k, r[0].Text, c)
		}
	}
	metrics := make([]string, 0, len(res.Samples))
	for _, s := range res.Samples {
		metrics = append(metrics, s.Metric)
	}
	slices.Sort(metrics)
	wantMetrics := []string{
		"temp.cpu.coretemp.package-id-0", "temp.cpu.k10temp.tdie", "temp.cpu.k10temp.tdie-2", "temp.cpu.zenpower.tctl",
	}
	if !slices.Equal(metrics, wantMetrics) {
		t.Errorf("samples = %q, want %q", metrics, wantMetrics)
	}
}

func TestFormerCoreSensorForgottenSilently(t *testing.T) {
	hw := &fakeHwmon{sensors: cpuHost(48)}
	c := temps.New(hw, nil, cfg(), fakeOpen(nil))
	// The seen set as 001 stored it, with every core, Tctl and Tccd judged.
	seen001 := `{"hwmon":{` +
		`"temps/coretemp/package-id-0":"coretemp Package id 0",` +
		`"temps/coretemp/core-3":"coretemp Core 3",` +
		`"temps/k10temp/tctl":"k10temp Tctl",` +
		`"temps/k10temp/tccd1-2":"k10temp Tccd1 #2"}}`
	if err := c.Restore(seen001); err != nil {
		t.Fatal(err)
	}
	res := c.Run(context.Background())
	for _, r := range res.Readings {
		if r.Kind == health.Event {
			t.Errorf("unexpected event %+v", r)
		}
	}
	forget := slices.Sorted(slices.Values(res.Forget))
	want := []string{"temps/coretemp/core-3", "temps/k10temp/tccd1-2", "temps/k10temp/tctl"}
	if !slices.Equal(forget, want) {
		t.Errorf("Forget = %q, want %q", forget, want)
	}
	if res = c.Run(context.Background()); len(res.Forget) != 0 {
		t.Errorf("second Forget = %q, want none", res.Forget)
	}
}

func TestNoCPUSensor(t *testing.T) {
	cpuNone := func(err error) bool {
		for _, ce := range health.CheckErrors(err) {
			if ce.Key("temps") == "temps/cpu/none" {
				return true
			}
		}

		return false
	}
	pkg := temps.Sensor{Chip: "coretemp", Label: "Package id 0", Celsius: 40}
	board := temps.Sensor{Chip: "acpitz", Label: "temp1", Celsius: 40}
	drive := temps.Sensor{Chip: "nvme", Label: "Composite", Celsius: 40}
	core := temps.Sensor{Chip: "coretemp", Label: "Core 0", Celsius: 40}
	tccd := temps.Sensor{Chip: "k10temp", Label: "Tccd1", Celsius: 40}
	unreadablePkg := temps.Sensor{Chip: "k10temp", Label: "Tctl", Err: errors.New("input/output error")}
	for _, tc := range []struct {
		name    string
		sensors []temps.Sensor
		readErr error
		want    bool
	}{
		{"empty read", nil, nil, true},
		{"board and drive only", []temps.Sensor{board, drive}, nil, true},
		{"cores without a package", []temps.Sensor{core, tccd}, nil, true},
		{"package present", []temps.Sensor{board, pkg}, nil, false},
		{"package present but unreadable", []temps.Sensor{board, unreadablePkg}, nil, false},
		{"failed read", nil, errors.New("boom"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := temps.New(&fakeHwmon{sensors: tc.sensors, err: tc.readErr}, nil, cfg(), fakeOpen(nil)).
				Run(context.Background())
			if got := cpuNone(res.Err); got != tc.want {
				t.Errorf("temps/cpu/none = %v, want %v (err %v)", got, tc.want, res.Err)
			}
		})
	}
	// Without an hwmon source (GPUs only) there is nothing to miss.
	gpu := &fakeGPU{gpus: []temps.GPU{{Index: 0, Celsius: new(40.0)}}}
	if res := temps.New(nil, gpu, cfg(), fakeOpen(nil)).Run(context.Background()); res.Err != nil {
		t.Errorf("GPU-only check: %v", res.Err)
	}
}

// boardHost has one CPU package (so no temps/cpu/none) and board sensors.
func boardHost(board ...temps.Sensor) *fakeHwmon {
	return &fakeHwmon{sensors: append([]temps.Sensor{{Chip: "coretemp", Label: "Package id 0", Celsius: 40}}, board...)}
}

func sampled(res health.CheckResult, metric string) (float64, bool) {
	for _, s := range res.Samples {
		if s.Metric == metric {
			return s.Value, true
		}
	}

	return 0, false
}

func TestUnlistedBoardNoCrit(t *testing.T) {
	hw := boardHost(
		temps.Sensor{Chip: "nct6775", Label: "AUXTIN0", Celsius: 40},
		temps.Sensor{Chip: "nct6775", Label: "AUXTIN1", Celsius: -128},
	)
	c := temps.New(hw, nil, cfg(), fakeOpen(nil))
	// 001 judged every board sensor, so the stored seen set still has one.
	if err := c.Restore(`{"hwmon":{"temps/nct6775/auxtin0":"nct6775 AUXTIN0"}}`); err != nil {
		t.Fatal(err)
	}
	// A simulated week of hourly reads, with the unlisted sensor reading high.
	for hour := range 7 * 24 {
		hw.sensors[1].Celsius = 40 + float64(hour%100)
		res := c.Run(context.Background())
		if res.Err != nil {
			t.Fatalf("hour %d: %v", hour, res.Err)
		}
		for _, r := range res.Readings {
			if r.Kind == health.Event || r.Verdict != health.Good {
				t.Fatalf("hour %d: unexpected reading %+v", hour, r)
			}
			if strings.HasPrefix(r.Key, "temps/nct6775/") {
				t.Fatalf("hour %d: unlisted board sensor judged: %+v", hour, r)
			}
		}
		wantForget := []string(nil)
		if hour == 0 {
			wantForget = []string{"temps/nct6775/auxtin0"}
		}
		if !slices.Equal(res.Forget, wantForget) {
			t.Fatalf("hour %d: Forget = %q, want %q", hour, res.Forget, wantForget)
		}
		if v, ok := sampled(res, "temp.board.nct6775.auxtin0"); !ok || v != hw.sensors[1].Celsius {
			t.Fatalf("hour %d: sample = %v %v, want %v", hour, v, ok, hw.sensors[1].Celsius)
		}
		if _, ok := sampled(res, "temp.board.nct6775.auxtin1"); ok {
			t.Fatalf("hour %d: implausible unlisted value sampled", hour)
		}
	}
}

func TestBoardCrit(t *testing.T) {
	conf := cfg()
	conf.BoardSensors = append(conf.BoardSensors,
		config.BoardSensor{Chip: "nct*", Label: "CPUTIN", Limit: new(60)},
		config.BoardSensor{Chip: "nct*", Label: "CPUTIN", Limit: new(99)}, // shadowed: the first match wins
		config.BoardSensor{Chip: "it87*", Label: ""},                      // no limit: limits.board
	)
	hw := boardHost(
		temps.Sensor{Chip: "nct6775", Label: "SYSTIN", Celsius: 79.9, Crit: new(80.0)},
		temps.Sensor{Chip: "nct6775", Label: "PECI Agent 0", Celsius: 80, Crit: new(80.0)},
		temps.Sensor{Chip: "nct6775", Label: "CPUTIN", Celsius: 60, Crit: new(100.0)},
		temps.Sensor{Chip: "it8792", Label: "temp3", Celsius: 70},
		temps.Sensor{Chip: "acpitz", Label: "temp1", Celsius: 69.9},
	)
	res := temps.New(hw, nil, conf, fakeOpen(nil)).Run(context.Background())
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	for key, want := range map[string]struct {
		v    health.Verdict
		text string
	}{
		"temps/nct6775/systin":       {health.Good, "nct6775 SYSTIN at 79.9°C"},
		"temps/nct6775/peci-agent-0": {health.Bad, "nct6775 PECI Agent 0 at 80.0°C, at or above the 80°C limit"},
		"temps/nct6775/cputin":       {health.Bad, "nct6775 CPUTIN at 60.0°C, at or above the 60°C limit"},
		"temps/it8792/temp3":         {health.Bad, "it8792 temp3 at 70.0°C, at or above the 70°C limit"},
		"temps/acpitz/temp1":         {health.Good, "acpitz temp1 at 69.9°C"},
	} {
		r := find(res, key)
		if len(r) != 1 || r[0].Verdict != want.v || r[0].Text != want.text || r[0].Kind != health.State {
			t.Errorf("%s = %+v, want %v %q", key, r, want.v, want.text)
		}
	}
	if v, ok := sampled(res, "temp.board.nct6775.peci-agent-0"); !ok || v != 80 {
		t.Errorf("sample = %v %v", v, ok)
	}
}

func TestImplausible(t *testing.T) {
	hw := &fakeHwmon{sensors: []temps.Sensor{
		{Chip: "coretemp", Label: "Package id 0", Celsius: 40},
		{Chip: "acpitz", Label: "temp1", Celsius: 151},
		{Chip: "acpitz", Label: "temp2", Celsius: -40.5},
		{Chip: "acpitz", Label: "temp3", Celsius: 150},
		{Chip: "acpitz", Label: "temp4", Celsius: -40},
		{Chip: "nct6775", Label: "SYSTIN", Celsius: 127.5, Crit: new(100.0)},
	}}
	gpu := &fakeGPU{gpus: []temps.GPU{{Index: 0, Celsius: new(255.0)}}}
	c := temps.New(hw, gpu, cfg(), fakeOpen(nil))
	res := c.Run(context.Background())
	for key, want := range map[string]health.Verdict{
		"temps/acpitz/temp1":   health.Unknown,
		"temps/acpitz/temp2":   health.Unknown,
		"temps/acpitz/temp3":   health.Bad,
		"temps/acpitz/temp4":   health.Good,
		"temps/nct6775/systin": health.Bad,
		"temps/gpu/0":          health.Unknown,
	} {
		r := find(res, key)
		if len(r) != 1 || r[0].Verdict != want || r[0].Kind != health.State {
			t.Errorf("%s = %+v, want %v", key, r, want)
		}
	}
	const implausible = "acpitz temp1 reads 151.0°C, outside the plausible -40…150°C range"
	if r := find(res, "temps/acpitz/temp1"); len(r) == 1 && r[0].Text != implausible {
		t.Errorf("text = %q", r[0].Text)
	}
	for _, m := range []string{"temp.board.acpitz.temp1", "temp.board.acpitz.temp2", "temp.gpu.0"} {
		if _, ok := sampled(res, m); ok {
			t.Errorf("implausible %s sampled", m)
		}
	}

	// A sensor that turns implausible while hot keeps its state: it is
	// neither good nor gone.
	hw.sensors[3].Celsius = 300
	res = c.Run(context.Background())
	if r := find(res, "temps/acpitz/temp3"); len(r) != 1 || r[0].Verdict != health.Unknown {
		t.Errorf("temp3 = %+v, want one Unknown", r)
	}
	if len(res.Forget) != 0 {
		t.Errorf("Forget = %q", res.Forget)
	}
}

func TestGPUAtLimit(t *testing.T) {
	gpu := &fakeGPU{gpus: []temps.GPU{
		{Index: 0, Celsius: new(80.0)},
		{Index: 1, Celsius: new(79.9)},
	}}
	hw := &fakeHwmon{sensors: []temps.Sensor{{Chip: "k10temp", Label: "Tctl", Celsius: 85}}}
	res := temps.New(hw, gpu, cfg(), fakeOpen(nil)).Run(context.Background())
	for key, want := range map[string]health.Verdict{
		"temps/gpu/0":        health.Bad,
		"temps/gpu/1":        health.Good,
		"temps/k10temp/tctl": health.Bad,
	} {
		if r := find(res, key); len(r) != 1 || r[0].Verdict != want {
			t.Errorf("%s = %+v, want %v", key, r, want)
		}
	}
	if r := find(res, "temps/gpu/0"); len(r) == 1 && r[0].Text != "gpu 0 at 80.0°C, at or above the 80°C limit" {
		t.Errorf("text = %q", r[0].Text)
	}
}

// engineOpen answers temps.OpenState from an engine, as the runner does.
type engineOpen struct{ e *health.Engine }

func (o engineOpen) IsOpen(key string) bool {
	c, ok := o.e.Condition(key)

	return ok && c.Open
}

// feed applies one run to the engine as the runner does: readings first, then
// Forget. It returns the alerts and the keys the runner would raise as
// unreadable checker errors.
func feed(e *health.Engine, now time.Time, res health.CheckResult) (alerts []health.Alert, unreadable []string) {
	for _, r := range res.Readings {
		if r.Kind == health.State && r.Verdict == health.Unknown {
			unreadable = append(unreadable, r.Key+"/unreadable")
		}
	}
	alerts = e.Apply(now, "temps", time.Minute, 0, res.Readings)
	for _, k := range res.Forget {
		e.Clear(k)
	}

	return alerts, unreadable
}

func t0() time.Time { return time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC) }

func TestUnreadableKeepsProblem(t *testing.T) {
	const amd0, amd1 = "/sys/devices/pci0000:00/0000:00:18.3", "/sys/devices/pci0000:00/0000:00:19.3"
	hw := &fakeHwmon{sensors: []temps.Sensor{
		{Chip: "k10temp", Label: "Tdie", Device: amd0, Celsius: 60},
		{Chip: "k10temp", Label: "Tdie", Device: amd1, Celsius: 95},
	}}
	e := health.NewEngine()
	open := engineOpen{e}
	now := t0()
	c := temps.New(hw, nil, cfg(), open, temps.WithClock(func() time.Time { return now }))
	alerts, _ := feed(e, now, c.Run(context.Background()))
	if len(alerts) != 1 || !open.IsOpen("temps/k10temp/tdie-2") {
		t.Fatalf("alerts = %+v, want the second package open", alerts)
	}

	// The first package stops reading: its key keeps its slot, so the second
	// package is still judged under tdie-2.
	ioErr := errors.New("input/output error")
	hw.sensors[0].Err = ioErr
	now = now.Add(time.Minute)
	res := c.Run(context.Background())
	if res.Err != nil || len(res.Forget) != 0 {
		t.Fatalf("err = %v, forget = %q", res.Err, res.Forget)
	}
	r := find(res, "temps/k10temp/tdie")
	if len(r) != 1 || r[0].Verdict != health.Unknown || r[0].Kind != health.State ||
		r[0].Text != "k10temp Tdie cannot be read: input/output error" {
		t.Fatalf("tdie = %+v, want one Unknown state", r)
	}
	if r := find(res, "temps/k10temp/tdie-2"); len(r) != 1 || r[0].Verdict != health.Bad {
		t.Fatalf("tdie-2 = %+v, want the second package still Bad", r)
	}

	// Now the hot package stops reading too: its open problem is kept.
	hw.sensors[1].Err = ioErr
	since := now
	now = now.Add(time.Minute)
	res = c.Run(context.Background())
	alerts, unreadable := feed(e, now, res)
	if len(alerts) != 0 || len(res.Forget) != 0 {
		t.Fatalf("alerts = %+v, forget = %q, want none", alerts, res.Forget)
	}
	slices.Sort(unreadable)
	wantUnreadable := []string{"temps/k10temp/tdie-2/unreadable", "temps/k10temp/tdie/unreadable"}
	if !slices.Equal(unreadable, wantUnreadable) {
		t.Fatalf("unreadable = %q, want %q", unreadable, wantUnreadable)
	}
	if !open.IsOpen("temps/k10temp/tdie-2") {
		t.Fatal("a failed read resolved the open problem")
	}
	wantSince := []temps.UnreadableSensor{
		{Key: "temps/k10temp/tdie", Name: "k10temp Tdie", Since: since},
		{Key: "temps/k10temp/tdie-2", Name: "k10temp Tdie #2", Since: now},
	}
	if got := c.Unreadable(); !slices.Equal(got, wantSince) {
		t.Fatalf("Unreadable = %+v, want %+v", got, wantSince)
	}

	// The unreadable state survives a restart.
	data, err := c.Export()
	if err != nil {
		t.Fatal(err)
	}
	c2 := temps.New(hw, nil, cfg(), open)
	if err := c2.Restore(data); err != nil {
		t.Fatal(err)
	}
	if got := c2.Unreadable(); !slices.Equal(got, wantSince) {
		t.Fatalf("restored Unreadable = %+v, want %+v", got, wantSince)
	}

	// Readable and cool again: resolved the normal way.
	hw.sensors[0].Err, hw.sensors[1].Err, hw.sensors[1].Celsius = nil, nil, 50
	now = now.Add(time.Minute)
	alerts, _ = feed(e, now, c.Run(context.Background()))
	if len(alerts) != 1 || alerts[0].Kind != health.AlertResolved || open.IsOpen("temps/k10temp/tdie-2") {
		t.Fatalf("alerts = %+v, want one resolution", alerts)
	}
	if got := c.Unreadable(); len(got) != 0 {
		t.Fatalf("Unreadable = %+v after recovery", got)
	}
}

func TestGPUNoDevicesKeepsState(t *testing.T) {
	gpu := &fakeGPU{gpus: []temps.GPU{{Index: 0, Celsius: new(90.0)}, {Index: 1, Celsius: new(50.0)}}}
	e := health.NewEngine()
	open := engineOpen{e}
	now := t0()
	c := temps.New(nil, gpu, cfg(), open)
	if alerts, _ := feed(e, now, c.Run(context.Background())); len(alerts) != 1 || !open.IsOpen("temps/gpu/0") {
		t.Fatalf("alerts = %+v, want gpu 0 open", alerts)
	}

	for _, step := range []struct {
		name string
		gpus []temps.GPU
		err  error
		want map[string]health.Verdict
	}{
		{
			"[N/A] after a reading",
			[]temps.GPU{{Index: 0}, {Index: 1, Celsius: new(50.0)}},
			nil,
			map[string]health.Verdict{"temps/gpu/0": health.Unknown, "temps/gpu/1": health.Good},
		},
		{
			"the driver reports no devices", nil, temps.ErrNoGPUs,
			map[string]health.Verdict{"temps/gpu/0": health.Unknown, "temps/gpu/1": health.Unknown},
		},
	} {
		now = now.Add(time.Minute)
		gpu.gpus, gpu.err = step.gpus, step.err
		res := c.Run(context.Background())
		alerts, _ := feed(e, now, res)
		if res.Err != nil || len(alerts) != 0 || len(res.Forget) != 0 || len(res.Readings) != len(step.want) {
			t.Fatalf("%s: err = %v, alerts = %+v, forget = %q, readings = %+v",
				step.name, res.Err, alerts, res.Forget, res.Readings)
		}
		for key, v := range step.want {
			if r := find(res, key); len(r) != 1 || r[0].Verdict != v || r[0].Kind != health.State {
				t.Errorf("%s: %s = %+v, want %v", step.name, key, r, v)
			}
		}
		if !open.IsOpen("temps/gpu/0") {
			t.Fatalf("%s: the open problem was resolved", step.name)
		}
	}
	if r := find(c.Run(context.Background()), "temps/gpu/0"); len(r) != 1 ||
		r[0].Text != "gpu 0 is not reported: the driver reports no devices" {
		t.Errorf("no-devices text = %+v", r)
	}

	// Reporting again and cool: resolved the normal way.
	gpu.gpus, gpu.err = []temps.GPU{{Index: 0, Celsius: new(60.0)}, {Index: 1, Celsius: new(50.0)}}, nil
	now = now.Add(time.Minute)
	if alerts, _ := feed(e, now, c.Run(context.Background())); len(alerts) != 1 ||
		alerts[0].Kind != health.AlertResolved {
		t.Fatalf("alerts = %+v, want one resolution", alerts)
	}

	// A GPU that never reports a reading is unreadable too.
	res := temps.New(nil, &fakeGPU{gpus: []temps.GPU{{Index: 2}}}, cfg(), fakeOpen(nil)).Run(context.Background())
	if r := find(res, "temps/gpu/2"); len(r) != 1 || r[0].Verdict != health.Unknown ||
		r[0].Text != "gpu 2 reports no temperature" {
		t.Errorf("never read = %+v, want Unknown", r)
	}
	// With no GPU ever seen, no devices is no news.
	res = temps.New(nil, &fakeGPU{err: temps.ErrNoGPUs}, cfg(), fakeOpen(nil)).Run(context.Background())
	if res.Err != nil || len(res.Readings) != 0 {
		t.Errorf("no GPUs ever = %+v", res)
	}
}

func TestGoneWithoutProblemSilent(t *testing.T) {
	pkg := temps.Sensor{
		Chip:    "coretemp",
		Label:   "Package id 0",
		Device:  "/sys/devices/platform/coretemp.0",
		Celsius: 50,
	}
	acpi := temps.Sensor{
		Chip:    "acpitz",
		Label:   "temp1",
		Device:  "/sys/devices/virtual/thermal/thermal_zone0",
		Celsius: 40,
	}
	systin := temps.Sensor{
		Chip: "nct6775", Label: "SYSTIN", Device: "/sys/devices/platform/nct6775.656", Celsius: 85, Crit: new(80.0),
	}
	cputin := temps.Sensor{
		Chip: "nct6775", Label: "CPUTIN", Device: "/sys/devices/platform/nct6775.656", Celsius: 40, Crit: new(80.0),
	}
	// AUXTIN0, sampled only, keeps the nct6775 chip in the read.
	aux := temps.Sensor{Chip: "nct6775", Label: "AUXTIN0", Device: "/sys/devices/platform/nct6775.656", Celsius: 30}
	hw := &fakeHwmon{sensors: []temps.Sensor{pkg, acpi, systin, cputin, aux}}
	e := health.NewEngine()
	open := engineOpen{e}
	c := temps.New(hw, nil, cfg(), open)
	now := t0()
	feed(e, now, c.Run(context.Background()))
	if !open.IsOpen("temps/nct6775/systin") || open.IsOpen("temps/acpitz/temp1") {
		t.Fatal("want only SYSTIN open")
	}

	// SYSTIN's input vanishes while its chip stays: not gone, and its open
	// problem is kept.
	hw.sensors = []temps.Sensor{pkg, acpi, cputin, aux}
	now = now.Add(time.Minute)
	res := c.Run(context.Background())
	alerts, unreadable := feed(e, now, res)
	if len(alerts) != 0 || len(res.Forget) != 0 ||
		!slices.Equal(unreadable, []string{"temps/nct6775/systin/unreadable"}) {
		t.Fatalf("alerts = %+v, forget = %q, unreadable = %q", alerts, res.Forget, unreadable)
	}
	if !open.IsOpen("temps/nct6775/systin") {
		t.Fatal("the open problem was resolved")
	}

	// CPUTIN, fine, vanishes the same way: dropped without a message.
	hw.sensors = []temps.Sensor{pkg, acpi, aux}
	now = now.Add(time.Minute)
	res = c.Run(context.Background())
	if alerts, _ = feed(e, now, res); len(alerts) != 0 || !slices.Equal(res.Forget, []string{"temps/nct6775/cputin"}) {
		t.Fatalf("alerts = %+v, forget = %q", alerts, res.Forget)
	}

	// Both chips leave the read: acpitz, fine, is dropped silently; SYSTIN,
	// open, is reported gone.
	hw.sensors = []temps.Sensor{pkg}
	now = now.Add(time.Minute)
	res = c.Run(context.Background())
	alerts, _ = feed(e, now, res)
	if len(alerts) != 1 || alerts[0].Key != "temps/nct6775/systin" || alerts[0].Text != "nct6775 SYSTIN gone" {
		t.Fatalf("alerts = %+v, want only the SYSTIN gone event", alerts)
	}
	if got := slices.Sorted(slices.Values(res.Forget)); !slices.Equal(got,
		[]string{"temps/acpitz/temp1", "temps/nct6775/systin"}) {
		t.Fatalf("forget = %q", got)
	}
	if res = c.Run(context.Background()); len(res.Forget)+len(res.Readings) != 1 {
		t.Fatalf("second run = %+v, want only the package reading", res)
	}
}

func TestRetiredSensorEventOnlyWhenOpen(t *testing.T) {
	hw := &fakeHwmon{sensors: append(cpuHost(48), temps.Sensor{Chip: "nct6775", Label: "AUXTIN0", Celsius: 40})}
	open := fakeOpen{"temps/coretemp/core-3": true, "temps/nct6775/auxtin0": true}
	c := temps.New(hw, nil, cfg(), open)
	seen001 := `{"hwmon":{` +
		`"temps/coretemp/core-3":"coretemp Core 3",` +
		`"temps/coretemp/core-4":"coretemp Core 4",` +
		`"temps/k10temp/tctl":"k10temp Tctl",` +
		`"temps/nct6775/auxtin0":"nct6775 AUXTIN0"}}`
	if err := c.Restore(seen001); err != nil {
		t.Fatal(err)
	}
	res := c.Run(context.Background())
	events := map[string]string{}
	for _, r := range res.Readings {
		if r.Kind == health.Event {
			events[r.Key] = r.Text
		}
	}
	want := map[string]string{
		"temps/coretemp/core-3": "coretemp Core 3 is no longer judged separately; the package sensor covers it",
		"temps/nct6775/auxtin0": "nct6775 AUXTIN0 is no longer judged: it is not listed and exposes no critical limit",
	}
	if len(events) != len(want) {
		t.Fatalf("events = %q, want %q", events, want)
	}
	for k, v := range want {
		if events[k] != v {
			t.Errorf("%s event = %q, want %q", k, events[k], v)
		}
	}
	forget := slices.Sorted(slices.Values(res.Forget))
	wantForget := []string{
		"temps/coretemp/core-3", "temps/coretemp/core-4", "temps/k10temp/tctl", "temps/nct6775/auxtin0",
	}
	if !slices.Equal(forget, wantForget) {
		t.Errorf("Forget = %q, want %q", forget, wantForget)
	}
}

func TestOwnCritRange(t *testing.T) {
	hw := boardHost(
		temps.Sensor{Chip: "nct6775", Label: "AUXTIN0", Celsius: 130, Crit: new(127.0)},
		temps.Sensor{Chip: "nct6775", Label: "AUXTIN1", Celsius: 130, Crit: new(255.0)},
		temps.Sensor{Chip: "nct6775", Label: "AUXTIN2", Celsius: 130, Crit: new(0.0)},
		temps.Sensor{Chip: "nct6775", Label: "AUXTIN3", Celsius: 150, Crit: new(150.0)},
		temps.Sensor{Chip: "nct6775", Label: "AUXTIN4", Celsius: 0.5, Crit: new(0.5)},
	)
	res := temps.New(hw, nil, cfg(), fakeOpen(nil)).Run(context.Background())
	for key, want := range map[string]int{
		"temps/nct6775/auxtin0": 0, "temps/nct6775/auxtin1": 0, "temps/nct6775/auxtin2": 0,
		"temps/nct6775/auxtin3": 1, "temps/nct6775/auxtin4": 1,
	} {
		r := find(res, key)
		if len(r) != want || (want == 1 && r[0].Verdict != health.Bad) {
			t.Errorf("%s = %+v, want %d Bad readings", key, r, want)
		}
	}
	if _, ok := sampled(res, "temp.board.nct6775.auxtin1"); !ok {
		t.Error("a sensor with an unset crit is still sampled")
	}
}

func TestPreZenK10tempFallback(t *testing.T) {
	const dev = "/sys/devices/pci0000:00/0000:00:18.3"
	sole := temps.Sensor{Chip: "k10temp", Label: "temp1", Device: dev, Celsius: 86}
	res := temps.New(&fakeHwmon{sensors: []temps.Sensor{sole}}, nil, cfg(), fakeOpen(nil)).Run(context.Background())
	if res.Err != nil {
		t.Fatalf("err = %v, want the sole input taken as the package sensor", res.Err)
	}
	if r := find(res, "temps/k10temp/temp1"); len(r) != 1 || r[0].Verdict != health.Bad {
		t.Fatalf("temp1 = %+v, want Bad at the CPU limit", r)
	}

	// Two unlabeled inputs: neither is the package sensor.
	two := []temps.Sensor{sole, {Chip: "k10temp", Label: "temp2", Device: dev, Celsius: 40}}
	res = temps.New(&fakeHwmon{sensors: two}, nil, cfg(), fakeOpen(nil)).Run(context.Background())
	if len(res.Readings) != 0 || res.Err == nil {
		t.Fatalf("readings = %+v, err = %v, want temps/cpu/none", res.Readings, res.Err)
	}

	// Tctl wins over an unlabeled input.
	tctl := []temps.Sensor{sole, {Chip: "k10temp", Label: "Tctl", Device: dev, Celsius: 40}}
	res = temps.New(&fakeHwmon{sensors: tctl}, nil, cfg(), fakeOpen(nil)).Run(context.Background())
	if len(res.Readings) != 1 || len(find(res, "temps/k10temp/tctl")) != 1 {
		t.Fatalf("readings = %+v, want Tctl only", res.Readings)
	}
}

func TestSensorWeekQuiet(t *testing.T) {
	const zone0 = "/sys/devices/virtual/thermal/thermal_zone0"
	hw := &fakeHwmon{}
	e := health.NewEngine()
	now := t0()
	c := temps.New(hw, nil, cfg(), engineOpen{e}, temps.WithClock(func() time.Time { return now }))
	// Upgraded from 001, which judged every core.
	if err := c.Restore(`{"hwmon":{"temps/coretemp/core-3":"coretemp Core 3"}}`); err != nil {
		t.Fatal(err)
	}
	const step = 10 * time.Minute
	end := now.Add(7 * 24 * time.Hour)
	for i := 0; now.Before(end); i++ {
		sensors := []temps.Sensor{
			{Chip: "coretemp", Label: "Package id 0", Device: "/sys/devices/platform/coretemp.0", Celsius: 60},
		}
		for core := range 16 {
			// Single cores spike past the CPU limit now and then.
			sensors = append(sensors, temps.Sensor{
				Chip: "coretemp", Label: "Core " + strconv.Itoa(core), Device: "/sys/devices/platform/coretemp.0",
				Celsius: 50 + float64((i+core)%50),
			})
		}
		// An unlisted board sensor without crit, reading nonsense at times.
		sensors = append(sensors, temps.Sensor{Chip: "nct6775", Label: "AUXTIN0", Celsius: float64(i%300 - 128)})
		// A listed sensor that is fine and leaves on day three.
		if now.Before(t0().Add(72 * time.Hour)) {
			sensors = append(sensors, temps.Sensor{Chip: "acpitz", Label: "temp1", Device: zone0, Celsius: 40})
		}
		hw.sensors = sensors

		res := c.Run(context.Background())
		alerts, unreadable := feed(e, now, res)
		if res.Err != nil || len(alerts) != 0 || len(unreadable) != 0 {
			t.Fatalf("%v: err = %v, alerts = %+v, unreadable = %q", now, res.Err, alerts, unreadable)
		}
		now = now.Add(step)
	}
	if conds := e.Conditions(); len(conds) != 0 {
		t.Fatalf("conditions after a quiet week = %+v", conds)
	}
}

// TestSampleOnlyErrIsSilent: an unlisted board sensor without a critical limit
// that cannot be read gives no reading, no checker error and no sample.
func TestSampleOnlyErrIsSilent(t *testing.T) {
	hw := boardHost(temps.Sensor{Chip: "nct6775", Label: "AUXTIN0", Err: errors.New("i/o")})
	c := temps.New(hw, nil, cfg(), fakeOpen(nil))
	res := c.Run(context.Background())
	if res.Err != nil {
		t.Fatalf("Err = %v, want no checker error", res.Err)
	}
	if r := find(res, "temps/nct6775/auxtin0"); len(r) != 0 {
		t.Fatalf("readings = %+v, want none", r)
	}
	if _, ok := sampled(res, "temp.board.nct6775.auxtin0"); ok {
		t.Fatal("a sample for an unreadable sample-only sensor")
	}
	if got := c.Unreadable(); len(got) != 0 {
		t.Fatalf("Unreadable = %+v, want none", got)
	}
	for _, s := range c.Sensors() {
		if strings.Contains(s.Metric, "auxtin0") {
			t.Fatalf("Sensors lists the unreadable sample-only sensor: %+v", s)
		}
	}
}

// TestSensorsStatus: what /temp needs of each sensor: the metric, a label, the
// limit it is judged against (none for a sample-only one) and, when it gives no
// usable reading, since when.
func TestSensorsStatus(t *testing.T) {
	now := t0()
	hw := boardHost(
		temps.Sensor{Chip: "acpitz", Label: "temp1", Celsius: 40},
		temps.Sensor{Chip: "nct6775", Label: "SYSTIN", Celsius: 40, Crit: new(95.0)},
		temps.Sensor{Chip: "nct6775", Label: "AUXTIN0", Celsius: 41},
		temps.Sensor{Chip: "nct6775", Label: "AUXTIN1", Celsius: 41, Crit: new(90.0)},
	)
	sixty := 60.0
	gpu := &fakeGPU{gpus: []temps.GPU{{Index: 0, Name: "Example GPU", Celsius: &sixty}}}
	c := temps.New(hw, gpu, cfg(), fakeOpen(nil), temps.WithClock(func() time.Time { return now }))
	c.Run(context.Background())

	want := []temps.SensorStatus{
		{Metric: "temp.board.acpitz.temp1", Label: "acpitz temp1", Limit: 70},
		{Metric: "temp.board.nct6775.auxtin0", Label: "nct6775 auxtin0"},
		{Metric: "temp.board.nct6775.auxtin1", Label: "nct6775 auxtin1", Limit: 90},
		{Metric: "temp.board.nct6775.systin", Label: "nct6775 systin", Limit: 95},
		{Metric: "temp.cpu.coretemp.package-id-0", Label: "coretemp package-id-0", Limit: 85},
		{Metric: "temp.gpu.0", Label: "gpu 0", Limit: 80},
	}
	if got := c.Sensors(); !slices.Equal(got, want) {
		t.Fatalf("Sensors = %+v\nwant %+v", got, want)
	}

	// The CPU package stops reading: unreadable since this run, limit kept.
	hw.sensors[0].Err = errors.New("i/o")
	now = now.Add(time.Minute)
	c.Run(context.Background())
	got := c.Sensors()
	var cpu temps.SensorStatus
	for _, s := range got {
		if s.Metric == "temp.cpu.coretemp.package-id-0" {
			cpu = s
		}
	}
	if !cpu.Since.Equal(now) || cpu.Limit != 85 || len(got) != len(want) {
		t.Fatalf("cpu = %+v (of %d), want unreadable since %v with limit 85", cpu, len(got), now)
	}

	// An implausible value counts too: the board sensor shows Since, limit kept.
	hw.sensors[0].Err = nil
	hw.sensors[1].Celsius = 200
	later := now.Add(time.Minute)
	now = later
	c.Run(context.Background())
	for _, s := range c.Sensors() {
		switch s.Metric {
		case "temp.board.acpitz.temp1":
			if !s.Since.Equal(later) || s.Limit != 70 {
				t.Fatalf("implausible = %+v, want since %v with limit 70", s, later)
			}
		case "temp.cpu.coretemp.package-id-0":
			if !s.Since.IsZero() {
				t.Fatalf("cpu = %+v, want readable again", s)
			}
		}
	}
}

// blockingHwmon blocks Sensors until release is closed; entered is closed
// when a read has begun.
type blockingHwmon struct {
	entered, release chan struct{}
}

func (b *blockingHwmon) Sensors() ([]temps.Sensor, error) {
	close(b.entered)
	<-b.release

	return []temps.Sensor{{Chip: "coretemp", Label: "Package id 0", Celsius: 50}}, nil
}

// TestReadDoesNotHoldTheLock: a hung hwmon read does not block Export,
// Sensors or Unreadable (the checkpoint and /temp).
func TestReadDoesNotHoldTheLock(t *testing.T) {
	hw := &blockingHwmon{entered: make(chan struct{}), release: make(chan struct{})}
	c := temps.New(hw, nil, cfg(), fakeOpen(nil))
	ran := make(chan struct{})
	go func() {
		c.Run(context.Background())
		close(ran)
	}()
	<-hw.entered

	done := make(chan struct{})
	go func() {
		if _, err := c.Export(); err != nil {
			t.Error(err)
		}
		c.Sensors()
		c.Unreadable()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("Export blocked behind a hung hwmon read")
	}
	close(hw.release)
	<-ran
}
