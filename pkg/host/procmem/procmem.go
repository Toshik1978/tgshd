// Package procmem reads host memory facts from procfs: /proc/meminfo, the
// memory pressure stall file and per-process RSS with the process cgroup.
package procmem

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
)

const (
	defaultRoot = "/proc"
	kib         = 1024
)

// ErrNoPressure is returned when the pressure stall file is absent (PSI disabled).
var ErrNoPressure = errors.New("memory pressure information is unavailable")

// MemInfo is the /proc/meminfo subset the memory checks need, in bytes.
type MemInfo struct {
	Total, Available, SwapTotal, SwapFree uint64
}

// Pressure holds the memory PSI averages as percentages.
type Pressure struct {
	SomeAvg10, SomeAvg60, FullAvg10, FullAvg60 float64
}

// Process is one process with its resident set size and cgroup path.
type Process struct {
	PID    int
	Name   string
	RSS    uint64 // bytes
	Cgroup string // the "0::<path>" cgroup v2 path, empty when unknown
}

// Reader reads memory facts from a procfs-style directory.
type Reader struct {
	root string
}

// New creates a Reader over the given procfs-style root directory.
func New(root string) *Reader {
	return &Reader{root: root}
}

// NewDefault creates a Reader over the real /proc.
func NewDefault() *Reader {
	return New(defaultRoot)
}

// ReadMemInfo parses MemTotal, MemAvailable, SwapTotal and SwapFree.
func (r *Reader) ReadMemInfo() (MemInfo, error) {
	data, err := os.ReadFile(filepath.Join(r.root, "meminfo"))
	if err != nil {
		return MemInfo{}, fmt.Errorf("read meminfo: %w", err)
	}
	var info MemInfo
	dst := map[string]*uint64{
		"MemTotal":     &info.Total,
		"MemAvailable": &info.Available,
		"SwapTotal":    &info.SwapTotal,
		"SwapFree":     &info.SwapFree,
	}
	for line := range bytes.SplitSeq(data, []byte{'\n'}) {
		key, rest, ok := bytes.Cut(line, []byte{':'})
		if !ok {
			continue
		}
		d, wanted := dst[string(key)]
		if !wanted {
			continue
		}
		kb, err := parseKB(rest)
		if err != nil {
			return MemInfo{}, fmt.Errorf("parse meminfo %s: %w", key, err)
		}
		*d = kb * kib
		delete(dst, string(key))
		if len(dst) == 0 {
			return info, nil
		}
	}
	for key := range dst {
		return MemInfo{}, fmt.Errorf("parse meminfo: %s not found", key)
	}

	return info, nil
}

// ReadPressure parses the some and full avg10/avg60 values of /proc/pressure/memory.
// A missing file yields ErrNoPressure, wrapping the path error.
func (r *Reader) ReadPressure() (Pressure, error) {
	data, err := os.ReadFile(filepath.Join(r.root, "pressure", "memory"))
	if errors.Is(err, os.ErrNotExist) {
		return Pressure{}, fmt.Errorf("%w: %w", ErrNoPressure, err)
	}
	if err != nil {
		return Pressure{}, fmt.Errorf("read memory pressure: %w", err)
	}
	var p Pressure
	var haveSome, haveFull bool
	for line := range bytes.SplitSeq(data, []byte{'\n'}) {
		kind, rest, _ := bytes.Cut(line, []byte{' '})
		var a10, a60 *float64
		switch string(kind) {
		case "some":
			a10, a60, haveSome = &p.SomeAvg10, &p.SomeAvg60, true
		case "full":
			a10, a60, haveFull = &p.FullAvg10, &p.FullAvg60, true
		default:
			continue
		}
		if err := parseAverages(rest, a10, a60); err != nil {
			return Pressure{}, fmt.Errorf("parse memory pressure %s: %w", kind, err)
		}
	}
	if !haveSome || !haveFull {
		return Pressure{}, errors.New("parse memory pressure: some or full line missing")
	}

	return p, nil
}

// TopProcesses returns up to n processes with the largest RSS, biggest first.
// Processes that vanish while being read, or have no resident memory, are skipped.
func (r *Reader) TopProcesses(n int) ([]Process, error) {
	entries, err := os.ReadDir(r.root)
	if err != nil {
		return nil, fmt.Errorf("read proc directory: %w", err)
	}
	var procs []Process
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		if p, ok := r.readProcess(pid, e.Name()); ok {
			procs = append(procs, p)
		}
	}
	slices.SortFunc(procs, func(a, b Process) int {
		return cmp.Or(cmp.Compare(b.RSS, a.RSS), cmp.Compare(a.PID, b.PID))
	})

	return procs[:max(0, min(n, len(procs)))], nil
}

func (r *Reader) readProcess(pid int, dir string) (Process, bool) {
	status, err := os.ReadFile(filepath.Join(r.root, dir, "status"))
	if err != nil {
		return Process{}, false
	}
	p := Process{PID: pid}
	var haveRSS bool
	for line := range bytes.SplitSeq(status, []byte{'\n'}) {
		key, rest, ok := bytes.Cut(line, []byte{':'})
		if !ok {
			continue
		}
		switch string(key) {
		case "Name":
			p.Name = string(bytes.TrimSpace(rest))
		case "VmRSS":
			kb, err := parseKB(rest)
			if err != nil {
				return Process{}, false
			}
			p.RSS, haveRSS = kb*kib, true
		}
	}
	if !haveRSS {
		return Process{}, false
	}
	if cg, err := os.ReadFile(filepath.Join(r.root, dir, "cgroup")); err == nil {
		p.Cgroup = cgroupV2Path(cg)
	}

	return p, true
}

func cgroupV2Path(data []byte) string {
	for line := range bytes.SplitSeq(data, []byte{'\n'}) {
		if path, ok := bytes.CutPrefix(line, []byte("0::")); ok {
			return string(bytes.TrimSpace(path))
		}
	}

	return ""
}

// parseKB parses " 1234 kB" into 1234.
func parseKB(b []byte) (uint64, error) {
	fields := bytes.Fields(b)
	if len(fields) == 0 {
		return 0, errors.New("empty value")
	}
	v, err := strconv.ParseUint(string(fields[0]), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("value %q: %w", fields[0], err)
	}

	return v, nil
}

// parseAverages fills avg10 and avg60 from "avg10=1.00 avg60=2.00 ...".
func parseAverages(b []byte, a10, a60 *float64) error {
	var got10, got60 bool
	for field := range bytes.FieldsSeq(b) {
		key, val, ok := bytes.Cut(field, []byte{'='})
		if !ok {
			continue
		}
		var dst *float64
		switch string(key) {
		case "avg10":
			dst, got10 = a10, true
		case "avg60":
			dst, got60 = a60, true
		default:
			continue
		}
		v, err := strconv.ParseFloat(string(val), 64)
		if err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		*dst = v
	}
	if !got10 || !got60 {
		return errors.New("avg10 or avg60 missing")
	}

	return nil
}
