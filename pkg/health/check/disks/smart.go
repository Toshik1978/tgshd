// Package disks implements the drive hardware-health check. This file decodes
// the JSON produced by `smartctl --json=c -a` into a typed report.
package disks

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

// Bus is the drive transport as reported by smartctl.
type Bus string

// Known drive buses.
const (
	BusATA  Bus = "ATA"
	BusNVMe Bus = "NVMe"
	// BusSCSI is what smartctl reports for SCSI and SAS drives.
	BusSCSI Bus = "SCSI"
)

// Health is the SMART overall-health verdict of a read.
type Health int

// Health verdicts. The zero value is unknown: a read that carries no verdict
// must never count as healthy.
const (
	HealthUnknown Health = iota
	HealthOK
	HealthFailing
)

// String returns the verdict as stored in the baseline: ok, failing or unknown.
func (h Health) String() string {
	switch h {
	case HealthOK:
		return "ok"
	case HealthFailing:
		return "failing"
	case HealthUnknown:
	}

	return "unknown"
}

// Gap names a section the read lacked.
type Gap string

// Gaps a read can have (research R2). A drive's self-test log being present
// but not decodable is not a gap: see Report.SelfTestFormat.
const (
	GapAttributes    Gap = "attributes"
	GapNVMeHealthLog Gap = "nvme-health-log"
	GapSelfTestLog   Gap = "selftest-log"
	GapTemperature   Gap = "temperature"
)

// SelfTestResult classifies one self-test log entry.
type SelfTestResult int

// Self-test results.
const (
	SelfTestPassed SelfTestResult = iota
	SelfTestAborted
	SelfTestInProgress
	SelfTestFailed
	// SelfTestSkipped is an unused NVMe log entry.
	SelfTestSkipped
)

// SelfTestFormat tells whether a drive's self-test log could be decoded.
type SelfTestFormat string

// Self-test log formats.
const (
	SelfTestFormatOK          SelfTestFormat = "ok"
	SelfTestFormatUnsupported SelfTestFormat = "unsupported"
)

// SelfTestID identifies a self-test log entry: when it ran, what kind of
// test it was and how it ended.
type SelfTestID struct {
	Hours int64 `json:"hours"`
	Type  int   `json:"type"`
	Code  int   `json:"code"`
}

// SelfTestEntry is one decoded self-test log entry.
type SelfTestEntry struct {
	ID          SelfTestID
	Description string
	Result      SelfTestResult

	hasHours bool // the log gave the entry's power-on hours
	// wraps marks hours that are 16 bits and wrap at ataSelfTestHoursWrap
	// (the ATA self-test log).
	wraps bool
}

// ataSelfTestHoursWrap is where the 16-bit lifetime hours of an ATA
// self-test log entry wrap.
const ataSelfTestHoursWrap = 1 << 16

// CounterNVMeMediaErrors is the Report.Counters key of the NVMe media error
// counter. ATA counters are keyed with ATACounter.
const CounterNVMeMediaErrors = "nvme.media_errors"

// smartctl exit status bits (see smartctl(8)).
const (
	exitCommandLine = 1 << 0 // command line did not parse
	exitDeviceOpen  = 1 << 1 // device open failed / no SMART support
	exitDiskFailing = 1 << 3 // SMART status check returned DISK FAILING

	// NVMe data units are thousands of 512-byte blocks.
	nvmeDataUnitBytes = 512000

	// Self-test result classification.
	ataSelfTestStatusShift = 4
	ataSelfTestAbortedLast = 2
	ataSelfTestInProgress  = 15
	nvmeSelfTestAbortedMax = 3
	nvmeSelfTestUnused     = 0xF

	// ATA attribute 241: total LBAs written.
	ataTotalLBAsWritten = 241
	// ATA attribute 177: wear levelling count; its normalised value counts
	// down from 100 on a SATA SSD. Only a row smartctl names
	// Wear_Leveling_Count is wear: other controllers use the id for something
	// else (SandForce's Wear_Range_Delta, whose normalised value is often 0).
	ataWearLeveling     = 177
	ataWearLevelingName = "Wear_Leveling_Count"
	maxATAID            = 255
	percentFull         = 100
)

