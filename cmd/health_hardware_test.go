package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/fx/fxtest"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/Toshik1978/tgshd/pkg/health"
	"github.com/Toshik1978/tgshd/pkg/health/check/disks"
	"github.com/Toshik1978/tgshd/pkg/health/check/kernel"
	"github.com/Toshik1978/tgshd/pkg/health/check/temps"
	hcfg "github.com/Toshik1978/tgshd/pkg/health/config"
	"github.com/Toshik1978/tgshd/pkg/health/store"
	"github.com/Toshik1978/tgshd/pkg/host/edac"
	"github.com/Toshik1978/tgshd/pkg/host/execprobe"
	"github.com/Toshik1978/tgshd/pkg/host/hwmon"
	"github.com/Toshik1978/tgshd/pkg/host/journal"
	"github.com/Toshik1978/tgshd/pkg/host/kmsg"
	"github.com/Toshik1978/tgshd/pkg/host/nvidia"
	"github.com/Toshik1978/tgshd/pkg/host/smartctl"
)

type fakeDriveBook struct{}

func (*fakeDriveBook) Baselines() []store.Baseline { return nil }
func (*fakeDriveBook) Retire(string) bool          { return false }

// fakePersister records what it was restored from and how often it saved.
type fakePersister struct {
	mu       sync.Mutex
	restored []store.State
	saves    int
}

func (p *fakePersister) Restore(st store.State) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.restored = append(p.restored, st)

	return nil
}

func (p *fakePersister) Save(*store.State) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.saves++

	return nil
}

// nvmeHot is smartctl JSON of a healthy NVMe drive at 80°C.
const nvmeHot = `{"device":{"name":"/dev/nvme0","protocol":"NVMe"},"model_name":"Example NVMe SSD",
"serial_number":"TEST0000000001","smart_status":{"passed":true},"temperature":{"current":80},
"power_on_time":{"hours":100},"nvme_smart_health_information_log":{"critical_warning":0,"temperature":80,
"available_spare":100,"available_spare_threshold":10,"percentage_used":1,"media_errors":0,"num_err_log_entries":0},
"nvme_self_test_log":{"table":[]}}`

type fakeSmart struct{}

func (fakeSmart) Drives() ([]disks.Node, error) {
	return []disks.Node{
		{Name: "nvme0n1", Path: "/sys/devices/pci0000:00/0000:00:1d.0/0000:01:00.0/nvme/nvme0/nvme0n1"},
	}, nil
}

func (fakeSmart) Run(context.Context, string, bool) ([]byte, int, error) {
	return []byte(nvmeHot), 0, nil
}

func hasKey(readings []health.Reading, key string, v health.Verdict) bool {
	return slices.ContainsFunc(readings, func(r health.Reading) bool { return r.Key == key && r.Verdict == v })
}

func TestHardwareChecksFollowTheConfig(t *testing.T) {
	t.Parallel()

	for _, hs := range []*healthSetup{{}, {Config: &hcfg.Config{}}} {
		for name, set := range map[string]checkSet{
			"disks": disksChecks(hs, zap.NewNop()), "temps": tempsChecks(hs, newOpenState()), "kernel": kernelChecks(hs, newContainerNames(), zap.NewNop()),
		} {
			if len(set.Checks)+len(set.Errors)+len(set.Persisters)+len(set.Background) != 0 || set.Drives != nil {
				t.Fatalf("%s without a section: %+v", name, set)
			}
		}
	}

	hs := &healthSetup{Config: &hcfg.Config{
		Disks: &hcfg.Disks{}, Temperatures: &hcfg.Temperatures{}, Kernel: &hcfg.Kernel{},
	}}
	if set := disksChecks(hs, zap.NewNop()); len(set.Checks) != 1 || set.Checks[0].Name != disksCheckName ||
		len(set.Persisters) != 1 || set.Drives == nil {
		t.Fatalf("disks section: %+v", set)
	}
	if set := tempsChecks(hs, newOpenState()); len(set.Checks) != 1 || set.Checks[0].Name != tempsCheckName ||
		len(set.Persisters) != 1 {
		t.Fatalf("temperatures section: %+v", set)
	}
	set := kernelChecks(hs, newContainerNames(), zap.NewNop())
	if len(set.Checks) != 1 || set.Checks[0].Name != kernelCheckName ||
		len(set.Persisters) != 1 || len(set.Background) != 1 {
		t.Fatalf("kernel section: %+v", set)
	}
}

