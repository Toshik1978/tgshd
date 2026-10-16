package kmsg_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Toshik1978/tgshd/pkg/host/kmsg"
)

func TestParseRecord(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    kmsg.Record
		wantErr bool
	}{
		{
			"generic",
			"6,339,5140900,-;NET: Registered protocol family 10\n",
			kmsg.Record{
				Facility: 0,
				Level:    6,
				Seq:      339,
				Time:     5140900 * time.Microsecond,
				Message:  "NET: Registered protocol family 10",
			},
			false,
		},
		{
			"facility and level",
			"27,12,100,-;blk_update_request: I/O error, dev nvme0n1, sector 0",
			kmsg.Record{
				Facility: 3,
				Level:    3,
				Seq:      12,
				Time:     100 * time.Microsecond,
				Message:  "blk_update_request: I/O error, dev nvme0n1, sector 0",
			},
			false,
		},
		{
			"continuation ignored", "3,7,1,-;sd 0:0:0:0: [sda] failed\n SUBSYSTEM=scsi\n DEVICE=+scsi:0:0:0:0\n",
			kmsg.Record{Level: 3, Seq: 7, Time: time.Microsecond, Message: "sd 0:0:0:0: [sda] failed"},
			false,
		},
		{
			"extra fields and semicolon in message", "4,1,2,-,caller=T1;a;b\n",
			kmsg.Record{Level: 4, Seq: 1, Time: 2 * time.Microsecond, Message: "a;b"},
			false,
		},
		{"no semicolon", "6,1,2,-", kmsg.Record{}, true},
		{"too few fields", "6,1;msg", kmsg.Record{}, true},
		{"bad prio", "x,1,2,-;msg", kmsg.Record{}, true},
		{"bad seq", "6,x,2,-;msg", kmsg.Record{}, true},
		{"bad usec", "6,1,x,-;msg", kmsg.Record{}, true},
		{"empty", "", kmsg.Record{}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := kmsg.ParseRecord(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestBootID(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "boot_id")
	if err := os.WriteFile(p, []byte("11111111-2222-3333-4444-555555555555\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := kmsg.New(filepath.Join(dir, "kmsg"), p).BootID()
	if err != nil || got != "11111111-2222-3333-4444-555555555555" {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := kmsg.New("", filepath.Join(dir, "missing")).BootID(); err == nil {
		t.Fatal("expected error for missing boot id")
	}
}

func TestRunFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "kmsg")
	content := "6,1,10,-;first\n SUBSYSTEM=x\ngarbage line\n3,2,20,-;second\n"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	var got []kmsg.Record
	err := kmsg.New(p, "").Run(context.Background(), func(r kmsg.Record) { got = append(got, r) })
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Message != "first" || got[1].Seq != 2 {
		t.Fatalf("got %+v", got)
	}
}

func TestRunOpenError(t *testing.T) {
	err := kmsg.New(filepath.Join(t.TempDir(), "nope"), "").Run(context.Background(), func(kmsg.Record) {})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestRunCancelFIFO(t *testing.T) {
	p := filepath.Join(t.TempDir(), "kmsg")
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	w, err := os.OpenFile(p, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	ctx, cancel := context.WithCancel(context.Background())
	recs := make(chan kmsg.Record, 4)
	done := make(chan error, 1)
	go func() { done <- kmsg.New(p, "").Run(ctx, func(r kmsg.Record) { recs <- r }) }()

	if _, err := w.WriteString("6,5,1,-;hello\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-recs:
		if r.Message != "hello" {
			t.Fatalf("got %+v", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no record")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run after cancel: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
}

func TestParseChunk(t *testing.T) {
	recs := kmsg.ParseChunk(strings.Join([]string{"6,1,1,-;a", " K=V", "6,2,2,-;b", ""}, "\n"))
	if len(recs) != 2 || recs[1].Message != "b" {
		t.Fatalf("got %+v", recs)
	}
}

func TestSnapshotReadsWhatIsBufferedAndReturns(t *testing.T) {
	p := filepath.Join(t.TempDir(), "kmsg")
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	w, err := os.OpenFile(p, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err = w.WriteString("3,5,1,-;one\n6,6,2,-;two\n"); err != nil {
		t.Fatal(err)
	}

	var got []kmsg.Record
	// The writer stays open and idle: a blocking read would hang here.
	if err = kmsg.New(p, "").Snapshot(context.Background(), func(r kmsg.Record) { got = append(got, r) }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Message != "one" || got[1].Seq != 6 {
		t.Fatalf("got %+v", got)
	}
}

func TestSnapshotPlainFileAndErrors(t *testing.T) {
	p := filepath.Join(t.TempDir(), "kmsg")
	if err := os.WriteFile(p, []byte("4,1,1,-;x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	n := 0
	if err := kmsg.New(p, "").Snapshot(context.Background(), func(kmsg.Record) { n++ }); err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if err := kmsg.New(p+".missing", "").Snapshot(context.Background(), func(kmsg.Record) {}); err == nil {
		t.Fatal("missing source: want an error")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := kmsg.New(p, "").Snapshot(ctx, func(kmsg.Record) {}); err == nil {
		t.Fatal("cancelled: want an error")
	}
}
