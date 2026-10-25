package config

import "time"

// Containers is the container health check.
type Containers struct {
	Common `yaml:",inline"`

	Exclude []string `yaml:"exclude"`
}

// Header is an HTTP header whose value comes from an environment variable.
type Header struct {
	Name     string `yaml:"name"`
	ValueEnv string `yaml:"value_env"`
	Prefix   string `yaml:"prefix"`
	// Placeholders are extra markers that mean the secret was never set; a
	// header whose value contains one (case-insensitive) is a checker error.
	Placeholders []string `yaml:"placeholders"`
}

// HTTP is one endpoint check.
type HTTP struct {
	Common `yaml:",inline"`

	Name      string   `yaml:"name"`
	URL       string   `yaml:"url"`
	Timeout   Duration `yaml:"timeout"`
	Status    [2]int   `yaml:"status"`
	ConnectTo string   `yaml:"connect_to"`
	DoH       string   `yaml:"doh"`
	Header    *Header  `yaml:"header"`
	DependsOn string   `yaml:"depends_on"`
	Container string   `yaml:"container"`

	FollowRedirects bool        `yaml:"follow_redirects"`
	AuthRefusal     AuthRefusal `yaml:"auth_refusal"`
}

// Freshness is one file-age check.
type Freshness struct {
	Common `yaml:",inline"`

	Name    string   `yaml:"name"`
	Path    string   `yaml:"path"`
	Content Content  `yaml:"content"`
	MaxAge  Duration `yaml:"max_age"`

	FutureTolerance Duration `yaml:"future_tolerance"`
	// URL reads the stamp over HTTP instead of from Path (exactly one is set).
	// Timeout and Header apply to URL only; Timeout defaults to 15s.
	URL     string   `yaml:"url"`
	Timeout Duration `yaml:"timeout"`
	Header  *Header  `yaml:"header"`
}

// Fix is the optional auto-remediation of a command check. After,
// MaxPerHour and GraceAfterStart take their defaults (2, 3, 2m) when zero.
type Fix struct {
	After            int      `yaml:"after"`
	RestartContainer string   `yaml:"restart_container"`
	Run              []string `yaml:"run"`
	MaxPerHour       int      `yaml:"max_per_hour"`
	GraceAfterStart  Duration `yaml:"grace_after_start"`
}

// Command is one custom-command check.
type Command struct {
	Common `yaml:",inline"`

	Name    string   `yaml:"name"`
	Run     []string `yaml:"run"`
	Timeout Duration `yaml:"timeout"`
	Fix     *Fix     `yaml:"fix"`
	Report  Report   `yaml:"report"`

	// ExpectOutput must match the probe output; ForbidOutput must not (Go RE2).
	ExpectOutput   string         `yaml:"expect_output"`
	ForbidOutput   string         `yaml:"forbid_output"`
	TimeoutVerdict TimeoutVerdict `yaml:"timeout_verdict"`
}

// ModemProbe is the periodic modem round-trip probe.
type ModemProbe struct {
	PhoneEnv    string       `yaml:"phone_env"`
	WeekdayName string       `yaml:"weekday"`
	Time        string       `yaml:"time"`
	ReplyWithin Duration     `yaml:"reply_within"`
	MaxSilence  Duration     `yaml:"max_silence"`
	Weekday     time.Weekday `yaml:"-"`
}

// Memory is the memory check and, nested under Guard, the independent memory
// guard. It takes the common keys that apply to a warning check, but not
// needs_internet, depends_on or summary_group, so it does not embed Common.
type Memory struct {
	Interval  Duration `yaml:"interval"`
	Threshold Duration `yaml:"threshold"`
	Mode      Mode     `yaml:"mode"`
	// PauseInQuietWindows holds the warnings back during quiet windows; the
	// guard never pauses.
	PauseInQuietWindows bool `yaml:"pause_in_quiet_windows"`

	WarnAvailablePercent int      `yaml:"warn_available_percent"`
	WarnSwapPercent      int      `yaml:"warn_swap_percent"`
	WarnPressurePercent  int      `yaml:"warn_pressure_percent"`
	GrowthWindow         Duration `yaml:"growth_window"`
	GrowthWarn           Size     `yaml:"growth_warn"`
	// AuditExclude lists containers allowed to run without a memory limit.
	AuditExclude []string `yaml:"audit_exclude"`

	Guard Guard `yaml:"guard"`
}

// Guard configures the memory guard. FailedMode is the delivery of reports
// that tgshd could not fix the problem.
type Guard struct {
	Intervene Intervene `yaml:"intervene"`
	// NeverTouch lists container names or systemd unit names that the guard
	// never stops or restarts. A unit may be named without ".service".
	NeverTouch []string `yaml:"never_touch"`

	DangerAvailablePercent int      `yaml:"danger_available_percent"`
	DangerPressurePercent  int      `yaml:"danger_pressure_percent"`
	DangerFor              Duration `yaml:"danger_for"`
	CulpritWindow          Duration `yaml:"culprit_window"`
	CulpritMinGrowth       Size     `yaml:"culprit_min_growth"`
	// RepeatWindow 0 disables the stop rule (always restart).
	RepeatWindow   Duration `yaml:"repeat_window"`
	RecoveryWindow Duration `yaml:"recovery_window"`
	ActionTimeout  Duration `yaml:"action_timeout"`
	FailedMode     Mode     `yaml:"failed_mode"`
}