// TestStandbyStatusAgrees: the disks check skips exactly the status the
// smartctl adapter asks smartctl to exit with for a drive in standby.
func TestStandbyStatusAgrees(t *testing.T) {
	t.Parallel()

	if disks.StandbyStatus != smartctl.StandbyStatus {
		t.Fatalf("disks.StandbyStatus = %d, smartctl.StandbyStatus = %d", disks.StandbyStatus, smartctl.StandbyStatus)
	}
}

// TestWaitDelaysFitTheRunMargin: health.Bound keeps a margin of
// min(RunMargin, a tenth of the time left) before the run's deadline. With
// the full margin (at least 10×RunMargin left), a killed probe whose output
// pipes stay open uses at most half of it, leaving the rest for the kill and
// the check to return, so the check judges it rather than the run being
// abandoned. With less than 10×WaitDelay left the margin is shorter than
// WaitDelay and this does not hold; the adapters document that.
func TestWaitDelaysFitTheRunMargin(t *testing.T) {
	t.Parallel()

	for name, d := range map[string]time.Duration{
		"smartctl": smartctl.WaitDelay, "execprobe": execprobe.WaitDelay,
	} {
		if 2*d > health.RunMargin {
			t.Errorf("%s.WaitDelay = %s, want at most half of health.RunMargin %s", name, d, health.RunMargin)
		}
	}
}

func TestBuildDisksCheck(t *testing.T) {
	t.Parallel()

	d := &hcfg.Disks{Interval: hcfg.Duration(time.Hour), WearPercent: 80}
	tc := &hcfg.Temperatures{Limits: hcfg.Limits{DriveNVMe: 70, DriveSATA: 60}}
	now := func() time.Time { return time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC) }
	set := buildDisksCheck(d, tc, fakeSmart{}, now, zap.NewNop(), false)
	chk := set.Checks[0]
	if chk.Name != "disks" || chk.Interval != time.Hour {
		t.Fatalf("check = %+v", chk)
	}

	if res := chk.Run(t.Context()); !errors.Is(res.Err, disks.ErrBaselinesNotLoaded) {
		t.Fatalf("run before restore = %+v, want the baselines required", res)
	}
	p := set.Persisters[0]
	if err := p.Restore(store.State{}); err != nil {
		t.Fatal(err)
	}
	res := chk.Run(t.Context())
	if res.Err != nil || !hasKey(res.Readings, "disks/TEST0000000001/temp", health.Bad) {
		t.Fatalf("readings = %+v, err %v; want the drive judged against temperatures.limits", res.Readings, res.Err)
	}

	if !set.Drives.Retire("TEST0000000001") {
		t.Fatal("the drive book must be the disks check")
	}
	var st store.State
	if err := p.Save(&st); err != nil {
		t.Fatal(err)
	}
	if len(st.Baselines) != 1 || st.Baselines[0].Serial != "TEST0000000001" || !st.Baselines[0].Retired {
		t.Fatalf("saved baselines = %+v, want the retired drive", st.Baselines)
	}

	// The stored baselines come back on restore: the drive is known and stays retired.
	again := buildDisksCheck(d, nil, fakeSmart{}, now, zap.NewNop(), false)
	if err := again.Persisters[0].Restore(st); err != nil {
		t.Fatal(err)
	}
	if b := again.Drives.Baselines(); len(b) != 1 || !b[0].Retired {
		t.Fatalf("restored baselines = %+v", b)
	}
	if res := again.Checks[0].Run(t.Context()); hasKey(res.Readings, "disks/TEST0000000001/temp", health.Bad) {
		t.Fatalf("without a temperatures section drive temperatures are not judged: %+v", res.Readings)
	}
}

type fakeHwmon struct {
	sensors []hwmon.Sensor
	err     error
}

func (f fakeHwmon) Sensors() ([]hwmon.Sensor, error) { return f.sensors, f.err }

type fakeGPUs struct {
	gpus     []nvidia.GPU
	err      error
	deadline bool
}

func (f *fakeGPUs) GPUs(ctx context.Context) ([]nvidia.GPU, error) {
	_, f.deadline = ctx.Deadline()

	return f.gpus, f.err
}

func TestNvidiaSourceMapsNoDevices(t *testing.T) {
	t.Parallel()

	_, err := nvidiaSource{reader: &fakeGPUs{err: nvidia.ErrNoDevices}}.GPUs(t.Context())
	if !errors.Is(err, temps.ErrNoGPUs) {
		t.Fatalf("err = %v, want temps.ErrNoGPUs", err)
	}
}

