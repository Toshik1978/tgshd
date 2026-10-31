package command

import (
	"context"

	"github.com/Toshik1978/tgshd/pkg/telegram/route"
)

// Publisher declare message publisher.
type Publisher interface {
	// Publish publishes message.
	Publish(ctx context.Context, dest route.Destination, msg string) error
}

// BuiltinCommands returns every built-in menu entry. Handlers return their
// entry from Commands(), and the cmd fallbacks return the very same values.
func BuiltinCommands() []route.BotCommand {
	return []route.BotCommand{
		{Name: "ping", Description: "Ping the configured hosts"},
		{Name: "power", Description: "Show UPS input voltage"},
		{Name: "speed", Description: "Run an internet speed test"},
		{Name: "5g", Description: "Switch the modem to 5G"},
		{Name: "4g", Description: "Switch the modem to 4G"},
		{Name: "3g", Description: "Switch the modem to 3G"},
		{Name: "sms", Description: "Send SMS: /sms <phone> <text>"},
		{Name: "status", Description: "Show current health status"},
		{Name: "clear", Description: "Clear a condition: /clear <condition key>"},
		{Name: "retire", Description: "Retire a drive: /retire <serial or last 4+ chars>"},
		{Name: "history", Description: "Alert history: /history [check] [24h|7d|30d]"},
		{Name: "temp", Description: "Show temperatures"},
		{Name: "disks", Description: "Show disk health"},
	}
}

// BuiltinCommand looks a built-in menu entry up by command name.
func BuiltinCommand(name string) (route.BotCommand, bool) {
	for _, c := range BuiltinCommands() {
		if c.Name == name {
			return c, true
		}
	}

	return route.BotCommand{}, false
}

// builtinCommands wraps the entry for name as a handler's command list; it is
// empty for a name that is not built in.
func builtinCommands(name string) []route.BotCommand {
	if entry, ok := BuiltinCommand(name); ok {
		return []route.BotCommand{entry}
	}

	return nil
}
