package command

import (
	"fmt"
	"strings"

	"github.com/Toshik1978/tgshd/pkg/telegram/route"
)

// scriptSeparator splits a script line into the command name and description.
const scriptSeparator = " - "

// parseScriptCommands reads the custom script's command list: one
// "name - description" entry per non-blank line, split on the first " - ".
// Both parts are trimmed and one leading "/" is dropped from the name. A line
// without the separator is skipped and reported in problems. Name and
// description rules are not checked here; route.BuildCommands does that.
func parseScriptCommands(out string) (cmds []route.BotCommand, problems []string) {
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		name, desc, ok := strings.Cut(line, scriptSeparator)
		desc = strings.TrimSpace(desc)
		if !ok || desc == "" {
			problems = append(problems,
				fmt.Sprintf("script command line %q skipped: expected \"name - description\"", line))
			continue
		}

		cmds = append(cmds, route.BotCommand{
			Name:        strings.TrimPrefix(strings.TrimSpace(name), "/"),
			Description: desc,
		})
	}

	return cmds, problems
}