func TestTemperatureSources(t *testing.T) {
	t.Parallel()

	maxC, crit := 90.0, 100.0
	unreadable := errors.New("input/output error")
	in := []hwmon.Sensor{
		{
			Chip: "coretemp", Label: "Package id 0", Device: "/sys/devices/example/hwmon3",
			Celsius: 85, Max: &maxC, Crit: &crit,
		},
		{Chip: "acpitz", Label: "temp1", Device: "/sys/devices/example/hwmon4", Err: unreadable},
	}
	got, err := hwmonSource{reader: fakeHwmon{sensors: in}}.Sensors()
	if err != nil || len(got) != 2 {
		t.Fatalf("sensors = %+v, %v", got, err)
	}
	want := temps.Sensor{
		Chip: "coretemp", Label: "Package id 0", Device: "/sys/devices/example/hwmon3",
		Celsius: 85, Max: &maxC, Crit: &crit,
	}
	if got[0] != want {
		t.Fatalf("sensor = %+v, want %+v", got[0], want)
	}
	if got[1].Device != "/sys/devices/example/hwmon4" || !errors.Is(got[1].Err, unreadable) {
		t.Fatalf("unreadable sensor = %+v", got[1])
	}
	readErr := errors.New("permission denied")
	if _, err = (hwmonSource{reader: fakeHwmon{err: readErr}}).Sensors(); !errors.Is(err, readErr) {
		t.Fatalf("err = %v", err)
	}

	hot := 70.0
	gpus := &fakeGPUs{
		gpus: []nvidia.GPU{{Index: 0, Name: "Example GPU", Celsius: &hot}, {Index: 1, Name: "Example GPU"}},
	}
	g, err := nvidiaSource{reader: gpus}.GPUs(t.Context())
	if err != nil || len(g) != 2 || g[0].Celsius == nil || *g[0].Celsius != 70 || g[0].Index != 0 ||
		g[0].Name != "Example GPU" ||
		g[1].Index != 1 ||
		g[1].Celsius != nil {
		t.Fatalf("gpus = %+v, %v", g, err)
	}
	gpus.err = readErr
	if _, err = (nvidiaSource{reader: gpus}).GPUs(t.Context()); !errors.Is(err, readErr) {
		t.Fatalf("err = %v", err)
	}
}

func TestBuildTempsCheck(t *testing.T) {
	t.Parallel()

	tc := &hcfg.Temperatures{
		Interval: hcfg.Duration(time.Minute),
		Limits:   hcfg.Limits{CPU: 80, GPU: 85, Board: 70},
		// board_sensors reaches the check: SYSTIN is judged against 65, not 70.
		BoardSensors: []hcfg.BoardSensor{{Chip: "nct*", Label: "SYSTIN", Limit: new(65)}},
	}
	sensors := fakeHwmon{sensors: []hwmon.Sensor{
		{Chip: "coretemp", Label: "Package id 0", Celsius: 95},
		{Chip: "nct6775", Label: "SYSTIN", Celsius: 66},
	}}
	sixty := 60.0
	gpus := &fakeGPUs{gpus: []nvidia.GPU{{Index: 0, Name: "Example GPU", Celsius: &sixty}}}
	set := buildTempsCheck(tc, hwmonSource{reader: sensors}, nvidiaSource{reader: gpus}, newOpenState())
	chk := set.Checks[0]
	if chk.Name != "temps" || chk.Interval != time.Minute {
		t.Fatalf("check = %+v", chk)
	}

	res := chk.Run(t.Context())
	if res.Err != nil || !hasKey(res.Readings, "temps/gpu/0", health.Good) ||
		!hasKey(res.Readings, "temps/nct6775/systin", health.Bad) || len(res.Readings) != 3 {
		t.Fatalf("readings = %+v, err %v", res.Readings, res.Err)
	}
	if !gpus.deadline {
		t.Fatal("nvidia-smi must run with a deadline")
	}

	var st store.State
	if err := set.Persisters[0].Save(&st); err != nil {
		t.Fatal(err)
	}
	if v := st.KV[tempsSeenKey]; tempsSeenKey != "temps.seen" || !strings.Contains(v, "temps/gpu/0") {
		t.Fatalf("saved kv = %v, want the seen sensors under temps.seen", st.KV)
	}
	if err := set.Persisters[0].Restore(st); err != nil {
		t.Fatal(err)
	}
}

// fakeOpenSource answers from a fixed set of open keys and their texts.
type fakeOpenSource map[string]string

func (f fakeOpenSource) Problem(key string) (bool, string) {
	text, ok := f[key]

	return ok, text
}

