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

// Sensor is one temperature input of a hwmon chip. Max and Crit are nil when the
// chip does not expose them.
type Sensor struct {
	Chip    string
	Label   string
	Celsius float64
	Max     *float64
	Crit    *float64
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

// Sensors returns every readable temperature sensor. A missing root yields no
// sensors and no error; unreadable or malformed inputs are skipped.
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
	if chip == "" {
		chip = filepath.Base(dir)
	}
	inputs, err := filepath.Glob(filepath.Join(dir, "temp*_input"))
	if err != nil {
		return nil
	}
	slices.Sort(inputs)

	var out []Sensor
	for _, in := range inputs {
		prefix := strings.TrimSuffix(in, "_input")
		value, ok := readMilli(in)
		if !ok {
			continue
		}
		label := readString(prefix + "_label")
		if label == "" {
			label = filepath.Base(prefix)
		}
		s := Sensor{Chip: chip, Label: label, Celsius: value}
		if v, ok := readMilli(prefix + "_max"); ok {
			s.Max = &v
		}
		if v, ok := readMilli(prefix + "_crit"); ok {
			s.Crit = &v
		}
		out = append(out, s)
	}

	return out
}

func readString(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func readMilli(path string) (float64, bool) {
	n, err := strconv.ParseInt(readString(path), 10, 64)
	if err != nil {
		return 0, false
	}
	return float64(n) / milli, true
}
