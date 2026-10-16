package main

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/fx/fxtest"

	"github.com/Toshik1978/tgshd/pkg/health"
	"github.com/Toshik1978/tgshd/pkg/health/check/disks"
	"github.com/Toshik1978/tgshd/pkg/health/check/kernel"
	"github.com/Toshik1978/tgshd/pkg/health/check/temps"
	hcfg "github.com/Toshik1978/tgshd/pkg/health/config"
	"github.com/Toshik1978/tgshd/pkg/health/store"
	"github.com/Toshik1978/tgshd/pkg/host/edac"
	"github.com/Toshik1978/tgshd/pkg/host/execprobe"
	"github.com/Toshik1978/tgshd/pkg/host/hwmon"
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
"available_spare":100,"available_spare_threshold":10,"percentage_used":1,"media_errors":0,"num_err_log_entries":0}}`

type fakeSmart struct{}

func (fakeSmart) Drives() ([]string, error) { return []string{"nvme0n1"}, nil }

func (fakeSmart) Run(context.Context, string) ([]byte, int, error) { return []byte(nvmeHot), 0, nil }

func hasKey(readings []health.Reading, key string, v health.Verdict) bool {
	return slices.ContainsFunc(readings, func(r health.Reading) bool { return r.Key == key && r.Verdict == v })
}

func TestHardwareChecksFollowTheConfig(t *testing.T) {
	t.Parallel()

	for _, hs := range []*healthSetup{{}, {Config: &hcfg.Config{}}} {
		for name, set := range map[string]checkSet{
			"disks": disksChecks(hs), "temps": tempsChecks(hs), "kernel": kernelChecks(hs),
		} {
			if len(set.Checks)+len(set.Errors)+len(set.Persisters)+len(set.Background) != 0 || set.Drives != nil {
				t.Fatalf("%s without a section: %+v", name, set)
			}
		}
	}

	hs := &healthSetup{Config: &hcfg.Config{
		Disks: &hcfg.Disks{}, Temperatures: &hcfg.Temperatures{}, Kernel: &hcfg.Kernel{},
	}}
	if set := disksChecks(hs); len(set.Checks) != 1 || set.Checks[0].Name != disksCheckName ||
		len(set.Persisters) != 1 || set.Drives == nil {
		t.Fatalf("disks section: %+v", set)
	}
	if set := tempsChecks(hs); len(set.Checks) != 1 || set.Checks[0].Name != tempsCheckName ||
		len(set.Persisters) != 1 {
		t.Fatalf("temperatures section: %+v", set)
	}
	if set := kernelChecks(hs); len(set.Checks) != 1 || set.Checks[0].Name != kernelCheckName ||
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
	set := buildDisksCheck(d, tc, fakeSmart{}, now)
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
	again := buildDisksCheck(d, nil, fakeSmart{}, now)
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

func TestTemperatureSources(t *testing.T) {
	t.Parallel()

	maxC, crit := 90.0, 100.0
	in := []hwmon.Sensor{{Chip: "coretemp", Label: "Package id 0", Celsius: 85, Max: &maxC, Crit: &crit}}
	got, err := hwmonSource{reader: fakeHwmon{sensors: in}}.Sensors()
	if err != nil || len(got) != 1 {
		t.Fatalf("sensors = %+v, %v", got, err)
	}
	if want := (temps.Sensor{Chip: "coretemp", Label: "Package id 0", Celsius: 85, Max: &maxC, Crit: &crit}); got[0] != want {
		t.Fatalf("sensor = %+v, want %+v", got[0], want)
	}
	readErr := errors.New("permission denied")
	if _, err = (hwmonSource{reader: fakeHwmon{err: readErr}}).Sensors(); !errors.Is(err, readErr) {
		t.Fatalf("err = %v", err)
	}

	gpus := &fakeGPUs{gpus: []nvidia.GPU{{Index: 0, Name: "Example GPU", Celsius: 70}}}
	g, err := nvidiaSource{reader: gpus}.GPUs(t.Context())
	if err != nil || len(g) != 1 || g[0] != (temps.GPU{Index: 0, Name: "Example GPU", Celsius: 70}) {
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
	}
	sensors := fakeHwmon{sensors: []hwmon.Sensor{{Chip: "coretemp", Label: "Package id 0", Celsius: 95}}}
	gpus := &fakeGPUs{gpus: []nvidia.GPU{{Index: 0, Name: "Example GPU", Celsius: 60}}}
	set := buildTempsCheck(tc, hwmonSource{reader: sensors}, nvidiaSource{reader: gpus})
	chk := set.Checks[0]
	if chk.Name != "temps" || chk.Interval != time.Minute {
		t.Fatalf("check = %+v", chk)
	}

	res := chk.Run(t.Context())
	if res.Err != nil || !hasKey(res.Readings, "temps/gpu/0", health.Good) || len(res.Readings) != 2 {
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

func TestBuildKernelCheck(t *testing.T) {
	t.Parallel()

	k := &hcfg.Kernel{Interval: hcfg.Duration(time.Minute)}
	log := &fakeKmsg{
		records: []kmsg.Record{{Level: 3, Seq: 7, Message: "Buffer I/O error on dev sda, logical block 0"}},
	}
	now := func() time.Time { return time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC) }
	set := buildKernelCheck(k, log, fakeEDAC{}, now)
	chk := set.Checks[0]
	if chk.Name != "kernel" || chk.Interval != time.Minute || len(set.Background) != 1 ||
		set.Background[0].Key != "kernel/kmsg/read" {
		t.Fatalf("set = %+v", set)
	}

	// The stored cursor of this boot is restored before the reader starts, so
	// the buffered record after it is news.
	stored := `{"cursor":{"boot_id":"00000000-0000-4000-8000-000000000001","seq":5,"seen":true}}`
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
	set := buildKernelCheck(k, log, fakeEDAC{}, now)
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

	stored := `{"cursor":{"boot_id":"00000000-0000-4000-8000-000000000001","seq":5,"seen":true}}`
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

	set := buildKernelCheck(&hcfg.Kernel{}, &fakeKmsg{}, fakeEDAC{}, time.Now)
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
	set := buildKernelOneShot(&hcfg.Kernel{}, log, fakeEDAC{}, time.Now)
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
	set := buildKernelOneShot(&hcfg.Kernel{}, &fakeSnapshot{snapErr: denied}, fakeEDAC{}, time.Now)
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
	if set := kernelChecks(hs); len(set.Checks) != 1 || len(set.Background) != 0 {
		t.Fatalf("one-shot kernel set: %+v", set)
	}
}
