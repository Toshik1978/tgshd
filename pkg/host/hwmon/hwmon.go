// Package hwmon reads temperature sensors from the Linux hwmon sysfs tree.
package hwmon

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

const (
	defaultRoot = "/sys/class/hwmon"
	milli       = 1000
)

// Sensor is one temperature input of a hwmon chip. Device is the resolved hwmon
// device path, which orders the instances of one chip. Max and Crit are nil when
// the chip does not expose them. Err is set when the input exists but cannot be
// read (EIO, ENODATA, garbage); Celsius is then meaningless.
type Sensor struct {
	Chip    string
	Label   string
	Device  string
	Celsius float64
	Max     *float64
	Crit    *float64
	Err     error
}

// Reader walks a hwmon root directory.
type Reader struct {
	root string
}

// New creates a Reader over the given hwmon root (/sys/class/hwmon style).
func New(root string) *Reader {
	return &Reader{root: root}
}

// NewDefault creates a Reader over /sys/class/hwmon.
func NewDefault() *Reader {
	return New(defaultRoot)
}

// Sensors returns every readable temperature sensor except those of drive
// chips (drivetemp, nvme). A missing root yields no sensors and no error; an
// unreadable or malformed input is kept with Err set.
func (r *Reader) Sensors() ([]Sensor, error) {
	chips, err := filepath.Glob(filepath.Join(r.root, "hwmon*"))
	if err != nil {
		return nil, fmt.Errorf("list hwmon chips: %w", err)
	}
	slices.Sort(chips)

	var out []Sensor
	for _, dir := range chips {
		out = append(out, readChip(dir)...)
	}

	return out, nil
}

func readChip(dir string) []Sensor {
	chip := readString(filepath.Join(dir, "name"))
	if isDriveChip(chip) {
		return nil
	}
	if chip == "" {
		chip = filepath.Base(dir)
	}
	device, err := filepath.EvalSymlinks(dir)
	if err != nil {
		device = dir
	}
	inputs, err := filepath.Glob(filepath.Join(dir, "temp*_input"))
	if err != nil {
		return nil
	}
	slices.Sort(inputs)

	var out []Sensor
	for _, in := range inputs {
		prefix := strings.TrimSuffix(in, "_input")
		value, err := readMilli(in)
		label := readString(prefix + "_label")
		if label == "" {
			label = filepath.Base(prefix)
		}
		s := Sensor{Chip: chip, Label: label, Device: device}
		if err != nil {
			s.Err = err
			out = append(out, s)

			continue
		}
		s.Celsius = value
		// An unreadable _max or _crit is treated as absent.
		if v, err := readMilli(prefix + "_max"); err == nil {
			s.Max = &v
		}
		if v, err := readMilli(prefix + "_crit"); err == nil {
			s.Crit = &v
		}
		out = append(out, s)
	}

	return out
}

// isDriveChip reports a drive's own hwmon chip. Its inputs are never read:
// a drivetemp read may reset a drive's spin-down timer, and the disks check
// takes drive temperatures from SMART.
func isDriveChip(chip string) bool {
	return chip == "drivetemp" || chip == "nvme"
}

func readString(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func readMilli(path string) (float64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", filepath.Base(path), err)
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", filepath.Base(path), err)
	}

	return float64(n) / milli, nil
}
