package kernel_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Toshik1978/tgshd/pkg/health"
	"github.com/Toshik1978/tgshd/pkg/health/check/kernel"
)

type fakeBoot struct {
	id    string
	err   error
	calls int
}

func (f *fakeBoot) BootID() (string, error) {
	f.calls++

	return f.id, f.err
}

type fakeEDAC struct {
	ctrls []kernel.Counters
	err   error
	block chan struct{}
}

func (f *fakeEDAC) Read() ([]kernel.Counters, error) {
	if f.block != nil {
		<-f.block
	}

	return f.ctrls, f.err
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

type rig struct {
	k    *kernel.Check
	boot *fakeBoot
	edac *fakeEDAC
	clk  *clock
}

// newRig returns a rig whose first drain already happened, so the fresh-install
// baseline is out of the way (seq 0 was seen).
func newRig(t *testing.T) *rig {
	t.Helper()

	r := newRaw(t)
	r.k.Consume(kernel.Record{Message: "boot"})
	if res := r.run(t); len(res.Readings) != 0 || res.Err != nil {
		t.Fatalf("baseline: %+v", res)
	}

	return r
}

// newRaw returns a rig with a fresh, never-run check.
func newRaw(t *testing.T) *rig {
	t.Helper()

	r := &rig{
		boot: &fakeBoot{id: "boot-1"},
		edac: &fakeEDAC{},
		clk:  &clock{t: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)},
	}
	r.k = kernel.New(r.boot, r.edac, health.NewRateMarks(), r.clk.now, kernel.WithTimeout(50*time.Millisecond))

	return r
}

func rec(seq uint64, msg string) kernel.Record {
	return kernel.Record{Level: 3, Seq: seq, Message: msg}
}

func (r *rig) run(t *testing.T) health.CheckResult {
	t.Helper()

	return r.k.Run(context.Background())
}

const ioErr = "blk_update_request: I/O error, dev sda, sector 1000 op 0x0:(READ) flags 0x0 phys_seg 1 prio class 0"

func TestClassification(t *testing.T) {
	tests := []struct {
		msg, key string
	}{
		{ioErr, "kernel/storage-io"},
		{"I/O error, dev sda, sector 8 op 0x1:(WRITE)", "kernel/storage-io"},
		{"critical medium error, dev sda, sector 8 op 0x0:(READ)", "kernel/storage-io"},
		{"critical target error, dev sda, sector 8 op 0x0:(READ)", "kernel/storage-io"},
		{"critical nexus error, dev sda, sector 8 op 0x0:(READ)", "kernel/storage-io"},
		{"timeout error, dev sda, sector 8 op 0x0:(READ)", "kernel/storage-io"},
		{"nvme nvme0: I/O 12 QID 3 timeout, aborting", "kernel/nvme"},
		{"nvme nvme0: resetting controller", "kernel/nvme"},
		{"nvme nvme0: Device not ready; aborting reset", "kernel/nvme"},
		{"nvme nvme0: controller is down; will reset: CSTS=0xffffffff", "kernel/nvme"},
		{"ata1.00: exception Emask 0x0 SAct 0x0 SErr 0x0 action 0x6", "kernel/ata"},
		{"ata1: hard resetting link", "kernel/ata"},
		{"ata2.01: failed command: READ DMA", "kernel/ata"},
		{"mce: [Hardware Error]: CPU 0: Machine Check: 0 Bank 5", "kernel/machine-check"},
		{"CPU3: Core temperature above threshold, cpu clock throttled (total events = 1)", "kernel/thermal-throttle"},
		{"Package temperature above threshold, cpu clock throttled", "kernel/thermal-throttle"},
	}
	for _, tc := range tests {
		t.Run(tc.msg, func(t *testing.T) {
			r := newRig(t)
			r.k.Consume(rec(1, tc.msg))
			res := r.run(t)
			if len(res.Readings) != 1 || res.Readings[0].Key != tc.key {
				t.Fatalf("readings = %+v, want key %s", res.Readings, tc.key)
			}
			rd := res.Readings[0]
			if rd.Kind != health.Event || rd.Verdict != health.Bad || !strings.Contains(rd.Text, tc.msg) {
				t.Errorf("reading = %+v", rd)
			}
		})
	}
}

