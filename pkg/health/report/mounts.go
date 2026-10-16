package report

import (
	"slices"
	"strings"
)

const (
	// mountsNone is shown for a drive with nothing mounted.
	mountsNone = "-"
	// mountsUnknown is shown when the mounts cannot be determined.
	mountsUnknown = "?"
)

// MountEntry is one mounted filesystem: its device number ("major:minor") and
// mount point. It mirrors the host adapter's data without importing it.
type MountEntry struct {
	Dev        string
	MountPoint string
}

// BlockHolder is a device (md, dm) stacked on another block device.
type BlockHolder struct {
	Name string
	Dev  string
}

// BlockPartition is a partition of a block device and the devices stacked on it.
type BlockPartition struct {
	Name    string
	Dev     string
	Holders []BlockHolder
}

// BlockDevice is one block device with its partitions and holders.
type BlockDevice struct {
	Name       string
	Dev        string
	Partitions []BlockPartition
	Holders    []BlockHolder
}

// DriveMounts returns the mount points of the drive node ("sda" or
// "/dev/sda"), its partitions and everything stacked on them (md, dm),
// sorted and joined with ", ". It is "-" when nothing is mounted and "?" when
// readErr is set or the drive is not in the topology.
func DriveMounts(node string, entries []MountEntry, topology []BlockDevice, readErr error) string {
	if readErr != nil {
		return mountsUnknown
	}
	name := strings.TrimPrefix(node, "/dev/")
	byName := make(map[string]BlockDevice, len(topology))
	for _, d := range topology {
		byName[d.Name] = d
	}
	drive, ok := byName[name]
	if !ok {
		return mountsUnknown
	}

	devs := map[string]bool{drive.Dev: true}
	for _, h := range drive.Holders {
		addHolder(devs, h, byName)
	}
	for _, p := range drive.Partitions {
		devs[p.Dev] = true
		for _, h := range p.Holders {
			addHolder(devs, h, byName)
		}
	}

	var points []string
	for _, e := range entries {
		if devs[e.Dev] {
			points = append(points, e.MountPoint)
		}
	}
	if len(points) == 0 {
		return mountsNone
	}
	slices.Sort(points)

	return strings.Join(slices.Compact(points), ", ")
}

// addHolder adds a holder's device and, through its own topology entry, the
// devices stacked on it. A holder already seen ends the walk (cycle guard).
func addHolder(devs map[string]bool, h BlockHolder, byName map[string]BlockDevice) {
	if devs[h.Dev] {
		return
	}
	devs[h.Dev] = true
	for _, next := range byName[h.Name].Holders {
		addHolder(devs, next, byName)
	}
}