func TestOpenStateLateBound(t *testing.T) {
	t.Parallel()

	const key = "temps/acpitz/temp1"
	o := newOpenState()
	var open temps.OpenState = o
	if gotOpen, gotText := o.Problem(key); open.IsOpen(key) || gotOpen || gotText != "" {
		t.Fatal("an unbound open state must answer closed")
	}
	o.bind(fakeOpenSource{key: "acpitz 91°C"})
	if !open.IsOpen(key) || open.IsOpen("temps/acpitz/temp2") {
		t.Fatal("a bound open state must answer from its source")
	}
	if gotOpen, gotText := o.Problem(key); !gotOpen || gotText != "acpitz 91°C" {
		t.Fatalf("Problem = %v, %q, want open with its source's text", gotOpen, gotText)
	}

	// newHealth binds the adapter the checks were built with to its runner.
	p := enabledHealthParams(t, &fakePublisher{}, fxtest.NewLifecycle(t))
	p.Open = newOpenState()
	out := newHealth(p)
	p.Open.mu.Lock()
	src := p.Open.source
	p.Open.mu.Unlock()
	if len(out.Workers) != 1 || src == nil || any(src) != any(out.Workers[0]) {
		t.Fatalf("open state bound to %T, want the runner %v", src, out.Workers)
	}
}

type fakeEDAC struct {
	counters []edac.Counters
	err      error
}

func (f fakeEDAC) Read() ([]edac.Counters, error) { return f.counters, f.err }

func TestKernelAdapters(t *testing.T) {
	t.Parallel()

	rec := kmsg.Record{Facility: 0, Level: 3, Seq: 42, Time: 5 * time.Second, Message: "Buffer I/O error on dev sda"}
	want := kernel.Record{Facility: 0, Level: 3, Seq: 42, Time: 5 * time.Second, Message: "Buffer I/O error on dev sda"}
	if got := kernelRecord(rec); got != want {
		t.Fatalf("kernelRecord() = %+v, want %+v", got, want)
	}

	got, err := edacSource{reader: fakeEDAC{counters: []edac.Counters{
		{Controller: "mc0", Correctable: 3, Uncorrectable: 1},
	}}}.Read()
	if err != nil || len(got) != 1 || got[0] != (kernel.Counters{Controller: "mc0", Correctable: 3, Uncorrectable: 1}) {
		t.Fatalf("counters = %+v, %v", got, err)
	}
	if got, err = (edacSource{reader: fakeEDAC{}}).Read(); got != nil || err != nil {
		t.Fatalf("no EDAC = %+v, %v; want nil, nil", got, err)
	}
	readErr := errors.New("read ce_count: permission denied")
	if _, err = (edacSource{reader: fakeEDAC{err: readErr}}).Read(); !errors.Is(err, readErr) {
		t.Fatalf("err = %v", err)
	}

	ctrlErr := errors.New("parse edac counter ce_count")
	got, err = edacSource{reader: fakeEDAC{counters: []edac.Counters{
		{Controller: "mc0", Err: ctrlErr},
		{Controller: "mc1", Correctable: 2},
	}}}.Read()
	if err != nil || len(got) != 2 || !errors.Is(got[0].Err, ctrlErr) || got[1].Err != nil || got[1].Correctable != 2 {
		t.Fatalf("per-controller counters = %+v, %v", got, err)
	}
}

// fakeKmsg delivers its records to the reader callback, then waits for ctx.
type fakeKmsg struct {
	records []kmsg.Record
	err     error
	// delivered, when set, is closed once the records were handed over.
	delivered chan struct{}
}

func (*fakeKmsg) BootID() (string, error) { return "00000000-0000-4000-8000-000000000001", nil }

func (f *fakeKmsg) Run(ctx context.Context, fn func(kmsg.Record)) error {
	if f.err != nil {
		return f.err
	}
	for _, r := range f.records {
		fn(r)
	}
	if f.delivered != nil {
		close(f.delivered)
	}
	<-ctx.Done()

	return nil
}

