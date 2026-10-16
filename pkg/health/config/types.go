package config

import "time"

// Config is the typed health configuration. A nil pointer section or an empty
// list means the check type is absent from the file and must not run.
type Config struct {
	General      General
	Summary      *Summary
	Network      *Network
	UPS          *UPS
	Disks        *Disks
	Temperatures *Temperatures
	Kernel       *Kernel
	Containers   *Containers
	HTTP         []HTTP
	Freshness    []Freshness
	Command      []Command
	ModemProbe   *ModemProbe
}

// General holds daemon-wide settings. It is always present.
type General struct {
	StateDir        string   `yaml:"state_dir"`
	HeartbeatFile   string   `yaml:"heartbeat_file"`
	MaintenanceLock string   `yaml:"maintenance_lock"`
	BootGrace       Duration `yaml:"boot_grace"`
	Concurrency     int      `yaml:"concurrency"`
}

// Summary configures the weekly report.
type Summary struct {
	WeekdayName string       `yaml:"weekday"`
	Time        string       `yaml:"time"`
	Weekday     time.Weekday `yaml:"-"`
}

// Common holds the keys shared by every check.
type Common struct {
	Interval      Duration `yaml:"interval"`
	Threshold     Duration `yaml:"threshold"`
	Mode          Mode     `yaml:"mode"`
	NeedsInternet bool     `yaml:"needs_internet"`
	// SummaryGroup merges the checks that name the same group into one line
	// of the weekly summary; empty keeps one line per check.
	SummaryGroup string `yaml:"summary_group"`
}

// Network is the connectivity check, evaluated first every cycle.
type Network struct {
	Common `yaml:",inline"`

	InternetTargets []string `yaml:"internet_targets"`
}

// UPS is the power check (connection settings come from BOT_NUT_*).
type UPS struct {
	Common `yaml:",inline"`
}

// Disks is the SMART check.
type Disks struct {
	Common `yaml:",inline"`

	WearPercent    int      `yaml:"wear_percent"`
	SelftestMaxAge Duration `yaml:"selftest_max_age"`
	ExcludeSerials []string `yaml:"exclude_serials"`

	SpareFloorPercent     int              `yaml:"spare_floor_percent"`
	ForceReadAfter        Duration         `yaml:"force_read_after"`
	IncludeSerials        []string         `yaml:"include_serials"`
	ExtraCounters         []int            `yaml:"extra_counters"`
	ExtraCountersBySerial map[string][]int `yaml:"extra_counters_by_serial"`
}

// Limits are the temperature limits in degrees Celsius; a PROBLEM is raised at or above.
type Limits struct {
	CPU       int `yaml:"cpu"`
	DriveNVMe int `yaml:"drive_nvme"`
	DriveSATA int `yaml:"drive_sata"`
	GPU       int `yaml:"gpu"`
	Board     int `yaml:"board"`
}

// Temperatures is the thermal check. The drive limits are judged by the disks check.
type Temperatures struct {
	Common `yaml:",inline"`

	Limits Limits `yaml:"limits"`

	// LimitsBySerial overrides the drive limit for one drive serial.
	LimitsBySerial map[string]int `yaml:"limits_by_serial"`
	// BoardSensors lists the board sensors that are judged against a limit.
	BoardSensors []BoardSensor `yaml:"board_sensors"`
}

// Kernel is the EDAC/kmsg check.
type Kernel struct {
	Common `yaml:",inline"`
}
