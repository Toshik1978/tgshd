package memory_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Toshik1978/tgshd/pkg/health"
	"github.com/Toshik1978/tgshd/pkg/health/check/memory"
)

const gb = uint64(1) << 30

type fakeSource struct {
	host     memory.Host
	hostErr  error
	services []memory.Service
	svcErr   error
	process  memory.Process
	procErr  error
	svcCalls int

	containers []memory.Container
	ctrErr     error
}

func (f *fakeSource) Host(context.Context) (memory.Host, error) { return f.host, f.hostErr }

func (f *fakeSource) Services(context.Context) ([]memory.Service, error) {
	f.svcCalls++
	return f.services, f.svcErr
}

func (f *fakeSource) TopProcess(context.Context) (memory.Process, error) { return f.process, f.procErr }

func (f *fakeSource) Containers(context.Context) ([]memory.Container, error) {
	return f.containers, f.ctrErr
}

func settings() memory.Settings {
	return memory.Settings{WarnAvailablePercent: 15, WarnSwapPercent: 80, WarnPressurePercent: 10}
}

func healthy() memory.Host {
	return memory.Host{
		Total: 62 * gb, Available: 28 * gb, SwapTotal: 8 * gb, SwapFree: 6 * gb,
		SomeAvg60: 0, HasPressure: true,
	}
}

func find(t *testing.T, res health.CheckResult, key string) health.Reading {
	t.Helper()
	for _, r := range res.Readings {
		if r.Key == key {
			return r
		}
	}
	t.Fatalf("no reading %q in %+v", key, res.Readings)

	return health.Reading{}
}

func sample(t *testing.T, res health.CheckResult, metric string) (float64, bool) {
	t.Helper()
	for _, s := range res.Samples {
		if s.Metric == metric {
			return s.Value, true
		}
	}

	return 0, false
}

func TestAvailableProblemNamesTopService(t *testing.T) {
	host := healthy()
	host.Available = gb / 10 * 56
	src := &fakeSource{
		host:     host,
		services: []memory.Service{{Name: "db", Anon: 4 * gb}, {Name: "web-app", Anon: 31 * gb}},
	}

	res := memory.New(src, settings()).Run(context.Background())

	r := find(t, res, "memory/available")
	if r.Verdict != health.Bad || r.Kind != health.State {
		t.Fatalf("reading = %+v", r)
	}
	if want := "9% available (5.6 of 62 GB); top: web-app 31 GB"; r.Text != want {
		t.Fatalf("text = %q, want %q", r.Text, want)
	}
	if find(t, res, "memory/swap").Verdict != health.Good || find(t, res, "memory/pressure").Verdict != health.Good {
		t.Fatalf("other readings must stay good: %+v", res.Readings)
	}
	if v, ok := sample(t, res, "memory.available_pct"); !ok || v < 8.9 || v > 9.1 {
		t.Fatalf("available sample = %v %v", v, ok)
	}
}

func TestSwapProblem(t *testing.T) {
	host := healthy()
	host.SwapFree = gb / 10 * 12
	src := &fakeSource{host: host, services: []memory.Service{{Name: "web-app", Anon: 31 * gb}}}

	res := memory.New(src, settings()).Run(context.Background())

	r := find(t, res, "memory/swap")
	if want := "swap 85% used (6.8 of 8 GB); top: web-app 31 GB"; r.Verdict != health.Bad || r.Text != want {
		t.Fatalf("reading = %+v, want text %q", r, want)
	}
	if v, ok := sample(t, res, "memory.swap_pct"); !ok || v < 84.9 || v > 85.1 {
		t.Fatalf("swap sample = %v %v", v, ok)
	}
}

func TestPressureProblem(t *testing.T) {
	host := healthy()
	host.SomeAvg60 = 12
	src := &fakeSource{host: host, services: []memory.Service{{Name: "web-app", Anon: 31 * gb}}}

	res := memory.New(src, settings()).Run(context.Background())

	r := find(t, res, "memory/pressure")
	if want := "pressure 12% (stalled, 1 min avg); top: web-app 31 GB"; r.Verdict != health.Bad || r.Text != want {
		t.Fatalf("reading = %+v, want text %q", r, want)
	}
	if v, ok := sample(t, res, "memory.pressure_some_avg60"); !ok || v != 12 {
		t.Fatalf("pressure sample = %v %v", v, ok)
	}
}

