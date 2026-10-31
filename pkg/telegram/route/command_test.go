package route

import "testing"

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
