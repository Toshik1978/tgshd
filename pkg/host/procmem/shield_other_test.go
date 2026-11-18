//go:build !linux

package procmem

import (
	"errors"
	"testing"
)

func TestShieldUnsupportedOffLinux(t *testing.T) {
	if err := NewShield().Apply(); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("Apply() = %v, want errors.ErrUnsupported", err)
	}
}