// ErrUnreadable means smartctl could not read the drive (command line error or
// device open failure) or its output was not usable.
var ErrUnreadable = errors.New("drive unreadable")

// SelfTest is the newest finished (passed or failed) self-test log entry;
// in-progress and aborted entries are skipped.
type SelfTest struct {
	Description  string
	Failed       bool
	PowerOnHours *int64 // drive lifetime hours when the test ran, nil if not reported
}

// Report is the decoded state of one drive. Pointer fields are nil and Counters
// entries are absent when smartctl did not report the value.
type Report struct {
	Serial string
	Model  string
	Device string
	Bus    Bus

	// Health is explicit only when smart_status.passed is present or exit
	// status bit 3 is set; otherwise it is HealthUnknown.
	Health Health

	// Gaps lists the sections the read lacked.
	Gaps []Gap

	Temperature      *int
	PowerOnHours     *int64
	PercentageUsed   *int
	AvailableSpare   *int
	SpareThreshold   *int
	CriticalWarning  *int
	DataWrittenBytes *uint64
	// SelfTest is the newest finished entry of SelfTestLog, or nil.
	SelfTest *SelfTest
	// SelfTestLog is the decoded self-test log, newest first.
	SelfTestLog []SelfTestEntry
	// SelfTestFormat is unsupported when the drive's self-test log cannot be
	// decoded, and always for a drive that is neither ATA nor NVMe.
	SelfTestFormat SelfTestFormat

	// NVMeErrorLogEntries is num_err_log_entries. It is history-only, not an
	// error counter: the log also records harmless entries.
	NVMeErrorLogEntries *uint64

	// Counters holds the error counters that should never rise, keyed by
	// ATACounter(id) or the CounterNVMe* constants.
	Counters map[string]uint64
}

// ATACounter returns the Report.Counters key of an ATA attribute.
func ATACounter(id int) string {
	return "ata." + strconv.Itoa(id)
}

// BuiltinCounters returns the ATA attribute ids tracked for every drive, as
// a fresh set the caller may extend.
func BuiltinCounters() map[int]bool {
	set := make(map[int]bool)
	for _, id := range []int{5, 179, 181, 182, 183, 187, 188, 195, 197, 198, 199} {
		set[id] = true
	}

	return set
}

type smartJSON struct {
	Smartctl struct {
		Messages []struct {
			String string `json:"string"`
		} `json:"messages"`
	} `json:"smartctl"`
	Device struct {
		Name     string `json:"name"`
		Protocol string `json:"protocol"`
	} `json:"device"`
	RotationRate *int    `json:"rotation_rate"`
	LogicalBlock *uint64 `json:"logical_block_size"`
	ModelName    string  `json:"model_name"`
	SerialNumber string  `json:"serial_number"`
	SmartStatus  *struct {
		Passed *bool `json:"passed"`
	} `json:"smart_status"`
	ATAAttributes struct {
		Table []struct {
			ID    int    `json:"id"`
			Name  string `json:"name"`
			Value *int   `json:"value"`
			Raw   *struct {
				Value *uint64 `json:"value"`
			} `json:"raw"`
		} `json:"table"`
	} `json:"ata_smart_attributes"`
	NVMeHealth *struct {
		CriticalWarning *int    `json:"critical_warning"`
		Temperature     *int    `json:"temperature"`
		AvailableSpare  *int    `json:"available_spare"`
		SpareThreshold  *int    `json:"available_spare_threshold"`
		PercentageUsed  *int    `json:"percentage_used"`
		DataUnitsWrote  *uint64 `json:"data_units_written"`
		MediaErrors     *uint64 `json:"media_errors"`
		ErrLogEntries   *uint64 `json:"num_err_log_entries"`
	} `json:"nvme_smart_health_information_log"`
	Temperature *struct {
		Current *int `json:"current"`
	} `json:"temperature"`
	PowerOnTime *struct {
		Hours *int64 `json:"hours"`
	} `json:"power_on_time"`
	ATASelfTest *struct {
		Standard *struct {
			Table []struct {
				LifetimeHours *int64 `json:"lifetime_hours"`
				Type          struct {
					Value int `json:"value"`
				} `json:"type"`
				Status struct {
					Value  *int   `json:"value"`
					String string `json:"string"`
				} `json:"status"`
			} `json:"table"`
		} `json:"standard"`
	} `json:"ata_smart_self_test_log"`
	NVMeSelfTest *struct {
		Table []struct {
			PowerOnHours *int64 `json:"power_on_hours"`
			Code         struct {
				Value int `json:"value"`
			} `json:"self_test_code"`
			Result struct {
				Value  *int   `json:"value"`
				String string `json:"string"`
			} `json:"self_test_result"`
		} `json:"table"`
	} `json:"nvme_self_test_log"`
}

