package sysfs_test

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/Toshik1978/tgshd/pkg/host/sysfs"
)

// Addresses in /proc/net/route are little-endian hex: 198.51.100.1 is 016433C6,
// 203.0.113.254 is FE7100CB.
const routeNonDefaultFirst = `Iface	Destination	Gateway	Flags
eth0	0000000A	00000000	0001
eth0	00000000	FE7100CB	0003
`

func TestParseUptime(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"typical", "350735.47 234388.90\n", 350735*time.Second + 470*time.Millisecond, false},
		{"integer", "12 5", 12 * time.Second, false},
		{"empty", "", 0, true},
		{"garbage", "abc def", 0, true},
		{"negative", "-1.0 0", 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := sysfs.ParseUptime(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestParseDefaultGateway(t *testing.T) {
	const hdr = "Iface\tDestination\tGateway\tFlags\n"
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"rfc5737", hdr + "eth0\t00000000\t016433C6\t0003\n", "198.51.100.1", false},
		{"skips non-default", routeNonDefaultFirst, "203.0.113.254", false},
		{"no default", hdr + "eth0\t0000000A\t00000000\t0001\n", "", true},
		{"no gateway flag", hdr + "eth0\t00000000\t016433C6\t0001\n", "", true},
		{"empty", "", "", true},
		{"bad hex", hdr + "eth0\t00000000\tZZ\t0003\n", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := sysfs.ParseDefaultGateway(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if !tc.wantErr && got.String() != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestReaderFromFiles(t *testing.T) {
	dir := t.TempDir()
	up := filepath.Join(dir, "uptime")
	rt := filepath.Join(dir, "route")
	if err := os.WriteFile(up, []byte("90.5 10.0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rt, []byte(routeNonDefaultFirst), 0o600); err != nil {
		t.Fatal(err)
	}
	r := sysfs.New(up, rt)

	d, err := r.Uptime()
	if err != nil || d != 90*time.Second+500*time.Millisecond {
		t.Fatalf("Uptime = %v, %v", d, err)
	}
	gw, err := r.DefaultGateway()
	if err != nil || gw.String() != "203.0.113.254" {
		t.Fatalf("DefaultGateway = %v, %v", gw, err)
	}

	missing := sysfs.New(filepath.Join(dir, "none"), filepath.Join(dir, "none"))
	if _, err = missing.Uptime(); err == nil {
		t.Fatal("expected error for missing uptime file")
	}
	if _, err = missing.DefaultGateway(); err == nil {
		t.Fatal("expected error for missing route file")
	}
}

func TestNewDefault(t *testing.T) {
	if sysfs.NewDefault() == nil {
		t.Fatal("nil reader")
	}
}

const mountInfoFixture = `22 28 0:21 / /proc rw,nosuid shared:13 - proc proc rw
28 1 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw
40 28 9:0 / /srv/data rw,relatime shared:20 - ext4 /dev/md0 rw
41 28 253:0 /sub /mnt/with\040space rw,relatime - ext4 /dev/mapper/vg-lv rw
`

func TestMountInfo(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mountinfo")
	if err := os.WriteFile(path, []byte(mountInfoFixture), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := sysfs.NewDefault().WithTopologyPaths(path, dir).MountInfo()
	if err != nil {
		t.Fatalf("MountInfo: %v", err)
	}
	want := []sysfs.MountEntry{
		{Dev: sysfs.DevNum{Major: 0, Minor: 21}, MountPoint: "/proc"},
		{Dev: sysfs.DevNum{Major: 8, Minor: 1}, MountPoint: "/"},
		{Dev: sysfs.DevNum{Major: 9, Minor: 0}, MountPoint: "/srv/data"},
		{Dev: sysfs.DevNum{Major: 253, Minor: 0}, MountPoint: "/mnt/with space"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}

	if _, err := sysfs.NewDefault().WithTopologyPaths(filepath.Join(dir, "none"), dir).MountInfo(); err == nil {
		t.Fatal("missing mountinfo: want error")
	}
}

func TestParseMountInfoMalformed(t *testing.T) {
	if _, err := sysfs.ParseMountInfo("1 2 notadev / /x rw - ext4 x rw\n"); err == nil {
		t.Fatal("bad device number: want error")
	}
	if _, err := sysfs.ParseMountInfo("1 2 8:1\n"); err == nil {
		t.Fatal("short line: want error")
	}
	if got, err := sysfs.ParseMountInfo("\n"); err != nil || len(got) != 0 {
		t.Fatalf("blank input: got %v, %v", got, err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestBlockTopology(t *testing.T) {
	dir := t.TempDir()
	// sda with partitions sda1 (held by md0) and sda2; md0 is a top-level device.
	writeFile(t, filepath.Join(dir, "sda", "dev"), "8:0\n")
	writeFile(t, filepath.Join(dir, "sda", "sda1", "dev"), "8:1\n")
	writeFile(t, filepath.Join(dir, "sda", "sda1", "partition"), "1\n")
	writeFile(t, filepath.Join(dir, "sda", "sda2", "dev"), "8:2\n")
	writeFile(t, filepath.Join(dir, "sda", "sda2", "partition"), "2\n")
	writeFile(t, filepath.Join(dir, "sda", "queue", "rotational"), "1\n")
	if err := os.MkdirAll(filepath.Join(dir, "sda", "sda1", "holders"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "md0"), filepath.Join(dir, "sda", "sda1", "holders", "md0")); err != nil {
		t.Fatal(err)
	}
	// sdb is held by dm-0 directly.
	writeFile(t, filepath.Join(dir, "sdb", "dev"), "8:16\n")
	if err := os.MkdirAll(filepath.Join(dir, "sdb", "holders"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "dm-0"), filepath.Join(dir, "sdb", "holders", "dm-0")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "md0", "dev"), "9:0\n")
	writeFile(t, filepath.Join(dir, "dm-0", "dev"), "253:0\n")

	got, err := sysfs.NewDefault().WithTopologyPaths("", dir).BlockTopology()
	if err != nil {
		t.Fatalf("BlockTopology: %v", err)
	}
	md0 := sysfs.Holder{Name: "md0", Dev: sysfs.DevNum{Major: 9}}
	dm0 := sysfs.Holder{Name: "dm-0", Dev: sysfs.DevNum{Major: 253}}
	want := []sysfs.BlockDevice{
		{Name: "dm-0", Dev: sysfs.DevNum{Major: 253}},
		{Name: "md0", Dev: sysfs.DevNum{Major: 9}},
		{
			Name: "sda", Dev: sysfs.DevNum{Major: 8},
			Partitions: []sysfs.Partition{
				{Name: "sda1", Dev: sysfs.DevNum{Major: 8, Minor: 1}, Holders: []sysfs.Holder{md0}},
				{Name: "sda2", Dev: sysfs.DevNum{Major: 8, Minor: 2}},
			},
		},
		{Name: "sdb", Dev: sysfs.DevNum{Major: 8, Minor: 16}, Holders: []sysfs.Holder{dm0}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}

	if _, err := sysfs.NewDefault().WithTopologyPaths("", filepath.Join(dir, "none")).BlockTopology(); err == nil {
		t.Fatal("missing block dir: want error")
	}
}
