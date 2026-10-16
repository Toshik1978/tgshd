package temps_test

import (
	"context"
	"errors"
	"testing"

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

func limits() config.Limits { return config.Limits{CPU: 85, GPU: 80, Board: 70} }

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
	gpu := &fakeGPU{gpus: []temps.GPU{{Index: 0, Name: "Example GPU", Celsius: 60}}}
	res := temps.New(hw, gpu, limits()).Run(context.Background())
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
	res := temps.New(hw, nil, config.Limits{}).Run(context.Background())
	if r := find(res, "temps/acpitz/temp1"); len(r) != 1 || r[0].Verdict != health.Good {
		t.Errorf("got %+v", r)
	}
}

func TestGoneReportedOnce(t *testing.T) {
	hw := &fakeHwmon{sensors: []temps.Sensor{
		{Chip: "acpitz", Label: "temp1", Celsius: 80},
		{Chip: "acpitz", Label: "temp2", Celsius: 40},
	}}
	c := temps.New(hw, nil, limits())
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
	if ev != 1 || good != 1 {
		t.Fatalf("event=%d good=%d in %+v", ev, good, res.Readings)
	}
	if r := c.Run(context.Background()); len(find(r, "temps/acpitz/temp1")) != 0 {
		t.Errorf("gone reported twice: %+v", r.Readings)
	}
}

func TestAdapterErrorEmitsNothingNotGone(t *testing.T) {
	hw := &fakeHwmon{sensors: []temps.Sensor{{Chip: "acpitz", Label: "temp1", Celsius: 40}}}
	gpu := &fakeGPU{gpus: []temps.GPU{{Index: 0, Celsius: 40}}}
	c := temps.New(hw, gpu, limits())
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
	c = temps.New(&fakeHwmon{err: errors.New("boom")}, &fakeGPU{}, limits())
	if res = c.Run(context.Background()); res.Err == nil {
		t.Error("want Err for a source failing since start")
	}
}

func TestGPUGone(t *testing.T) {
	gpu := &fakeGPU{gpus: []temps.GPU{{Index: 1, Name: "Example GPU", Celsius: 90}}}
	c := temps.New(nil, gpu, limits())
	if r := find(c.Run(context.Background()), "temps/gpu/1"); len(r) != 1 || r[0].Verdict != health.Bad {
		t.Fatalf("got %+v", r)
	}
	gpu.gpus = nil
	if r := find(c.Run(context.Background()), "temps/gpu/1"); len(r) != 2 {
		t.Fatalf("got %+v", r)
	}
}

func TestDuplicateKeysAreDisambiguated(t *testing.T) {
	hw := &fakeHwmon{sensors: []temps.Sensor{
		{Chip: "coretemp", Label: "Package id 0", Celsius: 40},
		{Chip: "coretemp", Label: "Package id 0", Celsius: 41},
	}}
	res := temps.New(hw, nil, limits()).Run(context.Background())
	if len(res.Readings) != 2 || res.Readings[0].Key == res.Readings[1].Key {
		t.Errorf("keys collide: %+v", res.Readings)
	}
}

func TestBadSensorDisappears(t *testing.T) {
	hw := &fakeHwmon{sensors: []temps.Sensor{{Chip: "acpitz", Label: "temp1", Celsius: 99}}}
	c := temps.New(hw, nil, limits())
	c.Run(context.Background())
	hw.sensors = nil
	var sawGood, sawEvent bool
	for _, r := range c.Run(context.Background()).Readings {
		if r.Kind == health.Event {
			sawEvent = r.Text == "acpitz temp1 gone"
		} else if r.Verdict == health.Good {
			sawGood = true
			if r.Text != "" {
				t.Errorf("resolving reading text = %q, want empty", r.Text)
			}
		}
	}
	if !sawGood || !sawEvent {
		t.Errorf("good=%v event=%v", sawGood, sawEvent)
	}
}

func TestExportRestore(t *testing.T) {
	hw := &fakeHwmon{sensors: []temps.Sensor{{Chip: "acpitz", Label: "temp1", Celsius: 40}}}
	gpu := &fakeGPU{gpus: []temps.GPU{{Index: 0, Celsius: 40}}}
	c := temps.New(hw, gpu, limits())
	c.Run(context.Background())
	data, err := c.Export()
	if err != nil {
		t.Fatal(err)
	}

	hw2 := &fakeHwmon{}
	gpu2 := &fakeGPU{}
	c2 := temps.New(hw2, gpu2, limits())
	if err := c2.Restore(data); err != nil {
		t.Fatal(err)
	}
	res := c2.Run(context.Background())
	for _, k := range []string{"temps/acpitz/temp1", "temps/gpu/0"} {
		if r := find(res, k); len(r) != 2 {
			t.Errorf("%s = %+v, want good + gone event", k, r)
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
		{Chip: "coretemp", Label: "Core 0", Celsius: 85},
		{Chip: "Weird Chip/1", Label: "", Celsius: 10},
	}}
	res := temps.New(hw, nil, limits()).Run(context.Background())
	if r := find(res, "temps/coretemp/core-0"); len(r) != 1 || r[0].Verdict != health.Good {
		t.Errorf("limit-equal = %+v, want Good", r)
	}
	if r := find(res, "temps/weird-chip-1/temp"); len(r) != 1 {
		t.Errorf("fallback key missing: %+v", res.Readings)
	}
}
