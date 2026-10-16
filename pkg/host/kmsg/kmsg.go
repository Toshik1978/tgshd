// Package kmsg reads kernel log records from /dev/kmsg and the current boot id.
package kmsg

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	defaultKmsgPath   = "/dev/kmsg"
	defaultBootIDPath = "/proc/sys/kernel/random/boot_id"

	// readBufSize exceeds the kernel's maximum record size (8 KiB) as required by /dev/kmsg.
	readBufSize = 16 * 1024

	minHeaderFields = 3
	facilityShift   = 3
	levelMask       = 7
)

// Record is one parsed kernel log record.
type Record struct {
	Facility int
	Level    int
	Seq      uint64
	Time     time.Duration // since boot
	Message  string
}

// Reader reads /dev/kmsg-style records from a configurable path.
type Reader struct {
	path       string
	bootIDPath string
}

// New creates a Reader for the given kmsg device path and boot id file.
func New(path, bootIDPath string) *Reader {
	return &Reader{path: path, bootIDPath: bootIDPath}
}

// NewDefault creates a Reader over the real /dev/kmsg and /proc boot id.
func NewDefault() *Reader {
	return New(defaultKmsgPath, defaultBootIDPath)
}

// BootID returns the current boot id.
func (r *Reader) BootID() (string, error) {
	data, err := os.ReadFile(r.bootIDPath)
	if err != nil {
		return "", fmt.Errorf("read boot id: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}

// Run reads records from the start of the kernel buffer and calls fn for each, until ctx is
// cancelled (returns nil) or the source reaches EOF (returns nil; the real device never does).
// Records overwritten before being read (EPIPE) are skipped and reading continues. Records
// that cannot be parsed are dropped.
func (r *Reader) Run(ctx context.Context, fn func(Record)) error {
	f, err := os.Open(r.path)
	if err != nil {
		return fmt.Errorf("open kmsg: %w", err)
	}
	stop := context.AfterFunc(ctx, func() { _ = f.Close() })
	defer func() {
		stop()
		_ = f.Close()
	}()

	buf := make([]byte, readBufSize)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			for _, rec := range ParseChunk(string(buf[:n])) {
				fn(rec)
			}
		}
		switch {
		case err == nil, errors.Is(err, syscall.EPIPE):
			continue
		case ctx.Err() != nil:
			return nil
		case errors.Is(err, io.EOF):
			return nil
		default:
			return fmt.Errorf("read kmsg: %w", err)
		}
	}
}

// ParseChunk parses every record in text, skipping continuation lines and malformed records.
func ParseChunk(text string) []Record {
	var out []Record
	for line := range strings.SplitSeq(text, "\n") {
		if line == "" || line[0] == ' ' {
			continue
		}
		rec, err := ParseRecord(line)
		if err != nil {
			continue
		}
		out = append(out, rec)
	}

	return out
}

// ParseRecord parses the first line of a record: "prio,seq,usec,flags[,...];message".
// Continuation lines (starting with a space) are ignored.
func ParseRecord(s string) (Record, error) {
	line, _, _ := strings.Cut(s, "\n")
	header, msg, ok := strings.Cut(line, ";")
	if !ok {
		return Record{}, errors.New("kmsg record: missing message separator")
	}
	fields := strings.Split(header, ",")
	if len(fields) < minHeaderFields+1 {
		return Record{}, fmt.Errorf("kmsg record: %d header fields", len(fields))
	}
	prio, err := strconv.Atoi(fields[0])
	if err != nil || prio < 0 {
		return Record{}, fmt.Errorf("kmsg record: bad priority %q", fields[0])
	}
	seq, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return Record{}, fmt.Errorf("kmsg record: bad sequence %q: %w", fields[1], err)
	}
	usec, err := strconv.ParseUint(fields[2], 10, 63)
	if err != nil {
		return Record{}, fmt.Errorf("kmsg record: bad timestamp %q: %w", fields[2], err)
	}

	return Record{
		Facility: prio >> facilityShift,
		Level:    prio & levelMask,
		Seq:      seq,
		Time:     time.Duration(usec) * time.Microsecond,
		Message:  msg,
	}, nil
}