func TestBuildKernelCheckNamesOOMVictims(t *testing.T) {
	t.Parallel()

	const id = "0123456789abcdef0123456789abcdef"
	names := newContainerNames()
	m := map[string]string{id: "web-app"}
	names.ids.Store(&m)

	log := &fakeKmsg{records: []kmsg.Record{
		{Level: 6, Seq: 7, Message: "oom-kill:constraint=CONSTRAINT_NONE,nodemask=(null),cpuset=/,mems_allowed=0," +
			"global_oom,task_memcg=/system.slice/docker-" + id + ".scope,task=python3,pid=4242,uid=0"},
		{Level: 3, Seq: 8, Message: "Out of memory: Killed process 4242 (python3) total-vm:2000kB, " +
			"anon-rss:2097152kB, file-rss:0kB, shmem-rss:0kB, UID:0 pgtables:8kB oom_score_adj:0"},
	}}
	now := func() time.Time { return time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC) }
	set := buildKernelCheck(&hcfg.Kernel{}, log, fakeEDAC{}, now, nil, zap.NewNop(), names.name)

	stored := `{"cursor":{"boot_id":"00000000-0000-4000-8000-000000000001","seq":6,"seen":true}}`
	if err := set.Persisters[0].Restore(store.State{KV: map[string]string{kernelStateKey: stored}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- set.Background[0].Run(ctx) }()

	var res health.CheckResult
	for range 100 {
		if res = set.Checks[0].Run(t.Context()); len(res.Readings) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done

	want := "Kernel: out of memory, killed python3 (pid 4242) in container web-app, 2 GB"
	if len(res.Readings) != 1 || res.Readings[0].Text != want {
		t.Fatalf("readings = %+v, want %q", res.Readings, want)
	}
}

func TestBuildKernelCheck(t *testing.T) {
	t.Parallel()

	k := &hcfg.Kernel{Interval: hcfg.Duration(time.Minute)}
	log := &fakeKmsg{
		records: []kmsg.Record{{Level: 3, Seq: 7, Message: "Buffer I/O error on dev sda, logical block 0"}},
	}
	now := func() time.Time { return time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC) }
	set := buildKernelCheck(k, log, fakeEDAC{}, now, nil, zap.NewNop(), nil)
	chk := set.Checks[0]
	if chk.Name != "kernel" || chk.Interval != time.Minute || len(set.Background) != 1 ||
		set.Background[0].Key != "kernel/kmsg/read" {
		t.Fatalf("set = %+v", set)
	}

	// The stored cursor of this boot is restored before the reader starts, so
	// the buffered record after it is news.
	stored := `{"cursor":{"boot_id":"00000000-0000-4000-8000-000000000001","seq":6,"seen":true}}`
	if err := set.Persisters[0].Restore(store.State{KV: map[string]string{kernelStateKey: stored}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- set.Background[0].Run(ctx) }()

	var res health.CheckResult
	for range 100 {
		if res = chk.Run(t.Context()); len(res.Readings) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("reader = %v", err)
	}
	if !hasKey(res.Readings, "kernel/storage-io", health.Bad) {
		t.Fatalf("readings = %+v, want the storage error announced", res.Readings)
	}

	var st store.State
	if err := set.Persisters[0].Save(&st); err != nil {
		t.Fatal(err)
	}
	if v := st.KV["kernel.state"]; !strings.Contains(v, `"seq":7`) {
		t.Fatalf("saved kv = %v, want the advanced cursor under kernel.state", st.KV)
	}

	log.err = errors.New("open kmsg: permission denied")
	if err := set.Background[0].Run(t.Context()); !errors.Is(err, log.err) {
		t.Fatalf("reader error = %v", err)
	}
}

func TestRegisterBackgroundRunsUntilStopAndRetriesFailures(t *testing.T) {
	t.Parallel()

	var (
		mu     sync.Mutex
		raised []string
		runs   int
	)
	failing := background{Key: "kernel/kmsg/read", Run: func(context.Context) error {
		mu.Lock()
		defer mu.Unlock()

		runs++

		return errors.New("open kmsg: permission denied")
	}}
	stopped := make(chan struct{})
	blocking := background{Key: "other/run", Run: func(ctx context.Context) error {
		<-ctx.Done()
		close(stopped)

		return nil
	}}
	raise := func(key, text string) {
		mu.Lock()
		defer mu.Unlock()

		raised = append(raised, key+": "+text)
	}

	lc := fxtest.NewLifecycle(t)
	registerBackground(lc, []background{failing, blocking}, raise, time.Millisecond)
	mu.Lock()
	if runs != 0 {
		t.Fatal("nothing may run before start")
	}
	mu.Unlock()

	lc.RequireStart()
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := runs
		mu.Unlock()
		if n >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("runs = %d, want the failing reader retried", n)
		}
		time.Sleep(time.Millisecond)
	}
	lc.RequireStop()
	<-stopped

	mu.Lock()
	defer mu.Unlock()
	if len(raised) < 2 || !strings.HasPrefix(raised[0], "kernel/kmsg/read: ") ||
		!strings.Contains(raised[0], "open kmsg: permission denied") {
		t.Fatalf("raised = %q", raised)
	}
}

func TestRegisterBackgroundWithoutJobs(t *testing.T) {
	t.Parallel()

	lc := fxtest.NewLifecycle(t)
	registerBackground(lc, nil, func(string, string) { t.Error("nothing to raise") }, time.Millisecond)
	lc.RequireStart().RequireStop()
}

