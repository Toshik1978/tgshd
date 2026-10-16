// Package sysfs reads host facts from procfs: uptime and the default gateway.
package sysfs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	defaultUptimePath = "/proc/uptime"
	defaultRoutePath  = "/proc/net/route"

	defaultMountInfoPath = "/proc/self/mountinfo"
	defaultBlockDir      = "/sys/block"

	flagUp      = 0x1
	flagGateway = 0x2
)

// ErrNoDefaultGateway is returned when the routing table has no usable default route.
var ErrNoDefaultGateway = errors.New("no default gateway")

// Reader reads uptime and the default gateway from configurable paths.
type Reader struct {
	uptimePath string
	routePath  string

	mountInfoPath string
	blockDir      string
}

// New creates a Reader using the given /proc/uptime and /proc/net/route style files.
func New(uptimePath, routePath string) *Reader {
	return &Reader{
		uptimePath: uptimePath, routePath: routePath,
		mountInfoPath: defaultMountInfoPath, blockDir: defaultBlockDir,
	}
}

// NewDefault creates a Reader over the real /proc files.
func NewDefault() *Reader {
	return New(defaultUptimePath, defaultRoutePath)
}

// WithTopologyPaths overrides the mountinfo file and the /sys/block style
// directory that MountInfo and BlockTopology read; an empty path keeps the
// current one. It returns the Reader for chaining.
func (r *Reader) WithTopologyPaths(mountInfoPath, blockDir string) *Reader {
	if mountInfoPath != "" {
		r.mountInfoPath = mountInfoPath
	}
	if blockDir != "" {
		r.blockDir = blockDir
	}

	return r
}

// Uptime returns the time since boot.
func (r *Reader) Uptime() (time.Duration, error) {
	data, err := os.ReadFile(r.uptimePath)
	if err != nil {
		return 0, fmt.Errorf("read uptime: %w", err)
	}
	return ParseUptime(string(data))
}

// DefaultGateway returns the IPv4 address of the default gateway.
func (r *Reader) DefaultGateway() (netip.Addr, error) {
	data, err := os.ReadFile(r.routePath)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("read routes: %w", err)
	}
	return ParseDefaultGateway(string(data))
}

// ParseUptime parses the content of /proc/uptime ("<uptime> <idle>").
func ParseUptime(content string) (time.Duration, error) {
	fields := strings.Fields(content)
	if len(fields) == 0 {
		return 0, errors.New("parse uptime: empty input")
	}
	secs, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, fmt.Errorf("parse uptime: %w", err)
	}
	if secs < 0 || math.IsNaN(secs) || math.IsInf(secs, 0) {
		return 0, fmt.Errorf("parse uptime: invalid value %q", fields[0])
	}

	return time.Duration(math.Round(secs*100)) * 10 * time.Millisecond, nil
}

// ParseDefaultGateway parses the content of /proc/net/route and returns the
// gateway of the first up default route (destination 0.0.0.0, gateway flag set).
// Addresses in that file are little-endian hex.
func ParseDefaultGateway(content string) (netip.Addr, error) {
	lines := strings.Split(content, "\n")
	for _, line := range lines[min(1, len(lines)):] {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[1] != "00000000" {
			continue
		}
		flags, err := strconv.ParseUint(fields[3], 16, 32)
		if err != nil {
			return netip.Addr{}, fmt.Errorf("parse route flags %q: %w", fields[3], err)
		}
		if flags&flagUp == 0 || flags&flagGateway == 0 {
			continue
		}
		gw, err := strconv.ParseUint(fields[2], 16, 32)
		if err != nil {
			return netip.Addr{}, fmt.Errorf("parse route gateway %q: %w", fields[2], err)
		}
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], uint32(gw))

		return netip.AddrFrom4(b), nil
	}

	return netip.Addr{}, ErrNoDefaultGateway
}

// DevNum is a device number as the kernel prints it, "major:minor".
type DevNum struct {
	Major, Minor uint32
}

// String returns "major:minor".
func (d DevNum) String() string { return fmt.Sprintf("%d:%d", d.Major, d.Minor) }

// MountEntry is one /proc/self/mountinfo line reduced to the device number
// and the mount point.
type MountEntry struct {
	Dev        DevNum
	MountPoint string
}

// Holder is a device stacked on another (md, dm) as listed in a holders
// directory. Dev is zero when the holder has no readable device number.
type Holder struct {
	Name string
	Dev  DevNum
}

