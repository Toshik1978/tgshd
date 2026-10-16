package config

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

const day = 24 * time.Hour

// Duration is a time.Duration that is written in configuration files with Go
// syntax ("30s", "15m", "6h") plus "d" for days ("8d", "1d12h").
type Duration time.Duration

// Duration returns the value as a time.Duration.
func (d Duration) Duration() time.Duration {
	return time.Duration(d)
}

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return errors.New("duration must be a scalar")
	}

	v, err := parseDuration(node.Value)
	if err != nil {
		return err
	}

	*d = Duration(v)

	return nil
}

func parseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "0" {
		return 0, nil
	}

	var days time.Duration

	if i := strings.IndexByte(s, 'd'); i >= 0 {
		n, err := strconv.ParseUint(s[:i], 10, 16)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q: days must be a whole number", s)
		}

		days = time.Duration(n) * day
		s = s[i+1:]
		if s == "" {
			return days, nil
		}
	}

	rest, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q: %w", s, err)
	}

	if rest < 0 && days > 0 {
		return 0, fmt.Errorf("invalid duration %q: negative remainder after days", s)
	}

	return days + rest, nil
}
