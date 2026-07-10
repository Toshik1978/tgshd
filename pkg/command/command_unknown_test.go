package command

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/zap"
)

type fakeScript struct {
	name       string
	nameErr    error
	executeErr error
	executed   bool
	lastCmd    string
}

func (f *fakeScript) Name() (string, error) {
	return f.name, f.nameErr
}

func (f *fakeScript) Execute(_ context.Context, cmd string) error {
	f.executed = true
	f.lastCmd = cmd
	return f.executeErr
}

func TestUnknownName(t *testing.T) {
	script := &fakeScript{name: "custom-unknown"}
	cmd := NewUnknownCommand(zap.NewNop(), &fakePublisher{}, script)
	if got := cmd.Name(); got != "custom-unknown" {
		t.Errorf("Name() = %q, want %q", got, "custom-unknown")
	}
}

func TestUnknownEnabled(t *testing.T) {
	cmd := NewUnknownCommand(zap.NewNop(), &fakePublisher{}, &fakeScript{})
	if !cmd.Enabled() {
		t.Error("Enabled() = false, want true")
	}
}

func TestUnknownHandleHappyPath(t *testing.T) {
	pub := &fakePublisher{}
	script := &fakeScript{}
	cmd := NewUnknownCommand(zap.NewNop(), pub, script)

	if err := cmd.Handle(context.Background(), 55, "/whatever arg1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !script.executed || script.lastCmd != "/whatever arg1" {
		t.Errorf(
			"expected Execute called with the raw command, got executed=%v cmd=%q",
			script.executed,
			script.lastCmd,
		)
	}
	if pub.count != 1 || pub.lastRecipient != 55 {
		t.Errorf("expected one reply to sender 55, got count=%d recipient=%d", pub.count, pub.lastRecipient)
	}
	if pub.lastMsg != "Script succeeded!" {
		t.Errorf("expected reply %q, got %q", "Script succeeded!", pub.lastMsg)
	}
}

func TestUnknownHandleExecuteError(t *testing.T) {
	pub := &fakePublisher{}
	script := &fakeScript{executeErr: errors.New("boom")}
	cmd := NewUnknownCommand(zap.NewNop(), pub, script)

	err := cmd.Handle(context.Background(), 55, "/whatever")
	if err == nil {
		t.Fatal("expected an error when Execute fails")
	}
	if pub.count != 0 {
		t.Error("publisher must not be called when Execute fails")
	}
}

func TestUnknownNamePanicsOnScriptNameError(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected Name() to panic when Script.Name() fails")
		}
	}()

	script := &fakeScript{nameErr: errors.New("boom")}
	cmd := NewUnknownCommand(zap.NewNop(), &fakePublisher{}, script)
	cmd.Name()
}

func TestUnknownHandlePublishError(t *testing.T) {
	pub := &errPublisher{err: errors.New("publish failed")}
	script := &fakeScript{}
	cmd := NewUnknownCommand(zap.NewNop(), pub, script)

	err := cmd.Handle(context.Background(), 55, "/whatever")
	if err == nil {
		t.Fatal("expected an error when publish fails")
	}
}
