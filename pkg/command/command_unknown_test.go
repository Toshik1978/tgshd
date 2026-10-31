package command

import (
	"context"
	"errors"
	"slices"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/Toshik1978/tgshd/pkg/telegram/route"
)

type fakeScript struct {
	name       string
	nameErr    error
	nameCalls  int
	executeErr error
	executed   bool
	lastCmd    string
	lastThread int
}

func (f *fakeScript) List(_ context.Context) (string, error) {
	f.nameCalls++
	return f.name, f.nameErr
}

func (f *fakeScript) Execute(_ context.Context, cmd string, threadID int) error {
	f.executed = true
	f.lastThread = threadID
	f.lastCmd = cmd
	return f.executeErr
}

func TestUnknownCommandsParsedFromScriptOutput(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	script := &fakeScript{name: "alpha - First\nbroken line\n/beta - Second\nalso broken\n"}
	cmd, err := NewUnknownCommand(zap.New(core), &fakePublisher{}, script)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := []route.BotCommand{{Name: "alpha", Description: "First"}, {Name: "beta", Description: "Second"}}
	if got := cmd.Commands(); !slices.Equal(got, want) {
		t.Errorf("Commands() = %+v, want %+v", got, want)
	}
	if n := logs.Len(); n != 2 {
		t.Errorf("logged %d warnings, want one per problem (2)", n)
	}
	cmd.CustomScript()
}

func TestUnknownCommandsResolvedOnceAtConstruction(t *testing.T) {
	script := &fakeScript{name: "custom - Custom"}
	cmd, err := NewUnknownCommand(zap.NewNop(), &fakePublisher{}, script)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Commands() must be served from the value resolved once at construction
	// and must never re-run the (side-effectful) script.
	_ = cmd.Commands()
	_ = cmd.Commands()

	if script.nameCalls != 1 {
		t.Errorf("Script.List() called %d times, want exactly 1 (resolved at construction)", script.nameCalls)
	}
}

func TestUnknownConstructorErrorOnScriptNameError(t *testing.T) {
	script := &fakeScript{nameErr: errors.New("boom")}

	_, err := NewUnknownCommand(zap.NewNop(), &fakePublisher{}, script)
	if err == nil {
		t.Fatal("expected NewUnknownCommand to return an error when Script.List() fails")
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

	if err := cmd.Handle(context.Background(), testOrigin(55), "/whatever arg1"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !script.executed || script.lastCmd != "/whatever arg1" {
		t.Errorf(
			"expected Execute called with the raw command, got executed=%v cmd=%q",
			script.executed,
			script.lastCmd,
		)
	}
	if script.lastThread != 3 {
		t.Errorf("expected Execute called with thread 3, got %d", script.lastThread)
	}
	if pub.count != 1 || pub.lastDest != (route.Destination{ChatID: 55, ThreadID: 3, Silent: true}) {
		t.Errorf("expected one reply to sender 55, got count=%d dest=%+v", pub.count, pub.lastDest)
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

	handleErr := cmd.Handle(context.Background(), testOrigin(55), "/whatever")
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

	handleErr := cmd.Handle(context.Background(), testOrigin(55), "/whatever")
	if handleErr == nil {
		t.Fatal("expected an error when publish fails")
	}
}
