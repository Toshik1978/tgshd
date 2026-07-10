package command

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.uber.org/zap"
)

type fakePower struct {
	valid   bool
	voltage float64
	err     error
}

func (f *fakePower) Valid() bool {
	return f.valid
}

func (f *fakePower) Voltage(_ context.Context) (float64, error) {
	return f.voltage, f.err
}

func TestPowerName(t *testing.T) {
	cmd := NewPowerCommand(zap.NewNop(), &fakePublisher{}, &fakePower{})
	if got := cmd.Name(); got != "power" {
		t.Errorf("Name() = %q, want %q", got, "power")
	}
}

func TestPowerEnabled(t *testing.T) {
	cases := []struct {
		name  string
		valid bool
		want  bool
	}{
		{"valid power", true, true},
		{"invalid power", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cmd := NewPowerCommand(zap.NewNop(), &fakePublisher{}, &fakePower{valid: c.valid})
			if got := cmd.Enabled(); got != c.want {
				t.Errorf("Enabled() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestPowerHandleHappyPath(t *testing.T) {
	pub := &fakePublisher{}
	power := &fakePower{valid: true, voltage: 12.345}
	cmd := NewPowerCommand(zap.NewNop(), pub, power)

	if err := cmd.Handle(context.Background(), 42, "/power"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pub.count != 1 || pub.lastRecipient != 42 {
		t.Errorf("expected one reply to sender 42, got count=%d recipient=%d", pub.count, pub.lastRecipient)
	}
	if !strings.Contains(pub.lastMsg, "12.3") {
		t.Errorf("expected reply to contain formatted voltage, got %q", pub.lastMsg)
	}
}

func TestPowerHandleVoltageError(t *testing.T) {
	pub := &fakePublisher{}
	power := &fakePower{valid: true, err: errors.New("boom")}
	cmd := NewPowerCommand(zap.NewNop(), pub, power)

	err := cmd.Handle(context.Background(), 42, "/power")
	if err == nil {
		t.Fatal("expected an error when Voltage fails")
	}
	if pub.count != 0 {
		t.Error("publisher must not be called when Voltage fails")
	}
}

func TestPowerHandlePublishError(t *testing.T) {
	pub := &errPublisher{err: errors.New("publish failed")}
	power := &fakePower{valid: true, voltage: 5}
	cmd := NewPowerCommand(zap.NewNop(), pub, power)

	err := cmd.Handle(context.Background(), 42, "/power")
	if err == nil {
		t.Fatal("expected an error when publish fails")
	}
}
