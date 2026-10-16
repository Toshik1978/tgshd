package config

// BoardSensor selects board sensors by hwmon chip and label; both accept a "*" glob.
// An empty Label selects every label of the chip. A missing Limit is filled with
// Limits.Board when the configuration is validated, so it is non-nil afterwards.
type BoardSensor struct {
	Chip  string `yaml:"chip"`
	Label string `yaml:"label"`
	Limit *int   `yaml:"limit"`
}