func TestKernelCheckWaitsForItsStoredState(t *testing.T) {
	t.Parallel()

	k := &hcfg.Kernel{Interval: hcfg.Duration(time.Minute)}
	log := &fakeKmsg{
		records:   []kmsg.Record{{Level: 3, Seq: 7, Message: "Buffer I/O error on dev sda, logical block 0"}},
		delivered: make(chan struct{}),
	}
	now := func() time.Time { return time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC) }
	set := buildKernelCheck(k, log, fakeEDAC{}, now, nil, zap.NewNop(), nil)
	chk := set.Checks[0]

	// The reader may start before the stored state is restored (a slow store
	// at start): it only buffers.
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() { _ = set.Background[0].Run(ctx) }()
	<-log.delivered

	for range 2 {
		res := chk.Run(t.Context())
		if !errors.Is(res.Err, errKernelStateNotLoaded) || len(res.Readings) != 0 {
			t.Fatalf("run before restore = %+v, want the state required and nothing announced", res)
		}
	}

	stored := `{"cursor":{"boot_id":"00000000-0000-4000-8000-000000000001","seq":6,"seen":true}}`
	if err := set.Persisters[0].Restore(store.State{KV: map[string]string{kernelStateKey: stored}}); err != nil {
		t.Fatal(err)
	}
	res := chk.Run(t.Context())
	if res.Err != nil || len(res.Readings) != 1 || !hasKey(res.Readings, "kernel/storage-io", health.Bad) {
		t.Fatalf("after restore = %+v, want the buffered record announced once", res)
	}
}

func TestKernelCheckRunsAfterAFailedRestore(t *testing.T) {
	t.Parallel()

	set := buildKernelCheck(&hcfg.Kernel{}, &fakeKmsg{}, fakeEDAC{}, time.Now, nil, zap.NewNop(), nil)
	err := set.Persisters[0].Restore(store.State{KV: map[string]string{kernelStateKey: "{not json"}})
	if err == nil {
		t.Fatal("a malformed stored state must be reported")
	}
	if res := set.Checks[0].Run(t.Context()); errors.Is(res.Err, errKernelStateNotLoaded) {
		t.Fatalf("run = %+v; a reported restore failure must not block the check for good", res)
	}
}

// fakeSnapshot serves the one-shot kernel buffer read.
type fakeSnapshot struct {
	fakeKmsg

	snapErr error
}

func (f *fakeSnapshot) Snapshot(_ context.Context, fn func(kmsg.Record)) error {
	for _, r := range f.records {
		fn(r)
	}

	return f.snapErr
}

func TestBuildKernelOneShotPrintsTheCurrentMatchesWithoutBackgroundOrSaving(t *testing.T) {
	t.Parallel()

	log := &fakeSnapshot{
		records: []kmsg.Record{{Level: 3, Seq: 7, Message: "Buffer I/O error on dev sda, logical block 0"}},
	}
	set := buildKernelOneShot(&hcfg.Kernel{}, log, fakeEDAC{}, time.Now, nil)
	if len(set.Background) != 0 || len(set.Checks) != 1 {
		t.Fatalf("set = %+v", set)
	}
	if err := set.Persisters[0].Restore(store.State{}); err != nil {
		t.Fatal(err)
	}
	// No fresh-install suppression: the very first run shows the match.
	res := set.Checks[0].Run(t.Context())
	if res.Err != nil || len(res.Readings) != 1 {
		t.Fatalf("res = %+v", res)
	}
}

func TestBuildKernelOneShotUnreadableKmsgReportsTheError(t *testing.T) {
	t.Parallel()

	denied := errors.New("open kmsg: permission denied")
	set := buildKernelOneShot(&hcfg.Kernel{}, &fakeSnapshot{snapErr: denied}, fakeEDAC{}, time.Now, nil)
	if err := set.Persisters[0].Restore(store.State{}); err != nil {
		t.Fatal(err)
	}
	if res := set.Checks[0].Run(t.Context()); !errors.Is(res.Err, denied) {
		t.Fatalf("err = %v, want the read error", res.Err)
	}
}

func TestKernelChecksOneShotHasNoBackgroundReader(t *testing.T) {
	t.Parallel()

	hs := &healthSetup{Config: &hcfg.Config{Kernel: &hcfg.Kernel{}}, OneShot: true}
	if set := kernelChecks(hs, newContainerNames(), zap.NewNop()); len(set.Checks) != 1 || len(set.Background) != 0 {
		t.Fatalf("one-shot kernel set: %+v", set)
	}
}

// fakeSmartRunner is a hand-written smartctlRunner.
type fakeSmartRunner struct {
	nodes []smartctl.Node
	err   error
	node  string
	wake  bool
}

func (f *fakeSmartRunner) Drives() ([]smartctl.Node, error) { return f.nodes, f.err }

