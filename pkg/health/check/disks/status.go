package disks

import (
	"maps"
	"slices"
	"time"
)

// DriveStatus is what the check knows of one drive for the status reports
// (/disks, the one-shot CLI and the weekly summary). It is built from the
// baseline and the values of the drive's last successful read.
type DriveStatus struct {
	Serial string
	Model  string
	Bus    Bus
	// Transport is nvme, sata, sas, usb or other; empty for a baseline stored
	// before it was recorded.
	Transport string
	// Node is the device node the drive was last read from.
	Node string
	// Health is the last read's SMART verdict: ok, failing or unknown.
	Health string
	// LastReadAt is the last successful read.
	LastReadAt time.Time
	// AsleepSince is the first run that found the drive asleep since its last
	// read; zero when it is not.
	AsleepSince time.Time
	// UnreadableSince is the first failed read since the last successful one;
	// zero when the drive is readable.
	UnreadableSince time.Time

	// Temperature, Wear (percent used), Spare and SpareThreshold (the drive's
	// own) are from the last successful read since the daemon started; nil
	// when the drive reported no such value or has not been read since.
	Temperature    *int
	Wear           *int
	Spare          *int
	SpareThreshold *int
	// WearLimit and SpareFloor are the configured limits Wear and Spare are
	// judged against.
	WearLimit  int
	SpareFloor int

	// SelfTestNewest is the newest finished self-test, nil when none is known.
	SelfTestNewest *SelfTestNewest
	// SelfTestNewestAge is how long before the last read the newest finished
	// self-test ran. It is power-on time (the drive's lifetime hours then
	// and now), not calendar time: a drive that was off for a while ages
	// less. Nil when either hour count is unknown (after a restart, until
	// the next read).
	SelfTestNewestAge *time.Duration
	// SelfTestPassObserved is false while no pass has been newly observed
	// since the drive was first seen: a pass in the log adopted at first
	// sighting may be old, so SelfTestPassAge then counts from first sight.
	SelfTestPassObserved bool
	// SelfTestPassAge is the calendar time since the last observed pass, or
	// since the drive was first seen when none was.
	SelfTestPassAge time.Duration
	// SelfTestReadable is false when the drive's self-test log is not
	// recognised, or has not been read yet: it is not judged (FR-012).
	SelfTestReadable bool
	// SelfTestFormat is ok or unsupported; empty until the log has been read
	// (a baseline from before a restart), which is not "unreadable".
	SelfTestFormat SelfTestFormat
}

// liveValues are the readings of a drive's last successful read that the
// baseline does not keep. They live in memory only.
type liveValues struct {
	temperature, wear, spare, spareThreshold *int
	powerOnHours                             *int64
}

// noteLive remembers the values of a successful read; a value the read lacked
// is forgotten, so it is never shown stale.
func (c *Check) noteLive(rep *Report) {
	lv := liveValues{
		temperature: copyInt(rep.Temperature), wear: copyInt(rep.PercentageUsed),
		spare: copyInt(rep.AvailableSpare), spareThreshold: copyInt(rep.SpareThreshold),
	}
	if lv.temperature != nil && !plausibleTemp(*lv.temperature) {
		lv.temperature = nil
	}
	if rep.PowerOnHours != nil {
		h := *rep.PowerOnHours
		lv.powerOnHours = &h
	}
	c.live[rep.Serial] = lv
}

func copyInt(v *int) *int {
	if v == nil {
		return nil
	}
	c := *v

	return &c
}

// Status returns the status of every drive that is not retired, sorted by
// serial. The result shares nothing with the check.
func (c *Check) Status() []DriveStatus {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()
	out := make([]DriveStatus, 0, len(c.bases))
	for _, serial := range slices.Sorted(maps.Keys(c.bases)) {
		b := c.bases[serial]
		if b.retired {
			continue
		}
		live := c.live[serial]
		s := DriveStatus{
			Serial: b.serial, Model: b.model, Bus: Bus(b.bus),
			Transport: b.data.Transport, Node: b.data.Node, Health: b.data.Health,
			LastReadAt:      b.lastSeen,
			AsleepSince:     unixTime(b.data.AsleepSince),
			UnreadableSince: unixTime(b.data.UnreadableSince),
			Temperature:     copyInt(live.temperature), Wear: copyInt(live.wear),
			Spare: copyInt(live.spare), SpareThreshold: copyInt(live.spareThreshold),
			WearLimit: c.cfg.WearPercent, SpareFloor: c.cfg.SpareFloorPercent,
			SelfTestPassAge:      SelfTestAge(b.data.SelfTestOKAt, b.firstSeen, now),
			SelfTestReadable:     b.data.SelfTestFormat == SelfTestFormatOK,
			SelfTestFormat:       b.data.SelfTestFormat,
			SelfTestPassObserved: b.data.SelfTestOKAt > 0,
		}
		if n := b.data.SelfTestNewest; n != nil {
			cp := *n
			s.SelfTestNewest = &cp
			s.SelfTestNewestAge = newestAge(n.Hours, live.powerOnHours, Bus(b.bus) == BusATA)
		}
		out = append(out, s)
	}

	return out
}

// newestAge is the power-on time between a test at hours and the drive's
// current hours; nil when either is unknown or the result is negative. The
// hours of an ATA test (ata) are 16 bits and wrap at 65536, so once the
// drive is past that they are compared modulo 65536.
func newestAge(hours int64, now *int64, ata bool) *time.Duration {
	if hours <= 0 || now == nil {
		return nil
	}
	d := *now - hours
	if ata && *now >= ataSelfTestHoursWrap {
		d %= ataSelfTestHoursWrap
		if d < 0 {
			d += ataSelfTestHoursWrap
		}
	}
	if d < 0 {
		return nil
	}
	age := time.Duration(d) * time.Hour

	return &age
}
