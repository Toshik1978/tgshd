package command

import (
	"slices"
	"strings"
	"testing"

	"github.com/Toshik1978/tgshd/pkg/telegram/route"
)

func TestParseScriptCommands(t *testing.T) {
	tests := []struct {
		name         string
		out          string
		want         []route.BotCommand
		wantProblems []string // substrings, one per expected problem
	}{
		{name: "empty output"},
		{name: "blank lines only", out: "\n  \r\n\t\n"},
		{
			name: "single entry",
			out:  "wake - Wake a PC: /wake <host>\n",
			want: []route.BotCommand{{Name: "wake", Description: "Wake a PC: /wake <host>"}},
		},
		{
			name: "several entries with blanks and CRLF",
			out:  "wake - Wake a PC\r\n\r\nbackup - Run the backup now\r\n",
			want: []route.BotCommand{
				{Name: "wake", Description: "Wake a PC"},
				{Name: "backup", Description: "Run the backup now"},
			},
		},
		{
			name: "leading slash dropped once",
			out:  "/wake - Wake\n//x - Twice",
			want: []route.BotCommand{
				{Name: "wake", Description: "Wake"},
				{Name: "/x", Description: "Twice"},
			},
		},
		{
			name: "trimmed name and description",
			out:  "  wake   -   Wake a PC  \n",
			want: []route.BotCommand{{Name: "wake", Description: "Wake a PC"}},
		},
		{
			name: "split on first separator only",
			out:  "a - b - c",
			want: []route.BotCommand{{Name: "a", Description: "b - c"}},
		},
		{
			name:         "old pipe format skipped",
			out:          "a|b|c\n",
			wantProblems: []string{`"a|b|c"`},
		},
		{
			name:         "hyphen without spaces skipped",
			out:          "wake-Wake a PC",
			wantProblems: []string{`"wake-Wake a PC"`},
		},
		{
			name:         "empty description skipped",
			out:          "wake - \n",
			wantProblems: []string{`"wake -"`},
		},
		{
			name: "bad line does not stop the rest",
			out:  "junk\nok - Fine\nmore junk\n",
			want: []route.BotCommand{{Name: "ok", Description: "Fine"}},
			wantProblems: []string{
				`"junk"`,
				`"more junk"`,
			},
		},
		{
			name: "name and description rules left to BuildCommands",
			out:  "Bad Name - " + strings.Repeat("x", 300),
			want: []route.BotCommand{{Name: "Bad Name", Description: strings.Repeat("x", 300)}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, problems := parseScriptCommands(tt.out)
			if !slices.Equal(got, tt.want) {
				t.Errorf("cmds = %#v, want %#v", got, tt.want)
			}
			if len(problems) != len(tt.wantProblems) {
				t.Fatalf("problems = %q, want %d", problems, len(tt.wantProblems))
			}
			for i, sub := range tt.wantProblems {
				if !strings.Contains(problems[i], sub) {
					t.Errorf("problem %d = %q, want it to contain %s", i, problems[i], sub)
				}
			}
		})
	}
}
