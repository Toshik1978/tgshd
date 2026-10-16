package disks

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
	r, err := DecodeSMART(fixture(t, "sata_healthy"), 0, BuiltinCounters())
	if err != nil {
		t.Fatalf("DecodeSMART: %v", err)
	}

	if r.Serial != "TEST0000000001" || r.Model != "Example SATA SSD" || r.Device != "/dev/sda" || r.Bus != BusATA {
		t.Errorf("identity = %+v", r)
	}
	if r.Health != HealthOK {
		t.Errorf("Health = %v, want ok", r.Health)
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
		r, err := DecodeSMART(fixture(t, name), 0, BuiltinCounters())
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
		"4":  {true, 20, "newest"},
		"9":  {true, 20, "newest"},
		"15": {false, 10, "old pass"},
	}
	for v, w := range tests {
		r, err := DecodeSMART(log(v), 0, BuiltinCounters())
		if err != nil || r.SelfTest == nil {
			t.Fatalf("%s: %v %v", v, err, r.SelfTest)
		}
		if r.SelfTest.Failed != w.failed || *r.SelfTest.PowerOnHours != w.hours || r.SelfTest.Description != w.desc {
			t.Errorf("result %s: %+v", v, r.SelfTest)
		}
	}
}

func TestDecodeSATACounters(t *testing.T) {
	r, err := DecodeSMART(fixture(t, "sata_counters"), 0, BuiltinCounters())
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
	r, err := DecodeSMART(fixture(t, "nvme_healthy"), 0, BuiltinCounters())
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
	if v, ok := r.Counters[CounterNVMeMediaErrors]; !ok || v != 0 {
		t.Errorf("media errors = %d, %v", v, ok)
	}
	if len(r.Counters) != 1 {
		t.Errorf("Counters = %v, want only the media error counter", r.Counters)
	}
	if r.NVMeErrorLogEntries == nil || *r.NVMeErrorLogEntries != 0 {
		t.Errorf("NVMeErrorLogEntries = %v", r.NVMeErrorLogEntries)
	}
	if r.SelfTest == nil || r.SelfTest.Failed {
		t.Errorf("SelfTest = %+v", r.SelfTest)
	}
}