// DecodeSMART decodes smartctl JSON output together with the process exit
// status. Exit bits 0-1 (command line / device open failure) yield an error
// wrapping ErrUnreadable; bit 3 marks the drive failing; other bits are
// informational and the JSON is decoded regardless. counters is the set of ATA
// attribute ids kept in Report.Counters, compared by raw value.
func DecodeSMART(stdout []byte, exitStatus int, counters map[int]bool) (Report, error) {
	var raw smartJSON
	jsonErr := json.Unmarshal(stdout, &raw)

	if exitStatus&(exitCommandLine|exitDeviceOpen) != 0 {
		return Report{}, openError(&raw, jsonErr, exitStatus)
	}
	if jsonErr != nil {
		return Report{}, fmt.Errorf("%w: decode smartctl json: %w", ErrUnreadable, jsonErr)
	}
	if raw.SerialNumber == "" {
		return Report{}, fmt.Errorf("%w: no serial number reported", ErrUnreadable)
	}

	r := Report{
		Serial:   raw.SerialNumber,
		Model:    raw.ModelName,
		Device:   raw.Device.Name,
		Bus:      Bus(raw.Device.Protocol),
		Counters: map[string]uint64{},
	}
	r.Health = raw.health(exitStatus)
	r.Temperature, r.PowerOnHours = raw.generic()

	applyATA(&r, &raw, counters)
	applyNVMe(&r, &raw)
	applySelfTest(&r, &raw)
	r.Gaps = gaps(&r, &raw)

	return r, nil
}

func openError(raw *smartJSON, jsonErr error, exitStatus int) error {
	msg := "smartctl exit status " + strconv.Itoa(exitStatus)
	if jsonErr == nil && len(raw.Smartctl.Messages) > 0 {
		msg += ": " + raw.Smartctl.Messages[0].String
	}

	return fmt.Errorf("%w: %s", ErrUnreadable, msg)
}

// health returns the explicit verdict: failing when smart_status.passed is
// false or exit status bit 3 is set, ok when smart_status.passed is true, and
// unknown when the output carries neither.
func (s *smartJSON) health(exitStatus int) Health {
	switch {
	case exitStatus&exitDiskFailing != 0:
		return HealthFailing
	case s.SmartStatus == nil || s.SmartStatus.Passed == nil:
		return HealthUnknown
	case *s.SmartStatus.Passed:
		return HealthOK
	default:
		return HealthFailing
	}
}

func (s *smartJSON) generic() (temperature *int, hours *int64) {
	if s.Temperature != nil {
		temperature = s.Temperature.Current
	}
	if s.PowerOnTime != nil {
		hours = s.PowerOnTime.Hours
	}

	return temperature, hours
}