func TestNonMatching(t *testing.T) {
	r := newRig(t)
	for i, m := range []string{
		"usb 1-1: new high-speed USB device",
		"xblk_update_request: I/O error",
		"EXT4-fs (sda1): mounted filesystem",
		"nvme nvme0: pci function 0000:01:00.0",
		"nvme nvme0: Shutdown timeout set to 10 seconds",
		"ata1: hard resetting link",
	} {
		r.k.Consume(kernel.Record{Level: 6, Seq: uint64(i + 1), Message: m})
	}
	// Even at warning level the routine NVMe line is not an error.
	r.k.Consume(kernel.Record{Level: 4, Seq: 100, Message: "nvme nvme0: Shutdown timeout set to 10 seconds"})
	// Userspace writes (facility != 0) are ignored.
	r.k.Consume(kernel.Record{Facility: 1, Level: 3, Seq: 101, Message: ioErr})
	if res := r.run(t); len(res.Readings) != 0 || res.Err != nil {
		t.Errorf("result = %+v", res)
	}
}

func TestEpisodeLifecycle(t *testing.T) {
	r := newRig(t)

	r.k.Consume(rec(1, ioErr))
	if first := r.run(t); len(first.Readings) != 1 {
		t.Fatalf("start: %+v", first.Readings)
	}

	// More records within 6 h: silent.
	r.clk.t = r.clk.t.Add(time.Hour)
	r.k.Consume(rec(2, ioErr))
	if quiet := r.run(t); len(quiet.Readings) != 0 {
		t.Fatalf("within window: %+v", quiet.Readings)
	}

	// After 6 h since start, still happening is sent once.
	r.clk.t = r.clk.t.Add(5*time.Hour + time.Minute)
	r.k.Consume(rec(3, ioErr))
	res := r.run(t)
	if len(res.Readings) != 1 || !strings.Contains(res.Readings[0].Text, "still") {
		t.Fatalf("still: %+v", res.Readings)
	}
	r.clk.t = r.clk.t.Add(time.Hour)
	r.k.Consume(rec(4, ioErr))
	if quiet := r.run(t); len(quiet.Readings) != 0 {
		t.Fatalf("after still: %+v", quiet.Readings)
	}

	// Silence for more than 24 h, then a new record starts a new episode.
	r.clk.t = r.clk.t.Add(25 * time.Hour)
	if quiet := r.run(t); len(quiet.Readings) != 0 {
		t.Fatalf("quiet: %+v", quiet.Readings)
	}
	r.k.Consume(rec(5, ioErr))
	res = r.run(t)
	if len(res.Readings) != 1 || strings.Contains(res.Readings[0].Text, "still") {
		t.Fatalf("new episode: %+v", res.Readings)
	}
}

func TestLevelGate(t *testing.T) {
	r := newRig(t)
	r.k.Consume(kernel.Record{Level: 6, Seq: 1, Message: "ata1: hard resetting link"})
	r.k.Consume(kernel.Record{Level: 5, Seq: 2, Message: "nvme nvme0: resetting controller"})
	if res := r.run(t); len(res.Readings) != 0 {
		t.Fatalf("info-level lines must not match: %+v", res.Readings)
	}
	r.k.Consume(kernel.Record{Level: 4, Seq: 3, Message: "ata1: hard resetting link"})
	if res := r.run(t); len(res.Readings) != 1 {
		t.Fatalf("warning-level must match: %+v", res.Readings)
	}
}

func TestEpisodeBoundaries(t *testing.T) {
	for _, tc := range []struct {
		gap       time.Duration
		wantStill bool
	}{
		{24 * time.Hour, false},
		{23*time.Hour + 59*time.Minute, true},
	} {
		r := newRig(t)
		r.k.Consume(rec(1, ioErr))
		r.run(t)
		r.clk.t = r.clk.t.Add(tc.gap)
		r.k.Consume(rec(2, ioErr))
		res := r.run(t)
		if len(res.Readings) != 1 {
			t.Fatalf("gap %v: %+v", tc.gap, res.Readings)
		}
		if got := strings.Contains(res.Readings[0].Text, "still"); got != tc.wantStill {
			t.Errorf("gap %v: still = %v, text %q", tc.gap, got, res.Readings[0].Text)
		}
	}
}

func TestFreshInstallBaseline(t *testing.T) {
	r := newRaw(t)
	// Run before any record arrived: nothing to baseline yet.
	if res := r.run(t); len(res.Readings) != 0 || res.Err != nil {
		t.Fatalf("empty: %+v", res)
	}
	// The ring buffer replay is history and is not announced.
	r.k.Consume(rec(1, ioErr))
	r.k.Consume(rec(2, ioErr))
	if res := r.run(t); len(res.Readings) != 0 {
		t.Fatalf("baseline announced history: %+v", res.Readings)
	}
	state, _ := r.k.Export()
	if !strings.Contains(state, `"seq":2`) || !strings.Contains(state, `"boot-1"`) {
		t.Errorf("cursor not committed: %s", state)
	}
	// A genuinely new record is announced.
	r.k.Consume(rec(3, ioErr))
	if res := r.run(t); len(res.Readings) != 1 {
		t.Fatalf("new record: %+v", res.Readings)
	}
}