func TestDecodeNVMeCritical(t *testing.T) {
	r, err := DecodeSMART(fixture(t, "nvme_critical"), 0, BuiltinCounters())
	if err != nil {
		t.Fatalf("DecodeSMART: %v", err)
	}

	if r.Health != HealthFailing {
		t.Errorf("Health = %v, want failing", r.Health)
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
	if r.Counters[CounterNVMeMediaErrors] != 3 || r.NVMeErrorLogEntries == nil || *r.NVMeErrorLogEntries != 7 {
		t.Errorf("counters = %v", r.Counters)
	}
}

func TestDecodeHealthFailed(t *testing.T) {
	r, err := DecodeSMART(fixture(t, "health_failed"), 8, BuiltinCounters())
	if err != nil {
		t.Fatalf("DecodeSMART: %v", err)
	}
	if r.Health != HealthFailing {
		t.Errorf("Health = %v, want failing", r.Health)
	}

	// Exit bit 3 alone also marks the drive failing.
	r, err = DecodeSMART(fixture(t, "sata_healthy"), 8, BuiltinCounters())
	if err != nil {
		t.Fatalf("DecodeSMART: %v", err)
	}
	if r.Health != HealthFailing {
		t.Errorf("exit bit 3: Health = %v, want failing", r.Health)
	}
}

func TestDecodeSelfTestFailed(t *testing.T) {
	r, err := DecodeSMART(fixture(t, "selftest_failed"), 64, BuiltinCounters())
	if err != nil {
		t.Fatalf("DecodeSMART: %v", err)
	}
	if r.SelfTest == nil || !r.SelfTest.Failed || !strings.Contains(r.SelfTest.Description, "read failure") {
		t.Errorf("SelfTest = %+v", r.SelfTest)
	}
	if r.Health != HealthOK {
		t.Errorf("Health = %v, want ok for a self-test failure", r.Health)
	}
}

func TestDecodeInformationalExitBits(t *testing.T) {
	// Bits 2, 4, 5, 6, 7 are informational: the JSON is still decoded.
	for _, exit := range []int{4, 16, 32, 64, 128, 4 | 16 | 128} {
		r, err := DecodeSMART(fixture(t, "sata_healthy"), exit, BuiltinCounters())
		if err != nil {
			t.Errorf("exit %d: %v", exit, err)
		} else if r.Health != HealthOK {
			t.Errorf("exit %d: Health = %v", exit, r.Health)
		}
	}
}

func TestDecodeOpenFailure(t *testing.T) {
	for _, exit := range []int{1, 2, 3, 2 | 8} {
		_, err := DecodeSMART(fixture(t, "open_failure"), exit, BuiltinCounters())
		if !errors.Is(err, ErrUnreadable) {
			t.Errorf("exit %d: err = %v, want ErrUnreadable", exit, err)
		}
	}

	_, err := DecodeSMART(fixture(t, "open_failure"), 2, BuiltinCounters())
	if err == nil || !strings.Contains(err.Error(), "Unable to detect device type") {
		t.Errorf("err = %v, want smartctl message", err)
	}
}

func TestDecodeMissingAttributes(t *testing.T) {
	data := []byte(`{"device":{"name":"/dev/sdb","protocol":"ATA"},"serial_number":"TEST0000000003",
		"ata_smart_attributes":{"table":[{"id":5,"raw":{"value":2}}]}}`)

	r, err := DecodeSMART(data, 0, BuiltinCounters())
	if err != nil {
		t.Fatalf("DecodeSMART: %v", err)
	}
	if len(r.Counters) != 1 || r.Counters[ATACounter(5)] != 2 {
		t.Errorf("Counters = %v", r.Counters)
	}
	if r.Temperature != nil || r.PowerOnHours != nil || r.SelfTest != nil || r.Health != HealthUnknown ||
		r.DataWrittenBytes != nil {
		t.Errorf("unreported values set: %+v", r)
	}
}

func TestDecodeAttributeWithoutRaw(t *testing.T) {
	data := []byte(`{"device":{"name":"/dev/sdb","protocol":"ATA"},"serial_number":"TEST0000000003",
		"ata_smart_attributes":{"table":[{"id":5},{"id":197,"raw":{}}]}}`)

	r, err := DecodeSMART(data, 0, BuiltinCounters())
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
			if _, err := DecodeSMART([]byte(in), 0, BuiltinCounters()); !errors.Is(err, ErrUnreadable) {
				t.Errorf("err = %v, want ErrUnreadable", err)
			}
		})
	}
}

func TestDecodeBuiltinCounterSet(t *testing.T) {
	r, err := DecodeSMART(fixture(t, "sata_counters_183"), 0, BuiltinCounters())
	if err != nil {
		t.Fatalf("DecodeSMART: %v", err)
	}

	want := map[int]uint64{5: 0, 179: 1, 181: 2, 182: 3, 183: 4, 195: 5}
	if len(r.Counters) != len(want) {
		t.Errorf("Counters = %v", r.Counters)
	}
	for id, w := range want {
		if got, ok := r.Counters[ATACounter(id)]; !ok || got != w {
			t.Errorf("counter %d = %d, %v; want %d", id, got, ok, w)
		}
	}
	if r.PercentageUsed != nil {
		t.Errorf("PercentageUsed = %d without attribute 177", *r.PercentageUsed)
	}
}

func TestDecodeExtraCounters(t *testing.T) {
	set := BuiltinCounters()
	set[1], set[232] = true, true
	r, err := DecodeSMART(fixture(t, "sata_counters_183"), 0, set)
	if err != nil {
		t.Fatalf("DecodeSMART: %v", err)
	}
	if r.Counters[ATACounter(1)] != 7 || r.Counters[ATACounter(232)] != 6 || len(r.Counters) != 8 {
		t.Errorf("Counters = %v", r.Counters)
	}

	r, err = DecodeSMART(fixture(t, "sata_counters_183"), 0, map[int]bool{0: true, 256: true, 5: true})
	if err != nil {
		t.Fatalf("DecodeSMART: %v", err)
	}
	if len(r.Counters) != 1 {
		t.Errorf("Counters = %v, want only ATA 5", r.Counters)
	}
}

func TestDecodeSATAWear(t *testing.T) {
	r, err := DecodeSMART(fixture(t, "sata_ssd_177"), 0, BuiltinCounters())
	if err != nil {
		t.Fatalf("DecodeSMART: %v", err)
	}
	if r.PercentageUsed == nil || *r.PercentageUsed != 12 {
		t.Errorf("PercentageUsed = %v, want 12", r.PercentageUsed)
	}
}

