package kernel_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/Toshik1978/tgshd/pkg/health"
	"github.com/Toshik1978/tgshd/pkg/health/check/kernel"
)

// fakeJournal serves the records of a past boot. With block set it waits for
// the context to end and then returns the records read so far with its error.
type fakeJournal struct {
	recs   []kernel.Record
	err    error
	block  bool
	during func() // called inside Read, before it returns
	calls  int
	boot   string
}

func (f *fakeJournal) Read(ctx context.Context, bootID string) ([]kernel.Record, error) {
	f.calls++
	f.boot = bootID
	if f.during != nil {
		f.during()
	}
	if f.block {
		<-ctx.Done()

		return f.recs, fmt.Errorf("read journal: %w", ctx.Err())
	}

	return f.recs, f.err
}

// at returns a record logged at the given kmsg time since boot, in microseconds.
func at(seq, usec uint64, msg string) kernel.Record {
	return kernel.Record{Level: 3, Seq: seq, Time: time.Duration(usec) * time.Microsecond, Message: msg}
}

// journalRec returns a journal record: the journal carries no kmsg sequence number.
func journalRec(usec uint64, msg string) kernel.Record {
	return at(0, usec, msg)
}

const (
	ghes   = "{1}[Hardware Error]: Hardware error from APEI Generic Hardware Error Source: 0"
	mceRec = "mce: [Hardware Error]: CPU 0: Machine Check: 0 Bank 5: example"
	usbRec = "usb 1-1: new high-speed USB device number 2 using example_hcd"
	ataRst = "ata1: hard resetting link"
)

// previousBoot runs a boot-1 check over the records and returns its state.
func previousBoot(t *testing.T, recs ...kernel.Record) string {
	t.Helper()

	r := newRig(t)
	for _, rc := range recs {
		r.k.Consume(rc)
	}
	r.run(t)
	state, err := r.k.Export()
	if err != nil {
		t.Fatal(err)
	}

	return state
}

// nextBoot returns a boot-2 rig restored from state, with the journal and logger.
func nextBoot(t *testing.T, state string, j kernel.JournalSource) (*rig, *observer.ObservedLogs) {
	t.Helper()

	core, logs := observer.New(zapcore.DebugLevel)
	r := newRigWith(t, kernel.WithJournal(j), kernel.WithLogger(zap.New(core)))
	r.boot.id = "boot-2"
	if err := r.k.Restore(state); err != nil {
		t.Fatal(err)
	}

	return r, logs
}

func boottailErrors(res health.CheckResult) []health.CheckError {
	var out []health.CheckError
	for _, ce := range health.CheckErrors(res.Err) {
		if ce.Subject == "boottail" {
			out = append(out, ce)
		}
	}

	return out
}

func assertMovedToNewBoot(t *testing.T, r *rig) {
	t.Helper()

	st, err := r.k.Export()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(st, `"boot_id":"boot-2"`) {
		t.Errorf("cursor not moved to the new boot: %s", st)
	}
}

