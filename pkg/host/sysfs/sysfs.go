// Package sysfs reads host facts from procfs: uptime and the default gateway.
package sysfs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultUptimePath = "/proc/uptime"
	defaultRoutePath  = "/proc/net/route"

	flagUp      = 0x1
	flagGateway = 0x2
)

// ErrNoDefaultGateway is returned when the routing table has no usable default route.
var ErrNoDefaultGateway = errors.New("no default gateway")

// Reader reads uptime and the default gateway from configurable paths.
type Reader struct {
	uptimePath string
	routePath  string
}

// New creates a Reader using the given /proc/uptime and /proc/net/route style files.
func New(uptimePath, routePath string) *Reader {
	return &Reader{uptimePath: uptimePath, routePath: routePath}
}

// NewDefault creates a Reader over the real /proc files.
func NewDefault() *Reader {
	return New(defaultUptimePath, defaultRoutePath)
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