func TestDecodeSATAWearConditions(t *testing.T) {
	const tmpl = `{"device":{"name":"/dev/sda","protocol":%q},"serial_number":"TEST0000000001",%s
		"ata_smart_attributes":{"table":[{"id":177,"value":%d,"raw":{"value":1}}]}}`
	tests := []struct {
		name     string
		protocol string
		rotation string
		value    int
		want     *int
	}{
		{"rotating disk", "ATA", `"rotation_rate":7200,`, 80, nil},
		{"rotation unreported", "ATA", ``, 80, nil},
		{"value above 100", "ATA", `"rotation_rate":0,`, 253, nil},
		{"negative value", "ATA", `"rotation_rate":0,`, -1, nil},
		{"full health", "ATA", `"rotation_rate":0,`, 100, new(0)},
		{"exhausted", "ATA", `"rotation_rate":0,`, 0, new(100)},
		{"other bus", "NVMe", `"rotation_rate":0,`, 80, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			in := fmt.Sprintf(tmpl, tc.protocol, tc.rotation, tc.value)
			r, err := DecodeSMART([]byte(in), 0, BuiltinCounters())
			if err != nil {
				t.Fatalf("DecodeSMART: %v", err)
			}
			switch {
			case tc.want == nil && r.PercentageUsed != nil:
				t.Errorf("PercentageUsed = %d, want nil", *r.PercentageUsed)
			case tc.want != nil && (r.PercentageUsed == nil || *r.PercentageUsed != *tc.want):
				t.Errorf("PercentageUsed = %v, want %d", r.PercentageUsed, *tc.want)
			}
		})
	}
}

func TestNVMeErrorLogIsNotACounter(t *testing.T) {
	r, err := DecodeSMART(fixture(t, "nvme_errlog_rise"), 0, BuiltinCounters())
	if err != nil {
		t.Fatalf("DecodeSMART: %v", err)
	}
	if _, ok := r.Counters["nvme.num_err_log_entries"]; ok {
		t.Error("num_err_log_entries in Counters")
	}
	if r.NVMeErrorLogEntries == nil || *r.NVMeErrorLogEntries != 12 {
		t.Errorf("NVMeErrorLogEntries = %v, want 12", r.NVMeErrorLogEntries)
	}
}

func TestHealthVerdict(t *testing.T) {
	tests := []struct {
		name    string
		fixture string
		exit    int
		want    Health
	}{
		{"passed true", "sata_healthy", 0, HealthOK},
		{"passed false", "health_failed", 0, HealthFailing},
		{"no verdict", "no_smart_status", 0, HealthUnknown},
		{"exit bit 3 without status", "no_smart_status", 8, HealthFailing},
		{"exit bit 3 overrides passed", "sata_healthy", 8, HealthFailing},
		{"other exit bits keep unknown", "no_smart_status", 64, HealthUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := DecodeSMART(fixture(t, tt.fixture), tt.exit, BuiltinCounters())
			if err != nil {
				t.Fatalf("DecodeSMART: %v", err)
			}
			if r.Health != tt.want {
				t.Errorf("Health = %v, want %v", r.Health, tt.want)
			}
		})
	}

	for h, want := range map[Health]string{HealthUnknown: "unknown", HealthOK: "ok", HealthFailing: "failing"} {
		if got := h.String(); got != want {
			t.Errorf("Health(%d).String() = %q, want %q", int(h), got, want)
		}
	}
}

// without removes top-level keys from a fixture.
func without(t *testing.T, name string, keys ...string) []byte {
	t.Helper()

	var m map[string]any
	if err := json.Unmarshal(fixture(t, name), &m); err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
	for _, k := range keys {
		delete(m, k)
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}

	return out
}

