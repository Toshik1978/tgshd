package journal

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeJournalctl writes an executable shell script standing in for journalctl.
func fakeJournalctl(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "journalctl")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	return path
}

func TestDecodeJSONLines(t *testing.T) {
	argsFile := filepath.Join(t.TempDir(), "args")
	fixture, err := filepath.Abs("testdata/journal_prev_boot.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	bin := fakeJournalctl(t, `printf '%s\n' "$@" > '`+argsFile+`'`+"\n"+`cat '`+fixture+`'`)

	got, err := New(bin).Read(context.Background(), "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	want := []Record{
		{
			Message:  "EXT4-fs error (device sdz1): ext4_find_entry:1455: inode #2: comm example: reading directory lblock 0",
			Priority: 3,
			Usec:     1000500,
		},
		{Message: "ata9: hard resetting link", Priority: 4, Usec: 2000750},
		{Message: "error \xff!", Priority: 6, Usec: 3001000},
		{Message: "Kernel panic - not syncing: example", Priority: 2, Usec: 4000000},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d records, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("record %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	args, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	wantArgs := "--boot=0123456789abcdef0123456789abcdef\n--dmesg\n--output=json\n" +
		"--output-fields=MESSAGE,PRIORITY,SYSLOG_FACILITY,_SOURCE_MONOTONIC_TIMESTAMP\n--no-pager\n--quiet\n"
	if string(args) != wantArgs {
		t.Errorf("args = %q, want %q", args, wantArgs)
	}
}

func TestUnavailable(t *testing.T) {
	tests := []struct {
		name   string
		script string // empty means the binary is absent
	}{
		{"binary absent", ""},
		{"no journal files", "echo 'No journal files were found.' >&2; exit 1"},
		{
			"boot not available",
			"echo 'Data from the specified boot (-1) is not available: No such boot ID in journal' >&2; exit 1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bin := filepath.Join(t.TempDir(), "missing")
			if tt.script != "" {
				bin = fakeJournalctl(t, tt.script)
			}
			_, err := New(bin).Read(context.Background(), "abc")
			if !errors.Is(err, ErrUnavailable) {
				t.Errorf("err = %v, want ErrUnavailable", err)
			}
		})
	}

	t.Run("other failure is not unavailable", func(t *testing.T) {
		bin := fakeJournalctl(t, "echo 'boom' >&2; exit 1")
		_, err := New(bin).Read(context.Background(), "abc")
		if err == nil || errors.Is(err, ErrUnavailable) {
			t.Errorf("err = %v, want a non-ErrUnavailable error", err)
		}
	})
}

func TestRecordCap(t *testing.T) {
	bin := fakeJournalctl(t, `i=0; while [ $i -lt 10 ]; do
echo '{"PRIORITY":"3","SYSLOG_FACILITY":"0","_SOURCE_MONOTONIC_TIMESTAMP":"'$i'","MESSAGE":"m"}'; i=$((i+1)); done`)
	r := New(bin)
	r.maxRecords = 4

	got, err := r.Read(context.Background(), "abc")
	if !errors.Is(err, ErrCapped) {
		t.Fatalf("err = %v, want ErrCapped", err)
	}
	if len(got) != 4 {
		t.Errorf("got %d records, want 4", len(got))
	}

	if MaxRecords != 200000 {
		t.Errorf("MaxRecords = %d, want 200000", MaxRecords)
	}
}

func TestDecodeErrorStopsJournalctl(t *testing.T) {
	// After the malformed line journalctl keeps its stdout open, as it would
	// while blocked writing more output that nobody reads.
	bin := fakeJournalctl(t,
		`echo '{"PRIORITY":"3","SYSLOG_FACILITY":"0","_SOURCE_MONOTONIC_TIMESTAMP":"1","MESSAGE":"m"}'
echo 'not json'; exec sleep 30`)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	got, err := New(bin).Read(ctx, "abc")
	if err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the decode error", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Read took %v, want journalctl stopped on the decode error", elapsed)
	}
	if len(got) != 1 {
		t.Errorf("got %d records, want the 1 read before the malformed line", len(got))
	}
}

func TestContextCancelled(t *testing.T) {
	bin := fakeJournalctl(t,
		`echo '{"PRIORITY":"3","SYSLOG_FACILITY":"0","_SOURCE_MONOTONIC_TIMESTAMP":"1","MESSAGE":"m"}'; exec sleep 30`)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	got, err := New(bin).Read(ctx, "abc")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	if len(got) != 1 {
		t.Errorf("got %d records, want the 1 read before the deadline", len(got))
	}
}