// TestPreviousBootTail proves US3 acceptance 1 and SC-007: a hardware error
// logged in the previous boot after the last processed record is reported by
// the first Run after the reboot.
func TestPreviousBootTail(t *testing.T) {
	state := previousBoot(t,
		at(1, 1000000, usbRec),
		at(2, 2000000, ataRst),
		at(3, 2000000, ioErr), // the cursor: same time as the record before and after it
	)
	if !strings.Contains(state, `"usec":2000000`) {
		t.Fatalf("cursor usec not stored: %s", state)
	}

	j := &fakeJournal{recs: []kernel.Record{
		journalRec(1000000, usbRec),
		journalRec(2000000, ataRst),                                 // tie before the cursor: already processed
		journalRec(2000000, ioErr),                                  // the cursor record itself
		journalRec(2000000, ghes),                                   // tie after the cursor: new
		{Facility: 1, Time: 2500 * time.Millisecond, Message: ghes}, // not a kernel record
		journalRec(3000000, usbRec),
		{Level: 0, Time: 3500 * time.Millisecond, Message: mceRec}, // PRIORITY missing reads as 0
	}}
	r, logs := nextBoot(t, state, j)

	res := r.run(t)
	if res.Err != nil {
		t.Fatalf("err = %v", res.Err)
	}
	if j.boot != "boot-1" {
		t.Errorf("journal read boot %q, want boot-1", j.boot)
	}
	want := "Kernel (previous boot): machine check errors found (2 records). Last: " + mceRec
	if len(res.Readings) != 1 || res.Readings[0].Text != want || res.Readings[0].Key != "kernel/machine-check" ||
		res.Readings[0].Kind != health.Event || res.Readings[0].Verdict != health.Bad {
		t.Fatalf("readings = %+v, want one %q", res.Readings, want)
	}
	if logs.Len() != 0 {
		t.Errorf("logs = %+v", logs.All())
	}
	assertMovedToNewBoot(t, r)

	if res := r.run(t); len(res.Readings) != 0 || res.Err != nil || j.calls != 1 {
		t.Fatalf("second run: %+v, journal calls %d", res, j.calls)
	}
}

func TestPreviousBootTailWithoutProcessedRecord(t *testing.T) {
	// A cursor of the previous boot that never saw a record covers no record.
	j := &fakeJournal{recs: []kernel.Record{journalRec(0, ghes), journalRec(10, ioErr)}}
	r, _ := nextBoot(t, `{"cursor":{"boot_id":"boot-1"}}`, j)

	res := r.run(t)
	if len(res.Readings) != 2 || res.Err != nil {
		t.Fatalf("result = %+v", res)
	}
}

func TestNoPersistentJournal(t *testing.T) {
	state := previousBoot(t, at(1, 1000000, usbRec))
	j := &fakeJournal{err: fmt.Errorf("journal source: %w", kernel.ErrNoJournal)}
	r, logs := nextBoot(t, state, j)

	res := r.run(t)
	if len(res.Readings) != 0 || res.Err != nil {
		t.Fatalf("result = %+v, want nothing", res)
	}
	entries := logs.All()
	if len(entries) != 1 ||
		entries[0].Message != "previous boot's kernel log tail could not be checked: no persistent journal" {
		t.Fatalf("logs = %+v", entries)
	}
	assertMovedToNewBoot(t, r)

	r.run(t)
	if j.calls != 1 || logs.Len() != 1 {
		t.Errorf("journal calls = %d, logs = %d, want 1 and 1", j.calls, logs.Len())
	}
}

// TestBootTailReadErrorKeepsRecordsRead proves FR-020: a read failing midway
// (e.g. a decode error) still reports the records read before the failure,
// logs a warning and raises no checker error.
func TestBootTailReadErrorKeepsRecordsRead(t *testing.T) {
	state := previousBoot(t, at(1, 1000000, usbRec))
	j := &fakeJournal{
		recs: []kernel.Record{journalRec(1000000, usbRec), journalRec(2000000, ghes)},
		err:  errors.New("decode journal record: unexpected end of JSON input"),
	}
	r, logs := nextBoot(t, state, j)

	res := r.run(t)
	if res.Err != nil {
		t.Fatalf("err = %v, want no checker error", res.Err)
	}
	if len(res.Readings) != 1 || !strings.HasPrefix(res.Readings[0].Text,
		"Kernel (previous boot): machine check errors found (1 record). Last: ") {
		t.Fatalf("readings = %+v, want the record read before the error reported", res.Readings)
	}
	if logs.Len() != 1 || !strings.Contains(logs.All()[0].Message, "could not be checked") {
		t.Errorf("logs = %+v", logs.All())
	}
	assertMovedToNewBoot(t, r)
}

