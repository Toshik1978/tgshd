package config

import (
	"errors"
	"fmt"
	"math/bits"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Size is a byte count that is written in configuration files as a positive
// whole number followed by a unit: B, KB, MB, GB or TB (powers of 1024).
type Size uint64

// sizeUnit returns the power-of-two shift of the unit that ends s and the
// number before it. "KB" is tried before "B" so it is not read as bytes.
func sizeUnit(s string) (digits string, shift uint, ok bool) {
	for _, u := range []struct {
		suffix string
		shift  uint
	}{{"KB", 10}, {"MB", 20}, {"GB", 30}, {"TB", 40}, {"B", 0}} {
		if d, found := strings.CutSuffix(s, u.suffix); found {
			return d, u.shift, true
		}
	}

	return "", 0, false
}

// Bytes returns the value as a byte count.
func (s Size) Bytes() uint64 {
	return uint64(s)
}

// UnmarshalYAML implements yaml.Unmarshaler.
func (s *Size) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return errors.New("size must be a scalar")
	}

	v, err := parseSize(node.Value)
	if err != nil {
		return err
	}

	*s = Size(v)

	return nil
}

func parseSize(s string) (uint64, error) {
	s = strings.TrimSpace(s)

	digits, shift, ok := sizeUnit(s)
	if !ok {
		return 0, fmt.Errorf("invalid size %q: want a whole number and a unit (B, KB, MB, GB, TB)", s)
	}

	n, err := strconv.ParseUint(digits, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q: want a whole number and a unit (B, KB, MB, GB, TB)", s)
	}

	if n == 0 {
		return 0, fmt.Errorf("invalid size %q: must be greater than zero", s)
	}

	if bits.Len64(n)+int(shift) > 64 {
		return 0, fmt.Errorf("invalid size %q: too large", s)
	}

	return n << shift, nil
}
