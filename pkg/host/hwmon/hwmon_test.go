package hwmon_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Toshik1978/tgshd/pkg/host/hwmon"
)

func write(t *testing.T, root, chip, file, content string) {
	t.Helper()
	dir := filepath.Join(root, chip)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSensors(t *testing.T) {
	root := t.TempDir()
	write(t, root, "hwmon0", "name", "coretemp\n")
	write(t, root, "hwmon0", "temp1_input", "45000\n")
	write(t, root, "hwmon0", "temp1_label", "Package id 0\n")
	write(t, root, "hwmon0", "temp1_max", "100000\n")
	write(t, root, "hwmon0", "temp1_crit", "105000\n")
	write(t, root, "hwmon0", "temp2_input", "41500\n")
	write(t, root, "hwmon1", "name", "acpitz\n")
	write(t, root, "hwmon1", "temp1_input", "garbage\n")
	write(t, root, "hwmon1", "temp2_input", "30000\n")
	write(t, root, "hwmon2", "temp1_input", "20000\n") // no name file

	got, err := hwmon.New(root).Sensors()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("got %d sensors: %+v", len(got), got)
	}
	s := got[0]
	if s.Chip != "coretemp" || s.Label != "Package id 0" || s.Celsius != 45 {
		t.Errorf("sensor 0: %+v", s)
	}
	if s.Max == nil || *s.Max != 100 || s.Crit == nil || *s.Crit != 105 {
		t.Errorf("limits: %+v", s)
	}
	if got[1].Label != "temp2" || got[1].Celsius != 41.5 || got[1].Max != nil || got[1].Crit != nil {
		t.Errorf("sensor 1: %+v", got[1])
	}
	if got[2].Chip != "acpitz" || got[2].Label != "temp1" || got[2].Err == nil {
		t.Errorf("sensor 2: %+v", got[2])
	}
	if got[3].Chip != "acpitz" || got[3].Label != "temp2" || got[3].Err != nil {
		t.Errorf("sensor 3: %+v", got[3])
	}
	if got[4].Chip != "hwmon2" {
		t.Errorf("sensor 4: %+v", got[4])
	}
}

func TestSensorsMissingRoot(t *testing.T) {
	got, err := hwmon.New(filepath.Join(t.TempDir(), "absent")).Sensors()
	if err != nil || len(got) != 0 {
		t.Fatalf("got %+v, %v", got, err)
	}
}

func TestUnreadableInput(t *testing.T) {
	root := t.TempDir()
	write(t, root, "hwmon0", "name", "acpitz\n")
	write(t, root, "hwmon0", "temp1_input", "garbage\n")
	write(t, root, "hwmon0", "temp1_max", "90000\n")
	write(t, root, "hwmon0", "temp2_input", "\n")
	// A directory in place of the input makes the read fail with an I/O error.
	if err := os.MkdirAll(filepath.Join(root, "hwmon0", "temp3_input"), 0o750); err != nil {
		t.Fatal(err)
	}
	write(t, root, "hwmon0", "temp4_input", "31000\n")

	got, err := hwmon.New(root).Sensors()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("got %d sensors: %+v", len(got), got)
	}
	for i, label := range []string{"temp1", "temp2", "temp3"} {
		if got[i].Label != label || got[i].Err == nil {
			t.Errorf("sensor %d = %+v, want %s with Err", i, got[i], label)
		}
		if got[i].Max != nil || got[i].Crit != nil {
			t.Errorf("sensor %d keeps limits: %+v", i, got[i])
		}
	}
	if got[3].Label != "temp4" || got[3].Err != nil || got[3].Celsius != 31 {
		t.Errorf("sensor 3 = %+v", got[3])
	}
}

func TestSensorDevice(t *testing.T) {
	base := t.TempDir()
	device := filepath.Join(base, "devices", "platform", "coretemp.1", "hwmon", "hwmon3")
	if err := os.MkdirAll(device, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(device, "name"), []byte("coretemp\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(device, "temp1_input"), []byte("40000\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "class", "hwmon")
	if err := os.MkdirAll(root, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(device, filepath.Join(root, "hwmon0")); err != nil {
		t.Fatal(err)
	}

	got, err := hwmon.New(root).Sensors()
	if err != nil || len(got) != 1 {
		t.Fatalf("got %+v, %v", got, err)
	}
	want, err := filepath.EvalSymlinks(device)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Device != want {
		t.Errorf("Device = %q, want %q", got[0].Device, want)
	}
}

// TestSkipsDriveChips: drivetemp and nvme chips are not read at all, so a
// read never wakes a drive; their temperatures come from SMART.
func TestSkipsDriveChips(t *testing.T) {
	root := t.TempDir()
	write(t, root, "hwmon0", "name", "coretemp\n")
	write(t, root, "hwmon0", "temp1_input", "45000\n")
	write(t, root, "hwmon1", "name", "drivetemp\n")
	write(t, root, "hwmon1", "temp1_input", "35000\n")
	write(t, root, "hwmon2", "name", "nvme\n")
	write(t, root, "hwmon2", "temp1_input", "40000\n")

	got, err := hwmon.New(root).Sensors()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Chip != "coretemp" {
		t.Fatalf("got %+v, want only the coretemp sensor", got)
	}
}
