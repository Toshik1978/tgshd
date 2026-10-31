package route

import "strings"

// CommandName returns the bare "/name" of the command at the start of text.
// "/name" and "/name@<botUsername>" (case-insensitive) both yield "/name";
// "/name@<another bot>" is not ours and yields false, as does text that is
// not a command.
func CommandName(text, botUsername string) (string, bool) {
	fields := strings.Fields(text)
	if len(fields) == 0 || !strings.HasPrefix(fields[0], "/") {
		return "", false
	}
	name, bot, hasBot := strings.Cut(fields[0], "@")
	if !hasBot {
		return name, true
	}
	if bot == "" || !strings.EqualFold(bot, botUsername) {
		return "", false
	}

	return name, true
}
