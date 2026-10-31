package command

import (
	"slices"
	"testing"

	"go.uber.org/zap"

	"github.com/Toshik1978/tgshd/pkg/telegram/route"
)

type commander interface {
	Commands() []route.BotCommand
}

func TestHandlersReturnTheirBuiltinEntry(t *testing.T) {
	lg := zap.NewNop()
	pub := &fakePublisher{}
	cases := []struct {
		handler  commander
		wantName string
	}{
		{NewPingCommand(lg, pub, nil, nil), "ping"},
		{NewPowerCommand(lg, pub, nil), "power"},
		{NewSpeedCommand(lg, pub, nil), "speed"},
		{NewSmsCommand(lg, pub, nil, 1), "sms"},
		{NewStatusCommand(lg, pub, nil, nil), "status"},
		{NewClearCommand(lg, pub, nil), "clear"},
		{NewRetireCommand(lg, pub, nil), "retire"},
		{NewHistoryCommand(lg, pub, nil), "history"},
		{NewTempCommand(lg, pub, nil, nil), "temp"},
		{NewDisksCommand(lg, pub, nil, nil), "disks"},
		{NewNetworkCommand(lg, pub, nil, "5g"), "5g"},
		{NewNetworkCommand(lg, pub, nil, "4g"), "4g"},
		{NewNetworkCommand(lg, pub, nil, "3g"), "3g"},
	}
	for _, c := range cases {
		t.Run(c.wantName, func(t *testing.T) {
			want, ok := BuiltinCommand(c.wantName)
			if !ok {
				t.Fatalf("no built-in entry for %q", c.wantName)
			}
			got := c.handler.Commands()
			if !slices.Equal(got, []route.BotCommand{want}) {
				t.Errorf("Commands() = %+v, want [%+v]", got, want)
			}
		})
	}
}

func TestBuiltinCommandsAreValid(t *testing.T) {
	all := BuiltinCommands()
	if len(all) != 13 {
		t.Errorf("BuiltinCommands() has %d entries, want 13", len(all))
	}
	for _, c := range all {
		if err := c.Valid(); err != nil {
			t.Errorf("built-in %q is invalid: %v", c.Name, err)
		}
		if got, ok := BuiltinCommand(c.Name); !ok || got != c {
			t.Errorf("BuiltinCommand(%q) = %+v, %v; want %+v", c.Name, got, ok, c)
		}
	}
	if _, ok := BuiltinCommand("nope"); ok {
		t.Error("BuiltinCommand(\"nope\") found an entry")
	}
}
