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

// newRigWith is newRig with extra options.
func newRigWith(t *testing.T, opts ...kernel.Option) *rig {
	t.Helper()

	r := newRawWith(t, opts...)
	r.k.Consume(kernel.Record{Message: "boot"})
	if res := r.run(t); len(res.Readings) != 0 || res.Err != nil {
		t.Fatalf("baseline: %+v", res)
	}

	return r
}

// newRaw returns a rig with a fresh, never-run check.
func newRaw(t *testing.T) *rig {
	t.Helper()

	return newRawWith(t)
}

// newRawWith is newRaw with extra options.
func newRawWith(t *testing.T, opts ...kernel.Option) *rig {
	t.Helper()

	r := &rig{
		boot: &fakeBoot{id: "boot-1"},
		edac: &fakeEDAC{},
		clk:  &clock{t: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)},
	}
	opts = append([]kernel.Option{kernel.WithTimeout(50 * time.Millisecond)}, opts...)
	r.k = kernel.New(r.boot, r.edac, health.NewRateMarks(), r.clk.now, opts...)

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
		{"EXT4-fs warning (device sda1): ext4_end_bio:343: I/O error 10 writing to inode 12", "kernel/storage-io"},
		{"XFS (sdb1): metadata I/O error in \"xfs_trans_read_buf_map\" at daddr 0x2", "kernel/storage-io"},
		{"Buffer I/O error on dev looper0, logical block 3", "kernel/storage-io"},
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
		"blk_update_request: I/O error, dev loop0, sector 8",
		"I/O error, dev sr0, sector 8 op 0x0:(READ)",
		"Buffer I/O error on dev zram1, logical block 3",
		"EXT4-fs warning (device loop0): ext4_end_bio:343: I/O error 10 writing to inode 12 starting block 34)",
		"Buffer I/O error on device loop5, logical block 0",
		"XFS (loop1): metadata I/O error in \"xfs_trans_read_buf_map\" at daddr 0x2 len 1 error 5",
		"isofs_fill_super: bread failed, dev=sr0, iso_blknum=16, block=32 I/O error (device sr0)",
		"ata1: SATA link down (SStatus 0 SControl 300)",
		"ata1: SATA link up 6.0 Gbps (SStatus 133 SControl 300)",
		"EXT4-fs (sda1): mounted filesystem",
		"nvme nvme0: pci function 0000:01:00.0",
		"nvme nvme0: Shutdown timeout set to 10 seconds",
	} {
		r.k.Consume(kernel.Record{Level: 6, Seq: uint64(i + 1), Message: m})
	}
	// Even at warning level the routine NVMe line is not an error.
	r.k.Consume(kernel.Record{Level: 4, Seq: 10, Message: "nvme nvme0: Shutdown timeout set to 10 seconds"})
	// Userspace writes (facility != 0) are ignored.
	r.k.Consume(kernel.Record{Facility: 1, Level: 3, Seq: 11, Message: ioErr})
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

	// Silence for more than 24 h: the unreported record is still announced, then a
	// new record starts a new episode.
	r.clk.t = r.clk.t.Add(25 * time.Hour)
	if quiet := r.run(t); len(quiet.Readings) != 1 || !strings.Contains(quiet.Readings[0].Text, "still") {
		t.Fatalf("unreported: %+v", quiet.Readings)
	}
	r.k.Consume(rec(5, ioErr))
	res = r.run(t)
	if len(res.Readings) != 1 || strings.Contains(res.Readings[0].Text, "still") {
		t.Fatalf("new episode: %+v", res.Readings)
	}
}

func TestNVMeAnyLevel(t *testing.T) {
	for _, msg := range []string{
		"nvme nvme0: resetting controller",
		"nvme nvme0: I/O 12 QID 3 timeout, aborting",
		"nvme nvme0: I/O error, dev nvme0n1, sector 8",
	} {
		for level, key := range classifyAt(t, msg) {
			if key != "kernel/nvme" {
				t.Errorf("%q level %d: key = %q", msg, level, key)
			}
		}
	}
}