// applyATA collects the tracked counters, the data written and the SATA SSD
// wear (attribute 177 named Wear_Leveling_Count). An attribute row without a
// raw value is "not reported". Attribute 188 (command timeout) may pack
// several 16-bit counters into raw.value on some drives; it is used as-is for
// rise detection, without masking.
func applyATA(r *Report, raw *smartJSON, counters map[int]bool) {
	solidState := r.Bus == BusATA && raw.RotationRate != nil && *raw.RotationRate == 0
	for _, a := range raw.ATAAttributes.Table {
		if solidState && wearLeveling(a.ID, a.Name) {
			r.PercentageUsed = wearUsed(a.Value)
		}
		if a.Raw == nil || a.Raw.Value == nil {
			continue
		}
		if tracked(counters, a.ID) {
			r.Counters[ATACounter(a.ID)] = *a.Raw.Value
		}
		if a.ID == ataTotalLBAsWritten {
			r.DataWrittenBytes = writtenBytes(*a.Raw.Value, raw.LogicalBlock)
		}
	}
}

// wearLeveling reports whether an attribute row is the wear levelling count.
func wearLeveling(id int, name string) bool {
	return id == ataWearLeveling && name == ataWearLevelingName
}

func writtenBytes(lbas uint64, blockSize *uint64) *uint64 {
	if blockSize == nil {
		return nil
	}
	w := lbas * *blockSize

	return &w
}

func applyNVMe(r *Report, raw *smartJSON) {
	h := raw.NVMeHealth
	if h == nil {
		return
	}

	r.CriticalWarning = h.CriticalWarning
	r.AvailableSpare = h.AvailableSpare
	r.SpareThreshold = h.SpareThreshold
	r.PercentageUsed = h.PercentageUsed
	if r.Temperature == nil {
		r.Temperature = h.Temperature
	}
	if h.DataUnitsWrote != nil {
		w := *h.DataUnitsWrote * nvmeDataUnitBytes
		r.DataWrittenBytes = &w
	}
	if h.MediaErrors != nil {
		r.Counters[CounterNVMeMediaErrors] = *h.MediaErrors
	}
	r.NVMeErrorLogEntries = h.ErrLogEntries
}

// applySelfTest decodes the self-test log. A drive that is neither ATA nor
// NVMe (SCSI/SAS) is always unsupported, with or without a log; so is a log
// whose entries cannot be decoded, in which case it yields no entries.
func applySelfTest(r *Report, raw *smartJSON) {
	if raw.selfTestLogMissing(r.Bus) {
		// A missing log is a gap (GapSelfTestLog), not an unrecognised
		// format: the check reports it and the stored format is left alone.
		r.SelfTestFormat = SelfTestFormatOK

		return
	}
	var entries []SelfTestEntry
	var ok bool
	switch r.Bus {
	case BusATA:
		entries, ok = ataSelfTestEntries(raw)
	case BusNVMe:
		entries, ok = nvmeSelfTestEntries(raw)
	case BusSCSI:
		// The SCSI self-test log is not decoded: always unsupported.
	}
	if !ok {
		r.SelfTestFormat = SelfTestFormatUnsupported

		return
	}
	r.SelfTestFormat = SelfTestFormatOK
	r.SelfTestLog = entries
	for _, e := range entries {
		if e.Result == SelfTestPassed || e.Result == SelfTestFailed {
			r.SelfTest = &SelfTest{
				Description: e.Description, Failed: e.Result == SelfTestFailed, PowerOnHours: hoursOf(e),
			}

			break
		}
	}
}

// selfTestLogMissing reports an ATA or NVMe drive whose output has no
// self-test log section.
func (s *smartJSON) selfTestLogMissing(bus Bus) bool {
	return (bus == BusATA && s.ATASelfTest == nil) || (bus == BusNVMe && s.NVMeSelfTest == nil)
}

// hoursOf returns the entry's power-on hours, nil when the log gave none.
func hoursOf(e SelfTestEntry) *int64 {
	if e.hasHours {
		h := e.ID.Hours

		return &h
	}

	return nil
}

