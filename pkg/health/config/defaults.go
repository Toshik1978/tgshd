package config

import (
	"time"

	"github.com/Toshik1978/tgshd/pkg/health"
)

// Default values for every section, as shown in the configuration contract.

func common(interval, threshold time.Duration, mode Mode) Common {
	return Common{Interval: Duration(interval), Threshold: Duration(threshold), Mode: mode}
}

func defaultGeneral() General {
	return General{
		StateDir:      "/var/lib/tgshd",
		HeartbeatFile: "/run/tgshd/heartbeat",
		BootGrace:     Duration(10 * time.Minute),
		Concurrency:   4,
	}
}

func defaultSummary() Summary {
	return Summary{WeekdayName: "monday", Time: "09:00"}
}

func defaultNetwork() Network {
	return Network{Common: common(5*time.Minute, 0, ModeSMS)}
}

func defaultUPS() UPS {
	return UPS{Common: common(5*time.Minute, 5*time.Minute, ModeTLG)}
}

func defaultDisks() Disks {
	return Disks{
		Common:         common(15*time.Minute, 0, ModeTLG),
		WearPercent:    80,
		SelftestMaxAge: Duration(8 * day),
	}
}

func defaultTemperatures() Temperatures {
	return Temperatures{
		Common: common(15*time.Minute, 15*time.Minute, ModeTLG),
		Limits: Limits{CPU: 85, DriveNVMe: 70, DriveSATA: 55, GPU: 85, Board: 75},
	}
}

func defaultKernel() Kernel {
	return Kernel{Common: common(15*time.Minute, 0, ModeTLG)}
}

func defaultContainers() Containers {
	return Containers{Common: common(5*time.Minute, 15*time.Minute, ModeTLG)}
}

func defaultHTTP() HTTP {
	return HTTP{
		Common:  common(10*time.Minute, 10*time.Minute, ModeTLG),
		Timeout: Duration(15 * time.Second),
		Status:  [2]int{200, 399},
	}
}

func defaultFreshness() Freshness {
	return Freshness{
		Common:  common(15*time.Minute, 0, ModeTLG),
		Content: ContentEpoch,
	}
}

func defaultCommand() Command {
	return Command{
		Common:  common(5*time.Minute, 0, ModeTLG),
		Timeout: Duration(health.MaxCheckTimeout),
		Report:  ReportAlert,
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