func TestResolvedTexts(t *testing.T) {
	host := memory.Host{
		Total: 62 * gb, Available: 28 * gb, SwapTotal: 8 * gb, SwapFree: 6 * gb, HasPressure: true,
	}
	src := &fakeSource{host: host}

	res := memory.New(src, settings()).Run(context.Background())

	for key, want := range map[string]string{
		"memory/available": "45% available",
		"memory/swap":      "swap 25% used",
		"memory/pressure":  "pressure 0%",
	} {
		r := find(t, res, key)
		if r.Verdict != health.Good || r.Text != want {
			t.Errorf("%s = %+v, want good %q", key, r, want)
		}
	}
	if src.svcCalls != 0 {
		t.Errorf("services read %d times with nothing to name", src.svcCalls)
	}
	if res.Err != nil {
		t.Errorf("err = %v", res.Err)
	}
}

func TestNoSwapHost(t *testing.T) {
	host := healthy()
	host.SwapTotal, host.SwapFree = 0, 0

	res := memory.New(&fakeSource{host: host}, settings()).Run(context.Background())

	for _, r := range res.Readings {
		if r.Key == "memory/swap" {
			t.Fatalf("swap reading on a host without swap: %+v", r)
		}
	}
	if _, ok := sample(t, res, "memory.swap_pct"); ok {
		t.Fatal("swap sample on a host without swap")
	}
	if len(res.Readings) != 2 {
		t.Fatalf("readings = %+v", res.Readings)
	}
}

func TestMissingPressureIsOneCheckErrorAndOthersContinue(t *testing.T) {
	host := healthy()
	host.HasPressure = false
	host.Available = 5 * gb

	res := memory.New(&fakeSource{host: host}, settings()).Run(context.Background())

	errs := health.CheckErrors(res.Err)
	if len(errs) != 1 || errs[0].Subject != "pressure" || errs[0].Kind != "unavailable" {
		t.Fatalf("errors = %+v", errs)
	}
	if !errors.Is(res.Err, memory.ErrNoPressure) {
		t.Fatalf("err does not wrap ErrNoPressure: %v", res.Err)
	}
	if find(t, res, "memory/available").Verdict != health.Bad {
		t.Fatal("available judgement must continue")
	}
	for _, r := range res.Readings {
		if r.Key == "memory/pressure" {
			t.Fatalf("pressure reading without PSI: %+v", r)
		}
	}
	if _, ok := sample(t, res, "memory.pressure_some_avg60"); ok {
		t.Fatal("pressure sample without PSI")
	}
}

func TestHostReadFailureIsCheckError(t *testing.T) {
	res := memory.New(&fakeSource{hostErr: errors.New("boom")}, settings()).Run(context.Background())

	errs := health.CheckErrors(res.Err)
	if len(errs) != 1 || errs[0].Subject != "host" || errs[0].Kind != "unreadable" {
		t.Fatalf("errors = %+v", errs)
	}
	if len(res.Readings) != 0 {
		t.Fatalf("readings = %+v", res.Readings)
	}
}

func TestTopFallsBackToProcessWhenNoServiceHoldsMost(t *testing.T) {
	host := healthy()
	host.Available = 5 * gb // 57 GB in use
	src := &fakeSource{
		host:     host,
		services: []memory.Service{{Name: "web-app", Anon: 10 * gb}, {Name: "db", Anon: 8 * gb}},
		process:  memory.Process{Name: "python3", PID: 4242, RSS: 40 * gb},
	}

	res := memory.New(src, settings()).Run(context.Background())

	want := "8% available (5 of 62 GB); top: python3 (pid 4242) 40 GB"
	if got := find(t, res, "memory/available").Text; got != want {
		t.Fatalf("text = %q, want %q", got, want)
	}
}

func TestTopOmittedWhenNothingKnown(t *testing.T) {
	host := healthy()
	host.Available = 5 * gb
	src := &fakeSource{host: host, svcErr: errors.New("no cgroups"), procErr: errors.New("no procfs")}

	res := memory.New(src, settings()).Run(context.Background())

	if got, want := find(t, res, "memory/available").Text, "8% available (5 of 62 GB)"; got != want {
		t.Fatalf("text = %q, want %q", got, want)
	}
	errs := health.CheckErrors(res.Err)
	if len(errs) != 2 {
		t.Fatalf("errors = %+v", errs)
	}
	if errs[0].Subject != "services" || errs[1].Subject != "processes" {
		t.Fatalf("subjects = %q, %q", errs[0].Subject, errs[1].Subject)
	}
}