func TestBootIDUnreadableDoesNotLoseOrDouble(t *testing.T) {
	r := newRig(t)
	state, _ := r.k.Export()

	r2 := newRaw(t)
	if err := r2.k.Restore(state); err != nil {
		t.Fatal(err)
	}
	r2.boot.err = errors.New("boom")
	r2.k.Consume(rec(1, ioErr))
	r2.k.Consume(rec(2, ioErr))
	if res := r2.run(t); res.Err == nil {
		t.Fatal("want error")
	}
	if r2.boot.calls != 1 {
		t.Errorf("BootID calls = %d, want 1 (only Run reads it)", r2.boot.calls)
	}
	r2.boot.err = nil
	res := r2.run(t)
	if len(res.Readings) != 1 || !strings.Contains(res.Readings[0].Text, "2 records") {
		t.Fatalf("readings = %+v", res.Readings)
	}
}

func TestSeqZeroIsProcessed(t *testing.T) {
	r := newRig(t) // seq 0 processed, cursor {Seen, 0}
	r.k.Consume(rec(0, ioErr))
	if res := r.run(t); len(res.Readings) != 0 {
		t.Fatalf("seq 0 replayed: %+v", res.Readings)
	}
	state, _ := r.k.Export()
	r2 := newRaw(t)
	if err := r2.k.Restore(state); err != nil {
		t.Fatal(err)
	}
	r2.k.Consume(rec(0, ioErr))
	r2.k.Consume(rec(1, ioErr))
	// Seq 0 is skipped (already processed), seq 1 is new.
	res := r2.run(t)
	if res.Err != nil || len(res.Readings) != 1 || !strings.Contains(res.Readings[0].Text, "(1 record)") {
		t.Fatalf("after restart: %+v", res)
	}
	st, _ := r2.k.Export()
	if !strings.Contains(st, `"seq":1`) {
		t.Errorf("state = %s", st)
	}
}

func TestSingularRecord(t *testing.T) {
	r := newRig(t)
	r.k.Consume(rec(1, ioErr))
	res := r.run(t)
	if strings.Contains(res.Readings[0].Text, "1 records") {
		t.Errorf("grammar: %q", res.Readings[0].Text)
	}
}

func TestStillRequiresRecords(t *testing.T) {
	r := newRig(t)
	r.k.Consume(rec(1, ioErr))
	r.run(t)
	r.clk.t = r.clk.t.Add(7 * time.Hour)
	if res := r.run(t); len(res.Readings) != 0 {
		t.Fatalf("no records, no message: %+v", res.Readings)
	}
}

func TestSeqCursorSkipsSeen(t *testing.T) {
	r := newRig(t)
	r.k.Consume(rec(5, ioErr))
	r.k.Consume(rec(5, ioErr))
	r.k.Consume(rec(4, ioErr))
	res := r.run(t)
	if len(res.Readings) != 1 || !strings.Contains(res.Readings[0].Text, "1 record") {
		t.Fatalf("readings = %+v", res.Readings)
	}
}

func TestRestartSameBoot(t *testing.T) {
	r := newRig(t)
	r.k.Consume(rec(1, ioErr))
	r.k.Consume(rec(2, "usb 1-1: something"))
	r.run(t)
	state, err := r.k.Export()
	if err != nil {
		t.Fatal(err)
	}

	r2 := newRig(t)
	r2.clk.t = r.clk.t.Add(time.Minute)
	if err := r2.k.Restore(state); err != nil {
		t.Fatal(err)
	}
	// Replay from the start of the ring buffer plus one new record.
	r2.k.Consume(rec(1, ioErr))
	r2.k.Consume(rec(2, "usb 1-1: something"))
	r2.k.Consume(rec(3, ioErr))
	res := r2.run(t)
	if len(res.Readings) != 0 {
		t.Fatalf("replayed records must be skipped, new one folded into the episode: %+v", res.Readings)
	}
	st, _ := r2.k.Export()
	if !strings.Contains(st, `"seq":3`) {
		t.Errorf("cursor not advanced: %s", st)
	}
}