func (f *fakeSmartRunner) Run(_ context.Context, node string, wake bool) ([]byte, int, error) {
	f.node, f.wake = node, wake

	return []byte("{}"), 4, nil
}

func TestSmartctlSourceMapsNodes(t *testing.T) {
	t.Parallel()

	runner := &fakeSmartRunner{nodes: []smartctl.Node{
		{
			Name: "sda",
			Path: "/sys/devices/example/sda",
		},
		{Name: "nvme0n1", Path: "/sys/devices/pci0000:00/0000:00:1d.0/0000:01:00.0/nvme/nvme0/nvme0n1"},
	}}
	src := smartctlSource{runner: runner}
	got, err := src.Drives()
	if err != nil {
		t.Fatal(err)
	}
	want := []disks.Node{
		{
			Name: "sda",
			Path: "/sys/devices/example/sda",
		},
		{Name: "nvme0n1", Path: "/sys/devices/pci0000:00/0000:00:1d.0/0000:01:00.0/nvme/nvme0/nvme0n1"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("Drives() = %+v, want %+v", got, want)
	}

	out, status, err := src.Run(t.Context(), "sda", true)
	if err != nil || status != 4 || string(out) != "{}" || runner.node != "sda" || !runner.wake {
		t.Fatalf("Run = %q, %d, %v; runner saw %q wake=%v", out, status, err, runner.node, runner.wake)
	}

	runner.err = errors.New("boom")
	if _, err = src.Drives(); !errors.Is(err, runner.err) {
		t.Fatalf("Drives() error = %v, want the runner's", err)
	}
}

type fakeJournal struct {
	mu    sync.Mutex
	calls []string
	recs  []journal.Record
	err   error
}

func (f *fakeJournal) Read(_ context.Context, bootID string) ([]journal.Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = append(f.calls, bootID)

	return f.recs, f.err
}

func TestJournalSourceMapsErrors(t *testing.T) {
	t.Parallel()

	boom := errors.New("boom")
	recs := []journal.Record{{Message: "m", Priority: 3, Facility: 0, Usec: 1500000}}
	tests := []struct {
		name     string
		err      error
		want     error
		wantRecs int
	}{
		{"unavailable", fmt.Errorf("run: %w", journal.ErrUnavailable), kernel.ErrNoJournal, 0},
		{"capped", fmt.Errorf("run: %w", journal.ErrCapped), kernel.ErrJournalCapped, 1},
		{"other", boom, boom, 0},
		{"ok", nil, nil, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			src := journalSource{reader: &fakeJournal{recs: recs[:tt.wantRecs], err: tt.err}}
			got, err := src.Read(t.Context(), "b")
			if tt.want == nil && err != nil || tt.want != nil && !errors.Is(err, tt.want) {
				t.Fatalf("err = %v, want %v", err, tt.want)
			}
			if len(got) != tt.wantRecs {
				t.Fatalf("records = %d, want %d", len(got), tt.wantRecs)
			}
			if tt.wantRecs == 1 {
				want := kernel.Record{Facility: 0, Level: 3, Time: 1500 * time.Millisecond, Message: "m"}
				if got[0] != want {
					t.Errorf("record = %+v, want %+v", got[0], want)
				}
			}
		})
	}
}

// TestKernelCheckJournalWiring proves buildKernelCheck hands a non-nil journal reader to the
// check, which reads the stored (previous) boot's tail on its first run, and that a nil reader
// makes no journal call.
func TestKernelCheckJournalWiring(t *testing.T) {
	t.Parallel()

	const oldBoot = "00000000-0000-4000-8000-0000000000aa"
	stored := `{"cursor":{"boot_id":"` + oldBoot + `","seq":6,"seen":true}}`
	tests := []struct {
		name   string
		reader func(*fakeJournal) journalReader
		want   []string
	}{
		{"reader", func(f *fakeJournal) journalReader { return f }, []string{oldBoot}},
		{"nil reader", func(*fakeJournal) journalReader { return nil }, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fj := &fakeJournal{}
			set := buildKernelCheck(&hcfg.Kernel{}, &fakeKmsg{}, fakeEDAC{}, time.Now, tt.reader(fj), zap.NewNop(), nil)
			state := store.State{KV: map[string]string{kernelStateKey: stored}}
			if err := set.Persisters[0].Restore(state); err != nil {
				t.Fatal(err)
			}
			set.Checks[0].Run(t.Context())

			fj.mu.Lock()
			defer fj.mu.Unlock()
			if !slices.Equal(fj.calls, tt.want) {
				t.Fatalf("journal calls = %v, want %v", fj.calls, tt.want)
			}
		})
	}
}

