package disks

import (
	"slices"
	"strings"
)

// Transports of a drive, as ClassifyTransport reports them.
const (
	transportNVMe  = "nvme"
	transportSATA  = "sata"
	transportSAS   = "sas"
	transportUSB   = "usb"
	transportOther = "other"
)

// ClassifyTransport judges the transport of a drive from the raw resolved
// sysfs path of its block device (research R5): a path segment usb (usb2, ...)
// is usb; nvme, nvme-subsystem or nvme-subsysN (native multipath) is nvme;
// ata (ata1, ...) is sata; end_device-, sas_ or port- under a SCSI host
// segment (host6, ...) is sas; anything else (virtio, MMC, FireWire,
// Thunderbolt bridges) is other.
func ClassifyTransport(path string) string {
	segs := strings.Split(path, "/")
	switch {
	case slices.ContainsFunc(segs, func(s string) bool { return numbered(s, "usb") }):
		return transportUSB
	case slices.ContainsFunc(segs, nvmeSegment):
		return transportNVMe
	case slices.ContainsFunc(segs, func(s string) bool { return numbered(s, "ata") }):
		return transportSATA
	case slices.ContainsFunc(segs, func(s string) bool { return numbered(s, "host") && s != "host" }) &&
		slices.ContainsFunc(segs, sasSegment):
		return transportSAS
	}

	return transportOther
}

// numbered reports whether seg is name, optionally followed by digits.
func numbered(seg, name string) bool {
	rest, ok := strings.CutPrefix(seg, name)
	if !ok {
		return false
	}

	return strings.Trim(rest, "0123456789") == ""
}

// nvmeSegment reports whether seg is an NVMe controller class directory
// (nvme) or, for a namespace behind native multipath, which resolves under
// /sys/devices/virtual, its subsystem (nvme-subsystem, nvme-subsysN).
func nvmeSegment(seg string) bool {
	return seg == "nvme" || seg == "nvme-subsystem" || numbered(seg, "nvme-subsys")
}

// sasSegment reports whether seg is a SAS port, end device or expander.
func sasSegment(seg string) bool {
	return strings.HasPrefix(seg, "end_device-") || strings.HasPrefix(seg, "sas_") || strings.HasPrefix(seg, "port-")
}

// readable reports whether nodes of the transport are read at all: nvme, sata
// and sas always, usb and other only when disks.include_serials is set.
func (c *Check) readable(transport string) bool {
	switch transport {
	case transportUSB, transportOther:
		return len(c.cfg.IncludeSerials) > 0
	}

	return true
}

// tracked reports whether a drive read from a node of the transport is
// tracked: any drive on nvme, sata or sas, a usb or other one only when its
// serial is listed in disks.include_serials.
func (c *Check) tracked(transport, serial string) bool {
	switch transport {
	case transportUSB, transportOther:
		return slices.Contains(c.cfg.IncludeSerials, serial)
	}

	return true
}

// external reports whether the transport is usb or other, whose drives are
// tracked only when listed in disks.include_serials.
func external(transport string) bool {
	return transport == transportUSB || transport == transportOther
}