func TestBootTailWithoutJournalSource(t *testing.T) {
	state := previousBoot(t, at(1, 1000000, usbRec))
	r := newRig(t)
	r.boot.id = "boot-2"
	if err := r.k.Restore(state); err != nil {
		t.Fatal(err)
	}

	if res := r.run(t); len(res.Readings) != 0 || res.Err != nil {
		t.Fatalf("result = %+v", res)
	}
	assertMovedToNewBoot(t, r)
}

func TestBootTailIncomplete(t *testing.T) {
	tests := []struct {
		name    string
		journal *fakeJournal
		ctx     func() (context.Context, context.CancelFunc)
		reason  string
	}{
		{
			name:    "time limit",
			journal: &fakeJournal{recs: []kernel.Record{journalRec(2000000, ghes)}, block: true},
			ctx: func() (context.Context, context.CancelFunc) {
				return context.WithTimeout(context.Background(), 300*time.Millisecond)
			},
			reason: "time limit",
		},
		{
			name: "record cap",
			journal: &fakeJournal{
				recs: []kernel.Record{journalRec(2000000, ghes)},
				err:  fmt.Errorf("journal source: %w", kernel.ErrJournalCapped),
			},
			ctx:    func() (context.Context, context.CancelFunc) { return context.WithCancel(context.Background()) },
			reason: "record cap",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			state := previousBoot(t, at(1, 1000000, usbRec))
			r, _ := nextBoot(t, state, tc.journal)

			ctx, cancel := tc.ctx()
			defer cancel()
			res := r.k.Run(ctx)

			if len(res.Readings) != 1 || !strings.HasPrefix(res.Readings[0].Text,
				"Kernel (previous boot): machine check errors found (1 record). Last: ") {
				t.Fatalf("readings = %+v, want the records read reported", res.Readings)
			}
			errs := boottailErrors(res)
			want := "previous boot's kernel log tail was not read completely: " + tc.reason
			if len(errs) != 1 || errs[0].Kind != "incomplete" ||
				errs[0].Key("kernel") != "kernel/boottail/incomplete" ||
				errs[0].Err.Error() != want {
				t.Fatalf("errors = %+v, want %q", errs, want)
			}
			assertMovedToNewBoot(t, r)

			if res := r.run(t); len(res.Readings) != 0 || res.Err != nil || tc.journal.calls != 1 {
				t.Fatalf("second run: %+v, journal calls %d (the tail must not be retried)", res, tc.journal.calls)
			}
		})
	}
}

func TestBootTailBoundedByMaxCheckTimeout(t *testing.T) {
	state := previousBoot(t, at(1, 1000000, usbRec))
	src := &deadlineSource{}
	r, _ := nextBoot(t, state, src)

	earliest := time.Now().Add(health.MaxCheckTimeout)
	r.run(t)
	latest := time.Now().Add(health.MaxCheckTimeout)
	if src.deadline.Before(earliest) || src.deadline.After(latest) {
		t.Fatalf("read deadline %s, want %s after the read started", src.deadline, health.MaxCheckTimeout)
	}
}

// deadlineSource records the deadline of the context it is read with.
type deadlineSource struct {
	deadline time.Time
}

func (d *deadlineSource) Read(ctx context.Context, _ string) ([]kernel.Record, error) {
	d.deadline, _ = ctx.Deadline()

	return nil, nil
}

func TestBootTailRunCancelledKeepsCursor(t *testing.T) {
	state := previousBoot(t, at(1, 1000000, usbRec))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	j := &fakeJournal{block: true, during: cancel}
	r, _ := nextBoot(t, state, j)

	if res := r.k.Run(ctx); res.Err == nil || len(boottailErrors(res)) != 0 {
		t.Fatalf("result = %+v, want the run's cancellation", res)
	}
	st, _ := r.k.Export()
	if !strings.Contains(st, `"boot_id":"boot-1"`) {
		t.Fatalf("cursor moved although the run was cancelled: %s", st)
	}

	j.block, j.during = false, nil
	r.run(t)
	if j.calls != 2 {
		t.Errorf("journal calls = %d, want the tail read again", j.calls)
	}
	assertMovedToNewBoot(t, r)
}
