package sysfs_test

import (
	"os"
	"path/filepath"
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
