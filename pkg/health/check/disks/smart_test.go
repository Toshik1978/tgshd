package disks

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("testdata", name+".json"))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}

	return data
}

func TestDecodeSATAHealthy(t *testing.T) {
	r, err := DecodeSMART(fixture(t, "sata_healthy"), 0)
	if err != nil {
		t.Fatalf("DecodeSMART: %v", err)
	}

	if r.Serial != "TEST0000000001" || r.Model != "Example SATA SSD" || r.Device != "/dev/sda" || r.Bus != BusATA {
		t.Errorf("identity = %+v", r)
	}
	if r.HealthFailed {
		t.Error("HealthFailed = true")
	}
	if r.Temperature == nil || *r.Temperature != 34 {
		t.Errorf("Temperature = %v", r.Temperature)
	}
	if r.PowerOnHours == nil || *r.PowerOnHours != 4200 {
		t.Errorf("PowerOnHours = %v", r.PowerOnHours)
	}
	for _, id := range []int{5, 187, 188, 197, 198, 199} {
		v, ok := r.Counters[ATACounter(id)]
		if !ok || v != 0 {
			t.Errorf("counter %d = %d, %v", id, v, ok)
		}
	}
	if r.PercentageUsed != nil || r.AvailableSpare != nil || r.CriticalWarning != nil {
		t.Error("NVMe-only fields set on SATA drive")
	}
	if r.SelfTest == nil || r.SelfTest.Failed || r.SelfTest.Description != "Completed without error" {
		t.Errorf("SelfTest = %+v", r.SelfTest)
	}
	if r.SelfTest != nil && (r.SelfTest.PowerOnHours == nil || *r.SelfTest.PowerOnHours != 4190) {
		t.Errorf("SelfTest hours = %v", r.SelfTest.PowerOnHours)
	}
	if r.DataWrittenBytes == nil || *r.DataWrittenBytes != 1000000*512 {
		t.Errorf("DataWrittenBytes = %v", r.DataWrittenBytes)
	}
}

func TestDecodeUnfinishedSelfTestSkipped(t *testing.T) {
	for _, name := range []string{"selftest_inprogress", "selftest_aborted"} {
		r, err := DecodeSMART(fixture(t, name), 0)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		st := r.SelfTest
		if st == nil || st.Failed || st.PowerOnHours == nil || *st.PowerOnHours != 4000 {
			t.Errorf("%s: SelfTest = %+v, want older finished pass at 4000h", name, st)
		}
	}
}

func TestDecodeNVMeSelfTestStates(t *testing.T) {
	log := func(v string) []byte {
		return []byte(`{"device":{"name":"/dev/nvme0","protocol":"NVMe"},"serial_number":"TEST0000000002",
			"nvme_self_test_log":{"table":[
			{"self_test_result":{"value":` + v + `,"string":"newest"},"power_on_hours":20},
			{"self_test_result":{"value":0,"string":"old pass"},"power_on_hours":10}]}}`)
	}
	tests := map[string]struct {
		failed bool
		hours  int64
		desc   string
	}{
		"0": {false, 20, "newest"},
		"7": {true, 20, "newest"},
		"1": {
			false,
			10,
			"old pass",
		},
		"4":  {false, 10, "old pass"},
		"9":  {false, 10, "old pass"},
		"15": {false, 10, "old pass"},
	}
	for v, w := range tests {
		r, err := DecodeSMART(log(v), 0)
		if err != nil || r.SelfTest == nil {
			t.Fatalf("%s: %v %v", v, err, r.SelfTest)
		}
		if r.SelfTest.Failed != w.failed || *r.SelfTest.PowerOnHours != w.hours || r.SelfTest.Description != w.desc {
			t.Errorf("result %s: %+v", v, r.SelfTest)
		}
	}
}

func TestDecodeSATACounters(t *testing.T) {
	r, err := DecodeSMART(fixture(t, "sata_counters"), 0)
	if err != nil {
		t.Fatalf("DecodeSMART: %v", err)
	}

	want := map[int]uint64{5: 8, 187: 2, 188: 1, 197: 3, 198: 4, 199: 5}
	for id, w := range want {
		if got := r.Counters[ATACounter(id)]; got != w {
			t.Errorf("counter %d = %d, want %d", id, got, w)
		}
	}
	if _, ok := r.Counters[ATACounter(1)]; ok {
		t.Error("untracked attribute 1 reported")
	}
}

func TestDecodeNVMeHealthy(t *testing.T) {
	r, err := DecodeSMART(fixture(t, "nvme_healthy"), 0)
	if err != nil {
		t.Fatalf("DecodeSMART: %v", err)
	}

	if r.Serial != "TEST0000000002" || r.Bus != BusNVMe || r.Device != "/dev/nvme0" {
		t.Errorf("identity = %+v", r)
	}
	if r.CriticalWarning == nil || *r.CriticalWarning != 0 {
		t.Errorf("CriticalWarning = %v", r.CriticalWarning)
	}
	if r.AvailableSpare == nil || *r.AvailableSpare != 100 || r.SpareThreshold == nil || *r.SpareThreshold != 10 {
		t.Errorf("spare = %v / %v", r.AvailableSpare, r.SpareThreshold)
	}
	if r.PercentageUsed == nil || *r.PercentageUsed != 1 {
		t.Errorf("PercentageUsed = %v", r.PercentageUsed)
	}
	if r.DataWrittenBytes == nil || *r.DataWrittenBytes != 2000000*512000 {
		t.Errorf("DataWrittenBytes = %v", r.DataWrittenBytes)
	}
	if r.Temperature == nil || *r.Temperature != 38 {
		t.Errorf("Temperature = %v", r.Temperature)
	}
	if r.PowerOnHours == nil || *r.PowerOnHours != 1500 {
		t.Errorf("PowerOnHours = %v", r.PowerOnHours)
	}
	for _, k := range []string{CounterNVMeMediaErrors, CounterNVMeErrorLog} {
		if v, ok := r.Counters[k]; !ok || v != 0 {
			t.Errorf("counter %s = %d, %v", k, v, ok)
		}
	}
	if r.SelfTest == nil || r.SelfTest.Failed {
		t.Errorf("SelfTest = %+v", r.SelfTest)
	}
}

