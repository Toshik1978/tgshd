package route

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestCommandName(t *testing.T) {
	tests := []struct {
		name string
		text string
		bot  string
		want string
		ok   bool
	}{
		{"plain", "/ping", "tgshd_test_bot", "/ping", true},
		{"plain with args", "/ping 1.1.1.1", "tgshd_test_bot", "/ping", true},
		{"own bot", "/ping@tgshd_test_bot", "tgshd_test_bot", "/ping", true},
		{"own bot with args", "/ping@tgshd_test_bot 1.1.1.1", "tgshd_test_bot", "/ping", true},
		{"own bot case-insensitive", "/ping@TgShd_Test_Bot", "tgshd_test_bot", "/ping", true},
		{"other bot", "/ping@other_bot", "tgshd_test_bot", "", false},
		{"other bot with args", "/ping@other_bot 1.1.1.1", "tgshd_test_bot", "", false},
		{"empty text", "", "tgshd_test_bot", "", false},
		{"blank text", "   ", "tgshd_test_bot", "", false},
		{"not a command", "ping", "tgshd_test_bot", "", false},
		{"empty suffix", "/ping@", "tgshd_test_bot", "", false},
		{"unknown bot username", "/ping@tgshd_test_bot", "", "", false},
		{"leading space", "  /ping", "tgshd_test_bot", "/ping", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := CommandName(tt.text, tt.bot)
			if got != tt.want || ok != tt.ok {
				t.Errorf("CommandName(%q, %q) = (%q, %v), want (%q, %v)", tt.text, tt.bot, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestBotCommandValid(t *testing.T) {
	tests := []struct {
		name    string
		cmd     BotCommand
		wantErr bool
	}{
		{"simple", BotCommand{"ping", "Ping a host"}, false},
		{"digits and underscore", BotCommand{"a_1", "x"}, false},
		{"32 char name", BotCommand{strings.Repeat("a", 32), "x"}, false},
		{"33 char name", BotCommand{strings.Repeat("a", 33), "x"}, true},
		{"empty name", BotCommand{"", "x"}, true},
		{"leading slash", BotCommand{"/ping", "x"}, true},
		{"uppercase", BotCommand{"Ping", "x"}, true},
		{"dash", BotCommand{"my-cmd", "x"}, true},
		{"empty description", BotCommand{"ping", ""}, true},
		{"256 char description", BotCommand{"ping", strings.Repeat("a", 256)}, false},
		{"257 char description", BotCommand{"ping", strings.Repeat("a", 257)}, true},
		{"256 multibyte runes", BotCommand{"ping", strings.Repeat("я", 256)}, false},
		{"257 multibyte runes", BotCommand{"ping", strings.Repeat("я", 257)}, true},
		{"newline", BotCommand{"ping", "a\nb"}, true},
		{"carriage return", BotCommand{"ping", "a\rb"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.cmd.Valid(); (err != nil) != tt.wantErr {
				t.Errorf("Valid() = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func names(cmds []BotCommand) []string {
	out := make([]string, 0, len(cmds))
	for _, c := range cmds {
		out = append(out, c.Name)
	}

	return out
}

func TestBuildCommands(t *testing.T) {
	many := func(n int) []BotCommand {
		out := make([]BotCommand, 0, n)
		for i := range n {
			out = append(out, BotCommand{fmt.Sprintf("c%03d", i), "d"})
		}

		return out
	}
	tests := []struct {
		name         string
		builtins     []BotCommand
		script       []BotCommand
		wantRouted   []string
		wantMenu     int
		wantProblems []string // substrings, one per problem in order
	}{
		{"empty", nil, nil, []string{}, 0, nil},
		{
			"builtins sorted then script in order",
			[]BotCommand{{"zeta", "z"}, {"alpha", "a"}},
			[]BotCommand{{"yy", "y"}, {"bb", "b"}},
			[]string{"alpha", "zeta", "yy", "bb"},
			4, nil,
		},
		{
			"builtin beats script",
			[]BotCommand{{"ping", "p"}},
			[]BotCommand{{"ping", "other"}, {"extra", "e"}},
			[]string{"ping", "extra"},
			2,
			[]string{"ping"},
		},
		{
			"repeat inside script",
			nil,
			[]BotCommand{{"a", "1"}, {"a", "2"}},
			[]string{"a"},
			1,
			[]string{"a"},
		},
		{
			"invalid dropped",
			[]BotCommand{{"ok", "fine"}},
			[]BotCommand{{"Bad", "x"}, {"good", ""}, {"fine", "y"}},
			[]string{"ok", "fine"},
			2,
			[]string{"Bad", "good"},
		},
		{"exactly 100", many(100), nil, nil, 100, nil},
		{"101 capped", many(101), nil, nil, 100, []string{"100"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			routed, menu, problems := BuildCommands(tt.builtins, tt.script)
			if tt.wantRouted != nil && !slices.Equal(names(routed), tt.wantRouted) {
				t.Errorf("routed = %v, want %v", names(routed), tt.wantRouted)
			}
			if tt.wantRouted == nil && len(routed) != len(tt.builtins)+len(tt.script) {
				t.Errorf("routed has %d entries", len(routed))
			}
			if len(menu) != tt.wantMenu {
				t.Errorf("menu has %d entries, want %d", len(menu), tt.wantMenu)
			}
			if !slices.Equal(names(menu), names(routed)[:len(menu)]) {
				t.Error("menu is not a prefix of routed")
			}
			if len(problems) != len(tt.wantProblems) {
				t.Fatalf("problems = %q, want %d", problems, len(tt.wantProblems))
			}
			for i, sub := range tt.wantProblems {
				if !strings.Contains(problems[i], sub) {
					t.Errorf("problem %d = %q, want it to contain %q", i, problems[i], sub)
				}
			}
		})
	}
}
