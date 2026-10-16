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
	if len(got) != 4 {
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
	if got[2].Chip != "acpitz" || got[2].Label != "temp2" {
		t.Errorf("sensor 2: %+v", got[2])
	}
	if got[3].Chip != "hwmon2" {
		t.Errorf("sensor 3: %+v", got[3])
	}
}

func TestSensorsMissingRoot(t *testing.T) {
	got, err := hwmon.New(filepath.Join(t.TempDir(), "absent")).Sensors()
	if err != nil || len(got) != 0 {
		t.Fatalf("got %+v, %v", got, err)
	}
}
