// Package edac reads memory error counters from the EDAC sysfs tree.
package edac

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const defaultRoot = "/sys/devices/system/edac/mc"

// Counters holds the cumulative error counts of one memory controller.
type Counters struct {
	Controller    string // e.g. "mc0"
	Correctable   uint64
	Uncorrectable uint64
}

// Reader reads EDAC counters below a configurable root.
type Reader struct {
	root string
}

// New creates a Reader over the given mc directory.
func New(root string) *Reader {
	return &Reader{root: root}
}

// NewDefault creates a Reader over the real sysfs EDAC tree.
func NewDefault() *Reader {
	return New(defaultRoot)
}

// Read returns the counters of every memory controller, sorted by name. When the EDAC
// directory does not exist (no ECC support) it returns nil without an error.
func (r *Reader) Read() ([]Counters, error) {
	dirs, err := filepath.Glob(filepath.Join(r.root, "mc[0-9]*"))
	if err != nil {
		return nil, fmt.Errorf("list edac controllers: %w", err)
	}
	var out []Counters
	for _, dir := range dirs { // Glob returns lexical order
		ce, err := readCount(filepath.Join(dir, "ce_count"))
		if err != nil {
			return nil, err
		}
		ue, err := readCount(filepath.Join(dir, "ue_count"))
		if err != nil {
			return nil, err
		}
		out = append(out, Counters{Controller: filepath.Base(dir), Correctable: ce, Uncorrectable: ue})
	}

	return out, nil
}

func readCount(path string) (uint64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read edac counter: %w", err)
	}
	v, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse edac counter %s: %w", filepath.Base(path), err)
	}

	return v, nil
}
