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
}

// Freshness is one file-age check.
type Freshness struct {
	Common `yaml:",inline"`

	Name    string   `yaml:"name"`
	Path    string   `yaml:"path"`
	Content Content  `yaml:"content"`
	MaxAge  Duration `yaml:"max_age"`
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