func TestBuildDisksCheckLogsTheUpgradeRetire(t *testing.T) {
	t.Parallel()

	core, logs := observer.New(zapcore.WarnLevel)
	d := &hcfg.Disks{Interval: hcfg.Duration(time.Hour), WearPercent: 80}
	now := func() time.Time { return time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC) }
	set := buildDisksCheck(d, nil, fakeSmart{}, now, zap.New(core), false)
	err := set.Persisters[0].Restore(store.State{Baselines: []store.Baseline{{
		Serial: "TEST0000000009", Data: []byte(`{"node":"sdb","transport":"usb"}`),
	}}})
	if err != nil {
		t.Fatal(err)
	}
	set.Checks[0].Run(t.Context())
	if got := logs.All(); len(got) != 1 ||
		!strings.Contains(got[0].Message, "drive TEST0000000009 on a usb transport") {
		t.Fatalf("log = %+v, want the retire line through the given logger", got)
	}
}

// sleepySmart is one drive that is read once and then sleeps; it records
// forced reads.
type sleepySmart struct {
	mu     sync.Mutex
	asleep bool
	wakes  int
}

func (*sleepySmart) Drives() ([]disks.Node, error) { return fakeSmart{}.Drives() }

func (s *sleepySmart) Run(_ context.Context, _ string, wake bool) ([]byte, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if wake {
		s.wakes++
	}
	if s.asleep && !wake {
		return []byte("{}"), disks.StandbyStatus, nil
	}

	return []byte(nvmeHot), 0, nil
}

// TestBuildDisksCheckForcedReadOnlyInTheDaemon: the daemon's disks check
// force-reads a drive asleep for force_read_after; the one-shot CLI's never
// does and reports the drive asleep through its status.
func TestBuildDisksCheckForcedReadOnlyInTheDaemon(t *testing.T) {
	t.Parallel()

	for _, oneShot := range []bool{false, true} {
		t.Run(fmt.Sprintf("oneShot=%v", oneShot), func(t *testing.T) {
			t.Parallel()

			clock := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
			src := &sleepySmart{}
			d := &hcfg.Disks{Interval: hcfg.Duration(time.Hour), ForceReadAfter: hcfg.Duration(24 * time.Hour)}
			set := buildDisksCheck(d, nil, src, func() time.Time { return clock }, zap.NewNop(), oneShot)
			if err := set.Persisters[0].Restore(store.State{}); err != nil {
				t.Fatal(err)
			}
			if res := set.Checks[0].Run(t.Context()); res.Err != nil {
				t.Fatalf("first run: %v", res.Err)
			}

			src.asleep = true
			for range 30 {
				clock = clock.Add(time.Hour)
				set.Checks[0].Run(t.Context())
			}
			if oneShot && src.wakes != 0 {
				t.Fatalf("one-shot wakes = %d, want none", src.wakes)
			}
			if !oneShot && src.wakes == 0 {
				t.Fatal("the daemon never force-read the drive")
			}
			st, ok := set.Drives.(driveStatuses)
			if !ok {
				t.Fatal("the drive book has no Status")
			}
			if got := st.Status(); oneShot && (len(got) != 1 || got[0].AsleepSince.IsZero()) {
				t.Fatalf("Status() = %+v, want the drive asleep", got)
			}
		})
	}
}

// TestBuildDisksCheckPerSerialLimit: temperatures.limits_by_serial reaches the
// disks check through the section it is built with.
func TestBuildDisksCheckPerSerialLimit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		limits map[string]int
		want   health.Verdict
	}{
		{"no override: the class limit", nil, health.Bad},
		{"override raises this drive's limit", map[string]int{"TEST0000000001": 150}, health.Good},
		{"override of another serial", map[string]int{"TEST0000000002": 150}, health.Bad},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			d := &hcfg.Disks{Interval: hcfg.Duration(time.Hour), WearPercent: 80}
			tc := &hcfg.Temperatures{Limits: hcfg.Limits{DriveNVMe: 70, DriveSATA: 60}, LimitsBySerial: tt.limits}
			now := func() time.Time { return time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC) }
			set := buildDisksCheck(d, tc, fakeSmart{}, now, zap.NewNop(), false)
			if err := set.Persisters[0].Restore(store.State{}); err != nil {
				t.Fatal(err)
			}
			res := set.Checks[0].Run(t.Context())
			if res.Err != nil || !hasKey(res.Readings, "disks/TEST0000000001/temp", tt.want) {
				t.Fatalf("readings = %+v, err %v; want the temp %v", res.Readings, res.Err, tt.want)
			}
		})
	}
}
