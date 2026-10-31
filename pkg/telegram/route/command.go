package route

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

const (
	maxDescriptionLen = 256
	// maxMenuCommands is the most commands Telegram shows in a command menu.
	maxMenuCommands = 100
)

var commandNameRe = regexp.MustCompile(`^[a-z0-9_]{1,32}$`)

// BotCommand is one entry of the Telegram command menu: a bare name (no
// leading "/") and a one-line description.
type BotCommand struct {
	Name        string
	Description string
}

// Valid reports the first rule the command breaks, or nil.
func (c BotCommand) Valid() error {
	if !commandNameRe.MatchString(c.Name) {
		return fmt.Errorf("command name %q must match [a-z0-9_]{1,32} without a leading \"/\"", c.Name)
	}
	if strings.ContainsAny(c.Description, "\r\n") {
		return fmt.Errorf("command %q description must be a single line", c.Name)
	}
	if n := utf8.RuneCountInString(c.Description); n < 1 || n > maxDescriptionLen {
		return fmt.Errorf("command %q description must be 1-%d characters, got %d", c.Name, maxDescriptionLen, n)
	}

	return nil
}

// BuildCommands orders, validates and de-duplicates the command set: builtins
// sorted by name, then script in its own order. Invalid entries and repeated
// names (first wins, so built-ins beat the script) are dropped, one problem
// each. routed is every surviving entry; menu is the first 100 of routed, with
// one problem if it had to be capped.
func BuildCommands(builtins, script []BotCommand) (routed, menu []BotCommand, problems []string) {
	sorted := slices.Clone(builtins)
	slices.SortStableFunc(sorted, func(a, b BotCommand) int { return strings.Compare(a.Name, b.Name) })

	routed = make([]BotCommand, 0, len(sorted)+len(script))
	seen := make(map[string]bool, len(sorted)+len(script))
	for _, c := range append(sorted, script...) {
		if err := c.Valid(); err != nil {
			problems = append(problems, "dropping invalid command: "+err.Error())
			continue
		}
		if seen[c.Name] {
			problems = append(problems, fmt.Sprintf(
				"dropping command %q: name already used by an earlier command", c.Name,
			))
			continue
		}
		seen[c.Name] = true
		routed = append(routed, c)
	}

	menu = routed
	if len(routed) > maxMenuCommands {
		menu = routed[:maxMenuCommands]
		problems = append(problems, fmt.Sprintf(
			"command menu capped at %d of %d commands; the rest still work but are not listed",
			maxMenuCommands, len(routed),
		))
	}

	return routed, menu, problems
}

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
