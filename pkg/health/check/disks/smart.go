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
)

// Counter keys for the NVMe error counters in Report.Counters. ATA counters
// are keyed with ATACounter.
const (
	CounterNVMeMediaErrors = "nvme.media_errors"
	CounterNVMeErrorLog    = "nvme.num_err_log_entries"
)

// smartctl exit status bits (see smartctl(8)).
const (
	exitCommandLine = 1 << 0 // command line did not parse
	exitDeviceOpen  = 1 << 1 // device open failed / no SMART support
	exitDiskFailing = 1 << 3 // SMART status check returned DISK FAILING

	// NVMe data units are thousands of 512-byte blocks.
	nvmeDataUnitBytes = 512000

	// Self-test result classification.
	nvmeSelfTestFatal      = 5
	nvmeSelfTestSegments   = 7
	ataSelfTestFatalFirst  = 3
	ataSelfTestFatalLast   = 8
	ataSelfTestStatusShift = 4

	// ATA attribute 241: total LBAs written.
	ataTotalLBAsWritten = 241
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

	// HealthFailed is set by smart_status.passed=false or exit status bit 3.
	HealthFailed bool

	Temperature      *int
	PowerOnHours     *int64
	PercentageUsed   *int
	AvailableSpare   *int
	SpareThreshold   *int
	CriticalWarning  *int
	DataWrittenBytes *uint64
	SelfTest         *SelfTest

	// Counters holds the error counters that should never rise, keyed by
	// ATACounter(id) or the CounterNVMe* constants.
	Counters map[string]uint64
}

// ATACounter returns the Report.Counters key of an ATA attribute.
func ATACounter(id int) string {
	return "ata." + strconv.Itoa(id)
}

func trackedATA(id int) bool {
	switch id {
	case 5, 187, 188, 197, 198, 199:
		return true
	}

	return false
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
	LogicalBlock *uint64 `json:"logical_block_size"`
	ModelName    string  `json:"model_name"`
	SerialNumber string  `json:"serial_number"`
	SmartStatus  *struct {
		Passed *bool `json:"passed"`
	} `json:"smart_status"`
	ATAAttributes struct {
		Table []struct {
			ID  int `json:"id"`
			Raw *struct {
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
		Standard struct {
			Table []struct {
				LifetimeHours *int64 `json:"lifetime_hours"`
				Status        struct {
					Value  int    `json:"value"`
					String string `json:"string"`
					Passed *bool  `json:"passed"`
				} `json:"status"`
			} `json:"table"`
		} `json:"standard"`
	} `json:"ata_smart_self_test_log"`
	NVMeSelfTest *struct {
		Table []struct {
			PowerOnHours *int64 `json:"power_on_hours"`
			Result       struct {
				Value  int    `json:"value"`
				String string `json:"string"`
			} `json:"self_test_result"`
		} `json:"table"`
	} `json:"nvme_self_test_log"`
}

// DecodeSMART decodes smartctl JSON output together with the process exit
// status. Exit bits 0-1 (command line / device open failure) yield an error
// wrapping ErrUnreadable; bit 3 marks the drive failing; other bits are
// informational and the JSON is decoded regardless.
func DecodeSMART(stdout []byte, exitStatus int) (Report, error) {
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
	r.HealthFailed = exitStatus&exitDiskFailing != 0 || raw.healthFailed()
	r.Temperature, r.PowerOnHours = raw.generic()

	applyATA(&r, &raw)
	applyNVMe(&r, &raw)
	r.SelfTest = newestSelfTest(&raw)

	return r, nil
}

func openError(raw *smartJSON, jsonErr error, exitStatus int) error {
	msg := "smartctl exit status " + strconv.Itoa(exitStatus)
	if jsonErr == nil && len(raw.Smartctl.Messages) > 0 {
		msg += ": " + raw.Smartctl.Messages[0].String
	}

	return fmt.Errorf("%w: %s", ErrUnreadable, msg)
}

func (s *smartJSON) healthFailed() bool {
	return s.SmartStatus != nil && s.SmartStatus.Passed != nil && !*s.SmartStatus.Passed
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

// applyATA collects the tracked counters and the data written. An attribute
// row without a raw value is "not reported". Attribute 188 (command timeout)
// may pack several 16-bit counters into raw.value on some drives; it is used
// as-is for rise detection, without masking.
func applyATA(r *Report, raw *smartJSON) {
	for _, a := range raw.ATAAttributes.Table {
		if a.Raw == nil || a.Raw.Value == nil {
			continue
		}
		if trackedATA(a.ID) {
			r.Counters[ATACounter(a.ID)] = *a.Raw.Value
		}
		if a.ID == ataTotalLBAsWritten && raw.LogicalBlock != nil {
			w := *a.Raw.Value * *raw.LogicalBlock
			r.DataWrittenBytes = &w
		}
	}
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
	if h.ErrLogEntries != nil {
		r.Counters[CounterNVMeErrorLog] = *h.ErrLogEntries
	}
}

// newestSelfTest returns the newest finished self-test log entry (the logs
// list the newest first), or nil.
func newestSelfTest(raw *smartJSON) *SelfTest {
	if st := newestATASelfTest(raw); st != nil {
		return st
	}

	return newestNVMeSelfTest(raw)
}

func newestATASelfTest(raw *smartJSON) *SelfTest {
	if raw.ATASelfTest != nil {
		for _, e := range raw.ATASelfTest.Standard.Table {
			code := e.Status.Value >> ataSelfTestStatusShift
			switch {
			case code == 0:
				return &SelfTest{Description: e.Status.String, PowerOnHours: e.LifetimeHours}
			case code >= ataSelfTestFatalFirst && code <= ataSelfTestFatalLast:
				return &SelfTest{Description: e.Status.String, Failed: true, PowerOnHours: e.LifetimeHours}
			}
		}
	}

	return nil
}

func newestNVMeSelfTest(raw *smartJSON) *SelfTest {
	if raw.NVMeSelfTest != nil {
		for _, e := range raw.NVMeSelfTest.Table {
			switch v := e.Result.Value; {
			case v == 0:
				return &SelfTest{Description: e.Result.String, PowerOnHours: e.PowerOnHours}
			case v >= nvmeSelfTestFatal && v <= nvmeSelfTestSegments:
				return &SelfTest{Description: e.Result.String, Failed: true, PowerOnHours: e.PowerOnHours}
			}
		}
	}

	return nil
}
