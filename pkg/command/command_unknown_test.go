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
	nameCalls  int
	executeErr error
	executed   bool
	lastCmd    string
}

func (f *fakeScript) Name(_ context.Context) (string, error) {
	f.nameCalls++
	return f.name, f.nameErr
}

func (f *fakeScript) Execute(_ context.Context, cmd string) error {
	f.executed = true
	f.lastCmd = cmd
	return f.executeErr
}

func TestUnknownName(t *testing.T) {
	script := &fakeScript{name: "custom-unknown"}
	cmd, err := NewUnknownCommand(zap.NewNop(), &fakePublisher{}, script)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cmd.Name(); got != "custom-unknown" {
		t.Errorf("Name() = %q, want %q", got, "custom-unknown")
	}
}

func TestUnknownNameResolvedOnceAtConstruction(t *testing.T) {
	script := &fakeScript{name: "custom-unknown"}
	cmd, err := NewUnknownCommand(zap.NewNop(), &fakePublisher{}, script)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Name() must be served from the value resolved once at construction and
	// must never re-run the (side-effectful) script on the dispatch hot path.
	_ = cmd.Name()
	_ = cmd.Name()

	if script.nameCalls != 1 {
		t.Errorf("Script.Name() called %d times, want exactly 1 (resolved at construction)", script.nameCalls)
	}
}

func TestUnknownConstructorErrorOnScriptNameError(t *testing.T) {
	script := &fakeScript{nameErr: errors.New("boom")}

	_, err := NewUnknownCommand(zap.NewNop(), &fakePublisher{}, script)
	if err == nil {
		t.Fatal("expected NewUnknownCommand to return an error when Script.Name() fails")
	}
}

func TestUnknownEnabled(t *testing.T) {
	cmd, err := NewUnknownCommand(zap.NewNop(), &fakePublisher{}, &fakeScript{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cmd.Enabled() {
		t.Error("Enabled() = false, want true")
	}
}

func TestUnknownHandleHappyPath(t *testing.T) {
	pub := &fakePublisher{}
	script := &fakeScript{}
	cmd, err := NewUnknownCommand(zap.NewNop(), pub, script)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

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
	cmd, err := NewUnknownCommand(zap.NewNop(), pub, script)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	handleErr := cmd.Handle(context.Background(), 55, "/whatever")
	if handleErr == nil {
		t.Fatal("expected an error when Execute fails")
	}
	if pub.count != 0 {
		t.Error("publisher must not be called when Execute fails")
	}
}

func TestUnknownHandlePublishError(t *testing.T) {
	pub := &errPublisher{err: errors.New("publish failed")}
	script := &fakeScript{}
	cmd, err := NewUnknownCommand(zap.NewNop(), pub, script)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	handleErr := cmd.Handle(context.Background(), 55, "/whatever")
	if handleErr == nil {
		t.Fatal("expected an error when publish fails")
	}
}