func TestDecodeNVMeCritical(t *testing.T) {
	r, err := DecodeSMART(fixture(t, "nvme_critical"), 0)
	if err != nil {
		t.Fatalf("DecodeSMART: %v", err)
	}

	if !r.HealthFailed {
		t.Error("HealthFailed = false")
	}
	if r.CriticalWarning == nil || *r.CriticalWarning != 5 {
		t.Errorf("CriticalWarning = %v", r.CriticalWarning)
	}
	if r.AvailableSpare == nil || *r.AvailableSpare != 8 {
		t.Errorf("AvailableSpare = %v", r.AvailableSpare)
	}
	if r.PercentageUsed == nil || *r.PercentageUsed != 92 {
		t.Errorf("PercentageUsed = %v", r.PercentageUsed)
	}
	if r.Counters[CounterNVMeMediaErrors] != 3 || r.Counters[CounterNVMeErrorLog] != 7 {
		t.Errorf("counters = %v", r.Counters)
	}
}

func TestDecodeHealthFailed(t *testing.T) {
	r, err := DecodeSMART(fixture(t, "health_failed"), 8)
	if err != nil {
		t.Fatalf("DecodeSMART: %v", err)
	}
	if !r.HealthFailed {
		t.Error("HealthFailed = false")
	}

	// Exit bit 3 alone also marks the drive failing.
	r, err = DecodeSMART(fixture(t, "sata_healthy"), 8)
	if err != nil {
		t.Fatalf("DecodeSMART: %v", err)
	}
	if !r.HealthFailed {
		t.Error("exit bit 3 did not set HealthFailed")
	}
}

func TestDecodeSelfTestFailed(t *testing.T) {
	r, err := DecodeSMART(fixture(t, "selftest_failed"), 64)
	if err != nil {
		t.Fatalf("DecodeSMART: %v", err)
	}
	if r.SelfTest == nil || !r.SelfTest.Failed || !strings.Contains(r.SelfTest.Description, "read failure") {
		t.Errorf("SelfTest = %+v", r.SelfTest)
	}
	if r.HealthFailed {
		t.Error("HealthFailed set for self-test failure")
	}
}

func TestDecodeInformationalExitBits(t *testing.T) {
	// Bits 2, 4, 5, 6, 7 are informational: the JSON is still decoded.
	for _, exit := range []int{4, 16, 32, 64, 128, 4 | 16 | 128} {
		r, err := DecodeSMART(fixture(t, "sata_healthy"), exit)
		if err != nil {
			t.Errorf("exit %d: %v", exit, err)
		} else if r.HealthFailed {
			t.Errorf("exit %d: HealthFailed", exit)
		}
	}
}

func TestDecodeOpenFailure(t *testing.T) {
	for _, exit := range []int{1, 2, 3, 2 | 8} {
		_, err := DecodeSMART(fixture(t, "open_failure"), exit)
		if !errors.Is(err, ErrUnreadable) {
			t.Errorf("exit %d: err = %v, want ErrUnreadable", exit, err)
		}
	}

	_, err := DecodeSMART(fixture(t, "open_failure"), 2)
	if err == nil || !strings.Contains(err.Error(), "Unable to detect device type") {
		t.Errorf("err = %v, want smartctl message", err)
	}
}

func TestDecodeMissingAttributes(t *testing.T) {
	data := []byte(`{"device":{"name":"/dev/sdb","protocol":"ATA"},"serial_number":"TEST0000000003",
		"ata_smart_attributes":{"table":[{"id":5,"raw":{"value":2}}]}}`)

	r, err := DecodeSMART(data, 0)
	if err != nil {
		t.Fatalf("DecodeSMART: %v", err)
	}
	if len(r.Counters) != 1 || r.Counters[ATACounter(5)] != 2 {
		t.Errorf("Counters = %v", r.Counters)
	}
	if r.Temperature != nil || r.PowerOnHours != nil || r.SelfTest != nil || r.HealthFailed ||
		r.DataWrittenBytes != nil {
		t.Errorf("unreported values set: %+v", r)
	}
}

func TestDecodeAttributeWithoutRaw(t *testing.T) {
	data := []byte(`{"device":{"name":"/dev/sdb","protocol":"ATA"},"serial_number":"TEST0000000003",
		"ata_smart_attributes":{"table":[{"id":5},{"id":197,"raw":{}}]}}`)

	r, err := DecodeSMART(data, 0)
	if err != nil {
		t.Fatalf("DecodeSMART: %v", err)
	}
	if len(r.Counters) != 0 {
		t.Errorf("Counters = %v, want none", r.Counters)
	}
}

func TestDecodeErrors(t *testing.T) {
	tests := map[string]string{
		"not json":       `smartctl: error`,
		"empty":          ``,
		"missing serial": `{"device":{"name":"/dev/sdb","protocol":"ATA"}}`,
	}
	for name, in := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeSMART([]byte(in), 0); !errors.Is(err, ErrUnreadable) {
				t.Errorf("err = %v, want ErrUnreadable", err)
			}
		})
	}
}