// Partition is a partition of a block device with the devices stacked on it.
type Partition struct {
	Name    string
	Dev     DevNum
	Holders []Holder
}

// BlockDevice is one /sys/block entry: its partitions and the devices
// stacked directly on it.
type BlockDevice struct {
	Name       string
	Dev        DevNum
	Partitions []Partition
	Holders    []Holder
}

// MountInfo returns the mounted filesystems from /proc/self/mountinfo.
func (r *Reader) MountInfo() ([]MountEntry, error) {
	data, err := os.ReadFile(r.mountInfoPath)
	if err != nil {
		return nil, fmt.Errorf("read mountinfo: %w", err)
	}

	return ParseMountInfo(string(data))
}

// BlockTopology returns every block device under /sys/block that has a
// device number, sorted by name, with its partitions and holders. It is raw
// data: nothing is matched against mounts here.
func (r *Reader) BlockTopology() ([]BlockDevice, error) {
	entries, err := os.ReadDir(r.blockDir)
	if err != nil {
		return nil, fmt.Errorf("read block devices: %w", err)
	}
	var out []BlockDevice
	for _, e := range entries {
		dir := filepath.Join(r.blockDir, e.Name())
		dev, err := readDevNum(dir)
		if err != nil {
			continue // not a block device directory
		}
		bd := BlockDevice{Name: e.Name(), Dev: dev, Holders: r.readHolders(dir)}
		subs, err := os.ReadDir(dir)
		if err != nil {
			return nil, fmt.Errorf("read block device %s: %w", e.Name(), err)
		}
		for _, sub := range subs {
			subDir := filepath.Join(dir, sub.Name())
			if _, err := os.Stat(filepath.Join(subDir, "partition")); err != nil {
				continue
			}
			pdev, err := readDevNum(subDir)
			if err != nil {
				continue
			}
			part := Partition{Name: sub.Name(), Dev: pdev, Holders: r.readHolders(subDir)}
			bd.Partitions = append(bd.Partitions, part)
		}
		out = append(out, bd)
	}
	slices.SortFunc(out, func(a, b BlockDevice) int { return strings.Compare(a.Name, b.Name) })

	return out, nil
}

// readHolders lists dir/holders; a holder's device number comes from its own
// top-level /sys/block entry. A missing directory means no holders.
func (r *Reader) readHolders(dir string) []Holder {
	entries, err := os.ReadDir(filepath.Join(dir, "holders"))
	if err != nil {
		return nil
	}
	var out []Holder
	for _, e := range entries {
		h := Holder{Name: e.Name()}
		if dev, err := readDevNum(filepath.Join(r.blockDir, e.Name())); err == nil {
			h.Dev = dev
		}
		out = append(out, h)
	}

	return out
}

func readDevNum(dir string) (DevNum, error) {
	data, err := os.ReadFile(filepath.Join(dir, "dev"))
	if err != nil {
		return DevNum{}, fmt.Errorf("read dev: %w", err)
	}

	return parseDevNum(strings.TrimSpace(string(data)))
}

func parseDevNum(s string) (DevNum, error) {
	majStr, minStr, ok := strings.Cut(s, ":")
	if !ok {
		return DevNum{}, fmt.Errorf("parse device number %q", s)
	}
	major, err := strconv.ParseUint(majStr, 10, 32)
	if err != nil {
		return DevNum{}, fmt.Errorf("parse device number %q: %w", s, err)
	}
	minor, err := strconv.ParseUint(minStr, 10, 32)
	if err != nil {
		return DevNum{}, fmt.Errorf("parse device number %q: %w", s, err)
	}

	return DevNum{Major: uint32(major), Minor: uint32(minor)}, nil
}

// ParseMountInfo parses the content of /proc/self/mountinfo: field 3 is the
// device number and field 5 the mount point, with spaces and other special
// characters escaped as octal (\040).
func ParseMountInfo(content string) ([]MountEntry, error) {
	var out []MountEntry
	for line := range strings.SplitSeq(content, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 5 {
			return nil, fmt.Errorf("parse mountinfo: short line %q", line)
		}
		dev, err := parseDevNum(fields[2])
		if err != nil {
			return nil, fmt.Errorf("parse mountinfo: %w", err)
		}
		out = append(out, MountEntry{Dev: dev, MountPoint: unescapeOctal(fields[4])})
	}

	return out, nil
}

// unescapeOctal decodes the \NNN escapes of mountinfo paths.
func unescapeOctal(s string) string {
	if !strings.Contains(s, "\\") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}

	return b.String()
}