func TestSectionGaps(t *testing.T) {
	tests := []struct {
		name string
		json []byte
		want []Gap
	}{
		{"complete ata", fixture(t, "sata_healthy"), nil},
		{"complete nvme", fixture(t, "nvme_healthy"), nil},
		{"ata without attributes", fixture(t, "no_attribute_table"), []Gap{GapAttributes}},
		{"ata without self-test log", fixture(t, "no_selftest_log"), []Gap{GapSelfTestLog}},
		{"ata without temperature", without(t, "sata_healthy", "temperature"), []Gap{GapTemperature}},
		{
			"nvme without health log", without(t, "nvme_healthy", "nvme_smart_health_information_log"),
			[]Gap{GapNVMeHealthLog},
		},
		{
			"nvme without health log or temperature",
			without(t, "nvme_healthy", "nvme_smart_health_information_log", "temperature"),
			[]Gap{GapNVMeHealthLog, GapTemperature},
		},
		{"nvme without self-test log", without(t, "nvme_healthy", "nvme_self_test_log"), []Gap{GapSelfTestLog}},
		{
			"nvme temperature only in the health log",
			without(t, "nvme_healthy", "temperature"), nil,
		},
		{"scsi needs only the verdict", fixture(t, "sas_selftest"), nil},
		{"scsi without log", without(t, "sas_selftest", "scsi_self_test_0"), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := DecodeSMART(tt.json, 0, BuiltinCounters())
			if err != nil {
				t.Fatalf("DecodeSMART: %v", err)
			}
			if !slices.Equal(r.Gaps, tt.want) {
				t.Errorf("Gaps = %v, want %v", r.Gaps, tt.want)
			}
		})
	}
}

func ataLog(entries string) []byte {
	return []byte(`{"device":{"name":"/dev/sda","protocol":"ATA"},"serial_number":"TEST0000000001",
		"ata_smart_self_test_log":{"standard":{"table":[` + entries + `]}}}`)
}

func nvmeLog(entries string) []byte {
	return []byte(`{"device":{"name":"/dev/nvme0","protocol":"NVMe"},"serial_number":"TEST0000000002",
		"nvme_self_test_log":{"table":[` + entries + `]}}`)
}

func ataCodeLog(code int) []byte {
	return ataLog(fmt.Sprintf(`{"type":{"value":2},"status":{"value":%d,"string":"s"},"lifetime_hours":100}`, code))
}

func nvmeCodeLog(code int) []byte {
	return nvmeLog(fmt.Sprintf(
		`{"self_test_code":{"value":2},"self_test_result":{"value":%d,"string":"s"},"power_on_hours":100}`, code,
	))
}

func TestSelfTestClassification(t *testing.T) {
	ata, nvme := ataCodeLog, nvmeCodeLog
	tests := []struct {
		name string
		json []byte
		want SelfTestResult
	}{
		{"ata passed", ata(0x00), SelfTestPassed},
		{"ata aborted by host", ata(0x10), SelfTestAborted},
		{"ata reset", ata(0x20), SelfTestAborted},
		{"ata read failure", ata(0x71), SelfTestFailed},
		{"ata first failure code", ata(0x30), SelfTestFailed},
		{"ata reserved code", ata(0x90), SelfTestFailed},
		{"ata last reserved code", ata(0xE0), SelfTestFailed},
		{"ata in progress", ata(0xF3), SelfTestInProgress},
		{"nvme passed", nvme(0), SelfTestPassed},
		{"nvme aborted by command", nvme(1), SelfTestAborted},
		{"nvme aborted by reset", nvme(2), SelfTestAborted},
		{"nvme namespace removed", nvme(3), SelfTestAborted},
		{"nvme fatal", nvme(4), SelfTestFailed},
		{"nvme failed segment", nvme(7), SelfTestFailed},
		{"nvme reserved", nvme(9), SelfTestFailed},
		{"nvme unused entry", nvme(0xF), SelfTestSkipped},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, err := DecodeSMART(tt.json, 0, BuiltinCounters())
			if err != nil {
				t.Fatalf("DecodeSMART: %v", err)
			}
			if len(r.SelfTestLog) != 1 || r.SelfTestLog[0].Result != tt.want {
				t.Fatalf("SelfTestLog = %+v, want one %v entry", r.SelfTestLog, tt.want)
			}
			if r.SelfTestFormat != SelfTestFormatOK {
				t.Errorf("SelfTestFormat = %q, want ok", r.SelfTestFormat)
			}
		})
	}
}

