package config

import (
	"time"

	"github.com/Toshik1978/tgshd/pkg/health"
)

// Default values for every section, as shown in the configuration contract.

func common(interval, threshold time.Duration, mode Mode) Common {
	return Common{Interval: Duration(interval), Threshold: Duration(threshold), Mode: mode, PauseInQuietWindows: true}
}

// hardwareCommon is common for the hardware checks, which keep running through
// quiet windows unless pause_in_quiet_windows is set.
func hardwareCommon(interval, threshold time.Duration, mode Mode) Common {
	c := common(interval, threshold, mode)
	c.PauseInQuietWindows = false

	return c
}

func defaultGeneral() General {
	return General{
		StateDir:      "/var/lib/tgshd",
		HeartbeatFile: "/run/tgshd/heartbeat",
		BootGrace:     Duration(10 * time.Minute),
		Concurrency:   4,

		MaintenanceMaxHold:   Duration(6 * time.Hour),
		TelegramFailingAfter: Duration(15 * time.Minute),
	}
}

func defaultSummary() Summary {
	return Summary{WeekdayName: "monday", Time: "09:00"}
}

// defaultNetwork's threshold is its interval; a threshold left out of the
// config follows a configured interval instead (see networkSection, FR-035).
func defaultNetwork() Network {
	return Network{Common: common(5*time.Minute, 5*time.Minute, ModeSMS)}
}

func defaultUPS() UPS {
	return UPS{Common: common(5*time.Minute, 5*time.Minute, ModeTLG)}
}

func defaultDisks() Disks {
	return Disks{
		Common:            hardwareCommon(15*time.Minute, 0, ModeTLG),
		WearPercent:       80,
		SelftestMaxAge:    Duration(40 * day),
		SpareFloorPercent: 20,
		ForceReadAfter:    Duration(24 * time.Hour),
	}
}

func defaultTemperatures() Temperatures {
	return Temperatures{
		Common:         hardwareCommon(15*time.Minute, 0, ModeTLG),
		Limits:         Limits{CPU: 90, DriveNVMe: 60, DriveSATA: 60, GPU: 85, Board: 75},
		LimitsBySerial: map[string]int{},
		BoardSensors:   []BoardSensor{},
	}
}

func defaultKernel() Kernel {
	return Kernel{Common: hardwareCommon(15*time.Minute, 0, ModeTLG)}
}

func defaultContainers() Containers {
	return Containers{Common: common(5*time.Minute, 15*time.Minute, ModeTLG)}
}

func defaultHTTP() HTTP {
	return HTTP{
		Common:  common(10*time.Minute, 10*time.Minute, ModeTLG),
		Timeout: Duration(15 * time.Second),
		Status:  [2]int{200, 399},

		AuthRefusal: AuthRefusalDown,
	}
}

func defaultFreshness() Freshness {
	return Freshness{
		Common:  common(15*time.Minute, 0, ModeTLG),
		Content: ContentEpoch,

		FutureTolerance: Duration(time.Hour),
		Timeout:         Duration(15 * time.Second),
	}
}

func defaultCommand() Command {
	return Command{
		Common:  common(5*time.Minute, 0, ModeTLG),
		Timeout: Duration(health.MaxCheckTimeout),
		Report:  ReportAlert,

		TimeoutVerdict: TimeoutBroken,
	}
}

func defaultModemProbe() ModemProbe {
	return ModemProbe{
		WeekdayName: "sunday",
		Time:        "11:02",
		ReplyWithin: Duration(30 * time.Minute),
		MaxSilence:  Duration(8 * day),
	}
}

func defaultFix() Fix {
	return Fix{After: 2, MaxPerHour: 3, GraceAfterStart: Duration(2 * time.Minute)}
}

func defaultMemory() Memory {
	return Memory{
		Interval:            Duration(time.Minute),
		Threshold:           Duration(5 * time.Minute),
		Mode:                ModeTLG,
		PauseInQuietWindows: true,

		WarnAvailablePercent: 15,
		WarnSwapPercent:      80,
		WarnPressurePercent:  10,
		GrowthWindow:         Duration(time.Hour),
		GrowthWarn:           4 << 30,
		AuditExclude:         []string{},

		Guard: Guard{
			Intervene:              InterveneReport,
			NeverTouch:             []string{},
			DangerAvailablePercent: 5,
			DangerPressurePercent:  20,
			DangerFor:              Duration(30 * time.Second),
			CulpritWindow:          Duration(10 * time.Minute),
			CulpritMinGrowth:       1 << 30,
			RepeatWindow:           Duration(6 * time.Hour),
			RecoveryWindow:         Duration(2 * time.Minute),
			ActionTimeout:          Duration(time.Minute),
			FailedMode:             ModeSMS,
		},
	}
}
