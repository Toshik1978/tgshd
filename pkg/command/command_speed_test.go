package command

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.uber.org/zap"
)

type fakeSpeedtest struct {
	dl, ul float64
	err    error
}

func (f *fakeSpeedtest) Speedtest(_ context.Context) (float64, float64, error) {
	return f.dl, f.ul, f.err
}

func TestSpeedName(t *testing.T) {
	cmd := NewSpeedCommand(zap.NewNop(), &fakePublisher{}, &fakeSpeedtest{})
	if got := cmd.Name(); got != "speed" {
		t.Errorf("Name() = %q, want %q", got, "speed")
	}
}

func TestSpeedEnabled(t *testing.T) {
	cmd := NewSpeedCommand(zap.NewNop(), &fakePublisher{}, &fakeSpeedtest{})
	if !cmd.Enabled() {
		t.Error("Enabled() = false, want true")
	}
}

func TestSpeedHandleHappyPath(t *testing.T) {
	pub := &fakePublisher{}
	speedtest := &fakeSpeedtest{dl: 123.456, ul: 12.34}
	cmd := NewSpeedCommand(zap.NewNop(), pub, speedtest)

	if err := cmd.Handle(context.Background(), 7, "/speed"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pub.count != 1 || pub.lastRecipient != 7 {
		t.Errorf("expected one reply to sender 7, got count=%d recipient=%d", pub.count, pub.lastRecipient)
	}
	if !strings.Contains(pub.lastMsg, "123.46") || !strings.Contains(pub.lastMsg, "12.34") {
		t.Errorf("expected reply to contain formatted speeds, got %q", pub.lastMsg)
	}
}

func TestSpeedHandleSpeedtestError(t *testing.T) {
	pub := &fakePublisher{}
	speedtest := &fakeSpeedtest{err: errors.New("boom")}
	cmd := NewSpeedCommand(zap.NewNop(), pub, speedtest)

	err := cmd.Handle(context.Background(), 7, "/speed")
	if err == nil {
		t.Fatal("expected an error when Speedtest fails")
	}
	if pub.count != 0 {
		t.Error("publisher must not be called when Speedtest fails")
	}
}

func TestSpeedHandlePublishError(t *testing.T) {
	pub := &errPublisher{err: errors.New("publish failed")}
	speedtest := &fakeSpeedtest{dl: 1, ul: 1}
	cmd := NewSpeedCommand(zap.NewNop(), pub, speedtest)

	err := cmd.Handle(context.Background(), 7, "/speed")
	if err == nil {
		t.Fatal("expected an error when publish fails")
	}
}