// ataSelfTestEntries decodes the standard ATA log. ok is false when the log is
// absent, has no standard section or has an entry without status.value.
func ataSelfTestEntries(raw *smartJSON) (entries []SelfTestEntry, ok bool) {
	if raw.ATASelfTest == nil || raw.ATASelfTest.Standard == nil {
		return nil, false
	}
	for _, e := range raw.ATASelfTest.Standard.Table {
		if e.Status.Value == nil {
			return nil, false
		}
		code := *e.Status.Value >> ataSelfTestStatusShift
		entry := SelfTestEntry{
			ID:          SelfTestID{Type: e.Type.Value, Code: code},
			Description: e.Status.String,
			Result:      classifyATA(code),
			wraps:       true,
		}
		if e.LifetimeHours != nil {
			entry.ID.Hours, entry.hasHours = *e.LifetimeHours, true
		}
		entries = append(entries, entry)
	}

	return entries, true
}

// nvmeSelfTestEntries decodes the NVMe log. ok is false when the log is absent
// or has an entry without self_test_result.value.
func nvmeSelfTestEntries(raw *smartJSON) (entries []SelfTestEntry, ok bool) {
	if raw.NVMeSelfTest == nil {
		return nil, false
	}
	for _, e := range raw.NVMeSelfTest.Table {
		if e.Result.Value == nil {
			return nil, false
		}
		entry := SelfTestEntry{
			ID:          SelfTestID{Type: e.Code.Value, Code: *e.Result.Value},
			Description: e.Result.String,
			Result:      classifyNVMe(*e.Result.Value),
		}
		if e.PowerOnHours != nil {
			entry.ID.Hours, entry.hasHours = *e.PowerOnHours, true
		}
		entries = append(entries, entry)
	}

	return entries, true
}

// classifyATA classifies the status nibble of an ATA log entry; every code
// that is not a pass, an abort or in progress counts as a failure, reserved
// codes included.
func classifyATA(code int) SelfTestResult {
	switch {
	case code == 0:
		return SelfTestPassed
	case code <= ataSelfTestAbortedLast:
		return SelfTestAborted
	case code == ataSelfTestInProgress:
		return SelfTestInProgress
	default:
		return SelfTestFailed
	}
}

// classifyNVMe classifies a self_test_result.value; every code that is not a
// pass, an abort or an unused entry counts as a failure, reserved codes
// included.
func classifyNVMe(code int) SelfTestResult {
	switch {
	case code == 0:
		return SelfTestPassed
	case code <= nvmeSelfTestAbortedMax:
		return SelfTestAborted
	case code == nvmeSelfTestUnused:
		return SelfTestSkipped
	default:
		return SelfTestFailed
	}
}

// gaps lists the sections the read lacked. A SCSI/SAS drive needs nothing
// beyond the verdict. A self-test log that is present but undecodable is no
// gap.
func gaps(r *Report, raw *smartJSON) []Gap {
	var out []Gap
	if r.Bus == BusATA {
		if raw.ATAAttributes.Table == nil {
			out = append(out, GapAttributes)
		}
		if raw.ATASelfTest == nil {
			out = append(out, GapSelfTestLog)
		}
	}
	if r.Bus == BusNVMe {
		if raw.NVMeHealth == nil {
			out = append(out, GapNVMeHealthLog)
		}
		if raw.NVMeSelfTest == nil {
			out = append(out, GapSelfTestLog)
		}
	}
	if r.Temperature == nil {
		out = append(out, GapTemperature)
	}

	return out
}

// wearUsed converts a normalised wear-levelling value (100 new, falling) to
// the percentage used; a value outside 0-100 is not a percentage.
func wearUsed(value *int) *int {
	if value == nil || *value < 0 || *value > percentFull {
		return nil
	}
	used := percentFull - *value

	return &used
}

func tracked(counters map[int]bool, id int) bool {
	return counters[id] && id >= 1 && id <= maxATAID
}