func TestFirstMatchOrder(t *testing.T) {
	tests := []struct{ msg, key string }{
		{"ata3: hard resetting link after I/O error", "kernel/ata"},
		{"ata3.00: exception Emask 0x0: I/O error", "kernel/ata"},
		{"nvme nvme0: Device not ready: I/O error", "kernel/nvme"},
		{"mce: [Hardware Error]: I/O error", "kernel/machine-check"},
	}
	for _, tc := range tests {
		for level, key := range classifyAt(t, tc.msg) {
			if key != tc.key {
				t.Errorf("%q level %d: key = %q, want %s", tc.msg, level, key, tc.key)
			}
		}
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

func TestStillHappeningWithoutNewRecord(t *testing.T) {
	r := newRig(t)
	start := r.clk.t
	r.k.Consume(rec(1, ioErr))
	r.run(t)

	r.clk.t = start.Add(time.Hour)
	r.k.Consume(rec(2, ioErr))
	r.k.Consume(rec(3, ioErr))
	if res := r.run(t); len(res.Readings) != 0 {
		t.Fatalf("within window: %+v", res.Readings)
	}

	// No further record arrives: the window ends and the two unreported records are announced.
	r.clk.t = start.Add(6 * time.Hour)
	res := r.run(t)
	want := "Kernel: storage I/O errors still happening (2 records since " +
		start.UTC().Format(time.RFC3339) + "). Last: " + ioErr
	if len(res.Readings) != 1 || res.Readings[0].Text != want {
		t.Fatalf("still: %+v, want %q", res.Readings, want)
	}

	// Cleared: nothing more without a new record, and the count is persisted.
	r.clk.t = start.Add(13 * time.Hour)
	if res = r.run(t); len(res.Readings) != 0 {
		t.Fatalf("after still: %+v", res.Readings)
	}
	state, err := r.k.Export()
	if err != nil || !strings.Contains(state, `"unreported":0`) {
		t.Fatalf("state %q, err %v", state, err)
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

func TestCursorUsecFollowsSeq(t *testing.T) {
	r := newRig(t)
	r.k.Consume(kernel.Record{Seq: 4, Time: 1500 * time.Millisecond, Message: "usb 1-1: something"})
	r.k.Consume(kernel.Record{Seq: 5, Time: 3 * time.Second, Message: "usb 1-1: other"})
	r.run(t)
	state, _ := r.k.Export()
	if !strings.Contains(state, `"seq":5`) || !strings.Contains(state, `"usec":3000000`) {
		t.Fatalf("state = %s", state)
	}

	// After a restart in the same boot, the ring buffer replays older records only:
	// the stored position and its time stay.
	r2 := newRig(t)
	if err := r2.k.Restore(state); err != nil {
		t.Fatal(err)
	}
	r2.k.Consume(kernel.Record{Seq: 4, Time: 1500 * time.Millisecond, Message: "usb 1-1: something"})
	r2.run(t)
	state2, _ := r2.k.Export()
	if !strings.Contains(state2, `"seq":5`) || !strings.Contains(state2, `"usec":3000000`) {
		t.Fatalf("state = %s", state2)
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

func TestEDACControllerErrKeepsStored(t *testing.T) {
	r := newRig(t)
	r.edac.ctrls = []kernel.Counters{{Controller: "mc0", Correctable: 1}}
	if res := r.run(t); len(res.Readings) != 0 {
		t.Fatalf("baseline: %+v", res.Readings)
	}
	r.edac.ctrls = []kernel.Counters{{Controller: "mc0", Err: errors.New("unreadable")}}
	if res := r.run(t); len(res.Readings) != 0 {
		t.Fatalf("errored controller judged: %+v", res.Readings)
	}
	// The stored baseline survived: a rise against it is still seen.
	r.edac.ctrls = []kernel.Counters{{Controller: "mc0", Correctable: 3}}
	if res := r.run(t); len(res.Readings) != 1 {
		t.Fatalf("rise after error = %+v", res.Readings)
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

func TestReplayReportsMatchesIgnoringCursorAndBaseline(t *testing.T) {
	boot := &fakeBoot{id: "boot-1"}
	clk := &clock{t: time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)}
	k := kernel.New(boot, &fakeEDAC{}, health.NewRateMarks(), clk.now, kernel.WithReplay())
	// A stored cursor past the records must not hide them.
	if err := k.Restore(`{"Cursor":{"BootID":"boot-1","Seq":100,"Seen":true}}`); err != nil {
		t.Fatal(err)
	}
	k.Consume(rec(5, "Buffer I/O error on dev sda, logical block 0"))
	k.Consume(rec(6, "Buffer I/O error on dev sda, logical block 1"))
	k.Consume(rec(7, "unrelated"))

	res := k.Run(t.Context())
	if res.Err != nil || len(res.Readings) != 1 || !strings.Contains(res.Readings[0].Text, "2 records") {
		t.Fatalf("res = %+v", res)
	}
	after, _ := k.Export()
	if !strings.Contains(after, "100") {
		t.Fatalf("cursor changed: %s", after)
	}
}

// classifyAt feeds msg at every kernel level and returns the key each level yields ("" for none).
func classifyAt(t *testing.T, msg string) []string {
	t.Helper()

	keys := make([]string, 8)
	for level := range keys {
		r := newRig(t)
		r.k.Consume(kernel.Record{Level: level, Seq: 1, Message: msg})
		res := r.run(t)
		if len(res.Readings) > 1 {
			t.Fatalf("%q at level %d: %+v", msg, level, res.Readings)
		}
		if len(res.Readings) == 1 {
			keys[level] = res.Readings[0].Key
		}
	}

	return keys
}

func TestGHESRecord(t *testing.T) {
	for _, msg := range []string{
		"{1}[Hardware Error]: Hardware error from APEI Generic Hardware Error Source: 1",
		"[Hardware Error]: event severity: corrected",
		"mce: [Hardware Error]: CPU 0: Machine Check: 0 Bank 5",
		"mce: CPU0: Machine Check Exception: 4 Bank 2",
		"mce: Machine check events logged",
		"EDAC MC0: Machine check events logged",
	} {
		for level, key := range classifyAt(t, msg) {
			if key != "kernel/machine-check" {
				t.Errorf("%q level %d: key = %q", msg, level, key)
			}
		}
	}
}

func TestIOErrorAnywhere(t *testing.T) {
	for _, msg := range []string{
		ioErr,
		"EXT4-fs error: I/O error while writing superblock",
		"Buffer I/O error on dev sdb1, logical block 7, async page read",
	} {
		for level, key := range classifyAt(t, msg) {
			if key != "kernel/storage-io" {
				t.Errorf("%q level %d: key = %q", msg, level, key)
			}
		}
	}
	for _, msg := range []string{
		"blk_update_request: I/O error, dev loop3, sector 0 op 0x0:(READ)",
		"I/O error, dev sr0, sector 0 op 0x0:(READ)",
		"I/O error, dev fd0, sector 0 op 0x0:(READ)",
		"Buffer I/O error on dev zram0, logical block 1",
	} {
		for level, key := range classifyAt(t, msg) {
			if key != "" {
				t.Errorf("%q level %d: key = %q, want none", msg, level, key)
			}
		}
	}
}

func TestATALinkResetAnyLevel(t *testing.T) {
	for _, msg := range []string{
		"ata3: SError: { PHYRdyChg CommWake }",
		"ata3.00: SError: { UnrecovData }",
		"ata3: hard resetting link",
		"ata3: soft resetting link",
		"ata3.00: resetting link",
		"ata3: COMRESET failed (errno=-16)",
		"ata3: link is slow to respond, please be patient (ready=0)",
		"ata3: limiting SATA link speed to 3.0 Gbps",
		"ata3.00: exception Emask 0x10 SAct 0x0 SErr 0x4050000 action 0xe frozen",
		"ata3.00: failed command: READ FPDMA QUEUED",
	} {
		for level, key := range classifyAt(t, msg) {
			if key != "kernel/ata" {
				t.Errorf("%q level %d: key = %q", msg, level, key)
			}
		}
	}
}

func lostErrors(t *testing.T, res health.CheckResult) []health.CheckError {
	t.Helper()

	var out []health.CheckError
	for _, ce := range health.CheckErrors(res.Err) {
		if ce.Kind == "lost" {
			out = append(out, ce)
		}
	}

	return out
}

func TestLostRecords(t *testing.T) {
	const wantKey = "kernel/kmsg/lost"

	t.Run("sequence gap", func(t *testing.T) {
		r := newRaw(t)
		r.k.Consume(rec(10, "usb 1-1: something"))
		r.k.Consume(rec(11, "usb 1-1: something"))
		r.k.Consume(rec(15, "usb 1-1: something"))
		lost := lostErrors(t, r.run(t))
		if len(lost) != 1 || lost[0].Key("kernel") != wantKey {
			t.Fatalf("lost = %+v", lost)
		}
		if got := lost[0].Err.Error(); got != "3 kernel records were lost before they could be read" {
			t.Errorf("message = %q", got)
		}
		// Reported once: the count restarts after the report.
		if res := r.run(t); res.Err != nil {
			t.Fatalf("second run: %v", res.Err)
		}
	})

	t.Run("contiguous and replayed records lose nothing", func(t *testing.T) {
		r := newRig(t)
		r.k.Consume(rec(1, ioErr))
		r.k.Consume(rec(1, ioErr))
		r.k.Consume(rec(2, ioErr))
		if res := r.run(t); res.Err != nil {
			t.Fatalf("err = %v", res.Err)
		}
	})

	t.Run("first record against the stored cursor", func(t *testing.T) {
		r := newRig(t)
		r.k.Consume(rec(5, ioErr))
		r.run(t)
		state, _ := r.k.Export()

		r2 := newRaw(t)
		if err := r2.k.Restore(state); err != nil {
			t.Fatal(err)
		}
		r2.k.Consume(rec(9, ioErr)) // 6, 7, 8 were lost while the daemon was down
		lost := lostErrors(t, r2.run(t))
		if len(lost) != 1 || !strings.HasPrefix(lost[0].Err.Error(), "3 kernel records") {
			t.Fatalf("lost = %+v", lost)
		}
	})

	t.Run("first record of a new boot is not compared", func(t *testing.T) {
		r := newRig(t)
		r.k.Consume(rec(100, ioErr))
		r.run(t)
		state, _ := r.k.Export()

		r2 := newRaw(t)
		r2.boot.id = "boot-2"
		if err := r2.k.Restore(state); err != nil {
			t.Fatal(err)
		}
		r2.k.Consume(rec(3, ioErr))
		if lost := lostErrors(t, r2.run(t)); len(lost) != 0 {
			t.Fatalf("lost = %+v", lost)
		}
	})

	t.Run("inbox overflow", func(t *testing.T) {
		r := newRig(t)
		const n = 10000 + 7
		for i := uint64(1); i <= n; i++ {
			r.k.Consume(rec(i, ioErr))
		}
		lost := lostErrors(t, r.run(t))
		if len(lost) != 1 || !strings.HasPrefix(lost[0].Err.Error(), "7 kernel records") {
			t.Fatalf("lost = %+v", lost)
		}
	})

	t.Run("reported beside an EDAC error", func(t *testing.T) {
		r := newRig(t)
		r.edac.err = errors.New("boom")
		r.k.Consume(rec(4, "usb 1-1: something"))
		res := r.run(t)
		if len(lostErrors(t, res)) != 1 || !strings.Contains(res.Err.Error(), "boom") {
			t.Fatalf("err = %v", res.Err)
		}
	})
}
