package disks

import "testing"

func TestClassifyTransport(t *testing.T) {
	tests := []struct {
		name, path, want string
	}{
		{"usb", "/sys/devices/pci0000:00/0000:00:14.0/usb2/2-1/2-1:1.0/host6/target6:0:0/6:0:0:0/block/sdb", "usb"},
		{"usb segment", "/sys/devices/pci0000:00/usb/2-1/host6/block/sdb", "usb"},
		{"nvme", "/sys/devices/pci0000:00/0000:00:1d.0/0000:01:00.0/nvme/nvme0/nvme0n1", "nvme"},
		{"sata", "/sys/devices/pci0000:00/0000:00:1f.2/ata1/host0/target0:0:0/0:0:0:0/block/sda", "sata"},
		{
			"sas end device",
			"/sys/devices/pci0000:00/0000:00:03.0/host2/port-2:0/end_device-2:0/target2:0:0/2:0:0:0/block/sdc",
			"sas",
		},
		{"sas port", "/sys/devices/pci0000:00/0000:00:03.0/host2/port-2:1/target2:0:1/2:0:1:0/block/sdd", "sas"},
		{
			"sas expander",
			"/sys/devices/pci0000:00/0000:00:03.0/host2/port-2:0/sas_expander/target2:0:2/block/sde",
			"sas",
		},
		{"port without scsi host", "/sys/devices/example/port-2:0/block/sdf", "other"},
		{"virtio", "/sys/devices/pci0000:00/0000:00:05.0/virtio2/host2/target2:0:0/2:0:0:0/block/sda", "other"},
		{"mmc", "/sys/devices/pci0000:00/0000:00:08.0/mmc_host/mmc0/mmc0:0001/block/mmcblk0", "other"},
		{"empty", "", "other"},
		{"usb as a name part is not a segment", "/sys/devices/example/husband/block/sda", "other"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifyTransport(tt.path); got != tt.want {
				t.Errorf("ClassifyTransport(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}