func TestUncommittedSeqNotPersisted(t *testing.T) {
	r := newRig(t)
	r.k.Consume(rec(1, ioErr))
	r.run(t)
	r.k.Consume(rec(9, ioErr)) // consumed, not yet turned into readings
	state, _ := r.k.Export()
	if !strings.Contains(state, `"seq":1`) {
		t.Errorf("state = %s", state)
	}
}

func TestNewBootResetsCursor(t *testing.T) {
	r := newRig(t)
	r.k.Consume(rec(100, ioErr))
	r.run(t)
	state, _ := r.k.Export()

	r2 := newRig(t)
	r2.boot.id = "boot-2"
	r2.clk.t = r.clk.t.Add(48 * time.Hour)
	if err := r2.k.Restore(state); err != nil {
		t.Fatal(err)
	}
	r2.k.Consume(rec(1, ioErr))
	res := r2.run(t)
	if len(res.Readings) != 1 || strings.Contains(res.Readings[0].Text, "still") {
		t.Fatalf("readings = %+v", res.Readings)
	}
}

func TestBootIDError(t *testing.T) {
	r := newRaw(t)
	r.boot.err = errors.New("boom")
	r.k.Consume(rec(1, ioErr))
	res := r.run(t)
	if res.Err == nil {
		t.Fatal("want error")
	}
}

func TestEDAC(t *testing.T) {
	r := newRig(t)
	r.edac.ctrls = []kernel.Counters{{Controller: "mc0", Correctable: 1, Uncorrectable: 0}, {Controller: "mc1"}}
	if res := r.run(t); len(res.Readings) != 0 {
		t.Fatalf("baseline: %+v", res.Readings)
	}
	r.edac.ctrls = []kernel.Counters{{Controller: "mc0", Correctable: 4, Uncorrectable: 1}, {Controller: "mc1"}}
	res := r.run(t)
	if len(res.Readings) != 2 {
		t.Fatalf("readings = %+v", res.Readings)
	}
	for _, rd := range res.Readings {
		if rd.Kind != health.Event || rd.Verdict != health.Bad {
			t.Errorf("reading = %+v", rd)
		}
	}
	if res.Readings[0].Key != "kernel/edac/mc0/ce" || res.Readings[1].Key != "kernel/edac/mc0/ue" {
		t.Errorf("keys = %+v", res.Readings)
	}
	if res := r.run(t); len(res.Readings) != 0 {
		t.Fatalf("unchanged: %+v", res.Readings)
	}
	// Counter reset (module reload) is a new baseline, not a rise.
	r.edac.ctrls = []kernel.Counters{{Controller: "mc0", Correctable: 0}}
	if res := r.run(t); len(res.Readings) != 0 {
		t.Fatalf("reset: %+v", res.Readings)
	}
}

func TestEDACStatePersisted(t *testing.T) {
	r := newRig(t)
	r.edac.ctrls = []kernel.Counters{{Controller: "mc0", Correctable: 2}}
	r.run(t)
	state, _ := r.k.Export()

	r2 := newRig(t)
	if err := r2.k.Restore(state); err != nil {
		t.Fatal(err)
	}
	r2.edac.ctrls = []kernel.Counters{{Controller: "mc0", Correctable: 3}}
	if res := r2.run(t); len(res.Readings) != 1 {
		t.Fatalf("readings = %+v", res.Readings)
	}
}

func TestEDACError(t *testing.T) {
	r := newRig(t)
	r.edac.err = errors.New("sysfs unreadable")
	r.k.Consume(rec(1, ioErr))
	res := r.run(t)
	if res.Err == nil {
		t.Fatal("want checker error")
	}
	if len(res.Readings) != 1 {
		t.Errorf("kmsg events must not be lost: %+v", res.Readings)
	}
}

func TestEDACTimeout(t *testing.T) {
	r := newRig(t)
	block := make(chan struct{})
	defer close(block)
	r.edac.block = block
	start := time.Now()
	res := r.k.Run(context.Background())
	if res.Err == nil || !strings.Contains(res.Err.Error(), "timed out") {
		t.Fatalf("err = %v, want timeout", res.Err)
	}
	if time.Since(start) > 5*time.Second {
		t.Error("timeout not enforced")
	}
}

func TestCancelledContext(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if res := r.k.Run(ctx); res.Err == nil {
		t.Fatal("want error on cancelled context")
	}
}

func TestRestoreErrors(t *testing.T) {
	r := newRig(t)
	if err := r.k.Restore(""); err != nil {
		t.Errorf("empty: %v", err)
	}
	if err := r.k.Restore("{bad"); err == nil {
		t.Error("want error")
	}
}
