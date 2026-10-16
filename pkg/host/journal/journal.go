// Package journal reads a boot's kernel messages from the persistent system journal by running
// journalctl.
package journal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const (
	defaultBinary = "journalctl"

	// MaxRecords is the most records Read hands over before it stops with ErrCapped.
	MaxRecords = 200000

	// waitDelay bounds how long Read waits for I/O after journalctl is killed.
	waitDelay = time.Second
	// maxStderr bounds the stderr text kept for error reports.
	maxStderr = 4096
)

var (
	// ErrUnavailable is returned when journalctl is absent or the journal holds no data for
	// the requested boot.
	ErrUnavailable = errors.New("journal unavailable")
	// ErrCapped is returned, together with the records read, when the journal holds more than
	// MaxRecords records for the boot.
	ErrCapped = errors.New("journal record cap reached")
)

// unavailableMarkers lists the journalctl messages that mean there is no journal data to read.
func unavailableMarkers() []string {
	return []string{"No journal files were found", "is not available"}
}

// Record is one kernel journal entry.
type Record struct {
	Message  string
	Priority int
	Facility int
	// Usec is the kmsg time since boot, in microseconds (_SOURCE_MONOTONIC_TIMESTAMP).
	Usec uint64
}

// Reader runs journalctl.
type Reader struct {
	binary     string
	maxRecords int
}

// New creates a Reader over the given journalctl binary.
func New(binary string) *Reader {
	return &Reader{binary: binary, maxRecords: MaxRecords}
}

// NewDefault creates a Reader over the journalctl found in PATH.
func NewDefault() *Reader {
	return New(defaultBinary)
}

// entry is the subset of a journalctl JSON object that Read uses. journald prints numeric
// fields as strings and MESSAGE as an array of numbers when it holds non-UTF-8 bytes.
type entry struct {
	Message   json.RawMessage `json:"MESSAGE"`
	Priority  json.Number     `json:"PRIORITY"`
	Facility  json.Number     `json:"SYSLOG_FACILITY"`
	Timestamp json.Number     `json:"_SOURCE_MONOTONIC_TIMESTAMP"`
}

// Read returns the kernel records of the boot with the given id, oldest first. It stops at
// MaxRecords with ErrCapped and on ctx ending with the context's error; in both cases the
// records read so far are returned too. It has no timer of its own.
func (r *Reader) Read(ctx context.Context, bootID string) ([]Record, error) {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	cmd := exec.CommandContext(runCtx, r.binary, //nolint:gosec // binary is configured, not user input
		"--boot="+bootID, "--dmesg", "--output=json",
		"--output-fields=MESSAGE,PRIORITY,SYSLOG_FACILITY,_SOURCE_MONOTONIC_TIMESTAMP",
		"--no-pager", "--quiet")
	cmd.WaitDelay = waitDelay
	var stderr limitedBuffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("journalctl stdout: %w", err)
	}
	if err := cmd.Start(); err != nil {
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
		}

		return nil, fmt.Errorf("start journalctl: %w", err)
	}

	records, capped, decodeErr := decode(stdout, r.maxRecords)
	if capped || decodeErr != nil {
		// Nobody reads stdout any more: stop journalctl so it cannot block on a full pipe.
		cancel()
	}
	waitErr := cmd.Wait()

	return records, readError(ctx, capped, decodeErr, waitErr, stderr.String())
}

// readError picks the error Read reports, most telling first: the caller's context ending,
// the record cap, malformed output (journalctl was stopped for it), then journalctl's own failure.
func readError(ctx context.Context, capped bool, decodeErr, waitErr error, stderr string) error {
	switch {
	case ctx.Err() != nil:
		return fmt.Errorf("read journal: %w", ctx.Err())
	case capped:
		return ErrCapped
	case decodeErr != nil:
		return fmt.Errorf("decode journal: %w", decodeErr)
	case waitErr != nil:
		return runError(waitErr, stderr)
	}

	return nil
}

// decode reads up to limit records from rd. capped reports that more records followed.
func decode(rd io.Reader, limit int) (records []Record, capped bool, err error) {
	dec := json.NewDecoder(rd)
	dec.UseNumber()
	for {
		var e entry
		if err := dec.Decode(&e); err != nil {
			if errors.Is(err, io.EOF) {
				return records, false, nil
			}

			return records, false, fmt.Errorf("decode record: %w", err)
		}
		if len(records) >= limit {
			return records, true, nil
		}
		records = append(records, e.record())
	}
}

func (e entry) record() Record {
	usec, _ := strconv.ParseUint(e.Timestamp.String(), 10, 64)
	prio, _ := strconv.Atoi(e.Priority.String())
	fac, _ := strconv.Atoi(e.Facility.String())

	return Record{Message: e.message(), Priority: prio, Facility: fac, Usec: usec}
}

// message decodes MESSAGE as a string or as an array of byte values.
func (e entry) message() string {
	var s string
	if json.Unmarshal(e.Message, &s) == nil {
		return s
	}
	var nums []int
	if json.Unmarshal(e.Message, &nums) != nil {
		return ""
	}
	b := make([]byte, len(nums))
	for i, n := range nums {
		b[i] = byte(n & 0xff)
	}

	return string(b)
}

// runError classifies a journalctl failure.
func runError(err error, stderr string) error {
	for _, m := range unavailableMarkers() {
		if strings.Contains(stderr, m) {
			return fmt.Errorf("%w: %s", ErrUnavailable, strings.TrimSpace(stderr))
		}
	}

	return fmt.Errorf("journalctl: %w: %s", err, strings.TrimSpace(stderr))
}

// limitedBuffer keeps the first maxStderr bytes written to it.
type limitedBuffer struct{ buf bytes.Buffer }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := maxStderr - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(room, len(p))])
	}

	return len(p), nil
}

func (b *limitedBuffer) String() string { return b.buf.String() }