func TestSelfTestLogIdentity(t *testing.T) {
	t.Run("identity and order", func(t *testing.T) {
		r, err := DecodeSMART(ataLog(`
			{"type":{"value":1},"status":{"value":113,"string":"fail"},"lifetime_hours":4190},
			{"type":{"value":2},"status":{"value":0,"string":"ok"},"lifetime_hours":4000}`), 0, BuiltinCounters())
		if err != nil {
			t.Fatalf("DecodeSMART: %v", err)
		}
		want := []SelfTestID{{Hours: 4190, Type: 1, Code: 7}, {Hours: 4000, Type: 2, Code: 0}}
		got := make([]SelfTestID, 0, len(r.SelfTestLog))
		for _, e := range r.SelfTestLog {
			got = append(got, e.ID)
		}
		if !slices.Equal(got, want) {
			t.Errorf("identities = %+v, want %+v", got, want)
		}
		if r.SelfTest == nil || !r.SelfTest.Failed {
			t.Errorf("SelfTest = %+v, want the newest finished (failed) entry", r.SelfTest)
		}
	})

	t.Run("nvme identity", func(t *testing.T) {
		r, err := DecodeSMART(nvmeCodeLog(5), 0, BuiltinCounters())
		if err != nil {
			t.Fatalf("DecodeSMART: %v", err)
		}
		if got, want := r.SelfTestLog[0].ID, (SelfTestID{Hours: 100, Type: 2, Code: 5}); got != want {
			t.Errorf("ID = %+v, want %+v", got, want)
		}
	})
}

func TestSelfTestLogEdges(t *testing.T) {
	t.Run("empty log is readable", func(t *testing.T) {
		r, err := DecodeSMART(ataLog(""), 0, BuiltinCounters())
		if err != nil {
			t.Fatalf("DecodeSMART: %v", err)
		}
		if r.SelfTestFormat != SelfTestFormatOK || len(r.SelfTestLog) != 0 {
			t.Errorf("format %q, log %+v", r.SelfTestFormat, r.SelfTestLog)
		}
	})

	for name, js := range map[string][]byte{
		"ata entry without status.value": ataLog(`{"type":{"value":1},"status":{"string":"s"},"lifetime_hours":1}`),
		"nvme entry without result":      nvmeLog(`{"power_on_hours":1}`),
		"ata extended-only log":          fixture(t, "selftest_unsupported"),
	} {
		t.Run(name, func(t *testing.T) {
			r, err := DecodeSMART(js, 0, BuiltinCounters())
			if err != nil {
				t.Fatalf("DecodeSMART: %v", err)
			}
			if r.SelfTestFormat != SelfTestFormatUnsupported {
				t.Errorf("SelfTestFormat = %q, want unsupported", r.SelfTestFormat)
			}
			if r.SelfTest != nil || len(r.SelfTestLog) != 0 {
				t.Errorf("undecodable log yielded entries: %+v %+v", r.SelfTest, r.SelfTestLog)
			}
			if slices.Contains(r.Gaps, GapSelfTestLog) {
				t.Errorf("Gaps = %v: a present log is not a gap", r.Gaps)
			}
		})
	}
}

func TestMissingSelfTestLogIsGapNotUnsupported(t *testing.T) {
	for name, js := range map[string][]byte{
		"ata":  fixture(t, "no_selftest_log"),
		"nvme": without(t, "nvme_healthy", "nvme_self_test_log"),
	} {
		t.Run(name, func(t *testing.T) {
			r, err := DecodeSMART(js, 0, BuiltinCounters())
			if err != nil {
				t.Fatalf("DecodeSMART: %v", err)
			}
			if r.SelfTestFormat == SelfTestFormatUnsupported {
				t.Error("a missing log was marked unsupported")
			}
			if !slices.Contains(r.Gaps, GapSelfTestLog) {
				t.Errorf("Gaps = %v, want selftest-log", r.Gaps)
			}
		})
	}
}

func TestSCSISelfTestUnsupported(t *testing.T) {
	for name, js := range map[string][]byte{
		"with log":    fixture(t, "sas_selftest"),
		"without log": without(t, "sas_selftest", "scsi_self_test_0"),
	} {
		t.Run(name, func(t *testing.T) {
			r, err := DecodeSMART(js, 0, BuiltinCounters())
			if err != nil {
				t.Fatalf("DecodeSMART: %v", err)
			}
			if r.SelfTestFormat != SelfTestFormatUnsupported {
				t.Errorf("SelfTestFormat = %q, want unsupported", r.SelfTestFormat)
			}
			if r.SelfTest != nil || len(r.SelfTestLog) != 0 {
				t.Errorf("SCSI drive yielded entries: %+v %+v", r.SelfTest, r.SelfTestLog)
			}
			if r.Health != HealthOK || len(r.Gaps) != 0 {
				t.Errorf("Health %v, Gaps %v", r.Health, r.Gaps)
			}
		})
	}
}
