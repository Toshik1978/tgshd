package report_test

import (
	"errors"
	"testing"

	"github.com/Toshik1978/tgshd/pkg/health/report"
)

func TestDriveMountsPartitions(t *testing.T) {
	topology := []report.BlockDevice{
		{Name: "sda", Dev: "8:0", Partitions: []report.BlockPartition{
			{Name: "sda1", Dev: "8:1"},
			{Name: "sda2", Dev: "8:2"},
		}},
		{Name: "sdb", Dev: "8:16", Partitions: []report.BlockPartition{{Name: "sdb1", Dev: "8:17"}}},
		{Name: "nvme0n1", Dev: "259:0", Partitions: []report.BlockPartition{{Name: "nvme0n1p1", Dev: "259:1"}}},
	}
	entries := []report.MountEntry{
		{Dev: "8:2", MountPoint: "/home"},
		{Dev: "8:1", MountPoint: "/"},
		{Dev: "8:1", MountPoint: "/"}, // the same mount seen twice
		{Dev: "8:17", MountPoint: "/srv/data"},
		{Dev: "259:1", MountPoint: "/mnt/fast"},
		{Dev: "0:21", MountPoint: "/proc"},
	}

	tests := []struct {
		name    string
		node    string
		entries []report.MountEntry
		topo    []report.BlockDevice
		err     error
		want    string
	}{
		{"partitions sorted", "sda", entries, topology, nil, "/, /home"},
		{"dev prefix", "/dev/sda", entries, topology, nil, "/, /home"},
		{"other drive", "sdb", entries, topology, nil, "/srv/data"},
		{"nvme partitions", "nvme0n1", entries, topology, nil, "/mnt/fast"},
		{
			"whole disk mounted", "sdc",
			[]report.MountEntry{{Dev: "8:32", MountPoint: "/mnt/raw"}},
			[]report.BlockDevice{{Name: "sdc", Dev: "8:32"}},
			nil, "/mnt/raw",
		},
		{"nothing mounted", "sda", nil, topology, nil, "-"},
		{"drive not in topology", "sdz", entries, topology, nil, "?"},
		{"read error", "sda", entries, topology, errors.New("denied"), "?"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := report.DriveMounts(tt.node, tt.entries, tt.topo, tt.err); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDriveMountsHolders(t *testing.T) {
	md0 := report.BlockHolder{Name: "md0", Dev: "9:0"}
	topology := []report.BlockDevice{
		{Name: "sda", Dev: "8:0", Partitions: []report.BlockPartition{
			{Name: "sda1", Dev: "8:1", Holders: []report.BlockHolder{md0}},
		}},
		{Name: "sdb", Dev: "8:16", Holders: []report.BlockHolder{{Name: "dm-0", Dev: "253:0"}}},
		// md0 is itself held by an LVM volume.
		{Name: "md0", Dev: "9:0", Holders: []report.BlockHolder{{Name: "dm-1", Dev: "253:1"}}},
		{Name: "dm-0", Dev: "253:0"},
		{Name: "dm-1", Dev: "253:1"},
	}
	entries := []report.MountEntry{
		{Dev: "9:0", MountPoint: "/srv/array"},
		{Dev: "253:0", MountPoint: "/srv/crypt"},
		{Dev: "253:1", MountPoint: "/srv/volume"},
		{Dev: "8:1", MountPoint: "/boot"},
	}

	if got := report.DriveMounts("sda", entries, topology, nil); got != "/boot, /srv/array, /srv/volume" {
		t.Fatalf("sda = %q", got)
	}
	if got := report.DriveMounts("sdb", entries, topology, nil); got != "/srv/crypt" {
		t.Fatalf("sdb = %q", got)
	}

	// A holder cycle must not loop forever.
	cyclic := []report.BlockDevice{
		{Name: "sda", Dev: "8:0", Holders: []report.BlockHolder{{Name: "md0", Dev: "9:0"}}},
		{Name: "md0", Dev: "9:0", Holders: []report.BlockHolder{{Name: "md0", Dev: "9:0"}}},
	}
	if got := report.DriveMounts("sda", entries, cyclic, nil); got != "/srv/array" {
		t.Fatalf("cyclic = %q", got)
	}
}
