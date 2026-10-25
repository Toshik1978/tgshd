package procmem

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const meminfo = `MemTotal:        8000000 kB
MemFree:          100000 kB
MemAvailable:    2000000 kB
Buffers:           10000 kB
SwapTotal:       4000000 kB
SwapFree:        3000000 kB
`

func TestReadMemInfo(t *testing.T) {
	root := t.TempDir()
	write(t, root, "meminfo", meminfo)
	got, err := New(root).ReadMemInfo()
	if err != nil {
		t.Fatal(err)
	}
	want := MemInfo{Total: 8000000 << 10, Available: 2000000 << 10, SwapTotal: 4000000 << 10, SwapFree: 3000000 << 10}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestReadMemInfoErrors(t *testing.T) {
	tests := map[string]string{
		"missing available": "MemTotal: 1 kB\nSwapTotal: 0 kB\nSwapFree: 0 kB\n",
		"bad number":        "MemTotal: x kB\nMemAvailable: 1 kB\nSwapTotal: 0 kB\nSwapFree: 0 kB\n",
		"empty":             "",
	}
	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			write(t, root, "meminfo", content)
			if _, err := New(root).ReadMemInfo(); err == nil {
				t.Fatal("expected error")
			}
		})
	}
	if _, err := New(t.TempDir()).ReadMemInfo(); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestReadPressure(t *testing.T) {
	root := t.TempDir()
	write(t, root, "pressure/memory",
		"some avg10=1.50 avg60=2.25 avg300=0.10 total=123\nfull avg10=0.50 avg60=0.75 avg300=0.00 total=45\n")
	got, err := New(root).ReadPressure()
	if err != nil {
		t.Fatal(err)
	}
	want := Pressure{SomeAvg10: 1.5, SomeAvg60: 2.25, FullAvg10: 0.5, FullAvg60: 0.75}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestReadPressureMissing(t *testing.T) {
	_, err := New(t.TempDir()).ReadPressure()
	if !errors.Is(err, ErrNoPressure) || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("got %v, want ErrNoPressure wrapping the missing file", err)
	}
}

func TestReadPressureMalformed(t *testing.T) {
	for name, content := range map[string]string{
		"no full":    "some avg10=1.0 avg60=2.0 avg300=0 total=1\n",
		"bad number": "some avg10=a avg60=2.0 total=1\nfull avg10=0 avg60=0 total=1\n",
		"no avg60":   "some avg10=1.0 total=1\nfull avg10=0 avg60=0 total=1\n",
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			write(t, root, "pressure/memory", content)
			_, err := New(root).ReadPressure()
			if err == nil || errors.Is(err, ErrNoPressure) {
				t.Fatalf("got %v, want parse error", err)
			}
		})
	}
}

func addProc(t *testing.T, root, pid, name string, rssKB int, cgroup string) {
	t.Helper()
	status := "Name:\t" + name + "\nState:\tS (sleeping)\n"
	if rssKB >= 0 {
		status += "VmRSS:\t" + strconv.Itoa(rssKB) + " kB\n"
	}
	write(t, root, pid+"/status", status)
	if cgroup != "" {
		write(t, root, pid+"/cgroup", cgroup)
	}
}

func TestTopProcesses(t *testing.T) {
	root := t.TempDir()
	addProc(t, root, "10", "web-app", 5000, "0::/system.slice/web-app.service\n")
	addProc(t, root, "11", "db", 9000, "12:memory:/legacy\n0::/system.slice/db.service\n")
	addProc(t, root, "12", "worker", 100, "")                     // no cgroup file
	addProc(t, root, "13", "kthread", -1, "0::/\n")               // no VmRSS: skipped
	write(t, root, "14/cgroup", "0::/gone.slice\n")               // status missing: vanished
	write(t, root, "self/status", "Name:\tx\nVmRSS:\t99999 kB\n") // non-numeric dir
	addProc(t, root, "15", "backup", 700, "0::/system.slice/backup.service\n")

	got, err := New(root).TopProcesses(3)
	if err != nil {
		t.Fatal(err)
	}
	want := []Process{
		{PID: 11, Name: "db", RSS: 9000 << 10, Cgroup: "/system.slice/db.service"},
		{PID: 10, Name: "web-app", RSS: 5000 << 10, Cgroup: "/system.slice/web-app.service"},
		{PID: 15, Name: "backup", RSS: 700 << 10, Cgroup: "/system.slice/backup.service"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("entry %d: got %+v, want %+v", i, got[i], want[i])
		}
	}

	all, err := New(root).TopProcesses(100)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 || all[3].Name != "worker" || all[3].Cgroup != "" {
		t.Fatalf("unexpected full list: %+v", all)
	}

	none, err := New(root).TopProcesses(0)
	if err != nil || len(none) != 0 {
		t.Fatalf("n=0: got %+v, %v", none, err)
	}
}

func TestTopProcessesMissingRoot(t *testing.T) {
	if _, err := New(filepath.Join(t.TempDir(), "nope")).TopProcesses(3); err == nil {
		t.Fatal("expected error")
	}
}
